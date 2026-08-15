# Transport for an erasure channel (2026-08-15)

The path this project targets is not congested. It erases. About 42% of packets
are dropped independently of the sending rate — at 1 Mbit/s as readily as at 12,
and ICMP loses 37% at five packets a second. The measurement and its method are
in `docs/PATH-CHARACTER-20260813.md`; this document is what follows from it.

Three layers changed, and only one of them is the erasure code. The largest
single gain is the congestion controller, and the code's contribution is
latency rather than throughput. Both of those were surprises, and both are
measured below.

## What the path does

Two regimes, separated by a knee at about 14.5 Mbit/s delivered:

- **Below it**, a memoryless erasure channel of about 42%. `P(loss | previous
  arrived)` equals the overall loss rate to within sampling noise, so the
  losses are independent. Nothing a sender does changes it.
- **Above it**, a token-bucket policer at about 25 Mbit/s refilled in quanta,
  whose losses cluster into runs. This is the only genuine congestion signal on
  the path.

`internal/pathsim` reproduces both, checked against the live figures at 1, 4,
12 and 50 Mbit/s offered, on delivered rate and on loss structure together.

## Telling the regimes apart

`internal/lossmodel` reads two statistics from the sequence numbers that
arrive.

`P(loss | previous arrived)` against the overall loss rate says whether the
loss is independent: a memoryless channel makes them equal, a queue does not.
The **burst factor** — mean loss-burst length divided by what independence
alone would produce — is 1 for an independent channel and larger when losses
cluster, and it is exactly the factor by which correlation shortens the
effective length of a coded block.

Which part of the loss is congestion is a min-filter, in the same spirit as
BBR's windowed minimum round trip: the channel's erasure rate is the lowest
loss seen recently, because the channel does not stop being lossy when the
queue drains, and everything above that floor is what the sender caused. This
is what makes the split self-correcting rather than a policy — congestion
lasting longer than the filter's window becomes the floor, and is then treated
as the channel it has effectively become.

## The congestion controller: the largest gain

Measured across the emulated path, carrying QUIC datagrams:

