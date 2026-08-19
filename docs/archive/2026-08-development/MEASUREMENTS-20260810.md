# Real-path measurements — 2026-08-10

> [!WARNING]
> **Historical protocol-3 measurement notebook.** It predates the first public
> protocol and contains development-time conclusions, not release claims.

> Historical protocol-3 record. HELLO/OPEN_FAST and its CLI configuration were
> removed by the clean-slate protocol-4 identity design.

These measurements were run from the China client host to the fixed egress
`<EGRESS-HOST>` (`<EGRESS-IP>:12443`, TLS SNI `<EGRESS-SNI>`). This
document contains results from more than one measurement window. The earlier
pilot sections used the then-valid physical source address `<CLIENT-LAN-IP>`;
the final matched TUIC/QUEQIAO campaign and the UDP smoke test used the current
physical source address `<TETHER-CLIENT-IP>`, with the Clash TUN route excluded from
the outer connection. The existing Xray, sing-box, Cloudflare, Nginx,
WireGuard, and Clash configurations were not modified.

The endpoint binary was built as Linux/amd64 from the checked-out source and
verified by SHA-256 before each isolated systemd-run controller instance. The
normal `queqiaod-dev.service` was restored and verified active after the
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
Those numbers came from a separate campaign and are not a claim that queqiao
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
queqiao therefore does not yet meet the low-latency reliability bar.

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
the isolated `queqiaod-dev.service` and its SHA-256 was verified on both
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
integration tests. The project remains a development release: SOCKS5 UDP
ASSOCIATE is implemented with bounded in-session rescue, but the rescue opens
a fresh remote association and cannot recover datagrams lost during the
transition. TUN/VLESS ingress and HTTP/3 preservation are not implemented,
BBR is unsafe on the measured path,
and controlled loss/reordering/MTU, prolonged-soak, and dedicated upload
campaigns remain release gates. Flow-idle/lifetime limits and timeout metrics
are now implemented, but their resource-pressure behavior still needs a
dedicated soak campaign. The existing
`queqiaod-dev.service` on `:12443` was not changed by the isolated controller
tests and remains the rollback path.

## Latest development deployment

The lifecycle-hardening commit `efa358a` was built as Linux/amd64 and
installed as `/usr/local/bin/queqiaod` for `queqiaod-dev.service`; the pre-fix
binary is retained at `/usr/local/bin/queqiaod-rollback-pre-efa358a` (and the
older documented rollback remains available). The deployed SHA-256 is
`a7b183ffb2891d2495056fa6adaa4697d3cefd0c6e0700d7a2b678bb52180475`.
After restart, a fresh Google `generate_204` returned HTTP 204 in 1.043 s and
a one-flow 10-MiB CacheFly smoke returned HTTP 200 with exactly 10,485,760
bytes. Local and remote metrics both reported zero failed flows and zero flow
timeouts. `queqiaod-dev`, Xray, sing-box, Cloudflare, Nginx, and the other
pre-existing services were all active; no temporary `:12444` benchmark unit
was left running.

## Final current-window TUIC comparison and UDP smoke

The following measurements were taken after the UDP-association hardening,
using source revision `fd2ffac` (the UDP-hardening build immediately before
the deployed address-resolver revision). The current service is
`55363f9` (`queqiaod dev-55363f9`, remote binary SHA-256
`0de7071fee1601d8b28ffaebe7b6c902b5b70d99b3d088ecfb2342f7a5949113`); the
transport code is unchanged between those two revisions. The local outer
socket was explicitly bound to `<TETHER-CLIENT-IP>`. These trials are a new,
matched time window and must not be mixed with the older pilot numbers above.

The workload was the exact 10 MiB CacheFly object
`https://cachefly.cachefly.net/10mb.test`; a trial counted as complete only
when HTTP returned 200, curl exited zero, and exactly 10,485,760 body bytes
were received. Five trials were run for each profile:

| Profile | Completion | Completion times (s) | Median completion | Median goodput |
|---|---:|---|---:|---:|
| TUIC, existing server inbound `:2444` | 5/5 | 5.763, 2.590, 8.405, 10.192, 7.282 | 7.282 s | 11.52 Mbps |
| QUEQIAO safe default, one lane | 5/5 | 15.743, 18.233, 15.178, 31.863, 16.560 | 16.560 s | 5.07 Mbps |
| QUEQIAO forced QUIC, one lane | 3/5 | 26.688, 14.747, 26.819, 88.897 partial, 10.002 no body | — | — |
| QUEQIAO forced TLS/TCP, one lane | 3/5 | 12.708, 18.044, 23.144, 10.002 no body, 10.002 no body | — | — |

