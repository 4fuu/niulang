# Real-path measurements — 2026-08-10

These measurements were run from the China client host to the fixed egress
`icourses-dev` (`23.135.236.244:12443`, TLS SNI `icourses-dev.01.me`). The
outer client was bound to the physical source address `192.168.3.66`, so the
Clash TUN route did not capture the path being measured. The existing Xray,
sing-box, Cloudflare, Nginx, WireGuard, and Clash configurations were not
modified.

The endpoint binary was built as Linux/amd64 from the checked-out source and
verified by SHA-256 before each isolated systemd-run controller instance. The
normal `wanoptd-dev.service` was restored and verified active after the
campaign.

## Method

The repeated-measures matrix used five randomized blocks, flow counts 1, 2,
4, and 8, one-second cooldowns, and the same client/server/destination for
every controller. Each SSH flow transferred 2 MiB. Each HTTP flow requested
the fixed 10 MiB CacheFly object. An HTTP trial counted as successful only
when curl exited zero, returned HTTP 200, and downloaded the complete
10,485,760-byte body; partial bodies and 90-second timeouts stayed in the
denominator. Values below are aggregate wall-clock goodput, not the fastest
individual stream. Five-trial bootstrap intervals for medians use 100,000
resamples; with n=5 they are intentionally wide.

## Matched controller results

| Controller | Workload | N=1 median Mbps (95% bootstrap CI) | N=8 median Mbps (95% bootstrap CI) | Completion |
|---|---|---:|---:|---:|
| Stock apNet QUIC (Reno control) | SSH download | 1.80 (1.12–4.52) | 9.37 (7.17–15.27) | 5/5, 40/40 |
| Stock apNet QUIC (Reno control) | SSH upload | 1.93 (1.38–4.54) | 12.52 (7.62–33.89) | 5/5, 40/40 |
| Stock apNet QUIC (Reno control) | HTTP download | 4.35 (2.16–29.65) | 28.47 (2.55–32.37) | 5/5, 33/40 |
| Adaptive rate controller | SSH download | 3.85 (3.58–3.97) | 29.54 (26.47–30.31) | 5/5, 40/40 |
| Adaptive rate controller | SSH upload | 3.62 (3.60–3.71) | 26.39 (25.17–27.87) | 5/5, 40/40 |
| Adaptive rate controller | HTTP download | 19.55 (2.61–20.43) | 101.84 (26.15–109.74) | 5/5, 40/40 |
| Brutal, 1 MiB/s per QUIC lane | SSH download | 3.52 (3.22–3.71) | 29.54 (27.40–29.74) | 5/5, 40/40 |
| Brutal, 1 MiB/s per QUIC lane | SSH upload | 3.57 (3.13–3.58) | 28.08 (27.02–28.22) | 5/5, 40/40 |
| Brutal, 1 MiB/s per QUIC lane | HTTP download | 9.42 (7.69–9.74) | 67.50 (13.74–75.08) | 5/5, 39/40 |

The stock controller is both slower and much more variable. Adaptive had no
partial HTTP trial in this block. Brutal was stable for SSH but still lost one
of forty HTTP flows; a fixed per-lane target is therefore not a reliability
guarantee and is not an unattended default.

For context, the existing matched endpoint campaign measured Hysteria 2 at
30.2 Mbps (HTTP N=1) and 170.7 Mbps (N=8), and TUIC at 18.7 and 120.6 Mbps.
Those numbers came from a separate campaign and are not a claim that wanopt
currently beats either implementation.

## API and browsing latency

Fresh and serially reused HTTPS requests were measured directly through the
Brutal and Adaptive local SOCKS nodes. A 401 from OpenAI is an expected
application response; other failures and 20-second timeouts count as failed.

| Controller | Phase | Google success; median / p95 s | OpenAI success; median / p95 s |
|---|---|---:|---:|
| Adaptive | Fresh | 10/10; 1.037 / 1.053 | 9/10; 1.085 / 1.097 |
| Adaptive | Reused | 10/10; 1.004 / 1.048 | 9/10; 1.077 / 1.129 |
| Brutal | Fresh | 20/20; 1.602 / 2.236 | 15/20; 1.110 / 1.383 |
| Brutal | Reused | 20/20; 0.246 / 0.800 | 20/20; 0.318 / 19.941 |

The reused-Brutal OpenAI p95 is a severe tail, despite a low median. The
earlier TUIC/Hysteria 2 campaign completed 45/45 requests for both transports;
wanopt therefore does not yet meet the low-latency reliability bar.

## Aggregate pacing and interactive reserve

Both endpoints used the adaptive controller with one shared 8 MiB/s aggregate
budget and a 512 KiB/s reserve for NEW/INTERACTIVE frames. Eight simultaneous
10 MiB HTTP downloads completed 8/8 in 11.88 s (about 56.5 Mbps aggregate).
Ten Google requests issued during the transfer completed 10/10 with median
1.03 s and p95 1.12 s. This demonstrates that the limiter protects an
interactive service rate; the configured rate is intentionally below the
unpaced path peak.

