import Foundation

enum TrafficPolicy: String, Codable, CaseIterable, Identifiable, Sendable {
    case allTraffic = "all-traffic"
    case excludeLocalNetworks = "exclude-local-networks"

    var id: String { rawValue }

    var title: String {
        switch self {
        case .allTraffic:
            return "All traffic"
        case .excludeLocalNetworks:
            return "Exclude local networks"
        }
    }

    var detail: String {
        switch self {
        case .allTraffic:
            return "Route IPv4, IPv6, and DNS traffic through the selected Queqiao provider."
        case .excludeLocalNetworks:
            return "Keep private and link-local destinations outside the tunnel; " +
                "route internet and DNS traffic through Queqiao."
        }
    }
}

struct ProfileSummary: Codable, Equatable, Sendable {
    let version: Int
    let name: String
    let endpoint: String
    let providerID: String
    let gatewayID: String
    let accountID: String
    let deviceID: String
    let deviceName: String
    let certificateExpiry: String

    enum CodingKeys: String, CodingKey {
        case version
        case name
        case endpoint
        case providerID = "provider_id"
        case gatewayID = "gateway_id"
        case accountID = "account_id"
        case deviceID = "device_id"
        case deviceName = "device_name"
        case certificateExpiry = "certificate_expiry"
    }
}

struct StoredProfile: Codable, Identifiable, Equatable, Sendable {
    /// How many hand-entered bypass routes one profile may hold.
    ///
    /// The catalog is a single Keychain blob rewritten on every save, so the
    /// list has to be bounded somewhere. Anyone who wants more destinations
    /// than this off the tunnel wants a generated set, not a typed one.
    static let maximumBypassRoutes = 256

    let id: String
    let secretAccount: String
    var displayName: String
    var summary: ProfileSummary
    var trafficPolicy: TrafficPolicy
    /// Destinations kept off the tunnel, in canonical CIDR text. Sanitized by
    /// ProfileCatalog.normalize on every load and save, so a caller may hand
    /// this whatever the user typed.
    var bypassRoutes: [String] = []
    /// Whether the bundled registry set for China is added to the bypass list.
    /// Experimental, and address-based only: see CountryRoutes.
    var bypassChinaDirect = false
    let importedAt: String

    /// Splits a text field into candidate entries.
    ///
    /// Newlines, commas, semicolons and spaces all separate, because a list
    /// pasted out of a config file, a shell command, or another client arrives
    /// in all four forms and rejecting three of them teaches nothing.
    static func routeEntries(from text: String) -> [String] {
        text
            .split(whereSeparator: { $0.isNewline || $0 == "," || $0 == ";" || $0 == " " || $0 == "\t" })
            .map(String.init)
    }
}

extension StoredProfile {
    /// Decoded by hand because Swift's synthesized Decodable ignores property
    /// defaults: a catalog written before a field existed would fail to decode
    /// and take every enrolled profile on the device with it.
    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        id = try container.decode(String.self, forKey: .id)
        secretAccount = try container.decode(String.self, forKey: .secretAccount)
        displayName = try container.decode(String.self, forKey: .displayName)
        summary = try container.decode(ProfileSummary.self, forKey: .summary)
        trafficPolicy = try container.decode(TrafficPolicy.self, forKey: .trafficPolicy)
        bypassRoutes = try container.decodeIfPresent([String].self, forKey: .bypassRoutes) ?? []
        bypassChinaDirect = try container.decodeIfPresent(Bool.self, forKey: .bypassChinaDirect) ?? false
        importedAt = try container.decode(String.self, forKey: .importedAt)
    }
}

struct ProfileCatalog: Codable, Equatable, Sendable {
    static let currentVersion = 1

    var version = currentVersion
    var selectedProfileID: String?
    var profiles: [StoredProfile] = []

    mutating func normalize() {
        var seen = Set<String>()
        profiles = profiles.filter { !$0.id.isEmpty && seen.insert($0.id).inserted }
        for index in profiles.indices {
            profiles[index].bypassRoutes = Self.sanitizedRoutes(profiles[index].bypassRoutes)
        }
        if let selectedProfileID, !profiles.contains(where: { $0.id == selectedProfileID }) {
            self.selectedProfileID = profiles.first?.id
        } else if selectedProfileID == nil {
            selectedProfileID = profiles.first?.id
        }
    }

