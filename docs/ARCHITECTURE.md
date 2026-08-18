# Queqiao architecture

## Components

```text
provider admin CLI ── atomic authorization state ──┐
                                                  v
application → SOCKS5 client → TLS 1.3/QUIC or TCP → gateway → destination
                    │                              │
                    └─ private device profile      └─ provider identity state
```

The provider creates users and one-time invitations. Enrollment generates a
device key on the client and returns one self-contained profile. The gateway
uses the same public TCP/UDP endpoint for enrollment, renewal, and data, with
isolated ALPNs and TLS policies.

## Trust boundaries

The pinned provider root identifies a trust domain, not a DNS name. Constrained
gateway and device issuers separate server and client roles. Normal TLS
handshakes authenticate both sides and map the client leaf to an immutable
provider/account/device principal. Mutable policy remains in the authorization
store and is checked at handshake, at every stream open, and periodically for
active flows.

The invitation is a short-lived bearer credential only for enrollment. Session,
flow, lane, and UDP-resume IDs route already-authenticated state; none grants
authority. A secondary lane must have the exact principal of the original flow.

## TCP stream data path

1. The client assigns random session/flow IDs and sends `OPEN` with a bounded
   canonical destination.
2. The gateway applies destination policy and dials it, returning `OPEN_OK` or
   a coarse `RESET`.
3. Each direction is split into bounded `DATA` frames at logical byte offsets.
4. Cumulative and bounded range acknowledgements permit safe retransmission
   across lane failure without exposing duplicates to the application.
5. `CLOSE`, final ACKs, and metadata-only tombstones finish half-close and
   recover a lost final acknowledgement.

The application protocol remains end-to-end through the SOCKS proxy. Queqiao
can see the destination, byte sizes, and timing, but does not terminate an
application's own TLS.

## Pooling, isolation, and fallback

Short/control flows share a persistent QUIC connection, avoiding a new
connection handshake per flow. Behavioral classification (`NEW`, `INTERACTIVE`,
`BULK`) is a scheduling hint, not an authorization boundary. When useful, a
bulk flow moves to a separately authenticated connection so it does not fill
the pooled control congestion window.

`auto` prefers QUIC and prepares delayed TLS/TCP fallback. Only differential
evidence—QUIC failure while TCP reaches the same endpoint—penalizes UDP. A
cooldown avoids repeatedly delaying applications on a blocked path.

TCP fallback can use multiple independent lanes for a bulk flow. Logical byte
offsets and range acknowledgements preserve order. Lane counts, replay state,
and recovery attempts are bounded.

## UDP

SOCKS5 UDP ASSOCIATE preserves packet boundaries. QUIC datagrams are preferred;
TLS/TCP uses the control stream. A bounded replay window rejects duplicate or
stale packets.

After lane failure, the gateway briefly retains the remote UDP socket under a
random single-use token. A replacement with both that token and the same
authenticated principal can reclaim it, preserving the source address seen by
the destination. In-flight datagrams may still be lost, as UDP permits.

## Uplink measurement

An uplink change invalidates pooled congestion state. The client can establish
a fresh authenticated QUIC connection and send bounded `PROBE` padding. The
gateway discards at most 128 KiB without opening a destination or reflecting
data; QUIC acknowledgements update the loss model used by the first real flow.

## Bounds and persistence

Global and per-user sessions, connections, frames, reassembly, acknowledgement
ranges, lanes, retained relays, enrollment messages, handshakes, idle periods,
and flow lifetimes are bounded. Provider JSON state is written by temporary
file, fsync, atomic rename, and directory sync. The server reloads complete
snapshots and retains the last known-good state if a replacement is malformed.
