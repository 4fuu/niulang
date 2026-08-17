# Implementation roadmap

Stages 0--2 and 4 are implemented, Stage 3 was retired after measurement, and
the repository-actionable Stage 5 gates are complete. Wider NAT/middlebox
operation and independent third-party review remain external qualifications.
Public-preview release engineering is implemented: a fail-closed wire contract,
current-tree privacy review, pinned full-history secret scan, exact linked-module
licenses and CycloneDX SBOMs, reviewed static-analysis baseline, credential
rotation tooling, real-path soak harness, and a non-publishing candidate
workflow with six native runtime jobs. A complete non-publishing candidate run
has passed those gates. Publication still requires provenance on the exact
public candidate, an independently protected maintainer approval, and every
remaining preview blocker; this roadmap does not authorize a tag or release.

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
detector. A later real-path intermittent run measured a 27.7-second
first-loss-to-recovered-reply bound, then proved that fresh post-cooldown
associations returned to QUIC.

## Stage 5 — integration and release hardening

- ~~TUN integration through Clash Verge and setup guide.~~ Done: Clash/mihomo
  owns transparent TUN capture and hands selected traffic to queqiao's local
  SOCKS5 TCP/UDP ingress. Direct in-process TUN/VLESS ingress is deliberately
  outside the current two-process architecture.
- ~~Native QUIC DATAGRAM mode for UDP, retaining stream/TCP fallback.~~ Done:
  chosen by QUIC's own capability exchange rather than configured, with the
  control stream retained for lanes that have no datagrams. Measured under
  seeded impairment and exercised on the live blocked/restored QUIC path.
- ~~Cross-platform packaging.~~ Done: deterministic Linux, macOS, and Windows
  archives for amd64 and arm64, embedded metadata, SHA-256 checksums, exact
  linked-dependency licenses, adjacent/embedded CycloneDX SBOMs, native runtime
  smoke jobs, and a manual reviewed-commit release workflow. A tag push alone
  cannot publish.
- ~~Fuzzing and race automation.~~ Done: the weekly deep workflow discovers
  every fuzz target and runs the complete suite under the race detector.
- ~~Resource-limit and security review.~~ Done for the paired SOCKS deployment:
  documented trust boundaries and residual risk, bounded unauthenticated QUIC
  connections, sessions, frames, flow buffers, replay state, relay sockets,
  lanes, and lifetimes, plus pinned vulnerability/static/history scanning, a
  reviewed gosec baseline, and an enforced patched Go toolchain. Independent
  third-party audit remains external release qualification rather than
  repository implementation work.
- ~~Reproducible benchmark reports.~~ Done: versioned JSON records include the
  exact invocation, seeded path, VCS state, toolchain, module graph, latency,
  and contention results; matrix bundles add the source patch and checksums.
- ~~Deterministic intermittent-block and fallback soak.~~ Done: UDP is removed,
  associations fall back to TCP without changing their relay source, UDP is
  restored on the same endpoint, and post-cooldown probes return to QUIC. The
  normal/race soak runs weekly with checksummed provenance.
- ~~Real-path intermittent firewall and bounded production soak.~~ Done: an
  unchanged UDP association recovered valid replies over TCP while UDP stayed
  blocked, a fresh association returned to QUIC after rule removal, and the
  upgraded production pair completed 114/115 persistent UDP probes plus 40/40
  HTTPS flows over ten minutes with stable descriptors and no failed flows.
  NAT/middlebox diversity remains an external deployment qualification.
- ~~Documented rollback instructions.~~ Done: checksum verification, atomic
  binary installation, retained prior builds, and explicit service rollback.
