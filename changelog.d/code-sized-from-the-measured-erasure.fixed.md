Forward error correction is now sized for the erasure the path is measured to
be applying, and no longer for the congestion controller's floor. The floor is
biased low so that pacing errs towards slowing down, and the coded layer built
its whole view of the channel out of that one number, so on a path erasing
19.9% it sized parity for 1.76% and handed 11% of the payload back to the
session to re-issue a round trip later. Of the flows measured during the
incident that saw more than 5% erasure, every one ran a code sized for less
than half of it and 97% ran no code at all. The shared path model now carries
the measured erasure and its burstiness alongside the floor, and each consumer
reads the one whose error it can survive.

The code rate is also chosen differently. There is no residual target any
more -- a sender never observes the residual it chose, so it was an
unverifiable setpoint -- and no minimum coded loss below which parity was
refused. Both are replaced by choosing the rate that delivers a block soonest,
counting what an unrepairable block costs in round trips before its data
arrives, so a clean path buys no parity and a lossy one buys as much as it is
worth without either case needing a constant. A channel too lossy for any
allowed rate to repair a block more often than not is now reported as such
rather than being given a code that cannot work.