The forced profiles are reported for diagnostic separation, not as competing
production recommendations. On this window TUIC had the lower median
completion time and higher median goodput. QUEQIAO's `auto` mode completed all
five safe-default trials, while the forced QUIC and forced TCP runs each had
two application failures. The path is sufficiently variable that a single
five-trial median is not a capacity guarantee; longer repeated blocks, loss
injection, and tail-percentile reporting are required before selecting an
unattended controller.

Three SOCKS5 UDP-associate DNS queries to `1.1.1.1:53` through QUEQIAO `auto`
also completed with valid 71-byte replies and matching transaction IDs. Both
local and remote metrics reported:

```text
queqiao_flows_started_total 3
queqiao_flows_completed_total 3
queqiao_flows_failed_total 0
queqiao_bytes_up_total 87
queqiao_bytes_down_total 183
queqiao_lane_failures_total 0
queqiao_lane_replacements_total 0
```

For the final smoke the client used `--local-address auto`; it selected the
then-current physical `<TETHER-CLIENT-IP>` and completed over QUIC with
`queqiao_fallbacks_total 0`. Earlier in the same window, a fixed address became
invalid when DHCP moved the host to `<TETHER-PEER-IP>`, producing
`bind: can't assign requested address`; this is the operational failure that
the automatic resolver is intended to prevent. If multiple physical IPv4
addresses are active, configure `if:NAME` or a literal address. The production
service was not changed to capture the Clash TUN route automatically.

This validates bounded UDP framing, US-side resolution, peer pinning, graceful
dissociation, and the code-level rescue path (the controlled fault test uses a
local dual-stack server). It does not yet demonstrate recovery of every
intermittent-loss pattern on the real China-US path; native QUIC DATAGRAM and
loss/reordering campaign coverage remain release gates. The service was left
active on `:12443` and the previous binary is available at
`/usr/local/bin/queqiaod-rollback-fd2ffac` on the server.

## Real-path mid-session UDP-to-TCP rescue

With the deployed `55363f9` client and server, a temporary local client used
`--transport auto`, `--fallback-delay 2s`, `--local-address auto`, and one
SOCKS UDP association. A 71-byte DNS reply through the initial QUIC lane was
verified first. The server then installed exactly one temporary firewall rule,
`INPUT -p udp --dport 12443 -j DROP`, leaving TLS/TCP/SSH untouched. After
QUIC dead-path detection and one bounded rescue attempt, a second DNS query
through the unchanged local SOCKS UDP endpoint completed in 9.51 s. The local
metrics at recovery were:

```text
queqiao_lane_failures_total 1
queqiao_lane_replacements_total 1
queqiao_fallbacks_total 1
queqiao_udp_association_reconnects_total 1
queqiao_udp_association_rescue_failures_total 0
```

The local association then closed cleanly with one completed and zero failed
flows. On the server, the dead original relay was counted as one failed flow
and the fresh TCP association as one completed flow (three total UDP
associations in the process, two completed and one failed). The firewall rule
was removed in the test's cleanup path and independently verified absent. This
is evidence of bounded same-SOCKS-endpoint rescue, not lossless datagram
resumption: packets in flight at the QUIC transition can still be lost, and
the old remote relay is reclaimed by its idle bound.

## Final quality checks

At the final revision the following checks passed:

```text
go test ./...
go test -race ./...
go vet ./...
protocol decoder fuzz test (2 s)
multipath reassembler fuzz test (2 s)
bash -n scripts/bench_http.sh scripts/bench_single_flow.sh
```

These are software-quality gates only. They do not waive the real-path
release gates in the interpretation above.

## Post-deployment single-flow validation

After deploying `fd2ffac`, the local client on `127.0.0.1:12081` was run in
`auto`, Reno, one-lane mode and the exact 10 MiB CacheFly object was requested
five times with the same complete-body criterion. This is a validation block,
not a replacement for the earlier matched TUIC campaign:

| Trial | HTTP | Body bytes | Total seconds | Complete |
|---:|---:|---:|---:|---:|
| 1 | 200 | 10,485,760 | 18.642567 | yes |
| 2 | 200 | 10,485,760 | 20.220707 | yes |
| 3 | 200 | 10,485,760 | 24.775699 | yes |
| 4 | 200 | 10,485,760 | 18.091671 | yes |
| 5 | — | 261,452 | 180.435546 | no, curl exit 28 |

