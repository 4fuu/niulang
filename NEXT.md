# Next optimization

`NEXT.md` defines one active, related optimization group. It is not a backlog.
Finish or stop this group before replacing it with another one.

For every group:

1. Record the starting commits, path settings, seeds, trial counts, and baseline
   measurements here before changing behavior.
2. After testing a candidate, replace the pending result section with the
   baseline, candidate, absolute and percentage differences, completion count,
   tails, CPU, memory, and decision. Record a negative result as carefully as a
   positive one.
3. Move the completed comparison and its interpretation to
   `docs/BENCHMARKING.md` before replacing this file with the next group. Keep
   generated JSON, logs, manifest, machine details, and `source.patch` outside
   the repository, and record their durable location with the completed
   campaign.

## Active group: HTTP/3 startup under loss

### Objective

Reduce cold HTTP/3 connection and lane-establishment latency, and remove
control-path setup failures under burst loss. Preserve the warm connection
pool, adaptive erasure/FEC behavior, application UDP latency, and the real `h3`
carrier.

This group does not change cross-flow page scheduling, sustained-transfer
pacing, FEC policy, or TCP standby policy. Whole-page and bulk burst tails are
a separate optimization group after startup behavior is understood.

### Recorded baseline

Campaign date: 2026-08-27. Tests ran serially on one Linux x86_64 machine with
16 CPUs, 33.7 GB memory, and Go 1.25.13. Each source used its own seeded path
harness with the same arguments, so these results are same-machine comparisons,
not packet-for-packet pairings.

| Version | Commit |
| --- | --- |
| Current HTTP/3 protocol 2 | `c4e2d27b9b5736e8512c84475eebad845bfaadf8` |
| Direct pre-HTTP/3 parent | `a24bdc4b02d7adb23d0ccb8444e4c2133899ebc7` |
| Previous Niulang release | `8055b5671c387912f28d268495aca716d3652399` |
| Upstream Queqiao | `cb720f17156d162ea86e23bff7fa0a514332af20` |

Connection reuse cell: 200 ms RTT, 50 Mbit/s, 5% independent loss, 64 KiB,
10 trials.

| Version | Complete | Cold median | Warm median | Warm maximum |
| --- | ---: | ---: | ---: | ---: |
| Current | 10/10 | 1523.091 ms | 205.104 ms | 206.248 ms |
| Pre-HTTP/3 | 10/10 | 1415.353 ms | 205.235 ms | 406.935 ms |
| Previous release | 10/10 | 1464.473 ms | 205.167 ms | 206.388 ms |
| Upstream | 10/10 | 1263.663 ms | 220.660 ms | 228.807 ms |

The current cold median was 7.61% slower than its direct parent and 20.53%
slower than upstream. Its warm median was unchanged from both Niulang controls
and 7.05% faster than upstream.

UDP startup cell: 100 ms RTT, 50 Mbit/s, 15% loss in mean bursts of six,
1 MiB bulk object, 100 UDP packets of 1200 bytes at 20 ms intervals, seed
50001, 10 trials.

| Version | UDP setup | Delivered | Delivery | Delivered p95 median | Worst delivered |
| --- | ---: | ---: | ---: | ---: | ---: |
| Current | 9/10 | 799/900 | 88.78% | 105.089 ms | 227.236 ms |
| Pre-HTTP/3 | 10/10 | 862/1000 | 86.20% | 104.614 ms | 144.632 ms |
| Previous release | 10/10 | 884/1000 | 88.40% | 104.826 ms | 575.223 ms |

The failed current trial ended during warm-up with `EOF` before sending an
application UDP packet. Seed 57001 then completed in six current repetitions
and once on each Niulang control. Treat this as a startup-stability signal, not
a confirmed deterministic defect.

### Investigation order

1. Add or use stage timings that separate QUIC handshake, HTTP/3 SETTINGS,
   Extended CONNECT request/response, destination open, pool publication, and
   retry delay. Keep this diagnostic data out of the wire protocol unless it
   proves useful as production telemetry.
2. Determine whether the first lane waits on work that pool prewarming can
   complete once per connection, or whether a failed generation is retained
   after SETTINGS or CONNECT loss.
3. Trace the burst-loss `EOF` through pool retirement, GOAWAY, request-stream
   closure, and retry ownership. A retry must use one live generation and must
   not close lanes already accepted on another generation.
4. Make the smallest owning change. Do not increase timeouts, hide setup
   failures, or add unconditional retries merely to improve completion counts.

### Matched validation

Run a five-trial smoke test first. For the final comparison, alternate baseline
and candidate order and retain per-trial results.

1. Connection reuse: 30 trials at 200 ms RTT, 50 Mbit/s, and 5% independent
   loss, pooled and unpooled. Report cold and warm p50/p95/max plus setup
   failures.
2. Burst startup: 30 trials beginning at seed 50001 with the UDP startup cell
   above. Report setup/warm-up failures separately from packets offered,
   residual UDP loss, delivered p95/max, and bulk completion.
3. Low-loss guard: 10 trials at 100 ms RTT, 50 Mbit/s, and 5% independent loss.
   Report bulk goodput, interactive p95, UDP delivery and p95, ambient erasures,
   and sender-induced bottleneck drops.
4. Resource guard: record wall time, user/system CPU, and peak RSS for every
   final cell on an otherwise idle machine.

### Acceptance conditions

- On the matched connection cell, candidate cold p50 is no greater than 105%
  of the direct pre-HTTP/3 result, and cold p95 does not regress from the current
  baseline.
- Candidate warm p50 and p95 remain within 5 ms of the current pooled result.
- The candidate has no setup or warm-up failure in 30 burst trials. If the
  matched current baseline also completes 30/30, do not claim that an
  unobserved failure was fixed; base the decision on stage latency and tails.
- Low-loss bulk median stays within 3% of the matched current baseline. UDP
  delivery decreases by no more than one percentage point, and delivered p95
  increases by no more than 5 ms.
- HTTP/3 remains the QUIC carrier, mTLS authorization remains mandatory, and
  congestion/erasure estimates and adaptive FEC remain available.

### Result

Pending. Replace this paragraph with the final baseline/candidate table,
artifact location, commit range, statistical interpretation, and ship/stop
decision before starting another optimization group.
