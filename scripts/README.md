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
