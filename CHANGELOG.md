# Changelog

All notable user-visible changes are recorded here. Queqiao uses semantic
versioning for release artifacts, while the pre-1.0 wire-compatibility rules
remain stricter than semantic versioning alone; see `docs/PROTOCOL.md`.

## Unreleased

### Added

- Native iOS and Android applications with connection-first home screens, live
  aggregate session counters, secure multi-profile invitation imports,
  explicit active-profile selection, profile management, explicit paste/share
  imports, and per-profile all-traffic or local-network-bypass routing
  policies.
- Android export mode, which is what the released Android app now is: it
  enrolls the device and serves the gateway as an authenticated SOCKS5
  endpoint on loopback for v2rayNG, mihomo, sing-box, or any other client that
  already owns the device tunnel, with per-install credentials in the
  Keystore-backed store, a changeable port, in-app setup snippets, and the
  per-app exclusion step stated first because skipping it loops the uplink.
  Routing rules, per-app policy, and DNS stay with that client rather than
  being reimplemented here. See `docs/ANDROID-EXPORT.md`.
- RFC 1929 username/password authentication for the SOCKS5 listener, required
  in Android export mode because loopback is shared with every other app on
  the device. The desktop listener's behavior is unchanged.
- Per-profile iOS bypass routes: hand-entered CIDR blocks kept off the tunnel,
  refused rather than silently dropped when they do not parse.
- An experimental per-profile iOS option to keep APNIC address blocks delegated
  to China off the tunnel, backed by a reproducible, provenance-documented
  bundled route set and strict cross-language format tests.
- Per-profile iOS automatic connection rules: bring the tunnel up on Wi-Fi, on
  cellular, or both, and keep it down on Wi-Fi networks the user names. Names
  are typed and never scanned, so no location permission is involved, and a
  manual disconnect pauses the rules until the next manual connect instead of
  being undone by them.
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
- Committed protocol-1 conformance vectors in `testdata/protocol1/vectors.json`
  covering frame headers that must be accepted and refused, acknowledgement
  ranges, destination canonicalization, UDP association and PACKET carriage,
  RESET payloads, sliding-window coefficients and repairs, coded datagrams, and
  enrollment invitations. The test suite replays them on every run, which makes
  the coefficient generator -- derived on both ends and never transmitted, so a
  mismatch would otherwise surface only as unexplained loss -- checkable by a
  second implementation.
- Explicit protocol-1 bounds where the specification previously left a
  receiver's obligations to the sender's policy: the repair window, the
  receiver's decoder width and linear-system floor, and the payload, frame, and
  byte budget of a path probe.

### Changed

- The project license is now the MIT License.
- The Android full-device `VpnService` tunnel is now a debug-only build
  variant, retained as the vehicle that drives the shared packet stack end to
  end on real hardware and never published. The released app declares no
  `BIND_VPN_SERVICE` and no `android.net.VpnService` intent filter, which CI
  asserts against the assembled release APK. A full Android tunnel put the
  data plane in competition with mature routing clients over rules it has no
  engine for; export mode composes with them instead.
- Public-facing operational evidence uses placeholders instead of active host
  addresses and workstation-specific paths.
- The frame payload limit is now a constant of protocol 1 rather than a
  configurable one, and the `--max-payload` flag is removed. Version 1
  negotiates no capabilities, so a per-deployment receive limit made two peers
  mutually unintelligible in one direction with no way to attribute the
  failure. `--chunk-size` is unchanged and is now validated against the wire
  limit; operators who set `--max-payload` should drop it.

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
- A gateway that accepts a path probe without echoing it is now treated as the
  protocol violation it is. The client checks every echoed frame against what it
  sent, and on a mismatch logs the discrepancy, counts a peer protocol
  violation, and discards the prewarmed lane and pooled connection instead of
  handing user traffic to a peer that disagrees about protocol 1. A probe cut
  short by its own time budget remains an incomplete measurement, not a fault.
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
