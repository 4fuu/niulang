# Implementation roadmap

## Stage 0 — repository and measurement foundation

- Dedicated repository and reproducible Go toolchain.
- Protocol and threat-model documents.
- Classifier and scheduler unit tests.
- China-US benchmark harness with no live-profile disruption.

## Stage 1 — one-lane paired PEP

- Local SOCKS5 ingress.
- US destination dialer.
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

## Stage 3 — adaptive multipath lanes

- Multiple authenticated QUIC connections.
- Cross-lane sequence/reassembly.
- Adaptive lane growth and retirement.
- Per-lane health and global congestion budget.

Gate: single-flow bulk improvement is demonstrated on at least three separate
time windows without unacceptable interactive tail latency.

## Stage 4 — automatic fallback and resumption

- UDP health state machine.
- UDP/TCP race for new sessions.
- TCP lane fallback.
- Session resume and lane replacement.

Gate: injected UDP loss/blocking causes new and existing sessions to recover
within a measured bound without exposing duplicate application bytes.

## Stage 5 — TUN and release hardening

- TUN integration and Clash Verge setup guide.
- Cross-platform packaging.
- Fuzzing, race tests, resource limits, and security review.
- Reproducible benchmark reports and rollback instructions.

