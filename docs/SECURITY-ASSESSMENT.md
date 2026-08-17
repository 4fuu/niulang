# Independent security assessment package

## Status and independence

The in-repository review is maintainer-authored and is not independent. This
document defines the package and acceptance criteria for an external reviewer;
it must not be marked complete by the implementation author or by an automated
code-review tool acting on the author's behalf.

The maintainer-side review in
[`STATIC-SECURITY-AUDIT-20260817.md`](STATIC-SECURITY-AUDIT-20260817.md) is an
input to this assessment, not a substitute for an independent reviewer.

Reviewer name/organization: **pending**

Candidate commit: **pending**

Review dates: **pending**
Final report: **pending**

## Review scope

The external review covers:

- TLS configuration, certificate identity, ALPN, HELLO HMAC construction,
  timestamps/nonces, replay cache, and secret handling;
- wire parsing before allocation, frame flags/types, ACK ranges, packet
  envelopes, fast-open capability gating, and fail-closed version behavior;
- session, QUIC connection, lane, flow, buffer, replay, reassembly, UDP relay,
  resume-token, timeout, and tombstone admission bounds;
- UDP relay-source resumption, token entropy/single use/expiry, TCP recovery,
  duplicate suppression, and transport downgrade/fallback behavior;
- egress DNS resolution and destination policy, including rebinding and private,
  loopback, link-local, multicast, documentation, and benchmark ranges;
- unauthenticated local SOCKS/metrics exposure, logging, service permissions,
  core dumps, configuration permissions, and release/rollback handling;
- dependency/toolchain vulnerabilities, SBOM accuracy, provenance attestations,
  and release-workflow permissions.

Out of scope unless separately contracted: cryptanalysis of TLS or QUIC,
security of Clash/mihomo, the operating system, the destination application,
traffic-analysis anonymity, denial of service beyond documented resource
ceilings, and multi-user tenancy that v0.1 does not claim to support.

## Materials supplied

- `docs/ARCHITECTURE.md`, `docs/PROTOCOL.md`, `docs/SECURITY-REVIEW.md`,
  `docs/KNOWN-LIMITATIONS.md`, and `docs/PRODUCTION-DESIGN.md`;
- candidate source and full Git history;
- normal/deep CI evidence, fuzz target inventory and results, vulnerability
  report, SBOMs, provenance attestations, and release-candidate checksums;
- deterministic fallback-soak and redacted real-path field evidence;
- deployment templates and a disposable two-endpoint test environment with
  non-production credentials.

## Finding requirements

Each finding records severity, affected commit/component, preconditions,
reproduction, impact, recommended remediation, maintainer disposition, fixing
commit, and retest result. The final report explicitly states limitations and
whether the supported v0.1 topology was assessed.

Critical and high findings block every public binary release. Medium and low
findings must be fixed or documented with a concrete rationale and mitigation.
The maintainer publishes the final report or a reviewer-approved public summary
and links it from `docs/RELEASE-CHECKLIST.md`.
