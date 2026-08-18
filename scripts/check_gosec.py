#!/usr/bin/env python3
"""Reject gosec findings outside the reviewed public-release baseline."""

from __future__ import annotations

import collections
import json
import pathlib
import sys


# Counts are deliberately scoped to both rule and file. This is narrower than
# passing -exclude to gosec: a new rule, a finding in a new file, or an increase
# in any reviewed bucket fails the build and requires a fresh human triage.
BASELINE = {
    # The notice generator is release tooling. Its paths and module reference
    # come from operator arguments and a reviewed dependency lock; exec.Command
    # passes the reference as one argument without a shell, and the 0755 output
    # directory contains deliberately public license text.
    ("G104", "mobile/tools/notices/main.go"): 3,
    ("G115", "cmd/pathprobe/main.go"): 5,
    ("G115", "cmd/queqiaobench/main.go"): 3,
    ("G115", "cmd/queqiaod/main.go"): 2,
    ("G115", "cmd/queqiaopack/main.go"): 1,
    ("G115", "internal/baseline/baseline.go"): 1,
    ("G115", "internal/coded/coded.go"): 12,
    ("G115", "internal/congestion/adaptive.go"): 1,
    ("G115", "internal/congestion/bbr.go"): 1,
    ("G115", "internal/congestion/bbr_tuic.go"): 11,
    ("G115", "internal/congestion/bbr_tuic_bw.go"): 1,
    ("G115", "internal/congestion/brutal.go"): 2,
    ("G115", "internal/congestion/erasure.go"): 1,
    ("G115", "internal/congestion/pacer.go"): 3,
    ("G115", "internal/fec/window.go"): 22,
    ("G115", "internal/lossmodel/lossmodel.go"): 1,
    ("G115", "internal/metrics/metrics.go"): 3,
    ("G115", "internal/multipath/reassembly.go"): 1,
    ("G115", "internal/pep/client.go"): 2,
    ("G115", "internal/pep/mpflow.go"): 10,
    ("G115", "internal/pep/server.go"): 1,
    ("G115", "internal/pep/stripedsend.go"): 5,
    ("G115", "internal/protocol/frame.go"): 1,
    ("G115", "internal/session/packet.go"): 1,
    ("G115", "internal/socks5/socks5.go"): 1,
    ("G115", "internal/stripe/stripe.go"): 1,
    ("G204", "cmd/queqiaopack/main.go"): 1,
    ("G204", "internal/extproxy/process.go"): 1,
    ("G204", "mobile/tools/notices/main.go"): 1,
    ("G301", "cmd/queqiaopack/main.go"): 3,
    ("G301", "mobile/tools/notices/main.go"): 1,
    ("G302", "cmd/queqiaopack/main.go"): 2,
    ("G304", "cmd/queqiaod/main.go"): 2,
    ("G304", "cmd/queqiaopack/main.go"): 6,
    ("G304", "cmd/queqiaoref/main.go"): 2,
    ("G304", "internal/identity/enrollment.go"): 1,
    ("G304", "internal/identity/filelock_unix.go"): 1,
    ("G304", "internal/identity/pki.go"): 6,
    ("G304", "internal/identity/profile.go"): 1,
    ("G304", "internal/identity/store.go"): 1,
    ("G304", "mobile/tools/notices/main.go"): 1,
    ("G306", "cmd/queqiaobench/report.go"): 1,
    ("G306", "cmd/queqiaopack/main.go"): 2,
    ("G306", "cmd/queqiaoref/main.go"): 1,
    ("G402", "internal/identity/tls.go"): 1,
    ("G404", "internal/pathsim/pathsim.go"): 6,
    ("G404", "internal/pathsim/tcp.go"): 2,
}


def relative_file(value: str, root: pathlib.Path) -> str:
    path = pathlib.Path(value)
    try:
        return path.resolve().relative_to(root.resolve()).as_posix()
    except ValueError:
        return path.as_posix()


def main() -> int:
    if len(sys.argv) not in (2, 3):
        print("usage: check_gosec.py REPORT.json [REPOSITORY]", file=sys.stderr)
        return 2
    report = pathlib.Path(sys.argv[1])
    root = pathlib.Path(sys.argv[2]) if len(sys.argv) == 3 else pathlib.Path.cwd()
    try:
        document = json.loads(report.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        print(f"cannot read gosec report: {error}", file=sys.stderr)
        return 2

    issues = document.get("Issues") or []
    counts = collections.Counter(
        (issue.get("rule_id", ""), relative_file(issue.get("file", ""), root))
        for issue in issues
    )
    failures = []
    for key, count in sorted(counts.items()):
        allowed = BASELINE.get(key, 0)
        if count > allowed:
            failures.append(f"{key[0]} {key[1]}: found {count}, reviewed maximum {allowed}")
    if failures:
        print("unreviewed gosec findings:", file=sys.stderr)
        for failure in failures:
            print(f"  {failure}", file=sys.stderr)
        return 1
    print(f"gosec baseline accepted {len(issues)} reviewed findings in {len(counts)} buckets")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
