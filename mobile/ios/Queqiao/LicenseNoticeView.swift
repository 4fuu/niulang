import SwiftUI

struct LicenseNoticeView: View {
    private let notices: String

    init(bundle: Bundle = .main) {
        if let url = bundle.url(forResource: "THIRD_PARTY_NOTICES", withExtension: "txt"),
           let text = try? String(contentsOf: url, encoding: .utf8) {
            notices = text
        } else {
            notices = "The embedded third-party license notices are unavailable."
        }
    }

    var body: some View {
        ScrollView {
            Text(notices)
                .font(.system(.caption2, design: .monospaced))
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding()
        }
        .navigationTitle("Open-source licenses")
        .navigationBarTitleDisplayMode(.inline)
    }
}
