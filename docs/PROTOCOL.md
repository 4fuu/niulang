# Queqiao protocol version 1

> [!IMPORTANT]
> **Status:** First public wire contract
>
> **Wire version byte:** `1`
>
> **Data ALPN:** `queqiao/1`
>
> **Compatibility:** Version 1 only; mismatches fail closed
> **Last reviewed:** 2026-08-19

This document specifies the protocol implemented by the current Queqiao source
tree. Earlier private development builds used higher internal wire numbers.
Those builds were never a public compatibility contract; the first public
protocol is deliberately numbered 1 and has no legacy handshake or downgrade
path.

The key words **MUST**, **MUST NOT**, **SHOULD**, and **MAY** describe protocol
requirements. Unless stated otherwise, integers are unsigned and encoded in
network byte order (big-endian).

## 1. Protocol layers

Queqiao version 1 has four related layers:

1. **Identity bootstrap:** a `queqiao://` invitation and a bounded enrollment
   exchange create a per-device identity.
2. **Authenticated carrier:** TLS 1.3 over QUIC/UDP or TCP establishes the
   provider, gateway, account, and device principal.
3. **Logical flow protocol:** fixed-size frame headers carry TCP byte streams,
   UDP packets, acknowledgements, recovery state, and lane lifecycle.
4. **Optional coded datagram substrate:** QUIC DATAGRAM carries selected DATA
   frames through a sliding-window erasure code. The reliable QUIC stream
   always remains the control substrate.

Application TLS is not terminated. Queqiao sees the requested destination,
frame sizes, and timing, then relays application bytes or datagrams.

## 2. Carrier and TLS contract

The gateway normally listens on the same numeric port for UDP and TCP.

| Purpose | Carrier | TLS authentication | ALPN |
| --- | --- | --- | --- |
| Data over QUIC | QUIC over UDP | mutual TLS | `queqiao/1` |
| Data over TCP | TLS over TCP | mutual TLS | `queqiao/1` |
| First enrollment | TLS over TCP | pinned gateway; no client certificate | `queqiao-enroll/1` |
| Device renewal | TLS over TCP | mutual TLS | `queqiao-renew/1` |

TLS 1.3 is mandatory. There is no plaintext data mode and no application-level
shared tunnel secret.

### 2.1 Provider and endpoint identity

The invitation pins a self-signed Ed25519 provider root by SHA-256 fingerprint.
The client validates a complete certificate chain to that exact root and checks
one gateway URI identity:

```text
queqiao://PROVIDER_ID/gateway/GATEWAY_ID
```

DNS names are routing inputs, not identities. A device certificate contains a
client-auth identity of the form:

```text
queqiao://PROVIDER_ID/account/ACCOUNT_ID/device/DEVICE_ID
```

Constrained gateway and device issuers separate certificate roles. A gateway
certificate cannot authenticate as a device, and a device certificate cannot
authenticate as a gateway.

The server MUST authorize the device public key and mutable account/device
state during the TLS handshake. A long-lived QUIC connection MAY carry many
streams, so the server re-authorizes the established principal before accepting
each new OPEN, JOIN, or PROBE stream. Active resources are also subject to
server-side revocation checks.

### 2.2 ALPN isolation

The unauthenticated enrollment TLS configuration is selected only when the
client offers exactly `queqiao-enroll/1`. Renewal is selected only when the
client offers exactly `queqiao-renew/1`. Offering either control ALPN alongside
another protocol MUST NOT select the weaker enrollment configuration.

A data connection that does not negotiate `queqiao/1` is incompatible and MUST
be rejected. Neither endpoint falls back to a previous Queqiao protocol.

### 2.3 QUIC and TCP carriage

Each QUIC bidirectional stream or TLS/TCP connection carries a sequence of
Queqiao frames on its reliable byte stream. QUIC connections negotiate DATAGRAM
support; when both endpoints support it, the same connection can additionally
carry coded DATA frames and UDP PACKET frames as datagrams.

One QUIC connection may pool many logical flows on separate streams. QUIC
datagrams are connection-scoped and are demultiplexed by the `flow_id` inside
the recovered Queqiao frame.

TCP uses the identical reliable frame stream. A flow that has handed off to
TCP MUST NOT simultaneously schedule data over QUIC. A configured TCP-only
bundle may attach additional authenticated TCP lanes with JOIN; each socket
retains its kernel congestion controller, while Queqiao preserves one logical
byte-offset space above them.

