import Foundation

enum DiagnosticLevel: String, Codable, Sendable {
    case info
    case warning
    case error
}

struct DiagnosticEntry: Codable, Identifiable, Equatable, Sendable {
    let id: UUID
    let timestamp: Date
    let level: DiagnosticLevel
    let component: String
    let message: String
}

private struct DiagnosticArchive: Codable, Sendable {
    static let currentVersion = 1

    var version = currentVersion
    var entries: [DiagnosticEntry] = []
}

/// A small encrypted diagnostic ring shared by the app and packet-tunnel
/// extension. It records lifecycle failures, never packet contents or secrets.
struct DiagnosticStore: Sendable {
    static let account = "connection-diagnostics-v1"
    static let maximumEntries = 100

    private static let lock = NSLock()
    private let keychain: KeychainStore

    init(keychain: KeychainStore) {
        self.keychain = keychain
    }

    init() throws {
        keychain = try KeychainStore()
    }

    func append(level: DiagnosticLevel, component: String, message: String) throws {
        Self.lock.lock()
        defer { Self.lock.unlock() }

        var archive = try loadLocked()
        archive.entries.append(DiagnosticEntry(
            id: UUID(),
            timestamp: Date(),
            level: level,
            component: Self.sanitize(component),
            message: Self.sanitize(message)
        ))
        if archive.entries.count > Self.maximumEntries {
            archive.entries.removeFirst(archive.entries.count - Self.maximumEntries)
        }
        try saveLocked(archive)
    }

    func entries() throws -> [DiagnosticEntry] {
        Self.lock.lock()
        defer { Self.lock.unlock() }
        return Array(try loadLocked().entries.reversed())
    }

    func clear() throws {
        Self.lock.lock()
        defer { Self.lock.unlock() }
        try keychain.delete(account: Self.account)
    }

    static func sanitize(_ value: String) -> String {
        var result = value.replacingOccurrences(of: "\r\n", with: "\n")
        result = result.replacingOccurrences(of: "\r", with: "\n")
        let replacements = [
            (#"queqiao://[^\s\"'<>]+"#, "queqiao://<redacted>"),
            (
                #"(?i)(\"?(?:token|private_key|certificate|device_key)\"?\s*[:=]\s*)\"?[^\s,}\"]+\"?"#,
                "$1<redacted>"
            )
        ]
        for (pattern, replacement) in replacements {
            guard let expression = try? NSRegularExpression(pattern: pattern) else { continue }
            let range = NSRange(result.startIndex..., in: result)
            result = expression.stringByReplacingMatches(
                in: result,
                range: range,
                withTemplate: replacement
            )
        }
        if result.count > 1_000 {
            result = String(result.prefix(1_000)) + "…"
        }
        return result
    }

    private func loadLocked() throws -> DiagnosticArchive {
        guard let encoded = try keychain.get(account: Self.account) else {
            return DiagnosticArchive()
        }
        guard let data = encoded.data(using: .utf8) else {
            throw DiagnosticStoreError.invalidArchive
        }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        let archive = try decoder.decode(DiagnosticArchive.self, from: data)
        guard archive.version == DiagnosticArchive.currentVersion else {
            throw DiagnosticStoreError.unsupportedArchiveVersion
        }
        return archive
    }

    private func saveLocked(_ archive: DiagnosticArchive) throws {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        encoder.outputFormatting = [.sortedKeys]
        let data = try encoder.encode(archive)
        guard let encoded = String(data: data, encoding: .utf8) else {
            throw DiagnosticStoreError.invalidArchive
        }
        try keychain.set(encoded, account: Self.account)
    }
}

enum DiagnosticStoreError: LocalizedError {
    case invalidArchive
    case unsupportedArchiveVersion

    var errorDescription: String? {
        switch self {
        case .invalidArchive:
            return "The encrypted connection log is invalid."
        case .unsupportedArchiveVersion:
            return "The connection log was written by an unsupported Queqiao version."
        }
    }
}
