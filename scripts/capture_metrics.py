#!/usr/bin/env python3
"""Record timestamped Niulang /metrics snapshots as dashboard-ready JSON Lines."""

import argparse
import datetime
import json
import pathlib
import signal
import sys
import time
import urllib.request


MAX_RESPONSE_BYTES = 1024 * 1024


def utc_now() -> str:
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def parse_metrics(text: str) -> dict[str, float]:
    result: dict[str, float] = {}
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#") or not line.startswith("niulang_"):
            continue
        fields = line.split()
        if len(fields) not in (2, 3):
            continue
        name = fields[0].partition("{")[0]
        try:
            result[name] = float(fields[1])
        except ValueError:
            continue
    return result


def fetch_metrics(url: str, timeout: float) -> dict[str, float]:
    request = urllib.request.Request(url, headers={"Accept": "text/plain"})
    with urllib.request.urlopen(request, timeout=timeout) as response:
        if response.status != 200:
            raise RuntimeError(f"metrics endpoint returned HTTP {response.status}")
        payload = response.read(MAX_RESPONSE_BYTES + 1)
    if len(payload) > MAX_RESPONSE_BYTES:
        raise RuntimeError("metrics response exceeds 1 MiB")
    return parse_metrics(payload.decode("utf-8"))


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", required=True, help="Niulang metrics URL, normally http://127.0.0.1:PORT/metrics")
    parser.add_argument("--output", required=True, type=pathlib.Path, help="new JSONL output path")
    parser.add_argument("--interval", type=float, default=1, help="seconds between scrape starts (default: 1)")
    parser.add_argument("--duration", type=float, default=0, help="capture duration in seconds; 0 runs until interrupted")
    parser.add_argument("--timeout", type=float, default=3, help="per-scrape HTTP timeout in seconds")
    parser.add_argument("--label", default="local", help="opaque source label stored with each record")
    return parser


def run(arguments=None) -> int:
    options = build_parser().parse_args(arguments)
    if options.interval <= 0 or options.duration < 0 or options.timeout <= 0:
        raise SystemExit("interval and timeout must be positive; duration must be non-negative")
    if not options.label.strip():
        raise SystemExit("label must not be empty")
    if options.output.exists():
        raise SystemExit(f"output already exists: {options.output}")
    options.output.parent.mkdir(parents=True, exist_ok=True)

    stopping = False

    def stop(_signum, _frame):
        nonlocal stopping
        stopping = True

    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGTERM, stop)

    started = time.monotonic()
    next_scrape = started
    samples = failures = 0
    with options.output.open("x", encoding="utf-8", buffering=1) as output:
        while not stopping:
            now = time.monotonic()
            if options.duration and now - started >= options.duration and samples + failures > 0:
                break
            if now < next_scrape:
                time.sleep(min(next_scrape - now, 0.25))
                continue
            record = {
                "schema_version": 1,
                "type": "metrics",
                "label": options.label,
                "started_utc": utc_now(),
                "elapsed_seconds": round(now - started, 6),
            }
            try:
                record["metrics"] = fetch_metrics(options.url, options.timeout)
                record["status"] = "ok"
                samples += 1
            except Exception as error:  # Capture must preserve gaps, not invent samples.
                record["status"] = "failed"
                record["error"] = str(error)[:500]
                failures += 1
            output.write(json.dumps(record, sort_keys=True) + "\n")
            next_scrape += options.interval
            if next_scrape < time.monotonic() - options.interval:
                next_scrape = time.monotonic()

    print(f"wrote {samples} metrics snapshots and {failures} failures to {options.output}")
    return 0 if samples else 1


if __name__ == "__main__":
    sys.exit(run())
