import Foundation
import NetworkExtension

/// When the tunnel is allowed to bring itself up without being asked.
///
/// Held apart from StoredProfile so the rule construction below can be tested
/// without a Keychain, and read back out of the profile through
/// `StoredProfile.onDemandPolicy`.
struct OnDemandPolicy: Equatable, Sendable {
    /// Wi-Fi networks the user considers trusted enough not to tunnel.
    var trustedNetworks: [String]
    var connectOnCellular: Bool
    var isEnabled: Bool

    static let off = OnDemandPolicy(trustedNetworks: [], connectOnCellular: true, isEnabled: false)
}

/// Turns an on-demand policy into the rule list NEVPNManager evaluates.
///
/// iOS walks `onDemandRules` in order and stops at the first rule whose match
/// criteria hold, so the order below *is* the policy: trusted Wi-Fi is checked
/// before Wi-Fi in general, and a terminal rule covers interfaces that are
/// neither. Building the list in one pure function is what makes that order
/// something tests can pin rather than something a reviewer has to trace.
enum OnDemandRules {
    /// How many trusted networks one profile may carry. The catalog is a single
    /// Keychain blob shared with the extension, so the list is bounded for the
    /// same reason the bypass list is.
    static let maximumTrustedNetworks = 64

    /// Splits a text field into candidate network names.
    ///
    /// Newlines only. An SSID may legally contain a comma, a space, or a
    /// semicolon, so the more forgiving splitting the bypass list uses would
    /// silently cut real network names in half.
    static func entries(from text: String) -> [String] {
        text.split(whereSeparator: \.isNewline)
            .map { $0.trimmingCharacters(in: .whitespaces) }
            .filter { !$0.isEmpty }
    }

    /// Trims, drops blanks, removes repeats, and bounds the list.
    ///
    /// Comparison is case-sensitive: an SSID is a byte string, and "Home" and
    /// "home" are two different networks as far as the radio is concerned.
    static func sanitizedNetworks(_ entries: [String]) -> [String] {
        var seen = Set<String>()
        let unique = entries
            .map { $0.trimmingCharacters(in: .whitespaces) }
            .filter { !$0.isEmpty && seen.insert($0).inserted }
        return Array(unique.prefix(maximumTrustedNetworks))
    }

    /// The rules to install, in evaluation order. An empty result means the
    /// tunnel connects only when asked, and the caller must also clear
    /// `isOnDemandEnabled`.
    static func rules(for policy: OnDemandPolicy) -> [NEOnDemandRule] {
        guard policy.isEnabled else { return [] }
        var rules: [NEOnDemandRule] = []

        let trusted = sanitizedNetworks(policy.trustedNetworks)
        if !trusted.isEmpty {
            let stayOff = NEOnDemandRuleDisconnect()
            stayOff.interfaceTypeMatch = .wiFi
            // Matched by the system against the joined network. Queqiao never
            // scans, so this needs no location permission.
            stayOff.ssidMatch = trusted
            rules.append(stayOff)
        }

        let cellular: NEOnDemandRule = policy.connectOnCellular
            ? NEOnDemandRuleConnect()
            : NEOnDemandRuleDisconnect()
        cellular.interfaceTypeMatch = .cellular
        rules.append(cellular)

        let wifi = NEOnDemandRuleConnect()
        wifi.interfaceTypeMatch = .wiFi
        rules.append(wifi)

        // Anything that is neither — a wired accessory, or no interface at all.
        // Stated rather than left to the system's default so the list is total.
        let otherwise = NEOnDemandRuleDisconnect()
        otherwise.interfaceTypeMatch = .any
        rules.append(otherwise)

        return rules
    }

    /// One line describing what the rules above will do, for the settings
    /// screen and the diagnostic ring.
    static func summary(for policy: OnDemandPolicy) -> String {
        guard policy.isEnabled else { return "Connects only when you ask." }
        var text = policy.connectOnCellular
            ? "Connects automatically on Wi-Fi and cellular"
            : "Connects automatically on Wi-Fi, never on cellular"
        let trusted = sanitizedNetworks(policy.trustedNetworks)
        switch trusted.count {
        case 0:
            text += "."
        case 1:
            text += ", except on \(trusted[0])."
        default:
            text += ", except on \(trusted.count) trusted networks."
        }
        return text
    }
}
