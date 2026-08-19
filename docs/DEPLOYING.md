# Deployment guide

> [!NOTE]
> **Status:** Current operational guide for public protocol 1
> **Last reviewed:** 2026-08-19

This guide covers a provider gateway, individual user enrollment, a desktop
SOCKS client, Clash/mihomo integration, and replacement of an earlier
development tunnel. Protocol 1 is the only supported wire protocol. Client and
server must therefore be upgraded together.

## What is configured where

The provider chooses three durable values:

- a private provider-state directory, conventionally
  `/var/lib/queqiao/provider`;
- a display name users will recognize; and
- one public `host:port` endpoint placed in every invitation and profile.

The endpoint may be an IP address or DNS name. Queqiao authenticates the
provider root pinned by the invitation, not a WebPKI name, so no public CA or
Let's Encrypt certificate is required. The endpoint must remain reachable on
both TCP and UDP unless the provider intentionally offers only one transport.

Each user receives one temporary `queqiao://` invitation. Importing it creates
one private profile containing the endpoint, provider pin, device certificate,
and locally generated device key. Users never copy provider keys, CA files,
shared secrets, or individual JSON fields.

## Install the gateway

Install the exact reviewed binary and confirm its protocol before creating
state:

```sh
sudo install -m 0755 ./queqiaod /usr/local/bin/queqiaod
/usr/local/bin/queqiaod --version
```

The output must contain `wire=1`. Create a dedicated account once:

```sh
sudo useradd --system --user-group \
  --home-dir /var/lib/queqiao --shell /usr/sbin/nologin queqiao
sudo install -d -m 0700 -o queqiao -g queqiao /var/lib/queqiao
sudo install -d -m 0750 -o queqiao -g queqiao /var/log/queqiao
```

Initialize a new trust domain. The final state path must not already exist:

```sh
sudo -u queqiao /usr/local/bin/queqiaod provider init \
  --state /var/lib/queqiao/provider \
  --name "Example Network" \
  --endpoint gateway.example.net:443
```

Refusing an existing path is intentional: silently replacing this root would
strand every enrolled device. Back up the resulting directory encrypted. It
contains issuer keys and is the provider's highest-value secret.

On Unix, the gateway refuses to load a provider directory accessible to the
group or other users and reports the exact `chmod 700` repair. On Windows,
POSIX mode bits do not describe access: place state under the dedicated service
account, remove inherited access with the directory's DACL (for example with
`icacls`), and grant full control only to that account and `SYSTEM`. Do not keep
provider state on a shared or synchronizing folder on either platform.

### systemd

Install [`deploy/queqiaod.service`](../deploy/queqiaod.service), then create
`/etc/queqiao/queqiaod.env` owned by `root:queqiao` with mode `0640`:

```text
QUEQIAOD_ARGS=--state /var/lib/queqiao/provider --listen :443 --transport auto --max-sessions 4096 --metrics-listen 127.0.0.1:19090 --log-level info --log-format json --log-file /var/log/queqiao/server.log --telemetry-log-interval 5s
```

The environment file is a whitespace-separated argument list; do not put a
state path containing spaces in it. Start and verify the service:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now queqiaod
systemctl is-active queqiaod
sudo ss -lntup | grep ':443'
curl -fsS http://127.0.0.1:19090/metrics | head
sudo test -s /var/log/queqiao/server.log
sudo tail -n 5 /var/log/queqiao/server.log
```

With `--transport auto`, two listener rows are expected: TCP and UDP on the
same port. Permit both in the host firewall and the cloud security group.
Binding metrics to loopback avoids exposing an unauthenticated operations
endpoint. The server runtime log is independent of `/metrics`: it contains the
same performance counters as timestamped JSON records and rotates internally
at 32 MiB with five backups. See [`LOGGING.md`](LOGGING.md).

### Tune provider socket queues

Linux's default socket limits are often too small for a QUIC gateway. A burst
of new flows can then overflow both the UDP receive queue and TCP listen queue
even when the host has idle CPU and memory. Queqiao's QUIC dependency requests
an 8 MiB UDP buffer; the provider should leave additional kernel headroom and
use larger network and SYN backlogs.

Run the repository's idempotent tuning script on every Linux provider:

```sh
sudo ./deploy/tune-server.sh
```

The script installs `/etc/sysctl.d/90-queqiao-performance.conf`, immediately
applies 16 MiB UDP socket maxima and larger network/TCP backlogs, verifies the
effective values, and restarts `queqiaod.service` if it is active so the QUIC
listener obtains its larger buffer. Use `--no-restart` when coordinating a
separate maintenance window, or `--service NAME` for a differently named
systemd unit. `--dry-run` prints the settings without changing the host.

Afterward, confirm that the listener has room and that its drop counters do not
keep increasing under normal traffic:

```sh
sudo ss -lntpm | grep queqiao
sudo ss -unapm | grep queqiao
nstat -az | grep -E 'UdpRcvbufErrors|ListenOverflows|ListenDrops'
```

Private, loopback, link-local, multicast, and unspecified destinations are
blocked after DNS resolution. Add `--allow-private-destinations` only when the
service is intentionally an access proxy into a private network.

## Add users and issue invitations

Create a separate account for every customer or administrative boundary:

```sh
sudo -u queqiao /usr/local/bin/queqiaod provider add-user \
  --state /var/lib/queqiao/provider \
  --name alice \
  --max-sessions 8
