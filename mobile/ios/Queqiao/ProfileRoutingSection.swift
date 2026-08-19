import SwiftUI

/// Everything about where one profile's traffic goes: the coarse traffic
/// policy, and the list of destinations kept off the tunnel entirely.
///
/// Split out of ProfilesView because iOS gives Queqiao no consumer app to hand
/// routing policy to, so this is the whole of the routing surface and it is the
/// part most likely to keep growing.
struct ProfileRoutingSection: View {
    @EnvironmentObject private var model: TunnelModel
    let profile: StoredProfile
    @State private var bypassDraft = ""

    var body: some View {
        Group {
            trafficPolicySection
            bypassSection
        }
        .task(id: profile.id) { bypassDraft = storedBypassText }
        .onChange(of: profile.bypassRoutes) { _, _ in bypassDraft = storedBypassText }
    }

    private var trafficPolicySection: some View {
        Section {
            Picker("Routing", selection: policyBinding) {
                ForEach(TrafficPolicy.allCases) { policy in
                    Text(policy.title).tag(policy)
                }
            }
            .pickerStyle(.inline)
            .labelsHidden()
            .disabled(!model.canChangeProfile)

            Text(profile.trafficPolicy.detail)
                .font(.footnote)
                .foregroundStyle(.secondary)
        } header: {
            Text("Traffic policy")
        } footer: {
            Text(
                "DNS uses the Queqiao tunnel in both modes. Local-network exclusions " +
                "affect private and link-local destinations only."
            )
        }
    }

    private var bypassSection: some View {
        Section {
            TextField(
                "203.0.113.0/24\n2001:db8::/32",
                text: $bypassDraft,
                axis: .vertical
            )
            .lineLimit(3...8)
            .autocorrectionDisabled()
            .textInputAutocapitalization(.never)
            .font(.callout.monospaced())
            .disabled(!model.canChangeProfile)

            if hasUnsavedEdits {
                Button("Save bypass routes") {
                    Task { await model.setBypassRoutes(from: bypassDraft, for: profile.id) }
                }
                .disabled(!model.canChangeProfile)
                Button("Discard changes", role: .cancel) { bypassDraft = storedBypassText }
            }

            LabeledContent(
                "In use",
                value: "\(profile.bypassRoutes.count) of \(StoredProfile.maximumBypassRoutes)"
            )
        } header: {
            Text("Bypass routes")
        } footer: {
            Text(
                "Addresses and CIDR blocks listed here stay off the tunnel and use " +
                "the device's normal network. One per line, or separated by commas.\n\n" +
                "DNS still resolves through Queqiao, so this cannot match on domain " +
                "names — only on addresses."
            )
        }
    }

    private var storedBypassText: String {
        profile.bypassRoutes.joined(separator: "\n")
    }

    /// Compares against the stored list rather than tracking an edited flag, so
    /// typing a route and typing it back out again leaves no stale prompt.
    private var hasUnsavedEdits: Bool {
        StoredProfile.routeEntries(from: bypassDraft) != profile.bypassRoutes
    }

    private var policyBinding: Binding<TrafficPolicy> {
        Binding(
            get: { model.profile(id: profile.id)?.trafficPolicy ?? profile.trafficPolicy },
            set: { policy in
                Task { await model.setTrafficPolicy(policy, for: profile.id) }
            }
        )
    }
}
