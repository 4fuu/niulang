import XCTest
import NetworkExtension
@testable import Queqiao

/// On-demand rules decide when iOS brings the tunnel up without being asked,
/// and the system evaluates them in order and stops at the first match. Order
/// is therefore the policy, not a detail — so these tests pin the whole list
/// rather than checking that a rule of some kind exists.
final class OnDemandRulesTests: XCTestCase {
    private func shape(_ rules: [NEOnDemandRule]) -> [String] {
        rules.map { rule in
            let action = rule is NEOnDemandRuleConnect ? "connect" : "disconnect"
            let interface: String
            switch rule.interfaceTypeMatch {
            case .wiFi: interface = "wifi"
            case .cellular: interface = "cellular"
            case .any: interface = "any"
            default: interface = "other"
            }
            let networks = (rule.ssidMatch ?? []).joined(separator: "+")
            return networks.isEmpty ? "\(action):\(interface)" : "\(action):\(interface):\(networks)"
        }
    }

    // MARK: the rule list

    func testADisabledPolicyInstallsNoRulesAtAll() {
        XCTAssertTrue(OnDemandRules.rules(for: .off).isEmpty)
        let configured = OnDemandPolicy(
            trustedNetworks: ["Home"],
            connectOnCellular: true,
            isEnabled: false
        )
        XCTAssertTrue(OnDemandRules.rules(for: configured).isEmpty)
    }

    func testTheDefaultPolicyConnectsOnBothInterfaces() {
        let policy = OnDemandPolicy(trustedNetworks: [], connectOnCellular: true, isEnabled: true)
        XCTAssertEqual(
            shape(OnDemandRules.rules(for: policy)),
            ["connect:cellular", "connect:wifi", "disconnect:any"]
        )
    }

    func testTrustedNetworksAreCheckedBeforeWiFiInGeneral() {
        let policy = OnDemandPolicy(
            trustedNetworks: ["Home", "Office"],
            connectOnCellular: true,
            isEnabled: true
        )
        XCTAssertEqual(
            shape(OnDemandRules.rules(for: policy)),
            ["disconnect:wifi:Home+Office", "connect:cellular", "connect:wifi", "disconnect:any"]
        )
    }

    func testRefusingCellularProducesADisconnectRuleRatherThanNoRule() {
        // A missing cellular rule would fall through to the terminal rule, which
        // happens to disconnect today. Saying it outright keeps the meaning from
        // depending on what comes after.
        let policy = OnDemandPolicy(trustedNetworks: [], connectOnCellular: false, isEnabled: true)
        XCTAssertEqual(
            shape(OnDemandRules.rules(for: policy)),
            ["disconnect:cellular", "connect:wifi", "disconnect:any"]
        )
    }

    func testTheListEndsWithATerminalRuleSoNoInterfaceIsUnhandled() {
        for cellular in [true, false] {
            for trusted in [[], ["Home"]] {
                let policy = OnDemandPolicy(
                    trustedNetworks: trusted,
                    connectOnCellular: cellular,
                    isEnabled: true
                )
                let rules = OnDemandRules.rules(for: policy)
                XCTAssertEqual(rules.last?.interfaceTypeMatch, .any)
                XCTAssertTrue(rules.last is NEOnDemandRuleDisconnect)
            }
        }
    }

    func testTrustedNetworksAreSanitizedBeforeTheyReachTheRule() {
        let policy = OnDemandPolicy(
            trustedNetworks: ["  Home  ", "", "Home", "home", "   "],
            connectOnCellular: true,
            isEnabled: true
        )
        XCTAssertEqual(
            OnDemandRules.rules(for: policy).first?.ssidMatch,
            ["Home", "home"]
        )
    }

    // MARK: network names

    func testOnlyLineBreaksSeparateNetworkNames() {
        // An SSID may contain a comma, a space, or a semicolon. Splitting on
        // those — as the bypass list does — would cut real names in half.
        XCTAssertEqual(
            OnDemandRules.entries(from: "Cafe, Bar\nHome Wi-Fi\n\n  Office; Annex  "),
            ["Cafe, Bar", "Home Wi-Fi", "Office; Annex"]
        )
    }

    func testSanitizingKeepsCaseAndOrderAndDropsRepeats() {
        XCTAssertEqual(
            OnDemandRules.sanitizedNetworks(["b", "A", "a", "b", "A"]),
            ["b", "A", "a"]
        )
    }

    func testTheTrustedListIsBounded() {
        let many = (0..<(OnDemandRules.maximumTrustedNetworks + 40)).map { "net-\($0)" }
        XCTAssertEqual(
            OnDemandRules.sanitizedNetworks(many).count,
            OnDemandRules.maximumTrustedNetworks
        )
    }

    // MARK: the summary line

    func testTheSummarySaysWhatTheRulesWillDo() {
        XCTAssertEqual(OnDemandRules.summary(for: .off), "Connects only when you ask.")
        XCTAssertEqual(
            OnDemandRules.summary(
                for: OnDemandPolicy(trustedNetworks: [], connectOnCellular: true, isEnabled: true)
            ),
            "Connects automatically on Wi-Fi and cellular."
        )
        XCTAssertEqual(
            OnDemandRules.summary(
                for: OnDemandPolicy(trustedNetworks: ["Home"], connectOnCellular: false, isEnabled: true)
            ),
            "Connects automatically on Wi-Fi, never on cellular, except on Home."
        )
        XCTAssertEqual(
            OnDemandRules.summary(
                for: OnDemandPolicy(
                    trustedNetworks: ["Home", "Office"],
                    connectOnCellular: true,
                    isEnabled: true
                )
            ),
            "Connects automatically on Wi-Fi and cellular, except on 2 trusted networks."
        )
    }

    // MARK: persistence

    func testTheSettingsSurviveTheCatalogAndDefaultSafely() throws {
        let legacy = """
        {"version":1,"selectedProfileID":"first","profiles":[{"id":"first",
        "secretAccount":"secret.first","displayName":"Example","summary":{"version":1,
        "name":"Example","endpoint":"gateway.example:443","provider_id":"provider",
        "gateway_id":"gateway","account_id":"account","device_id":"device",
        "device_name":"Phone","certificate_expiry":"2030-01-01T00:00:00Z"},
        "trafficPolicy":"all-traffic","importedAt":"2026-01-01T00:00:00Z"}]}
        """
        var catalog = try JSONDecoder().decode(ProfileCatalog.self, from: Data(legacy.utf8))

        // A profile written before the feature existed must not start bringing
        // the tunnel up on its own.
        XCTAssertFalse(catalog.profiles[0].onDemandEnabled)
        XCTAssertEqual(catalog.profiles[0].trustedNetworks, [])
        XCTAssertTrue(catalog.profiles[0].connectOnCellular)
        XCTAssertEqual(OnDemandRules.rules(for: catalog.profiles[0].onDemandPolicy), [])

        catalog.profiles[0].onDemandEnabled = true
        catalog.profiles[0].trustedNetworks = ["  Home  ", "Home", ""]
        catalog.profiles[0].connectOnCellular = false
        catalog.normalize()
        XCTAssertEqual(catalog.profiles[0].trustedNetworks, ["Home"])

        let restored = try JSONDecoder().decode(
            ProfileCatalog.self,
            from: JSONEncoder().encode(catalog)
        )
        XCTAssertEqual(restored.profiles[0].onDemandPolicy, catalog.profiles[0].onDemandPolicy)
    }
}