| controller | delivered |
|---|---|
| quic-go default (Reno/Cubic) | 0.09–0.13 Mbit/s |
| BBR | 0.39–0.95 |
| BBR-TUIC (this repo's port) | 1.10–5.56 |
| Brutal, told 25 Mbit/s | 14.03 |
| **erasure** | **10.31–11.50** |

Capacity is 25 × 0.58 = 14.5 Mbit/s. Every loss-responsive controller gives the
path away; Brutal reaches it only by ignoring loss and pacing at a rate a human
typed in, which is the one thing this transport may not require.

`internal/congestion.ErasureSender` makes two corrections.

**Channel loss is not forwarded as congestion.** Only the share of losses above
the measured floor reaches BBR. The fractional part is carried rather than
rounded away, so a congestive share below a half still registers — that is
exactly the mild persistent congestion a controller most needs to see.

**The pacing rate is divided by the arrival rate.** This is the subtler of the
two and it is why plain BBR *collapses* rather than merely under-shooting.
BBR's bandwidth estimate is the rate that is **delivered**, while its pacer
governs the rate that is **sent**. On a clean path those are the same number.
On an erasure channel they differ by the arrival rate, and pacing a
delivered-rate estimate makes the sending rate its own input: sending S
delivers S(1−p), which becomes the next estimate, which is paced as S(1−p),
which delivers S(1−p)². It walks down to nothing. Dividing by the arrival rate
restores the property BBR assumes, and the loop then converges on the
bottleneck instead — in startup S grows by the gain each round until delivery
stops growing, which happens exactly when S reaches the bottleneck.

On a lossless path the floor is zero, the correction is one, and the
suppression never triggers; measured there it delivers 16.23 Mbit/s against the
stock controller's 10.68.

Available as `--congestion erasure`.

## The erasure code: latency, not bandwidth

`internal/fec` is a systematic Cauchy-matrix Reed-Solomon code over GF(256):
any k of n shards reconstruct, and a receiver that loses nothing does no work.
`internal/fec.Choose` sizes it from the measured floor, the burst factor and a
target residual — at the live channel, (124,256) at 2.06× overhead with a
residual near a thousandth. `internal/coded` carries it over an unreliable
datagram carrier with retransmission for the residual.

The honest accounting is that **on a memoryless erasure channel, retransmission
uses less bandwidth than coding.** ARQ resends only what was actually lost,
1/(1−p) = 1.72× at 42%. A block code must provision for the binomial and pays
about 2.06× even when sized ideally. Measured end to end over QUIC on the
emulated path, a 1 MiB transfer went at 1.47 Mbit/s coded against 1.77 Mbit/s
on a reliable stream — the stream is slightly *ahead* on throughput, and that
is not a defect to be tuned away but arithmetic.

What retransmission cannot do is deliver on time. A lost packet costs a round
trip, a packet lost twice costs two, and at 42% loss 18% of packets need three
transmissions or more. Small messages at a steady rate, timed write to read
over the same path and the same controller:

| | median | p90 | p99 |
|---|---|---|---|
| **coded datagrams** | **153 ms** | 628 ms | 1.21 s |
| reliable stream | 1.372 s | 1.691 s | 1.96 s |

Nine times better at the median. The stream's figure is head-of-line blocking:
at 42% loss something is always missing, and everything behind it waits.

So the code earns its place on interactive and loss-sensitive traffic, and
costs about 15% of throughput on bulk. That is a per-class decision, not a
global one, which is why `fec.Class` exists.

## Live validation (2026-08-15)

The emulator is fitted to the path, not the path. Three controllers were run
against the live link, downloading the same 50 MiB file through the proxy from
the measurement server, bound to the LAN address so the host TUN route is
bypassed, interleaved and repeated because the path moves between trials. The
channel measured at the same time: ~35% memoryless loss below the knee (burst
factor 1.10), rising to 52% and clustered (1.42) at 30 Mbit/s offered, with
delivered capacity about 14.5 Mbit/s.

One lane, 20 s per run, Mbit/s:

| round | reno | bbr-tuic | erasure |
|---|---|---|---|
| 1 | 0.11 | 1.87 | 10.31 |
| 2 | 0.10 | 10.27 | 11.63 |
| 3 | 0.08 | 10.31 | 10.90 |

The erasure controller is the fastest in every round and, more usefully, the
only consistent one: BBR-TUIC collapsed to 1.87 in the first round and reached
10 in the others, which is the loss-responsive failure appearing and clearing
with the path. At about 11 Mbit/s against a 14.5 Mbit/s capacity it takes 75%
of what the channel can deliver, where Reno takes under 1%.

The emulator therefore had the ordering right and the margin wrong: it
predicted the erasure controller would beat BBR-TUIC nine-fold, and live the
gap is nearer 1.5x against BBR-TUIC's good rounds. What the emulator does not
reproduce is BBR-TUIC's variance.

### Striping costs more than it gains here

The same comparison at four lanes:

| round | bbr-tuic | erasure |
|---|---|---|
| 1 | 8.70 | 8.63 |
| 2 | 7.73 | 7.64 |

Four lanes are worse than one — about 8 Mbit/s against the single lane's 11 —
and the controller no longer matters, because both land in the same place.
This is consistent with what the open-loop probe found in
`docs/PATH-CHARACTER-20260813.md`: the bottleneck is per endpoint pair, not per
4-tuple, so lanes do not multiply the share. They do multiply the offered rate
into one policer, which pushes the aggregate past the knee where loss turns
correlated and wasteful, and each lane's controller applies the erasure
compensation independently so the overshoot compounds.

Two things follow. On this path the lane count should be one unless a measured
probe shows a per-4-tuple limiter, which is what the open-loop split test is
for. And the erasure compensation, if lanes are ever used together, has to be
shared across them rather than applied per lane.

## What is not done

The coded channel is a complete transport but is not yet wired into the PEP's
lanes; `internal/pep` still runs on reliable QUIC streams. The erasure
controller *is* wired in and is the change that matters most for throughput.

Interleaving is modelled and sized for (`Params.InterleaveDepth` divides the
burst factor) but the sender does not yet spread shards across blocks, so
depth is 1 in practice. On a path whose above-knee loss clusters, implementing
it would buy back most of the rate that clustering costs.

The erasure compensation is per lane and should be per endpoint pair, which is
what the four-lane result above shows. Until that is fixed, striping and this
controller should not be combined.

The coded channel has not been measured on the live link at all -- only the
controller has. Its latency advantage is emulator evidence so far.
