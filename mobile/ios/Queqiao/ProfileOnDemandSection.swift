import SwiftUI

/// The automatic-connection settings for one profile.
///
/// Kept apart from ProfileRoutingSection because it answers a different
/// question — *when* the tunnel comes up rather than *where* traffic goes —
/// and because both sections carry enough explanation to crowd one file.
struct ProfileOnDemandSection: View {
    @EnvironmentObject private var model: TunnelModel
    let profile: StoredProfile
    @State private var networkDraft = ""

    var body: some View {
        Group {
            enableSection
            trustedNetworkSection
        }
        .task(id: profile.id) { networkDraft = storedNetworkText }
        .onChange(of: profile.trustedNetworks) { _, _ in networkDraft = storedNetworkText }
    }

    private var enableSection: some View {
        Section {
            Toggle("Connect automatically", isOn: enabledBinding)
                .disabled(!model.canChangeProfile)
            Toggle("Include cellular", isOn: cellularBinding)
                .disabled(!model.canChangeProfile || !profile.onDemandEnabled)
            Text(OnDemandRules.summary(for: profile.onDemandPolicy))
                .font(.footnote)
                .foregroundStyle(.secondary)
        } header: {
            Text("Automatic connection")
        } footer: {
            Text(
                "iOS brings the tunnel up on its own when these rules match, " +
                "including after a reboot and before the app has been opened.\n\n" +
                "Disconnecting by hand pauses this until you connect again, so " +
                "the button always means what it says."
            )
        }
    }

    private var trustedNetworkSection: some View {
        Section {
            TextField("Home Wi-Fi\nOffice", text: $networkDraft, axis: .vertical)
                .lineLimit(2...6)
                .autocorrectionDisabled()
                .textInputAutocapitalization(.never)
                .disabled(!model.canChangeProfile || !profile.onDemandEnabled)

            if hasUnsavedEdits {
                Button("Save trusted networks") {
                    Task { await model.setOnDemandPolicy(draftPolicy, for: profile.id) }
                }
                .disabled(!model.canChangeProfile)
                Button("Discard changes", role: .cancel) { networkDraft = storedNetworkText }
            }

            LabeledContent(
                "In use",
                value: "\(profile.trustedNetworks.count) of \(OnDemandRules.maximumTrustedNetworks)"
            )
        } header: {
            Text("Trusted Wi-Fi networks")
        } footer: {
            Text(
                "On these networks the tunnel stays down until you connect it. " +
                "One network name per line, matched exactly — names may contain " +
                "spaces and commas, so only line breaks separate them.\n\n" +
                "Queqiao never scans for networks and asks for no location " +
                "permission; iOS matches the name for it."
            )
        }
    }

    private var storedNetworkText: String {
        profile.trustedNetworks.joined(separator: "\n")
    }

    private var hasUnsavedEdits: Bool {
        OnDemandRules.entries(from: networkDraft) != profile.trustedNetworks
    }

    private var draftPolicy: OnDemandPolicy {
        OnDemandPolicy(
            trustedNetworks: OnDemandRules.entries(from: networkDraft),
            connectOnCellular: profile.connectOnCellular,
            isEnabled: profile.onDemandEnabled
        )
    }

    private var enabledBinding: Binding<Bool> {
        policyBinding(\.isEnabled)
    }

    private var cellularBinding: Binding<Bool> {
        policyBinding(\.connectOnCellular)
    }

    /// One flag of the policy, written back as a whole policy so the store
    /// never holds a half-applied rule set.
    private func policyBinding(_ field: WritableKeyPath<OnDemandPolicy, Bool>) -> Binding<Bool> {
        Binding(
            get: { current[keyPath: field] },
            set: { newValue in
                var policy = current
                policy[keyPath: field] = newValue
                Task { await model.setOnDemandPolicy(policy, for: profile.id) }
            }
        )
    }

    private var current: OnDemandPolicy {
        model.profile(id: profile.id)?.onDemandPolicy ?? profile.onDemandPolicy
    }
}
