import Foundation

final class NotificationToken: @unchecked Sendable {
    private let token: NSObjectProtocol

    init(_ token: NSObjectProtocol) {
        self.token = token
    }

    deinit {
        NotificationCenter.default.removeObserver(token)
    }
}

final class TimerToken: @unchecked Sendable {
    private let timer: Timer

    init(_ timer: Timer) {
        self.timer = timer
    }

    func invalidate() {
        timer.invalidate()
    }

    deinit {
        timer.invalidate()
    }
}

struct PresentedError: Identifiable {
    let id = UUID()
    let title: String
    let message: String
}

enum ModelError: LocalizedError {
    case missingProfile
    case emptyCoreResult
    case invalidPacketTunnelIdentifier
    case disconnectBeforeEditing
    case disconnectBeforeTesting
    case emptyMetrics

    var errorDescription: String? {
        switch self {
        case .missingProfile:
            return "Import and select a Queqiao profile before connecting."
        case .emptyCoreResult:
            return "The Queqiao core returned an empty result."
        case .invalidPacketTunnelIdentifier:
            return "The packet-tunnel bundle identifier is not configured."
        case .disconnectBeforeEditing:
            return "Disconnect the VPN before changing its selected profile or routing policy."
        case .disconnectBeforeTesting:
            return "Disconnect the VPN before testing provider connections."
        case .emptyMetrics:
            return "The packet-tunnel extension returned no metrics."
        }
    }
}
