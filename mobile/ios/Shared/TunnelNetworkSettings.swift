import Foundation
import NetworkExtension

/// Builds the interface description iOS installs for the tunnel.
///
/// This lives in Shared rather than beside the provider so it can be tested at
/// all: project.yml puts Shared in the test target and PacketTunnel in neither.
/// Every value here is a policy decision that shows up as behaviour on a
/// device — the MTU that bounds every packet, the resolver every name goes to,
/// the addresses the interface answers on — and none of it was covered while it
/// sat inline in a NetworkExtension subclass.
enum TunnelNetworkSettings {
    /// Sized for the QUIC uplink rather than for the local link: the tunnel's
    /// packets are carried inside the transport, and 1280 is the IPv6 minimum
    /// every path must accept without fragmenting.
    ///
    /// Int64 because the core takes it as one: the interface iOS installs and
    /// the interface the packet engine believes it has must be the same number,
    /// and separate literals in two files would eventually stop being.
    static let mtu: Int64 = 1_280

    /// The interface's own addresses. Both are single-host, because nothing is
    /// reachable *on* the tunnel interface — it is a funnel, not a subnet — and
    /// the ULA and RFC 1918 blocks chosen are unlikely to collide with a
    /// network the device is also attached to.
    static let ipv4Address = "10.77.0.2"
    static let ipv6Address = "fd77:7171:6f::2"

    /// Names resolve through the tunnel, so a lookup cannot leak the
    /// destination to the local network's resolver. The cost is stated plainly
    /// in the routing UI: resolving from the gateway's vantage point is what
    /// makes an address-based bypass list approximate.
    static let dnsServers = ["1.1.1.1", "2606:4700:4700::1111"]

    /// Builds settings that send everything into the tunnel except the plan's
    /// exclusions.
    static func make(plan: RoutePlan, remoteAddress: String) -> NEPacketTunnelNetworkSettings {
        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: remoteAddress)
        settings.mtu = NSNumber(value: mtu)

        let ipv4 = NEIPv4Settings(addresses: [ipv4Address], subnetMasks: ["255.255.255.255"])
        ipv4.includedRoutes = [.default()]
        let ipv4Excluded = plan.ipv4Routes
        if !ipv4Excluded.isEmpty {
            ipv4.excludedRoutes = ipv4Excluded
        }
        settings.ipv4Settings = ipv4

        let ipv6 = NEIPv6Settings(addresses: [ipv6Address], networkPrefixLengths: [128])
        ipv6.includedRoutes = [.default()]
        let ipv6Excluded = plan.ipv6Routes
        if !ipv6Excluded.isEmpty {
            ipv6.excludedRoutes = ipv6Excluded
        }
        settings.ipv6Settings = ipv6

        // An empty match domain is the wildcard: every query, not just the ones
        // for a listed suffix, goes to the servers above.
        let dns = NEDNSSettings(servers: dnsServers)
        dns.matchDomains = [""]
        settings.dnsSettings = dns
        return settings
    }
}
