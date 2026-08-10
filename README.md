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
classification, SOCKS5 TCP CONNECT and bounded UDP ASSOCIATE, new-flow
UDP/TCP racing, bounded lane recovery, completion tombstones, aggregate
pacing, and opt-in QUIC controllers. The stock apNet
QUIC controller is retained as the control; an independently implemented
BBRv1-shaped controller, an adaptive controller, and a Hysteria-style
fixed-rate (Brutal) controller can be selected for measurement.
An isolated development service is deployed without replacing the existing
tunnels. The latest five-block real-link evidence is recorded in
[`docs/MEASUREMENTS-20260810.md`](docs/MEASUREMENTS-20260810.md); the earlier
pilot remains in [`docs/MEASUREMENTS-20260809.md`](docs/MEASUREMENTS-20260809.md).

The prototype is still not safe to use as a general-purpose production
tunnel: BBR has failed on the measured path and broader loss/soak campaigns
remain outstanding. UDP is currently carried over reliable stream frames
(native QUIC DATAGRAM and TUN/VLESS ingress are not yet implemented), and a
mid-session rescue creates a fresh authenticated association rather than
resuming the old remote relay. The project has not passed all
controlled-loss/resource release gates in
[`docs/PRODUCTION-DESIGN.md`](docs/PRODUCTION-DESIGN.md).

## Design goals

- One local SOCKS5/TUN-facing agent and one fixed-egress US agent.
- One application TCP flow can be framed, reordered, and striped over
  multiple QUIC lanes.
- A PIAS-inspired policy that protects one-shot and interactive flows while
  allocating additional lanes to bulk flows.
- No HTTPS MITM: the optimizer forwards encrypted application bytes.
- UDP health probing, UDP/TCP racing, fallback, and bounded mid-session lane
  replacement.
- Dedicated cold flows pipeline the authenticated `HELLO` and destination
  `OPEN` frames, preserving the original wire order of acknowledgements while
  removing one sequential China-US control exchange. Pooled streams retain
  capability-negotiated `OPEN_FAST`.
- One aggregate token bucket with an interactive reserve above all lanes.
- Optional localhost `/metrics` counters for flow completion, bytes, fallback,
  lane failure/replacement, PIAS class transitions, active QUIC RTT/loss, and
  controller mode/max bandwidth/pacing/cwnd/bytes-in-flight/min-RTT/recovery
  telemetry, plus explicit flow-idle/lifetime timeouts. The endpoint is
  loopback-only in the development service.
- Reproducible measurements for latency, throughput, loss, queueing, and
  application-visible failures.

The unattended client default is one QUIC lane because independent congestion
controllers can reduce goodput on a lossy path. `--max-lanes` and
`--bulk-start-lanes` are explicit path-validation knobs for measured bulk
experiments; adaptive growth still requires positive marginal gain.
`--quic-pool` is an explicit opt-in that keeps one bounded QUIC connection for
initial/control streams and lets multiple short flows share its congestion
controller; configured bulk lane joins remain independent. These modes must
be validated with the supplied single-flow and concurrent-flow harnesses
before being enabled in a live Clash profile. The first pooled stream performs
the authenticated `HELLO`; subsequent streams on a capable peer use a
connection-scoped fast open while retaining independent flow identities and
US-side destination-policy checks. Capability-free peers automatically keep
the legacy per-stream handshake.

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
  --local-address auto --transport auto \
  --secret-file .dev/session.secret
```

`--local-address auto` discovers the active non-loopback, non-point-to-point
IPv4 address before each outer dial, so ordinary DHCP changes do not strand
the client. A literal address or `if:NAME` may be used when the host has more
than one physical IPv4 address. Binding it is preferable to using
the Clash fake-DNS address, which would route the PEP through the tunnel being
measured.

For a controlled bulk experiment, both endpoints can select the fixed-rate
controller. The rate is bytes per second per QUIC lane; it must be measured for
the path and reduced if loss or interactive tail latency rises:

```sh
wanoptd --mode local --listen 127.0.0.1:12080 \
  --remote 23.135.236.244:12443 --server-name icourses-dev.01.me \
  --local-address auto --transport quic \
  --congestion brutal --brutal-bytes-per-sec 1048576 \
  --max-lanes 8 --secret-file .dev/session.secret
```

The matching server must use the same `--congestion` and rate. To constrain
all lanes and flows together while reserving service for interactive traffic,
add the same aggregate budget at both endpoints, for example:

```text
--aggregate-bytes-per-sec 8388608 \
--interactive-reserve-bytes-per-sec 524288
```

`adaptive` is the safer experimental choice when no target is known; `reno`
is the correctness baseline. `bbr` is the original path-specific experimental
controller. `bbr-tuic` is a separate, opt-in Go port of TUIC's
`quinn-congestions` BBR model; it adds ACK aggregation, a round-based
bandwidth filter, TUIC-style recovery, and ProbeRTT behavior, but it is not a
claim that the controller is faster on every path. Both BBR variants must pass
the same loss, queue-delay, and application-tail campaign before selection.
Brutal remains an operator-supplied measurement mode, not a safe unattended
default. These modes are not a recommendation to change a live Clash profile
without a rollback plan.

For an operator-only health endpoint, bind metrics to loopback and keep it off
the public listener:

```text
--metrics-listen 127.0.0.1:19090
```

Flows are bounded by default at 30 minutes without application payload and
24 hours total lifetime. These limits protect session and destination-socket
resources while still allowing quiet interactive sessions; tune them only
with an explicit operational policy using `--flow-idle-timeout` and
`--flow-max-lifetime`.

## Security model

The wire protocol must use an audited TLS/QUIC implementation and explicit
session authentication. It must impose limits on frame sizes, concurrent
flows, buffered bytes, handshake work, and reconnect attempts. Rolling a new
cryptographic primitive or accepting unauthenticated lane joins is out of
scope.
