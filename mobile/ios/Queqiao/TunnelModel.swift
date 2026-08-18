import Foundation
import NetworkExtension
import UIKit
import Mobilecore

@MainActor
final class TunnelModel: ObservableObject {
    @Published var invitation = ""
    @Published var deviceName = ""
    @Published var isImporterPresented = false
    @Published private(set) var status = "Disconnected"
    @Published private(set) var profiles: [StoredProfile] = []
    @Published private(set) var selectedProfileID: String?
    @Published private(set) var hasDraft = false
    @Published private(set) var isBusy = false
    @Published private(set) var metrics = TunnelMetrics.empty
    @Published private(set) var diagnosticEntries: [DiagnosticEntry] = []
    @Published var presentedError: PresentedError?

    private var manager: NETunnelProviderManager?
    private var statusObserver: NotificationToken?
    private var metricsTimer: TimerToken?
    private var previousConnectionStatus: NEVPNStatus = .invalid
    private var disconnectRequested = false

    var selectedProfile: StoredProfile? {
        profiles.first(where: { $0.id == selectedProfileID })
    }

    var hasProfiles: Bool { !profiles.isEmpty }

    var isTunnelActive: Bool {
        switch manager?.connection.status {
        case .connected, .connecting, .disconnecting, .reasserting:
            return true
        default:
            return false
        }
    }

    var canChangeProfile: Bool { !isBusy && !isTunnelActive }

    init() {
        deviceName = UIDevice.current.name
        let token = NotificationCenter.default.addObserver(
            forName: .NEVPNStatusDidChange,
            object: nil,
            queue: .main
        ) { [weak self] _ in
            Task { @MainActor in self?.updateStatus() }
        }
        statusObserver = NotificationToken(token)
        Task {
            await loadManager()
            await refreshProfiles()
            await refreshDiagnostics()
        }
    }

    func receiveInvitation(_ url: URL) {
        guard url.scheme?.lowercased() == "queqiao" else {
            present(ModelError.invalidInvitationLink, title: "Cannot import profile")
            return
        }
        do {
            try MobileCore.validateInvitation(url.absoluteString)
            invitation = url.absoluteString
            isImporterPresented = true
        } catch {
            present(error, title: "Invalid invitation")
        }
    }

    func enroll() async {
        guard !isBusy else { return }
        let invitation = invitation.trimmingCharacters(in: .whitespacesAndNewlines)
        let deviceName = deviceName.trimmingCharacters(in: .whitespacesAndNewlines)
        guard hasDraft || (!invitation.isEmpty && !deviceName.isEmpty) else { return }
        isBusy = true
        status = hasDraft ? "Resuming enrollment…" : "Importing profile…"
        do {
            try await Task.detached(priority: .userInitiated) {
                let store = try ProfileStore()
                var draft = try store.enrollmentDraft()
                if draft == nil {
                    try MobileCore.validateInvitation(invitation)
                    draft = try MobileCore.prepareEnrollment(invitation: invitation, deviceName: deviceName)
                    guard let draft else { throw ModelError.emptyCoreResult }
                    try store.saveEnrollmentDraft(draft)
                }
                guard let draft else { throw ModelError.emptyCoreResult }
                let profile = try MobileCore.completeEnrollment(draft: draft)
                _ = try store.importProfile(profile)
                try store.discardEnrollmentDraft()
            }.value
            self.invitation = ""
            self.deviceName = UIDevice.current.name
            isImporterPresented = false
            await refreshProfiles()
        } catch {
            present(error, title: "Profile import failed")
        }
        isBusy = false
        updateStatus()
    }

    func discardEnrollmentDraft() async {
        guard !isBusy else { return }
        do {
            try await Task.detached { try ProfileStore().discardEnrollmentDraft() }.value
            hasDraft = false
        } catch {
            present(error, title: "Could not discard enrollment")
        }
    }

