# Vision and design principles

> [!NOTE]
> **Status:** Current project direction
>
> **Applies to:** Protocol 1 and future explicitly versioned successors
> **Last reviewed:** 2026-08-20

Queqiao exists to make a difficult, known long-haul link useful today. It is a
ready-to-use transport for a client and a trusted gateway, and it keeps
evolving as measurements from more networks teach us where the current design
works, where it fails, and what should replace it.

This is a focused promise, not a claim to replace the Internet's general
congestion control. Queqiao is most useful when many application flows first
cross the same client-to-gateway segment and that segment is the dominant
bottleneck.

## The deployment insight

General-purpose transports cannot assume that two connections share a path.
They therefore learn and act independently for each connection. Queqiao knows
more about its deployment: web, SSH, voice/video, and transfer flows may have
different final destinations, but they first cross the same endpoint pair.

That shared segment becomes the optimization unit. Queqiao can coordinate its
aggregate offered load, share loss/RTT/capacity evidence, and protect latency
headroom across flows instead of asking every connection to rediscover the same
bottleneck.

This shape appears in intercontinental proxies and branch tunnels, remote
corporate access, poor hotel/mobile/residential links to a stable relay, and
individual long-haul legs inside an overlay. The repository supplies the
paired data plane; discovery, global routing, and mesh coordination can be
provided by a larger product around it.

If the dominant bottleneck is beyond the gateway, differs by destination, or is
a public resource outside the operator's authority, the assumption does not
hold automatically. Measure the deployment again before relying on the policy.

## Principles that guide the implementation

### Share one path model across flows

Per-flow byte progress remains separate, but loss, delivery rate, RTT, pacing,
and latency reserve belong to the shared client-to-gateway path. A change from
Wi-Fi to cellular creates a new path model because it changes the bottleneck.

### Separate random loss from congestion

Rate-independent erasure below a capacity knee is not relieved by backing off.
Loss that appears or becomes clustered as offered rate crosses the knee is
congestion. Rather than give every missing packet the same meaning, the sender
declines to read loss as congestion at all and brakes on queueing delay
instead, while the erasure it measures is what sizes its code and compensates
its window. The regime, not the packet, is what separates the two.

### Control total traffic at the bottleneck

Data, parity, and retransmissions all consume the same physical bottleneck. The
sender budgets them together and reserves room for control and interactive
traffic. Aggressive recovery against non-congestive erasure does not excuse
overrunning the real congestion knee.

### Choose recovery for the path's RTT

Retransmission is byte-efficient but may add a full RTT before useful data can
continue. Sliding-window coding spends extra wire bytes to repair some gaps
without waiting. Queqiao can use coding while the RTT is more expensive than
the parity, then return to retransmission as byte efficiency becomes dominant.

### Keep acknowledgements and control traffic moving

Acknowledgements and recovery control must reach the sender even when coded
data is missing. Reliable control, priority scheduling, and reactive isolation
keep bulk traffic from trapping the feedback or new work that releases it.

### Measure upstream and downstream separately

The two directions can have different capacity, shaping, RTT contribution, and
loss behavior. Queqiao keeps direction-specific estimates and recovery policy
instead of copying a downstream model onto the upstream.

### Use one flow design for all workloads

Short requests, interactive sessions, and bulk transfers are evaluation views,
not application-selected protocols. Every flow keeps the same logical framing,
byte offsets, acknowledgement semantics, and recovery state while its policy
changes with observed bytes, rate, direction, age, and idle gaps.

## What can change

The current mechanisms—path estimation, erasure-aware control, coding,
aggregate pacing, behavioral classification, isolation, and carrier fallback—
are replaceable implementation choices. A new mechanism should preserve the
principles above, carry evidence from the target path, and make its limits
explicit.

Wire evolution is never implicit. A wire-incompatible change increments the
protocol version, updates the [protocol specification](PROTOCOL.md), documents
the migration path, and fails closed instead of silently accepting an unsafe
legacy mode.

## Build the project with us

The maintainers cannot reproduce every carrier, residential ISP, hotel,
enterprise firewall, route, or long-fat network. A reproducible counterexample
is as valuable as a performance win. See [contributing network evidence](CONTRIBUTING-NETWORK-EVIDENCE.md)
and the [general contribution guide](../CONTRIBUTING.md).