## 3. Identifiers and scope

| Identifier | Width | Requirement | Scope |
| --- | ---: | --- | --- |
| `session_id` | 128 bits | Random and non-zero, except no all-zero value is accepted | Names the logical session and its replacement lanes |
| `flow_id` | 64 bits | Random and non-zero for TCP/UDP flows; zero only for PROBE | Demultiplexes a flow within a session/connection |
| `lane_id` | 64 bits | Non-zero on JOIN; lane zero is the initial lane | Names a physical carrier attached to a logical flow |
| UDP resume token | 128 bits | Random, single-use, principal-bound | Reclaims one retained gateway UDP relay |

Identifiers route authenticated state; none is a credential. A JOIN or UDP
resume request MUST have the same provider/account/device principal as the
resource creator. Implementations MUST NOT authorize a request from possession
of an identifier alone.

## 4. Frame envelope

Every logical frame begins with this fixed 46-byte header:

| Offset | Size | Field | Version-1 rule |
| ---: | ---: | --- | --- |
| 0 | 2 | magic | ASCII `WO` (`0x57 0x4f`) |
| 2 | 1 | version | `0x01` |
| 3 | 1 | type | One value from §5 |
| 4 | 2 | flags | Only flags valid for the frame type |
| 6 | 16 | session ID | Opaque bytes |
| 22 | 8 | flow ID | Unsigned integer |
| 30 | 8 | sequence | Meaning depends on frame type |
| 38 | 4 | payload length | Number of bytes following this header |
| 42 | 1 | traffic class | `0`, `1`, or `2` |
| 43 | 3 | reserved | All zero |

The default absolute payload limit is 1 MiB. A deployment MAY configure a
smaller limit on both peers; the receiver validates the declared length
before allocating. The frame header and payload form one record. A datagram
MUST contain exactly one complete encoded frame after coded-substrate
reassembly; trailing bytes are invalid.

The receiver MUST reject bad magic, a version other than 1, an unknown type,
unknown flags, a class above 2, non-zero reserved bytes, or a payload beyond its
limit. A version mismatch is reported distinctly from malformed framing so an
operator can perform a coordinated upgrade.

## 5. Frame types

| Value | Name | Purpose | Normal payload |
| ---: | --- | --- | --- |
| 1 | `OPEN` | Create a TCP flow or UDP association | Destination or UDP marker |
| 2 | `OPEN_OK` | Accept OPEN or JOIN | Empty, except resumable UDP grant |
| 3 | `JOIN` | Attach a replacement/isolation/TCP lane | 8-byte lane ID |
| 4 | `DATA` | Carry bytes at a logical offset | Application bytes |
| 5 | `ACK` | Acknowledge a cumulative byte offset and optional ranges | Optional ACK ranges |
| 6 | `CLOSE` | Declare a direction's final offset or abort | Empty |
| 7 | `RESET` | Refuse or terminate with a coarse reason | Reset code and text |
| 8 | `PACKET` | Carry one UDP datagram and destination | Encoded UDP packet |
| 9 | `PROBE` | Bounded authenticated path padding/echo | Non-empty padding |

The first frame on an authenticated stream MUST be OPEN, JOIN, or PROBE. All
later frames must match the session and flow established by that first frame.

## 6. Flags

| Bit | Name | Valid use |
| ---: | --- | --- |
| 0 | `FIN` | Required on CLOSE |
| 1 | `ACK_FINAL` | ACK that confirms a final offset |
| 2 | `ACK_UP` | ACK covers client-to-gateway bytes |
| 3 | `ACK_DOWN` | ACK covers gateway-to-client bytes |
| 4 | `CLOSE_ABORT` | CLOSE cancels both directions instead of half-closing one |
| 5 | `RESERVE_CONTROL` | OPEN reserves the initial lane as control, or JOIN replaces that role |
| 7 | `ACK_RANGES` | ACK payload contains selective byte ranges |

Bits 6 and 8–15 are reserved and MUST be zero. `RESERVE_CONTROL` is invalid on
any type other than OPEN or JOIN. `ACK_RANGES` is invalid on any type other
than ACK.

State-machine validation is stricter than header parsing:

- DATA, OPEN_OK, RESET, PACKET, and PROBE normally carry zero flags.
- A TCP-flow ACK carries exactly one of `ACK_UP` or `ACK_DOWN`; it may add
  `ACK_FINAL` or `ACK_RANGES`, but a final ACK has no range payload.
- A UDP close ACK is `ACK_FINAL` with no direction bit and sequence zero.
- CLOSE carries `FIN` and may add `CLOSE_ABORT`; ACK direction/final bits are
  invalid on CLOSE.

## 7. Traffic class is a hint

The class byte has these values:

| Value | Name |
| ---: | --- |
| 0 | `NEW` |
| 1 | `INTERACTIVE` |
| 2 | `BULK` |

Class is behavioral metadata used by scheduling and telemetry. It is not a
separate flow protocol, an authorization boundary, or an application-declared
quality-of-service entitlement. A receiver MUST NOT trust the class byte to
grant access or allocate unbounded resources. The class may change over the
life of one flow while all other identifiers and sequencing remain unchanged.

## 8. Opening a flow

### 8.1 TCP destination OPEN

The client chooses a non-zero random session ID and flow ID. OPEN uses sequence
zero, class NEW, and either zero flags or `RESERVE_CONTROL`. Its payload is the
UTF-8/ASCII-compatible canonical `host:port` destination:

- payload length is 1–255 bytes;
- host is non-empty and contains no space or control character;
- port is decimal in the range 1–65535;
- IPv6 literals use brackets; and
- alternate spellings such as zero-padded ports are canonicalized.

DNS resolution and destination policy are applied at the gateway. Private,
loopback, multicast, link-local, and unspecified destinations are rejected by
default at the product-policy layer.

The gateway answers with matching session/flow IDs:

- OPEN_OK with an empty payload after the destination is accepted; or
- RESET with a coarse error payload.

The client may optimistically accept application bytes and send DATA before it
receives OPEN_OK. On the reliable stream, OPEN precedes those DATA frames. If
the first DATA also uses coded datagrams, the sender places one bounded safety
copy of its first frame behind OPEN on the reliable stream until OPEN_OK is
observed. The receiver may briefly hold a bounded number of coded frames that
arrive before their OPEN is processed.

### 8.2 UDP association OPEN

An OPEN payload beginning with the following exact five-byte marker creates a
basic UDP association:

```text
57 4f 55 44 01    # "WOUD" + 1
```

A resumable association uses:

```text
57 4f 55 44 02 [optional 16-byte resume token]
```

With no token, the gateway creates a relay and returns an OPEN_OK payload:

| Offset | Size | Field |
| ---: | ---: | --- |
| 0 | 1 | `resumed`: `0` for fresh, `1` for reclaimed |
| 1 | 16 | newly issued single-use resume token |

With a token, the gateway attempts to reclaim the retained relay belonging to
the same authenticated device. An unknown, expired, used, or wrong-principal
token degrades to a fresh relay; it never authorizes access to another relay.
Every successful open issues a new token, including a successful resume.

## 9. TCP byte-stream state machine

Both application directions have independent sequence and FIN state within the
same logical flow.

```mermaid
stateDiagram-v2
    [*] --> Opening: OPEN
    Opening --> Active: OPEN_OK
    Opening --> Reset: RESET
    Active --> HalfClosed: local CLOSE(FIN)
    Active --> Active: DATA / cumulative ACK
    Active --> Reset: RESET or CLOSE(FIN|ABORT)
    HalfClosed --> Closing: ACK_FINAL for local FIN
    HalfClosed --> Closing: peer CLOSE(FIN)
    Closing --> Closed: both FIN directions acknowledged
    Reset --> [*]
    Closed --> [*]
```

The diagram is conceptual: optimistic DATA may be in flight during Opening,
and either direction can half-close first.

### 9.1 DATA

For DATA, `sequence` is the zero-based byte offset of `payload` in the sender's
application byte stream. The interval carried is
`[sequence, sequence + payload_length)`. Receivers place segments by offset,
deduplicate overlap, buffer within explicit limits, and deliver only the
contiguous prefix to the application.

Arrival order is not application order. DATA may be retransmitted on a
replacement lane, delivered out of order by coded datagrams, or striped across
configured TCP fallback lanes without changing its logical offset.

### 9.2 ACK

For ACK, `sequence` is the cumulative next byte: every byte below it in the
direction named by `ACK_UP` or `ACK_DOWN` has arrived.

