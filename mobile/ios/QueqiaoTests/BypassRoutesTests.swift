import XCTest
@testable import Queqiao

/// The bypass list is persisted inside the Keychain catalog, which is the one
/// blob every enrolled profile lives in. A decode failure there is not a lost
/// setting, it is a lost device identity — so these tests are as much about
/// migration as about routing.
final class BypassRoutesTests: XCTestCase {
    private func makeProfile(
        bypassRoutes: [String] = [],
        bypassChinaDirect: Bool = false
    ) -> StoredProfile {
        StoredProfile(
            id: "first",
            secretAccount: "secret.first",
            displayName: "Example",
            summary: ProfileSummary(
                version: 1,
                name: "Example",
                endpoint: "gateway.example:443",
                providerID: "provider",
                gatewayID: "gateway",
                accountID: "account",
                deviceID: "device",
                deviceName: "Phone",
                certificateExpiry: "2030-01-01T00:00:00Z"
            ),
            trafficPolicy: .allTraffic,
            bypassRoutes: bypassRoutes,
            bypassChinaDirect: bypassChinaDirect,
            importedAt: "2026-08-18T00:00:00Z"
        )
    }

    private func decodeCatalog(_ json: String) throws -> ProfileCatalog {
        try JSONDecoder().decode(ProfileCatalog.self, from: Data(json.utf8))
    }

    func testACatalogWrittenBeforeTheFieldExistedStillDecodes() throws {
        let legacy = """
        {"version":1,"selectedProfileID":"first","profiles":[{
          "id":"first","secretAccount":"secret.first","displayName":"Example",
          "trafficPolicy":"all-traffic","importedAt":"2026-08-18T00:00:00Z",
          "summary":{"version":1,"name":"Example","endpoint":"gateway.example:443",
            "provider_id":"provider","gateway_id":"gateway","account_id":"account",
            "device_id":"device","device_name":"Phone",
            "certificate_expiry":"2030-01-01T00:00:00Z"}
        }]}
        """

        let catalog = try decodeCatalog(legacy)

        XCTAssertEqual(catalog.profiles.count, 1)
        XCTAssertEqual(catalog.profiles[0].bypassRoutes, [])
        XCTAssertFalse(catalog.profiles[0].bypassChinaDirect)
        XCTAssertEqual(catalog.profiles[0].summary.deviceID, "device")
    }

    func testTheFieldSurvivesAnEncodeDecodeRoundTrip() throws {
        let catalog = ProfileCatalog(
            selectedProfileID: "first",
            profiles: [
                makeProfile(
                    bypassRoutes: ["10.0.0.0/8", "2001:db8::/32"],
                    bypassChinaDirect: true
                )
            ]
        )

        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        let restored = try JSONDecoder().decode(
            ProfileCatalog.self,
            from: try encoder.encode(catalog)
        )

        XCTAssertEqual(restored, catalog)
    }

    func testNormalizeCanonicalizesDeduplicatesAndDropsInvalidEntries() {
        var catalog = ProfileCatalog(
            selectedProfileID: "first",
            profiles: [
                makeProfile(bypassRoutes: [
                    "  10.1.2.3/8  ",
                    "10.0.0.0/8",
                    "nonsense",
                    "203.0.113.7",
                    "2001:db8:1::/32"
                ])
            ]
        )

        catalog.normalize()

        // Host bits cleared, the duplicate that produced collapsed away, the
        // bare address kept as a host route, and the junk gone.
        XCTAssertEqual(
            catalog.profiles[0].bypassRoutes,
            ["10.0.0.0/8", "203.0.113.7/32", "2001:db8::/32"]
        )
    }

    func testNormalizeBoundsTheListSoTheCatalogCannotGrowWithoutLimit() {
        let overflow = (0..<(StoredProfile.maximumBypassRoutes + 40)).map { index in
            "10.\(index / 256).\(index % 256).0/24"
        }
        var catalog = ProfileCatalog(
            selectedProfileID: "first",
            profiles: [makeProfile(bypassRoutes: overflow)]
        )

        catalog.normalize()

        XCTAssertEqual(catalog.profiles[0].bypassRoutes.count, StoredProfile.maximumBypassRoutes)
    }

    func testNormalizeIsIdempotent() {
        var catalog = ProfileCatalog(
            selectedProfileID: "first",
            profiles: [makeProfile(bypassRoutes: ["10.1.2.3/8", "bad", "10.0.0.0/8"])]
        )

        catalog.normalize()
        let once = catalog
        catalog.normalize()

        XCTAssertEqual(catalog, once)
    }

    func testRouteEntriesAcceptEveryShapeAListGetsPastedIn() {
        XCTAssertEqual(
            StoredProfile.routeEntries(from: "10.0.0.0/8\n192.168.0.0/16"),
            ["10.0.0.0/8", "192.168.0.0/16"]
        )
        XCTAssertEqual(
            StoredProfile.routeEntries(from: "10.0.0.0/8, 192.168.0.0/16; 172.16.0.0/12"),
            ["10.0.0.0/8", "192.168.0.0/16", "172.16.0.0/12"]
        )
        XCTAssertEqual(StoredProfile.routeEntries(from: "   \n  \n "), [])
    }

    func testParseListNamesTheEntriesThatFailedRatherThanCountingThem() {
        let result = IPPrefix.parseList(["10.0.0.0/8", " ", "nope", "1.2.3.4/40"])

        XCTAssertEqual(result.parsed.map(\.cidrText), ["10.0.0.0/8"])
        XCTAssertEqual(result.rejected, ["nope", "1.2.3.4/40"])
    }
}
