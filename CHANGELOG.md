# Changelog

All notable user-visible changes are recorded here. Queqiao uses semantic
versioning for release artifacts, while the pre-1.0 wire-compatibility rules
remain stricter than semantic versioning alone; see `docs/PROTOCOL.md`.

## Unreleased

### Added

- Explicit wire-protocol version reporting and diagnosable version mismatch
  errors.
- Public security reporting, supported-topology, known-limitations, field
  validation, security-review, and release-gate documentation.
- Deterministic CycloneDX software bills of materials and complete linked
  dependency license notices in release archives.
- Non-publishing release-candidate automation with native archive smoke tests
  and GitHub build-provenance attestations.
- Private-root/server-leaf/session-secret generation, coordinated rotation
  guidance, and a checksummed real-path TCP/UDP soak harness.

### Changed

- The project license is now the MIT License.
- Public-facing operational evidence uses placeholders instead of active host
  addresses and workstation-specific paths.

### Fixed

- Peer-supplied handshake timestamp and in-memory frame-length conversions are
  checked before narrowing.
- Outer EOF following a peer CLOSE is treated as a normal read half-close,
  preserving the lane's final-ACK/write direction without delaying retirement
  of a lane that fails before sending CLOSE.

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
