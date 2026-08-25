The erasure compensation no longer grows unless it is buying delivery.
Compensating is a bet that losses are independent of the sending rate, so
that sending more delivers proportionally more, and on a path that drops
because it is policed rather than because it erases, that bet is wrong in a
way that feeds itself: loss rises, the arrival rate falls, the compensation
asks to send more, and the policer drops more. An increase is now tested
against the delivered rate and refused if the previous one raised nothing.
It makes no judgement about the nature of the loss, only about whether
sending harder delivered more.

This bounds the loop but does not close it. On a policed path the bandwidth
estimate is a second and larger amplifier, because a token bucket passes a
burst at line rate and a max filter reports that burst as the path's
bandwidth. See the known limitations.
