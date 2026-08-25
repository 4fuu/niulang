### What the real path showed

The three ruled-out explanations below were all about the *sampler*. The
instrumentation, deployed to the live China-US gateway on a build behaviourally
identical to the one running there, showed the fault was not in the sampler at
all. It was in two paths that put a number into the filter without it ever
having been a sample:

| | estimate | widest sample ever taken | ratio |
| --- | ---: | ---: | ---: |
| Gateway | 2,260,356 | 1,649,254 | 1.37x |
| Client | 19,414,758 | 4,732,767 | **4.1x** |

A maximum over samples cannot exceed its widest sample. Something else was
supplying it.

**`restartFromIdle` re-seeds an all-time peak.** `peakBandwidth` only ever rose,
and the seed fires whenever the pipe empties -- which on a connection that is
application limited essentially always is constantly. Its own comment says the
bet is safe because "the filter's own window disproves it within ten rounds";
it cannot, because the seed is re-armed faster than any window retires it. The
peak now carries the time it was observed and is not put back once a
measurement of that age would have expired.

That is also why the two attempts on the sampler changed nothing. Bounding the
filter's memory fixed how long a *sample* survives, and this path was not
supplying samples.

**The pooled seed divides by the arrival rate.** A joining lane starts at
`(aggregate bottleneck / lanes) / arrival`, and dividing by the arrival rate
inflates it on exactly the lossy paths that can least afford it. Left alone for
now: one change at a time, and the measurement above does not separate its
contribution from the first.

### What it cost, and what it bought

On the emulated policer the overdrive falls from 7.3x to **2.4x** and the loss
from 49.8% to 36%. Cumulatively the case has gone 42x, 7.3x, 2.4x.

On the 42% erasure channel it costs about 15%: a median 1.23 s against 1.04 s
measured on the same machine in the same window. That cost is real and it is
the honest price of not pacing from data the filter has already decided not to
trust. Two things temper it. The test transfers 48 KB across a 300 ms path,
which is about four round trips and therefore almost entirely ramp -- a worst
case for any change to seeding. And the alternative bound was measured: the
filter's ten-second ceiling costs the same 15% and leaves the policed path at
9.5x rather than 2.4x, so the cheaper-looking option is not cheaper.

### What is still left

2.4x is not braked. The path remains unbraked in the sense that neither loss nor
delay reaches the controller on a policer; what has changed is that the
controller is no longer being handed a number nothing measured.

### Two attempts on the wrong component

**Bounding the filter's memory in wall time.** The filter kept a sample for ten
packet-timed rounds, which on an application-limited connection was sixty-six
minutes; expiring samples in time as well as in rounds fixed that, and it is
worth having. It does nothing for a policer. The bursts recur every refill
period, so there is always a recent high sample and the estimate never has to
fall back to anything. **Age was not the problem.**

**Averaging the sample within a round.** The filter is fed the largest
per-packet delivery rate in an acknowledgement batch, each measured over one
packet's own send-to-ack window; on a path that releases traffic in quanta
those windows land unevenly, so the distribution has an upper tail and a
maximum reports the tail. Feeding one rate per round should have removed the
tail. It measured no better -- 2.7x the shaped rate became 3.6x -- and was not
shipped.

**A synthetic estimator harness** reported 4,000 B/s on a known 250 KB/s
schedule. The harness's own acknowledgement pacing was wrong. That failure is
what made the next one careful enough to be right.

**It accepted the first proposal whole.** By the time erasure is first measured
the sender has already burst and been policed, so the first request is for
several times the rate, with no evidence yet to test it against. Compensation
now starts at none and takes a tenth of the remaining distance per round, so
each step is small enough that the next round's delivery can be attributed to
it.

**It measured its own output.** The bet was judged against
`TUICBBRSender.bandwidth`, which returns the *pacing rate* -- a value this
compensation is an input to. Every increase therefore appeared to have worked.
It now reads the delivered-rate estimate.

With both corrected, the overdrive on the emulated policer falls from **42x the
path to 7.3x** and the loss from 72.5% to 49.8%. The 42% erasure channel is
unaffected at a median 1.18 s against a 1.10 s baseline, which is what says the
bound has not simply been switched off.

### What is left

7.3x is not braked, it is less unbraked. The remaining terms are the startup
gain of 2.77, which is BBR probing and should end when delivery stops growing,
and a bandwidth estimate that reads 2.0x the path in the full stack against
1.01x when the estimator is driven in isolation. **That gap is the next thing to
measure rather than the next thing to guess**: the two differ by everything the
real stack adds, and which part matters is not established.

Four attempts have now been made on this. The first three reasoned about what
the code should be doing and were each refuted by measurement, the third by a
measurement that was itself broken. The fourth started by measuring and found
the fault somewhere none of the three had looked.
Until then, **a policed path is unbraked**, and this design should not be
deployed on one.

## Sequencing

An earlier draft opened with "aggregate enforcement point driven by the
controller, not a flag". That step does not exist: the enforcement point is the
congestion window capped at the pooled share, and it was already there. What it
needed was an estimate that could fall.

1. **Done.** Bandwidth filter expires on rounds or wall time, whichever first;
   an application-limited sample may replace an expired estimate.
2. **Done.** The shared path model carries the measured erasure and burst
   alongside the controller's floor, and `channel()` reads the measurement
   instead of fabricating a snapshot from one scalar.
3. **Done.** Parity sized by the rate that delivers a block soonest;
   `TargetResidual` and `minCodedLoss` deleted.
4. **Done.** The loss the path caused is exported, not only the share charged
   as congestion.
5. **Done.** Delay bound added: the round trip may not exceed twice the path's
   own minimum.
6. **Done.** Loss suppression deleted and the arrival-rate compensation moved
   onto the measurement, gated on a measured round trip so it cannot run ahead
   of the brake that bounds it.
7. `kindReport` closes the residual loop.

Steps 1-5 have no wire impact. Step 6 is additive and forward-compatible.

Splitting the delay bound from the removal of loss suppression is not cosmetic.
Deleting suppression hands every erasure to BBR as congestion, and on a 42%
erasure channel that is the collapse to 0.39 Mbit/s this controller exists to
avoid. Nothing may remove it until the delay bound is in place to brake
instead. Until then the two consumers read different numbers from the same
model, which is what the design argues for anyway: the controller keeps its
conservative floor, the code reads the measurement.

## Open questions

- **The delay budget is the one remaining constant.** It is a latency policy
  rather than a bandwidth number, so it does not violate the rule that
  bandwidth must be measured. Whether it is fixed, derived from `min_rtt`, or
  exposed is undecided.
- **Bucket detection.** Goodput that is high and then decays at constant `B` is
  a draining token bucket, which argues for lengthening the measurement window.
  Detecting it is a measurement; the threshold for acting on it is not yet
  specified.
- **Mixed-fleet behaviour during step 6.** Until both ends report, half the
  paths run open-loop. Acceptable, or gated behind a capability exchange?
