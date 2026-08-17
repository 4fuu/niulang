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
bytes.

Status: met for the bounded fallback mechanism. A live UDP blackhole recovered
the same SOCKS5 UDP association over a fresh authenticated TCP lane in 9.51 s,
preserving its local endpoint and remote relay source address. Deterministic
tests cover TCP stream replay without duplicate bytes, relay-source
preservation, single-use resume tokens, and repeated rescue under the race
detector. Intermittent blocking and long-soak coverage remain Stage 5 release
hardening rather than an unimplemented fallback mechanism.

## Stage 5 — integration and release hardening

- ~~TUN integration through Clash Verge and setup guide.~~ Done: Clash/mihomo
  owns transparent TUN capture and hands selected traffic to queqiao's local
  SOCKS5 TCP/UDP ingress. Direct in-process TUN/VLESS ingress is deliberately
  outside the current two-process architecture.
- ~~Native QUIC DATAGRAM mode for UDP, retaining stream/TCP fallback.~~ Done:
  chosen by QUIC's own capability exchange rather than configured, with the
  control stream retained for lanes that have no datagrams. Measured emulated,
  not live.
- ~~Cross-platform packaging.~~ Done: deterministic Linux, macOS, and Windows
  archives for amd64 and arm64, embedded provenance, SHA-256 checksums, and a
  tag-driven release workflow.
- ~~Fuzzing and race automation.~~ Done: the weekly deep workflow discovers
  every fuzz target and runs the complete suite under the race detector.
- ~~Resource-limit and security review.~~ Done for the paired SOCKS deployment:
  documented trust boundaries and residual risk, bounded unauthenticated QUIC
  connections, sessions, frames, flow buffers, replay state, relay sockets,
  lanes, and lifetimes, plus pinned vulnerability scanning and an enforced
  patched Go toolchain. Independent audit and live soak remain release gates.
- ~~Reproducible benchmark reports.~~ Done: versioned JSON records include the
  exact invocation, seeded path, VCS state, toolchain, module graph, latency,
  and contention results; matrix bundles add the source patch and checksums.
- ~~Deterministic intermittent-block and fallback soak.~~ Done: UDP is removed,
  associations fall back to TCP without changing their relay source, UDP is
  restored on the same endpoint, and post-cooldown probes return to QUIC. The
  normal/race soak runs weekly with checksummed provenance.
- Real-path intermittent firewall/NAT and long-duration soak campaigns.
- ~~Documented rollback instructions.~~ Done: checksum verification, atomic
  binary installation, retained prior builds, and explicit service rollback.
