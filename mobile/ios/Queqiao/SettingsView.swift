import SwiftUI

struct SettingsView: View {
    var body: some View {
        NavigationStack {
            Form {
                Section("Diagnostics") {
                    NavigationLink {
                        ConnectionLogsView()
                    } label: {
                        Label("Connection logs", systemImage: "doc.text.magnifyingglass")
                    }
                    Text(
                        "Startup failures and warnings are kept in an encrypted, on-device ring. " +
                        "Invitations and key material are redacted; traffic contents are never logged."
                    )
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                }

                Section("Traffic and privacy") {
                    Label("No ads or analytics", systemImage: "eye.slash")
                    Label(
                        "Aggregate connection counters stay in memory",
                        systemImage: "chart.bar.xaxis"
                    )
                    Text(
                        "The active provider can observe destinations, timing, sizes, and content " +
                        "that is not protected end-to-end. Queqiao does not sell traffic data."
                    )
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                }

                Section("Profile security") {
                    Label(
                        "Device keys are restricted to this iPhone",
                        systemImage: "iphone.and.arrow.forward"
                    )
                    Label(
                        "Keychain data does not synchronize to iCloud",
                        systemImage: "icloud.slash"
                    )
                    Text(
                        "Queqiao imports one-time invitations instead of portable private profile " +
                        "files. Deleting a profile requires a new invitation."
                    )
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                }

                Section("About") {
                    LabeledContent("Version", value: appVersion)
                    NavigationLink("Open-source licenses") { LicenseNoticeView() }
                }
            }
            .navigationTitle("Settings")
        }
    }

    private var appVersion: String {
        let version = Bundle.main.object(
            forInfoDictionaryKey: "CFBundleShortVersionString"
        ) as? String ?? "Unknown"
        let build = Bundle.main.object(forInfoDictionaryKey: "CFBundleVersion") as? String ?? ""
        return build.isEmpty ? version : "\(version) (\(build))"
    }
}

private struct ConnectionLogsView: View {
    @EnvironmentObject private var model: TunnelModel
    @State private var isClearConfirmationPresented = false

    var body: some View {
        Group {
            if model.diagnosticEntries.isEmpty {
                ContentUnavailableView(
                    "No Connection Logs",
                    systemImage: "doc.text.magnifyingglass",
                    description: Text("Try connecting, then return here to inspect startup events.")
                )
            } else {
                List(model.diagnosticEntries) { entry in
                    VStack(alignment: .leading, spacing: 5) {
                        HStack {
                            Label(entry.component, systemImage: symbol(for: entry.level))
                                .font(.caption.bold())
                                .foregroundStyle(color(for: entry.level))
                            Spacer()
                            Text(entry.timestamp, format: .dateTime.hour().minute().second())
                                .font(.caption.monospacedDigit())
                                .foregroundStyle(.secondary)
                        }
                        Text(entry.message)
                            .font(.callout)
                            .textSelection(.enabled)
                    }
                    .padding(.vertical, 3)
                }
                .refreshable { await model.refreshDiagnostics() }
            }
        }
        .navigationTitle("Connection Logs")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            if !model.diagnosticEntries.isEmpty {
                Button("Clear", role: .destructive) {
                    isClearConfirmationPresented = true
                }
            }
        }
        .confirmationDialog(
            "Clear connection logs?",
            isPresented: $isClearConfirmationPresented,
            titleVisibility: .visible
        ) {
            Button("Clear Logs", role: .destructive) {
                Task { await model.clearDiagnostics() }
            }
        }
        .task { await model.refreshDiagnostics() }
    }

    private func symbol(for level: DiagnosticLevel) -> String {
        switch level {
        case .info: return "info.circle.fill"
        case .warning: return "exclamationmark.triangle.fill"
        case .error: return "xmark.octagon.fill"
        }
    }

    private func color(for level: DiagnosticLevel) -> Color {
        switch level {
        case .info: return .secondary
        case .warning: return .orange
        case .error: return .red
        }
    }
}
