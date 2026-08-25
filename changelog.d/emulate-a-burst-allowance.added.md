The path emulator can model a token bucket with a burst allowance, through
Config.PolicerBurstBytes. A shaped path has two rates rather than one: a
short probe drains the bucket and measures the line, while sustained load
measures the shaping rate, and a path emulated with only the second is
indistinguishable from a slower one. The existing shallow bucket, one refill
quantum plus a packet, still models the live path and remains the default.
