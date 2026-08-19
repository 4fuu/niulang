# Project status

> [!IMPORTANT]
> **Release level:** Public-preview source tree; no production-ready claim
>
> **Wire protocol:** Version 1 only; incompatible versions fail closed
> **Last reviewed:** 2026-08-19

Queqiao is implemented and usable from source for its supported paired-gateway
topology. The current limitation is not the absence of a working system; it is
the breadth of independent protocol-1 field evidence and review needed before
making a production-ready claim.

## Applicability and current deployment shape

Queqiao's endpoint-pair data plane applies to intercontinental tunnels, remote
corporate access, poor access-network-to-relay links, and individual WAN legs
inside an overlay network. The common requirements are:

- a client with a known difficult WAN segment;
- one known, trusted gateway or relay beyond that segment;
- multiple application flows whose dominant bottleneck is the client–gateway
  path; and
- an operator who is authorized to use a non-TCP-friendly aggregate policy on
  that paired segment.

The current repository packages that data plane as a SOCKS5 client and provider
gateway, plus native mobile VPN adapters. It does not yet provide peer
discovery, global route exchange, or a complete multi-egress mesh control
plane. Those are integration layers, not limits on where the paired transport
can be used. Queqiao is not an anonymity network, CDN, or universal replacement
for the Internet's congestion control.

## Implemented in the current tree

| Area | Current capability |
| --- | --- |
| Data plane | SOCKS5 TCP CONNECT and UDP ASSOCIATE; pooled QUIC streams and datagrams; TLS/TCP fallback |
| WAN policy | shared endpoint-pair path model, erasure-aware control, aggregate pacing and interactive reserve |
| Recovery | byte-offset reassembly, range acknowledgements, bounded replay, sliding-window coding, lane replacement, UDP relay reclamation |
| Contention | behavioral flow state, priority queues, reactive bulk isolation, bounded opt-in TCP fallback striping |
| Identity | one-time invitations, provider-pinned gateway identity, per-device mutual TLS, renewal, revocation, per-user limits |
| Operations | bounded JSON logs, metrics, local visualizer, service examples, release packaging, SBOMs, and rollback procedure |
| Clients | command-line desktop client, an Android app exporting an authenticated local SOCKS5 endpoint for an existing routing client, and an iOS packet tunnel with a bounded bypass subset, all on the same protocol-1 core |
| Conformance | committed protocol-1 vectors for framing, acknowledgement, destination canonicalization, UDP carriage, sliding-window coding, and enrollment, replayed by the test suite |

## Evidence boundary

The open-loop characterization of the motivating path established a
rate-independent erasure floor and a separate congestion knee. Deterministic
tests and the benchmark harness exercise correctness, high loss, reordering,
contention, fallback, and resource bounds. Earlier live transport campaigns
also produced useful design evidence and negative results.

However:

- the published public-preview candidate and bounded field record from
  2026-08-17 used wire protocol 3;
- no complete, public protocol-1 candidate report is recorded yet;
- the required multi-network/multi-egress field matrix is incomplete;
- representative 24–72-hour protocol-1 field soaks are still open; and
- independent transport/security and mobile reviews are still open.

Therefore historical throughput figures are design evidence, not a promise of
current performance on another path. The most important prior live comparison
reached parity with its TUIC-shaped reference, not a demonstrated universal
advantage.

## Claim levels

| Claim | Status |
| --- | --- |
| Builds and runs from source | Supported by the current repository and CI design |
| Usable public preview | Intended current release level; final publication gates remain in the checklist |
| Works on every high-loss path | Not claimed |
| Faster than BBR- or QUIC-based proxies in general | Not claimed |
| Production-ready | Blocked on the explicit qualification and review gates |

The [public-release checklist](RELEASE-CHECKLIST.md) is the authority for these
claims. [Production design criteria](PRODUCTION-DESIGN.md) define the stronger
bar, and [field validation](FIELD-VALIDATION.md) defines the missing network
diversity.

## Compatibility policy

Protocol 1 is the only supported wire protocol. Protocol changes must increment
the version, update [the protocol specification](PROTOCOL.md), add fail-closed
tests, and describe an explicit migration path. Pre-1.0 does not mean silently
accepting incompatible or unauthenticated peers.

Protocol 1's limits are fixed by the specification, not by configuration.
Version 1 has no capability exchange, so two peers configured differently would
be mutually unintelligible in one direction with nothing on the wire to say so;
the payload limit, the repair-window and decoder bounds, and the path-probe
budget are therefore wire constants. `testdata/protocol1/vectors.json` records
those limits together with the encodings that carry them, including the repair
coefficients that are generated on both ends and never transmitted. The test
suite replays the file on every run, and regenerating it for anything already
published is a wire break requiring a new version rather than a routine update.

## Help qualify the design

Results from residential, mobile, hotel, campus, managed, asymmetric, highly
lossy, or unexpectedly clean long-haul networks are useful—especially when they
contradict the current assumptions. Report short-lived, interactive, and bulk
behavior against the same unified design and include a same-window baseline.

Follow [Contributing network evidence](CONTRIBUTING-NETWORK-EVIDENCE.md) so the
result is reproducible and safe to publish.
