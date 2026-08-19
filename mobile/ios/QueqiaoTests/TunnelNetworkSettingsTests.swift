import XCTest
import NetworkExtension
@testable import Queqiao

/// The interface description iOS installs for the tunnel.
///
/// These values were literals inside the packet-tunnel provider until they were
/// lifted here, which means they shipped uncovered: a wrong MTU fragments every
/// flow, a wrong match domain leaks every lookup to the local resolver, and an
/// exclusion filed under the wrong family is simply not applied. None of those
/// announce themselves.
final class TunnelNetworkSettingsTests: XCTestCase {
    private func settings(_ userRoutes: [String] = []) -> NEPacketTunnelNetworkSettings {
        TunnelNetworkSettings.make(
            plan: RoutePlan.make(userRoutes: userRoutes),
            remoteAddress: "198.51.100.7"
        )
    }

    func testTheTunnelCarriesBothFamiliesByDefault() throws {
        let built = settings()
        XCTAssertEqual(built.tunnelRemoteAddress, "198.51.100.7")
        XCTAssertEqual(built.mtu, NSNumber(value: TunnelNetworkSettings.mtu))

        let ipv4 = try XCTUnwrap(built.ipv4Settings)
        XCTAssertEqual(ipv4.addresses, [TunnelNetworkSettings.ipv4Address])
        XCTAssertEqual(ipv4.includedRoutes?.count, 1)
        let ipv6 = try XCTUnwrap(built.ipv6Settings)
        XCTAssertEqual(ipv6.addresses, [TunnelNetworkSettings.ipv6Address])
        XCTAssertEqual(ipv6.includedRoutes?.count, 1)
    }

    func testTheInterfaceAddressesAreSingleHosts() throws {
        // The tunnel is a funnel, not a subnet: a wider mask would claim
        // neighbouring addresses are on-link and reachable without the gateway.
        XCTAssertEqual(try XCTUnwrap(settings().ipv4Settings).subnetMasks, ["255.255.255.255"])
        XCTAssertEqual(try XCTUnwrap(settings().ipv6Settings).networkPrefixLengths, [128])
    }

    func testEveryLookupGoesThroughTheTunnelResolvers() throws {
        let dns = try XCTUnwrap(settings().dnsSettings)
        XCTAssertEqual(dns.servers, TunnelNetworkSettings.dnsServers)
        // The empty string is the wildcard. A non-empty list here would send
        // every unlisted suffix to whatever resolver the local network offers,
        // which is the leak the tunnel exists to close.
        XCTAssertEqual(dns.matchDomains, [""])
    }

    // MARK: exclusions

    func testExclusionsAreFiledUnderTheFamilyTheyBelongTo() throws {
        let built = settings(["203.0.113.0/24", "2001:db8::/32"])
        let ipv4 = try XCTUnwrap(built.ipv4Settings.flatMap(\.excludedRoutes))
        let ipv6 = try XCTUnwrap(built.ipv6Settings.flatMap(\.excludedRoutes))
        XCTAssertEqual(ipv4.map(\.destinationAddress), ["203.0.113.0"])
        XCTAssertEqual(ipv4.map(\.destinationSubnetMask), ["255.255.255.0"])
        XCTAssertEqual(ipv6.map(\.destinationAddress), ["2001:db8::"])
        XCTAssertEqual(ipv6.map(\.destinationNetworkPrefixLength), [NSNumber(value: 32)])
    }

    func testAFamilyWithNoExclusionsIsLeftAloneRatherThanSetToAnEmptyList() throws {
        // NEIPv4Settings.excludedRoutes distinguishes "nothing excluded" from
        // "an empty list supplied"; assigning the latter is a change in what is
        // being asked for, so an all-IPv4 plan must not touch IPv6.
        let built = settings(["203.0.113.0/24"])
        XCTAssertEqual(try XCTUnwrap(built.ipv4Settings?.excludedRoutes).count, 1)
        XCTAssertNil(built.ipv6Settings?.excludedRoutes)
    }

    func testTheLocalNetworkPolicyExcludesPrivateSpaceInBothFamilies() throws {
        let built = TunnelNetworkSettings.make(
            plan: RoutePlan.localNetworkPlan(),
            remoteAddress: "198.51.100.7"
        )
        let ipv4 = try XCTUnwrap(built.ipv4Settings.flatMap(\.excludedRoutes))
        let ipv6 = try XCTUnwrap(built.ipv6Settings.flatMap(\.excludedRoutes))
        XCTAssertTrue(ipv4.contains { $0.destinationAddress == "192.168.0.0" })
        XCTAssertTrue(ipv6.contains { $0.destinationAddress == "fe80::" })
        XCTAssertEqual(ipv4.count + ipv6.count, RoutePlan.localNetworks.count)
    }
}
