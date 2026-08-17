# Static security audit — 2026-08-17

This is the maintainer's pre-release static-analysis record. It is not an
independent security assessment; the independent review described in
[`SECURITY-ASSESSMENT.md`](SECURITY-ASSESSMENT.md) remains a release gate.

## Scope and results

The entire Go module and reachable Git history were checked from the release
readiness branch:

| Check | Version | Result |
| --- | --- | --- |
| `go test -short ./...` | Go version in `go.mod` | Pass |
| `go vet ./...` | Go version in `go.mod` | Pass |
| Staticcheck `-checks=all,-U1000 ./...` | v0.7.0 | Pass after removing dead code and correcting the reported defects |
| gosec `./...` | v2.28.0 | 124 reviewed findings; no unreviewed rule/file bucket |
| Gitleaks full-history scan plus positive canary | v8.24.3, checksum-pinned | Pass; zero findings in 251 commits |

The gosec total comprises G115 (95), G204 (2), G301 (3), G302 (2), G304
(10), G306 (4), and G404 (8). The scan prompted three direct hardening changes:

- reject handshake timestamps above the signed Unix range and compare clock
  skew without overflow;
- compare in-memory frame length before any narrowing conversion; and
- replace pointer-derived shared-path membership with a monotonic identifier.

## Reviewed gosec findings

- G115 reports bounded wire lengths, enum values, sequence-number modular
  arithmetic, FEC finite-field arithmetic, non-negative QUIC byte/duration
  types, and counters whose operational limits are far below the destination
  integer widths. The public CLI conversions occur only after explicit bounds
  checks. The remaining frame conversion follows a `len <= 1 MiB` check.
- G204 reports the release packager invoking the Go compiler with fixed command
  names and argument vectors, and the benchmark/reference harness launching an
  explicitly configured comparison process. Neither passes arguments through
  a shell.
- G301, G302, and G306 report public release directories, binaries, reports,
  certificates, and archives. These are intentionally readable. Credential
  generation is separate and runs under `umask 077`; private keys and session
  secrets are installed with mode 0600.
- G304 reports operator-selected CLI certificate/secret paths and packager
  reads rooted in the selected source/module directories. These interfaces
  intentionally read files selected by the invoking user and do not accept
  unauthenticated remote path input.
- G404 is confined to the deterministic path simulator, where reproducibility
  is the requirement and no random value protects a secret or authorization
  decision.

[`check_gosec.py`](../scripts/check_gosec.py) records a maximum count for each
reviewed rule/file bucket. CI runs the full scanner and fails if a new rule,
new file, or increased bucket appears; findings are not globally hidden with a
rule exclusion.

## Reproduction

```sh
go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 -checks=all,-U1000 ./...
go run github.com/securego/gosec/v2/cmd/gosec@v2.28.0 \
  -fmt json -out gosec.json ./... || test $? -eq 1
./scripts/check_gosec.py gosec.json .
./scripts/scan_history_secrets.sh .
```

The nonzero gosec process status is expected when reviewed findings exist; the
baseline checker supplies the gating status. A final history scan must be run
on the exact reviewed commit.