The block completed 4/5. The local post-block counters were six started, four
completed, two failed, 42,374,367 bytes down, six lane failures, and four lane
replacements (the counters also include the immediately preceding UDP smoke).
This is consistent with the project’s release decision: the bounded rescue
kept several flows complete, but tail reliability is not yet a production
guarantee. The host’s DHCP address changed during the broader work between
`<TETHER-CLIENT-IP>` and `<TETHER-PEER-IP>`; when the configured `--local-address` is not
present, every outer dial fails locally with `bind: can't assign requested
address`. A production client must discover the physical egress binding or
receive a configuration update on DHCP change rather than silently retaining
an obsolete address.

## QUIC stream-accept timeout regression and current loss window

Commit `efa358a` also fixes a server lifecycle bug found by a debug-log
campaign. The QUIC server used the per-stream `HandshakeTimeout` while waiting
for the next stream on an already-established connection. With the default
10-second value, a long transfer with no new stream caused the server to close
the entire active connection with `queqiao session complete`. This produced
repeated lane failures and replacements at approximately ten-second
intervals. The fix waits for the next stream with the server context instead;
the accepted stream still has its bounded authentication deadline, and the
connection remains bounded by QUIC idle timeout and shutdown. A regression
test holds an established pooled flow idle for 750 ms while the server
handshake timeout is 300 ms, then verifies a successful transfer.

The fix was deployed only to the existing development service on `:12443`;
the service stayed active and the previous binary was copied to
`/usr/local/bin/queqiaod-rollback-pre-efa358a`. It does not select BBR or alter
the existing Clash, Xray, sing-box, Cloudflare, Nginx, or WireGuard services.

Immediately after deployment, a five-trial one-lane 10-MiB CacheFly block was
run through the existing local node on `127.0.0.1:12081` (60-second timeout,
exact-body completion rule). All five trials returned HTTP 200 but timed out
with partial bodies: 2,703,324; 2,571,560; 2,473,256; 2,162,652; and 2,653,480
bytes. Completion was 0/5; median delivered bytes were 2,571,560 (about
0.343 Mb/s over the timeout). The remote metrics recorded five failed flows
and five lane failures. This is a valid current-window loss result, not an
accept-timeout artifact: the server logs show packet loss and no artificial
ten-second connection close.

For a matched isolated comparison on `:12444`, the corrected server was run
once with stock Reno and once with the experimental BBR controller. Reno
completed 0/5, with a median 2,571,560 bytes delivered in 60 seconds (about
0.343 Mb/s). BBR completed 0/5, with a median 2,161,960 bytes (about 0.288
Mb/s); it was slightly worse and remains disabled for production. The BBR
block had zero QUIC loss counters on the surviving server connection but did
not establish a useful delivery rate; that is evidence against automatic BBR
selection, not evidence that the controller is correct under all paths.

A fresh five-trial TUIC control through the existing `127.0.0.1:12086` node
completed 5/5 in 49.262, 24.309, 12.089, 7.969, and 8.301 seconds (median
12.089 s, about 6.94 Mb/s). The wide within-window spread confirms that the
China-to-US path is currently highly variable; the QUEQIAO result still fails
the bulk reliability bar, and no production performance claim is made from a
single block.

As a separate post-deployment latency smoke, ten fresh Google
`generate_204` requests through the unchanged local `auto` node completed
10/10 with HTTP 204. Total times were 1.332, 1.805, 1.843, 1.325, 1.533,
1.282, 1.358, 1.807, 1.830, and 2.263 seconds (median 1.668 s, maximum
2.263 s). This is a small availability check rather than a latency campaign;
it confirms that the server lifecycle change did not break short API/web
requests while the bulk block was loss-limited.

## Corrected BBR sampler and bounded recovery follow-up

The first corrected-BBR block above still contained two controller defects.
`quic-go` defines `ByteCount` as signed `int64`, while the overflow guards
used an unsigned all-bits-one value; positive ACK and send accounting could
therefore saturate to `-1`. In addition, the delivery sample was too dependent
on packet-to-ACK RTT. Commit `8b75465` uses signed-safe saturation and a
cumulative packet-level ACK slope capped by the send slope. A synthetic
200-ms delayed-ACK test now rejects the old one-packet-per-RTT estimate.

The corrected controller was then tested only on the isolated `:12444`
listener, with BBR on the US server and one client lane. The five exact-body
CacheFly trials were:

```text
19.451104 s, 16.076927 s, 6.955731 s, 16.691376 s, 8.812488 s
```

Completion was 5/5; median completion was 16.077 s (about 5.22 Mb/s). Both
client and server reported six completed flows, zero failed flows, zero lane
failures, and zero lane replacements (the client block included one preceding
smoke). Five fresh Google `generate_204` requests through the same corrected
BBR node were also 5/5 HTTP 204, with times 1.299, 1.276, 2.341, 1.661, and
1.318 s (median 1.318 s).