```

`--max-sessions 0` uses the gateway-wide limit. The limit spans all devices
owned by that user.

Create a one-time invitation and deliver the single printed URI through an
authenticated private channel:

```sh
sudo -u queqiao /usr/local/bin/queqiaod provider invite \
  --state /var/lib/queqiao/provider \
  --user alice \
  --expires-in 1h
```

The URI is a temporary bearer credential. The provider stores only its token
digest, and the lifetime cannot exceed seven days. A portal may capture the
stdout value or render it as a QR code without translating any fields.

Audit or revoke unused invitations without printing their tokens:

```sh
sudo -u queqiao /usr/local/bin/queqiaod provider list-invites \
  --state /var/lib/queqiao/provider --user alice
sudo -u queqiao /usr/local/bin/queqiaod provider revoke-invite \
  --state /var/lib/queqiao/provider --invite INVITE_ID
```

## Enroll a desktop client

The user imports the URI once:

```sh
queqiaod enroll 'queqiao://enroll/…'
```

The default profile path is printed on success. To select stable paths and a
recognizable device label explicitly:

```sh
queqiaod enroll 'queqiao://enroll/…' \
  --profile ~/.config/queqiao/example-network.json \
  --device-name alice-laptop \
  --local-address if:en0
```

`--local-address` defaults to `auto` for enrollment, normal client traffic,
and certificate renewal. Automatic selection excludes loopback and
point-to-point TUN interfaces. If two physical IPv4 interfaces are active,
Queqiao reports both instead of guessing; use `if:NAME` or a literal local IP.
This is especially important when Clash TUN owns the default route: bootstrap
and renewal must not depend on the tunnel they are configuring.

The device key is generated locally before the one-time token is sent. An
interrupted import leaves `PROFILE.enrolling`, mode `0600`. Retry the same URI,
profile path, and device name to reuse that key safely. Do not delete the draft
merely because the first response was lost; requesting a different key after
token consumption is correctly rejected as replay.

Start the client:

```sh
queqiaod client \
  --profile ~/.config/queqiao/example-network.json \
  --listen 127.0.0.1:12080 \
  --local-address if:en0 \
  --metrics-listen 127.0.0.1:12090
```

The client creates its JSON runtime log automatically. Run `queqiaod logs client`
to print the absolute path and a follow command; on macOS the default
is `~/Library/Logs/Queqiao/client.log`. The file contains five-second
performance snapshots and rotates with the same bounds as the server.

The profile must remain readable only by its owner. Queqiao rejects a
group/world-readable profile rather than silently using an exposed key.

For macOS, edit [`deploy/me.01.queqiao.client.plist`](../deploy/me.01.queqiao.client.plist)
to contain the installed binary and profile paths, then load it:

```sh
cp deploy/me.01.queqiao.client.plist ~/Library/LaunchAgents/
launchctl bootstrap "gui/$(id -u)" \
  ~/Library/LaunchAgents/me.01.queqiao.client.plist
launchctl print "gui/$(id -u)/me.01.queqiao.client"
tail -n 20 -f ~/Library/Logs/Queqiao/client.log
```

After changing an already loaded plist, use `launchctl bootout` followed by
`launchctl bootstrap`; `kickstart` restarts the cached definition and does not
re-read arguments from disk.

## Connect Clash or mihomo

Queqiao is a separate local SOCKS5 service, not a protocol parsed by Clash.
Add a loopback SOCKS5 node with UDP enabled and select it in the group used by
your rules. [`deploy/clash-queqiao.yaml`](../deploy/clash-queqiao.yaml) is a
complete starter profile; for an existing Clash profile, copy only its
`queqiao` proxy entry and add that name to the existing selector.

Verify the SOCKS service before selecting it:

```sh
nc -z 127.0.0.1 12080
curl --noproxy '' --proxy socks5h://127.0.0.1:12080 \
  --fail --show-error https://api.ipify.org
