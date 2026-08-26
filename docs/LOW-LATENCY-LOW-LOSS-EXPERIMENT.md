# Low-latency, relatively high-bandwidth, low-loss experiment — 2026-08-26

> [!WARNING]
> **Development evidence, not a production guarantee.** These results use a
> deterministic in-process path emulator on one Linux machine. They do not
> model a real NIC scheduler, middleboxes, NAT rebinding, path-MTU changes, or
> independent endpoint CPU contention.

## Decision

Do not change the production default from this campaign.

The data supports continuing with an opt-in **shared path-level aggregate QUIC
packet-byte pacing budget wrapped around the existing erasure controller**. It
does not support per-lane fixed Brutal as the production controller. The
shared prototype prevents a configured rate from multiplying with connections,
keeps the erasure/path model and adaptive FEC active, and leaves an explicit
interactive reserve. Its current pacing boundary is burst-bounded rather than
a strict UDP/IP wire cap, so it remains disabled by default.

The next production step is a QUIC pre-registration pacing hook that covers all
packets without moving send/PTO registration ahead of actual transmission.
Separately, fresh-path FEC convergence under burst loss needs improvement.

## Provenance and method

All cells ran serially on the current Amp orb; no second orb was started.

| Property | Value |
| --- | --- |
| CPU / memory | 16 vCPU Intel Xeon at 2.60 GHz / 31 GiB |
| OS | Linux 6.1.158+, x86_64 |
| Go | 1.25.13 |
| Hysteria2 | sing-box 1.13.18 |
| Primary path | 226 ms RTT, 100 Mbit/s, 1% and 5% independent loss |
| Primary sample | 10 seeded trials per cell |
| Transfer | 32 MiB, one logical flow, concurrent interactive probes |
| UDP | 100 datagrams per trial, 1200 bytes, 10 trials |

The first full campaign produced 87 JSON cells across 50--400 ms RTT, 50--100
Mbit/s, 0--5% primary loss, one 15% HOL boundary, burst loss, two UDP payload
sizes, three policer refill intervals, and connection reuse. Review found that
the in-process loopback client and server directions shared one production
path-model key and that model state survived across trials. Results from
controllers that consume that model were therefore contaminated.

The authoritative follow-up resets `pathmodel` before every trial and binds the
client to `127.0.0.2`, distinct from the server's `127.0.0.1`. All erasure,
shared-cap, FEC, and reuse claims below use that corrected bundle. BBR-TUIC,
Brutal, and real Hysteria2 do not consume Queqiao's path model, so the matching
cells from the original campaign remain usable. The Hysteria2 comparisons use
the same path parameters and initial seed; different protocols consume a
different number of packets, so “seeded” does not mean identical packet loss
positions.

Medians are across trials. UDP delivery pools 1,000 application packets while
UDP p95 and max are medians of the ten trial-level values. Completion always
requires the exact expected byte count. With only ten deterministic seed
replicates, small differences are descriptive, not population confidence
intervals. The paired Brutal sign counts are called out separately because
their direction is stable enough to test without assuming a metric
distribution.

## Throughput and latency

All rows are 226 ms / 100 Mbit/s with a normal one-BDP queue. `Drop` is
sender-induced bottleneck drop, separate from configured ambient erasure.

| Loss | Transport/controller | Complete | Median Mbit/s | Interactive p95 ms | Drop |
| ---: | --- | ---: | ---: | ---: | ---: |
| 1% | Queqiao erasure | 10/10 | 51.764 | 464.477 | 1.164% |
| 1% | Erasure + shared cap 95/reserve 10 | 10/10 | 47.720 | 241.108 | 0% |
| 1% | BBR-TUIC | 10/10 | 43.633 | 436.089 | 2.655% |
| 1% | Brutal-no-comp 95 per lane | 10/10 | 73.429 | 254.081 | 0% |
| 1% | Real Hysteria2 | 10/10 | 51.615 | 395.759 | 2.662% |
| 5% | Queqiao erasure | 10/10 | 38.549 | 253.176 | 0.450% |
| 5% | Erasure + shared cap 95/reserve 10 | 10/10 | 38.123 | 251.069 | 0% |
| 5% | BBR-TUIC | 10/10 | 38.267 | 485.495 | 1.748% |
| 5% | Brutal-no-comp 95 per lane | 10/10 | 57.175 | 474.159 | 0% |
| 5% | Real Hysteria2 | 10/10 | 43.159 | 615.671 | 1.511% |

The shared cap trades 7.8% median goodput at 1% loss for zero measured queue
drop and 223 ms lower interactive p95. At 5% loss its median goodput is within
1.1% of uncapped erasure, with zero measured queue drop. The 5% per-trial
goodput distributions are broad (about 21--48 Mbit/s for erasure and 21--45
Mbit/s for the cap), so that near-equality is not evidence of superiority in
either direction.