The prior corrected-sampler block without the recovery budget had matched
bulk rates but produced 38 lane failures and 37 replacements, including a
post-body replacement storm. Commit `bd1b808` bounds recovery to eight
attempts and applies exponential backoff after successful-but-immediately-
closed joins as well as failed handshakes. The follow-up block is evidence
that this guard prevents churn, not proof of lossless resume or universal BBR
stability. BBR remains opt-in and is not selected by `queqiaod-dev.service`;
controlled loss/reordering, interactive-under-bulk, upload, soak, and
multi-path campaigns remain release gates.

## Latest controlled loss-window matrix (b053b96)

After the final-ACK cleanup fix (`b053b96`), a fresh isolated Linux/amd64
build was installed as `/usr/local/bin/queqiaod-b053b96` and tested on the
temporary QUIC listener `<EGRESS-IP>:12444`. The live
`queqiaod-dev.service` on `:12443`, TUIC listener, Xray, sing-box, Cloudflare,
Nginx, WireGuard, and Clash profile were not changed. The client was bound
with `--local-address auto`; all trials used one physical China-to-US path
and the exact 10 MiB CacheFly object. A row was complete only for curl exit 0,
HTTP 200, and exactly 10,485,760 bytes. Each N=1 block contained five serial
trials; each N=2 block contained five pairs of concurrent trials. The timeout
was 120 s, and incomplete rows stayed in the denominator for completion
reliability.

| Profile | N=1 completion | N=1 completion times (s) | N=1 successful goodput (Mb/s) | N=2 completion | N=2 aggregate goodput (Mb/s) |
|---|---:|---|---|---:|---|
| TUIC control | 5/5 | 25.233, 12.874, 12.488, 10.746, 13.614 | 3.324, 6.162, 6.516, 6.717, 7.806 (median 6.516) | 10/10 | 3.675, 4.631, 7.209, 7.799, 8.124 (median 7.209) |
| QUEQIAO BBR-shaped, independent lane | 4/5 | 57.017, 56.709, failed at 10.001, 75.807, 64.055 | 1.107, 1.310, 1.471, 1.479 (successful median 1.390) | 10/10 | 2.269, 2.612, 2.653, 3.153, 3.535 (median 2.653) |
| QUEQIAO BBR-shaped, pooled control stream | 5/5 | 33.645, 46.244, 39.410, 48.946, 49.860 | 1.682, 1.714, 1.814, 2.129, 2.493 (median 1.814) | 10/10 | 1.601, 1.815, 2.082, 2.196, 2.319 (median 2.082) |
| QUEQIAO Reno control | 0/5 | 1,015,808--1,310,702 bytes delivered at the 120-s deadline; one dial failed at 12.803 s | no complete row | 0/10 | 999,424--1,260,858 bytes per flow at the 120-s deadline |

These are not capacity estimates: the China-to-US path was visibly in a
severe loss/throttle window, and no configuration was tested simultaneously
with another configuration. They do establish the current release decision:
the custom BBR sampler and pooled mode are not safe automatic choices, and
the stock Reno controller can fail closed under this window. TUIC remains the
measured bulk control. BBR's N=2 rows completed only because the bounded
recovery/tombstone path eventually delivered the logical body; its client
metrics still recorded substantial lane churn during the block, so completion
alone must not be treated as health.

For the same window, ten fresh Google `generate_204` requests gave these
total-time observations:

| Profile | HTTP success | Times (s) | Median / p95 (s) |
|---|---:|---|---:|
| TUIC | 10/10 | 0.489, 1.025, 0.477, 0.474, 0.679, 0.666, 0.541, 0.723, 0.515, 1.326 | 0.592 / 1.326 |
| QUEQIAO BBR-shaped | 10/10 | 2.023, 4.936, 3.191, 2.687, 1.818, 1.465, 1.880, 3.086, 2.087, 4.422 | 2.355 / 4.936 |
| QUEQIAO Adaptive | 10/10 | 2.232, 3.874, 1.908, 3.570, 1.658, 7.472, 2.310, 2.147, 1.617, 1.980 | 2.139 / 7.472 |
| QUEQIAO Brutal, 1 MiB/s/lane | 10/10 | 2.760, 3.908, 7.096, 4.156, 7.072, 1.606, 2.820, 5.890, 2.856, 3.599 | 3.658 / 7.096 |

