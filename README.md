# wanopt

`wanopt` is an experimental, open-source performance-enhancing proxy for
high-latency or lossy long-haul links. It is designed for the specific case
where a client in China must always egress from one fixed US server.

The project is intended to make one logical application flow able to use an
adaptive pool of independent encrypted transport lanes while preserving low
latency for interactive traffic. The first transport implementation will use
QUIC/UDP, with an authenticated TLS/TCP fallback when UDP is blocked or
unstable.

The scheduler is inspired by PIAS: new flows receive a short high-priority
budget, sustained one-way flows are demoted to bulk, and bidirectional
bursty flows remain interactive. Classification uses byte counts and timing,
not HTTPS decryption or MITM.

## Current status

The repository now contains an authenticated SOCKS-to-PEP prototype with
TLS/TCP, QUIC, lane joins, cross-lane reassembly, PIAS-inspired
classification, and new-flow UDP/TCP racing. An isolated development service
is deployed without replacing the existing tunnels. The prototype is still
not safe to use as a general-purpose production tunnel: existing-flow resume,
UDP destination ingress, congestion-control tuning, and the release gates in
[`docs/PRODUCTION-DESIGN.md`](docs/PRODUCTION-DESIGN.md) remain incomplete.
The current real-link evidence is recorded in
[`docs/MEASUREMENTS-20260809.md`](docs/MEASUREMENTS-20260809.md).

## Design goals

- One local SOCKS5/TUN-facing agent and one fixed-egress US agent.
- One application TCP flow can be framed, reordered, and striped over
  multiple QUIC lanes.
- A PIAS-inspired policy that protects one-shot and interactive flows while
  allocating additional lanes to bulk flows.
- No HTTPS MITM: the optimizer forwards encrypted application bytes.
- UDP health probing, UDP/TCP racing, fallback, and eventually lane resume.
- Reproducible measurements for latency, throughput, loss, queueing, and
  application-visible failures.

## Non-goals

- Circumventing a hard aggregate capacity limit on the China-US path.
- Automatically decrypting or classifying HTTPS URLs or payloads.
- Claiming that multiple lanes are always fair or faster.
- Replacing the existing tunnel before a measured rollback path exists.

## Development

```sh
go test ./...
go vet ./...
go run ./cmd/wanoptd --help
```

The real-link benchmark harness will live under `scripts/` and will use an
isolated listener on `icourses-dev`; the existing Xray, sing-box, and Clash
Verge services remain out of scope for automatic modification.

On a macOS host where Clash TUN captures the numeric server address, the local
agent can bind the outer socket to the physical source IP:

```sh
wanoptd --mode local --listen 127.0.0.1:12080 \
  --remote 23.135.236.244:12443 --server-name icourses-dev.01.me \
  --local-address 192.168.3.66 --transport auto \
  --secret-file .dev/session.secret
```

The source address is deployment-specific. Binding it is preferable to using
the Clash fake-DNS address, which would route the PEP through the tunnel being
measured.

## Security model

The wire protocol must use an audited TLS/QUIC implementation and explicit
session authentication. It must impose limits on frame sizes, concurrent
flows, buffered bytes, handshake work, and reconnect attempts. Rolling a new
cryptographic primitive or accepting unauthenticated lane joins is out of
scope.