Hysteria2 completed every throughput trial and was competitive on median
goodput, but its interactive tail and queue drop were both worse than the
shared-cap prototype in these two cells. This is a mechanism comparison, not
protocol parity: Hysteria2 does not use Queqiao framing or adaptive FEC.

## Policer boundary and aggregate invariant

The policer cells use 226 ms RTT, 100 Mbit/s, 1% independent loss, and a 32 MiB
transfer. The prototype is configured at 95 Mbit/s total with 10 Mbit/s held
from bulk connections.

| Refill | Mode | Complete | Median Mbit/s | Interactive p95 ms | Bottleneck drop |
| ---: | --- | ---: | ---: | ---: | ---: |
| 1 ms | Erasure | 8/10 | 32.124 | 730.334 | 43.948% |
| 1 ms | Erasure + shared cap | 10/10 | 39.185 | 244.379 | 6.996% |
| 8 ms | Erasure | 10/10 | 42.898 | 480.076 | 40.894% |
| 8 ms | Erasure + shared cap | 10/10 | 47.691 | 240.078 | 0% |
| 16 ms | Erasure | 10/10 | 39.612 | 729.462 | 40.768% |
| 16 ms | Erasure + shared cap | 10/10 | 48.451 | 239.760 | 0% |

The 1 ms residual is real. The wrapper permits a ten-packet burst, QUIC can
bypass pacing eligibility for some ACK/PTO work, and concurrent send loops can
race by approximately one packet before either charge is visible. Later sends
repay that debt, but a 1 ms policer can reject the burst first. Reducing the
burst to one packet exposed host timer granularity and collapsed measured
throughput to about 10 Mbit/s; six to eight packets still imposed 37--41
Mbit/s ceilings in 8 ms controls. The ten-packet setting is retained and the
feature is explicitly not described as a hard cap.

A clean 50 ms / 100 Mbit/s control configured the cap at 40 Mbit/s with a 5
Mbit/s reserve:

| Logical flows | Complete | Aggregate median Mbit/s | Interactive p95 ms | Drop / wrapper overshoot |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 5/5 | 37.915 | 54.176 | 0 / 0 |
| 4 | 5/5 | 38.767 | 54.482 | 0 / 0 |

Four flows do not receive four copies of the configured rate. This directly
checks the principal aggregate invariant across connection/lane sharing.

The existing aggregate application budget cannot substitute for this pacing
boundary. It admits `len(frame.Payload)` before queueing; it omits QUIC and
Queqiao headers, FEC repairs, retransmissions, handshake and probe packets, and
packet burst shape. Consequently an application-byte budget can be useful for
fairness while still overshooting a packet policer on the wire.

## UDP delivery and stream HOL

Datagram results use a fresh path model for every trial at 226 ms / 100 Mbit/s.

| Loss model | Mode | Delivered | Delivery | Median trial p95 ms | Median trial max ms |
| --- | --- | ---: | ---: | ---: | ---: |
| independent 1% | Erasure datagram | 983/1000 | 98.3% | 228.854 | 230.818 |
| independent 1% | Erasure + shared cap | 983/1000 | 98.3% | 229.140 | 231.106 |
| independent 1% | BBR-TUIC datagram | 957/1000 | 95.7% | 228.623 | 228.911 |
| independent 1% | Brutal-no-comp datagram | 956/1000 | 95.6% | 228.606 | 229.000 |
| independent 1% | Real Hysteria2 | 963/1000 | 96.3% | 229.318 | 229.749 |
| independent 5% | Erasure datagram | 986/1000 | 98.6% | 229.987 | 232.687 |
| independent 5% | Erasure + shared cap | 989/1000 | 98.9% | 230.218 | 232.299 |
| independent 5% | BBR-TUIC datagram | 812/1000 | 81.2% | 228.638 | 229.024 |
| independent 5% | Brutal-no-comp datagram | 811/1000 | 81.1% | 228.541 | 228.935 |
| independent 5% | Real Hysteria2 | 800/1000 | 80.0% | 228.902 | 229.279 |
| burst 5%, mean 4 | Erasure datagram | 892/1000 | 89.2% | 232.562 | 244.754 |
| burst 5%, mean 4 | Erasure + shared cap | 885/1000 | 88.5% | 230.866 | 239.911 |

The cap preserves the erasure/FEC path: independent-loss delivery is equal or
within three packets, and the seven-packet burst difference is too small and
not packet-paired because the modes consume different seeded loss events. It
does not solve FEC startup. Fresh models have no learned burst estimate, and
both modes leave about 11% residual application loss in the short burst cell.
That is the next FEC problem; carrying a learned model across nominally fresh
trials would only hide it.

Hysteria2's delivered-packet p95 remains near one RTT because missing datagrams
are counted as loss rather than delayed until recovered. Its 80% delivery at
5% path loss is therefore not lower latency at equal reliability.

Ordered-stream UDP demonstrates the opposite tradeoff:

| Path | Delivery | Median trial p95 | p95 above datagram | Median trial max | Worst max |
| --- | ---: | ---: | ---: | ---: | ---: |
| 226 ms / 1% | 1000/1000 | 557.815 ms | about 329 ms | 609.668 ms | 672.418 ms |
| 226 ms / 5% | 1000/1000 | 792.899 ms | about 563 ms | 844.152 ms | 1192.421 ms |
| 100 ms / 15% boundary | 1000/1000 | 518.849 ms | — | 608.080 ms | 1239.505 ms |

Stream retransmission achieves 100% delivery in these samples by making later
datagrams wait behind the missing byte range. The tail is too large to use TCP
multiplexing or UDP-on-stream as a low-latency mechanism.

## Connection reuse and AnyTLS

At 226 ms / 1% loss, all 10 trials completed:

| Mode | Median cold | Median warm |
| --- | ---: | ---: |
| Pooled QUIC | 1245.737 ms | 230.515 ms |
| Unpooled QUIC | 1491.895 ms | 463.913 ms |

The median warm saving is 233.398 ms, approximately one configured RTT, and
pooled warm trials were tightly grouped from 228.518 to 231.696 ms. Queqiao
should retain the mechanism also worth borrowing from AnyTLS: an authenticated
warm session pool. AnyTLS padding adds bytes and writes rather than removing a
round trip, and TCP multiplexing imports the ordered-stream HOL demonstrated
above. Neither is a latency optimization for this transport.

## Fixed Brutal control

`brutal-no-comp` reproducibly reduced policer drops relative to normal Brutal
under matched seeds: 10/10 trial pairs at 1 ms, 10/10 at 8 ms, and 8/10 at 16
ms. Under a fair sign null, 10/10 in one direction has one-sided probability
1/1024; eight or more of ten has probability 56/1024 (about 0.0547). This is
strong descriptive evidence that removing ACK-rate loss compensation reduces
policer overshoot, not evidence that fixed Brutal is the production design.

The control remains per lane, so configured capacity can multiply. It also
replaces erasure control, which removes the path estimate consumed by adaptive
FEC. Its high goodput and low queue drop are useful experiment controls but do
not satisfy the production invariants together.

## Implemented prototype and invariants

The minimal opt-in implementation has these boundaries:

1. One client or server owns a scheduler set keyed by the existing provider
   path identity. All QUIC connections on that path share one total bucket;
   different paths do not share policy state.
2. Bulk connections also charge a second bucket at `total - reserve`.
   Pooled/control connections charge the total bucket only, keeping the
   reserve available for interactive work.
3. The wrapper admits a packet only when both the inner controller and shared
   scheduler do. It synchronously charges the exact QUIC packet bytes reported
   by `OnPacketSent`.
4. The wrapper forwards `CongestionControlEx` ACK/loss events unchanged. The
   erasure estimate and adaptive FEC path remain owned by the inner controller.
5. The server changes a connection to bulk only after validating `OPEN` or
   `JOIN`; an invalid join cannot claim a different class.
6. Telemetry exports configured total/bulk rates, charged packet bytes,
   overshoot packets, and current debt. Pathsim separately reports ambient
   erasure and bottleneck rejection.
7. A zero cap is the compatible default. Reno is rejected because the current
   QUIC API cannot wrap its internal controller.

This implementation is not a strict NIC wire cap: UDP/IP headers are not
charged, the QUIC handshake precedes controller installation, path probes
bypass it, some ACK/PTO packets can bypass eligibility, and concurrent
connections can each race by roughly one packet. Blocking `PacketConn.Write`
was rejected because quic-go registers send and PTO state before the writer;
delaying there would corrupt RTT/PTO/erasure timing. A strict implementation
belongs before that registration in the apNet quic-go fork.

## Resource observations

The key 226 ms / 1% throughput cells each used 71.91 s wall time. Uncapped
erasure used 25.94 s user, 12.09 s system, and 285,236 KiB peak RSS; the cap
used 27.53 s user, 13.44 s system, and 210,540 KiB peak RSS. The aggregate
one/four-flow proof used 90.54 s wall, 29.80 s user, 21.17 s system, and
238,140 KiB peak RSS. These are whole-process cell measurements on one host,
not normalized transport overhead estimates.

## Reproducible artifacts

The workspace-local authoritative bundle is:

`.amp/in/artifacts/queqiao-low-latency-low-loss-final-20260826`

It contains the 22 corrected raw JSON and logs, `summary.tsv`,
`cross-transport.tsv`, `resources.tsv`, `manifest.txt`, `source-status.txt`,
`source.patch`, and `SHA256SUMS`.

The broader original matrix is:

`.amp/in/artifacts/queqiao-low-latency-bandwidth-20260826`

It contains 87 raw JSON and logs, family TSV files, resources, manifest,
source patch, and checksums. Use its BBR-TUIC, Brutal, and Hysteria2 rows, but
do not use its erasure-controller/FEC rows because of the path-model isolation
defect described above.