When `ACK_RANGES` is set, the payload is zero to 16 entries of:

| Offset within entry | Size | Field |
| ---: | ---: | --- |
| 0 | 8 | inclusive start |
| 8 | 8 | exclusive end |

Each `[start,end)` range MUST be non-empty, at or above the cumulative
sequence, sorted by start, and non-overlapping. Adjacent ranges are allowed.
The payload length MUST be a multiple of 16 and no greater than 256 bytes.

Protocol ACKs bound Queqiao's retained replay window and report delivery across
unreliable or replacement substrates. They do not replace QUIC or TCP transport
acknowledgements and are not a second per-packet congestion-control loop.

### 9.3 CLOSE and final ACK

CLOSE carries an empty payload, `FIN`, and the sender's final byte offset in
`sequence`. A normal CLOSE half-closes that application direction. The receiver
waits for every preceding byte, half-closes its destination socket in the same
direction, and returns ACK with the corresponding direction bit plus
`ACK_FINAL` at the exact final sequence.

`CLOSE_ABORT` means the application has fully abandoned the flow. The peer may
stop both directions and release the destination without waiting for missing
bytes, but it still returns the best-effort final ACK.

Final CLOSE and ACK state is idempotent. A server may retain a bounded,
metadata-only tombstone after both directions complete. A correctly
authenticated late JOIN can then receive the final ACK/CLOSE state; no
destination socket or application payload is retained in the tombstone.

## 10. Joining and replacing lanes

JOIN is the first frame on a separately authenticated stream or TLS/TCP
connection. It uses the existing non-zero session/flow IDs, sequence zero, and
an exactly eight-byte non-zero lane ID payload.

The gateway accepts JOIN only if:

- the target live flow or completion tombstone exists;
- the authenticated provider/account/device principal exactly matches the
  creator;
- the lane transport is compatible with the flow's current handoff state; and
- per-flow, per-user, and global lane limits permit it.

OPEN_OK acknowledges the join before the new lane becomes schedulable. RESET
refuses it. This ordering prevents replayed DATA from overtaking the join
acknowledgement.

`RESERVE_CONTROL` on JOIN replaces a failed control-lane generation. An
ordinary JOIN adds or replaces a data lane for isolation, recovery, or
configured TCP-only striping. The public protocol does not promise throughput
aggregation across QUIC connections; a QUIC flow has one data connection at a
time, apart from bounded transition/recovery state.

When the first TCP replacement is accepted for a QUIC flow, both endpoints
retire QUIC data lanes and continue on TCP. They do not mix QUIC and TCP data
for the same flow.

## 11. Reliable and datagram substrates

Every lane has a reliable control stream. Frame routing is:

| Frame | QUIC with DATAGRAM | QUIC without DATAGRAM | TLS/TCP |
| --- | --- | --- | --- |
| OPEN, OPEN_OK, JOIN, ACK, CLOSE, RESET, PROBE | reliable stream | reliable stream | reliable stream |
| DATA | coded datagram only while path coding and flow policy are active; otherwise stream | stream | stream |
| PACKET | QUIC datagram | stream fallback | stream fallback |

Control never enters the coded substrate. A DATA frame not recovered by FEC is
still missing at the logical-flow layer and can be reissued from retained
byte-offset state. PACKET retains UDP semantics and is not retransmitted merely
because its QUIC datagram was lost.

## 12. Coded datagram format

The coded substrate is directional and scoped to one QUIC connection. It
carries complete encoded Queqiao frames inside source symbols and GF(256)
repair symbols. Its delivery remains unreliable; FEC reduces erasure but does
not create a second reliable stream.

### 12.1 Datagram headers

Every coded datagram begins with a 32-bit transmission sequence used to infer
loss from gaps, followed by an 8-bit kind.

Source datagram (`kind = 0`):

| Offset | Size | Field |
| ---: | ---: | --- |
| 0 | 4 | transmission sequence |
| 4 | 1 | kind `0` |
| 5 | 4 | encoding symbol ID (`ESI`) |
| 9 | variable | source-symbol vector |

Repair datagram (`kind = 1`):

| Offset | Size | Field |
| ---: | ---: | --- |
| 0 | 4 | transmission sequence |
| 4 | 1 | kind `1` |
| 5 | 4 | repair ID (`RID`) |
| 9 | 4 | first source ESI covered |
| 13 | 2 | number of consecutive source symbols covered |
| 15 | variable | repair vector |