## Mid-session UDP failure and TCP rescue

For a controlled fault, the server temporarily dropped only inbound UDP/12443
with an exact iptables rule; the rule was removed immediately after the trial.
The client used `auto`, adaptive QUIC, and the aggregate limiter. A 100 MiB
HTTP response completed with HTTP 200, curl exit 0, and exactly 104,857,600
bytes in 57.99 s. QUIC dead-path detection is bounded at 15 s and the server
keeps a 45 s replacement grace; the successful run installed a TCP rescue
lane before the application deadline. The normal dev service and firewall
state were verified restored afterward.

## Interpretation and release decision

The implementation now has bounded lane queues, cumulative ACK/replay,
server-side stale-lane retirement, completion tombstones, aggregate pacing,
UDP/TCP new-flow racing, and a tested mid-session TCP rescue. It is still not
production-ready at that point in the campaign: HTTP completion loss and
OpenAI tail failures remained, the SOCKS ingress is TCP-only (no destination
UDP/TUN), and QUIC controller/lane
telemetry now covers active lane count, RTT, and QUIC loss counters but still
lacks bytes-in-flight/pacing/controller-rate signals, and controlled
loss/reordering/MTU/fuzz/resource campaigns need broader coverage. Keep the
existing tunnel as the rollback path and treat adaptive/Brutal as development
profiles until those gates pass.

## Post-hardening deployment smoke

After the campaign, the metrics-enabled Linux/amd64 build was installed in
the isolated `wanoptd-dev.service` and its SHA-256 was verified on both
hosts. A clean 10 MiB CacheFly transfer through one adaptive QUIC lane
returned HTTP 200, exactly 10,485,760 body bytes, and curl exit 0 in 2.736 s
(3.83 MB/s application body rate). The client-side completion counters showed
1 started, 1 completed, 0 failed, and 0 lane failures. During a separate
100 MiB transfer, the live local metrics sample reported one active lane,
approximately 199 ms smoothed RTT, and zero QUIC loss at that sample. These
are deployment and observability checks, not a replacement for the repeated
real-path campaign above.

The final `6301a47` build was then smoke-tested with a fresh Google
`generate_204`: HTTP 204, zero curl errors, 1.004 s total time, and the local
registry reported 1 completed / 0 failed flow and 0 lane failures. The remote
registry reported the same clean completion and zero lane failures after the
normal close handshake.

## Post-pool and single-logical-flow campaign

After the original campaign, the implementation was changed so an explicit
`--quic-pool` option can share one bounded QUIC connection for initial/control
streams. The server now accepts multiple streams per QUIC connection, with a
per-stream `MaxSessions` admission bound. This was tested end-to-end with two
logical SOCKS flows on one pooled connection, including a two-lane first flow;
the integration test passed repeatedly.

The pool was then measured on the real path with matched adaptive control. It
was materially worse for four/eight concurrent 10 MiB flows (about 6.6–7.0
Mbps aggregate in the first trial block) than independent lanes. This is a
controller/path result, not a correctness failure: a shared QUIC controller
must be a real BBR-like implementation before the pool can be the bulk
default. The option is therefore opt-in, and the production default remains
independent lanes for measured bulk performance.

The default independent-lane build was refreshed with five trials at each
concurrent-flow count. The rows below are aggregate wall-clock goodput, with
the same exact-body completion rule as above. The local client selected the
adaptive controller; the restored development service remained on its safe
stock/Reno server control, so this refresh is deliberately labeled a real-path
deployment result rather than a matched-controller comparison:

| Workload | N=1 median Mbps | N=2 median Mbps | N=4 median Mbps | N=8 median Mbps | Completion |
|---|---:|---:|---:|---:|---:|
| Adaptive HTTP, refreshed | 30.06 (7.28–31.36) | 61.20 (60.05–61.80) | 118.92 (116.17–120.34) | 191.56 (179.05–205.50) | 5/5 at every N |

The five N=1 aggregate observations were 31.36, 7.28, 31.18, 30.06, and
29.74 Mbps; that single 7.28-Mbps observation is why the median interval is
wide and why API/web tail reliability remains a release gate.

The new `scripts/bench_single_flow.sh` harness measures one application
connection per trial. A fixed eight-lane bootstrap was deliberately tested and
was poor (median 7.64 Mbps, all 5/5 complete), because simultaneous handshakes
and independent controllers temporarily starved the in-order reassembler. The
behavioral scheduler with the default one-lane NEW phase and negative-marginal-
gain retirement avoided that persistent regression: a 100 MiB single flow
completed 2/2 at 94.0 and 99.4 Mbps (median 96.7 Mbps), and a 10 MiB dynamic
single-flow check completed 5/5 with a 30.01-Mbps median. This is the reason
fixed `--initial-lanes=8` is not a production recommendation; lane growth must
be measured and reversible.

## Post-lifecycle-hardening matched campaign

