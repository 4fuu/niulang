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

Status: the gate now has a harness. `wanoptbench --interactive` issues small
requests during a bulk transfer and reports their distribution, split into
connect and first-byte time. At 200 ms and 1% loss with a 50 MiB transfer
running, interactive requests measure a 206 ms median and 367 ms 95th
percentile against the TUIC-shaped reference's 324 and 517; 206 ms is the idle
round trip, so they no longer queue behind bulk at all. This is achieved by
moving a classified bulk flow onto its own QUIC connection, and it holds only
with `--quic-pool`, where a shared control connection exists. The gate has not
been demonstrated on the live link, and the per-class queueing inside a lane is
not what produced this result.

## Stage 3 — adaptive multipath lanes

- Multiple authenticated QUIC connections.
- Cross-lane sequence/reassembly.
- Adaptive lane growth and retirement.
- Per-lane health and global congestion budget.

Gate: single-flow bulk improvement is demonstrated on at least three separate
time windows without unacceptable interactive tail latency.

Status: not met, but for a narrower reason than before. Striping raises
single-flow goodput only where the path polices per source address; on the
emulated per-flow-policed path four lanes carry 50 MiB at 53.0 Mbit/s against
one lane's 22.3 and a TUIC-shaped reference's 22.5, every transfer completing.
On a shared bottleneck extra lanes measure 60.6 against one lane's 58.7, which
is the correct outcome rather than a shortfall. What remains outstanding is the
gate itself: no live campaign has demonstrated this across three windows.

## Stage 4 — automatic fallback and resumption

- UDP health state machine.
- UDP/TCP race for new sessions.
- TCP lane fallback.
- Bounded in-session UDP rescue that keeps the local SOCKS socket while
  opening a fresh authenticated association; TCP fallback is selected by the
  shared UDP health machine.

Gate: injected UDP loss/blocking causes new and existing sessions to recover
within a measured bound, and a future resumable-association extension must
preserve datagrams across the transition without exposing duplicate bytes.

## Stage 5 — TUN and release hardening

- TUN integration and Clash Verge setup guide.
- Native QUIC DATAGRAM mode for UDP, retaining stream/TCP fallback.
- Cross-platform packaging.
- Fuzzing, race tests, resource limits, and security review.
- Reproducible benchmark reports and rollback instructions.
