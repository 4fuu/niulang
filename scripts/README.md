# Measurement harnesses

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
