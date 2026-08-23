# Changelog

All notable user-visible changes are recorded here. Queqiao uses semantic
versioning for release artifacts, while the pre-1.0 wire-compatibility rules
remain stricter than semantic versioning alone; see `docs/PROTOCOL.md`.

## Unreleased

### Added

- A per-account concurrent-device limit, `provider add-user --max-clients`,
  defaulting to 8. A device counts once however many flows it carries, so this
  is the limit that expresses "this account is for eight devices" -- which is
  what the per-account limit was widely assumed to already mean.
- `provider set-user-limits`, which corrects an account's limits in place. The
  only previous way to change them was to delete the account, which deletes
  every device enrolled against it.
- `queqiao_account_admission_refused_total` by reason, and an `account flow
  open refused` log record at `warn` naming the reason, account, and device.
  Account admission was previously the one admission decision the gateway made
  silently: no record at any log level and no counter, so a gateway refusing
  half an account's connections was indistinguishable from a healthy one.

### Changed

- `provider add-user --max-sessions` is renamed `--max-flows`, and now defaults
  to 1024 instead of deferring to the gateway ceiling. It counts concurrent
  flows -- one TCP connection or one UDP association each -- and never counted
  devices. A browser opens roughly six connections per host across dozens of
  hosts per page and holds them for the flow idle timeout, so a value chosen as
  though it were a device count fails in the least legible way available: most
  sites load and a few do not. The old flag name still works and warns.
  Existing accounts keep the limit they were given; correct one with
  `provider set-user-limits`.
- A flow open refused by an account limit now says which limit refused it:
  `account flow limit reached` or `account device limit reached`, replacing
  `account session unavailable`. A device that lost authorization between its
  handshake and its next flow open is answered with the AUTHENTICATION reset
  code and `device is not authorized` rather than being reported as a limit.
- `provider list-users` reports `MAX_FLOWS` and `MAX_CLIENTS` in place of
  `MAX_SESSIONS`.
- Provider authorization state writes the per-account flow limit as
  `max_flows`. An existing state naming it `max_sessions` is read unchanged and
  rewritten on the next save; a state naming both is refused rather than having
  one silently win.

### Fixed

- Provider state keeps its owner when a maintenance command runs under `sudo`.
  State files are installed by renaming a replacement over the target, and the
  replacement belonged to whoever ran the command, so one privileged
  `queqiaod provider add-user` transferred `authorization.json` to `root` and
  left the gateway's own service account unable to open it. The gateway then
  logged `authorization refresh failed; retaining last known-good state` once a
  second, kept admitting existing devices from its cached snapshot, and refused
  every new enrollment - which surfaced to users as an invalid invitation
  rather than as the server-side outage it was. Replacements now adopt the
  owner of the file they replace, or of the state directory when the file is
  new. The authorization lock is covered too: it is taken before the store is
  read and is never removed, so a lock left behind by a privileged run refused
  every later write.
- Enrollment refusals say which of the two possible things went wrong, and say
  it where an operator can see it. Every failure used to collapse into one
  sentence sent only to the client - `invitation is invalid, expired, already
  used, or unavailable` - covering both a genuinely spent invitation and a
  gateway that could not open its own authorization store. Nothing was logged
  server-side for a refused or an accepted enrollment at any level, and the
  gateway discarded the result at every call site, so a gateway refusing every
  enrollment it received looked exactly like one receiving none. A store that
  cannot be read, locked, or written is now reported as a temporary
  unavailability, recorded at error level, and never described to the client as
  a verdict on their invitation; a real refusal is recorded at warning level;
  an acceptance is recorded with the account and device it created. Records are
  rate-limited per outcome and carry the suppressed and total counts, so a
  storm stays one readable line. Enrollment attempts dropped because the
  enrollment slots were full are also reported now, matching the session and
  connection ceilings beside them.
- The deployment guide's worked example created an account with a flow ceiling
  of 8, which is too low to load a web page.

## v0.1.0 - 2026-08-19

### Added

- Native iOS and Android applications with connection-first home screens,
  secure multi-profile invitation imports, explicit active-profile selection,
  profile management, and explicit paste/share imports.
- Live aggregate session counters and per-profile all-traffic or
  local-network-bypass routing policies in the iOS packet-tunnel app.
- Android export mode, which is what the released Android app now is: it
  enrolls the device and serves the gateway as an authenticated SOCKS5
  endpoint on loopback for v2rayNG, mihomo, sing-box, or any other client that
  already owns the device tunnel, with per-install credentials in the
  Keystore-backed store, a changeable port, in-app setup snippets, and the
  per-app exclusion step stated first because skipping it loops the uplink.
  Routing rules, per-app policy, and DNS stay with that client rather than
  being reimplemented here. See `docs/ANDROID-EXPORT.md`.
