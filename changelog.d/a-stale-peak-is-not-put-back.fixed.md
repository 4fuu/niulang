A bandwidth estimate is no longer restored from a peak the filter would
already have expired. When the pipe empties, the sender re-seeds itself from
the best rate it has ever measured, so that a reused connection does not
repeat a discovery that is expensive on a lossy path. That peak only ever
rose, and the re-seed fires every time the pipe empties -- which on a
connection that is application limited essentially always is constantly --
so the estimate was re-armed faster than the filter could retire it.
Measured on a live gateway, the estimate stood at four times the widest
delivery sample the connection had ever taken, which a maximum over samples
cannot do. The peak now carries the time it was observed and is not put back
once a measurement of that age would have expired.

On an emulated policer this takes the overdrive from 7.3 times the path's
capacity to 2.4, and the loss from 49.8% to 36%. It costs about 15% on a 42%
erasure channel, which is the price of not pacing from data the filter has
already decided not to trust; the alternative bound was measured and costs
the same 15% while leaving the policer at 9.5 times.
