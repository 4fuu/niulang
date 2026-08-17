# Live-path comparison against deployed proxies — 2026-08-16

This is a real-path campaign. Every number comes from a client in China and a
fixed egress in the United States, with no emulator anywhere in the path, and
every peer is the implementation people actually deploy rather than an in-tree
imitation of it: sing-box 1.13.18 at both ends serving TUIC v5, Hysteria2,
VLESS over TLS, and VLESS over WebSocket.

Two results matter and they point opposite ways. On this path queqiao is the
fastest transport measured, by a margin that holds in every round. It also
stops moving data entirely after a couple of gigabytes and needs its client
process restarted to recover, which it did in three reproduction attempts out
of three. The throughput table exists only because the client was restarted
before every round.

## The path is not the one this project is designed for

`docs/DESIGN.md` builds on a channel that erases about 45% of packets at any
offered rate and polices above a knee near 14.5 Mbit/s. **The path measured
here is a different animal**, and that has to be said before any number is
read: the erasure code and the loss-insensitive controller were never under the
conditions they exist for, so nothing below confirms or refutes the design's
central claim.

`cmd/pathprobe`, open loop, 1200-byte payloads, 6 s per rate:

| Offered Mbit/s | Delivered | Loss % | Burst factor |
| ---: | ---: | ---: | ---: |
| 2 | 1.96 | 2.1 | 1.02 |
| 10 | 9.85 | 1.4 | 1.04 |
| 20 | 19.66 | 1.7 | 1.04 |
| 50 | 49.18 | 1.6 | 1.08 |
| 80 | 77.76 | 2.8 | — |
| 120 | 115.74 | 3.5 | — |
| 200 | 193.27 | 3.3 | — |
| 300 | 276.35 | 7.9 | — |

There is no knee below 200 Mbit/s, the loss is 1–3%, and the burst factor stays
between 1.02 and 1.08, so the loss is memoryless in the sense
`DESIGN.md` means. ICMP agrees: 249 ms mean over 30 source-bound pings, 204 to
330 ms, 3.3% loss. The bandwidth-delay product is about 6 MB.

## Binding the outer socket is not optional here

The client host runs Clash in TUN mode and its route to the egress points at
`utun4`. The naive version of this experiment measures every proxy *through the
tunnel it is meant to replace*. That was true on the first attempt and it was
caught by asking the server what source address it saw:

```
unbound socket        → server observed 23.135.236.244   (the existing tunnel's exit)
bound to 192.168.3.66 → server observed 120.244.189.31   (the real China Mobile address)
```

Every client here is therefore source-bound — `--local-address` for queqiaod,
`inet4_bind_address` for the sing-box outbounds. A capture on the server's
physical NIC during a simultaneous transfer through all four sing-box stacks
recorded 516 inbound packets, every one from `120.244.189.31`.

This is the third time a variant of this mistake has cost a campaign; see the
list in [`BENCHMARKING.md`](BENCHMARKING.md).

## Throughput

Six rounds, one 20-second download window per stack per round, stack order
rotated each round, queqiao's client restarted before each round so the stall
below is measured separately rather than smeared through this table.

A fixed *time* window rather than a fixed object is the only fair shape when
the stacks differ by two orders of magnitude: 16 MiB is four round trips for
one of them and three minutes for another.

| Stack | Median Mbit/s | Mean | Min | Max | Trials | × queqiao |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| queqiao | **143.06** | 137.31 | 105.77 | 159.33 | 6/6 | 1.00 |
| Hysteria2 | 90.15 | 84.25 | 46.91 | 104.20 | 6/6 | 0.63 |
| TUIC v5 | 76.79 | 74.70 | 47.08 | 87.20 | 6/6 | 0.54 |
| VLESS + TLS | 0.63 | 0.94 | 0.22 | 2.66 | 6/6 | 0.004 |
| VLESS + WebSocket | 0.39 | 0.38 | 0.14 | 0.74 | 6/6 | 0.003 |

