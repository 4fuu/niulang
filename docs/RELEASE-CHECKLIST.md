# Public release checklist

This checklist is the release authority for v0.1. A Git tag or GitHub Release
must not be created until every preview blocker is checked, the evidence links
name the candidate commit, and the maintainer has reviewed and approved that
exact commit. Production-ready language has additional gates below.

## Public-preview blockers

- [x] Supported topology and known limitations are documented.
- [x] Wire protocol 3 is documented, emitted in build metadata, and rejects
  every other version with a diagnosable error.
- [x] Security reporting instructions and residual risks are documented.
- [ ] A pinned full-history secret scan reports zero unresolved findings for
  the candidate commit.
- [ ] Deployed field-validation credentials and TLS material have been rotated
  after the final history scan; rollback material contains no superseded secret.
- [ ] Current-tree operational evidence contains no active secret, private key,
  private hostname, personal path, or unnecessary live host address.
- [ ] Linked dependency licenses and the CycloneDX SBOM match the packaged
  binary for every target.
- [ ] GitHub provenance attestations cover every archive, SBOM, and checksum
  manifest in the candidate artifact.
- [ ] Downloaded archives pass native runtime smoke tests on Linux, macOS, and
  Windows; unsupported native architectures are called out rather than implied.
- [ ] Normal CI, full tests, race tests, vet, vulnerability scan, fuzz smoke,
  fallback soak, package reproducibility, and actionlint are green on the exact
  candidate commit.
- [ ] Staticcheck and the reviewed gosec baseline pass on the exact candidate;
  retain the evidence described in
  [`STATIC-SECURITY-AUDIT-20260817.md`](STATIC-SECURITY-AUDIT-20260817.md).
- [ ] Install, one-endpoint patch upgrade, coordinated incompatible-version
  refusal, and rollback evidence is attached to the candidate report.
- [ ] `CHANGELOG.md` and the candidate release notes describe v0.1 as an
  experimental paired fixed-egress preview.
- [ ] The maintainer has reviewed the candidate artifacts and explicitly
  approved publication.
- [ ] The GitHub `public-release` environment requires the approving reviewer,
  and the final workflow inputs name the reviewed commit and candidate run.

## Production-ready claim blockers

- [ ] The real-network matrix in `FIELD-VALIDATION.md` is complete across the
  minimum independent access networks and two egress providers.
- [ ] At least two representative paths complete the required 24-72-hour soak
  without correctness failures or unbounded resource growth.
- [ ] An independent reviewer completes `SECURITY-ASSESSMENT.md`; all critical
  and high findings are fixed, and accepted lower-severity findings are public.
- [ ] Operational monitoring, incident response, supported-version lifetime,
  and credential-rotation ownership have named maintainers.

Completing the preview section permits a v0.1 experimental release; it does
not complete or waive the production-ready section.
