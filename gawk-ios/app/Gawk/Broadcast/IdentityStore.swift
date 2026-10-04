import Foundation
import Security

/// The Keychain items both the broadcaster and the server picker keep
/// (docs/67 D17): per relay, the last broadcast's code and R17 resume token,
/// and the per-server publish secret (R37, docs/40). All
/// `kSecAttrAccessibleAfterFirstUnlock`, so a broadcast can resume behind a
/// locked screen. One process, so no access group (OD17).
final class IdentityStore: @unchecked Sendable {
    private let service: String

    init(service: String = "fi.ioio.gawk") {
        self.service = service
    }

    struct Identity: Codable, Equatable {
        let code: String
        let token: String
    }

    func load(relay: String) -> Identity? {
        guard let data = read(account: "identity:" + relay) else { return nil }
        return try? JSONDecoder().decode(Identity.self, from: data)
    }

    func save(relay: String, code: String, token: String) {
        guard !code.isEmpty, !token.isEmpty,
            let data = try? JSONEncoder().encode(Identity(code: code, token: token))
        else { return }
        write(account: "identity:" + relay, data: data)
    }

    func forgetIdentity(relay: String) {
        delete(account: "identity:" + relay)
    }

    func secret(relay: String) -> String {
        read(account: "secret:" + relay).flatMap { String(data: $0, encoding: .utf8) } ?? ""
    }

    func setSecret(_ secret: String, relay: String) {
        if secret.isEmpty {
            delete(account: "secret:" + relay)
        } else {
            write(account: "secret:" + relay, data: Data(secret.utf8))
        }
    }

    private func query(account: String) -> [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
    }

    private func read(account: String) -> Data? {
        var q = query(account: account)
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        guard SecItemCopyMatching(q as CFDictionary, &out) == errSecSuccess else { return nil }
        return out as? Data
    }

    private func write(account: String, data: Data) {
        let q = query(account: account)
        let attrs: [String: Any] = [
            kSecValueData as String: data,
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlock,
        ]
        if SecItemUpdate(q as CFDictionary, attrs as CFDictionary) == errSecItemNotFound {
            SecItemAdd(q.merging(attrs) { $1 } as CFDictionary, nil)
        }
    }

    private func delete(account: String) {
        SecItemDelete(query(account: account) as CFDictionary)
    }
}