    func connect() async {
        guard !isBusy else { return }
        guard selectedProfile != nil else {
            isImporterPresented = true
            return
        }
        isBusy = true
        disconnectRequested = false
        status = "Validating profile…"
        do {
            let profileName = selectedProfile?.displayName ?? "selected profile"
            await recordDiagnostic(level: .info, message: "Connect requested for \(profileName)")
            let record = try await renewSelectedProfileIfNeeded()
            let manager = try await configuredManager(for: record)
            self.manager = manager
            try manager.connection.startVPNTunnel()
            updateStatus()
        } catch {
            await recordDiagnostic(level: .error, message: "Connection request failed: \(error.localizedDescription)")
            present(error, title: "Cannot connect")
        }
        isBusy = false
        updateStatus()
    }

    func disconnect() {
        disconnectRequested = true
        manager?.connection.stopVPNTunnel()
        updateStatus()
    }

    func selectProfile(id: String) async {
        guard canChangeProfile, id != selectedProfileID else { return }
        do {
            try await Task.detached { try ProfileStore().select(id: id) }.value
            await refreshProfiles()
        } catch {
            present(error, title: "Could not select profile")
        }
    }

    func renameProfile(id: String, name: String) async {
        do {
            try await Task.detached { try ProfileStore().rename(id: id, to: name) }.value
            await refreshProfiles()
        } catch {
            present(error, title: "Could not rename profile")
        }
    }

    func setTrafficPolicy(_ policy: TrafficPolicy, for id: String) async {
        guard canChangeProfile else {
            present(ModelError.disconnectBeforeEditing, title: "Disconnect first")
            return
        }
        do {
            try await Task.detached { try ProfileStore().setTrafficPolicy(policy, for: id) }.value
            await refreshProfiles()
        } catch {
            present(error, title: "Could not update traffic policy")
        }
    }

    func deleteProfile(id: String) async {
        guard canChangeProfile else {
            present(ModelError.disconnectBeforeEditing, title: "Disconnect first")
            return
        }
        do {
            try await Task.detached { try ProfileStore().delete(id: id) }.value
            await refreshProfiles()
        } catch {
            present(error, title: "Could not delete profile")
        }
    }

    func refreshProfiles() async {
        do {
            let values = try await Task.detached {
                let store = try ProfileStore()
                let catalog = try store.catalog()
                return (catalog.profiles, catalog.selectedProfileID, try store.hasEnrollmentDraft())
            }.value
            profiles = values.0.sorted {
                if $0.id == values.1 { return true }
                if $1.id == values.1 { return false }
                return $0.displayName.localizedCaseInsensitiveCompare($1.displayName) == .orderedAscending
            }
            selectedProfileID = values.1
            hasDraft = values.2
        } catch {
            present(error, title: "Stored profiles are unavailable")
        }
    }

    func profile(id: String) -> StoredProfile? {
        profiles.first(where: { $0.id == id })
    }

    func refreshDiagnostics() async {
        do {
            let entries = try await Task.detached {
                try DiagnosticStore().entries()
            }.value
            diagnosticEntries = entries
#if DEBUG
            DiagnosticExporter.export(entries)
#endif
        } catch {
            present(error, title: "Connection logs are unavailable")
        }
    }

    func clearDiagnostics() async {
        do {
            try await Task.detached { try DiagnosticStore().clear() }.value
            diagnosticEntries = []
        } catch {
            present(error, title: "Could not clear connection logs")
        }
    }
}

private extension TunnelModel {
    private func renewSelectedProfileIfNeeded() async throws -> StoredProfile {
        try await Task.detached(priority: .userInitiated) {
            let store = try ProfileStore()
            guard var (record, profile) = try store.selectedProfile() else {
                throw ModelError.missingProfile
            }
            if try MobileCore.profileNeedsRenewal(profile) {
                profile = try MobileCore.renewProfile(profile)
                try store.replaceProfile(profile, id: record.id)
                guard let refreshed = try store.profile(id: record.id)?.0 else {
                    throw ModelError.missingProfile
                }
                record = refreshed
            }
            return record
        }.value
    }

    func loadManager() async {
        do {
            let managers = try await NETunnelProviderManager.loadAllFromPreferences()
            manager = managers.first
            updateStatus()
        } catch {
            present(error, title: "VPN configuration is unavailable")
        }
    }

