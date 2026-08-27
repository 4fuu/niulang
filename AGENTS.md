# Niulang contributor guide

This file is the entry point for humans and coding agents changing Niulang.
Read the document that owns the area you plan to change instead of treating
this file as a substitute for the detailed contracts.

## Project contract

- Niulang is an independent Go proxy derived from Queqiao. Its goal is useful
  bandwidth with the lowest practical latency and packet loss, supported by
  repeatable measurements rather than maximum-throughput claims alone.
- `niulangd` is the product binary. `niulangbench`, `niulangref`, and
  `pathprobe` are measurement tools; do not turn controls or probes into
  production behavior.
- The supported release targets are Linux, macOS, and Windows on amd64 and
  arm64. Do not add mobile platform code or packaging.
- Public invitations use only `niulang://enroll/...`.
- Protocol 2 is the only supported contract. QUIC uses real HTTP/3 (`h3`) with
  a `niulang` Extended CONNECT tunnel; TCP uses the `niulang/2`,
  `niulang-standby/2`, `niulang-enroll/2`, and `niulang-renew/2` ALPNs. Identity
  URIs use `niulang://...`. There is no inherited wire, credential, or state
  compatibility and no downgrade path.
- Niulang is experiment-led. Every experimental transport behavior that shows
  a reproducible benefit in a supported path or workload should graduate into
  the automatic default policy instead of remaining behind a manual flag. Scope
  it to the conditions where it helps, retain a safe fallback where conditions
  change, and document measured tradeoffs; a universal win is not required.
  Protocol correctness and security remain hard gates.

## Read before changing

| Change | Authoritative document |
| --- | --- |
| Wire format, carrier behavior, identity, enrollment, or compatibility | [`docs/PROTOCOL.md`](docs/PROTOCOL.md) |
| Benchmark design, path models, metrics, controls, or performance claims | [`docs/BENCHMARKING.md`](docs/BENCHMARKING.md) |
| Installation, service definitions, or operational configuration | [`docs/DEPLOYING.md`](docs/DEPLOYING.md) |
| Runtime logs, metrics, or telemetry schema | [`docs/LOGGING.md`](docs/LOGGING.md) |
| Trust model, credentials, authorization, or vulnerability handling | [`SECURITY.md`](SECURITY.md) |
| Project positioning, supported platforms, and basic build or use | [`README.md`](README.md) |

When code and its owning document disagree, investigate the implementation and
history before deciding which is wrong. Do not silently preserve both stories.

## Repository map

- **`cmd/`** contains executable entry points:
  - `niulangd` is the client, gateway, enrollment, provider-management, and log
    CLI.
  - `niulangbench` runs reproducible transport trials over seeded emulated
    paths.
  - `niulangref` is the standalone TUIC-shaped measurement control, not a
    deployable tunnel.
  - `pathprobe` is an open-loop real-path rate and per-flow policing probe.
  - `niulangpack` creates deterministic release archives and SBOMs.
- **`internal/`** contains all non-public implementation packages:
  - transport and scheduling: `pep`, `stripe`, `multipath`, `session`,
    `classifier`, and `netbind`;
  - path response and recovery: `congestion`, `pathmodel`, `lossmodel`, `fec`,
    `coded`, and `limiter`;
  - protocol and security: `protocol`, `conformance`, and `identity`;
  - product boundaries: `socks5`, `metrics`, `operlog`, `memlimit`, and
    `udperr`;
  - measurement infrastructure: `baseline`, `extproxy`, and `pathsim`.
- **`deploy/`** owns install scripts, service definitions, tuning helpers, and
  the sample Clash configuration. Keep platform-specific behavior here rather
  than in benchmark scripts.
- **`docs/`** owns the durable protocol, benchmark, deployment, and logging
  contracts listed above.
- **`scripts/`** contains benchmark campaigns, result summarizers, release
  validation, and their Python tests. Campaign output is generated evidence,
  not source.
- **`testdata/`** contains checked-in deterministic fixtures. Protocol 2
  conformance vectors live in `testdata/protocol2/vectors.json`.
- **`.github/workflows/`** contains the supported-platform test and cross-build
  matrix.

## Working rules

- Use Go 1.25.13 and keep the module path `github.com/4fuu/niulang`.
- Make the smallest change that fully owns the behavior. Prefer an existing
  package boundary over a one-use wrapper or parallel source of truth.
- Preserve protocol 2 unless the task explicitly changes the wire contract. A
  wire change requires a versioning and migration decision, conformance
  coverage, and a matching protocol-document update; never add an implicit
  legacy parser or downgrade.
- Keep benchmark comparisons matched and seeded. Alternate trial order where
  time-varying load could bias one side, retain raw per-trial output, and report
  completion and tail behavior as well as medians.
- Treat an effective experiment as product work: once a seeded comparison
  demonstrates a benefit in its intended conditions without violating protocol
  correctness or security, make the behavior part of the default adaptive
  strategy and add regression coverage. Do not leave useful behavior opt-in
  merely because other path regimes need a different automatic choice.
- Do not weaken latency, loss, or timing assertions merely because a busy local
  machine flakes. Reproduce the failure, distinguish scheduler noise from a
  product regression, and fix the owning behavior when the evidence supports
  it.
- Separate ambient path erasure, sender-induced bottleneck drops, and
  application residual loss in code, telemetry, and conclusions.
- A configured rate must state whether it is a per-lane rate, aggregate
  application budget, or aggregate wire budget. Do not describe one as
  another.
- Keep provider state, client profiles, invitations, private keys, access
  tokens, public IP addresses, logs, packet captures, `.amp/`, and generated
  release or benchmark output out of commits.

## Verification

Run focused tests while iterating, then scale verification to the change. The
standard full source checks are:

```sh
go test ./... -count=1
go vet ./...
python3 -m unittest discover -s scripts -p 'test_*.py'
bash -n scripts/*.sh
git diff --check
```

Format changed Go files with `gofmt`. If it is not on `PATH`, use:

```sh
"$(go env GOROOT)/bin/gofmt" -w path/to/changed.go
```

For a protocol change, run the conformance and interoperability tests in
addition to package tests. For benchmark behavior, run a small deterministic
smoke test before a larger multi-trial matrix and preserve the manifest,
machine details, raw JSON, summaries, and source patch needed to reproduce it.

For release changes, build into a temporary directory and validate the exact
output:

```sh
go run ./cmd/niulangpack [flags]
python3 scripts/validate_release.py PATH_TO_DIST
```

Never claim a performance improvement from a compile-only check or a single
trial. Never publish or push as part of verification unless the task explicitly
requests that external action.

## Documentation changes

- Update the document that owns a changed contract; do not duplicate mutable
  details in several files.
- Update `docs/PROTOCOL.md` and conformance fixtures with wire, identity, or
  compatibility changes.
- Update `docs/BENCHMARKING.md` when a path model, workload, metric, baseline,
  or interpretation rule changes. Link reproducible evidence rather than
  replacing methodology with a conclusion.
- Update deployment documentation, scripts, and service definitions together
  when their operator-facing behavior changes.
- Keep `README.md` short: project purpose, differences that matter to users,
  supported platforms, and links to the detailed documentation.
