# Known limitations

> [!NOTE]
> **Status:** Current limitations for public protocol 1
> **Last reviewed:** 2026-08-19

- Queqiao is a WAN optimization data plane, not an anonymity network. The
  desktop ingress is SOCKS5 and the native mobile apps provide a full-device
  VPN adapter; in both cases the provider observes destinations and traffic
  shape.
- High loss by itself does not prove Queqiao's erasure model applies. Queue
  overflow, bursty wireless contention, shaping, route capture, and independent
  erasure require different responses and must be distinguished.
- The non-TCP-friendly policy assumes an operator-controlled endpoint-pair
  segment. It may be inappropriate when the dominant bottleneck is a shared
  public resource outside the operator's authority.
- No complete multi-network protocol-1 field campaign has been published yet;
  performance on unmeasured paths is not guaranteed.
- The SOCKS listener has no password authentication and should remain on
  loopback or another access-controlled interface.
- Metrics have no authentication; bind them to loopback or protect them.
- Provider state is an online high-value secret. Queqiao does not yet integrate
  a hardware security module or operating-system keychain for issuer keys.
- The portable client profile contains the device private key. A GUI may store
  it in a platform keychain later, but the current file must remain mode 0600.
- The packaged provider topology uses one gateway endpoint. The paired data
  plane can serve a leg in a wider corporate or mesh overlay, but multi-gateway
  discovery, route exchange, load balancing, and seamless trust-domain
  migration are not implemented here.
- Automatic physical-source selection currently considers IPv4 addresses.
  Hosts with several active physical IPv4 interfaces must choose one with
  `--local-address if:NAME`; an IPv6-only uplink needs an explicit local IPv6
  address.
- Revocation is enforced at new TLS/stream authorization and by a one-second
  active-flow poll; it is deliberately not instantaneous packet revocation.
- Device renewal requires a still-valid, non-revoked identity. After expiry or
  profile loss, the user needs a new one-time invitation.
- UDP rescue preserves the gateway relay socket when reclamation succeeds but
  cannot recover datagrams in flight during path failure.
- Automatic TCP fallback cannot bypass a network that blocks both transports
  or the selected gateway port.
- `--allow-private-destinations` removes the default SSRF boundary and should
  be used only for an intentional private-access service.
- The desktop SOCKS listener is intentionally loopback-only and has no remote
  authentication. Use a separately authenticated access layer rather than
  exposing it directly to a LAN or public network.
- A trust-root/issuer compromise requires creating a new provider state and
  re-enrolling users; device revocation is insufficient.
- The mobile clients are full-tunnel VPNs. They do not currently offer split
  tunneling, custom DNS, or per-app routing; DNS uses Cloudflare through the
  encrypted Queqiao tunnel.
- Android always-on VPN is explicitly disabled pending physical-device locked
  boot and restart qualification.
- Apple App Store and Google Play VPN publication both require organization
  developer accounts under current store rules. iOS is source-build/self-sign
  only, and Android public builds must use a permitted direct-distribution path
  unless an organization assumes Google Play publication.
