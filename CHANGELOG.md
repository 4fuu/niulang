# Changelog

All notable user-visible changes are recorded here. Queqiao uses semantic
versioning for release artifacts, while the pre-1.0 wire-compatibility rules
remain stricter than semantic versioning alone; see `docs/PROTOCOL.md`.

## Unreleased

### Added

- Native iOS and Android VPN applications with connection-first home screens,
  live aggregate session counters, secure multi-profile invitation imports,
  explicit active-profile selection, profile management, explicit paste/share
  imports, and per-profile all-traffic or local-network-bypass routing policies.
- Explicit wire-protocol version reporting and diagnosable version mismatch
  errors.
- The first public wire contract is protocol 1 (`queqiao/1`); unreleased
  development wire numbers are intentionally not compatibility targets.
- Public security reporting, supported-topology, known-limitations, field
  validation, security-review, and release-gate documentation.
- Deterministic CycloneDX software bills of materials and complete linked
  dependency license notices in release archives.
- Non-publishing release-candidate automation with native archive smoke tests
  and GitHub build-provenance attestations when the repository is public.
- Private-root/server-leaf/session-secret generation, coordinated rotation
  guidance, and a checksummed real-path TCP/UDP soak harness.
- Separate metrics and structured warnings for UDP-path unavailability and
  configured endpoints that fail over both QUIC/UDP and TLS/TCP.
- A light project icon in the README and release archives, so extracted release
  documentation retains its branding without a broken relative image.

### Changed

- The project license is now the MIT License.
- Public-facing operational evidence uses placeholders instead of active host
  addresses and workstation-specific paths.

### Fixed

- CI now installs a commit-pinned Android command-line toolchain and runs a
  checksum-verified, version-pinned SwiftLint binary instead of depending on
  mutable hosted-runner contents.
- Release candidates now require successful normal CI evidence for the exact
  commit, including mobile qualification, and final publication rebuilds every
  release target twice to reject non-reproducible output.
- The release packager now rejects dirty or ambiently overridden builds and
  mismatched commit/date metadata instead of trusting caller-supplied
  provenance strings.
- The default destination policy now rejects NAT64/6to4-encoded private IPv4
  targets and current non-global IANA special-purpose ranges.
- The desktop client now rejects non-loopback SOCKS listener addresses instead
  of allowing an accidentally exposed unauthenticated proxy.
- Mobile invitation fields opt out of platform state/autofill exposure, and
  iOS no longer marks dynamic core and error details as public unified-log data.
- Mobile apps no longer claim the unauthenticated `queqiao` custom URL scheme,
  preventing another installed app from intercepting an unused bearer invitation.
- The transitive `klauspost/compress` dependency is updated past the affected
  range for GO-2026-5841, even though the vulnerable S2 symbols were not
  reachable from Queqiao.
- Peer-supplied handshake timestamp and in-memory frame-length conversions are
  checked before narrowing.
- Outer EOF following a peer CLOSE is treated as a normal read half-close,
  preserving the lane's final-ACK/write direction without delaying retirement
  of a lane that fails before sending CLOSE.
- AUTO transport selection now treats TCP as a warm standby: a faster TCP
  handshake cannot count as UDP failure or drive the endpoint into TCP-only
  cooldown. A separate preference grace bounds application delay, pooled QUIC
  recovery continues behind a grace-expired TCP request, and only explicit
  QUIC failure confirmed by a working TCP control advances the conservative
  UDP-failure detector.

## v0.1.0 - planned

The first public preview has not been tagged or published.

Planned scope:

- Authenticated TLS/QUIC transport with TLS/TCP fallback.
- Fixed-egress SOCKS5 TCP CONNECT and UDP ASSOCIATE.
- QUIC DATAGRAM carriage for UDP, bounded recovery, and UDP relay-source
  preservation during TCP rescue.
- PIAS-inspired flow classification and reactive bulk-flow isolation.
- Deterministic archives for Linux, macOS, and Windows on amd64 and arm64.

The release remains blocked on every unchecked item in
[`docs/RELEASE-CHECKLIST.md`](docs/RELEASE-CHECKLIST.md).
