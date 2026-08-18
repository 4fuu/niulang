# Public release checklist

This checklist is the release authority for v0.1. A Git tag or GitHub Release
must not be created until every preview blocker is checked, the evidence links
name the candidate commit, and the maintainer has reviewed and approved that
exact commit. Production-ready language has additional gates below.

## Public-preview blockers

- [x] Supported topology and known limitations are documented.
- [x] Wire protocol 4 is documented, emitted in build metadata, and rejects
  every other version with a diagnosable error.
- [x] Security reporting instructions and residual risks are documented.
- [x] A pinned full-history secret scan reports zero unresolved findings for
  the candidate commit.
- [x] The history scan contains no deployed credential; field-validation
  credentials and TLS material have been rotated independently, and rollback
  material contains no superseded secret.
- [x] Current-tree operational evidence contains no active secret, private key,
  private hostname, personal path, or unnecessary live host address.
- [x] Linked dependency licenses and the CycloneDX SBOM match the packaged
  binary for every target.
- [ ] GitHub provenance attestations cover every archive, SBOM, and checksum
  manifest in the candidate artifact.
- [x] Downloaded archives pass native runtime smoke tests on Linux, macOS, and
  Windows; unsupported native architectures are called out rather than implied.
- [x] Normal CI, full tests, race tests, vet, vulnerability scan, fuzz smoke,
  fallback soak, package reproducibility, and actionlint are green on the exact
  candidate commit.
- [x] Staticcheck and the reviewed gosec baseline pass on the exact candidate;
  retain the evidence described in
  [`STATIC-SECURITY-AUDIT-20260817.md`](STATIC-SECURITY-AUDIT-20260817.md).
- [x] Install, one-endpoint patch upgrade, coordinated incompatible-version
  refusal, and rollback evidence is attached to the candidate report.
- [x] `CHANGELOG.md` and the candidate release notes describe v0.1 as an
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
- [ ] An independent reviewer assesses the design in `SECURITY.md`; all critical
  and high findings are fixed, and accepted lower-severity findings are public.
- [ ] Operational monitoring, incident response, supported-version lifetime,
  and credential-rotation ownership have named maintainers.

## Mobile release blockers

- [x] Android and iOS use the protocol-4 core with full IPv4/IPv6 TCP and UDP,
  crash-safe enrollment, platform secure storage, automatic renewal, bounded
  packet/session queues, and no third-party proxy application.
- [x] The exact linked runtime and isolated build-tool graphs are pinned,
  license-allowlisted, reproducibly audited, and their full notices are
  embedded in both applications.
- [x] Android release lint, R8, APK/AAB assembly, test-APK assembly, iOS strict
  lint, simulator build, and app/core boundary tests pass locally.
- [x] The dependency-free Android instrumentation suite passes all six
  storage, catalog, routing, and protocol-boundary checks on isolated API 33
  and API 35 emulators.
- [ ] Android API 30, 33, and current physical devices pass the Keystore suite
  and the complete profile-probe, TCP/UDP, IPv4/IPv6, DNS, permission, revoke,
  and lifecycle matrix on Wi-Fi and cellular.
- [ ] Current physical iPhones pass signing, install, packet-tunnel TCP/UDP,
  IPv4/IPv6, DNS, per-profile probe, permission, revoke, sleep/wake,
  Wi-Fi/cellular transition, and reconnect tests.
- [ ] iOS app-update, configuration removal, provider failure, and network-loss
  stops produce a named provider reason plus `fetchLastDisconnectError` output
  in the sanitized, shareable connection log.
- [ ] Both platforms pass 24-hour mixed interactive/bulk soak with bounded
  memory, goroutines/threads, descriptors, packet queues, and energy use.
- [ ] Near-expiry certificate renewal is demonstrated during an active tunnel,
  and revocation closes active traffic within the documented bound.
- [ ] Clean install, signed upgrade, failed-upgrade recovery, profile deletion,
  and signing-key recovery procedures are exercised on release artifacts.
- [ ] An independent mobile security/privacy review closes all critical and
  high findings and records accepted lower-severity findings publicly.
- [ ] Android Developer Console/package registration and, if applicable,
  Google Play Organization/VpnService declarations are complete. iOS remains
  source-build only unless an Organization team completes App Store review and
  jurisdiction-specific VPN obligations.

Completing the preview section permits a v0.1 experimental release; it does
not complete or waive the production-ready section.

The completed technical gates and deliberately open approval/external gates
are summarized in [`RELEASE-CANDIDATE-20260817.md`](RELEASE-CANDIDATE-20260817.md).
