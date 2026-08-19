#!/usr/bin/env python3
"""Select successful normal-CI evidence for one exact Git commit."""

from __future__ import annotations

import argparse
import json
import re
import sys
from typing import Any


FULL_SHA = re.compile(r"[0-9a-f]{40}\Z")
QUALIFYING_EVENTS = frozenset({"push", "workflow_dispatch"})


def select_ci_run(document: Any, sha: str) -> dict[str, Any]:
    """Return the newest qualifying run, rejecting malformed evidence."""
    if not FULL_SHA.fullmatch(sha):
        raise ValueError("commit must be a full lowercase SHA")
    if not isinstance(document, dict) or not isinstance(document.get("workflow_runs"), list):
        raise ValueError("GitHub response has no workflow_runs array")

    qualifying: list[dict[str, Any]] = []
    for run in document["workflow_runs"]:
        if not isinstance(run, dict):
            continue
        if (
            run.get("head_sha") != sha
            or run.get("conclusion") != "success"
            or run.get("event") not in QUALIFYING_EVENTS
        ):
            continue
        run_id = run.get("id")
        run_number = run.get("run_number")
        url = run.get("html_url")
        if (
            not isinstance(run_id, int)
            or isinstance(run_id, bool)
            or run_id <= 0
            or not isinstance(run_number, int)
            or isinstance(run_number, bool)
            or run_number <= 0
            or not isinstance(url, str)
            or not url.startswith(("https://", "http://"))
        ):
            continue
        qualifying.append({"id": run_id, "run_number": run_number, "html_url": url})

    if not qualifying:
        raise ValueError(f"no successful push or dispatch CI run found for {sha}")
    return max(qualifying, key=lambda run: (run["run_number"], run["id"]))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sha", required=True, help="exact 40-character lowercase commit SHA")
    arguments = parser.parse_args()
    try:
        document = json.load(sys.stdin)
        selected = select_ci_run(document, arguments.sha)
    except (OSError, json.JSONDecodeError, ValueError) as error:
        print(f"CI evidence rejected: {error}", file=sys.stderr)
        return 1
    print(json.dumps(selected, separators=(",", ":"), sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