queqiao led in all six rounds, not on the median alone.

The VLESS numbers are not a misconfiguration; both stacks moved bytes in every
trial. They are TCP's Mathis limit, `MSS / (RTT * sqrt(p))`, which at 249 ms
and 2% loss lands near 1 Mbit/s. WebSocket framing costs a further 38% on top.
This is the clearest live evidence for the repository's own first axiom: on a
path like this the loss response is the whole game, and a transport that owns
its loss recovery is a hundred times faster than one that rents it from TCP.

## Short-request latency

A 1 KiB HTTP fetch, five per stack per round against a freshly restarted
client, so the first is genuinely cold. One round trip is about 210 ms.

| Stack | Cold p50 | Warm p50 | Warm p95 | Warm p50 under load |
| --- | ---: | ---: | ---: | ---: |
| queqiao | 472 | 242 | 281 | 252 |
| TUIC v5 | 216 | 239 | 308 | 248 |
| Hysteria2 | 241 | 242 | 296 | 241 |
| VLESS + TLS | 739 | 735 | 838 | 665 |
| VLESS + WebSocket | 867 | 935 | 1024 | 938 |

Warm, the three QUIC stacks are indistinguishable and all at the floor. The one
weak number is queqiao's **cold** request: 472 ms, about one extra round trip
over TUIC's 216, paid once per client process rather than per flow. It is still
the first thing a user meets.

## Interactive applications

Throughput is not what an interactive application spends. Three shapes, each
measured as a round trip against an echo endpoint on the egress host — a round
trip because the two hosts have no synchronized clock and a one-way figure
would be mostly offset.

- **SSH-like** — 64-byte request/response every 50 ms on a held-open TCP
  connection.
- **Voice-like** — 172-byte datagrams every 20 ms over SOCKS5 UDP ASSOCIATE,
  the packetization of an Opus/RTP call.
- **Video-like** — 1100-byte datagrams every 5 ms, about 1.8 Mbit/s.

Idle, three rounds pooled, milliseconds:

| Shape | Stack | p50 | p95 | p99 | Loss % |
| --- | --- | ---: | ---: | ---: | ---: |
| SSH | queqiao | 211 | 292 | 302 | 0.0 |
| SSH | TUIC v5 | 206 | 237 | **265** | 0.0 |
| SSH | Hysteria2 | 239 | 299 | 433 | 0.0 |
| SSH | VLESS + TLS | 238 | 303 | 476 | 0.0 |
| SSH | VLESS + WS | 229 | 320 | 640 | 0.0 |
| Voice | queqiao | 209 | 287 | 387 | **1.9** |
| Voice | TUIC v5 | 209 | 283 | **297** | 3.2 |
| Voice | Hysteria2 | 224 | 296 | 318 | 3.1 |
| Voice | VLESS + TLS | 250 | 503 | 612 | 2.4 |
| Voice | VLESS + WS | 260 | 595 | 856 | 2.0 |
| Video | queqiao | 232 | 364 | 639 | 2.4 |
| Video | TUIC v5 | 223 | 318 | **551** | 3.9 |
| Video | Hysteria2 | 236 | 327 | 566 | 3.7 |
| Video | VLESS + TLS | 517 | 3192 | 3533 | **32.4** |
| Video | VLESS + WS | 1906 | 4310 | 4615 | **50.0** |

**VLESS cannot carry real-time media.** At 1.8 Mbit/s of datagrams it loses a
third (TLS) to a half (WebSocket) of them with seconds of latency, because
SOCKS5 UDP over VLESS is tunnelled inside a TCP stream: one lost segment
head-of-line blocks every datagram queued behind it, and the stream's
retransmission fights a real-time deadline nobody told it about. The three QUIC
stacks carry the same stream at 223–236 ms and 2.4–3.9% loss. This is the same
argument `DESIGN.md` makes for putting SOCKS UDP on QUIC datagrams, measured
against the alternative people actually run.