The latency observations reinforce the workload policy: a fixed-rate bulk
controller creates queueing delay for fresh short requests, while a custom
controller that is loss-limited can have a several-second handshake/tail. A
production client should therefore keep a low-latency control profile for
one-shot and interactive traffic, promote a flow only after a measured byte
and dwell threshold, and make promotion reversible when RTT or loss violates
the interactive budget. No custom controller should be selected globally
without a path-health gate.

All temporary `:12444`, `:19093`, and `:19094` resources were stopped after
the campaign. The fixed-egress development service was verified active.

## Controller-telemetry smoke

The same `995e5c2` Linux/amd64 build was used for a non-comparative 100 MiB
BBR telemetry smoke. The response was intentionally allowed to run for 120 s
and returned HTTP 200 with 40,189,185 bytes before curl timed out. At a
three-second scrape, the client-side sender reported smoothed RTT 183 ms,
maximum delivery estimate 334,837 B/s, pacing 334,837 B/s, congestion window
6,099 bytes, 43 bytes in flight, and no recovery. The US-side sender reported
smoothed RTT 195 ms, maximum delivery estimate 15,657,553 B/s, pacing
19,571,941 B/s, congestion window 106,355 bytes, 108,789 bytes in flight,
1,200 bytes/one packet lost, and recovery active. The directional difference
is expected because each QUIC endpoint controls its own send direction; it is
also precisely the information needed to distinguish a sender-controller
problem from a receiver-side or path bottleneck. This smoke is observability
evidence, not a performance claim.

## Dedicated US-side upload sink

To avoid public-service shaping and application limits, a temporary bounded
HTTP POST sink was run on `<EGRESS-IP>:28080`. It accepted a fixed number
of requests, required an exact `Content-Length`, and was stopped immediately
after each block; the port was verified closed afterward. Each request sent
exactly 10,485,760 zero bytes from China through one QUIC lane.

| Profile | Completion | Upload times (s) | Median goodput |
|---|---:|---|---:|
| TUIC control | 5/5 | 12.845, 14.935, 11.961, 12.440, 13.411 | 6.53 Mb/s |
| QUEQIAO live safe default (Reno/auto) | 5/5 | 26.696, 13.945, 16.410, 39.882, 22.101 | 3.79 Mb/s |
| QUEQIAO BBR-shaped, isolated `:12444` | 5/5 | 54.431, 61.711, 30.195, 29.500, 39.423 | 2.13 Mb/s |

The sink confirms that the upload direction is also materially weaker than
TUIC in this measurement window. BBR completed the logical request but was
slower and more variable; it remains experimental. The script is
[`scripts/upload_sink.py`](../../../scripts/upload_sink.py), deliberately bounded
and intended only for a temporary operator-controlled listener.

## Interactive requests during eight-flow bulk

As a final current-window stress check, eight concurrent 10 MiB downloads ran
while ten serial Google `generate_204` requests were issued through the same
local SOCKS endpoint. The 120-second exact-body rule was retained for every
bulk flow.

| Profile | Bulk completion | Bulk bytes per flow at deadline | Interactive success | Interactive times (s), median / maximum |
|---|---:|---|---:|---:|
| TUIC control | 0/8 | 7,503,800--7,602,104 | 10/10 | 3.151 / 8.468 |
| QUEQIAO live safe default | 0/8 | 999,424--3,440,604 | 10/10 | 3.571 / 6.534 |

Neither endpoint provided a useful bulk result in this severe loss window;
the interactive rows are therefore tail-latency observations under a
stressed path, not evidence that QUEQIAO has overtaken TUIC. A production
policy must gate bulk admission on path health and preserve an explicit
interactive reserve rather than infer safety from successful HTTP status
alone.

## TUIC-aligned BBR port (isolated, same-day follow-up)

The `bbr-tuic` controller is a separate opt-in implementation. It ports the
state machine and estimator used by TUIC's `quinn-congestions` BBR: a
send/ACK-rate minimum estimator, ten-round max filter, ACK-aggregation height,
startup/full-bandwidth detection, recovery conservation/growth, randomized
ProbeBW cycling, and BDP-based ProbeRTT. The public `quic-go` callback does not
provide an application-limited bit, so this implementation uses cumulative
send/ACK deltas and only marks a drained flight after an RTT-scale idle gap as
app-limited. It was deployed only on an isolated remote UDP listener `:12445`
and local SOCKS listeners `127.0.0.1:12087`/`:12088`; the live `:12443`
service was not changed. The binary SHA-256 was
`7f8049c930330af169d4a83b8c75e53fa3ffab9fa1bcdfefb027587d7151b48a`.
That first candidate was later found to apply `high_gain` to its IW/RTT
fallback before a delivery sample existed. The final candidate corrects this
to TUIC's IW/RTT initialization; its SHA-256 is
`631119d8b8ea48900f7514d04f5ba068c410d51937da0e4a04bfac7a15bbf372`.