Transmission sequence, ESI, and RID advance independently modulo 2³². Unknown
kinds and truncated datagrams are discarded.

### 12.2 Source-symbol vector

Every source vector begins:

| Offset | Size | Field |
| ---: | ---: | --- |
| 0 | 2 | meaningful payload length |
| 2 | 2 | fragment index |
| 4 | 2 | fragment count |
| 6 | variable | payload, zero-extended by repair arithmetic as needed |

For `fragment_count = 1`, the payload packs one or more complete Queqiao frames,
each prefixed by a 32-bit frame length. A frame larger than one symbol uses
consecutive ESIs with a common fragment count and indexes `0..count-1`; its
payload is the direct frame bytes without the per-frame 32-bit prefix. The
receiver emits the frame only when all of its fragments are present or
recovered. Incomplete groups are discarded once they fall beyond the bounded
assembly window.

### 12.3 Repair arithmetic

Repair vectors are random-linear combinations over GF(256) using primitive
polynomial `0x11d` (`x^8 + x^4 + x^3 + x^2 + 1`). Shorter source vectors are
zero-extended to the longest vector in the repair window.

For repair ID `rid` and zero-based symbol position `index` within the advertised
window, both endpoints generate the non-zero coefficient:

```text
x = rid * 2654435761 + index * 2246822519       (mod 2^32)
x = x XOR (x >> 15)
x = x * 2654435761                              (mod 2^32)
x = x XOR (x >> 13)
x = x * 2246822519                              (mod 2^32)
x = x XOR (x >> 16)
coefficient = (x mod 255) + 1
```

The repair vector is the byte-wise XOR sum of each source vector multiplied by
its coefficient in GF(256). Coefficients are regenerated rather than sent.
The receiver keeps a bounded linear system and may deliver recovered symbols
out of order.

Window size and parity rate are sender policy derived from current path state;
they are not separately negotiated. Every repair carries the exact source
window needed to decode it.

## 13. UDP PACKET format and replay window

PACKET preserves exactly one UDP datagram boundary. Its payload is:

| Offset | Size | Field |
| ---: | ---: | --- |
| 0 | 2 | destination length `N` |
| 2 | `N` | canonical `host:port` destination |
| `2+N` | remaining | UDP payload, at most 65,507 bytes |

The destination follows the same canonical rules as TCP OPEN and is at most
255 bytes. Client-to-gateway packets may name a DNS host or IP; gateway replies
encode the numeric source address observed on the relay socket.

PACKET sequence is an independent monotonically increasing packet number in
each direction. Receivers use a 64-packet bitmap window: a new high sequence
advances the window, an unseen sequence within the window is accepted once,
and duplicates or older packets are dropped. A gap is normal UDP loss and does
not reset the association.

A graceful UDP dissociation sends CLOSE with `FIN`, sequence zero, and an empty
payload. The peer answers ACK with `ACK_FINAL`, sequence zero, and no payload.

## 14. Path PROBE

PROBE is authenticated, names no destination, and uses:

- a non-zero session ID;
- flow ID zero;
- sequence values starting at zero and increasing by one;
- class NEW and zero flags; and
- a non-empty payload of at most 1,200 bytes.

The gateway accepts at most 128 frames and 128 KiB on one probe stream. It
echoes every valid frame exactly once with the same header and payload. The
client half-closes its stream to delimit the request, reads the echoes, and
uses QUIC transport acknowledgements in each sending direction to update that
direction's endpoint-pair path state.

The echo is equal-size and mutually authenticated, so it cannot amplify
traffic or create an SSRF destination. Invalid ordering, identifiers, flags,
class, size, or totals terminate the probe.

## 15. RESET payloads

RESET payload byte zero is a coarse code; up to 256 following bytes may carry
diagnostic UTF-8 text. Codes are:

| Value | Name | Meaning |
| ---: | --- | --- |
| 1 | `PROTOCOL` | Invalid state, frame, join, or unknown session |
| 2 | `AUTHENTICATION` | Authenticated identity is not permitted |
| 3 | `DESTINATION` | Destination is invalid, denied, or unavailable |
| 4 | `FLOW_LIMIT` | Session/flow/lane admission limit |
| 5 | `TRANSPORT` | Required relay or carrier resource unavailable |

