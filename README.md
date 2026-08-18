# Queqiao

Queqiao is a TLS 1.3–authenticated performance-enhancing proxy for difficult
long-distance links. A local agent exposes SOCKS5; a provider gateway carries
TCP and UDP traffic over QUIC, with TLS/TCP fallback.

The security model is intentionally simple for users:

- A provider sends one `queqiao://` invitation.
- The user imports it once. Queqiao generates the device key locally and
  writes one private client profile.
- No DNS certificate, public CA, CA file, UUID, password, or shared tunnel
  secret is configured by the user.
- Normal traffic is always encrypted and mutually authenticated. Encryption is
  not an optional mode that can accidentally be disabled.

The project is pre-release. Protocol version 4 is the only supported protocol;
there is no compatibility path for the former shared-secret handshake.

## Quick start

Build and test:

```sh
go test ./...
go build ./cmd/queqiaod
```

### Provider

Initialize one provider and gateway. `--endpoint` is the public address placed
in invitations and profiles; `--listen` below is the local bind address.

```sh
sudo queqiaod provider init \
  --state /var/lib/queqiao \
  --name "Example Network" \
  --endpoint gateway.example.net:443

sudo queqiaod provider add-user \
  --state /var/lib/queqiao \
  --name alice \
  --max-sessions 8

sudo queqiaod server \
  --state /var/lib/queqiao \
  --listen :443
```

Create a single-use invitation, valid for 24 hours by default:

```sh
sudo queqiaod provider invite --state /var/lib/queqiao --user alice
```

The command prints only the importable URI, so a portal can capture it or turn
it into a QR code. Invitation lifetimes are capped at seven days. The provider
stores only a SHA-256 digest of the random 256-bit enrollment token.

Useful provider operations:

```sh
sudo queqiaod provider list-users --state /var/lib/queqiao
sudo queqiaod provider list-invites --state /var/lib/queqiao --user alice
sudo queqiaod provider revoke-invite --state /var/lib/queqiao --invite INVITE_ID
sudo queqiaod provider list-devices --state /var/lib/queqiao --user alice
sudo queqiaod provider revoke-device --state /var/lib/queqiao --device DEVICE_ID
sudo queqiaod provider disable-user --state /var/lib/queqiao --user alice
```

Authorization changes are atomically persisted and a running gateway reloads
them automatically. New streams are refused immediately after reload; active
TCP, QUIC, and UDP flows are closed within about one second. Per-user session
limits apply across all of that user's devices.

### User

Import the URI. A device name defaults to the machine hostname, and the profile
path defaults to the operating system's user configuration directory.

```sh
queqiaod enroll 'queqiao://enroll/…'
```

The result is a single mode-0600 JSON profile. Start the SOCKS5 client with it:

```sh
queqiaod client --profile ~/.config/queqiao/PROVIDER_ID.json
```

The default listener is `127.0.0.1:1080`. Applications, Clash/mihomo, or a
system proxy can use it as an ordinary SOCKS5 proxy. SOCKS5 UDP ASSOCIATE is
supported. See [the Clash starter profile](deploy/clash-queqiao.yaml).

Device certificates are short-lived. The running client checks hourly and
automatically renews them in the final seven days using the same device key;
no restart or manual certificate operation is required. Renewal is mutually
authenticated and cannot change the account or device identity. If a profile
is lost, a device is revoked, or renewal is allowed to pass certificate expiry,
the provider issues a new one-time invitation.

## Android and iOS

The repository includes native Android and iOS clients in [`mobile/`](mobile).
They use the same protocol-4 client, enrollment, renewal, QUIC/TCP fallback,
UDP support, congestion behavior, and resource bounds as the desktop client.
The mobile packet adapter and SOCKS client are Queqiao-owned code; the project
does not embed tun2socks, sing-box, mihomo, or another proxy application.

Both major stores currently require an organization for a VPN app. Apple App
Review Guideline 5.4 says VPN apps may only be offered by developers enrolled
as an organization. Google Play likewise requires apps approved to use
`VpnService` to register with an Organization developer account. Therefore an
individual maintainer will not publish Queqiao to either App Store or Google
Play under a personal account.

Android remains buildable as a signed APK/AAB and can be distributed directly
where permitted. Wide direct distribution should use Android Developer Console
identity and package registration; its personal Full Distribution account is
distinct from Google Play publication. The iOS target is source-build only:
developers with their own paid Apple Developer Program membership select their
team and unique bundle identifiers, then sign it onto their own physical
devices. It is not a generally installable unsigned iPhone package.

Exact prerequisites, signing steps, current store and direct-distribution
constraints, the parity matrix, and release gates are in
[`docs/MOBILE.md`](docs/MOBILE.md). Mobile privacy behavior is documented in
[`PRIVACY.md`](PRIVACY.md).

## Trust model

Provider initialization creates an Ed25519 trust domain:

```text
provider root public key (pinned by invitation)
├── gateway issuer → gateway TLS identity
└── device issuer  → per-device TLS identity
```

The invitation embeds a compact SHA-256 root fingerprint and expected provider
and gateway IDs. During enrollment, the client validates that exact chain and
gateway URI identity without DNS/SNI. It then generates its Ed25519 private key
locally and submits only the public key. Normal TCP and QUIC connections use
TLS 1.3 mutual authentication; the server additionally checks the device's
current account, revocation state, expiry, and registered public key.

Session and flow IDs are random routing identifiers, never credentials. A lane
join and a retained UDP relay must match the authenticated device principal
that created the original resource.

More detail is in [SECURITY.md](SECURITY.md) and
[docs/PROTOCOL.md](docs/PROTOCOL.md).

## Network behavior

- QUIC is preferred and pooled so warm flow setup has no extra authentication
  exchange.
- TLS/TCP is a warm fallback when UDP is unavailable.
- Classified bulk traffic can move away from a pooled control connection.
- Range acknowledgements and bounded recovery preserve logical byte ordering
  across lane failures.
- A bounded, destination-free probe can measure a changed uplink before the
  first user flow.
- Destination policy rejects private and link-local addresses by default;
  providers must explicitly opt in with `--allow-private-destinations`.

Run `queqiaod client -h` or `queqiaod server -h` for performance and transport
tuning flags. The secure identity fields are profile/state derived and have no
command-line override.

## Repository map

- `cmd/queqiaod`: provider administration, enrollment, client, and server CLI
- `internal/identity`: provider PKI, invitation/profile handling, authorization,
  enrollment, renewal, and TLS verification
- `internal/pep`: SOCKS ingress, gateway, QUIC/TCP transport, recovery, and UDP
- `internal/protocol`: bounded version-4 framing
- `mobile/core`: shared packet engine and platform binding
- `mobile/android`: native Android `VpnService` application
- `mobile/ios`: native iOS Network Extension application
- `cmd/queqiaobench`: deterministic WAN benchmark harness
- `deploy`: service-manager and Clash examples

## Security reports

Please report vulnerabilities privately as described in [SECURITY.md](SECURITY.md).
