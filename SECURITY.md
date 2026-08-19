# Queqiao security model

## Supported security design

Queqiao protocol version 1 has one data-plane security mode: TLS 1.3 with
provider-pinned gateway authentication and provider-issued per-device mutual
authentication. Plaintext transport and application-level shared secrets do
not exist.

The provider root is an Ed25519 public-key trust anchor. Its SHA-256 fingerprint
and the expected provider/gateway identities are carried in a one-time
invitation and then in the client's single private profile. A public DNS name,
WebPKI certificate, and user-managed root CA file are unnecessary.

## Credentials

- Provider state contains the root and constrained gateway/device issuer keys,
  gateway identity, authorization database, and hashed enrollment tokens. The
  directory must be accessible only to the service account and its trusted
  administrators.
- Invitations contain provider metadata, a root fingerprint, expiry, and a
  random 256-bit bearer token. They contain no device private key. Treat an
  unused invitation like a temporary password and send it over a private
  channel. Mobile users should paste or explicitly share the invitation into
  Queqiao; the apps do not claim the unauthenticated `queqiao` custom URL scheme,
  which another installed application could impersonate.
- The client generates an Ed25519 device key locally during enrollment. The
  mode-0600 profile contains that key, its certificate, endpoint, and pinned
  provider identity. Back it up or re-enroll; never publish it.
- The provider stores the registered device public key, not its private key.

Invitation tokens are single-use, expire within seven days, and are stored by
digest. Concurrent consumption is atomic. Enrollment uses a separate ALPN,
strict bounded messages, and exact gateway pinning. The server selects the
unauthenticated enrollment TLS policy only when the client offers exactly that
ALPN, preventing ALPN-confusion downgrade of normal data connections.
If the success response is lost, the client retries its durably saved draft;
the provider reissues only for the exact already-registered device name and
public key. A changed key remains a replay failure, including after the
invitation's original expiry.

Enrollment, renewal, and normal client connections can bind their outer TCP or
UDP socket to `auto`, `if:NAME`, or a literal local IP. The CLI defaults to
`auto`, which excludes loopback and point-to-point TUN interfaces. This is a
route-isolation property rather than an authentication exception: the same
provider pin and TLS policy apply regardless of the selected source address.

## Authorization and isolation

Every data connection is authorized during the TLS handshake. Long-lived QUIC
connections are re-authorized for every new stream. Active flows and UDP
associations periodically recheck authorization, so device revocation, account
disablement, and account expiry take effect without restarting the server.

Per-user session limits span all devices. Session IDs, flow IDs, lane IDs, and
UDP resume tokens are not identities. Lane joins and UDP relay reclamation must
match the provider/account/device principal established by TLS.

Certificate roles are separated by constrained intermediates and EKUs:
gateway certificates cannot authenticate as devices, and device certificates
cannot authenticate as gateways. Client verification checks TLS 1.3, the exact
pinned root, the complete chain, server-auth usage, and the expected URI
identity. It intentionally does not use DNS names.

## Rotation and revocation

Device certificates last 30 days. A running client checks hourly and renews
automatically in the final seven days. Renewal uses mutual TLS, preserves the
device key and identity, reuses the client data path's physical source
selection, and rechecks server-side authorization. Gateway leaf
identities are reloaded and renewed hourly without stopping established
tunnels. Revoked or expired devices must use a fresh invitation.

If an unused invitation is exposed, revoke its ID with `provider
revoke-invite`; a successful unknown enrollment appears in `provider
list-devices` and should be revoked. If a client profile is exposed, revoke
that device. If provider state is exposed, stop the gateway, replace the
provider trust domain, and re-enroll all users: an issuer compromise cannot be
repaired by device revocation alone.

## Network policy and resource bounds

The gateway rejects private, loopback, multicast, unspecified, and link-local
destinations by default, including DNS results and IPv4 destinations encoded in
NAT64/6to4 IPv6 prefixes, to reduce SSRF and network-pivot risk. Non-routable
IANA special-purpose ranges are rejected as well. Operators can explicitly
allow private destinations for controlled deployments.

TLS/enrollment deadlines, maximum frame and enrollment sizes, global and
per-user sessions, lane counts, retained UDP relays, acknowledgement ranges,
and path probes are bounded. Authorization updates use atomic file replacement;
a malformed, structurally inconsistent, or unexpectedly extended update leaves
the last known-good in-memory state active.

## Limitations

- Queqiao is an encrypted proxy, not an anonymity system. The provider can see
  destination metadata and traffic timing.
- A compromised endpoint can use its own authorized account until revoked.
- The compact invitation solves trust bootstrap, not secure delivery; the
  provider still needs an authenticated channel to give it to the intended
  user.
- Provider state is a high-value online secret and should be protected with OS
  access controls, encrypted backups, and a dedicated service account.

## Reporting a vulnerability

Report privately through GitHub's private vulnerability reporting:

**<https://github.com/bojieli/queqiao/security/advisories/new>**

The draft advisory is visible only to you and the maintainer and stays private
until a fix is published, so it is the right place for exploit detail. If that
form is not available to you, open a public issue saying only that you hold a
security report and asking for a private channel: no reproduction, no affected
revision, no credentials.

Include the affected revision, the reproduction, the impact, and any proposed
mitigation. Remove client profiles, invitations, provider state, packet
captures, and public IP addresses from what you attach.

Do not open a public issue or pull request that carries exploit details or
credentials.
