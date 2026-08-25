The path emulator can change what it erases while traffic is crossing it, in
each direction independently, through Relay.SetLossRate and
Bottleneck.SetLossRate. A path that only ever erases what it was constructed
with cannot express the case a transport most needs to survive: a channel
that moves under a live flow, which is what the motivating incident was. A
negative rate leaves a direction alone so one can be changed without knowing
the other, and a change clears any correlated-loss burst state rather than
carrying the old regime across it.
