# Measurement harnesses

## Emulated-path comparison (the fast inner loop)

`bench_matrix.sh` runs wanopt and a TUIC-shaped reference proxy over one
deterministic emulated path, in a single process, via `cmd/wanoptbench`. Use it
for any transport change before spending time on the live link:

```sh
./scripts/bench_matrix.sh --trials 5 --output /tmp/matrix.tsv
```

The reference (`internal/baseline`) is a measurement control, not a product: it
reproduces TUIC's data-path shape on the same QUIC stack and controllers wanopt
uses, so a gap between the two rows is attributable to the transport design
rather than to the language or QUIC library. The emulator (`internal/pathsim`)
applies a fixed delay, seeded loss, a bottleneck with tail-drop queueing, and
optionally a per-source-address policer; one seed reproduces one loss pattern.

Single cells can be run directly:

```sh
go run ./cmd/wanoptbench --rtt 200 --loss 3 --rate 100 --bytes $((10*1024*1024)) \
    --trials 5 --flows 1,4 --latency
```

Extra lanes cannot raise one flow's goodput when the only limit is an aggregate
bottleneck. Use `--per-flow-rate` to model a path that polices per 4-tuple,
which is the regime where lanes are supposed to help:

```sh
go run ./cmd/wanoptbench --stacks wanopt --rtt 200 --loss 1 --rate 400 \
    --per-flow-rate 25 --bytes $((100*1024*1024)) --lanes 4 --initial-lanes 4
```

The emulator models independent per-packet loss and one bottleneck queue per
direction. It does not model bursty or correlated loss, reordering, variable
delay, or middleboxes, and both endpoints run on one machine. Live campaigns
remain necessary.

## Matched live comparison

`bench_live_matched.sh` alternates trials between two already-running SOCKS5
endpoints against one fixed remote object. The China-US path moves between
roughly 0% and 50% loss within minutes, so running all of one transport's
trials and then all of the other's compares two path windows rather than two
transports; alternating, and swapping which goes first each round, keeps the
comparison inside one window. Even so, a handful of rounds is not enough on
this link — expect to need well over ten, and report completion counts, not
only medians.

```sh
# US host: the reference server and an isolated wanopt server.
./wanoptref --mode server --listen :12531 --tls-cert c.pem --tls-key k.pem --token-file t
./wanoptd  --mode server --listen :12540 ...

# Local: one client each, both bound to the physical interface so a TUN-mode
# proxy does not capture the outer UDP socket and silently tunnel the test.
./wanoptd  --mode local --listen 127.0.0.1:12140 --remote IP:12540 --local-address 192.0.2.10 ...
./wanoptref --mode client --listen 127.0.0.1:12141 --remote IP:12531 --local-address 192.0.2.10 ...

./scripts/bench_live_matched.sh --url http://127.0.0.1:28095/10mb.bin --rounds 16
```

Use the literal server IP, never a hostname that a local proxy may resolve to a
fake IP. Both clients must use the same timeout, and any temporary listener
must be stopped when the campaign ends.

`bench_http.sh` measures independent application TCP flows through an already
running local SOCKS5 client. It emits one TSV row per flow and keeps partial
HTTP bodies, non-200 responses, curl errors, and timeouts in the output. A
row is complete only when curl exits successfully, the response is HTTP 200,
and the exact expected body length is present.

Example for the fixed CacheFly object used in the reports:

```sh
WANOPT_SOCKS5=127.0.0.1:12080 \
WANOPT_TRIALS=5 WANOPT_FLOWS='1 2 4 8' \
./scripts/bench_http.sh --output /tmp/wanopt-http.tsv
```

The script intentionally measures concurrent independent application flows.
It does not claim that eight application connections are equivalent to one
logical striped flow; the latter is covered by the PEP integration tests and
the real-path campaign reports. Run the client with a physical source binding
when Clash TUN/fake DNS would otherwise capture the outer endpoint, and keep
the existing tunnel as the rollback path.

`bench_single_flow.sh` measures exactly one HTTP application connection per
trial. Run it once for each separately configured client lane topology, and
keep the label in the output so the rows cannot be mistaken for independent
application flows:

```sh
WANOPT_SOCKS5=127.0.0.1:12080 WANOPT_LABEL=lanes-1 \
  WANOPT_TRIALS=5 ./scripts/bench_single_flow.sh --output /tmp/one.tsv
```

For a fixed-topology comparison, set `--initial-lanes=N --max-lanes=N` on the
client. The default client remains independent-lane mode; `--quic-pool` is an
explicit opt-in for a persistent multiplexed QUIC control connection and
should be enabled only after path-specific latency/throughput validation.

## Dedicated upload sink

Public upload endpoints are unsuitable throughput oracles. For a controlled
US-side upload measurement, run the bounded test-only sink on the fixed-egress
host, expose it only for the duration of the trial, and stop it afterward:

```sh
python3 scripts/upload_sink.py --listen 0.0.0.0:28080 --max-bytes 67108864
dd if=/dev/zero bs=1m count=10 2>/dev/null |
  curl --socks5-hostname 127.0.0.1:12081 --data-binary @- \
       --write-out '%{http_code}\t%{size_upload}\t%{time_total}\n' \
       http://23.135.236.244:28080/
```

The sink requires an explicit bounded `Content-Length`, accepts one request by
default, and exits after that request. It must not be left as a public
service; use a temporary systemd unit or process supervisor and verify that
the listener is gone after the measurement.
