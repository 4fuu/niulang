import Foundation

struct ProfileProbeResult: Decodable, Equatable, Sendable {
    let version: Int
    let transport: String
    let latencyMilliseconds: Int64

    enum CodingKeys: String, CodingKey {
        case version
        case transport
        case latencyMilliseconds = "latency_ms"
    }

    static func decode(_ encoded: String) throws -> ProfileProbeResult {
        let result = try JSONDecoder().decode(Self.self, from: Data(encoded.utf8))
        guard result.version == 1 else { throw ProfileProbeError.unsupportedVersion }
        guard result.transport == "quic" || result.transport == "tcp" else {
            throw ProfileProbeError.invalidTransport
        }
        guard result.latencyMilliseconds > 0 else { throw ProfileProbeError.invalidLatency }
        return result
    }
}

enum ProfileProbeState: Equatable, Sendable {
    case testing
    case available(ProfileProbeResult)
    case unavailable(String)

    var summary: String {
        switch self {
        case .testing:
            return "Testing…"
        case let .available(result):
            return "\(result.latencyMilliseconds) ms · \(result.transport.uppercased())"
        case .unavailable:
            return "Unavailable"
        }
    }

    var detail: String? {
        guard case let .unavailable(message) = self else { return nil }
        return message
    }

    var symbol: String {
        switch self {
        case .testing: return "clock.arrow.circlepath"
        case .available: return "checkmark.circle.fill"
        case .unavailable: return "exclamationmark.triangle.fill"
        }
    }
}

struct ProfileProbeOutcome: Sendable {
    let profileID: String
    let state: ProfileProbeState
}

enum ProfileProbeError: LocalizedError {
    case missingProfile
    case unsupportedVersion
    case invalidTransport
    case invalidLatency

    var errorDescription: String? {
        switch self {
        case .missingProfile:
            return "The Queqiao profile no longer exists."
        case .unsupportedVersion:
            return "The connection test returned an unsupported result version."
        case .invalidTransport:
            return "The connection test returned an unknown transport."
        case .invalidLatency:
            return "The connection test returned an invalid latency."
        }
    }
}

func probeStoredProfile(id: String) -> ProfileProbeOutcome {
    do {
        let store = try ProfileStore()
        guard let (_, profile) = try store.profile(id: id) else {
            throw ProfileProbeError.missingProfile
        }
        let encoded = try MobileCore.probeProfile(profile)
        return ProfileProbeOutcome(
            profileID: id,
            state: .available(try ProfileProbeResult.decode(encoded))
        )
    } catch {
        return ProfileProbeOutcome(
            profileID: id,
            state: .unavailable(error.localizedDescription)
        )
    }
}
