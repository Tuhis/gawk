import Foundation
import Observation

/// What the app knows about reaching a server (docs/70 D22, D24): the
/// core's `/echo` probe (docs/40), run off the main thread, one at a time
/// per server.
@MainActor
@Observable
final class ServerProbes {
    enum State: Equatable {
        case checking
        case reachable(rttMs: UInt32, operatorName: String?)
        case unreachable
    }

    private(set) var states: [String: State] = [:]
    /// The servers whose last answer said they screen streams for explicit
    /// content (R40, docs/71 D13): Settings marks them "Content screening".
    private(set) var screening: Set<String> = []
    @ObservationIgnored private var running: Set<String> = []
    /// Tests replace the probe. It is the core's probe of home (R74): with
    /// the server's pins, `lr=1`, and home's local-relay offer when it
    /// has one for this device (docs/76 §6.12).
    @ObservationIgnored var probe: @Sendable (_ url: String, _ insecure: Bool, _ certHashes: [String]) async -> HomeProbe = {
        url, insecure, certHashes in
        await Task.detached { probeHome(relayUrl: url, insecure: insecure, certHashes: certHashes) }.value
    }
    /// R74: a fresh offer a probe brought back, for the switch (the app
    /// root sets it to `LocalRelaySwitcher.consider`).
    @ObservationIgnored var onOffer: (@MainActor (_ url: String, _ offer: LocalRelayOffer) -> Void)?

    func state(_ url: String) -> State? { states[url] }

    /// Whether `url`'s last answer carried the screening notice.
    func screens(_ url: String) -> Bool { screening.contains(url) }

    /// Probes `url` unless a probe of it is already out. A new result
    /// replaces the old one; until then the old one stays on screen.
    func refresh(_ url: String, insecure: Bool, certHashes: [String] = []) {
        guard !running.contains(url) else { return }
        running.insert(url)
        if states[url] == nil || states[url] == .unreachable { states[url] = .checking }
        Task {
            let home = await probe(url, insecure, certHashes)
            running.remove(url)
            if let offer = home.offer { onOffer?(url, offer) }
            switch home.probe {
            case .reachable(let rtt, let name, let screens):
                states[url] = .reachable(rttMs: rtt, operatorName: name)
                if screens { screening.insert(url) } else { screening.remove(url) }
            case .unreachable:
                states[url] = .unreachable
                // An unreachable server says nothing either way.
                screening.remove(url)
            }
        }
    }

    /// The row's words (D22): "Checking…", "Connected · 24 ms", "Can't
    /// reach server". The operator's name goes beside the host, never in
    /// place of it (docs/40 F6), so it follows the time.
    static func describe(_ state: State?) -> String {
        switch state {
        case nil, .checking: "Checking…"
        case .reachable(let rtt, let name):
            ["Connected · \(rtt) ms", name].compactMap { $0 }.joined(separator: " · ")
        case .unreachable: "Can't reach server"
        }
    }
}

/// The host of an https URL, for strips and banners ("relay.example.net").
func hostOf(_ url: String) -> String {
    URLComponents(string: url)?.host ?? url
}