Messages are deliberately coarse and MUST NOT expose private authorization or
infrastructure detail. A RESET is terminal for the stream/attempt it addresses.

## 16. Enrollment and renewal messages

Invitation/profile schema versions and enrollment-service version 1 are
independent namespaces from the data-plane wire byte, even though all are `1`
for the first public release.

### 16.1 Invitation URI

The URI is:

```text
queqiao://enroll/BASE64URL_WITHOUT_PADDING(JSON)
```

The decoded strict JSON object contains:

| Field | Meaning |
| --- | --- |
| `v` | invitation schema, exactly `1` |
| `name` | display name, 1–128 characters |
| `provider` | provider ID |
| `endpoint` | gateway `host:port` |
| `gateway` | gateway ID |
| `root` | raw-base64url SHA-256 provider-root fingerprint |
| `token` | raw-base64url 256-bit enrollment bearer token |
| `expires` | RFC 3339 expiration, no more than seven days after issue |

Unknown fields, malformed endpoints, bad identifier shapes, excessive size,
and trailing JSON are rejected. The invitation is a short-lived credential and
must be delivered privately.

### 16.2 Message framing

Enrollment and renewal each exchange one request and one response on their
dedicated TLS connection. Every message is a 32-bit non-zero length followed by
one strict JSON object. The maximum message length is 64 KiB. Unknown fields or
trailing JSON values are invalid.

Enrollment request fields are `version` (`1`), `token`, `device_name`, and the
raw-base64url Ed25519 `public_key`. The device private key is generated and
persisted locally before the token is transmitted; it is never sent.

The successful response returns the provider/gateway identity, pinned root and
certificate, account/device IDs, issued device certificate, and issue time.
The client MUST validate that the certificate contains the locally generated
public key and the invited provider/gateway identity before saving the profile.

An exact retry after a lost enrollment response is idempotent for the already
registered device name and public key. A different key or name is a replay
failure, including after the invitation's original expiry.

Renewal uses mutual TLS on `queqiao-renew/1`, a request containing version `1`,
and the same bounded response form. Renewal MUST preserve provider, account,
device, and public-key identity.

## 17. Fallback behavior

Fallback is local policy around the same protocol, not a new wire version.
In automatic mode the client prefers QUIC and may prepare a delayed TLS/TCP
candidate. A merely slower TCP handshake is not proof that UDP failed. Only
differential evidence—QUIC failing while authenticated TCP reaches the same
endpoint—may place the endpoint in a temporary TCP-only cooldown.

An existing TCP byte stream can survive carrier loss by authenticating a JOIN
on the replacement transport and replaying unacknowledged logical offsets. UDP
rescue instead creates a new association and uses its principal-bound token to
reclaim the gateway relay socket. Datagrams in flight during failure remain
lost.

## 18. Versioning and extension rules

Version 1 has no generic capability-negotiation frame and no “ignore unknown”
extension rule. Unknown frame types, flags, classes, reserved bits, and wire
versions fail closed.

A future change that alters parsing, authentication, frame semantics, coded
datagram decoding, or state-machine behavior MUST increment the data protocol
version and change the data ALPN accordingly. It must document whether and how
operators can run both versions during a coordinated migration. Pre-1.0
software versioning does not waive this wire rule.

Changes to sender-only tuning that preserve every version-1 wire invariant—such
as pacing gains, classification thresholds, FEC rate selection, queue limits,
or fallback timing—do not require a wire increment.

## 19. Security and resource invariants

- TLS 1.3 is mandatory and every data carrier is mutually authenticated.
- Provider-root pinning plus URI identity replaces DNS/WebPKI authentication.
- Mutable authorization is checked independently of certificate validity.
- Session, flow, lane, and resume identifiers never grant authority.
- Payloads, ACK ranges, reassembly, replay, queues, probes, lanes, retained
  relays, idle time, and flow lifetime are bounded by the implementation.
- Enrollment/renewal are isolated by exact ALPN and bounded strict messages.
- Malformed or unknown protocol input never selects a legacy parser.

The broader threat model, credential lifecycle, and residual risks are in
[`SECURITY.md`](../SECURITY.md). Implementation components are mapped in
[`ARCHITECTURE.md`](ARCHITECTURE.md).
