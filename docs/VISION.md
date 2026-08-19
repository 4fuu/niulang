# Vision and design principles

> [!NOTE]
> **Status:** Current project direction
>
> **Applies to:** Protocol 1 and future explicitly versioned successors
> **Last reviewed:** 2026-08-19

Queqiao's goal is to be an evolving WAN optimization protocol for difficult,
known long-haul links. It should be useful now, measurable in operation, and
able to change when evidence from a new network contradicts the current
design.

It is not an attempt to invent one universal congestion controller or to claim
that conventional transports are wrong on the paths they were designed for.

## The deployment insight

General-purpose congestion control has to work when connections go to unrelated
destinations through unrelated bottlenecks. A controller therefore usually
learns and acts per connection.

Queqiao is deployed between two known tunnel endpoints; the current product
roles call them a client and provider gateway. Web, SSH, voice/video, and
transfer flows may continue to many destinations after that gateway, but they
first cross the same endpoint-pair WAN segment. When that segment is the
dominant bottleneck, the system can share a path model and an aggregate policy
across flows.

This pattern is widely useful: an intercontinental proxy or branch tunnel, a
remote employee reaching a corporate gateway, a device on a poor hotel/mobile/
residential link reaching a stable relay, or one long-haul leg of a
Tailscale-like overlay. Queqiao supplies the optimized paired data plane; a
larger overlay may supply discovery, routing, policy, and mesh coordination.

This is an explicit operating assumption, not a universal truth. If the
dominant bottleneck is elsewhere, changes rapidly, or is shared with traffic
outside the operator's control, the policy and its safety limits need to be
reevaluated.

## Network design principles

### Treat the endpoint pair as one congestion domain

The application destinations differ, but traffic first crosses the same
client–gateway segment. Loss state, delivered rate, pacing, and latency reserve
therefore belong to the endpoint-pair aggregate. Per-flow state still describes
flow progress; it does not pretend each flow has an independent bottleneck.

### Classify the loss process, not each lost packet

Rate-independent random erasure below a capacity knee is not relieved by
backing off. Loss that appears or becomes clustered as offered load crosses the
knee is congestion. The controller uses delivery rate and loss correlation to
separate these regimes rather than giving every loss the same meaning.

### Control aggregate offered load

On a high-erasure path, wire rate and application-useful delivery rate differ
substantially. The sender must budget parity and retransmission against the
shared physical bottleneck, not let each logical flow independently chase a
rate. Queqiao has no TCP-friendliness obligation on an operator-controlled
paired segment, but it still paces the aggregate at real congestion.

### Choose recovery against bandwidth–delay product

With a long RTT, a missing packet recovered by ARQ can delay useful bytes by a
full additional round trip. FEC can remove that wait but consumes capacity even
when its parity is not needed. Queqiao uses sliding-window coding while avoiding
an RTT is more valuable, then favors retransmission as flow progress makes byte
efficiency dominant. This changes policy inside one logical flow; it does not
select another transport architecture.

### Keep feedback independent of blocked data

An ordered stream gap must not hold the ACK or recovery signal needed to release
the sender. Queqiao separates reliable control from coded data within a
connection. Under cross-flow contention, priority and reactive isolation keep a
bulk congestion window from trapping new interactive work or its control
traffic.

### Model each direction independently

The downstream and upstream may traverse different capacity, shaping, and loss
regimes. Queqiao keeps direction-specific estimates and recovery decisions; it
does not infer upstream behavior from a downstream erasure floor.

### Use one flow architecture across workloads

Short requests, interactive sessions, and bulk transfers are evaluation
families, not separate protocols. Every TCP flow starts with the same logical
framing and can evolve as its observed byte count, rate, directionality, age,
and idle gaps change. Classification only supplies cross-cutting policy signals.

## What may evolve

The current mechanisms—shared path estimation, erasure-aware control,
sliding-window coding, aggregate pacing, behavioral classification, reactive
isolation, and TCP fallback striping—are an implementation, not the project's
identity. They may be replaced when a better measured design preserves the
principles above.

Wire evolution is explicit. A wire-incompatible change increments the protocol
version, documents its migration story, and fails closed rather than silently
negotiating an unsafe legacy mode.

Parity, regressions, rejected designs, invalid measurements, and path-specific
counterexamples remain part of the public record. Claims should shrink when the
available evidence is narrow.

## The community's role

The maintainers cannot reproduce every carrier, residential ISP, hotel,
enterprise firewall, route, or long-fat network. Reports from those networks
are necessary to discover where the shared-bottleneck or loss-model assumptions
hold and where they fail.

The most valuable contributions are reproducible measurements and
counterexamples, including cases where Queqiao is worse than a conventional
proxy. See [Contributing network evidence](CONTRIBUTING-NETWORK-EVIDENCE.md).
