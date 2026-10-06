import Foundation
import Observation

/// Where the app is: the tab, the full-screen player or room player over
/// it, and what a link asked for (docs/70 D1, D4, D21, K8).
@MainActor
@Observable
final class AppRouter {
    enum Tab: Hashable {
        case watch
        case broadcast
        case settings
    }

    /// A player over the tabs. `relay` is a link's server, dialed for this
    /// screen only: its `relay=`, or the default fleet when it has none
    /// (docs/68 D1). A non-default one shows as the server chip (K11).
    /// `nil` dials the server picked in Settings, for a typed code or one of
    /// Your rooms. `nick` is a room link's nickname for this visit.
    enum Screen: Identifiable, Equatable {
        case player(code: String, relay: String?)
        case room(code: String, relay: String?, nick: String?)

        var id: String {
            switch self {
            case .player(let code, let relay): "player \(code) \(relay ?? "")"
            case .room(let code, let relay, _): "room \(code) \(relay ?? "")"
            }
        }
    }

    /// What D21 is asking about: a screen, or a typed code still to look up.
    enum Pending: Equatable {
        case screen(Screen)
        case join(code: String, relay: String, insecure: Bool)
    }

    /// What a `gawk://broadcast` link fills in on Broadcast (docs/68 D4):
    /// never a start.
    struct BroadcastPrefill: Equatable {
        var room: String?
        var nick: String?
        var relay: String?
    }

    var tab: Tab = .watch
    var screen: Screen?
    /// The code the Watch box is looking up (D4), shown on Join.
    private(set) var resolving: String?
    /// D21: what was asked for while broadcasting, waiting for "Watch
    /// anyway".
    var confirmWhileLive: Pending?
    /// The selected server's Edit route.
    static func route(forRelay url: String) -> ServerRoute {
        isDefaultRelay(url: url) ? .builtIn : .saved(url)
    }

    /// The quiet line after a link, naming what it left out (docs/68 D1).
    var linkNotice: String?
    var broadcastPrefill: BroadcastPrefill?

    /// Whether a broadcast is up; set by the root view from the session.
    var isBroadcasting = false
    /// A server's Edit page to open in Settings (D25's Edit secret).
    var settingsRoute: ServerRoute?

    /// Settings, at the selected server's Edit page.
    func editServer(_ route: ServerRoute) {
        tab = .settings
        settingsRoute = route
    }


    /// Opens `screen`, asking first while live (D21).
    func open(_ screen: Screen) {
        if isBroadcasting {
            confirmWhileLive = .screen(screen)
        } else {
            self.screen = screen
        }
    }

    /// The Watch box's Join: a typed code is a room or a broadcast (D4).
    /// While live, D21 asks before anything is dialed.
    func join(code: String, relay: String, insecure: Bool) {
        guard BroadcastCode.isValid(code), resolving == nil else { return }
        if isBroadcasting {
            confirmWhileLive = .join(code: code, relay: relay, insecure: insecure)
        } else {
            resolve(code: code, relay: relay, insecure: insecure)
        }
    }

    /// "Watch anyway".
    func confirm() {
        let pending = confirmWhileLive
        confirmWhileLive = nil
        switch pending {
        case .screen(let s): screen = s
        case .join(let code, let relay, let insecure): resolve(code: code, relay: relay, insecure: insecure)
        case nil: break
        }
    }

    private func resolve(code: String, relay: String, insecure: Bool) {
        resolving = code
        let start = CoreRoomProbe.starter(relay: relay, insecure: insecure)
        Task { [weak self] in
            let target = await JoinResolver.resolve(code: code, start: start)
            guard let self, self.resolving == code else { return }
            self.resolving = nil
            switch target {
            case .room: self.screen = .room(code: code, relay: nil, nick: nil)
            case .broadcast: self.screen = .player(code: code, relay: nil)
            }
        }
    }

    /// A `gawk://` link (K8): `watch` opens the player, `room` the room
    /// player, `broadcast` fills Broadcast in and starts nothing. A link
    /// names its server completely: no `relay=` is the default fleet, never
    /// the server picked in Settings (docs/68 D1).
    func open(url: URL) {
        let parsed: ParsedLink
        do {
            parsed = try parseGawkLink(raw: url.absoluteString)
        } catch {
            linkNotice = "That link isn't a gawk link this app can open."
            return
        }
        linkNotice = Self.notice(dropped: parsed.dropped)
        switch parsed.link {
        case .watch(let id, let relay):
            tab = .watch
            open(.player(code: id, relay: Self.server(relay)))
        case .room(let code, let nick, let relay):
            tab = .watch
            open(.room(code: code, relay: Self.server(relay), nick: nick))
        case .broadcast(let room, let nick, let relay):
            tab = .broadcast
            broadcastPrefill = BroadcastPrefill(room: room, nick: nick, relay: Self.chip(for: relay))
        }
    }

    /// Names what a link left out, never its value (docs/68 D1).
    static func notice(dropped: [String]) -> String? {
        guard !dropped.isEmpty else { return nil }
        return "Left out of the link: \(dropped.joined(separator: ", "))."
    }

    /// A watch or room link's server: its `relay=`, or the default fleet.
    private static func server(_ relay: String?) -> String {
        chip(for: relay) ?? coreInfo().defaultRelayUrl
    }

    /// The server chip a screen's relay shows (K11): none for the default
    /// fleet, which is also no `relay=` at all on a broadcast link.
    static func chip(for relay: String?) -> String? {
        guard let relay, !isDefaultRelay(url: relay) else { return nil }
        return relay
    }
}
