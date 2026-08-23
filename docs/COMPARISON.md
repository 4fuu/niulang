# Comparing Queqiao with UDP-based transports

Queqiao should be compared with complete deployed stacks, not with a
congestion-control algorithm in isolation. TUIC and Hysteria 2 are useful
comparators because they carry proxy traffic over UDP/QUIC-style transports;
they make different choices about path state, recovery, and scheduling.

## Design comparison

| System | Main carrier | Optimization scope | Recovery model | Cross-flow policy | UDP application traffic |
| --- | --- | --- | --- | --- | --- |
| **Queqiao** | QUIC streams/datagrams with authenticated TLS/TCP fallback | Shared client-to-gateway endpoint pair | Selective sliding-window coding plus retransmission | Aggregate pacing, priority, and reactive isolation | QUIC datagrams when available, with bounded relay rescue |
| TUIC v5 | QUIC/UDP proxy transport | Usually per connection | QUIC stream recovery and protocol-specific UDP behavior | Normally supplied by the proxy and its connections | UDP relay semantics |
| Hysteria 2 | QUIC/UDP proxy transport | Usually per connection | Protocol-specific UDP/QUIC recovery | Normally supplied by the proxy and its connections | UDP relay semantics |

These are architectural distinctions, not a ranking. A different path can
reverse any performance result.

## Representative benchmark

The following table is from a six-round real-path campaign recorded in
[the benchmark report](archive/2026-08-development/MEASUREMENTS-20260816.md).
It used a client in China, a fixed US egress, sing-box 1.13.18 for the native
TUIC/Hysteria2 stacks, and a path with roughly 1–3% loss and no capacity knee
below 200 Mbit/s. Treat it as representative design evidence, not a universal
performance guarantee; repeat the benchmark on the current release and your
own path before making a deployment decision.

### Bulk download, 20-second windows

| System | Median goodput | Mean | Min | Max | Trials | Relative to Queqiao |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| **Queqiao** | **143.06 Mbit/s** | 137.31 | 105.77 | 159.33 | 6/6 | 1.00× |
| Hysteria2 | 90.15 Mbit/s | 84.25 | 46.91 | 104.20 | 6/6 | 0.63× |
| TUIC v5 | 76.79 Mbit/s | 74.70 | 47.08 | 87.20 | 6/6 | 0.54× |

Queqiao led all six rounds in that campaign's bulk window. The same report
also found that the advantage was not universal across workloads.

### Warm short-request latency and interactive tail

| System | Warm request p50 | SSH p99 under own bulk load | Voice p99 under own bulk load |
| --- | ---: | ---: | ---: |
| **Queqiao** | 242 ms | **940 ms** | 565 ms |
| TUIC v5 | 239 ms | 662 ms | **326 ms** |
| Hysteria2 | 242 ms | **526 ms** | 452 ms |

The bulk result therefore cannot be advertised as “Queqiao is faster” without
also showing the interactive tail. The campaign itself reported that Queqiao's
interactive degradation was the worst among the genuinely loaded stacks on
that path. The table is useful precisely because it includes that counterexample.

## Fixed-rate erasure coding: kcptun

kcptun is the comparator closest to what Queqiao actually does differently.
Both spend parity to avoid a round trip, and they choose how much in opposite
ways: Queqiao sizes its code from a measured erasure floor and revises it while
a flow runs, while a kcptun deployment fixes a ratio in advance. TUIC and
Hysteria 2 do not cover that axis.

The campaign below ran on 2026-08-23 from a client in China to the same
maintainer-controlled US egress, both ends on `main-126eb5c` (Go 1.25.13, wire
1), using `scripts/bench_live_matched.sh` to alternate a 4 MiB object between
the two SOCKS5 endpoints, twelve rounds each, order swapped every round. The
path measured 192-276 ms round trip and about 5% ICMP loss during the window.
kcptun ran `-mode fast3 -datashard 10 -parityshard 3 -nocomp`; its build came
from a restoration of the upstream repository, which has been withdrawn.

| kcptun send/receive window | Queqiao median | kcptun median | Matched rounds | Ratio |
| --- | ---: | ---: | ---: | ---: |
| 128/512, the implementation's default | **48.60 Mbit/s** | 5.09 Mbit/s | 12/12 to Queqiao | 9.64× |
| 2048/2048, sized to this path | **50.23 Mbit/s** | 25.36 Mbit/s | 12/12 to Queqiao | 2.10× |

Every trial in both campaigns completed with the exact object length; the sign
test over matched rounds gives p=0.0005 in each. Queqiao's own median moved by
3% between the two campaigns, which is the internal check that the difference
between the rows belongs to kcptun's configuration rather than to the path
moving underneath.

**The window matters more than the parity.** kcptun's default send window of
128 packets caps a 210 ms path at about 6.6 Mbit/s whatever the transport does,
and the first row measures that ceiling rather than the code. Quoting it alone
would be a rigged comparison. Sized to the path's delay-bandwidth product, the
same build is five times faster and the gap falls from 9.6× to 2.1×.

The limits of this cell are as important as the numbers. It is one path, one
object size, one direction, and one fifteen-minute window; the parity ratio was
not swept live; kcptun ran behind a loopback relay so that both transports left
by the same physical interface, costing it tens of microseconds against a
210 ms path; and the Queqiao client was simultaneously carrying the operator's
real traffic, roughly seventy concurrent flows, while the kcptun tunnel carried
only the benchmark. That asymmetry works against Queqiao, which still led every
round.

## What can be claimed today

- Queqiao has a clear architectural difference: it coordinates a known shared
  endpoint-pair bottleneck and can spend measured parity to avoid WAN RTT.
- The representative campaign is evidence that this approach can produce a
  large bulk-goodput advantage on one real path.
- Against a fixed-rate code on one real path, the advantage survives giving the
  comparator a window sized for that path, but it shrinks from 9.6× to 2.1×
  when it is given one.
- A current multi-network campaign remains the right next step before making a
  broader performance claim.

For a current claim, run the same alternating workload against all three
stacks, bind every outer socket to the intended physical interface, record
completion rates and tails, and publish the exact commit, toolchain, path, and
configuration. The [benchmarking guide](BENCHMARKING.md) and [network-evidence
guide](CONTRIBUTING-NETWORK-EVIDENCE.md) define that procedure.
