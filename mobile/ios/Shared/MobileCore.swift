import Foundation
import Mobilecore

enum MobileCoreError: LocalizedError {
    case missingNativeError
    case emptyResult

    var errorDescription: String? {
        switch self {
        case .missingNativeError:
            return "The Queqiao core reported failure without an error."
        case .emptyResult:
            return "The Queqiao core returned an empty result."
        }
    }
}

enum MobileCore {
    static func validateInvitation(_ invitation: String) throws {
        var error: NSError?
        guard MobilecoreValidateInvitation(invitation, &error) else {
            throw error ?? MobileCoreError.missingNativeError
        }
    }

    static func validateProfile(_ profile: String) throws {
        var error: NSError?
        guard MobilecoreValidateProfile(profile, &error) else {
            throw error ?? MobileCoreError.missingNativeError
        }
    }

    static func prepareEnrollment(invitation: String, deviceName: String) throws -> String {
        var error: NSError?
        let result = MobilecorePrepareEnrollment(invitation, deviceName, &error)
        if let error { throw error }
        guard !result.isEmpty else { throw MobileCoreError.emptyResult }
        return result
    }

    static func completeEnrollment(draft: String) throws -> String {
        var error: NSError?
        let result = MobilecoreCompleteEnrollment(draft, &error)
        if let error { throw error }
        guard !result.isEmpty else { throw MobileCoreError.emptyResult }
        return result
    }

    static func profileSummary(_ profile: String) throws -> String {
        var error: NSError?
        let result = MobilecoreProfileSummaryJSON(profile, &error)
        if let error { throw error }
        guard !result.isEmpty else { throw MobileCoreError.emptyResult }
        return result
    }

    static func profileNeedsRenewal(_ profile: String) throws -> Bool {
        var renewal: Int64 = 0
        var error: NSError?
        guard MobilecoreProfileNeedsRenewal(profile, &renewal, &error) else {
            throw error ?? MobileCoreError.missingNativeError
        }
        return renewal != 0
    }

    static func renewProfile(_ profile: String) throws -> String {
        var error: NSError?
        let result = MobilecoreRenewProfile(profile, &error)
        if let error { throw error }
        guard !result.isEmpty else { throw MobileCoreError.emptyResult }
        return result
    }
}

extension MobilecoreSession {
    func startChecked(profile: String, packetIO: MobilecorePacketIOProtocol, mtu: Int64) throws {
        try startPacketFlow(profile, packetIO: packetIO, mtu: mtu)
    }

    func stopChecked() throws {
        try stop()
    }
}
