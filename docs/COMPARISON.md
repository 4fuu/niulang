# Comparing Queqiao with UDP-based transports

Queqiao should be compared with complete deployed stacks, not with a
congestion-control algorithm in isolation. TUIC and Hysteria 2 are useful
comparators because they carry proxy traffic over UDP/QUIC-style transports;
they make different choices about path state, recovery, and scheduling.

## Design comparison

| Question | Queqiao | TUIC v5 | Hysteria 2 |
| --- | --- | --- | --- |
| Main carrier | QUIC streams and datagrams, with authenticated TLS/TCP fallback | QUIC/UDP proxy transport | QUIC/UDP proxy transport |
| Optimization scope | Shared client-to-gateway endpoint pair | Usually per connection | Usually per connection |
| Recovery model | Retransmission plus selective sliding-window coding for measured erasure | QUIC stream recovery and protocol-specific UDP behavior | QUIC/UDP recovery and protocol-specific congestion behavior |
| Cross-flow policy | Aggregate pacing, priority, and reactive bulk isolation | Normally supplied by the proxy and its connections | Normally supplied by the proxy and its connections |
| UDP application traffic | QUIC datagrams when available, with bounded relay rescue | UDP relay semantics | UDP relay semantics |
| Project claim | Path-scoped optimization with explicit evidence boundaries | General-purpose proxy transport | General-purpose proxy transport |

These are architectural distinctions, not a ranking. A different path can
reverse any performance result.

## Published historical comparison

The following table is from the six-round real-path campaign recorded in
[the archived report](archive/2026-08-development/MEASUREMENTS-20260816.md).
It used wire protocol 3, a client in China, a fixed US egress, sing-box 1.13.18
for the native TUIC/Hysteria2 stacks, and a path with roughly 1–3% loss and no
capacity knee below 200 Mbit/s. That is not the path model Queqiao currently
targets, and the result does not qualify protocol 1.

### Bulk download, 20-second windows

| Stack | Median goodput | Mean | Min | Max | Trials | Relative to Queqiao |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Queqiao (wire 3) | **143.06 Mbit/s** | 137.31 | 105.77 | 159.33 | 6/6 | 1.00× |
| Hysteria2 | 90.15 Mbit/s | 84.25 | 46.91 | 104.20 | 6/6 | 0.63× |
| TUIC v5 | 76.79 Mbit/s | 74.70 | 47.08 | 87.20 | 6/6 | 0.54× |

Queqiao led all six rounds in that campaign's bulk window. The same report
also found that the advantage was not universal across workloads.

### Warm short-request latency and interactive tail

| Stack | Warm request p50 | SSH p99 under own bulk load | Voice p99 under own bulk load |
| --- | ---: | ---: | ---: |
| Queqiao (wire 3) | 242 ms | **940 ms** | 565 ms |
| TUIC v5 | 239 ms | 662 ms | **326 ms** |
| Hysteria2 | 242 ms | **526 ms** | 452 ms |

The bulk result therefore cannot be advertised as “Queqiao is faster” without
also showing the interactive tail. The campaign itself reported that Queqiao's
interactive degradation was the worst among the genuinely loaded stacks on
that path. The table is useful precisely because it includes that counterexample.

## What can be claimed today

- Queqiao has a clear architectural difference: it coordinates a known shared
  endpoint-pair bottleneck and can spend measured parity to avoid WAN RTT.
- The historical wire-3 campaign is evidence that this approach can produce a
  large bulk-goodput advantage on one real path.
- It is not evidence that protocol 1 is faster than TUIC or Hysteria 2 in
  general. No complete public protocol-1 multi-network comparison is recorded
  yet.

For a current claim, run the same alternating workload against all three
stacks, bind every outer socket to the intended physical interface, record
completion rates and tails, and publish the exact commit, toolchain, path, and
configuration. The [benchmarking guide](BENCHMARKING.md) and [network-evidence
guide](CONTRIBUTING-NETWORK-EVIDENCE.md) define that procedure.
