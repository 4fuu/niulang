# Public-preview candidate report — 2026-08-17

## Decision

The repository-side implementation and documentation work for the v0.1 public
preview is complete. This report does **not** authorize publication. No tag or
GitHub Release was created, repository visibility was not changed, and the live
service was not upgraded to the candidate.

The first complete non-publishing candidate was `v0.1.0-rc.1` at commit
`c24fa15937f081e699016c6b482752c4c621f2ba`. GitHub Actions run
[`32019866832`](https://github.com/bojieli/queqiao/actions/runs/32019866832)
completed successfully. Full tests, race tests, all discovered fuzz targets,
vet, staticcheck, the reviewed gosec baseline, vulnerability scanning,
full-history secret scanning, deterministic fallback soak, double packaging,
and native execution on all six advertised OS/architecture targets passed.
Provenance was explicitly skipped because the repository was private.

The normal CI run
[`32018764158`](https://github.com/bojieli/queqiao/actions/runs/32018764158)
also passed on that commit.

## Artifact evidence

- Version: `v0.1.0-rc.1`
- Commit: `c24fa15937f081e699016c6b482752c4c621f2ba`
- Go: `go1.25.13`
- Wire protocol: `3`
- SHA-256 of the downloaded `SHA256SUMS` file:
  `c27b18a8323c4943507ff664e7cea2d57a0fe6317bf3091891f3b909b42fd65c`
- The complete candidate artifact passed `scripts/validate_release.py`.
- A local all-target build was byte-for-byte identical across two builds and
  byte-for-byte identical to the downloaded workflow artifact.
- Linux, macOS, and Windows archives executed successfully on native amd64 and
  arm64 GitHub-hosted runners.
- Every archive includes exact build metadata, the MIT project license,
  complete linked-module license texts, and an embedded CycloneDX 1.5 SBOM;
  the adjacent SBOM is identical.

## Compatibility, field, and security evidence

[`COMPATIBILITY-20260817.md`](COMPATIBILITY-20260817.md) records both rolling
wire-3 upgrade directions, candidate-to-candidate operation, explicit refusal
of a same-ALPN wire-2 peer, and an isolated atomic install/rollback exercise.
No production binary was changed.

The exact candidate completed a strict 60-second run over the existing field
path: 27/27 persistent SOCKS5 UDP probes, 3/3 verified HTTPS flows, and all five
final correctness probes passed, with no lane replacement, lane failure, flow
failure, or unbounded resource growth. The redacted longer field record is in
[`field-results/20260817-primary-high-port.md`](field-results/20260817-primary-high-port.md).
It is one path only and is not evidence for the wider matrix.

The pinned full-history scan used a positive canary and reported zero findings.
The current tree was separately checked for active secrets, private keys,
private hostnames, personal filesystem paths, and unnecessary live addresses.
Field TLS/session credentials were rotated independently and superseded secret
rollback material was removed.

## Gates intentionally still open

The following work cannot be honestly completed by repository automation or by
the implementation author alone:

- GitHub provenance attestations for the exact artifacts require the repository
  to be public. The publishing workflow fails closed while it is private.
- The `public-release` environment exists, but the current private-repository
  plan did not permit adding a required reviewer. Publication remains blocked
  until a human reviewer is required and administrator bypass is disabled where
  the plan permits.
- The maintainer must review and explicitly approve the exact commit and
  candidate artifacts.
- A production-ready claim additionally requires the independent access-network,
  egress-provider, port, OS, and impairment matrix plus two 24–72-hour soaks
  defined in [`FIELD-VALIDATION.md`](FIELD-VALIDATION.md).
- An independent human or third-party reviewer must complete
  [`SECURITY-ASSESSMENT.md`](SECURITY-ASSESSMENT.md), with all critical/high
  findings remediated and lower-severity dispositions published.
- Production monitoring, incident response, supported-version lifetime, and
  credential-rotation ownership still need named maintainers.

After any documentation reconciliation commit, run the complete non-publishing
candidate workflow again and select that exact successful commit for review.
Do not create a tag or invoke the publishing workflow until every public-preview
checkbox in [`RELEASE-CHECKLIST.md`](RELEASE-CHECKLIST.md) is complete.