Commit `49ef13c` added two correctness guards before this campaign. Adaptive
lane growth is now limited to one speculative join per 500-ms scheduler tick,
and a completed server tombstone replays both the peer's final ACK and the
server's own FIN. The latter closes a real tail failure in which the complete
HTTP body arrived but the local flow waited for a remote half-close until the
replacement timeout. The benchmark also waits for child curls before cleaning
its temporary directory when interrupted.

The following 10-MiB CacheFly matrix used five trials at each of N=1/2/4/8,
the exact-body completion rule above, independent QUIC lanes, and matched
client/server controllers on the isolated `:12444` listener. These are raw
medians over the individual flow goodputs (not a claim about a single
application flow's aggregate rate):

| Controller | N=1 median Mbps | N=2 median Mbps | N=4 median Mbps | N=8 median Mbps | Completion |
|---|---:|---:|---:|---:|---:|
| Reno (matched) | 29.87 | 30.08 | 28.27 | 27.17 | 75/75 |
| Adaptive (matched) | 21.02 | 20.86 | 20.53 | 15.68 | 75/75 |
| Brutal, 1 MiB/s/lane (matched) | 9.68 | 9.78 | 9.54 | 9.76 | 75/75 |
| BBR-shaped (matched, stopped after release failure) | 2.90* | 2.31* | 0* | 0* | 5/5, 6/10, 0/20, 0/40 |

`*` BBR values are medians of successful rows only; the run produced 45/75
successful HTTP rows, repeated 120-second timeouts at N=8, flow failures,
completion-watchdog events, and high lane-replacement churn. BBR is therefore
disabled as an automatic choice. Reno is the reliability control, while
Adaptive is the safer experimental controller for this path. Brutal is useful
only with an operator-tested rate and an aggregate budget; it is not a
general-purpose default.

The lifecycle counters after the corrected Adaptive and Brutal campaigns were
75 started, 75 completed, 0 failed, and 0 completion timeouts at both
endpoints. Adaptive recorded five local and two remote lane-failure events in
the final matrix, with two bounded replacements; these did not affect logical
completion. The earlier Adaptive campaign before the tombstone fix recorded
one to three local failures per 75-flow block and is retained as the
regression evidence that motivated the change.

## Fresh, reused, and interactive latency after hardening

With matched Adaptive control and the fixed US egress, 30 fresh Google
`generate_204` requests through QUIC were 30/30 HTTP 204, with median 1.035 s
and p95 1.047 s. The first request on one reused HTTPS connection took 1.037 s;
the following 19 requests were approximately 200 ms each (the measured WAN
RTT). Forced TCP was 20/20 successful, with fresh median 1.225 s and p95
1.243 s. The extra fresh latency is handshake cost, not a one-second physical
RTT lower bound.

During eight simultaneous 10-MiB downloads, a reused Google connection stayed
at 20/20 success: after the initial 1.088-s request, the median remained about
0.204 s and the p95 was 0.211 s. The eight bulk flows were 8/8 complete, with
per-flow rates from 1.60 to 2.63 MB/s. This is evidence that the current
classifier/scheduler does not starve an already-established interactive
connection, but it is not yet a controlled packet-loss or multi-hour soak
campaign.

An exploratory 1-MiB POST to `httpbin.org` returned HTTP 200 and transferred
the complete request body; it took 6.86 s through Adaptive. Public upload
endpoints impose application limits and are not suitable as a throughput
oracle, so a dedicated US-side upload sink is still required for a rigorous
single-stream upload matrix.

## Release status after this run

The unbounded adaptive-join and tombstone FIN bugs are fixed and covered by
integration tests. The project remains a development release: SOCKS5 ingress
is still TCP CONNECT only (no UDP ASSOCIATE, destination UDP, TUN, or VLESS),
HTTP/3 preservation is not implemented, BBR is unsafe on the measured path,
and controlled loss/reordering/MTU, prolonged-soak, and dedicated upload
campaigns remain release gates. Flow-idle/lifetime limits and timeout metrics
are now implemented, but their resource-pressure behavior still needs a
dedicated soak campaign. The existing
`wanoptd-dev.service` on `:12443` was not changed by the isolated controller
tests and remains the rollback path.

## Latest development deployment

The latest checked-out commit, `5213dd9`, was built as Linux/amd64 and
installed only as `/usr/local/bin/wanoptd` for `wanoptd-dev.service`; the
previous binary is retained at `/usr/local/bin/wanoptd-rollback-fdcb1b0`.
The deployed SHA-256 is
`672a1f0392177c3fc3ab575f0f01942053e0054f448019c3029355abe7875d38`.
After restart, a fresh Google `generate_204` returned HTTP 204 in 1.043 s and
a one-flow 10-MiB CacheFly smoke returned HTTP 200 with exactly 10,485,760
bytes. Local and remote metrics both reported zero failed flows and zero flow
timeouts. `wanoptd-dev`, Xray, sing-box, Cloudflare, Nginx, and the other
pre-existing services were all active; no temporary `:12444` benchmark unit
was left running.
