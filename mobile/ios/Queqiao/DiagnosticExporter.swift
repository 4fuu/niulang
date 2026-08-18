import Foundation

enum DiagnosticExporter {
    static let filename = "queqiao-connection-diagnostics.txt"

    static func render(_ entries: [DiagnosticEntry]) -> String {
        entries.map { entry in
            "\(entry.timestamp.ISO8601Format()) " +
                "\(entry.level.rawValue) \(DiagnosticStore.sanitize(entry.component)): " +
                DiagnosticStore.sanitize(entry.message)
        }.joined(separator: "\n")
    }

#if DEBUG
    static func exportForDebug(_ entries: [DiagnosticEntry]) {
        let body = render(entries)
        guard let caches = FileManager.default.urls(
            for: .cachesDirectory,
            in: .userDomainMask
        ).first else { return }
        let destination = caches.appendingPathComponent(filename, isDirectory: false)
        try? body.write(to: destination, atomically: true, encoding: .utf8)
        for line in body.split(separator: "\n") {
            print("QUEQIAO_DIAGNOSTIC \(line)")
        }
    }
#endif
}
