#!/usr/bin/env python3
"""Continuously measure small HTTPS requests through a Queqiao SOCKS5 client."""

from __future__ import annotations

import argparse
import datetime
import hashlib
import json
import math
import os
import pathlib
import re
import shutil
import signal
import socket
import subprocess
import sys
import time
from urllib.parse import urlsplit

try:
    from scripts import udp_association_check as udp_check
except ModuleNotFoundError:  # direct execution from the scripts directory
    import udp_association_check as udp_check


DEFAULT_TARGETS = (
    "cloudflare=https://www.cloudflare.com/cdn-cgi/trace",
    "google=https://www.google.com/generate_204",
    "apple=https://www.apple.com/library/test/success.html",
    "github=https://github.com/robots.txt",
    "wikipedia=https://www.wikipedia.org/robots.txt",
)
TARGET_NAME = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$")
CURL_WRITE_OUT = (
    "%{http_code}\t%{time_connect}\t%{time_appconnect}\t"
    "%{time_starttransfer}\t%{time_total}\t%{size_download}\n"
)


def utc_now() -> str:
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="milliseconds")


def parse_target(value: str) -> tuple[str, str]:
    name, separator, url = value.partition("=")
    if not separator or not TARGET_NAME.fullmatch(name):
        raise argparse.ArgumentTypeError("target must be NAME=https://URL with a short safe NAME")
    parsed = urlsplit(url)
    if parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.password:
        raise argparse.ArgumentTypeError("target URL must be HTTPS and must not contain credentials")
    return name, url


def display_endpoint(endpoint: tuple[str, int]) -> str:
    host, port = endpoint
    return f"[{host}]:{port}" if ":" in host else f"{host}:{port}"


def write_json(path: pathlib.Path, value) -> None:
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def write_checksums(directory: pathlib.Path) -> None:
    lines = []
    for path in sorted(directory.iterdir()):
        if path.is_file() and path.name != "SHA256SUMS":
            digest = hashlib.sha256(path.read_bytes()).hexdigest()
            lines.append(f"{digest}  {path.name}\n")
    (directory / "SHA256SUMS").write_text("".join(lines), encoding="utf-8")


def parse_curl_output(output: str) -> dict:
    fields = output.strip().split("\t")
    if len(fields) != 6:
        raise ValueError("curl did not emit the expected timing fields")
    try:
        http_status = int(fields[0])
        connect, appconnect, starttransfer, total = (float(value) for value in fields[1:5])
        size = int(float(fields[5]))
    except ValueError as error:
        raise ValueError("curl emitted an invalid timing field") from error
    if any(value < 0 or not math.isfinite(value) for value in (connect, appconnect, starttransfer, total)):
        raise ValueError("curl emitted a negative or non-finite timing field")
    return {
        "http_status": http_status,
        "local_proxy_connect_ms": round(connect * 1000, 3),
        "tls_ready_ms": round(appconnect * 1000, 3),
        "ttfb_ms": round(starttransfer * 1000, 3),
        "total_ms": round(total * 1000, 3),
        "bytes_downloaded": size,
    }


def run_probe(
    curl: str,
    proxy: tuple[str, int],
    target: tuple[str, str],
    connect_timeout: float,
    timeout: float,
) -> dict:
    name, url = target
    started = time.monotonic()
    command = [
        curl,
        "--noproxy",
        "",
        "--proxy",
        f"socks5h://{display_endpoint(proxy)}",
        "--location",
        "--max-redirs",
        "3",
        "--connect-timeout",
        str(connect_timeout),
        "--max-time",
        str(timeout),
        "--output",
        os.devnull,
        "--silent",
        "--show-error",
        "--user-agent",
        "queqiao-website-latency-soak/1",
        "--write-out",
        CURL_WRITE_OUT,
        url,
    ]
    try:
        result = subprocess.run(command, text=True, capture_output=True, check=False)
    except OSError as error:
        return {
            "status": "failed",
            "curl_exit": None,
            "observed_ms": round((time.monotonic() - started) * 1000, 3),
            "error": str(error)[:500],
        }

    record = {
        "curl_exit": result.returncode,
        "observed_ms": round((time.monotonic() - started) * 1000, 3),
    }
    try:
        record.update(parse_curl_output(result.stdout))
    except ValueError as error:
        record.update({"status": "failed", "error": str(error)})
        return record

    http_status = record["http_status"]
    if result.returncode == 0 and 200 <= http_status < 400:
        record["status"] = "ok"
    else:
        detail = result.stderr.strip() or f"curl exited {result.returncode}, HTTP {http_status}"
        record.update({"status": "failed", "error": detail[:500]})
    return record