- Detection of the one setup mistake Android export mode cannot survive: if
  the consumer's tunnel is carrying Queqiao's own uplink, the notification and
  the connection test name it instead of leaving a silent loop to time out.
  The check reads the app's own default network, which Android reports per-UID,
  and is advisory — it never blocks a connection.
- RFC 1929 username/password authentication for the SOCKS5 listener, required
  in Android export mode because loopback is shared with every other app on
  the device. The desktop listener's behavior is unchanged.
- Per-profile iOS bypass routes: hand-entered CIDR blocks kept off the tunnel,
  refused rather than silently dropped when they do not parse. A list that
  takes an entire address family off the tunnel is legal and left alone, but
  said out loud in the routing screen and in the extension's diagnostics,
  because otherwise it looks exactly like a broken gateway.
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
- `deploy/install-server.sh` and `deploy/install-client.sh`, which perform the
  gateway and desktop deployments the guide previously spelled out by hand.
  The server script installs the binary, service account, directories, unit,
  and environment file, initializes the provider, creates the first user, and
  prints one invitation only after the running gateway has been verified. It
  refuses to run over an existing provider state unless `--no-provider-init`
  says the trust root is being kept, because replacing a root strands every
  enrolled device. The client script enrolls one or more invitations, writes
  the multi-provider manifest, installs a per-user service that starts at
  login -- a LaunchAgent on macOS, a lingering systemd `--user` unit on Linux,
  which had no supervisor template at all -- and verifies each SOCKS5 listener
  end to end. Re-running it with a new invitation adds a provider and keeps the
  existing entries and their ports, and changing `--config-dir`, `--prefix`,
  `--label`, or `--service-name` relocates an install rather than starting a
  second one beside it: the profiles move with it, because an invitation is
  single-use and a profile left behind is a device that cannot be reissued.
- `queqiaod service install`, `status`, `print`, and `uninstall`. The client
  service is now generated by the binary from this machine's real paths, on
  macOS and Linux, so enrolling by hand no longer means hand-writing a service
  definition: `queqiaod enroll` prints the exact `service install` line to run
  next.

### Changed

- The client's default SOCKS5 listener moved from `127.0.0.1:1080` to
  `127.0.0.1:12080`, the port `deploy/clash-queqiao.yaml` and the deployment
  guide already point Clash at. The old default disagreed with the profile
  shipped beside it, so an unconfigured start routed nowhere. Deployments that
  pass `--listen` explicitly are unaffected.
- `deploy/me.01.queqiao.client.plist` is removed. It was a macOS-only template
  whose `/Users/YOU/.config/queqiao/PROVIDER-ID.json` placeholder pointed at a
  path `queqiaod enroll` never writes on macOS, where the profile goes to
  `~/Library/Application Support/queqiao/` instead; its header also recommended
  `launchctl load` and `kickstart`, which the guide correctly warned against.
  `queqiaod service print` renders the same file with real paths.

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

- The provider unit now grants `CAP_NET_BIND_SERVICE`. `deploy/queqiaod.service`
  runs as an unprivileged account with `NoNewPrivileges=true`, so the `--listen
  :443` the guide recommends could not bind: the capability has to be granted
  at exec because nothing may acquire it afterwards. The capability bounding
  set is now that one capability rather than the full set.
- SOCKS5 UDP associations survive an unreachable destination. Sending to a
  closed UDP port draws an ICMP port-unreachable that the host reports to the
  sender on a later read, and Windows reports it even for an unconnected
  socket; the relay loops treated that as fatal, so one destination going away
  ended the whole association and every other destination it carried. Nothing
  errored -- traffic simply stopped -- which is why it took a Windows CI
  runner to find. The measurement server in `cmd/pathprobe` had the same gap.
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

## Release scope

The first public preview is a paired fixed-egress deployment:

- Authenticated TLS/QUIC transport with TLS/TCP fallback.
- Fixed-egress SOCKS5 TCP CONNECT and UDP ASSOCIATE.
- QUIC DATAGRAM carriage for UDP, bounded recovery, and UDP relay-source
  preservation during TCP rescue.
- PIAS-inspired flow classification and reactive bulk-flow isolation.
- Deterministic archives for Linux, macOS, and Windows on amd64 and arm64.

Publication gates are recorded in
[`docs/RELEASE-CHECKLIST.md`](docs/RELEASE-CHECKLIST.md).