The path changed during this block, so the contemporaneous TUIC rows below
are the useful control. Each result is an exact 10-MiB CacheFly body and a
complete row requires HTTP 200, curl exit 0, and exactly 10,485,760 bytes.

| Profile | N=1 completion | N=1 goodput (Mb/s) | N=2 completion | N=2 aggregate goodput (Mb/s) |
|---|---:|---|---:|---|
| TUIC, contemporaneous control | 5/5 | 1.737, 5.810, 6.022, 5.493, 2.691 (median 5.493) | not rerun in this block | not rerun |
| QUEQIAO `bbr-tuic`, independent lane | 5/5 | 3.657, 3.126, 1.767, 3.424, 2.685 (median 3.126) | 9/10 flows complete; one pair had one dial failure | 5.660, 5.989, 5.442, incomplete, 4.612 (successful-pair median 5.660) |

The port is materially better than the old custom BBR in the previous
campaign, and its two-flow successful pairs are close to the contemporaneous
TUIC range. It still has lower single-flow median goodput and one two-flow
dial failure; this is not a deployment gate pass. More importantly, ten fresh
Google `generate_204` requests through the independent-lane port took
`1.516, 2.001, 2.811, 3.376, 4.798, 6.124, 6.677, 6.863, 7.831, 9.715` seconds
(median 5.750 s). A persistent pooled `bbr-tuic` control stream reduced this
to `1.102, 1.594, 1.699, 2.189, 2.795, 3.102, 3.269, 3.685, 3.696, 4.649`
seconds (median 2.998 s), still well behind contemporaneous TUIC fresh
requests (`0.478, 0.497, 0.496, 0.499, 0.509, 0.545, 0.695, 0.704, 1.044,
1.569`; median 0.620 s). This isolates a major architectural issue: a
controller port alone cannot remove the per-flow PEP/session and destination
dial latency. The pooled stream remains useful for amortization, but pooled
mode is not automatically enabled for bulk because it has not passed the
single-flow and failure gates.

Five serial 10-MiB uploads to the bounded US-side sink completed `5/5` in
`13.818, 16.059, 17.779, 16.176, 19.922` seconds (median 16.176 s,
approximately 5.19 Mb/s). This is better than the earlier QUEQIAO BBR upload
median but below the same campaign's TUIC control and remains experimental.

The corrected IW/RTT candidate then completed three exact 10-MiB confirmation
downloads in `30.709, 20.976, 30.856` seconds (`2.732, 3.999, 2.719` Mb/s;
median 2.732 Mb/s). Eight of ten independent fresh Google requests succeeded
in `1.151, 2.157, 2.842, 2.957, 4.002, 5.163, 5.623, 7.917` seconds (successful
median 3.480 s); two QUIC connection attempts hit the ten-second handshake
deadline before the controller is installed. This confirms that the
high-gain correction is appropriate model fidelity but does not solve the
outer-handshake reliability or per-flow latency problem. A production AUTO
profile must rescue these UDP handshake failures over its bounded TLS/TCP
path, and the controller remains opt-in.

The isolated listener and local test processes were intentionally left out of
the live deployment; they must be stopped before a clean handoff.

## Authenticated pooled fast-stream campaign (1d7a556)

The pooled fast-stream implementation was tested after the controller campaign
without changing `queqiaod-dev.service` or its `:12443` listener. The isolated
servers used the `70d431a` Linux/amd64 binary (SHA-256
`5cfaa72031cecc018898aa9de0aee511448630bd79b89c5c9819ac55c929ece3`) on
`:12446` with `adaptive` and `:12447` with `bbr-tuic`. The client used a
physical source binding (`--local-address auto`), SNI `<EGRESS-SNI>`,
and the fixed public address. All HTTP results below preserve the exact-body
and curl-exit completion rule; a 200 response with a timeout is incomplete.

### Short requests

Ten fresh Google `generate_204` requests were issued serially through one
adaptive QUIC client. The pooled profile used one persistent QUIC connection;
the first request performed `HELLO` plus `OPEN`, and later requests used
`OPEN_FAST`. The independent profile created a fresh QUIC connection for every
flow. The path was variable, so these are an amortization campaign rather than
a randomized causal estimate.