    /// Canonical, deduplicated, and bounded. Entries that are not CIDR blocks
    /// are dropped here rather than at connect time, where a bad one would
    /// surface as a tunnel that failed to configure.
    static func sanitizedRoutes(_ entries: [String]) -> [String] {
        var seen = Set<String>()
        let canonical = IPPrefix.parseList(entries).parsed
            .map(\.cidrText)
            .filter { seen.insert($0).inserted }
        return Array(canonical.prefix(StoredProfile.maximumBypassRoutes))
    }
}

struct ProfileStore: Sendable {
    static let catalogAccount = "profile-catalog-v1"
    static let profileAccountPrefix = "client-profile-v1."

    private let keychain: KeychainStore

    init(keychain: KeychainStore) {
        self.keychain = keychain
    }

    init() throws {
        keychain = try KeychainStore()
    }

    func catalog() throws -> ProfileCatalog {
        if let encoded = try keychain.get(account: Self.catalogAccount) {
            var catalog = try decodeCatalog(encoded)
            let original = catalog
            catalog.normalize()
            if catalog != original {
                try save(catalog)
            }
            return catalog
        }
        return try migrateLegacyProfile()
    }

    @discardableResult
    func importProfile(_ profileJSON: String) throws -> StoredProfile {
        try MobileCore.validateProfile(profileJSON)
        let summary = try decodeSummary(profileJSON)
        var catalog = try catalog()
        if let index = catalog.profiles.firstIndex(where: { $0.summary.deviceID == summary.deviceID }) {
            let account = catalog.profiles[index].secretAccount
            try keychain.set(profileJSON, account: account)
            catalog.profiles[index].summary = summary
            catalog.selectedProfileID = catalog.profiles[index].id
            try save(catalog)
            return catalog.profiles[index]
        }

        let identifier = UUID().uuidString.lowercased()
        let account = Self.profileAccountPrefix + identifier
        let record = StoredProfile(
            id: identifier,
            secretAccount: account,
            displayName: summary.name,
            summary: summary,
            trafficPolicy: .allTraffic,
            importedAt: ISO8601DateFormatter().string(from: Date())
        )
        try keychain.set(profileJSON, account: account)
        do {
            catalog.profiles.append(record)
            catalog.selectedProfileID = identifier
            try save(catalog)
        } catch {
            try? keychain.delete(account: account)
            throw error
        }
        return record
    }

    func profile(id: String) throws -> (StoredProfile, String)? {
        let catalog = try catalog()
        guard let record = catalog.profiles.first(where: { $0.id == id }),
              let profile = try keychain.get(account: record.secretAccount) else {
            return nil
        }
        try MobileCore.validateProfile(profile)
        return (record, profile)
    }

    func selectedProfile() throws -> (StoredProfile, String)? {
        let catalog = try catalog()
        guard let identifier = catalog.selectedProfileID else { return nil }
        return try profile(id: identifier)
    }

    func select(id: String) throws {
        var catalog = try catalog()
        guard catalog.profiles.contains(where: { $0.id == id }) else {
            throw ProfileStoreError.profileNotFound
        }
        catalog.selectedProfileID = id
        try save(catalog)
    }

    func rename(id: String, to requestedName: String) throws {
        let name = requestedName.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !name.isEmpty, name.count <= 80 else {
            throw ProfileStoreError.invalidDisplayName
        }
        var catalog = try catalog()
        guard let index = catalog.profiles.firstIndex(where: { $0.id == id }) else {
            throw ProfileStoreError.profileNotFound
        }
        catalog.profiles[index].displayName = name
        try save(catalog)
    }

    func setTrafficPolicy(_ policy: TrafficPolicy, for id: String) throws {
        var catalog = try catalog()
        guard let index = catalog.profiles.firstIndex(where: { $0.id == id }) else {
            throw ProfileStoreError.profileNotFound
        }
        catalog.profiles[index].trafficPolicy = policy
        try save(catalog)
    }

    /// Stores bypass routes for one profile. The entries are sanitized by
    /// save, so the caller may pass raw text split into candidates.
    func setBypassRoutes(_ routes: [String], for id: String) throws {
        var catalog = try catalog()
        guard let index = catalog.profiles.firstIndex(where: { $0.id == id }) else {
            throw ProfileStoreError.profileNotFound
        }
        catalog.profiles[index].bypassRoutes = routes
        try save(catalog)
    }

    func setBypassChinaDirect(_ enabled: Bool, for id: String) throws {
        var catalog = try catalog()
        guard let index = catalog.profiles.firstIndex(where: { $0.id == id }) else {
            throw ProfileStoreError.profileNotFound
        }
        catalog.profiles[index].bypassChinaDirect = enabled
        try save(catalog)
    }

