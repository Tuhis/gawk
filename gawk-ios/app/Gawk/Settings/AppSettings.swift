import Foundation
import Observation

/// One relay the user can pick (R37, docs/40): a name and an https origin.
/// The default fleet is not stored; it is the compiled-in default (CLAUDE.md:
/// the official deployment is the default target).
struct Server: Codable, Hashable, Identifiable {
    var name: String
    var url: String
    var id: String { url }
}

/// The app's settings (docs/67 D17): plain `UserDefaults`, one process.
/// Per-server secrets live in the Keychain (`IdentityStore`), never here.
@MainActor
@Observable
final class AppSettings {
    @ObservationIgnored private let defaults: UserDefaults

    /// Custom servers, in the order added.
    var servers: [Server] { didSet { save() } }
    /// The selected server's URL; empty is the default fleet.
    var selectedURL: String { didSet { save() } }
    /// Diagnostics are off until the user opts in (D23).
    var telemetry: Bool { didSet { save() } }
    /// The room nickname, and the name a broadcast's tile shows.
    var nickname: String { didSet { save() } }
    /// Rooms joined or attached to recently, most recent first (D21).
    var recentRooms: [String] { didSet { save() } }
    /// The notification note was seen (D18): shown before the first
    /// broadcast only.
    var sawCaptureNote: Bool { didSet { save() } }
    /// DEBUG builds only: accept a local relay's dev certificate.
    var insecure: Bool { didSet { save() } }

    init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
        servers = (defaults.data(forKey: "servers"))
            .flatMap { try? JSONDecoder().decode([Server].self, from: $0) } ?? []
        selectedURL = defaults.string(forKey: "selectedURL") ?? ""
        telemetry = defaults.bool(forKey: "telemetry")
        nickname = defaults.string(forKey: "nickname") ?? ""
        recentRooms = defaults.stringArray(forKey: "recentRooms") ?? []
        sawCaptureNote = defaults.bool(forKey: "sawCaptureNote")
        insecure = defaults.bool(forKey: "insecure")
    }

    /// The relay to dial: the selection, or the default fleet.
    var relayURL: String {
        selectedURL.isEmpty ? coreInfo().defaultRelayUrl : selectedURL
    }

    /// The display name of the selection.
    var relayName: String {
        servers.first { $0.url == selectedURL }?.name ?? "gawk (default)"
    }

    var isDefaultRelay: Bool { Gawk.isDefaultRelay(url: relayURL) }

    /// Remembers a room code, most recent first, at most eight.
    func noteRoom(_ code: String) {
        let c = code.trimmingCharacters(in: .whitespaces)
        guard !c.isEmpty else { return }
        recentRooms = Array(([c] + recentRooms.filter { $0 != c }).prefix(8))
    }

    /// Adds (or renames) a custom server; the URL is the key.
    func addServer(name: String, url: String) {
        let u = url.trimmingCharacters(in: .whitespaces)
        guard u.hasPrefix("https://") else { return }
        let n = name.trimmingCharacters(in: .whitespaces)
        if let i = servers.firstIndex(where: { $0.url == u }) {
            servers[i].name = n.isEmpty ? servers[i].name : n
        } else {
            servers.append(Server(name: n.isEmpty ? u : n, url: u))
        }
    }

    func removeServer(_ url: String) {
        servers.removeAll { $0.url == url }
        if selectedURL == url { selectedURL = "" }
    }

    private func save() {
        defaults.set(try? JSONEncoder().encode(servers), forKey: "servers")
        defaults.set(selectedURL, forKey: "selectedURL")
        defaults.set(telemetry, forKey: "telemetry")
        defaults.set(nickname, forKey: "nickname")
        defaults.set(recentRooms, forKey: "recentRooms")
        defaults.set(sawCaptureNote, forKey: "sawCaptureNote")
        defaults.set(insecure, forKey: "insecure")
    }
}
