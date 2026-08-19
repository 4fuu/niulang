import XCTest
import NetworkExtension
@testable import Queqiao

final class RoutePlanTests: XCTestCase {
    // MARK: parsing

    func testParsesIPv4AndIPv6AndClearsHostBits() {
        XCTAssertEqual(IPPrefix(cidr: "10.1.2.3/8")?.cidrText, "10.0.0.0/8")
        XCTAssertEqual(IPPrefix(cidr: "172.16.5.0/12")?.cidrText, "172.16.0.0/12")
        XCTAssertEqual(IPPrefix(cidr: "2001:250:1::5/34")?.cidrText, "2001:250::/34")
        XCTAssertEqual(IPPrefix(cidr: "fe80::abcd/10")?.cidrText, "fe80::/10")
    }

    func testABareAddressIsAHostRoute() {
        XCTAssertEqual(IPPrefix(cidr: "203.0.113.9")?.cidrText, "203.0.113.9/32")
        XCTAssertEqual(IPPrefix(cidr: "2606:4700:4700::1111")?.cidrText, "2606:4700:4700::1111/128")
    }

    func testRejectsMalformedInput() {
        for entry in [
            "",
            "not-an-address",
            "10.0.0.0/",
            "10.0.0.0/33",
            "10.0.0.0/-1",
            "10.0.0.0/8/8",
            "fc00::/129",
            "10.0.0.256/8",
            "10.0.0.0/eight"
        ] {
            XCTAssertNil(IPPrefix(cidr: entry), "accepted \(entry)")
        }
    }

    func testZeroLengthPrefixIsTheDefaultRoute() {
        XCTAssertEqual(IPPrefix(cidr: "8.8.8.8/0")?.cidrText, "0.0.0.0/0")
        XCTAssertEqual(IPPrefix(cidr: "2001:db8::/0")?.cidrText, "::/0")
    }

    // MARK: containment

    func testContainmentIsFamilyScoped() throws {
        let eight = try XCTUnwrap(IPPrefix(cidr: "10.0.0.0/8"))
        let sixteen = try XCTUnwrap(IPPrefix(cidr: "10.5.0.0/16"))
        let elsewhere = try XCTUnwrap(IPPrefix(cidr: "11.0.0.0/8"))
        let sixEight = try XCTUnwrap(IPPrefix(cidr: "fc00::/7"))

        XCTAssertTrue(eight.contains(sixteen))
        XCTAssertFalse(sixteen.contains(eight))
        XCTAssertTrue(eight.contains(eight))
        XCTAssertFalse(eight.contains(elsewhere))
        XCTAssertFalse(eight.contains(sixEight))
        XCTAssertTrue(eight.overlaps(sixteen))
        XCTAssertTrue(sixteen.overlaps(eight))
        XCTAssertFalse(eight.overlaps(elsewhere))
    }

    func testWideIPv6PrefixContainsDeepSubnet() throws {
        let seven = try XCTUnwrap(IPPrefix(cidr: "fc00::/7"))
        let deep = try XCTUnwrap(IPPrefix(cidr: "fd77:7171:6f::2/128"))
        XCTAssertTrue(seven.contains(deep))
    }

    // MARK: coalescing

    func testCoalesceAbsorbsSubnets() throws {
        let plan = IPPrefix.coalesce([
            try XCTUnwrap(IPPrefix(cidr: "10.5.0.0/16")),
            try XCTUnwrap(IPPrefix(cidr: "10.0.0.0/8")),
            try XCTUnwrap(IPPrefix(cidr: "10.5.6.7/32"))
        ])
        XCTAssertEqual(plan.map(\.cidrText), ["10.0.0.0/8"])
    }

    func testCoalesceMergesSiblingsRepeatedly() throws {
        let quarters = try ["0.0.0.0/2", "64.0.0.0/2", "128.0.0.0/2", "192.0.0.0/2"]
            .map { try XCTUnwrap(IPPrefix(cidr: $0)) }
        XCTAssertEqual(IPPrefix.coalesce(quarters).map(\.cidrText), ["0.0.0.0/0"])
    }

    func testCoalesceLeavesNonAdjacentBlocksAlone() throws {
        let blocks = try ["192.168.0.0/16", "10.0.0.0/8", "172.16.0.0/12"]
            .map { try XCTUnwrap(IPPrefix(cidr: $0)) }
        XCTAssertEqual(
            IPPrefix.coalesce(blocks).map(\.cidrText),
            ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"]
        )
    }