    func replaceProfile(_ profileJSON: String, id: String) throws {
        try MobileCore.validateProfile(profileJSON)
        let summary = try decodeSummary(profileJSON)
        var catalog = try catalog()
        guard let index = catalog.profiles.firstIndex(where: { $0.id == id }) else {
            throw ProfileStoreError.profileNotFound
        }
        guard catalog.profiles[index].summary.deviceID == summary.deviceID else {
            throw ProfileStoreError.identityChanged
        }
        try keychain.set(profileJSON, account: catalog.profiles[index].secretAccount)
        catalog.profiles[index].summary = summary
        try save(catalog)
    }

    func delete(id: String) throws {
        var catalog = try catalog()
        guard let index = catalog.profiles.firstIndex(where: { $0.id == id }) else {
            return
        }
        let removed = catalog.profiles.remove(at: index)
        if catalog.selectedProfileID == id {
            catalog.selectedProfileID = catalog.profiles.first?.id
        }
        try save(catalog)
        try keychain.delete(account: removed.secretAccount)
    }

    func hasEnrollmentDraft() throws -> Bool {
        try keychain.get(account: KeychainStore.enrollmentDraftAccount) != nil
    }

    func enrollmentDraft() throws -> String? {
        try keychain.get(account: KeychainStore.enrollmentDraftAccount)
    }

    func saveEnrollmentDraft(_ draft: String) throws {
        try keychain.set(draft, account: KeychainStore.enrollmentDraftAccount)
    }

    func discardEnrollmentDraft() throws {
        try keychain.delete(account: KeychainStore.enrollmentDraftAccount)
    }

    private func save(_ catalog: ProfileCatalog) throws {
        var normalized = catalog
        normalized.normalize()
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        let data = try encoder.encode(normalized)
        guard let encoded = String(data: data, encoding: .utf8) else {
            throw ProfileStoreError.invalidCatalog
        }
        try keychain.set(encoded, account: Self.catalogAccount)
    }

    private func decodeCatalog(_ encoded: String) throws -> ProfileCatalog {
        guard let data = encoded.data(using: .utf8) else {
            throw ProfileStoreError.invalidCatalog
        }
        let catalog = try JSONDecoder().decode(ProfileCatalog.self, from: data)
        guard catalog.version == ProfileCatalog.currentVersion else {
            throw ProfileStoreError.unsupportedCatalogVersion
        }
        return catalog
    }

    private func decodeSummary(_ profileJSON: String) throws -> ProfileSummary {
        let encoded = try MobileCore.profileSummary(profileJSON)
        guard let data = encoded.data(using: .utf8) else {
            throw ProfileStoreError.invalidSummary
        }
        return try JSONDecoder().decode(ProfileSummary.self, from: data)
    }

    private func migrateLegacyProfile() throws -> ProfileCatalog {
        var catalog = ProfileCatalog()
        guard let legacy = try keychain.get(account: KeychainStore.profileAccount) else {
            try save(catalog)
            return catalog
        }
        try MobileCore.validateProfile(legacy)
        let summary = try decodeSummary(legacy)
        let identifier = UUID().uuidString.lowercased()
        let account = Self.profileAccountPrefix + identifier
        let record = StoredProfile(
            id: identifier,
            secretAccount: account,
            displayName: summary.name,
            summary: summary,
            trafficPolicy: .allTraffic,
            importedAt: ISO8601DateFormatter().string(from: Date())
        )
        try keychain.set(legacy, account: account)
        catalog.profiles = [record]
        catalog.selectedProfileID = identifier
        do {
            try save(catalog)
        } catch {
            try? keychain.delete(account: account)
            throw error
        }
        // The catalog is now authoritative. A failed legacy cleanup may leave
        // an unreachable duplicate, but must never invalidate the migrated profile.
        try? keychain.delete(account: KeychainStore.profileAccount)
        return catalog
    }
}

enum ProfileStoreError: LocalizedError {
    case profileNotFound
    case invalidDisplayName
    case identityChanged
    case invalidCatalog
    case unsupportedCatalogVersion
    case invalidSummary

    var errorDescription: String? {
        switch self {
        case .profileNotFound:
            return "The selected Queqiao profile no longer exists."
        case .invalidDisplayName:
            return "Profile names must contain between 1 and 80 characters."
        case .identityChanged:
            return "A renewed profile attempted to change the enrolled device identity."
        case .invalidCatalog:
            return "The encrypted profile catalog is not valid UTF-8."
        case .unsupportedCatalogVersion:
            return "This profile catalog was written by an unsupported Queqiao version."
        case .invalidSummary:
            return "The Queqiao core returned an invalid profile summary."
        }
    }
}
