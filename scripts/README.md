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
