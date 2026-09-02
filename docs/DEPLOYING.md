# Deployment guide

> [!NOTE]
> **Status:** Current operational guide for Niulang protocol 3
> **Last reviewed:** 2026-09-02

This guide takes you from an empty host to a working paired deployment: a
provider gateway, one-time user enrollment, a desktop SOCKS client, and
Clash/mihomo integration. It also covers multi-provider clients, service
operation, upgrades, and rollback. Protocol 3 is the only supported wire
protocol, so client and server must be upgraded together.

**Download the binary first:** use the [latest
release](https://github.com/4fuu/niulang/releases/latest). Releases provide
archives for Linux, macOS, and Windows on amd64 and arm64. Check the archive
against the release's `SHA256SUMS` before running it. Build from source only for
development or for an uncovered platform.

Niulang assumes a paired client and gateway under the same operator's control.
Confirm that this model fits your network before exposing a service.

## Install and manage with the scripts

On Linux, [`deploy/manage.sh`](../deploy/manage.sh) is the interactive entry
point for installing, updating, and operating Niulang. It supports systemd,
OpenRC, and OpenWrt procd on amd64 and arm64. It downloads the selected stable
or prerelease build, verifies its `SHA256SUMS`, rejects binaries that do not
report protocol 3, and uses deployment files from the same release. It also
manages provider users, invitations, devices, multi-provider clients, and the
client's shared session limit.

```sh
./deploy/manage.sh
```

If a service definition from the former Queqiao deployment is present, the
manager asks at startup whether to migrate it. Migration stops the old role,
installs and verifies Niulang, and restores the old service's enabled/running
state if the new installation fails. It never converts or copies Queqiao
provider state, profiles, certificates, users, devices, or invitations:
protocol 3 creates a new trust domain, and every client must enroll with a new
`niulang://enroll/...` invitation. After verification, the manager can remove
the old service definition and binary. Deleting old keys, profiles,
configuration, and logs is a separate action requiring the exact confirmation
phrase printed by the tool.

The commands `legacy-stop`, `legacy-remove`, and `legacy-purge` separate the
reversible stop, service removal, and irreversible data deletion steps on all
three init systems. `legacy-purge` also removes the former OpenWrt UCI firewall
rule. On systemd, client installation and migration run as the login account
that will use the tunnel, while the provider is a system service. OpenRC and
OpenWrt use system services for both roles, so run the manager as root there.

The most useful non-interactive commands are:

```sh
./deploy/manage.sh server
./deploy/manage.sh client
./deploy/manage.sh add-provider
./deploy/manage.sh client-config
./deploy/manage.sh provider
./deploy/manage.sh update
./deploy/manage.sh source
./deploy/manage.sh status
```

`update` replaces the binary atomically, restarts every recognized Protocol 3
Niulang service, and restores the old binary if a service cannot run. It checks
provider state and client profiles before downloading or replacing anything;
an older protocol is redirected to the guided `server` or `client` flow rather
than being turned into a predictable failed restart. `source` stores the
official or custom GitHub `owner/repository` and whether `latest` may pick a
prerelease in `/etc/niulang/release-source`. A custom repository must publish
Niulang's normal archive names and `SHA256SUMS`; it is a source choice, not a
Queqiao or old-protocol compatibility path.

OpenWrt defaults to `/usr/bin/niulangd` and keeps durable profiles and provider
state under `/etc/niulang`; runtime logs go to procd/logread rather than flash.
The client binds its outer connection to the logical `wan` network by default,
resolves its current IPv4 address at service start, and has a procd interface
trigger that restarts it after address changes. Override the logical network
with `NIULANG_OPENWRT_NETWORK=wan2`. If WAN has no IPv4 yet, an absent client
instance is an expected delayed-start state rather than a successful data path.
The provider's prompted UDP port is opened with a named UCI
firewall rule. OpenRC defaults to `/usr/local/bin/niulangd`, a dedicated
`niulang` account, and `/var/lib/niulang`; listening below port 1024 requires
`setcap` from libcap.

The two lower-level installers remain available for systemd and desktop
automation. Read this section and skip to [Connect Clash or
mihomo](#connect-clash-or-mihomo); the rest of the guide remains the reference
for what they do and for hosts they do not cover.

On the Linux gateway, as root:

```sh
sudo ./deploy/install-server.sh \
  --name "Example Network" \
  --endpoint gateway.example.net:443 \
  --user alice \
  --tune
```

That installs the binary, service account, directories, hardened unit, and
environment file, initializes the provider, creates the first user, starts and
verifies the gateway, and finally prints one single-use invitation URI. Deliver
that URI over an authenticated private channel; it is a bearer credential.

On the client, as the account that will use the tunnel -- not with `sudo`:

```sh
./deploy/install-client.sh --invite 'niulang://enroll/...'
```

That enrolls the invitation, writes the provider manifest, installs a per-user
service that starts on its own, and confirms that a request actually leaves
through the gateway. Add a second provider later with the same command and only
the new invitation; existing providers and their loopback ports are kept.

If you already enrolled by hand, or you would rather see each step, the binary
installs its own service and needs no script:

```sh
niulangd enroll 'niulang://enroll/...'
niulangd service install --profile "$PROFILE"
```

Both scripts take `--dry-run`, use `--binary PATH` when you have a reviewed
release artifact instead of a source tree, and refuse any binary that does not
report `wire=3`.

The one thing `install-server.sh` will not do twice is create a provider trust
root. Re-running it against an initialized state directory stops before
touching the host; an upgrade passes `--no-provider-init` to keep that root and
replace only the binary, unit, and environment file.

## What is configured where

The provider chooses three durable values:

- a private provider-state directory, conventionally
  `/var/lib/niulang/provider`;
- a display name users will recognize; and
- one public `host:port` endpoint placed in every invitation and profile.

The endpoint may be an IP address or DNS name. Niulang authenticates the
provider root pinned by the invitation, not a WebPKI name, so no public CA or
Let's Encrypt certificate is required. The endpoint must remain reachable over
UDP.

Each user receives one temporary `niulang://` invitation. Importing it creates
one private profile containing the endpoint, provider pin, device certificate,
and locally generated device key. Users never copy provider keys, CA files,
shared secrets, or individual JSON fields.

## Install the gateway

`deploy/install-server.sh` performs this whole section. The steps below are
what it does, and the path to take on a host it does not cover: it requires
Linux with systemd and root.

Install the exact reviewed binary and confirm its protocol before creating
state:

```sh
sudo install -m 0755 ./niulangd /usr/local/bin/niulangd
/usr/local/bin/niulangd --version
```

The output must contain `wire=3`. Create a dedicated account once:

```sh
sudo useradd --system --user-group \
  --home-dir /var/lib/niulang --shell /usr/sbin/nologin niulang
sudo install -d -m 0700 -o niulang -g niulang /var/lib/niulang
sudo install -d -m 0750 -o niulang -g niulang /var/log/niulang
```

Initialize a new trust domain. The final state path must not already exist:

```sh
sudo -u niulang /usr/local/bin/niulangd provider init \
  --state /var/lib/niulang/provider \
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

Run the `provider` subcommands as the account that owns that directory, as every
example below does. Running one under plain `sudo` is no longer destructive -
state files keep the owner they already had rather than following the caller -
but the account that owns the state is still the account the gateway reads it
as, and keeping the two aligned is what makes a misconfiguration obvious.

### systemd

Install [`deploy/niulangd.service`](../deploy/niulangd.service), then create
`/etc/niulang/niulangd.env` owned by `root:niulang` with mode `0640`:

```text
NIULANGD_ARGS=--state /var/lib/niulang/provider --listen :443 --max-sessions 4096 --metrics-listen 127.0.0.1:19090 --log-level info --log-format json --log-file /var/log/niulang/server.log --telemetry-log-interval 5s
```

The environment file is a whitespace-separated argument list; do not put a
state path containing spaces in it.

The unit grants `CAP_NET_BIND_SERVICE` and bounds the capability set to it.
The service account is unprivileged and `NoNewPrivileges=true` blocks acquiring
a capability after exec, so without that grant a `--listen :443` cannot bind at
all. Start and verify the service:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now niulangd
systemctl is-active niulangd
sudo ss -lnup | grep ':443'
curl -fsS http://127.0.0.1:19090/metrics | head
sudo test -s /var/log/niulang/server.log
sudo tail -n 5 /var/log/niulang/server.log
```

One UDP listener row is expected. Permit that UDP port in the host firewall and
the cloud security group. The listener is a mutually authenticated HTTP/3
endpoint using the `h3` ALPN and Niulang Extended CONNECT requests. Forward it
at layer 4; a generic
HTTP reverse proxy that terminates TLS cannot preserve Niulang's provider pin,
device certificate authorization, or tunnel handler.
Binding metrics to loopback avoids exposing an unauthenticated operations
endpoint. The server runtime log is independent of `/metrics`: it contains the
same performance counters as timestamped JSON records and rotates internally
at 32 MiB with five backups. See [`LOGGING.md`](LOGGING.md).

### Tune provider socket queues

Linux's default socket limits are often too small for a QUIC gateway. A burst
of new flows can then overflow the UDP receive queue even when the host has idle
CPU and memory. Niulang's QUIC dependency requests an 8 MiB UDP buffer; the
provider should leave additional kernel headroom and a larger network backlog.

Run the repository's idempotent tuning script on every Linux provider:

```sh
sudo ./deploy/tune-server.sh
```

The script installs `/etc/sysctl.d/90-niulang-performance.conf`, immediately
applies 16 MiB UDP socket maxima and a larger network backlog, verifies the
effective values, and restarts `niulangd.service` if it is active so the QUIC
listener obtains its larger buffer. Use `--no-restart` when coordinating a
separate maintenance window, or `--service NAME` for a differently named
systemd unit. `--dry-run` prints the settings without changing the host.

Afterward, confirm that the listener has room and that its drop counters do not
keep increasing under normal traffic:

```sh
sudo ss -unapm | grep niulang
nstat -az | grep 'UdpRcvbufErrors'
```

Private, loopback, link-local, multicast, and unspecified destinations are
blocked after DNS resolution. Add `--allow-private-destinations` only when the
service is intentionally an access proxy into a private network.

## Add users and issue invitations

Create a separate account for every customer or administrative boundary:

```sh
sudo -u niulang /usr/local/bin/niulangd provider add-user \
  --state /var/lib/niulang/provider \
  --name alice \
  --max-clients 8
```

An account carries two limits, and they count different things:

- `--max-clients` is how many of the account's devices may be carrying traffic
  at once. A device counts once however much it carries, so this is the limit
  that expresses "this account is for eight devices". It defaults to 8, and
  zero admits every enrolled device.
- `--max-flows` is how many proxied flows the account may hold at once. One
  flow is one TCP connection or one UDP association — not one device, and not
  one page. It defaults to 1024, and zero defers to the gateway-wide
  `--max-sessions` ceiling.

Set the device limit and leave the flow limit alone unless you are deliberately
capping one account's footprint on the gateway. A flow ceiling low enough to be
interesting as a quota is low enough to break ordinary browsing: one page opens
roughly six connections per host across dozens of hosts, and with the default
`--flow-idle-timeout` those keep-alive connections keep holding their slots for
half an hour after the page finished loading. The failure that produces is hard
to recognize — most sites load, a few do not, and the only symptom is the
client reporting `peer reset flow: account flow limit reached`.

`--max-sessions` is the former name of `--max-flows` and still works. It is
deprecated because it reads like a device count and has never been one.

Both limits can be corrected in place, without deleting the account and every
device enrolled against it:

```sh
sudo -u niulang /usr/local/bin/niulangd provider set-user-limits \
  --state /var/lib/niulang/provider \
  --user alice \
  --max-flows 0
```

A limit you do not name keeps its current value. The gateway re-reads
authorization state every second, so a corrected limit admits new flows within
a second, without a restart and without disturbing open connections.

Refusals are counted at `niulang_account_admission_refused_total` by reason —
`flow_limit`, `client_limit`, `unauthorized` — and logged as `msg="account flow
open refused"` naming the account and device. Check those first when a user
reports that some sites work and others do not.

Create a one-time invitation and deliver the single printed URI through an
authenticated private channel:

```sh
sudo -u niulang /usr/local/bin/niulangd provider invite \
  --state /var/lib/niulang/provider \
  --user alice \
  --expires-in 1h
```

The URI is a temporary bearer credential. The provider stores only its token
digest, and the lifetime cannot exceed seven days. A portal may capture the
stdout value or render it as a QR code without translating any fields.

Audit or revoke unused invitations without printing their tokens:

```sh
sudo -u niulang /usr/local/bin/niulangd provider list-invites \
  --state /var/lib/niulang/provider --user alice
sudo -u niulang /usr/local/bin/niulangd provider revoke-invite \
  --state /var/lib/niulang/provider --invite INVITE_ID
```

## Set up a desktop client

There are two ways in, and they end in the same place. Either one leaves a
supervised client that starts on its own; do not run `niulangd client` in a
terminal as a deployment, because the process exits when any provider's
listener stops and nothing brings it back.

**One command.** `deploy/install-client.sh` enrolls, writes the manifest,
installs the service, and verifies that traffic reaches the gateway:

```sh
./deploy/install-client.sh --invite 'niulang://enroll/…'
```

**Two commands.** If you already enrolled by hand, or you want to see each step,
`niulangd` installs its own service:

```sh
niulangd enroll 'niulang://enroll/…'
niulangd service install --profile "$PROFILE"
```

`enroll` prints the exact `service install` line to run next, with the profile
path filled in. Both routes are the same on macOS and Linux.

### Where the files go

| What | macOS | Linux |
| --- | --- | --- |
| Profile | `~/Library/Application Support/niulang/` | `~/.config/niulang/` |
| Service | `~/Library/LaunchAgents/me.01.niulang.client.plist` | `~/.config/systemd/user/niulang-client.service` |
| Log | `~/Library/Logs/Niulang/client.log` | `~/.local/state/niulang/client.log` |

The profile directory is the platform configuration directory, which is what
`niulangd enroll` uses when `--profile` is not given. Run `niulangd logs client`
to print the log path and a follow command rather than assuming it.

Changing the layout later is a re-run, not a manual move. Passing a different
`--config-dir`, `--prefix`, `--label`, or `--service-name` to
`deploy/install-client.sh` relocates the install: the service is stopped, the
enrolled profiles move with it, and the superseded definition and binary are
removed. Profiles are moved rather than re-enrolled because an invitation is
single-use — the device key behind a consumed invitation cannot be reissued, so
a profile left behind is a device lost.

### Enrolling

```sh
niulangd enroll 'niulang://enroll/…'
```

The default profile path is printed on success. To choose the path and a
recognizable device label explicitly:

```sh
niulangd enroll 'niulang://enroll/…' \
  --profile ~/niulang/example-network.json \
  --device-name alice-laptop \
  --local-address if:en0
```

`--local-address` defaults to `auto` for enrollment, normal client traffic,
and certificate renewal. Automatic selection excludes loopback and
point-to-point TUN interfaces. If two physical IPv4 interfaces are active,
Niulang reports both instead of guessing; use `if:NAME` or a literal local IP.
This is especially important when Clash TUN owns the default route: bootstrap
and renewal must not depend on the tunnel they are configuring.

The device key is generated locally before the one-time token is sent. An
interrupted import leaves `PROFILE.enrolling`, mode `0600`. Retry the same URI,
profile path, and device name to reuse that key safely. Do not delete the draft
merely because the first response was lost; requesting a different key after
token consumption is correctly rejected as replay.

The profile must remain readable only by its owner. Niulang rejects a
group/world-readable profile rather than silently using an exposed key.

### The service

```sh
niulangd service install --profile "$PROFILE"   # or --providers MANIFEST
niulangd service status
niulangd service print --profile "$PROFILE"     # render it without installing
niulangd service uninstall                      # leaves profiles alone
```

`install` writes the definition, loads it, and leaves it starting on its own
from then on. The SOCKS5 listener defaults to `127.0.0.1:12080`, the port
[`deploy/clash-niulang.yaml`](../deploy/clash-niulang.yaml) already points at.
Use `--listen` to change it, `--label` or `--service-name` to run more than one
client, and `--binary` when the definition should name a path other than the
running executable.

There is no plist or unit to copy and edit. The definition is generated from
this machine's real paths, which is what a template with `/Users/YOU`
placeholders could never be: the placeholder path was not even the directory
`enroll` writes to on macOS.

On macOS this is a LaunchAgent, so it starts **at login**, not at boot. That is
the right scope for it — the profile is a `0600` private key owned by one
account and the listener serves that account's applications — but it does mean
the tunnel is not up before someone logs in. On Linux the unit is a systemd
`--user` unit and `install` also runs `loginctl enable-linger`, which does give
start-at-boot. Check either with:

```sh
niulangd service status
```

After editing a loaded plist by hand, use `launchctl bootout` then
`launchctl bootstrap`; `kickstart` restarts the definition launchd already
cached and does not re-read arguments from disk. `niulangd service install`
does the bootout/bootstrap pair for you.

## Connect Clash or mihomo

Niulang is a separate local SOCKS5 service, not a protocol parsed by Clash.
Add a loopback SOCKS5 node with UDP enabled and select it in the group used by
your rules. [`deploy/clash-niulang.yaml`](../deploy/clash-niulang.yaml) is a
complete starter profile; for an existing Clash profile, copy only its
`niulang` proxy entry and add that name to the existing selector.

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

## Connect to multiple providers

One client process can maintain independent connections to several providers
and expose one loopback SOCKS5 listener for each. Obtain a separate invitation
from every provider and enroll each one into a clearly named profile:

```sh
# Use the Hong Kong provider's invitation in the first command.
niulangd enroll 'niulang://enroll/…' \
  --profile ~/niulang/hk.json \
  --device-name alice-laptop

# Use the US West provider's invitation in the second command.
niulangd enroll 'niulang://enroll/…' \
  --profile ~/niulang/us.json \
  --device-name alice-laptop
```

`deploy/install-client.sh` writes this manifest for you. To write it by hand,
save it beside the profiles — the profile directory is
`~/Library/Application Support/niulang` on macOS and `~/.config/niulang` on
Linux:

```json
{
  "version": 1,
  "providers": [
    {"name": "hong-kong", "profile": "hk.json", "listen": "127.0.0.1:1081"},
    {"name": "us-west", "profile": "us.json", "listen": "127.0.0.1:1082"}
  ]
}
```

Start all configured providers together:

```sh
niulangd client \
  --providers ~/niulang/providers.json \
  --metrics-listen 127.0.0.1:12090
```

Manifest version 1 requires a nonempty, unique name, profile, and listener for
every provider. Relative profile paths are resolved from the manifest
directory. Each listener must use a literal loopback IP and a port between 1
and 65535; `--listen` cannot be combined with `--providers`.

Every entry must name a separately enrolled device. Two entries which resolve
to one device — a copied profile, a symlink, a hard link — are rejected at
startup: two clients on one certificate would leave two renewal loops racing to
save a single identity into two files.

Process-wide budgets stay process-wide rather than being granted once per
provider:

- `--max-sessions` is the combined admission limit. Half of it is reserved in
  equal shares, one share per provider, and half stays common. The reservation
  is what keeps a standby provider able to accept a connection while a busy one
  holds most of the common pool — without it a saturated primary starves the
  failover target that is supposed to replace it.
- `--aggregate-bytes-per-sec` and `--interactive-reserve-bytes-per-sec` pace
  the whole process. Providers share one budget, so the configured rate is the
  rate the uplink sees no matter how many providers are configured.
- `--metrics-listen` aggregates every provider's activity into one endpoint.
  Per-provider counters are not exported today; use the runtime log, which tags
  each record with the manifest name and listener, to attribute activity.

Other client runtime flags apply to each provider independently.

All listeners bind before the first gateway is dialled, so a provider whose
gateway is unreachable at startup cannot hold a healthy provider's SOCKS port
down. Certificate renewal then runs for every provider concurrently.

The process exits if any provider's listener stops, so a partially working
client never looks healthy to a service manager. Run it under a supervisor that
restarts it — a bare foreground `niulangd client --providers` will not come
back on its own. `niulangd service install --providers MANIFEST` sets that up.

To let Clash/mihomo choose between these endpoints, define one SOCKS5 proxy for
each listener and put them in a health-checked group. This `fallback` example
prefers Hong Kong while it is healthy and sends new connections to US West
when its health check fails:

```yaml
proxies:
  - name: niulang-hong-kong
    type: socks5
    server: 127.0.0.1
    port: 1081
    udp: true
  - name: niulang-us-west
    type: socks5
    server: 127.0.0.1
    port: 1082
    udp: true

proxy-groups:
  - name: Niulang
    type: fallback
    url: https://www.gstatic.com/generate_204
    interval: 300
    proxies:
      - niulang-hong-kong
      - niulang-us-west
```

Point the relevant Clash/mihomo rules at the `Niulang` group. Niulang itself
does not select providers. Clash/mihomo can switch only new connections;
existing connections are not migrated and fail if their provider becomes
unavailable. Use a `url-test` group instead when lowest measured latency is
more important than provider order.

## Upgrade an existing deployment

Protocol 3 deliberately has no state migration, shared-secret compatibility,
or wire downgrade. Provider state, profiles, and certificates created by a
different protocol version are rejected. Replacing a previous deployment on
the same endpoint therefore requires a new trust domain, device re-enrollment,
and a brief coordinated restart; existing flows cannot survive the boundary.

Use this order:

1. Record the old client/server versions, arguments, listener, and a known-good
   proxy request.
2. Copy the old binaries, service definitions, client configuration, provider
   state, and profiles into timestamped rollback directories. Do not overwrite
   them.
3. Create a new, empty protocol-3 provider-state directory and add the user
   while the old process still owns the public port. Never initialize over the
   previous state directory.
4. Install the protocol-3 binary and server unit, then restart the gateway.
   Verify `wire=3`, the UDP listener, and loopback metrics before issuing the
   fresh invitation or touching the client.
5. Enroll with the new CLI. Its default `--local-address auto` bypasses a host
   TUN; specify `if:en0` when the machine has multiple physical interfaces.
6. Atomically install the new client binary and profile-based service arguments,
   then restart the client service.
7. Force a SOCKS request with `--noproxy ''`, confirm server flow counters and
   QUIC lanes, and only then delete the consumed invitation copy.

`deploy/manage.sh` implements those safety boundaries interactively. On the
gateway, run `sudo ./deploy/manage.sh server`. If it detects old state, it
defaults to a sibling `*-p3` state directory, reuses the old display name,
endpoint, listener, limits, and metrics settings as prompt defaults, and leaves
the old state untouched. It prints the new one-time invitation only after the
Protocol 3 UDP service passes its startup checks. On each client, run
`./deploy/manage.sh client` as the login user (root on OpenRC/OpenWrt), paste
that invitation, and accept the default `niulang-p3` configuration/profile
path. The manager preserves the old listener and runtime settings. A failed
guided cutover restores the old binary and service definition; the new provider
state or profile remains available for diagnosis and a same-key retry. Custom
systemd binary, environment, state, log, and service-user paths are read from
the installed unit and retained.

Once both sides already use Protocol 3, subsequent releases are ordinary
in-place updates through `deploy/manage.sh update`; no re-enrollment is needed.
Do not use `update` for the Protocol 2→3 boundary—the manager intentionally
refuses it before touching the installed binary.

If the new service fails before enrollment, restore the old server unit,
binary, and provider state. If it fails after the client changes, restore the
old client binary and profile as well; a rollback is one complete trust domain,
not a mixture of old and new state. Keep the new provider state and enrolled
profile for diagnosis or a later retry unless they are known to be compromised.

## User and device lifecycle

```sh
sudo -u niulang niulangd provider list-users \
  --state /var/lib/niulang/provider
sudo -u niulang niulangd provider list-devices \
  --state /var/lib/niulang/provider --user alice
sudo -u niulang niulangd provider revoke-device \
  --state /var/lib/niulang/provider --device DEVICE_ID
sudo -u niulang niulangd provider disable-user \
  --state /var/lib/niulang/provider --user alice
```

The gateway reloads atomic authorization updates. Revocation or user disable
blocks new streams and closes active application TCP streams, QUIC connections,
and UDP associations within about one second. Re-enabling a user does not
un-revoke an individual device.

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
systemctl is-active niulangd
curl -fsS http://127.0.0.1:19090/metrics
launchctl print "gui/$(id -u)/me.01.niulang.client"
curl -fsS http://127.0.0.1:12090/metrics
```

The one gateway fault that does not show up in the flow counters is an
authorization store the gateway can no longer re-read. The snapshot already in
force stays in force, so established devices keep connecting and every rate and
error metric stays healthy, while every new enrollment fails. Alert on it
directly:

```
niulang_authorization_consecutive_refresh_failures > 30
time() - niulang_authorization_last_good_timestamp_seconds > 300
```

The first fires while the store is unreadable; the second catches a snapshot
that has gone stale for any reason. The runtime log carries the same run at
error level, restated once a minute with how long it has been failing, and a
`authorization refresh recovered` record when it ends.

Logs omit invitations, private keys, session identifiers, and payloads. Do not
publish provider state, profiles, old shared-secret files, or packet captures
without redaction.

## Troubleshooting

| Symptom | Action |
|---|---|
| `does not support Niulang enrollment` or `rejected Niulang protocol 3` | Confirm the invitation endpoint, client/server `wire=3`, and that no other QUIC service still owns the UDP port. |
| `more than one physical IPv4 address is active` | Choose the intended uplink with `--local-address if:NAME`; use the same value for enroll and client. |
| `interface … has no active IPv4 address` | Correct the interface name or connect it before retrying. The saved enrollment draft remains reusable. |
| Enrollment reports a pinned-identity error | The URI belongs to another provider, the provider state was replaced, or traffic is intercepted. Never bypass pin verification. |
| Invitation is expired or already used | Retry the matching `.enrolling` draft first. Otherwise revoke/audit the old invite and issue a new one. |
| Profile is rejected | Check that it is complete strict JSON; on Unix it must be mode `0600`, and on Windows it must have a private user DACL. Do not hand-edit identity fields. |
| SOCKS test shows the wrong egress and counters stay at zero | Force proxy use with `curl --noproxy '' --proxy socks5h://…`; inspect `NO_PROXY` and the selected Clash group. |
| Client repeatedly connects through itself | Bind enrollment, renewal, and data traffic to a physical source with `--local-address auto`, `if:NAME`, or an IP. |
| QUIC connection fails | Permit UDP on the gateway port and verify that any load balancer forwards QUIC without terminating it. |
| Authorization changes affect the wrong instance | Run provider commands against the exact same `--state` path used by the server unit. |
| Every enrolled client fails after state maintenance | Restore the original provider-state backup. Do not run `provider init` over or beside it and change the service path. |
