A flow that loses its last lane now spends one replacement grace on the
outage, rather than one for each part of the flow that happens to be waiting
for the lane. Four call sites wait for a lane that is not there — the flow's
own run loop, the frame and control writers, and the acknowledgement loop —
and a lane that dies with writes in flight leaves more than one of them
waiting for the same absent replacement. Each started a fresh 45-second grace
of its own, so a flow that was never going to be rescued failed only after
some multiple of the grace that depended on which writers were blocked when
its lane died. A live gateway showed this as failures clustered at 76–106
seconds against a 45-second grace, for flows that had finished their work in
the first second and were then held open by the application. The grace is
unchanged; what changed is that it is now the whole of what a flow waits, and
a flow that really is given a replacement lane is owed a full grace again for
any later outage.
