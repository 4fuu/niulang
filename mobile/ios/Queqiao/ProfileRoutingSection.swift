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
    /// How many blocks the bundled set holds, read from its header once. nil
    /// until it is read, and stays nil if the resource is missing — in which
    /// case the screen says nothing rather than guessing a number.
    @State private var bundledBlocks: Int?

    var body: some View {
        Group {
            trafficPolicySection
            bypassSection
            countrySetSection
        }
        .task(id: profile.id) {
            bypassDraft = storedBypassText
            bundledBlocks = try? CountryRoutes.blockCount()
        }
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

            if coversWholeFamily {
                Label(
                    "A route here covers an entire address family, so that traffic will "
                        + "not use the tunnel at all.",
                    systemImage: "exclamationmark.triangle"
                )
                .font(.footnote)
                .foregroundStyle(.orange)
            }
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

    private var countrySetSection: some View {
        Section {
            Toggle("Keep Chinese addresses direct", isOn: chinaDirectBinding)
                .disabled(!model.canChangeProfile)

            if let bundledBlocks {
                LabeledContent("Blocks in the set", value: bundledBlocks.formatted(.number))
            }
        } header: {
            Text("Bundled route set · experimental")
        } footer: {
            Text(
                "Adds the address blocks APNIC records as delegated to China to " +
                "the bypass list above.\n\n" +
                "Two limits are worth knowing before turning this on. The " +
                "registry records where a block was allocated, not where it is " +
                "used, so the match is approximate. And because DNS resolves " +
                "through the tunnel, a Chinese site answering with an address " +
                "outside this set still takes the tunnel — this matches on " +
                "addresses, never on domain names."
            )
        }
    }

    private var chinaDirectBinding: Binding<Bool> {
        Binding(
            get: { model.profile(id: profile.id)?.bypassChinaDirect ?? profile.bypassChinaDirect },
            set: { enabled in
                Task { await model.setBypassChinaDirect(enabled, for: profile.id) }
            }
        )
    }

    /// Whether the stored list takes all of IPv4 or all of IPv6 off the tunnel.
    /// Built through RoutePlan so the screen and the tunnel agree on what a
    /// default route is.
    private var coversWholeFamily: Bool {
        RoutePlan.make(userRoutes: profile.bypassRoutes).excludesDefaultRoute
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
