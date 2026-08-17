# TCP fallback striping measurement — 2026-08-17

## Outcome

Queqiao can maintain a capability-gated, TCP-only bundle of up to 16
independent TLS/TCP lanes for one fallback flow. The client remains at one lane
by default, the server admits up to 16, and QUIC remains one data lane per flow.
Eight TCP lanes are the recommended opt-in fallback setting: they reduced the
bad tail in the measured path without the connection and replay overhead seen
at 16.

The kernel still owns TCP congestion control. On Linux the server can select a
stock controller such as BBR per accepted socket with `--tcp-congestion=bbr`;
no kernel module or Queqiao congestion-control implementation is required.
User space owns only offset striping, a 1 MiB selective-ACK outstanding window,
and bounded re-injection. A hard lane failure releases its unacknowledged data
immediately. A live TCP lane uses a two-second expiry and re-injects only the
oldest reliable chunk per scheduler sweep, avoiding duplicate storms while the
kernel is still recovering.

## RTX Pro measurement

The test used the controlled Linux RTX Pro gateway and an isolated TCP port.
The production listener was not modified. `tcp_bbr` was loaded for the test,
`ss -tin` confirmed `bbr` on every test lane, and the host-wide default stayed
`cubic`. The transient service, test artifacts, and test-only module load were
removed afterward; the production service remained active.

Test artifact SHA-256:

```text
cee50ddd27c312f1e47acbd037959533c0ff8e8fae2c4cbd2e488766f5b196d3
```

The rotated 10 MiB matrix below used stock BBR, exact-size CacheFly objects,
three samples per lane count, and the final ACK window/two-second expiry. It
predates the last oldest-only re-injection bound, so the duplicate ratios are a
conservative result for the checked-in implementation.

| TCP lanes | Median (s) | Samples (s) | Median physical/logical bytes |
| ---: | ---: | --- | ---: |
| 1 | 13.999 | 13.310, 13.999, 50.251 | 1.000 |
| 2 | 12.973 | 11.829, 12.973, 13.591 | 1.010 |
| 4 | 17.009 | 13.283, 17.009, 29.426 | 1.089 |
| 8 | 11.579 | 11.566, 11.579, 19.400 | 1.088 |
| 16 | 12.790 | 10.909, 12.790, 16.073 | 1.136 |

The final code then completed a 100 MiB transfer on eight lanes in 47.50 s
(17.7 Mbit/s), with 108,170,432 physical lane bytes for 105,200,744 logical
flow bytes (1.028x) and no lane or flow failures. Before the oldest-only bound,
the same long-run test took 75.90 s and carried about 1.8x physical bytes.

A final stable-path 10 MiB spot check measured 13.925 s at one lane, 13.549 s
at eight, and 13.181 s at 16. Multiple lanes cannot exceed a fixed shared
bottleneck; the feature's value is fallback resilience and tail protection,
not a promised throughput multiplier.

## Rollout

1. Deploy the capability-advertising server first with a 16-lane admission
   ceiling and confirm BBR is available before selecting it.
2. Opt clients into `--tcp-fallback-lanes=8` where UDP fallback performance is
   a problem.
3. Keep 16 lanes as an experiment rather than a default; it had more socket and
   duplicate-byte overhead without a better median in the full matrix.
4. Monitor flow failures, lane failures, re-injections, and physical/logical
   byte ratios during rollout.
