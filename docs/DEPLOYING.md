# Deploying queqiao, and using it from Clash

This is the practical guide: how to build it, how to run the two ends, and how
to point Clash Verge or any mihomo-based client at it.

Read [the limits](#what-you-are-signing-up-for) before deploying this anywhere
you care about. The repository-actionable release gates in
[`PRODUCTION-DESIGN.md`](PRODUCTION-DESIGN.md) now have implementation and
evidence. The tunnel-stall defects have deterministic regressions; real UDP
blackholes recovered over TCP; restored UDP was re-preferred; packaging,
resource bounds, vulnerability scanning, and rollback are reproducible; and a
mixed production soak passed. Broader path/middlebox diversity and independent
review remain external qualifications.

The deployment trust boundaries and resource ceilings are enumerated in
[`SECURITY-REVIEW.md`](SECURITY-REVIEW.md). In particular, the local SOCKS and
metrics listeners are unauthenticated and should remain on loopback.

## The shape of a deployment

**queqiao is not a protocol Clash speaks.** There is no `type: queqiao` in
mihomo and there will not be one. A deployment is two processes and a plain
SOCKS5 hand-off:

```
application → Clash (TUN or system proxy)
                → SOCKS5 127.0.0.1:12080
                    → queqiaod --mode local      ← runs beside Clash, not inside it
                        → QUIC/UDP or TLS/TCP over the long-haul path
                            → queqiaod --mode server   ← on the egress host
                                → the destination
```

Clash keeps doing what it is good at — capture, DNS, and rule-based routing —
and forwards the flows it decides to proxy into queqiao's local ingress. That
is the integration the design intends, and it is the one that can be rolled
back by deleting one node and one rule.

## Build and install

Go 1.25.13 or later. Earlier Go 1.25 patch releases contain standard-library
vulnerabilities reachable through the TLS, X.509, HTTP metrics, and network
paths used by queqiao; the module directive enforces the patched toolchain.

For a tagged build, prefer the release archive for the target host and verify
its `SHA256SUMS`. [`RELEASING.md`](RELEASING.md) documents provenance, atomic
installation, and rollback. To build the entire six-target release matrix from
a checkout:

```sh
go run ./cmd/queqiaopack --version v0.1.0 \
  --commit "$(git rev-parse HEAD)" \
  --build-date "$(git show -s --format=%cI HEAD)" --output dist
```

For development builds:

```sh
git clone https://github.com/bojieli/queqiao && cd queqiao
go build ./... && go test ./...
go install ./cmd/...          # queqiaod, queqiaoref, queqiaobench, pathprobe
```

For the egress host, cross-compile rather than installing a toolchain there:

```sh
GOOS=linux GOARCH=amd64 go build -o queqiaod-linux-amd64 ./cmd/queqiaod
scp queqiaod-linux-amd64 egress:/usr/local/bin/queqiaod
```

## The egress side

Both ends authenticate with one high-entropy shared secret, and the client
verifies the server certificate against a pinned private root. Generate a
bounded-lifetime leaf and printable secret outside the repository:

```sh
./scripts/generate_credentials.sh --output /secure/queqiao-credentials \
  --server-name queqiao.node
sudo install -d -m 0750 -o root -g queqiao /etc/queqiao
sudo install -m 0644 /secure/queqiao-credentials/server.crt /etc/queqiao/cert.pem
sudo install -m 0640 -o root -g queqiao \
  /secure/queqiao-credentials/server.key /etc/queqiao/key.pem
sudo install -m 0640 -o root -g queqiao \
  /secure/queqiao-credentials/secret /etc/queqiao/secret
```

Keep `root-ca.key` offline. The egress does not need it. Copy only
`root-ca.crt` and `secret` to the client. The coordinated procedure and rollback
rules are in [`CREDENTIAL-ROTATION.md`](CREDENTIAL-ROTATION.md).

`--server-name` on the client must equal the certificate's name. It is a
verified TLS name, not a DNS lookup: the client dials the literal IP in
`--remote` and presents this as SNI.

Run it under systemd. [`deploy/queqiaod.service`](../deploy/queqiaod.service)
is the packaged hardened template; the minimal form is:

```ini
[Service]
ExecStart=/usr/local/bin/queqiaod --mode server --listen 0.0.0.0:12540 \
  --tls-cert /etc/queqiao/cert.pem --tls-key /etc/queqiao/key.pem \
  --secret-file /etc/queqiao/secret --log-level info
Restart=always
LimitNOFILE=1048576
```

```sh
sudo systemctl daemon-reload && sudo systemctl enable --now queqiaod
systemctl is-active queqiaod
```

One port carries both transports: queqiaod listens for QUIC on **UDP** and for
the authenticated TLS/TCP fallback on **TCP**, both on `--listen`. Open both in
any firewall, or UDP-only paths will silently take the slower fallback.

Do **not** pass `--allow-private-destinations` on a general-purpose egress. It
exists for benchmark rigs whose origin is on loopback, and it lets a client
reach the egress host's own private network.

## The client side

Copy `root-ca.crt` and `secret` from the trusted generation host to the client —
the secret is a credential, so treat it accordingly (`chmod 600`).

```sh
mkdir -p ~/.queqiao/bin && chmod 700 ~/.queqiao
cp $(command -v queqiaod) ~/.queqiao/bin/
install -m 0644 /secure/queqiao-credentials/root-ca.crt ~/.queqiao/cert.pem
install -m 0600 /secure/queqiao-credentials/secret ~/.queqiao/secret
```

```sh
queqiaod --mode local \
  --listen 127.0.0.1:12080 \
  --remote <EGRESS-IP>:12540 \
  --server-name queqiao.node \
  --root-ca ~/.queqiao/cert.pem \
  --secret-file ~/.queqiao/secret \
  --local-address if:en0 \
  --metrics-listen 127.0.0.1:12090
```

### `--local-address` is not optional when Clash is in TUN mode

This is the single most important flag on the page, and getting it wrong
produces a tunnel that appears to work.

Clash's TUN mode installs a default route through `198.18.0.1`. queqiao's own
outer connection to the egress host follows the host routing table like any
other socket, so it goes **into Clash**, which forwards it through whatever
proxy Clash is currently using. The result is queqiao tunnelled inside the
tunnel it was meant to replace — or a loop, if Clash's rule for that traffic
points back at queqiao.

`--local-address` binds the outer socket to a physical address so the packets
leave the real interface:

| Value | Use when |
| --- | --- |
| `if:en0` | recommended; resolves that interface's IPv4 before each dial, so DHCP changes are handled |
| `192.0.2.10` | a literal address, when the interface name is unstable |
| `auto` | one active physical interface only; errors if the choice is ambiguous |

**Verify it rather than assuming it.** On the egress host, watch what source
address actually arrives:

```sh
sudo tcpdump -nni any "port 12540 and inbound" -c 5
```

It must show your real uplink address. If it shows your existing proxy's exit
address, the binding is not taking effect and every byte is going through the
old tunnel.

Use the literal egress **IP** in `--remote`, never a hostname: with fake-IP DNS
a hostname resolves to a `198.18.x.x` address inside the tunnel.

### Running it in the background

macOS — [`deploy/me.01.queqiao.client.plist`](../deploy/me.01.queqiao.client.plist),
edited for your paths and egress IP:

```sh
cp deploy/me.01.queqiao.client.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/me.01.queqiao.client.plist
launchctl list | grep queqiao
```

Linux — the same unit as the server with `--mode local`, as a user service:

```sh
systemctl --user enable --now queqiao-client
```

Check it before touching Clash:

```sh
nc -z 127.0.0.1 12080 && echo "ingress up"
env -u NO_PROXY -u no_proxy curl --noproxy '' -sS \
    --socks5-hostname 127.0.0.1:12080 https://api.ipify.org
```

That must print the egress host's address. (`env -u NO_PROXY` matters: curl
honours `NO_PROXY` even when a proxy is named explicitly, so a shell with
`NO_PROXY=*` will report success while never touching the proxy.)