### Under the stack's own bulk load

| Stack | SSH p95 | SSH p99 | Voice p99 | Voice loss % |
| --- | ---: | ---: | ---: | ---: |
| queqiao | 292 → 658 | 302 → **940** | 387 → 565 | 1.9 → **9.0** |
| TUIC v5 | 237 → 363 | 265 → 662 | 297 → 326 | 3.2 → 4.6 |
| Hysteria2 | 299 → 412 | 433 → 526 | 318 → 452 | 3.1 → 8.5 |
| VLESS + TLS | 303 → 301 | 476 → 307 | 612 → 688 | 2.4 → 1.7 |
| VLESS + WS | 320 → 300 | 640 → 311 | 856 → 802 | 2.0 → 2.2 |

The VLESS rows must not be read as a win. Their bulk transfer runs at about
1 Mbit/s, so it never loads the link and there is nothing for the interactive
traffic to queue behind; those two rows are not comparable with the QUIC stacks,
which were saturating 77 to 143 Mbit/s at the time.

Among the stacks that were genuinely loaded, **queqiao degrades most.** Its SSH
p99 triples and its voice loss rises fivefold, against TUIC's 265 → 662 ms and
Hysteria2's 433 → 526. Part of that is a harder self-imposed load — queqiao is
pushing substantially more bulk during the test — but this is the case flow
classification and bulk isolation exist to protect, and on this path they did
not protect it. The emulated result in `README.md` (208 ms interactive median
under a 50 MiB transfer) does not reproduce here.

## The client stalls permanently under sustained transfer

> **Superseded by [`STALL-20260817.md`](STALL-20260817.md).** This was two
> unrelated defects, and the byte count was not the trigger. One -- a seeded
> round trip becoming a permanent `min_rtt` -- is fixed, and it was also
> depressing every throughput number in this document by about 30%. The
> other is triggered by *aborted* transfers, which this campaign's
> fixed-duration windows performed on every single trial; transfers that run
> to completion do not stall.

Found by accident: the first throughput campaign recorded 176, 156, 175, 167,
136 Mbit/s, then 5.3, then zero for every remaining round, while still
accepting SOCKS connections.

A dedicated reproduction ran back-to-back 20-second windows until goodput
collapsed, restarting the client each attempt. It failed all three:

| Attempt | Full-rate windows | Data moved | Last good rate | Self-recovered in 60 s |
| ---: | ---: | ---: | ---: | --- |
| 1 | 7 | 2.35 GiB | 139.5 Mbit/s | no |
| 2 | 6 | 2.10 GiB | 123.2 Mbit/s | no |
| 3 | 7 | 2.61 GiB | 166.0 Mbit/s | no |

There is no decline. Throughput holds between 123 and 179 Mbit/s and drops to
single digits in one window and to exactly zero in the next.

Counters sampled while stalled, server untouched throughout:

```
queqiao_active_flows                          9      started 19, completed 3, failed 7
queqiao_quic_lanes                            9      one connection per stuck flow, none closing
queqiao_quic_controller_app_limited_samples   146862
queqiao_quic_controller_non_app_limited_...   144    99.9% of samples marked application-limited
queqiao_quic_controller_max_bandwidth    322540 B/s  estimate collapsed to 2.6 Mbit/s
queqiao_quic_bytes_received            25641048042   25.6 GB of QUIC for 2.45 GB delivered (10.5x)
queqiao_quic_controller_state_misses        5573624  server side, acks with no matching state
```

The controller is `erasure`, the shipping default. BBR only updates its
bandwidth filter from samples that are *not* application-limited, and with 144
usable samples out of 147,006 the filter is starved; the estimate had fallen to
under 2% of what the link was delivering minutes earlier. The 10.5x ratio of
QUIC bytes to application bytes says the connection was spending its capacity
on something other than new data.