| Profile | Success | Times (s) | Median / max (s) |
|---|---:|---|---:|
| Adaptive, pooled fast stream | 10/10 | 8.376, 3.497, 1.844, 2.574, 2.807, 1.754, 2.586, 2.804, 2.434, 2.169 | 2.580 / 8.376 |
| Adaptive, independent QUIC | 10/10 | 3.563, 6.092, 4.388, 8.623, 4.095, 3.687, 6.735, 9.208, 4.498, 3.229 | 4.443 / 9.208 |

The cold pooled request was 8.376 s; the nine warmed requests had a median of
2.574 s. The pooled median was 42% lower than the contemporaneous independent
median, but the sample is too small and the path too variable for a release
claim. A new client using the corrected receiver was also tested against the
unchanged live service, which still emits the interim 32-byte capability
acknowledgement: five Google requests succeeded (times 5.266, 2.172, 3.455,
2.790, 4.325 s; median 3.455 s). This was a compatibility check, not a
comparison with the isolated server.

### Bulk and upload stress window

The path entered a severe loss/throttle window during the same session. This
is useful failure evidence, not a capacity estimate:

| Profile | Workload | Result |
|---|---|---|
| Adaptive, independent, one flow | 30-s 10 MiB download | 834,892 bytes, HTTP 200, curl 28 (incomplete; 0.223 Mb/s) |
| Adaptive, pooled, one flow | 30-s 10 MiB download | 917,504 bytes, HTTP 200, curl 28 (incomplete; 0.245 Mb/s) |
| `bbr-tuic`, independent, one flow | 30-s 10 MiB download | 10,125,222 bytes, HTTP 200, curl 28 (incomplete; 2.699 Mb/s) |
| `bbr-tuic`, independent, one flow | 60-s 10 MiB download | 10,485,760 bytes, HTTP 200, curl 0 in 34.380 s (2.440 Mb/s) |
| Adaptive, pooled, one upload | 10 MiB US sink | 10,485,760 bytes, HTTP 200, curl 0 in 25.409 s (3.301 Mb/s) |
| `bbr-tuic`, independent, one upload | 10 MiB US sink | 10,485,760 bytes, HTTP 200, curl 0 in 60.084 s (1.396 Mb/s) |
| Adaptive, pooled, two concurrent downloads | 60-s 10 MiB each | 0/2 complete; 1,097,018 and 1,064,942 bytes, curl 28 (0.146/0.142 Mb/s per flow) |

The result reinforces the existing release decision: pooled fast streams target
handshake latency and do not increase a hard path capacity limit. Bulk mode
still requires a path-health gate and must not select `adaptive`, `bbr-tuic`, or
pooling globally based on this stress window. The temporary listeners, upload
sink, and isolated processes were stopped; the live service remained active
and unchanged.

## Flow-stage latency profile after ACK-batch correction

Commit `49406de` corrected the TUIC-aligned estimator so a coalesced QUIC ACK
event contributes its complete acknowledged byte count instead of only the
first packet in the batch. Commit `1565f74` then added debug-only stage timing.
The instrumented candidate was run on isolated UDP `:12448`; the live `:12443`
service remained unchanged. The existing TUIC client on `127.0.0.1:12086` and
server inbound `:2444` were the contemporaneous control.

One fresh QUEQIAO Google `generate_204` request completed in 5.256 s. Its
measured critical path was:

| Stage | Duration |
|---|---:|
| Outer QUIC connection establishment | 2.869 s |
| QUEQIAO Hello authentication round trip | 0.905 s |
| Remaining flow-open path, including `OPEN` / `OPEN_OK` | 0.382 s |
| US-side DNS plus destination TCP connect | 0.0057 s |
| Curl destination TLS complete | 4.445 s from request start |
| First response byte / total | 5.256 s / 5.256 s |

The server-side profile independently measured 0.996 s waiting for the
post-Hello `OPEN`, only 5.7 ms dialing the destination, and 1.402 s running the
proxied application flow. This rules out US-side DNS/TCP establishment as the
dominant latency. The impaired China-to-US outer handshake plus QUEQIAO's two
serial control exchanges dominate a cold request.

A persistent authenticated QUIC pool removed most of that cost. Three serial
QUEQIAO requests completed in 4.858, 2.694, and 0.818 s. The first still paid a
2.444-s outer handshake; subsequent `OPEN_FAST` streams took about 0.264--0.265
s to open. The same-window TUIC control completed three requests in 0.590,
0.555, and 0.532 s. TUIC therefore retained both a lower median and lower
variance, but the warm QUEQIAO floor is no longer multi-second: the remaining
fixed protocol cost is approximately one China-US `OPEN_FAST` round trip plus
the end-to-end destination TLS/request exchange.

