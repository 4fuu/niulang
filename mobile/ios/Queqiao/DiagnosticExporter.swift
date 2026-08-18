import Foundation

#if DEBUG
enum DiagnosticExporter {
    static let filename = "queqiao-connection-diagnostics.txt"

    static func export(_ entries: [DiagnosticEntry]) {
        let body = entries.map { entry in
            "\(entry.timestamp.ISO8601Format()) " +
                "\(entry.level.rawValue) \(entry.component): \(entry.message)"
        }.joined(separator: "\n")
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
}
#endif
