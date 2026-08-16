# wanopt wire protocol (draft 1)

This is a design document, not a compatibility guarantee. Wire versions must
be negotiated explicitly before a release.

## Session transport

A session is established over an authenticated TLS/QUIC or TLS/TCP transport.
The client sends a random 128-bit `session_id` and a protocol version. The
server authenticates the configured credential before accepting any `OPEN` or
data frame. A reconnect may present a short-lived resume token bound to the
session and client identity.

## Frame envelope

Every frame has a fixed, bounded header followed by payload:

```text
magic(2) version(1) type(1) flags(2)
session_id(16) flow_id(8) sequence(8) payload_len(4)
class(1) reserved(3) payload(payload_len)
```

The first implementation must reject `payload_len` above the configured
maximum before allocating memory. Sequence numbers count bytes within a flow,
not frames. A receiver acknowledges contiguous bytes plus selective ranges.

Frame types are `HELLO`, `HELLO_OK`, `OPEN`, `OPEN_OK`, `DATA`, `ACK`,
`CLOSE`, `RESET`, `PACKET`, `OPEN_FAST`, and `OPEN_JOIN_FAST`.

Draft 1 removed three: `WINDOW`, `PING` and `PONG`. All three were specified
and none was ever sent. A frame type that exists only in this document is worse
than no frame type, because it reads as a property the implementation has. What
each was for, and what actually provides it, is in "Backpressure" and
"Liveness" below. The version byte is 2, so a draft-0 peer fails closed rather
than misreading a renumbered type.

### Pooled QUIC authentication

The first stream on a pooled QUIC connection performs the normal PSK/HMAC
`HELLO` exchange and opens its flow with `OPEN`. A capable server advertises
`CapabilityFastStreams` inside the otherwise opaque 16-byte `HELLO_OK` nonce:
the first eight bytes are `WOCAP001` and the remaining eight bytes are the
big-endian capability bitmap. The payload remains the original 24 bytes, so
old clients accept a new server and new clients treat an old server's random
nonce as a zero capability set.

After that acknowledgement, a new stream on the same TLS/QUIC connection may
start with `OPEN_FAST`. It still carries a fresh random `session_id`, non-zero
`flow_id`, destination or UDP-association marker, and the normal class. The
server performs all destination decoding, policy, admission, and resource
checks exactly as for `OPEN`; only the repeated `HELLO` round trip is removed.
`OPEN_FAST` is rejected on an unauthenticated connection, on TLS/TCP, and on
independent join/recovery lanes. A reconnected QUIC pool must authenticate and
negotiate capabilities again. If the capability is absent, every stream keeps
the legacy `HELLO` plus `OPEN` sequence.

`PACKET` is used only by a SOCKS5 UDP-associate session. The `OPEN` payload
for that session is the versioned `WOUD1` association marker rather than a
TCP `host:port`. Each packet payload is bounded and contains:

```text
destination_length(2) canonical_destination(destination_length)
udp_payload(remaining bytes)
```

The frame sequence is a packet sequence (starting at zero) and must increase
by one in each direction. The server resolves and validates the destination
at the fixed US egress using the same public/private-address policy as TCP;
the local client never performs the destination DNS lookup. SOCKS5 UDP
fragmentation is rejected, malformed datagrams are dropped locally, and an
association is bounded by the configured idle timeout and maximum lifetime.
The current implementation carries packets over reliable QUIC streams or
TLS/TCP, preserving packet boundaries while allowing automatic TCP rescue for
new and failed in-session associations. An in-session rescue for UDP opens a replacement
authenticated association, retaining the local SOCKS UDP socket. Where the
server advertised `CapabilityUDPResume`, the association's open carries the
16-byte token its OPEN_OK granted, and the server hands back the same remote
relay socket rather than binding a new one, so the destination keeps seeing one
source address. The token is single-use and reissued on every open, expires in
30 seconds, and buys nothing else: datagrams in flight when the lane died are
not replayed. TCP flows do resume: a replacement lane attaches to the
existing session and the flow continues on it.
`TypePacket` frames are carried on the connection's QUIC datagrams where
DATAGRAM was negotiated in both directions, and on the lane's control stream
otherwise. There is no capability of its own for this: both endpoints read the
same QUIC connection state, so a sender never routes a packet to a substrate
its peer is not draining, and a TLS/TCP lane is unchanged. Because a datagram
is neither retransmitted nor ordered, a receiver no longer requires the next
sequence number: it admits each once through a bounded anti-replay window and
drops duplicates and packets too far behind to place. A gap is loss, which is
what the application asked for by choosing UDP.