    func testCoalesceKeepsFamiliesSeparateAndSorted() throws {
        let mixed = try ["fc00::/8", "fd00::/8", "10.0.0.0/8"]
            .map { try XCTUnwrap(IPPrefix(cidr: $0)) }
        XCTAssertEqual(IPPrefix.coalesce(mixed).map(\.cidrText), ["10.0.0.0/8", "fc00::/7"])
    }

    func testCoalesceOfDuplicatesIsIdempotent() throws {
        let one = try XCTUnwrap(IPPrefix(cidr: "203.0.113.0/24"))
        XCTAssertEqual(IPPrefix.coalesce([one, one, one]).map(\.cidrText), ["203.0.113.0/24"])
    }

    // MARK: subtraction, the Android RoutePolicy mirror

    func testSubtractingCarvesAHoleAndCoversTheRest() throws {
        let everything = try XCTUnwrap(IPPrefix(cidr: "0.0.0.0/0"))
        let hole = try XCTUnwrap(IPPrefix(cidr: "10.0.0.0/8"))
        let remainder = everything.subtracting(hole)

        // Eight halvings of the address space, one per prefix bit consumed.
        XCTAssertEqual(remainder.count, 8)
        XCTAssertFalse(remainder.contains { $0.overlaps(hole) })
        for address in ["9.255.255.255", "11.0.0.0", "8.8.8.8", "192.168.1.1"] {
            let host = try XCTUnwrap(IPPrefix(cidr: address))
            XCTAssertTrue(remainder.contains { $0.contains(host) }, "\(address) lost its route")
        }
        for address in ["10.0.0.0", "10.255.255.255", "10.77.0.2"] {
            let host = try XCTUnwrap(IPPrefix(cidr: address))
            XCTAssertFalse(remainder.contains { $0.contains(host) }, "\(address) stayed routed")
        }
    }

    func testSubtractingTheWholeBlockLeavesNothing() throws {
        let block = try XCTUnwrap(IPPrefix(cidr: "10.0.0.0/8"))
        XCTAssertTrue(block.subtracting(block).isEmpty)
        let wider = try XCTUnwrap(IPPrefix(cidr: "10.0.0.0/7"))
        XCTAssertTrue(block.subtracting(wider).isEmpty)
    }

    func testSubtractingADisjointBlockChangesNothing() throws {
        let block = try XCTUnwrap(IPPrefix(cidr: "10.0.0.0/8"))
        let other = try XCTUnwrap(IPPrefix(cidr: "192.168.0.0/16"))
        let mismatchedFamily = try XCTUnwrap(IPPrefix(cidr: "fc00::/7"))
        XCTAssertEqual(block.subtracting(other), [block])
        XCTAssertEqual(block.subtracting(mismatchedFamily), [block])
    }

    func testSubtractingEveryLocalNetworkStillRoutesThePublicInternet() throws {
        var remainder = [try XCTUnwrap(IPPrefix(cidr: "0.0.0.0/0"))]
        for entry in RoutePlan.localNetworks {
            let exclusion = try XCTUnwrap(IPPrefix(cidr: entry))
            remainder = remainder.flatMap { $0.subtracting(exclusion) }
        }
        for address in ["1.1.1.1", "8.8.8.8", "203.0.113.10"] {
            let host = try XCTUnwrap(IPPrefix(cidr: address))
            XCTAssertTrue(remainder.contains { $0.contains(host) }, "\(address) lost its route")
        }
        for address in ["10.1.2.3", "127.0.0.1", "169.254.1.1", "172.20.0.1", "192.168.1.1", "100.64.0.1"] {
            let host = try XCTUnwrap(IPPrefix(cidr: address))
            XCTAssertFalse(remainder.contains { $0.contains(host) }, "\(address) stayed routed")
        }
    }

    // MARK: plans

    func testLocalNetworkPlanReproducesTheShippedRoutes() {
        let plan = RoutePlan.localNetworkPlan()

        XCTAssertTrue(plan.rejected.isEmpty)
        XCTAssertEqual(plan.truncated, 0)
        XCTAssertEqual(
            plan.ipv4Routes.map { [$0.destinationAddress, $0.destinationSubnetMask] },
            [
                ["10.0.0.0", "255.0.0.0"],
                ["100.64.0.0", "255.192.0.0"],
                ["127.0.0.0", "255.0.0.0"],
                ["169.254.0.0", "255.255.0.0"],
                ["172.16.0.0", "255.240.0.0"],
                ["192.168.0.0", "255.255.0.0"]
            ]
        )
        XCTAssertEqual(
            plan.ipv6Routes.map { [$0.destinationAddress, $0.destinationNetworkPrefixLength.stringValue] },
            [["::1", "128"], ["fc00::", "7"], ["fe80::", "10"]]
        )
    }

