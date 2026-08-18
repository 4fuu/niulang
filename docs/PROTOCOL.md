# Queqiao protocol version 4

Protocol version 4 is the only supported wire protocol. It deliberately has no
shared-secret HELLO, capability negotiation, or downgrade path.

## Transport and identity

TCP uses TLS 1.3 and QUIC uses TLS 1.3 through QUIC. Both negotiate
`queqiao/4`. A normal connection requires a provider-issued device certificate
and is rejected during TLS if the account/device is unknown, disabled, expired,
revoked, or registered under another public key.

The server certificate chain terminates at the provider root pinned in the
client profile. The client verifies the exact root fingerprint and the URI
identity `queqiao://PROVIDER/gateway/GATEWAY`; DNS names are not identities.
Device leaves use
`queqiao://PROVIDER/account/ACCOUNT/device/DEVICE` and client-auth EKU.

Two isolated control protocols share the listener:

- `queqiao-enroll/1`: no client certificate, exact ALPN offer only, one bounded
  invitation exchange.
- `queqiao-renew/1`: mutual TLS, one bounded certificate-renewal exchange.

Offering either ALPN together with another protocol does not select the weaker
enrollment configuration.

The client persists its generated key before first use of the one-time token.
An exact retry after a lost response is idempotent, even just after invitation
expiry; it can only recover the already registered device and cannot select a
different key or name.

## Frame envelope

All integers are big-endian. The fixed 46-byte header is:

| Offset | Size | Field |
|---:|---:|---|
| 0 | 2 | magic `WO` |
| 2 | 1 | version (`4`) |
| 3 | 1 | frame type |
| 4 | 2 | flags |
| 6 | 16 | session ID |
| 22 | 8 | flow ID |
| 30 | 8 | sequence |
| 38 | 4 | payload length |
| 42 | 1 | traffic class |
| 43 | 3 | zero reserved bytes |

Payloads are bounded before allocation. Unknown versions, types, flags, classes,
or non-zero reserved bytes fail closed.

Frame types are `OPEN`, `OPEN_OK`, `JOIN`, `DATA`, `ACK`, `CLOSE`, `RESET`,
`PACKET`, and `PROBE`.

## Flow open

TLS authenticates before application data, so the first stream frame is
`OPEN`. A non-zero random session and flow ID identify the logical flow. The
payload is a bounded canonical destination, the UDP association marker, or the
UDP-resume marker. The server applies destination policy and responds with
`OPEN_OK` or a coarse `RESET`.

`FlagReserveControl` is valid only on `OPEN`. It retains lane zero for
interactive/control traffic if classified bulk data moves to an independent
lane.

Clients may optimistically send application data before `OPEN_OK`; the flow
reader still requires and validates the acknowledgement. Until it arrives,
clean-path bytes remain behind `OPEN` on the reliable control stream. A coded
path may send immediately, but places one bounded duplicate of its first data
frame behind `OPEN`; early datagram loss or reordering therefore cannot leave
the remote byte stream permanently missing offset zero.
Operators can require destination confirmation before SOCKS success with
`--wait-for-open-ack`.

## Lane joins

An additional mutually authenticated stream begins with `JOIN`, whose payload
is exactly one non-zero 64-bit lane ID. The session/flow IDs select an existing
flow. The server attaches it only if its authenticated provider/account/device
principal exactly equals the creator's principal. Therefore intercepted IDs
are not bearer credentials and cannot cross users or devices.

## Ordered data and recovery

`DATA` sequence numbers are logical byte offsets. `ACK` carries a cumulative
offset and may include a bounded list of received byte ranges. `CLOSE` and final
ACKs implement half-close/final-close semantics. Lane replacement can re-send
unacknowledged chunks without changing the application's byte stream.

A completed flow retains a bounded metadata-only tombstone so a replacement
lane can recover a lost final ACK. It retains no destination socket or payload.

## UDP

`PACKET` preserves datagram boundaries and carries a canonical destination.
QUIC datagrams are preferred; an explicit measurement option can keep packets
on streams.

A failed association's relay socket can be retained briefly under a random
single-use token. Reclamation requires both the token and the same authenticated
device principal. Relays, grace time, and token width are bounded.

## Path probe

`PROBE` is authenticated, destination-free discard padding. It uses flow ID
zero and permits no flags. The server accepts at most 128 frames and 128 KiB per
probe stream and reflects no application response; QUIC transport
acknowledgements provide the loss measurement without an amplification or SSRF
surface.

## Security invariants

- TLS 1.3 is mandatory; there is no plaintext mode.
- Provider root pin + URI identity replaces DNS/WebPKI identity.
- Every normal connection has an authorized device certificate.
- Mutable authorization is checked independently of certificate validity.
- Routing identifiers never grant authority.
- Enrollment and renewal messages are length-bounded and versioned.
- Unknown protocol input fails closed; no legacy parsing branch exists.
