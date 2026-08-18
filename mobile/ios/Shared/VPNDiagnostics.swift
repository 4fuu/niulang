import Foundation
import NetworkExtension

struct VPNStatusObservation: Equatable {
    let previousStatus: NEVPNStatus?
    let currentStatus: NEVPNStatus
    let endedActiveEpisode: Bool

    var transitionDescription: String? {
        guard let previousStatus, previousStatus != currentStatus else { return nil }
        return "VPN status changed from \(previousStatus.diagnosticName) " +
            "to \(currentStatus.diagnosticName)"
    }
}

/// Retains logical connection state across the intermediate `disconnecting`
/// status so a system-initiated stop is still recognized at `disconnected`.
struct VPNStatusTracker {
    private(set) var previousStatus: NEVPNStatus?
    private var activeEpisode = false

    mutating func observe(_ status: NEVPNStatus) -> VPNStatusObservation {
        let endedActiveEpisode = activeEpisode && status.isTerminal
        let observation = VPNStatusObservation(
            previousStatus: previousStatus,
            currentStatus: status,
            endedActiveEpisode: endedActiveEpisode
        )
        if status.isActiveEpisode {
            activeEpisode = true
        } else if status.isTerminal {
            activeEpisode = false
        }
        previousStatus = status
        return observation
    }

    mutating func reset() {
        previousStatus = nil
        activeEpisode = false
    }
}

struct VPNDisconnectRecoveryMarker {
    static let defaultKey = "vpn-active-episode-v1"

    private let defaults: UserDefaults
    private let key: String

    init(defaults: UserDefaults = .standard, key: String = defaultKey) {
        self.defaults = defaults
        self.key = key
    }

    var needsDisconnectRecovery: Bool {
        defaults.bool(forKey: key)
    }

    func markConnected() {
        defaults.set(true, forKey: key)
    }

    func resolveDisconnect() {
        defaults.removeObject(forKey: key)
    }
}

enum VPNDiagnostics {
    private static let providerStopReasons = [
        0: "none",
        1: "user initiated",
        2: "provider failed",
        3: "no network available",
        4: "unrecoverable network change",
        5: "provider disabled",
        6: "authentication canceled",
        7: "configuration failed",
        8: "idle timeout",
        9: "configuration disabled",
        10: "configuration removed",
        11: "superseded by another VPN",
        12: "user logout",
        13: "user switch",
        14: "connection failed",
        15: "device sleep",
        16: "app update",
        17: "NetworkExtension internal error"
    ]

    private static let disconnectErrors = [
        1: "device overslept",
        2: "no network available",
        3: "unrecoverable network change",
        4: "configuration failed",
        5: "server-address resolution failed",
        6: "server not responding",
        7: "server unavailable",
        8: "authentication failed",
        9: "client certificate invalid",
        10: "client certificate not yet valid",
        11: "client certificate expired",
        12: "VPN extension failed",
        13: "configuration not found",
        14: "VPN extension disabled or updated",
        15: "negotiation failed",
        16: "server disconnected",
        17: "server certificate invalid",
        18: "server certificate not yet valid",
        19: "server certificate expired"
    ]

    static func providerStopReasonName(rawValue: Int) -> String {
        providerStopReasons[rawValue] ?? "unknown"
    }

    static func disconnectErrorName(domain: String, code: Int) -> String? {
        guard domain == NEVPNConnectionErrorDomain else { return nil }
        return disconnectErrors[code] ?? "unrecognized NetworkExtension error"
    }

    static func describeDisconnectError(_ error: NSError) -> String {
        let name = disconnectErrorName(domain: error.domain, code: error.code) ?? "disconnect error"
        return "\(name) [\(error.domain) code \(error.code)]: \(error.localizedDescription)"
    }

    @MainActor
    static func fetchLastDisconnectError(from connection: NEVPNConnection) async -> NSError? {
        await withCheckedContinuation { continuation in
            connection.fetchLastDisconnectError { error in
                continuation.resume(returning: error as NSError?)
            }
        }
    }
}

extension NEVPNStatus {
    var diagnosticName: String {
        switch self {
        case .invalid: return "invalid"
        case .disconnected: return "disconnected"
        case .connecting: return "connecting"
        case .connected: return "connected"
        case .reasserting: return "reasserting"
        case .disconnecting: return "disconnecting"
        @unknown default: return "unknown"
        }
    }

    fileprivate var isActiveEpisode: Bool {
        switch self {
        case .connecting, .connected, .reasserting, .disconnecting: return true
        case .invalid, .disconnected: return false
        @unknown default: return false
        }
    }

    fileprivate var isTerminal: Bool {
        switch self {
        case .invalid, .disconnected: return true
        case .connecting, .connected, .reasserting, .disconnecting: return false
        @unknown default: return true
        }
    }
}

extension NEProviderStopReason {
    var diagnosticName: String {
        VPNDiagnostics.providerStopReasonName(rawValue: rawValue)
    }
}
