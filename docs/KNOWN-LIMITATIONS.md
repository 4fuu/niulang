# Known limitations

- Queqiao is a performance-enhancing SOCKS proxy, not an anonymity network or
  a general-purpose VPN. The provider observes destinations and traffic shape.
- The SOCKS listener has no password authentication and should remain on
  loopback or another access-controlled interface.
- Metrics have no authentication; bind them to loopback or protect them.
- Provider state is an online high-value secret. Queqiao does not yet integrate
  a hardware security module or operating-system keychain for issuer keys.
- The portable client profile contains the device private key. A GUI may store
  it in a platform keychain later, but the current file must remain mode 0600.
- The default topology is one provider gateway endpoint. Multi-gateway
  discovery, load balancing, and seamless trust-domain migration are not yet
  implemented.
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
