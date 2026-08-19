# Contributing

Queqiao is an evolving WAN optimization protocol with broad applicability to
intercontinental tunnels, remote corporate access, weak access networks, and
WAN overlay links. Its optimization unit is precise: an endpoint pair whose
shared WAN segment is the dominant bottleneck. Before proposing a large
feature, open a design issue that explains the
measured problem, the supported topology it affects, and how the change will
be evaluated. Do not submit protocol or congestion-control changes justified
only by an emulated benchmark.

## Development checks

Use Go 1.25.13 or newer in the 1.25 line, then run:

```sh
go test -short -timeout 20m ./...
go vet ./...
test -z "$(gofmt -l .)"
python3 -m unittest discover -s scripts -p 'test_*.py'
go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 -checks=all,-U1000 ./...
```

Changes to framing, authentication, resource admission, recovery, concurrency,
or packaging should also run the relevant full/race/fuzz/fallback checks from
`.github/workflows/deep.yml`. Normal CI runs the short Go suite natively on
Linux, macOS, and Windows for amd64 and arm64; deep and release-candidate runs
exercise the full suite on all six targets and the race suite on every target
supported by Go (all except Windows/arm64). A wire change must increment
`internal/protocol.Version`, update `docs/PROTOCOL.md`, document its upgrade
path, and add fail-closed compatibility tests.

The full gosec invocation intentionally reports reviewed findings. Validate its
JSON with `scripts/check_gosec.py` as documented in
`docs/archive/2026-08-development/STATIC-SECURITY-AUDIT-20260817.md`; do not
silence a rule globally. That audit is a historical baseline; a release
candidate requires a fresh report for its exact commit.

Measurements must record the commit, toolchain, module graph, exact command,
path parameters, trial order, and raw output. Use alternating or shared-path
controls for live comparisons; do not compare transports in separate time
windows and attribute path movement to code.

Network reports should follow
[`docs/CONTRIBUTING-NETWORK-EVIDENCE.md`](docs/CONTRIBUTING-NETWORK-EVIDENCE.md).
Short-lived, interactive, and bulk traffic are evaluation families for the same
unified protocol, not separate architectures to implement or select.

## Pull requests

Keep changes scoped, include regression tests, and update the design or
operations documents whose guarantees change. Never commit credentials,
private keys, active host configuration, packet captures with user traffic, or
reports containing private infrastructure details. Report vulnerabilities as
described in `SECURITY.md`, not in a public issue or pull request.
