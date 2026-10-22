import Foundation
import Observation

/// R74 PP14 (docs/76 §6.12): a party's local relay that home offered, and
/// whether the next broadcast and the players use it. The decisions are
/// the Rust core's (`probeLocalRelay`, `LocalRelayPrompt`); this holds the
/// app's state around them.
///
/// - Only a **fresh** offer starts a switch (item 43.1): a held PoP lasts
///   the offer's validity and nothing remembered dials on its own.
/// - The candidates are probed in parallel, pinned (item 43.2).
/// - The first dial may be refused before the local-network prompt is
///   answered, so it is retried once after it, and never while the app is
///   not active (TN3179, item 44).
/// - A PoP that never answers is left for this app session (item 43.5);
///   "Use gawk.ioio.fi instead" is remembered per PoP handle (item 43.6).
/// - A live broadcast is never moved (item 43.3): an offer that comes
///   while live is for the next one.
@MainActor
@Observable
final class LocalRelaySwitcher {
    /// The PoP that answered: where the next broadcast publishes and the
    /// players watch, until `until`.
    struct Held: Equatable {
        var handle: String
        var name: String
        var url: String
        var certHashes: [String]
        var until: Date
        /// The home relay that offered it: only a broadcast or player bound
        /// for that server goes there.
        var home: String
    }

    /// Where a broadcast or a player dials.
    struct Target: Equatable {
        var url: String
        var certHashes: [String]
        /// `lr=1`: only toward home.
        var followOffers: Bool
        /// `wall=hidden`: the broadcaster hid from this PoP's wall.
        var wallHidden: Bool
        var pop: Held?
    }

    private(set) var held: Held?
    /// An offer came while live on home: "Your next broadcast will use it."
    private(set) var nextBroadcast = false

    @ObservationIgnored private var unreachable: Set<String> = []
    @ObservationIgnored private var probing: (offer: LocalRelayOffer, home: String)?
    @ObservationIgnored private var prompt: LocalRelayPrompt?
    @ObservationIgnored private var active = true
    @ObservationIgnored private var live = false
    @ObservationIgnored private let defaults: UserDefaults
    /// Tests replace the probe.
    @ObservationIgnored var probe: @Sendable (LocalRelayOffer) async -> LocalRelaySwitch? = { offer in
        await Task.detached { probeLocalRelay(offer: offer) }.value
    }
    /// A player's hook: a PoP answered, so it reconnects there with the
    /// same code (item 43.3: one freeze, once).
    @ObservationIgnored var onSwitch: (@MainActor (Held) -> Void)?

    private static let optOutKey = "localRelayOptOuts"
    private static let wallHiddenKey = "localRelayWallHidden"

    init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
    }

    var optOuts: Set<String> { Set(defaults.stringArray(forKey: Self.optOutKey) ?? []) }
    var wallHidden: Set<String> { Set(defaults.stringArray(forKey: Self.wallHiddenKey) ?? []) }

    /// The held PoP, while its offer is valid.
    func current(now: Date = .now) -> Held? {
        guard let held, held.until > now else { return nil }
        return held
    }

    /// The chip: "Assembly 2026 · local relay" (§7.3) while `home` is the
    /// selected server, or nil.
    func chip(home: String) -> String? {
        current().flatMap { $0.home == home ? "\($0.name) · local relay" : nil }
    }

    /// A fresh offer from `home`, from a probe or a live session.
    func consider(_ offer: LocalRelayOffer, live: Bool, home: String) {
        guard !optOuts.contains(offer.handle), !unreachable.contains(offer.handle) else { return }
        if var h = held, h.handle == offer.handle, h.home == home, h.until > .now {
            h.until = .now.addingTimeInterval(TimeInterval(offer.validitySecs))
            held = h
            if live { nextBroadcast = true }
            return
        }
        guard probing == nil else { return }
        self.live = live
        probing = (offer, home)
        let p = LocalRelayPrompt(active: active)
        prompt = p
        step(p.start())
    }

    /// The scene phase: dials wait while the app is not active, and the
    /// prompt answered (back to active after a failure) is the one retry.
    func setActive(_ active: Bool) {
        self.active = active
        if let p = prompt { step(p.setActive(active: active)) }
    }

    /// The chip's "Use gawk.ioio.fi instead": remembered for this PoP.
    func optOut() {
        guard let h = held else { return }
        var s = optOuts
        s.insert(h.handle)
        defaults.set(Array(s).sorted(), forKey: Self.optOutKey)
        held = nil
        nextBroadcast = false
    }

    /// Hide (`true`) or Show on `handle`'s party wall, remembered for the
    /// next publish there (item 34).
    func setWallHidden(_ hidden: Bool, handle: String) {
        var s = wallHidden
        if hidden { s.insert(handle) } else { s.remove(handle) }
        defaults.set(Array(s).sorted(), forKey: Self.wallHiddenKey)
    }

    /// The PoP refused the session: home for the rest of the app session.
    func refused() {
        if let h = held { unreachable.insert(h.handle) }
        held = nil
    }

    /// Where the next broadcast (or a new player) dials: the held PoP, or
    /// home with `lr=1`.
    func target(home: String, homeCertHashes: [String]) -> Target {
        if let pop = current(), pop.home == home {
            return Target(
                url: pop.url, certHashes: pop.certHashes, followOffers: false,
                wallHidden: wallHidden.contains(pop.handle), pop: pop)
        }
        return Target(url: home, certHashes: homeCertHashes, followOffers: true, wallHidden: false, pop: nil)
    }

    /// A broadcast started: what was said about the next one is spent.
    func broadcastStarted() {
        nextBroadcast = false
    }

    private func step(_ s: LocalRelayDialStep) {
        switch s {
        case .dial:
            dial()
        case .wait, .done:
            break
        case .retryAfter(let ms):
            let p = prompt
            Task { [weak self] in
                try? await Task.sleep(for: .milliseconds(Int(ms)))
                guard let self, let p, self.prompt === p else { return }
                self.step(p.retryDue())
            }
        case .giveUp:
            if let o = probing { unreachable.insert(o.offer.handle) }
            probing = nil
            prompt = nil
        }
    }

    private func dial() {
        guard let probing, let p = prompt else { return }
        let (offer, home) = (probing.offer, probing.home)
        let probe = self.probe
        Task { [weak self] in
            let found = await probe(offer)
            guard let self, self.prompt === p else { return }
            guard let found, found.handle == offer.handle, !self.optOuts.contains(offer.handle) else {
                self.step(p.failed())
                return
            }
            let h = Held(
                handle: found.handle, name: found.name, url: found.url,
                certHashes: found.certHashes,
                until: .now.addingTimeInterval(TimeInterval(offer.validitySecs)), home: home)
            self.held = h
            self.nextBroadcast = self.live
            self.probing = nil
            self.prompt = nil
            _ = p.succeeded()
            self.onSwitch?(h)
        }
    }
}

/// The switch's words (docs/76 §7.3); layout is PP1's to refine.
enum LocalRelayCopy {
    /// "Use gawk.ioio.fi instead": the reference UI's host on the default
    /// fleet, else the selected server's.
    @MainActor
    static func homeHost(_ settings: AppSettings) -> String {
        settings.isDefaultRelay ? hostOf(coreInfo().defaultAppUrl) : hostOf(settings.relayURL)
    }

    static func nextBroadcast(_ name: String) -> String {
        "\(name) has a local relay. Your next broadcast will use it."
    }
}