Two facts localize it. Restarting the **client alone** restored full throughput
immediately with the server process untouched, so the broken state is
client-side. And `cmd/queqiaoref`, which shares this repository's QUIC stack and
controllers, failed the same way in the same campaign — earlier and harder,
after a single 306 MiB transfer — which points at the shared transport layer
rather than at queqiao's own framing.

This is the blocking result. It is not a tuning problem and it is not on the
path's side.

## Correctness

| Check | Result | Detail |
| --- | --- | --- |
| `go build ./...`, `go vet ./...` | pass | no diagnostics |
| `go install ./cmd/...` | pass | 4 binaries, Go 1.25.7 |
| cross-build linux/amd64 | pass | deployed to the egress host |
| `go test ./...` | pass | 21 packages |
| `go test -race ./...` | pass | 21 packages, no data races |
| `go test -count=6` on transport packages | **1 failure** | `TestUDPAssociationRescuesToTCP`, i/o timeout |
| fuzz `FuzzOnDatagramNeverPanics` | pass | 9.8M execs in 90 s |
| fuzz `FuzzWindowDecoderNeverPanics` | pass | 30.6M execs in 90 s |
| byte integrity, live path | pass | every completed transfer matched SHA-256 |

The UDP/TCP rescue flake is real and is roughly at its documented rate — one
failure in six repeats — but the other half of the note in
`DESIGN-MULTIPATH.md`, that it appears on *every* run under `-race`, did not
hold: the full suite passed clean under `-race`.

**The stall is fail-stop, not data-corrupting.** Every completed transfer on
every stack matched the origin's SHA-256, queqiao included, under load and
immediately after recovery. A stalled client returns zero bytes, not wrong
ones. Both VLESS stacks failed the integrity run for a different reason worth
recording: given 300 seconds for a 16 MiB object they delivered 9.70, 9.37 and
12.25 MB — correct bytes, out of time.

## What this does not establish

- One path, one evening, about three hours. The path drifted while the campaign
  ran (202 to 260 ms mean RTT between rounds), which rotation controls for
  within a campaign and not across them.
- Controllers were not held constant: each stack ran its own shipping default,
  queqiao on `erasure` and the others on BBR. That is the right comparison for
  a deployment decision and the wrong one for attributing a difference to
  transport design rather than to congestion control.
- Upload, connection migration, UDP blocking and TCP fallback, restart
  recovery, and multi-hour soak are unmeasured. The last is untestable in this
  build for the reason above.
- The SSH shape yields about four samples a second on this path, so its p99
  rests on roughly 150 samples per cell and is indicative rather than tight.
  Voice and video pool thousands of packets and their percentiles are solid.

## Reproducing

Egress host, all listeners authenticated, origin bound to loopback so it is
reachable only through a proxy under test:

```sh
queqiaod --mode server --listen 0.0.0.0:12540 --tls-cert cert.pem --tls-key key.pem \
         --secret-file secret --allow-private-destinations
sing-box run -c singbox-server.json     # tuic 12550, hysteria2 12551, vless 12552/12553
python3 origin.py 28080 object.bin      # threaded; a single-threaded oracle skews results
```

Client, source-bound so the host TUN route cannot capture the outer socket:

```sh
queqiaod --mode local --listen 127.0.0.1:12180 --remote <EGRESS-IP>:12540 \
         --server-name queqiao.test --root-ca cert.pem --secret-file secret \
         --local-address if:en0
# sing-box outbounds carry "inet4_bind_address" for the same reason

pathprobe --mode client --remote <EGRESS-IP>:12599 --local-address if:en0 \
          --sweep 2,5,10,15,20,30,50 --duration 6 --pattern
```

One more trap, new to this campaign and now recorded in `BENCHMARKING.md`:
the measurement shell had `NO_PROXY=*` exported, and `curl` honours it even
when a proxy is named explicitly with `--socks5-hostname`. The first
"successful" transfers never touched a proxy at all. Clear it with
`env -u NO_PROXY -u no_proxy curl --noproxy ''`.

Stop every temporary listener when the campaign ends, including the ones on the
egress host.
