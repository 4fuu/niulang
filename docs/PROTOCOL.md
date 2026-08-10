# wanopt wire protocol (draft 0)

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
`WINDOW`, `CLOSE`, `RESET`, `PING`, `PONG`, `PACKET`, and `OPEN_FAST`.

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
new and failed in-session associations. An in-session rescue currently opens
a fresh authenticated association while retaining the local SOCKS UDP socket;
it does not resume the old remote UDP relay or retransmit datagrams lost during
the transport transition.
Native QUIC DATAGRAM mode is a planned optimization for loss-sensitive UDP.

`CLOSE` with `FlagFin` is a directional half-close. A sender may later send
the same final sequence with `FlagCloseAbort` when its application socket has
fully closed and the peer should release an otherwise idle keep-alive
destination. The receiver acknowledges the abort sequence before closing its
inner connection. This escalation is deliberately delayed after the normal
final ACK so legitimate half-closed uploads and interactive sessions can
continue to receive response bytes.

## Backpressure

`WINDOW` advertises the highest byte sequence the receiver is prepared to
buffer. The sender must not exceed it. Global session and per-flow buffered
byte limits are mandatory; a slow application must not make the remote agent
unboundedly buffer a large download.

## Lane identity

Each lane has a random `lane_id` and is bound to the authenticated session.
The server rejects a lane that is not explicitly joined to an existing
session. Lane joins are idempotent and expire unless refreshed.

## Reliability

QUIC streams can carry frames reliably, but cross-lane ordering is still the
responsibility of the session layer. Frames may be delivered out of order;
the receiver buffers only within a bounded reassembly window. TCP fallback
uses the same frame protocol so higher layers do not depend on the transport.

## Compatibility and security requirements

- TLS certificate verification is mandatory by default.
- No custom cryptography is permitted.
- Credentials are never logged.
- Error messages do not disclose whether arbitrary destinations are reachable.
- Handshake, frame, flow, buffer, and reconnect limits are configurable.
- Version negotiation must fail closed for unsupported versions.
