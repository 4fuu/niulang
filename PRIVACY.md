# Queqiao mobile privacy

Last updated: August 18, 2026.

Queqiao is open-source software that connects a device to the Queqiao provider
selected by an enrollment invitation. The project maintainer does not operate
a default public VPN service. The operator who gives a user an invitation is
the user's VPN provider and is responsible for its own privacy notice, legal
basis, retention, and local-law obligations.

## Traffic handled while connected

The Android and iOS apps route all device IPv4, IPv6, and DNS traffic through
the selected Queqiao gateway. The app-to-gateway tunnel is encrypted and
mutually authenticated. After the gateway forwards traffic, protection to the
final destination depends on the application protocol: HTTPS remains encrypted
end-to-end, while an unencrypted protocol can be read by the gateway operator
and other networks after egress.

The gateway operator can necessarily observe destination addresses, connection
times, traffic sizes and timing, and any content not independently encrypted
end-to-end. DNS is sent through the tunnel to Cloudflare's public resolvers at
`1.1.1.1` and `2606:4700:4700::1111`; Cloudflare's separate privacy terms then
apply to those DNS requests.

## Identity data

Enrollment sends the chosen device name and a newly generated public key to
the invited provider. The provider retains the account and device identifiers,
public key, certificate status, and authorization state needed to authenticate,
limit, renew, or revoke the device. The private key never leaves the device.

Android stores the enrollment draft and profile using AES-256-GCM with a
non-exportable Android Keystore key, authenticates the storage account name,
and excludes all app data from backup and device transfer. iOS stores the same
material in a shared app/extension Keychain group using
`AfterFirstUnlockThisDeviceOnly` with iCloud synchronization disabled. Choosing
“Forget this device” removes the local profile and draft; the provider operator
must separately revoke or remove its server-side device record.

## Collection by the app

The mobile apps contain no advertising, analytics, tracking SDK, account
system, or crash-reporting service. They do not sell traffic data. Packet and
transport counters are aggregate and remain in process memory. Diagnostic
errors may be retained temporarily by the operating system's normal logging
facilities; Queqiao does not intentionally log packet payloads or private keys.

The apps contact only the provider endpoint encoded in the invitation/profile
and the DNS resolvers described above as part of their operation. The exact
linked open-source modules and license texts are available inside each app and
in `mobile/legal/THIRD_PARTY_NOTICES.txt`.

Security issues should be reported privately as described in `SECURITY.md`.
