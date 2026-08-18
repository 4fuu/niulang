import Foundation
import Security

enum KeychainStoreError: LocalizedError {
    case missingAccessGroup
    case invalidData
    case status(OSStatus)

    var errorDescription: String? {
        switch self {
        case .missingAccessGroup:
            return "The shared Keychain access group is not configured."
        case .invalidData:
            return "The stored Queqiao identity is not valid UTF-8."
        case let .status(status):
            return (SecCopyErrorMessageString(status, nil) as String?) ?? "Keychain error \(status)"
        }
    }
}

struct KeychainStore: Sendable {
    static let profileAccount = "client-profile"
    static let enrollmentDraftAccount = "enrollment-draft"

    private let service = "io.github.bojieli.queqiao.mobile"
    private let accessGroup: String

    init(bundle: Bundle = .main) throws {
        guard let value = bundle.object(forInfoDictionaryKey: "QueqiaoKeychainAccessGroup") as? String,
              !value.isEmpty,
              !value.contains("$(") else {
            throw KeychainStoreError.missingAccessGroup
        }
        accessGroup = value
    }

    func set(_ value: String, account: String) throws {
        guard let data = value.data(using: .utf8), !data.isEmpty else {
            throw KeychainStoreError.invalidData
        }
        let lookup = baseQuery(account: account)
        let attributes: [CFString: Any] = [
            kSecValueData: data,
            kSecAttrAccessible: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
            kSecAttrSynchronizable: false
        ]
        let update = SecItemUpdate(lookup as CFDictionary, attributes as CFDictionary)
        if update == errSecSuccess {
            return
        }
        guard update == errSecItemNotFound else {
            throw KeychainStoreError.status(update)
        }
        var insertion = lookup
        attributes.forEach { insertion[$0] = $1 }
        let added = SecItemAdd(insertion as CFDictionary, nil)
        guard added == errSecSuccess else {
            throw KeychainStoreError.status(added)
        }
    }

    func get(account: String) throws -> String? {
        var query = baseQuery(account: account)
        query[kSecReturnData] = true
        query[kSecMatchLimit] = kSecMatchLimitOne
        var result: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &result)
        if status == errSecItemNotFound {
            return nil
        }
        guard status == errSecSuccess else {
            throw KeychainStoreError.status(status)
        }
        guard let data = result as? Data, let value = String(data: data, encoding: .utf8) else {
            throw KeychainStoreError.invalidData
        }
        return value
    }

    func delete(account: String) throws {
        let status = SecItemDelete(baseQuery(account: account) as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else {
            throw KeychainStoreError.status(status)
        }
    }

    private func baseQuery(account: String) -> [CFString: Any] {
        [
            kSecClass: kSecClassGenericPassword,
            kSecAttrService: service,
            kSecAttrAccount: account,
            kSecAttrAccessGroup: accessGroup,
            kSecAttrSynchronizable: false
        ]
    }
}
