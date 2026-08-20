# What the China-US path actually is (2026-08-13)

The measurement is completed on a hotel Wi-Fi in Dalian, which is a deliberately
difficult case with 42% packet loss rate.
I also performed measurements in my home (China Telecom residential Internet)
and the loss rate was about 15%.

## The measurement

`pathprobe` is open-loop: the client asks the server for a rate, the server
paces UDP at that rate and afterwards reports how many packets it put on the
wire, and the client counts what arrived. Nothing is retransmitted and nothing
is windowed, so the result is the channel and not a controller's opinion of it.

Downstream (US to China), one connection, 1200-byte payloads:

| offered Mbit/s | delivered | loss | P(loss \| prev arrived) | P(arrived \| prev lost) | mean burst | longest |
|---|---|---|---|---|---|---|
| 1 | 0.55 | 45.0% | 0.454 | 0.556 | 1.80 | 7 |
| 4 | 2.30 | 42.5% | 0.424 | 0.572 | 1.75 | 9 |
| 12 | 6.79 | 43.4% | 0.375 | 0.489 | 2.05 | 15 |
| 50 | 13.95 | 72.1% | 0.455 | 0.176 | 5.68 | 63 |

A memoryless (Bernoulli) erasure channel with loss `p` has
`P(loss | prev arrived) = p` and `P(arrived | prev lost) = 1 - p`. At 1 Mbit/s
the measured pair is 0.454 and 0.556 against a loss rate of 0.450: equal to
within sampling noise. At 4 Mbit/s, 0.424 and 0.572 against 0.425. The loss
below the knee is independent, and it is independent at a rate low enough that
no queue anywhere is involved.

ICMP to the same host on the same interface loses 37.3% of 150 pings sent at 5
per second, so this is not UDP policing either.

Above about 14.5 Mbit/s delivered a second and different process appears. At 50
Mbit/s offered, `P(loss | prev arrived)` is 0.455 while the overall loss is
0.721 -- losses now cluster, mean burst 5.68, runs up to 63 packets. That is a
queue overflowing.

## Two regimes, needing opposite responses

1. **A rate-independent i.i.d. erasure channel of about 42%.** It is present at
   1 Mbit/s and at 5 packets per second. It is a property of the path.
2. **Congestive burst loss above the roughly 14.5 Mbit/s delivered knee**, which
   is correlated and is the only real congestion signal on this path.

## What this explains

**Why nothing could saturate the link.** A loss-based controller reads regime 1
as catastrophic congestion. It is not congestion; there is nothing to back off
from, and backing off does not reduce it.

**Why multiple connections appeared to help.** TCP's Mathis limit,
`MSS / (RTT * sqrt(p))`, at `p = 0.42` and `RTT = 300 ms` gives about 90
kbit/s per connection. Measured with `curl` bound to the LAN address: one
connection 0.03 Mbit/s, two 0.10, four 0.22, eight 0.52 -- linear in the
connection count, which is what a loss-limited transport does. The gain was
real, but its mechanism is loss, not per-flow rate limiting by an ISP. The
open-loop probe shows the aggregate delivered rate is invariant to connection
count (1, 2, 4 and 8 connections at 30 Mbit/s each all deliver 14.3 to 14.7
Mbit/s in total, and the same total offered rate split 1, 2 or 4 ways delivers
the same total), so there is no per-4-tuple policer to exploit here.

**What the ceiling costs.** On a memoryless erasure channel the capacity is
`(1 - p)` times the line rate, so no scheme beats 0.58 times what is offered,
and the delivered ceiling of about 14.5 Mbit/s is the budget any design has to
work inside. Retransmission spends that budget in round trips instead of in
bandwidth: the expected transmissions per packet is `1 / (1 - p)` = 1.75, and
18% of packets need three or more tries, which at this RTT is a tail beyond 600
ms.
