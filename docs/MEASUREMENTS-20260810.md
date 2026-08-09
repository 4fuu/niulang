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
production-ready: HTTP completion loss and OpenAI tail failures remain, the
SOCKS ingress is TCP-only (no destination UDP/TUN), QUIC controller/lane
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
