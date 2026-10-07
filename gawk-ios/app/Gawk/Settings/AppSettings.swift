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

/// A room in "Your rooms" (docs/70 K12, docs/60 D8): the rooms this app
/// joined or attached to, saved ones kept. A static room's attach key and a
/// room's creator token are credentials, so they live in the Keychain
/// (`IdentityStore`), bound to `server` like the desktop's (docs/68 D5a).
struct RoomRecord: Codable, Hashable, Identifiable {
    /// The code or static slug, as last joined.
    var code: String
    /// The room's display name when last seen; empty until then.
    var name: String
    /// Starred: listed first, never dropped by the cap.
    var saved: Bool
    var lastJoined: Date
    /// The relay it was joined on. A room exists on one server only.
    var server: String

    var id: String { server + " " + code.lowercased() }
    /// The name, or the code before one is known.
    var title: String { name.isEmpty ? code : name }
}

/// The app's settings (docs/67 D17): plain `UserDefaults`, one process.
/// Per-server secrets live in the Keychain (`IdentityStore`), never here.
@MainActor
@Observable
final class AppSettings {
    /// How many rooms "Your rooms" keeps. Saved rooms and the room just
    /// joined are never dropped to make room (docs/60 D8).
    static let maxRooms = 8

    @ObservationIgnored private let defaults: UserDefaults

    /// Custom servers, in the order added.
    var servers: [Server] { didSet { save() } }
    /// The selected server's URL; empty is the default fleet.
    var selectedURL: String { didSet { save() } }
    /// Diagnostics are off until the user opts in (D23).
    var telemetry: Bool { didSet { save() } }
    /// The room nickname, and the name a broadcast's tile shows.
    var nickname: String { didSet { save() } }
    /// "Your rooms", most recently joined first (K12). Stored under the
    /// `recentRooms` key, which held bare codes before R68.
    private(set) var rooms: [RoomRecord] { didSet { save() } }
    /// The first-run notice was dismissed or a broadcast started (docs/70
    /// D10): shown before the first broadcast only.
    var sawCaptureNote: Bool { didSet { save() } }
    /// DEBUG builds only: accept a local relay's dev certificate.
    var insecure: Bool { didSet { save() } }
    /// The Quality menu's choice (docs/70 D12).
    var quality: QualityChoice { didSet { save() } }

    init(defaults: UserDefaults = .standard, now: Date = .now) {
        self.defaults = defaults
        servers = (defaults.data(forKey: "servers"))
            .flatMap { try? JSONDecoder().decode([Server].self, from: $0) } ?? []
        let selectedURL = defaults.string(forKey: "selectedURL") ?? ""
        self.selectedURL = selectedURL
        telemetry = defaults.bool(forKey: "telemetry")
        nickname = defaults.string(forKey: "nickname") ?? ""
        rooms = Self.loadRooms(defaults, server: selectedURL, now: now)
        sawCaptureNote = defaults.bool(forKey: "sawCaptureNote")
        insecure = defaults.bool(forKey: "insecure")
        quality = defaults.string(forKey: "quality").flatMap(QualityChoice.init(rawValue:)) ?? .auto
        // A pre-R68 list is rewritten as records at once (K12).
        if defaults.data(forKey: "recentRooms") == nil, defaults.object(forKey: "recentRooms") != nil {
            save()
        }
    }

    /// Records, or a pre-R68 list of codes rewritten as unsaved recent rooms
    /// on the server selected at the time, in their order (K12).
    private static func loadRooms(_ defaults: UserDefaults, server: String, now: Date) -> [RoomRecord] {
        if let data = defaults.data(forKey: "recentRooms") {
            return (try? JSONDecoder().decode([RoomRecord].self, from: data)) ?? []
        }
        let codes = defaults.stringArray(forKey: "recentRooms") ?? []
        let relay = server.isEmpty ? coreInfo().defaultRelayUrl : server
        return codes.enumerated().map { i, code in
            RoomRecord(
                code: code, name: "", saved: false,
                lastJoined: now.addingTimeInterval(-Double(i)), server: relay)
        }
    }

    /// The relay to dial: the selection, or the default fleet.
    var relayURL: String {
        selectedURL.isEmpty ? coreInfo().defaultRelayUrl : selectedURL
    }

    /// The compiled-in fleet's name in the server list.
    static let defaultServerName = "gawk"

    /// The display name of the selection.
    var relayName: String {
        servers.first { $0.url == selectedURL }?.name ?? Self.defaultServerName
    }

    var isDefaultRelay: Bool { Gawk.isDefaultRelay(url: relayURL) }

    /// The rooms on the selected server: saved first, then the most recent.
    var yourRooms: [RoomRecord] {
        let here = rooms.filter { $0.server == relayURL }
        return here.filter(\.saved) + here.filter { !$0.saved }
    }

    /// Remembers a room joined on `server` now: it moves to the front and
    /// keeps its star; a known name replaces the old one. The oldest unsaved
    /// rooms beyond the cap go, never the one just joined.
    func noteRoom(_ code: String, name: String = "", server: String, at now: Date = .now) {
        let c = code.trimmingCharacters(in: .whitespaces)
        guard !c.isEmpty else { return }
        var list = rooms
        var record = RoomRecord(code: c, name: name, saved: false, lastJoined: now, server: server)
        if let i = list.firstIndex(where: { $0.id == record.id }) {
            record.saved = list[i].saved
            if name.isEmpty { record.name = list[i].name }
            list.remove(at: i)
        }
        list.insert(record, at: 0)
        while list.count > Self.maxRooms,
            let drop = list.lastIndex(where: { !$0.saved && $0.id != record.id })
        {
            list.remove(at: drop)
        }
        rooms = list
    }

    /// The room's display name, as the relay last named it.
    func nameRoom(_ code: String, name: String, server: String) {
        guard !name.isEmpty,
            let i = rooms.firstIndex(where: { $0.server == server && $0.code.lowercased() == code.lowercased() }),
            rooms[i].name != name
        else { return }
        rooms[i].name = name
    }

    func setSaved(_ record: RoomRecord, _ saved: Bool) {
        guard let i = rooms.firstIndex(where: { $0.id == record.id }) else { return }
        rooms[i].saved = saved
    }

    func removeRoom(_ record: RoomRecord) {
        rooms.removeAll { $0.id == record.id }
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

    /// Edit changed a saved server's relay: the server keeps its place and
    /// its selection under the new URL. Its secret is the caller's to move.
    func moveServer(from old: String, to new: String, name: String) {
        guard new.hasPrefix("https://"), let i = servers.firstIndex(where: { $0.url == old }) else { return }
        servers.removeAll { $0.url == new && $0.url != old }
        let at = servers.firstIndex(where: { $0.url == old }) ?? i
        servers[at] = Server(name: name.isEmpty ? new : name, url: new)
        if selectedURL == old { selectedURL = new }
        rooms = rooms.map { r in
            var r = r
            if r.server == old { r.server = new }
            return r
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
        defaults.set(try? JSONEncoder().encode(rooms), forKey: "recentRooms")
        defaults.set(sawCaptureNote, forKey: "sawCaptureNote")
        defaults.set(insecure, forKey: "insecure")
        defaults.set(quality.rawValue, forKey: "quality")
    }
}