    func configuredManager(for record: StoredProfile) async throws -> NETunnelProviderManager {
        let manager = manager ?? NETunnelProviderManager()
        let configuration = NETunnelProviderProtocol()
        guard let providerIdentifier = Bundle.main.object(
            forInfoDictionaryKey: "QueqiaoPacketTunnelBundleIdentifier"
        ) as? String,
              !providerIdentifier.isEmpty,
              !providerIdentifier.contains("$(") else {
            throw ModelError.invalidPacketTunnelIdentifier
        }
        configuration.providerBundleIdentifier = providerIdentifier
        configuration.serverAddress = record.summary.endpoint
        configuration.disconnectOnSleep = false
        configuration.providerConfiguration = [
            "profileID": record.id,
            "trafficPolicy": record.trafficPolicy.rawValue
        ]
        manager.protocolConfiguration = configuration
        manager.localizedDescription = "Queqiao"
        manager.isEnabled = true
        manager.isOnDemandEnabled = false
        try await manager.saveToPreferences()
        try await manager.loadFromPreferences()
        return manager
    }

    func updateStatus() {
        let connectionStatus = manager?.connection.status ?? .invalid
        let priorStatus = previousConnectionStatus
        if connectionStatus != priorStatus {
            let endedUnexpectedly = connectionStatus == .disconnected && !disconnectRequested && (
                priorStatus == .connecting || priorStatus == .connected || priorStatus == .reasserting
            )
            Task {
                await recordDiagnostic(
                    level: .info,
                    message: "VPN status changed from \(Self.name(for: priorStatus)) " +
                        "to \(Self.name(for: connectionStatus))"
                )
                if endedUnexpectedly {
                    await recordDiagnostic(
                        level: .error,
                        message: "iOS ended the VPN without a disconnect request; " +
                            "see the packet-tunnel entries above for the cause"
                    )
                }
            }
            if connectionStatus == .disconnected || connectionStatus == .invalid {
                disconnectRequested = false
            }
        }
        previousConnectionStatus = connectionStatus
        switch connectionStatus {
        case .connected:
            status = "Connected"
            startMetricsUpdates()
        case .connecting:
            status = "Connecting…"
            stopMetricsUpdates(reset: false)
        case .disconnecting:
            status = "Disconnecting…"
            stopMetricsUpdates(reset: false)
        case .reasserting:
            status = "Reconnecting…"
            stopMetricsUpdates(reset: false)
        case .disconnected, .invalid:
            status = "Disconnected"
            stopMetricsUpdates(reset: true)
        @unknown default:
            status = "Unavailable"
            stopMetricsUpdates(reset: true)
        }
    }
    static func name(for status: NEVPNStatus) -> String {
        switch status {
        case .invalid: return "invalid"
        case .disconnected: return "disconnected"
        case .connecting: return "connecting"
        case .connected: return "connected"
        case .reasserting: return "reasserting"
        case .disconnecting: return "disconnecting"
        @unknown default: return "unknown"
        }
    }
    func startMetricsUpdates() {
        guard metricsTimer == nil else { return }
        Task { await refreshMetrics() }
        let timer = Timer.scheduledTimer(withTimeInterval: 1, repeats: true) { [weak self] _ in
            Task { @MainActor in await self?.refreshMetrics() }
        }
        metricsTimer = TimerToken(timer)
    }
    func stopMetricsUpdates(reset: Bool) {
        metricsTimer?.invalidate()
        metricsTimer = nil
        if reset { metrics = .empty }
    }

    func refreshMetrics() async {
        guard let session = manager?.connection as? NETunnelProviderSession,
              session.status == .connected else { return }
        do {
            let response: Data = try await withCheckedThrowingContinuation { continuation in
                do {
                    try session.sendProviderMessage(Data("metrics".utf8)) { data in
                        guard let data else {
                            continuation.resume(throwing: ModelError.emptyMetrics)
                            return
                        }
                        continuation.resume(returning: data)
                    }
                } catch {
                    continuation.resume(throwing: error)
                }
            }
            metrics = try TunnelMetrics.decode(response)
        } catch {
            // Metrics are operational decoration; a transient extension IPC failure must not alter tunnel state.
        }
    }

    func present(_ error: Error, title: String) {
        presentedError = PresentedError(title: title, message: error.localizedDescription)
    }

    func recordDiagnostic(level: DiagnosticLevel, message: String) async {
        await Task.detached {
            try? DiagnosticStore().append(level: level, component: "App", message: message)
        }.value
    }
}