    func testInvalidUserEntriesAreReportedRatherThanDropped() {
        let plan = RoutePlan.make(userRoutes: ["10.0.0.0/8", "   ", "nonsense", "1.2.3.4/40"])

        XCTAssertEqual(plan.excluded.map(\.cidrText), ["10.0.0.0/8"])
        XCTAssertEqual(plan.rejected, ["nonsense", "1.2.3.4/40"])
        XCTAssertEqual(plan.truncated, 0)
        XCTAssertTrue(plan.diagnosticSummary.contains("2 rejected as invalid"))
    }

    func testUserRoutesAreCoalescedBeforeTheLimitApplies() {
        let plan = RoutePlan.make(userRoutes: ["10.0.0.0/9", "10.128.0.0/9"], limit: 1)

        XCTAssertEqual(plan.excluded.map(\.cidrText), ["10.0.0.0/8"])
        XCTAssertEqual(plan.truncated, 0)
    }

    func testTheLimitDropsTheNarrowestBlocksAndSaysSo() {
        let plan = RoutePlan.make(
            userRoutes: ["203.0.113.1/32", "10.0.0.0/8", "192.168.0.0/16"],
            limit: 2
        )

        XCTAssertEqual(plan.excluded.map(\.cidrText), ["10.0.0.0/8", "192.168.0.0/16"])
        XCTAssertEqual(plan.truncated, 1)
        XCTAssertTrue(plan.diagnosticSummary.contains("1 dropped at the route limit"))
    }

    // MARK: the whole-family footgun

    func testAZeroLengthEntryIsReportedRatherThanDroppedOrIgnored() {
        // Legal, occasionally deliberate, and almost never meant: the tunnel
        // connects and then carries nothing. RoutePlan keeps it and says so;
        // silently dropping it would override someone who meant it, and
        // silently keeping it would look like a broken gateway.
        let plan = RoutePlan.make(userRoutes: ["0.0.0.0/0"])
        XCTAssertTrue(plan.excludesDefaultRoute)
        XCTAssertEqual(plan.excluded.map(\.cidrText), ["0.0.0.0/0"])

        XCTAssertTrue(RoutePlan.make(userRoutes: ["::/0"]).excludesDefaultRoute)
        XCTAssertTrue(RoutePlan.make(userRoutes: ["10.0.0.0/8", "::/0"]).excludesDefaultRoute)
    }

    func testAnOrdinaryPlanDoesNotClaimToCoverAFamily() {
        XCTAssertFalse(RoutePlan.make(userRoutes: ["10.0.0.0/8", "2001:db8::/32"]).excludesDefaultRoute)
        XCTAssertFalse(RoutePlan.localNetworkPlan().excludesDefaultRoute)
        XCTAssertFalse(RoutePlan.empty.excludesDefaultRoute)
    }

    func testUserRoutesKeepTheirPlaceWhenTheBuiltInSetIsTooLarge() throws {
        // Spaced two apart so no pair is a sibling: the point of this test is
        // the limit, not coalescing.
        let builtIn = try (0..<8).map { index in
            try XCTUnwrap(IPPrefix(cidr: "198.51.\(index * 2).0/24"))
        }
        let plan = RoutePlan.make(userRoutes: ["203.0.113.0/24"], builtIn: builtIn, limit: 3)

        XCTAssertTrue(plan.excluded.contains { $0.cidrText == "203.0.113.0/24" })
        XCTAssertEqual(plan.excluded.count, 3)
        XCTAssertEqual(plan.truncated, 6)
    }

    func testAFullUserListLeavesNoRoomForTheBuiltInSet() throws {
        let builtIn = [try XCTUnwrap(IPPrefix(cidr: "198.51.100.0/24"))]
        let plan = RoutePlan.make(userRoutes: ["203.0.113.0/24"], builtIn: builtIn, limit: 1)

        XCTAssertEqual(plan.excluded.map(\.cidrText), ["203.0.113.0/24"])
        XCTAssertEqual(plan.truncated, 1)
    }

    func testAnEmptyPlanProducesNoRoutes() {
        let plan = RoutePlan.make(userRoutes: [])

        XCTAssertTrue(plan.ipv4Routes.isEmpty)
        XCTAssertTrue(plan.ipv6Routes.isEmpty)
        XCTAssertEqual(plan.diagnosticSummary, "0 bypass routes")
    }
}