## Enabling it in Clash Verge

The recommended integration is to add queqiao to the profile you already use.
Do not create a second profile merely to add one node: it duplicates rules and
DNS/TUN settings, makes switching more confusing, and is unnecessary. Create a
local profile from [`deploy/clash-queqiao.yaml`](../deploy/clash-queqiao.yaml)
only when Clash Verge has no usable profile at all.

Queqiao is a **node inside a profile**. After adding it, look for `queqiao` on
Clash Verge's **Proxies** page, inside the selector you edited. It will not
appear as a new card on the **Profiles** page.

Before editing Clash, the standalone check in [the client section](#the-client-side)
must pass:

```sh
nc -z 127.0.0.1 12080 && echo "ingress up"
env -u NO_PROXY -u no_proxy curl --noproxy '' -sS \
    --socks5-hostname 127.0.0.1:12080 https://api.ipify.org
```

If that fails, fix or start `queqiaod` first. A Clash profile cannot make a
dead local SOCKS listener work.

### Recommended: edit the existing profile

1. In **Profiles**, identify the profile that is currently in use. Use Clash
   Verge's backup feature, or copy the YAML text to a safe file, before editing.
2. Right-click that profile and choose **Edit**, **Edit File**, or the
   equivalent YAML editor in your Clash Verge Rev version. Use the editor
   opened by Clash Verge so that Save performs validation and tells the
   background core to reload.
3. Add this item to the existing top-level `proxies:` list:

```yaml
  - name: queqiao
    type: socks5
    server: 127.0.0.1
    port: 12080
    udp: true          # queqiao implements SOCKS5 UDP ASSOCIATE
```

If the profile has no top-level `proxies:` key because it uses only
`proxy-providers`, create `proxies:` at the top level and put this item under
it. Mihomo permits local proxies and providers in the same profile. If a node
named `queqiao` already exists, update that item instead of creating a duplicate
name.

The `- name: queqiao` line must have exactly the same indentation as the other
proxy `- name:` lines. In particular, do not put it under the previous node's
`ws-opts:`, `reality-opts:`, or `tls:` block.

`udp: true` is worth setting. queqiao carries SOCKS UDP on the connection's
QUIC datagrams where QUIC negotiated them, so an application that chose UDP
keeps datagram semantics instead of having its packets serialised into a stream
where one loss stalls everything behind it.

4. Find the selector that your rules actually use. For example, the final rule
   below sends unmatched traffic to the group named `Auto`:

   ```yaml
   rules:
     - MATCH,Auto
   ```

   Add `queqiao` to that group's `proxies:` list. Prefer a group with
   `type: select`, so selecting queqiao is an explicit operation:

   ```yaml
   proxy-groups:
     - name: Auto
       type: select
       proxies:
         - existing-node-a
         - existing-node-b
         - queqiao
   ```

   Adding the node only under top-level `proxies:` is not enough: it may be
   valid but invisible in the selector. Creating a new group is also not
   enough unless a rule routes traffic to that group.

   A provider-backed group may have `use:` but no `proxies:` list. In that
   case, keep `use:` and add a sibling `proxies:` list containing `queqiao`;
   the two sources can coexist in one group.

5. Save in Clash Verge. Keep the current node selected until the new entry has
   passed its delay check. Then open **Proxies**, find the group edited above,
   and select `queqiao`.

A minimal complete before/after shape is:

```yaml
proxies:
  - name: existing-node
    # existing node fields remain unchanged
  - name: queqiao
    type: socks5
    server: 127.0.0.1
    port: 12080
    udp: true

proxy-groups:
  - name: Auto
    type: select
    proxies:
      - existing-node
      - queqiao

rules:
  - DOMAIN-SUFFIX,cn,DIRECT
  - GEOIP,CN,DIRECT
  - MATCH,Auto
```

Keep the rest of the existing profile—including its DNS, TUN, rules, proxy
providers, and other groups—unchanged.

### Remote subscriptions and profile updates

Editing the downloaded YAML of a remote subscription may be undone the next
time that subscription updates. If the profile is provider-managed and updates
automatically, put the same change in that profile's **Extension Script**
instead. Replace `Auto` below with the exact, case-sensitive name of the
`type: select` group you normally use:

```javascript
const QUEQIAO_GROUP = "Auto";

function main(config) {
  if (!Array.isArray(config.proxies)) config.proxies = [];

  const exists = config.proxies.some((proxy) => proxy.name === "queqiao");
  if (!exists) {
    config.proxies.push({
      name: "queqiao",
      type: "socks5",
      server: "127.0.0.1",
      port: 12080,
      udp: true,
    });
  }

  const groups = config["proxy-groups"] || [];
  const group = groups.find((item) => item.name === QUEQIAO_GROUP);
  if (!group) {
    throw new Error("queqiao: proxy group not found: " + QUEQIAO_GROUP);
  }
  if (group.type !== "select") {
    throw new Error("queqiao: target group must have type select");
  }
  if (!Array.isArray(group.proxies)) group.proxies = [];
  if (!group.proxies.includes("queqiao")) group.proxies.push("queqiao");

  return config;
}
```

Use a **profile-specific** extension script, not a global one, unless every
profile on the machine should expose the local Queqiao node. Clash Verge Rev's
official [custom-script documentation](https://www.clashverge.dev/guide/script.html)
describes how the `main(config)` hook modifies the profile before Mihomo loads
it. If the profile already has an extension script, merge the Queqiao logic
into its existing `main` function; do not replace the script or add a second
`main` function. If Save reports `proxy group not found`, correct
`QUEQIAO_GROUP`; the script deliberately fails validation rather than silently
adding an unreachable node.

### If no profile exists

Only for an empty Clash Verge installation:

1. Open **Profiles → New → Local** (wording varies slightly by version).
2. Select [`deploy/clash-queqiao.yaml`](../deploy/clash-queqiao.yaml), or paste
   its contents into the new profile, and save it through the UI. The official
   [local-profile guide](https://www.clashverge.dev/guide/profile.html)
   notes that Verge copies a selected file into its managed profile directory.
3. Activate the new profile, open **Proxies → Proxy**, run the delay check, and
   select `queqiao`.

Do not copy a YAML file directly into Clash Verge's `profiles/` directory and
expect it to appear in the UI. The application tracks profile metadata
separately; an unregistered file is deliberately invisible.

### Files not to edit

- Do not hand-edit `profiles.yaml`. It is Clash Verge's internal registry and
  the running application can overwrite manual entries when it exits.
- Do not hand-edit `clash-verge.yaml`. It is generated runtime output and will
  be replaced the next time Clash Verge builds the active configuration.
- Do not treat closing the main window as a core restart. On systems where
  Clash Verge keeps Mihomo in a background service, use the application's
  **Restart Core** action or save/reactivate the profile through the UI.

If an external editor is unavoidable, edit the actual source profile—not the
generated file—then explicitly reactivate the profile or restart the core.
Using Clash Verge's editor is less error-prone because it validates before
applying the change.

### Verify from Clash

First confirm that `queqiao` appears in the intended group on **Proxies** and
that its delay test succeeds. Select it, note the counters, generate traffic,
and read the counters again:

```sh
curl -fsS 127.0.0.1:12090/metrics \
  | grep -E 'flows_(started|completed)_total|bytes_(up|down)_total'
```

`flows_started_total` and the byte counters must increase while browsing. If
they do not, Clash is not routing to the node: confirm that queqiao is selected
in the group named by the matching rule. Finally, verify the observed egress
address is the Queqiao server's address, not the old proxy's address.

TCP fallback preserves connectivity, but it is the degraded path on the
high-latency, high-loss link queqiao targets. Monitor transport health
separately from aggregate flow success:

```sh
curl -fsS 127.0.0.1:12090/metrics \
  | grep -E 'fallbacks_total|udp_path_unavailable_total|endpoint_transport_races_failed_total'
```

`queqiao_udp_path_unavailable_total` increases when QUIC fails or does not
authenticate before TCP reaches the same configured endpoint. The client also
logs `UDP path unavailable or too slow; TCP fallback is degraded` with the
endpoint and QUIC outcome. `queqiao_endpoint_transport_races_failed_total`
instead increases when an `auto` race exhausts both QUIC/UDP and TLS/TCP; its
warning includes both transport errors. These are attempt counters: one SOCKS
request may contribute more than once because a lost flow open is retried up to
the configured bound.

### Other mihomo clients

Nothing above is Verge-specific. Any client that reads a mihomo configuration —
mihomo itself, Clash for Windows forks, ClashX Meta, OpenClash — takes the same
`socks5` node, and the only platform-specific part is how you keep
`queqiaod --mode local` running.

## What you are signing up for

Measured on a real China-US path on 2026-08-16
([`MEASUREMENTS-20260816.md`](MEASUREMENTS-20260816.md)), against sing-box
serving the alternatives:

- **It is fast.** 143 Mbit/s median where Hysteria2 got 90 and TUIC v5 got 77,
  ahead in all six rounds, and roughly 100x the two TCP-based VLESS
  configurations. Since the `min_rtt` fix in
  [`STALL-20260817.md`](STALL-20260817.md) the same windows measure about
  200 Mbit/s.
- **Cancelled downloads no longer retain flows.** The former failure left the
  client and server sending into a closed application until several abandoned
  transfers saturated the link. Local EOF is now signalled across the flow,
  stalled response delivery escalates to a bounded full-close, and the peer
  cancels its sender immediately. The original diagnosis and deterministic
  regression coverage are in
  [`STALL-20260817.md`](STALL-20260817.md).

Interactive latency still moves under queqiao's own bulk load, although the
corrected live A/B is in the range of the measured QUIC alternatives. Clash's
TUN-to-SOCKS hand-off is the supported transparent integration; direct
TUN/VLESS ingress is not part of the current architecture. Intermittent UDP
blocking, recovery, restart, and a bounded mixed production soak are recorded
in [`RELEASE-HARDENING-20260817.md`](RELEASE-HARDENING-20260817.md). Longer
operation across other NATs and middleboxes still needs field evidence.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| A copied YAML exists on disk but does not appear under Profiles | Files are not auto-registered; edit the existing profile, or use **New → Local** through the UI only when no profile exists |
| `queqiao` exists under `proxies:` but is absent from the Proxies page | Its name was not added to the `proxies:` list of the selector the UI displays |
| `queqiao` appears, but selecting it does not increase Queqiao metrics | The matching rule routes to a different group, or the core still has an older generated configuration |
| The edit disappears after a subscription refresh | The remote subscription replaced its downloaded YAML; apply the profile-specific Extension Script instead |
| Save reports `did not find expected key` or another YAML error | The new list item is nested under the previous proxy; align `- name: queqiao` with the other proxy entries |
| Closing and reopening the window does not apply the edit | Mihomo is still running as a background service; save/reactivate the profile or use **Restart Core** |
| Everything works but the egress IP is your *old* proxy's | `--local-address` missing or not taking effect; verify with `tcpdump` on the egress |
| `curl` through the SOCKS port succeeds but reports your own IP | `NO_PROXY` is set; use `env -u NO_PROXY -u no_proxy curl --noproxy ''` |
| Client starts, no flows ever appear in `/metrics` | Clash rules are not reaching the group that holds the node |
| Handshake fails with a certificate error | `--server-name` does not match the certificate's CN/SAN, or `--root-ca` is the wrong file |
| An older build works, then stops after several cancelled transfers | upgrade both endpoints; this was the abort lifecycle defect fixed in `STALL-20260817.md` |
| Throughput fine, UDP applications fail | peer negotiated no QUIC datagrams and fell back to the control stream, or the lane is TLS/TCP |

Logs are on stdout; `--log-level debug` reports the outer lane's transport,
whether it is pooled, and per-flow completion. `--metrics-listen` exposes
loopback-only counters for flows, lanes, fallbacks, controller state, and
rescue events.

## Removing it

```sh
launchctl unload ~/Library/LaunchAgents/me.01.queqiao.client.plist   # macOS
sudo systemctl disable --now queqiaod                                # egress
```

First select the previous node in Clash. Then delete the `queqiao` node and its
group entry from the profile, or remove/disable its profile-specific Extension
Script. Nothing else on the host is modified: queqiao installs no routes, no
TUN device, and no system proxy settings.
