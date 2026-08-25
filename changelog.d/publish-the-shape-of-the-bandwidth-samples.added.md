The delivery-rate samples behind the bandwidth estimate are now described in
the telemetry: their mean and their widest, and the interval and delivery
that produced the widest one. The estimate is a maximum over those samples,
and a maximum on its own cannot be read -- a rate is high either because the
path is fast or because the window it was measured over was short, and only
the interval and the delivery behind it tell those apart. A maximum far
above the mean is a tail rather than the path.

It is published because the question it answers could not be settled in a
harness. An estimator driven directly against a simulated policer reports
within one per cent of the path, while the same estimator in the full stack
reported twice it, and three explanations for that gap were measured and
ruled out before this existed. Deployed to a live gateway it answered the
question in one reading: the estimate stood at four times the widest sample
the connection had ever taken, which a maximum over samples cannot do, so
the fault was not in the sampling at all but in a second path that was
putting a number into the filter without it having been a sample. That is
the re-seeded peak fixed below.
