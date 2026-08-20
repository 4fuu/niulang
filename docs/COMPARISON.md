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

## What can be claimed today

- Queqiao has a clear architectural difference: it coordinates a known shared
  endpoint-pair bottleneck and can spend measured parity to avoid WAN RTT.
- The representative campaign is evidence that this approach can produce a
  large bulk-goodput advantage on one real path.
- A current multi-network campaign remains the right next step before making a
  broader performance claim.

For a current claim, run the same alternating workload against all three
stacks, bind every outer socket to the intended physical interface, record
completion rates and tails, and publish the exact commit, toolchain, path, and
configuration. The [benchmarking guide](BENCHMARKING.md) and [network-evidence
guide](CONTRIBUTING-NETWORK-EVIDENCE.md) define that procedure.
