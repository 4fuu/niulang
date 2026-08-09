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
`WINDOW`, `CLOSE`, `RESET`, `PING`, and `PONG`.

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

