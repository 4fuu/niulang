import Foundation
import NetworkExtension
import UIKit
import Mobilecore

@MainActor
final class TunnelModel: ObservableObject {
    @Published var invitation = ""
    @Published var deviceName = ""
    @Published var isImporterPresented = false
    @Published private(set) var status = "Loading VPN configuration…"
    @Published private(set) var profiles: [StoredProfile] = []
    @Published private(set) var selectedProfileID: String?
    @Published private(set) var hasDraft = false
    @Published private(set) var isBusy = false
    @Published private(set) var isManagerLoaded = false
    @Published private(set) var metrics = TunnelMetrics.empty
    @Published private(set) var diagnosticEntries: [DiagnosticEntry] = []
    @Published var profileProbeStates: [String: ProfileProbeState] = [:]
    @Published var presentedError: PresentedError?

    var manager: NETunnelProviderManager?
    private var statusObserver: NotificationToken?
    private var configurationObserver: NotificationToken?
    private var hasStarted = false
    var metricsTimer: TimerToken?
    var statusTracker = VPNStatusTracker()
    let disconnectRecoveryMarker = VPNDisconnectRecoveryMarker()
    var disconnectRequested = false
    var managerReloadInProgress = false
    var managerReloadPending = false
    var invalidStatusReloadAttempted = false

    func publishStatus(_ value: String) {
        status = value
    }

    func publishManagerLoaded(_ value: Bool) {
        isManagerLoaded = value
    }

    func publishMetrics(_ value: TunnelMetrics) {
        metrics = value
    }

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
        let configurationToken = NotificationCenter.default.addObserver(
            forName: .NEVPNConfigurationChange,
            object: nil,
            queue: .main
        ) { [weak self] _ in
            Task { @MainActor in
                guard let self else { return }
                await self.loadManager(reason: "iOS reported a VPN configuration change")
            }
        }
        configurationObserver = NotificationToken(configurationToken)
    }

    func start() async {
        guard !hasStarted else { return }
        hasStarted = true
        await loadManager()
        await refreshProfiles()
        await refreshDiagnostics()
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
        if managerReloadPending {
            await loadManager(reason: "queued VPN configuration change")
        } else {
            updateStatus()
        }
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
        if !isManagerLoaded {
            await loadManager(reason: "connect requested while the VPN manager was unavailable")
            guard isManagerLoaded else { return }
        }
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
        if managerReloadPending {
            await loadManager(reason: "app VPN configuration saved")
        } else {
            updateStatus()
        }
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

    /// Persists bypass routes, refusing entries that are not CIDR blocks rather
    /// than dropping them: silently discarding a typed route would leave the
    /// user believing a destination is off the tunnel when it is not.
    func setBypassRoutes(from text: String, for id: String) async {
        guard canChangeProfile else {
            present(ModelError.disconnectBeforeEditing, title: "Disconnect first")
            return
        }
        let entries = StoredProfile.routeEntries(from: text)
        let rejected = IPPrefix.parseList(entries).rejected
        guard rejected.isEmpty else {
            present(ModelError.invalidBypassRoutes(rejected), title: "Check the bypass list")
            return
        }
        do {
            try await Task.detached { try ProfileStore().setBypassRoutes(entries, for: id) }.value
            await refreshProfiles()
        } catch {
            present(error, title: "Could not update bypass routes")
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
            profileProbeStates = profileProbeStates.filter { entry in
                values.0.contains(where: { $0.id == entry.key })
            }
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
            DiagnosticExporter.exportForDebug(entries)
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

extension TunnelModel {
    func setBypassChinaDirect(_ enabled: Bool, for id: String) async {
        guard canChangeProfile else {
            present(ModelError.disconnectBeforeEditing, title: "Disconnect first")
            return
        }
        do {
            try await Task.detached { try ProfileStore().setBypassChinaDirect(enabled, for: id) }.value
            await refreshProfiles()
        } catch {
            present(error, title: "Could not update the bundled route set")
        }
    }
}