```

The empty `--noproxy` is deliberate. Environments with `NO_PROXY=*` otherwise
bypass even an explicitly supplied curl proxy and can produce a convincing but
irrelevant result. Confirm that client and server `flows_started_total`
counters increase during the request.

## Replace an earlier development tunnel

Protocol 4 deliberately has no shared-secret or wire-compatibility mode. An
old client cannot use a new server, and a new client cannot use an old server.
Replacing a service on the same endpoint therefore requires a brief coordinated
restart; existing flows cannot survive the protocol boundary.

Use this order:

1. Record the old client/server versions, arguments, listener, and a known-good
   proxy request.
2. Copy the old binaries, service definitions, client plist, and old credential
   files into timestamped rollback directories. Do not overwrite them.
3. Install the protocol-1 binary under its final path without restarting the
   old service.
4. Create `/var/lib/queqiao/provider`, add the user, and generate an invitation
   while the old process still owns the public port.
5. Install the new server unit and restart the gateway. Verify protocol 1,
   TCP and UDP listeners, and loopback metrics before touching the client.
6. Enroll with the new CLI. Its default `--local-address auto` bypasses a host
   TUN; specify `if:en0` when the machine has multiple physical interfaces.
7. Atomically install the new client binary and profile-based service arguments,
   then restart the client service.
8. Force a SOCKS request with `--noproxy ''`, confirm server flow counters and
   QUIC or TCP lanes, and only then delete the consumed invitation copy.

Generate the provider state beside the old credential files, not on top of
them. If the new service fails before enrollment, restore the old server unit
and binary. If it fails after the client changes, restore both client and
server as one rollback; mixed protocol versions will never connect. Keep the
new provider state and enrolled profile for diagnosis or a later retry unless
they are known to be compromised.

## User and device lifecycle

```sh
sudo -u queqiao queqiaod provider list-users \
  --state /var/lib/queqiao/provider
sudo -u queqiao queqiaod provider list-devices \
  --state /var/lib/queqiao/provider --user alice
sudo -u queqiao queqiaod provider revoke-device \
  --state /var/lib/queqiao/provider --device DEVICE_ID
sudo -u queqiao queqiaod provider disable-user \
  --state /var/lib/queqiao/provider --user alice
```

The gateway reloads atomic authorization updates. Revocation or user disable
blocks new streams and closes active TCP, QUIC, and UDP flows within about one
second. Re-enabling a user does not un-revoke an individual device.

Device certificates last 30 days. A running client checks hourly and renews in
the final seven days with the same private key and the same physical source
selection as its data connections. Gateway leaves are renewed and reloaded
hourly without interrupting established tunnels. Keep clocks synchronized.

## Backup and monitoring

Back up provider state after initialization and after account/device changes.
Restoring the exact directory preserves the root pin and all enrolled clients;
creating a new provider directory creates a different trust domain and requires
re-enrollment. Back up client profiles as secrets if issuing a replacement
invitation would be inconvenient.

Useful health checks are:

```sh
systemctl is-active queqiaod
curl -fsS http://127.0.0.1:19090/metrics
launchctl print "gui/$(id -u)/me.01.queqiao.client"
curl -fsS http://127.0.0.1:12090/metrics
```

Logs omit invitations, private keys, session identifiers, and payloads. Do not
publish provider state, profiles, old shared-secret files, or packet captures
without redaction.

## Troubleshooting

| Symptom | Action |
|---|---|
| `does not support Queqiao enrollment` or `rejected Queqiao protocol 1` | Confirm the invitation endpoint, client/server `wire=1`, and that no old TLS service still owns the port. |
| `more than one physical IPv4 address is active` | Choose the intended uplink with `--local-address if:NAME`; use the same value for enroll and client. |
| `interface … has no active IPv4 address` | Correct the interface name or connect it before retrying. The saved enrollment draft remains reusable. |
| Enrollment reports a pinned-identity error | The URI belongs to another provider, the provider state was replaced, or traffic is intercepted. Never bypass pin verification. |
| Invitation is expired or already used | Retry the matching `.enrolling` draft first. Otherwise revoke/audit the old invite and issue a new one. |
| Profile is rejected | Check that it is complete strict JSON; on Unix it must be mode `0600`, and on Windows it must have a private user DACL. Do not hand-edit identity fields. |
| SOCKS test shows the wrong egress and counters stay at zero | Force proxy use with `curl --noproxy '' --proxy socks5h://…`; inspect `NO_PROXY` and the selected Clash group. |
| Client repeatedly connects through itself | Bind enrollment, renewal, and data traffic to a physical source with `--local-address auto`, `if:NAME`, or an IP. |
| QUIC fails but TCP works | Permit UDP on the gateway port; `--transport auto` retains TCP fallback. |
| Authorization changes affect the wrong instance | Run provider commands against the exact same `--state` path used by the server unit. |
| Every enrolled client fails after state maintenance | Restore the original provider-state backup. Do not run `provider init` over or beside it and change the service path. |
