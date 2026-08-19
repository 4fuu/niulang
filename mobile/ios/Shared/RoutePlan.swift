import Foundation
import NetworkExtension

/// The exclusion set an iOS tunnel is configured with, together with an account
/// of everything that did not make it in.
///
/// iOS gives one packet-tunnel provider the whole device, so unlike Android
/// there is no consumer app to hand routing policy to. What Queqiao offers
/// instead is deliberately small: a list of destinations that stay off the
/// tunnel. Keeping the arithmetic here — rather than inline in the provider —
/// is what makes it testable, and a wrong route silently sends traffic
/// somewhere the user did not intend.
struct RoutePlan: Sendable, Equatable {
    /// Destinations kept outside the tunnel, coalesced and sorted.
    let excluded: [IPPrefix]
    /// Entries the user supplied that are not valid CIDR blocks.
    let rejected: [String]
    /// Prefixes dropped because the plan hit `limit`. Reported, never silent.
    let truncated: Int

    /// How many routes one plan may carry.
    ///
    /// Every exclusion is a route iOS installs and consults per packet, and
    /// setTunnelNetworkSettings has to complete inside the extension's startup
    /// budget, so the count is bounded rather than open.
    ///
    /// The number is set by the bundled country set, not guessed: the registry
    /// delegates China 7504 blocks once collapsed, and the alternative to
    /// carrying them all is aggregating neighbours together, which quietly
    /// takes addresses nobody asked for off the tunnel. Eight thousand fits the
    /// exact set with room for a full user list on top.
    static let defaultLimit = 8_192

    /// Private and link-local space, the exclusion set behind the
    /// "exclude local networks" traffic policy. It matches the Android
    /// client's RoutePolicy list exactly, and scripts/test_mobile_route_parity.py
    /// fails the build if they stop matching.
    static let localNetworks = [
        "10.0.0.0/8",
        "100.64.0.0/10",
        "127.0.0.0/8",
        "169.254.0.0/16",
        "172.16.0.0/12",
        "192.168.0.0/16",
        "::1/128",
        "fc00::/7",
        "fe80::/10"
    ]

    static let empty = RoutePlan(excluded: [], rejected: [], truncated: 0)

    /// Builds a plan from text the user typed plus a pre-coalesced built-in
    /// set, most often the bundled country list.
    ///
    /// User entries win every conflict with the built-in set. Someone who typed
    /// a prefix meant it; the built-in set is a convenience, and dropping part
    /// of it costs at worst some traffic taking the tunnel that need not have.
    static func make(
        userRoutes: [String],
        builtIn: [IPPrefix] = [],
        limit: Int = defaultLimit
    ) -> RoutePlan {
        let (parsed, rejected) = IPPrefix.parseList(userRoutes)

        var truncated = 0
        var kept = IPPrefix.coalesce(parsed)
        if kept.count > limit {
            truncated += kept.count - limit
            kept = widestFirst(kept, limit: limit)
        }

        let room = limit - kept.count
        if room > 0 && !builtIn.isEmpty {
            var additions = builtIn
            if additions.count > room {
                truncated += additions.count - room
                additions = widestFirst(additions, limit: room)
            }
            kept = IPPrefix.coalesce(kept + additions)
        } else if !builtIn.isEmpty {
            truncated += builtIn.count
        }

        return RoutePlan(excluded: kept, rejected: rejected, truncated: truncated)
    }

    /// The built-in local-network plan, which must reproduce byte for byte the
    /// routes the tunnel shipped before route handling was factored out.
    static func localNetworkPlan() -> RoutePlan {
        make(userRoutes: localNetworks)
    }

    var ipv4Routes: [NEIPv4Route] {
        excluded
            .filter { $0.family == .ipv4 }
            .map { NEIPv4Route(destinationAddress: $0.addressText, subnetMask: $0.subnetMaskText) }
    }

    var ipv6Routes: [NEIPv6Route] {
        excluded
            .filter { $0.family == .ipv6 }
            .map {
                NEIPv6Route(
                    destinationAddress: $0.addressText,
                    networkPrefixLength: NSNumber(value: $0.length)
                )
            }
    }

    /// Whether some entry takes an entire address family off the tunnel.
    ///
    /// A zero-length prefix is legal, occasionally deliberate, and almost never
    /// what someone meant: the tunnel connects and then carries nothing, which
    /// looks exactly like a broken gateway. Callers surface it; RoutePlan does
    /// not silently drop it, because a user who typed it may have meant it.
    var excludesDefaultRoute: Bool {
        excluded.contains { $0.length == 0 }
    }

    /// A single line for the diagnostic ring, so a truncated or partly rejected
    /// plan leaves a trace the user can find later.
    var diagnosticSummary: String {
        var parts = ["\(excluded.count) bypass routes"]
        if truncated > 0 { parts.append("\(truncated) dropped at the route limit") }
        if !rejected.isEmpty { parts.append("\(rejected.count) rejected as invalid") }
        return parts.joined(separator: ", ")
    }

    /// Keeps the blocks covering the most address space. Dropping the narrowest
    /// prefixes loses the least reach, and erring toward the tunnel is the safe
    /// direction: traffic still arrives, just not directly.
    private static func widestFirst(_ prefixes: [IPPrefix], limit: Int) -> [IPPrefix] {
        guard prefixes.count > limit else { return prefixes }
        let ranked = prefixes.sorted { lhs, rhs in
            if lhs.coverage != rhs.coverage { return lhs.coverage > rhs.coverage }
            return lhs < rhs
        }
        return ranked.prefix(limit).sorted()
    }
}