The bulk profile exposed two additional release blockers. One pooled QUEQIAO
10-MiB download completed in 51.475 s (1.63 Mb/s application goodput). During
the transfer, the remote controller entered recovery while its pacing-rate
telemetry ranged from roughly 2.6 to 15.4 MB/s; QUIC connection bytes sent
reached about 20.5 MiB while the application delivered 10 MiB. The public loss
counters incorrectly remained zero because apNet quic-go updates those
counters in its built-in CUBIC controller, while an externally installed
controller receives loss callbacks without access to the connection counter.
Zero exported loss is therefore not evidence of a loss-free path and the
telemetry must be repaired before controller decisions rely on it. A
same-window TUIC trial received 9,272,598 of 10,485,760 bytes before its 60-s
timeout, confirming that the path itself was also in a severe degradation
window; this single pair is diagnostic, not a throughput ranking.

Finally, after the QUEQIAO response body completed, a normal last-lane EOF was
not recognized as completed on the client. The recovery manager opened six
sequential zero-payload replacement lanes and eventually reported the bounded
45-s replacement timeout. Curl had already received the exact body, so this
did not inflate its 51.475-s measurement, but it retains resources and adds a
false flow failure. Completion-vs-rescue coordination is consequently a
production blocker alongside controller telemetry. All isolated profiling
listeners and clients were stopped after this campaign.

## Latency root-cause profile and initial-flow pipeline candidate

The next profile used a fresh worktree binary on isolated UDP `:12448` with
`bbr-tuic`, a fixed remote HTTP oracle on the US host (`:28081`, HTTP 200 with
the directory listing), and the existing TUIC control on local
`127.0.0.1:12086`. The live `:12443` service was not changed. The oracle was
chosen to remove Google certificate, HTTP/2, and application-server variance;
the server's own destination dial was typically 0.3--5 ms.

The packet capture explains the long tails. In a representative degraded
QUIC connection, the first Initial packet reached the US host at
`1786375905.017565`; the server's first response burst left at
`1786375905.022607`. Subsequent server retransmission bursts were separated by
approximately 2.25 s, 3.1 s, and 3.6 s. The client eventually completed, but
the loss/PTO schedule—not CPU, HMAC verification, DNS, or the destination
dial—accounted for the seconds of delay. A separate debug run measured an
outer QUIC dial of 7.85 s, with 0.28 s for the QUEQIAO Hello exchange and 0.26 s
for the remaining flow-open path; the server spent 0.12 ms authenticating and
only 0.4 ms dialing the oracle.

The original cold-flow protocol also serialized two application exchanges:

```text
QUIC/TLS handshake -> HELLO / HELLO_OK -> OPEN / OPEN_OK -> application
```

The new candidate client writes `HELLO` and `OPEN` back-to-back on a dedicated
lane. It is wire-compatible with the existing server: the server still sends
`HELLO_OK` before `OPEN_OK`, but `OPEN` is already buffered when the handler
finishes authentication. On the old sequential path, server `open_duration`
was commonly 0.2--0.35 s and reached 1.99 s when that request was lost. With
the pipelined client and the unchanged profiling server, server
`open_duration` was 3--26 microseconds in the successful samples. The end to
end flow time remains dominated by the outer QUIC handshake when that handshake
hits a PTO, so this is a fixed-cost reduction, not a cure for a blocked UDP
path.

For context, representative five-request samples collected during the same
campaign were:

| Profile | Resulting total times (s) |
|---|---|
| Existing TUIC control | 0.492, 0.421, 0.382, 0.378, 0.346 |
| QUEQIAO pooled `OPEN_FAST` | 0.563, 7.643, 2.008, 0.825, 2.009 |
| QUEQIAO fresh QUIC (legacy sequential control) | 4.906 in the captured sample; other fresh trials ranged from ~1.9 to ~9.7 |
| QUEQIAO fresh QUIC (pipelined HELLO+OPEN candidate) | 8.658, 1.756, 3.272, 3.212, plus one 10-s proxy-close failure |

These rows are intentionally diagnostic rather than a throughput or latency
release claim: the China path changed during the interleaved trials. They do
show the causal split. TUIC's persistent connection avoids repeated QUIC
handshakes; QUEQIAO's pooled mode removes repeated authentication but still
depends on the health of one QUIC connection; and the pipeline removes one
QUEQIAO request/response RTT without changing the UDP loss behavior.

The candidate passed the full Go, race, vet, and protocol integration suites.
It must still be tested with the automatic TCP race and pooled first-stream
pipeline before deployment. The temporary oracle, listeners, packet captures,
and profiling clients remain test-only resources and must be stopped after
the campaign.
