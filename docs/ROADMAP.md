# Implementation roadmap

## Stage 0 — repository and measurement foundation

- Dedicated repository and reproducible Go toolchain.
- Protocol and threat-model documents.
- Classifier and scheduler unit tests.
- China-US benchmark harness with no live-profile disruption.

## Stage 1 — one-lane paired PEP

- Local SOCKS5 ingress.
- US destination dialer.
- SOCKS5 UDP ASSOCIATE with bounded packet framing and US-side DNS/policy.
- Authenticated TLS/QUIC transport.
- Bounded sequence frames and backpressure.
- Metrics and graceful close/reset.

Gate: one-lane behavior matches a conventional proxy for correctness,
latency, half-close, large uploads, and cancellation.

## Stage 2 — PIAS-inspired scheduling

- NEW/INTERACTIVE/BULK state machine.
- Byte, rate, directionality, and idle-gap features.
- Hysteresis and class dwell times.
- Per-class queueing and aggregate pacing.

Gate: bulk cannot cause the interactive RTT target to exceed the configured
budget in controlled loss and bandwidth tests.

Status: met for the implemented isolation policy. `queqiaobench --interactive`
issues small requests during a bulk transfer and reports their distribution,
split into connect and first-byte time. At 200 ms and 1% loss with a 50 MiB transfer
running, interactive requests measure a 206 ms median and 367 ms 95th
percentile against the TUIC-shaped reference's 324 and 517; 206 ms is the idle
round trip, so they no longer queue behind bulk at all. This is achieved by
moving a classified bulk flow onto its own QUIC connection, and it holds only
with `--quic-pool`, where a shared control connection exists. The per-class
queueing inside a lane is not what produced this result.

The live link is now covered too. A three-round alternating old/fixed A/B moved
the SSH bulk p99 penalty from 347 to 205 ms and the voice p99 penalty from 102
to 31 ms; the fixed build's bulk p99 medians were 559 and 318 ms respectively.
Proactively isolating every bulk flow was tested and rejected because it moved
SSH p99 to 821 ms even while improving voice. The measured policy therefore
isolates reactively when another flow arrives.

## Stage 3 — adaptive multipath lanes (retired)

- Multiple authenticated QUIC connections.
- Cross-lane sequence/reassembly.
- Adaptive lane growth and retirement.
- Per-lane health and global congestion budget.

Gate: single-flow bulk improvement is demonstrated on at least three separate
time windows without unacceptable interactive tail latency.

Status: superseded rather than unmet. Open-loop probing showed that the live
bottleneck is per endpoint pair, not per 4-tuple, and the multi-lane data path
was deleted. Separate connections remain only for bulk isolation and failure
recovery; they do not aggregate one flow's capacity. The measurements and
deletion record are in `DESIGN-MULTIPATH.md`.

## Stage 4 — automatic fallback and resumption

- UDP health state machine.
- UDP/TCP race for new sessions.
- TCP lane fallback.
- Bounded in-session UDP rescue that keeps the local SOCKS socket while
  opening a replacement authenticated association that reclaims the same remote
  relay socket by token; TCP fallback is selected by the shared UDP health
  machine.

Gate: injected UDP loss/blocking causes new and existing sessions to recover
within a measured bound, and a resumable association must preserve the
remote relay's source address across the transition without exposing duplicate
bytes. The mechanism exists and is tested; the measured bound on the live path
is what remains.

## Stage 5 — TUN and release hardening

- TUN integration and Clash Verge setup guide.
- ~~Native QUIC DATAGRAM mode for UDP, retaining stream/TCP fallback.~~ Done:
  chosen by QUIC's own capability exchange rather than configured, with the
  control stream retained for lanes that have no datagrams. Measured emulated,
  not live.
- Cross-platform packaging.
- Fuzzing, race tests, resource limits, and security review.
- Reproducible benchmark reports and rollback instructions.
