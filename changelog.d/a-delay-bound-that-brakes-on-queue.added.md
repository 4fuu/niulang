The sender is now held back when the path is carrying more than one
bandwidth-delay product of queue. The bound is that the round trip may not
exceed twice the path's own minimum, which is the same rule as the
controller's 2.0 congestion-window gain expressed in the time domain, and it
is a ratio rather than a duration on purpose: a duration would have to be
chosen, and the choice would be a latency policy smuggled into a congestion
controller.

It matters because a deeply buffered bottleneck absorbs an overload instead
of dropping it, so there is no loss to respond to and delay is the only
evidence. It also catches something the window gain cannot: on an erasure
path the window is divided by the arrival rate so that a full one arrives,
which means what is sent can be several times the bottleneck's worth, and
the queue is downstream of that division. The bound is therefore applied
after the compensation rather than before it. queqiao_delay_brake_ratio
publishes how much of the rate the bound is currently removing, so a path
held back by its own queue can be told from one that simply measured less.
