# Mobile clients

Queqiao has native Android and iOS applications backed by one shared Go core.
These are full-device packet tunnels, not wrappers around the desktop user
interface. The Android app uses `VpnService`; the iOS app and its packet-tunnel
extension use public Network Extension APIs, including `NEPacketTunnelFlow`.
No private iOS API is used.

## Distribution constraints

These constraints are store policy, not a technical restriction in the source:

- [Apple App Review Guideline 5.4](https://developer.apple.com/app-store/review/guidelines/#vpn-apps)
  requires a VPN app to use the appropriate Network Extension API and says it
  may only be offered by a developer enrolled as an organization. An
  Individual Apple Developer Program membership can sign a development build
  onto that developer's registered devices, but it does not make the app
  eligible for public App Store publication.
- [Google Play Console requirements](https://support.google.com/googleplay/android-developer/answer/10788890)
  require developers of apps approved to use `VpnService` to register as an
  Organization. A personal Play account is therefore not a publication route
  for Queqiao.
- Android distribution outside Google Play is a separate system. The
  [Android Developer Console](https://developer.android.com/developer-verification)
  supports verified direct distribution, including personal accounts. Its
  Limited Distribution plan is capped at 20 authorized devices; Full
  Distribution supports wider installation. Regional verification begins on
  September 30, 2026 and expands globally in 2027, so release owners must
  re-check the current rules before publishing.

The project consequently produces Android APK/AAB artifacts suitable for
testing and permitted direct distribution, while iOS remains source-build and
self-sign only until an organization deliberately assumes store, privacy, and
support responsibility. Store policy can change; the linked primary sources
are authoritative.

## Functional parity

| Capability | Desktop | Android | iOS |
| --- | --- | --- | --- |
| One-time `queqiao://` enrollment | Yes | Yes | Yes |
| Crash-safe enrollment draft | Mode-0600 file | Keystore-encrypted | This-device-only Keychain |
| TLS 1.3 mutual authentication and root pin | Yes | Same core | Same core |
| Hourly certificate maintenance | Yes | Yes | Yes |
| QUIC with TLS/TCP fallback | Yes | Same core | Same core |
| SOCKS TCP and UDP | Ingress API | Internal adapter | Internal adapter |
| Full IPv4 and IPv6 tunnel | Via external TUN client | Native | Native |
| Bounded sessions and packet queues | Yes | Yes | Yes |
| Aggregate in-memory metrics | Yes | Yes | Yes |
| Multiple device-bound provider profiles | N/A (one profile per process) | Yes | Yes |
| Explicit selected-profile choice | N/A (CLI profile argument) | Yes | Yes |
| Authenticated per-profile reachability and latency test | No | Yes | Yes |
| Full-tunnel and local-network bypass policies | External TUN policy | Native | Native |

## Mobile product model

The mobile applications are organized as VPN clients rather than enrollment
forms:

- **Home** owns the connection state and the single Connect/Disconnect action.
  It shows the selected provider profile, traffic policy, enrolled device name,
  and aggregate per-connection transfer and flow counters. “Selected” means the
  profile that the next Connect action will use; it never means the VPN tunnel
  is connected. “Active device” is status information; it is not an action.
- **Profiles** is a multi-profile library. Importing another invitation adds a
  profile instead of overwriting the current one. Users can select, rename,
  inspect, test, change the route policy for, and delete profiles. “Test all
  connections” runs at most four iOS probes concurrently and runs Android
  probes serially on its bounded application worker. Each probe measures DNS,
  transport setup, mutual TLS, current device authorization, Queqiao protocol
  negotiation, and one authenticated control round trip. It opens no remote
  destination. Selection, testing, and routing changes require the tunnel to
  be disconnected so displayed state cannot diverge from the running
  extension or service or be measured through another active VPN.
- **Settings** contains stable privacy, key-storage, version, system VPN, and
  license information rather than connection controls. Its encrypted
  connection-log ring records named iOS stop reasons and the system's last
  disconnect error, and users can share a sanitized text copy from production
  builds. The app reloads its saved VPN manager after configuration changes;
  an unloaded manager is shown as loading rather than as a false disconnect.

Both apps accept a `queqiao://` invitation opened from another application.
iOS also offers an explicit paste action; Android accepts both a link and a
shared plain-text invitation. Enrollment remains crash-safe: a draft containing
the newly generated device key is encrypted before the one-time token is sent,
and an interrupted import resumes that exact draft.

Queqiao does not import or export the resulting private client-profile JSON as
a portable configuration file. That JSON contains the device identity and is
intentionally stored only in this-device-only Keychain storage on iOS or an
Android-Keystore-encrypted envelope excluded from backup. The portable input is
the provider-issued one-time invitation. Deleting a profile therefore requires
a new invitation, consistent with the desktop identity model.

Each profile has one of two policies:

- **All traffic** routes IPv4, IPv6, and DNS through Queqiao.
- **Exclude local networks** keeps IPv4 private, shared-address, loopback, and
  link-local destinations plus IPv6 unique-local, loopback, and link-local
  destinations outside the tunnel. Internet and DNS traffic still use
  Queqiao. iOS expresses these as excluded Network Extension routes. Android
  constructs the exact complement as included CIDR routes so behavior is the
  same on every supported API level, including releases before Android added
  `VpnService.Builder.excludeRoute`.

The encrypted catalog stores only profile metadata, selection, and policy;
each private profile remains a separate encrypted record. Existing single-
profile installations migrate in place on first launch. The packet-tunnel
extension and VPN service receive an explicit profile identifier when starting,
and automatic certificate renewal writes back only to that identity.

The apps deliberately do not expose experimental transport tuning in their
primary UI. They use the reviewed desktop defaults. Both install an MTU of
1280 and send DNS through the Queqiao tunnel to Cloudflare's `1.1.1.1` and
`2606:4700:4700::1111` resolvers. The default policy routes all IPv4 and IPv6
traffic; users may explicitly bypass local networks per profile. Always-on mode
is disabled on Android until restart and locked-device behavior has completed
the physical-device qualification matrix.

## Dependency policy

The packet adapter, SOCKS5 CONNECT/UDP ASSOCIATE implementation, lifecycle,
storage integration, and platform UI are maintained in this repository. The
only non-Queqiao runtime networking foundation added for mobile is the actively
maintained Apache-2.0 gVisor netstack; it supplies TCP/IP state machines, not a
proxy protocol or application. Android UI uses only the platform SDK, and iOS
uses only Apple system frameworks.

Every linked Go module is pinned in `mobile/runtime-dependencies.lock`, limited
to MIT, BSD-3-Clause, or Apache-2.0, and checked from the compiled package graph
by `mobile/scripts/audit-dependencies.sh` and from the built AAR/XCFramework by
`mobile/scripts/audit-mobile-binary.sh`. The x/mobile binding support that
gomobile links is included in the runtime lock and notices. The gomobile/gobind
command graph and Android build tools are pinned separately in
`mobile/build-tools.lock`; compiler dependencies such as x/mod, x/sync, and
x/tools are build-only and are not linked into the apps.
Gradle's complete downloaded plugin graph is SHA-256 pinned in
`mobile/android/gradle/verification-metadata.xml` for macOS, Linux, and Windows
host tools.
`mobile/legal/THIRD_PARTY_NOTICES.txt` is deterministically regenerated
from the exact module license files and embedded in both apps. A dependency
change must update the reviewed lock and notices in the same commit.

The mobile-specific maintenance review was refreshed on August 18, 2026.
The pinned [gVisor](https://github.com/google/gvisor) snapshot and
[Go mobile](https://go.googlesource.com/mobile) binding tools both have active
upstream development. Go mobile nevertheless describes itself as experimental
and provides no end-user support guarantee. Queqiao therefore treats the
generated binding as a replaceable boundary, audits the result rather than
trusting the generator, and makes upstream abandonment or an unpatched security
issue a release blocker. No third-party UI, VPN product, analytics SDK, or
general-purpose proxy application is embedded.

## Build the shared core

Both platforms require Go 1.26.6. The scripts select that patched toolchain,
install the exact pinned gomobile
tools into a temporary directory, verify both module graphs, run the core race
suite, regenerate license notices, remove Go debug tables, audit the linked
module graph, reject local checkout-path leakage, and replace the platform
framework only after a successful build.

Android prerequisites are JDK 17, Android SDK Platform 37, Android Build Tools
36, and NDK `28.0.12433566`:

```sh
export ANDROID_HOME=/absolute/path/to/Android/sdk
mobile/scripts/build-android-core.sh
cd mobile/android
./gradlew --dependency-verification strict \
  lintDebug lintRelease assembleDebug assembleDebugAndroidTest bundleRelease
```

Run the Keystore instrumentation suite on an API 30 or later emulator/device:

```sh
./gradlew --dependency-verification strict connectedDebugAndroidTest
```

The default release output is unsigned. For a directly distributable build,
use a dedicated long-lived release key and set all four variables:

```sh
export QUEQIAO_ANDROID_STORE_FILE=/absolute/path/to/release.jks
export QUEQIAO_ANDROID_STORE_PASSWORD='...'
export QUEQIAO_ANDROID_KEY_ALIAS='...'
export QUEQIAO_ANDROID_KEY_PASSWORD='...'
./gradlew --dependency-verification strict \
  -PqueqiaoVersionCode=1 -PqueqiaoVersionName=0.1.0 \
  bundleRelease assembleRelease
```

Never commit a keystore or password. Keep an offline backup: losing the key
prevents trustworthy updates. Register the final package name and signing
certificate through the applicable Android distribution console before a wide
release. Google Play additionally requires its `VpnService` declaration,
privacy policy, Data safety answers, and an Organization account.

## Build iOS for a physical device

Prerequisites are Xcode 26 or later, Go 1.26.6, and a paid Apple Developer
Program membership with the Network Extension capability available to the
selected team:

```sh
mobile/scripts/build-ios-core.sh
open mobile/ios/Queqiao.xcodeproj
```

In Xcode Build Settings, set these user-defined values to identifiers owned by
your team for all configurations:

- `QUEQIAO_APP_BUNDLE_ID`
- `QUEQIAO_EXTENSION_BUNDLE_ID` (normally the app ID plus `.PacketTunnel`)
- `QUEQIAO_KEYCHAIN_SUFFIX` (normally the app ID plus `.shared`)

Select the same development team for `Queqiao` and `PacketTunnel`, retain the
Network Extensions and Keychain Sharing capabilities, connect the registered
iPhone, and run the `Queqiao` scheme. Accept the system VPN configuration
prompt on the phone. A different developer repeats these steps with their own
paid membership, team, and bundle identifiers. There is no unsigned IPA that
can be installed randomly on arbitrary iPhones.

`project.yml` is the declarative project source. XcodeGen is optional and is
needed only when changing project structure; ordinary self-builds use the
committed `.xcodeproj`. If the project is regenerated, review the resulting
project diff and re-run both simulator and device qualification.

The portable boundary suite can run without signing:

```sh
cd mobile/ios
xcodebuild -project Queqiao.xcodeproj -scheme Queqiao \
  -destination 'platform=iOS Simulator,name=iPhone 17' \
  -derivedDataPath DerivedData CODE_SIGNING_ALLOWED=NO test
swiftlint lint --strict --config .swiftlint.yml .
```

Simulator success proves compilation and app/core boundary behavior only. A
simulator cannot qualify the real packet-tunnel lifecycle, signing,
Wi-Fi/cellular switching, sleep/wake, revocation, or sustained resource use.

## Release qualification

Do not label a mobile artifact production-ready until all unchecked mobile
items in `docs/RELEASE-CHECKLIST.md` are complete on the exact release commit.
At minimum this includes physical-device TCP/UDP and IPv4/IPv6 traffic, DNS,
QUIC-to-TCP fallback, certificate renewal, revocation, suspend/resume,
Wi-Fi/cellular transitions, bounded 24-hour load, clean install/update/rollback,
store/direct-distribution declarations, and independent security review.
