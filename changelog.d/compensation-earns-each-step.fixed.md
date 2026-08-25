The erasure compensation now starts at none and earns each increase. It is a
bet that losses are independent of the sending rate, so that sending more
delivers proportionally more, and on a path that drops because it is policed
rather than because it erases, the bet feeds itself: sending more is what
raises the loss that asks for more compensation. Each step is now a tenth of
the remaining distance, small enough that the next round's delivery can be
attributed to it, and is kept only if delivery actually improved.

The previous version of this bound did not work, for two reasons that were
mistakes in the bound rather than in the diagnosis. It accepted the first
proposal whole, and by the time erasure is first measured the sender has
already burst and been policed, so that proposal is for several times the
rate with no evidence yet to test it against. And it judged the bet against
the sender's pacing rate, which the compensation is itself an input to, so
every increase appeared to have worked.

Measured against an emulated policer the overdrive falls from 42 times the
path's capacity to 7.3, and the loss from 72.5% to 49.8%. A 42% erasure
channel is unaffected, which is what says the compensation has not simply
been switched off.
