# Deploying queqiao, and using it from Clash

This is the practical guide: how to build it, how to run the two ends, and how
to point Clash Verge or any mihomo-based client at it.

Read [the limits](#what-you-are-signing-up-for) before deploying this anywhere
you care about. The project has not met the release gates in
[`PRODUCTION-DESIGN.md`](PRODUCTION-DESIGN.md), and one defect measured on 2026-08-16
will interrupt a working tunnel.

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

Go 1.25 or later.

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

Both ends authenticate with a shared 32-byte secret, and the client verifies
the server's certificate against a pinned root, so a self-signed certificate is
correct here — there is nothing to trust publicly.

```sh
sudo mkdir -p /etc/queqiao && cd /etc/queqiao
sudo openssl req -x509 -newkey rsa:2048 -nodes \
     -keyout key.pem -out cert.pem -days 3650 \
     -subj "/CN=queqiao.node" -addext "subjectAltName=DNS:queqiao.node"
sudo sh -c 'head -c 32 /dev/urandom > secret'
sudo chmod 600 key.pem secret
```

`--server-name` on the client must equal the certificate's name. It is a
verified TLS name, not a DNS lookup: the client dials the literal IP in
`--remote` and presents this as SNI.

Run it under systemd. [`deploy/queqiaod-dev.service`](../deploy/queqiaod-dev.service)
is the hardened unit this project uses; the minimal form is:

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

Copy `cert.pem` and `secret` from the egress host to the client — the secret is
a credential, so treat it accordingly (`chmod 600`).

```sh
mkdir -p ~/.queqiao/bin && chmod 700 ~/.queqiao
cp $(command -v queqiaod) ~/.queqiao/bin/
scp egress:/etc/queqiao/cert.pem ~/.queqiao/cert.pem
scp egress:/etc/queqiao/secret   ~/.queqiao/secret && chmod 600 ~/.queqiao/secret
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

[`deploy/clash-queqiao.yaml`](../deploy/clash-queqiao.yaml) is a complete
profile. Import it with **Profiles → New → Local**, paste or select the file,
and leave your existing profile active until you have tested this one.

The node itself is three lines:

```yaml
proxies:
  - name: queqiao
    type: socks5
    server: 127.0.0.1
    port: 12080
    udp: true          # queqiao implements SOCKS5 UDP ASSOCIATE
```

`udp: true` is worth setting. queqiao carries SOCKS UDP on the connection's
QUIC datagrams where QUIC negotiated them, so an application that chose UDP
keeps datagram semantics instead of having its packets serialised into a stream
where one loss stalls everything behind it.

### Adding it to a profile you already use

You do not need a separate profile. Add the node above to your `proxies:`, add
`queqiao` to whichever `proxy-groups` entry you select from, and you can switch
to it from the Clash UI without touching your rules. Removing those two lines
is the entire rollback.

If you would rather test it without any risk to a working setup, the safe path
is the one in [`PRODUCTION-DESIGN.md`](PRODUCTION-DESIGN.md): a second,
**inactive** profile whose final `MATCH` rule points at the queqiao node. Your
live profile stays untouched and you switch back by selecting it again.

### Verify from Clash

With the profile selected, check that traffic is really taking the new path —
the client's own counters are the quickest answer:

```sh
curl -s 127.0.0.1:12090/metrics | grep -E 'flows_(started|completed)_total|bytes_down_total'
```

If `flows_started_total` stays at zero while you browse, Clash is not routing to
the node: check that your rules reach the group holding it.

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

Also unfinished, and relevant to a daily driver: interactive latency degrades
under queqiao's own bulk load more than the alternatives' does; TUN/VLESS
ingress does not exist, so Clash's SOCKS hand-off is the only integration; and
UDP-blocked, restart, and long-soak behaviour is unmeasured on a real path.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
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

Then delete the node and its group entry from your Clash profile, or select the
profile you were using before. Nothing else on the host is modified: queqiao
installs no routes, no TUN device, and no system proxy settings.