def percentile(values: list[float], percent: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    position = (len(ordered) - 1) * percent / 100
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return round(ordered[lower], 3)
    fraction = position - lower
    return round(ordered[lower] * (1 - fraction) + ordered[upper] * fraction, 3)


def latency_summary(events: list[dict], field: str) -> dict:
    values = [float(event[field]) for event in events if event["status"] == "ok" and field in event]
    return {
        "p50": percentile(values, 50),
        "p95": percentile(values, 95),
        "p99": percentile(values, 99),
        "max": round(max(values), 3) if values else None,
    }


def longest_failure_streak(events: list[dict]) -> int:
    longest = current = 0
    for event in events:
        if event["status"] == "ok":
            current = 0
        else:
            current += 1
            longest = max(longest, current)
    return longest


def aggregate(events: list[dict]) -> dict:
    attempts = len(events)
    successes = sum(event["status"] == "ok" for event in events)
    return {
        "attempts": attempts,
        "successes": successes,
        "failures": attempts - successes,
        "success_rate": successes / attempts if attempts else 0,
        "longest_failure_streak": longest_failure_streak(events),
        "latency_ms": {
            field.removesuffix("_ms"): latency_summary(events, field)
            for field in ("local_proxy_connect_ms", "tls_ready_ms", "ttfb_ms", "total_ms")
        },
    }


def build_summary(
    events: list[dict],
    target_names: list[str],
    completed: bool,
    interrupted: bool,
    elapsed: float,
    minimum_success_rate: float,
    maximum_p95_ms: float,
) -> dict:
    per_target = {}
    for name in target_names:
        target_events = [event for event in events if event["target"] == name]
        per_target[name] = aggregate(target_events)

    rounds: dict[int, list[dict]] = {}
    hours: dict[int, list[dict]] = {}
    for event in events:
        rounds.setdefault(int(event["round"]), []).append(event)
        hours.setdefault(int(float(event["elapsed_seconds"]) // 3600), []).append(event)
    all_failed = sum(
        len(round_events) == len(target_names)
        and all(event["status"] != "ok" for event in round_events)
        for round_events in rounds.values()
    )
    any_failed = sum(any(event["status"] != "ok" for event in round_events) for round_events in rounds.values())

    gates = []
    for name, result in per_target.items():
        rate_ok = result["success_rate"] >= minimum_success_rate
        p95 = result["latency_ms"]["ttfb"]["p95"]
        latency_ok = maximum_p95_ms <= 0 or (p95 is not None and p95 <= maximum_p95_ms)
        gates.append(rate_ok and latency_ok)

    return {
        "format": "queqiao-website-latency-summary-v1",
        "finished_utc": utc_now(),
        "elapsed_seconds": round(elapsed, 3),
        "completed_requested_duration": completed,
        "interrupted": interrupted,
        "rounds": len(rounds),
        "rounds_with_any_failure": any_failed,
        "rounds_with_all_targets_failed": all_failed,
        "minimum_success_rate": minimum_success_rate,
        "maximum_p95_ttfb_ms": maximum_p95_ms or None,
        "overall": aggregate(events),
        "targets": per_target,
        "hourly": [
            {"hour": hour, **aggregate(hour_events)} for hour, hour_events in sorted(hours.items())
        ],
        "stable": completed and not interrupted and bool(gates) and all(gates),
    }


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--socks",
        type=udp_check.parse_endpoint,
        default=("127.0.0.1", 12080),
        help="Queqiao SOCKS5 endpoint (default: 127.0.0.1:12080)",
    )
    parser.add_argument(
        "--target",
        action="append",
        type=parse_target,
        help="NAME=https://URL; repeat to replace the default target set",
    )
    parser.add_argument("--duration", type=float, default=8 * 60 * 60, help="run time in seconds (default: 28800)")
    parser.add_argument("--interval", type=float, default=60, help="seconds between round starts (default: 60)")
    parser.add_argument("--connect-timeout", type=float, default=5, help="curl connection timeout in seconds")
    parser.add_argument("--timeout", type=float, default=15, help="whole-request timeout in seconds")
    parser.add_argument(
        "--min-success-rate",
        type=float,
        default=0.99,
        help="per-target stability gate (default: 0.99)",
    )
    parser.add_argument(
        "--max-p95-ms",
        type=float,
        default=0,
        help="optional per-target p95 TTFB gate; 0 disables it",
    )
    parser.add_argument("--output-dir", required=True, type=pathlib.Path, help="new evidence directory")
    parser.add_argument("--quiet", action="store_true", help="suppress per-round progress")
    return parser


def run(arguments=None) -> int:
    options = build_parser().parse_args(arguments)
    if options.duration <= 0 or options.interval <= 0 or options.connect_timeout <= 0 or options.timeout <= 0:
        raise SystemExit("duration, interval, connect-timeout, and timeout must be positive")
    if options.connect_timeout > options.timeout:
        raise SystemExit("connect-timeout must not exceed timeout")
    if not 0 <= options.min_success_rate <= 1 or options.max_p95_ms < 0:
        raise SystemExit("min-success-rate must be between 0 and 1; max-p95-ms must be non-negative")
    if options.output_dir.exists():
        raise SystemExit(f"output directory already exists: {options.output_dir}")

    curl = shutil.which("curl")
    if not curl:
        raise SystemExit("curl is required")
    try:
        with socket.create_connection(options.socks, timeout=options.connect_timeout):
            pass
    except OSError as error:
        raise SystemExit(f"Queqiao SOCKS endpoint {display_endpoint(options.socks)} is unavailable: {error}")

    targets = options.target or [parse_target(value) for value in DEFAULT_TARGETS]
    names = [name for name, _ in targets]
    if len(set(names)) != len(names):
        raise SystemExit("target names must be unique")

    options.output_dir.mkdir(parents=True)
    manifest = {
        "format": "queqiao-website-latency-soak-v1",
        "started_utc": utc_now(),
        "platform": sys.platform,
        "socks": display_endpoint(options.socks),
        "duration_seconds": options.duration,
        "interval_seconds": options.interval,
        "connect_timeout_seconds": options.connect_timeout,
        "timeout_seconds": options.timeout,
        "minimum_success_rate": options.min_success_rate,
        "maximum_p95_ttfb_ms": options.max_p95_ms or None,
        "targets": [{"name": name, "url": url} for name, url in targets],
        "curl": subprocess.run([curl, "--version"], text=True, capture_output=True, check=False).stdout.splitlines()[0],
    }
    write_json(options.output_dir / "manifest.json", manifest)

    stopping = False

    def stop(_signum, _frame):
        nonlocal stopping
        stopping = True

    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGTERM, stop)
    events: list[dict] = []
    events_path = options.output_dir / "events.jsonl"
    started = time.monotonic()
    next_round = started
    round_number = 0
    with events_path.open("x", encoding="utf-8", buffering=1) as output:
        while not stopping:
            now = time.monotonic()
            if now - started >= options.duration and round_number:
                break
            if now < next_round:
                time.sleep(min(next_round - now, 0.25))
                continue
            round_number += 1
            round_successes = 0
            for target in targets:
                if stopping:
                    break
                event = {
                    "type": "https_latency",
                    "round": round_number,
                    "target": target[0],
                    "url": target[1],
                    "started_utc": utc_now(),
                    "elapsed_seconds": round(time.monotonic() - started, 6),
                }
                event.update(run_probe(curl, options.socks, target, options.connect_timeout, options.timeout))
                events.append(event)
                round_successes += event["status"] == "ok"
                output.write(json.dumps(event, sort_keys=True) + "\n")
            if not options.quiet:
                print(
                    f"{utc_now()} round={round_number} ok={round_successes}/{len(targets)} "
                    f"elapsed={time.monotonic() - started:.1f}s",
                    flush=True,
                )
            next_round += options.interval
            if next_round < time.monotonic():
                next_round = time.monotonic()

    elapsed = time.monotonic() - started
    completed = elapsed >= options.duration
    summary = build_summary(
        events,
        names,
        completed,
        stopping,
        elapsed,
        options.min_success_rate,
        options.max_p95_ms,
    )
    write_json(options.output_dir / "summary.json", summary)
    write_checksums(options.output_dir)
    print(json.dumps(summary, indent=2, sort_keys=True))
    if stopping:
        return 130
    return 0 if summary["stable"] else 1


if __name__ == "__main__":
    sys.exit(run())
