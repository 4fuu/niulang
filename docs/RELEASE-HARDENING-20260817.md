# Release-hardening and live fallback report — 2026-08-17

This report closes the repository-actionable Stage 4/5 work after merge commit
`1a0ba07`. Tests used an isolated server on `<EGRESS-IP>:12541` and local
SOCKS/metrics listeners on `127.0.0.1:12180/12190`; production remained on
`12540` and `12080` until the isolated matrix passed.

## Build and repository gates

The release build embedded:

```text
queqiaod roadmap-1a0ba07 commit=1a0ba07 built=2026-08-17T14:17:05+08:00 go=go1.25.13
```

The following passed before deployment:

- complete tests (`go test -timeout 50m ./...`);
- complete race tests (`go test -race -timeout 110m ./...`), including the
  548-second FEC package;
- vet, formatting, workflow lint, and all six direct cross-builds;
- deterministic release archives for Linux, macOS, and Windows on amd64 and
  arm64, with every SHA-256 checksum verified;
- `govulncheck v1.7.0 ./...` with no reachable vulnerability on Go 1.25.13;
- a clean-tree fallback soak at commit `cd8f56a`: 20 normal and five race
  repetitions, with all report checksums verified; and
- every PR #6 CI job, including the six-target build matrix and package smoke.

The first vulnerability scan on the former Go 1.25.7 toolchain found 11
reachable standard-library advisories. Requiring Go 1.25.13 made the same scan
clean and prevents release builds from silently using the vulnerable patch
level.

## Real intermittent UDP block and recovery

An HTTPS request through the isolated pair first returned the fixed egress IP
with one active QUIC lane and zero fallbacks. One persistent SOCKS5 UDP
association then issued DNS A queries to `1.1.1.1`.

After valid replies, the server installed exactly one temporary rule:

```text
INPUT -p udp --dport 12541 -m comment \
  --comment queqiao-roadmap-1a0ba07 -j DROP
```

Queries 27 through 37 received no reply. Query 38 and every query through 50
then received a valid DNS response while that rule was still present, proving
that the unchanged association had moved to TLS/TCP. The observed gap from the
first missing response to the first recovered response was approximately
27.7 seconds, including eleven two-second application receive deadlines and
the 0.5-second probe intervals. The complete run delivered 39/50 replies.

Client metrics after recovery were:

```text
queqiao_lane_failures_total 1
queqiao_lane_replacements_total 1
queqiao_fallbacks_total 1
queqiao_udp_association_reconnects_total 1
queqiao_udp_association_rescue_failures_total 0
```

The rule was removed by its full specification and independently verified
absent. A server-side packet capture then observed a fresh QUIC handshake on
UDP/12541 from the physical China uplink. A new post-cooldown association
completed 8/8 DNS queries at 194–197 ms without increasing the fallback
counter. This covers blocked, rescued, restored, and re-preferred behavior on
the real path. It does not make datagrams in flight lossless; the eleven missed
queries are the expected UDP semantics during failure detection.

## Atomic production upgrade and rollback

Only after the isolated matrix passed were the two production binaries
replaced atomically and their services restarted. The new server PID was
`135146`; the new local PID was `31378`. A post-restart HTTPS request through
the production SOCKS listener returned the expected fixed egress address.

The exact pre-upgrade binaries remain at:

```text
<SERVER-ROLLBACK-DIR>/queqiaod-rollback-pre-1a0ba07
<CLIENT-ROLLBACK-DIR>/queqiaod-rollback-pre-1a0ba07
```

Production remains on `<EGRESS-IP>:12540` and `127.0.0.1:12080`. The
isolated 12541/12180/12190 listeners were stopped, and both their absence and
the absence of the temporary firewall rule were verified.

## Mixed production soak

The upgraded production pair ran ten minutes of simultaneous traffic:

- one unchanged SOCKS5 UDP association sent 115 DNS queries five seconds
  apart, with 114 valid replies and a successful final five-query tail; and
- 40 independent HTTPS flows completed 40/40 and all reported the fixed
  egress IP.

Final local counters were 44 started, 44 completed, zero failed, zero active,
zero lane failures/replacements/fallbacks/reconnects/rescue failures, zero
completion/flow timeouts, and zero replay bytes. Local RSS moved from 19,888
KiB to 24,352 KiB while its descriptor count returned to the baseline 17.
Server RSS moved from 13,668 KiB to 17,892 KiB and its descriptor count returned
from nine during the active association to the baseline eight. The production
service had no warning-or-higher journal entries during the soak.

This is a bounded release soak, not a substitute for ongoing operational
monitoring or an independent security assessment. The remaining external
qualifications are broader path/middlebox diversity and third-party review,
not missing Stage 4/5 repository mechanisms.
