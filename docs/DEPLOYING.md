# Deployment guide

## Provider gateway

Create a dedicated service account and private state directory:

```sh
sudo install -d -m 0700 -o queqiao -g queqiao /var/lib/queqiao
sudo -u queqiao queqiaod provider init \
  --state /var/lib/queqiao/provider \
  --name "Example Network" \
  --endpoint gateway.example.net:443
```

The state path passed to `provider init` must not exist. This prevents an
operator typo from replacing the root pinned by every enrolled device. Back up
the resulting directory encrypted and restrict it to trusted administrators;
it contains provider issuer keys.

Start the gateway:

```sh
sudo -u queqiao queqiaod server \
  --state /var/lib/queqiao/provider \
  --listen :443 \
  --metrics-listen 127.0.0.1:19090
```

The default `auto` transport binds TCP and UDP to the same port. Permit both in
the host and cloud firewalls. TCP remains available when networks block UDP.
The systemd unit in `deploy/queqiaod.service` expects `QUEQIAOD_ARGS` in
`/etc/queqiao/queqiaod.env`, for example:

```sh
QUEQIAOD_ARGS=--state /var/lib/queqiao/provider --listen :443
```

Private, loopback, link-local, multicast, and unspecified destinations are
blocked after DNS resolution. Use `--allow-private-destinations` only when the
gateway is intentionally an access proxy into a private network.

## Users and devices

```sh
sudo -u queqiao queqiaod provider add-user \
  --state /var/lib/queqiao/provider --name alice --max-sessions 8

sudo -u queqiao queqiaod provider invite \
  --state /var/lib/queqiao/provider --user alice --expires-in 1h
```

Deliver the single printed URI through an authenticated private channel. It is
a temporary bearer credential. It can be rendered as a QR code or imported by
a provider application without translating fields.

Outstanding invitations can be audited and revoked without exposing their
tokens:

```sh
queqiaod provider list-invites --state /var/lib/queqiao/provider --user alice
queqiaod provider revoke-invite --state /var/lib/queqiao/provider --invite ID
```

The user runs:

```sh
queqiaod enroll 'queqiao://enroll/…'
queqiaod client --profile ~/.config/queqiao/PROVIDER_ID.json
```

No certificate files or shared secret cross the provider/user boundary. The
client key is created locally and the imported profile is mode 0600.

List and revoke devices:

```sh
queqiaod provider list-devices --state /var/lib/queqiao/provider --user alice
queqiaod provider revoke-device --state /var/lib/queqiao/provider --device ID
```

A running gateway reloads atomic authorization changes. Revocation blocks new
streams and closes active flows within about one second. Disabling a user has
the same effect across all devices. Re-enabling a user does not un-revoke an
individual device.

## Clash/mihomo

Queqiao exposes SOCKS5 rather than a Clash-native protocol. Start the Queqiao
client first, then add a normal loopback SOCKS5 proxy with UDP enabled. See
`deploy/clash-queqiao.yaml`.

When Clash TUN mode captures the default route, use `--local-address if:en0`
(or the relevant physical interface) so Queqiao's connection to its gateway
does not loop back into Clash. `auto` is the safe default outside such forced
TUN routing.

## Certificates and renewal

Users do not install or renew certificates manually. Device leaves last 30
days; a running client checks hourly and renews in the final seven days using
mutual TLS and the same private key. Gateway leaves are reloaded and renewed
hourly from provider state without interrupting established tunnels. Keep
system clocks correct.

If automatic device renewal fails while the existing identity remains valid,
the client logs a warning and continues. Repair reachability or provider state
before expiry. An expired or revoked identity needs a new invitation.

## Monitoring and backup

Bind metrics to loopback or a protected management network. Logs omit profile
keys, invitations, session IDs, and payloads. Back up provider state after
initialization and after user/device changes; back up client profiles as
secrets if re-enrollment would be inconvenient.

Restoring provider state preserves the root pin and enrolled clients. Creating
a new provider state directory creates a new trust domain and requires all
clients to import new invitations.

## Troubleshooting

| Symptom | Check |
|---|---|
| Enrollment reports a pin/identity error | The URI may target another gateway, or traffic is intercepted. Do not bypass verification. |
| Invitation is expired/already used | Retry the existing `.enrolling` draft after an interrupted import; otherwise issue a fresh invitation. |
| Client profile is rejected | It must be complete, strict JSON, and mode 0600 or stricter. |
| QUIC fails but TCP works | Permit UDP on the gateway port; `auto` will temporarily fall back. |
| Client loops through Clash TUN | Bind `--local-address` to the physical interface. |
| Revocation does not appear immediately | Confirm the provider command changed the same `--state` path and inspect gateway reload logs. |
| All existing clients fail after state work | Restore the original provider state; do not re-run `provider init`. |