If the server also advertises `CapabilityReserveControl`, a pooled client may
set `FlagReserveControl` on its `OPEN` or `OPEN_FAST` frame. This marks lane 0
as the authenticated control/rescue lane for that logical flow. After a
joined lane is established, bulk `DATA` frames prefer joined lanes with
independent QUIC congestion state; ACK, FIN, and interactive/control frames
continue to use lane 0. If no joined lane is healthy, bulk traffic falls back
to lane 0, so the capability is an isolation preference rather than a
correctness dependency. The flag is never sent to a peer that did not
negotiate the capability.

`CLOSE` with `FlagFin` is a directional half-close. A sender may later send
the same final sequence with `FlagCloseAbort` when its application socket has
fully closed and the peer should release an otherwise idle keep-alive
destination. The receiver acknowledges the abort sequence before closing its
inner connection. This escalation is deliberately delayed after the normal
final ACK so legitimate half-closed uploads and interactive sessions can
continue to receive response bytes.

## Backpressure

There is no application-level window frame, and there is no place for one.
Three bounds already apply, each at the layer that owns the memory:

- QUIC's own stream and connection flow control, which is what stops a lane's
  peer being made to buffer.
- The sender's write-ahead bound per lane: a lane may hold only what its
  transport has not yet taken, so a stream write that blocks stops the producer.
- The sender's retention bound per flow, which is what it may hold unacknowledged
  in case a lane dies and its chunks must be re-issued elsewhere.

The receiver's reassembly bound is sized from the last of these, so a peer
running this code cannot overflow it and a hostile peer is bounded by the same
per-flow figure.

## Liveness

There is no application-level ping. QUIC's keepalive refreshes an idle
connection and its idle timeout declares a dead one; a second mechanism above
that would only be a slower copy with no independent evidence.

## Lane identity

Each lane has a random `lane_id` and is bound to the authenticated session.
The server rejects a lane that is not explicitly joined to an existing
session. Lane joins are idempotent and expire unless refreshed.

## Reliability

QUIC streams carry frames reliably, but cross-lane ordering is the session
layer's responsibility. Frames may be delivered out of order; the receiver
buffers within a bounded reassembly window and delivers contiguously.

What a sender retains for a lane that dies is exactly two things: the chunks the
scheduler is holding, which are unacknowledged application bytes and may be
re-offered to any lane, and this flow's own half-close. There is no second,
frame-level retention window: it held the same bytes under a second limit, and
the budget, eviction path and "unreplayable" state that bounded it were all
mechanism for a copy that did not need to exist.

A lane join that names a session the peer does not hold is answered with a
`RESET` carrying `unknown session`. That answer is permanent -- a session
identifier is random and is never reissued -- so a client must treat it as
final rather than retrying.

TCP fallback uses the same frame protocol so higher layers do not depend on the
transport.

## Compatibility and security requirements

- TLS certificate verification is mandatory by default.
- No custom cryptography is permitted.
- Credentials are never logged.
- Error messages do not disclose whether arbitrary destinations are reachable.
- Handshake, frame, flow, buffer, and reconnect limits are configurable.
- Version negotiation must fail closed for unsupported versions.
