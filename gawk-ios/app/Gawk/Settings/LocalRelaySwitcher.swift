import Foundation
import Observation

/// R74 PP14 (docs/76 §6.12): a party's local relay that home offered, and
/// whether the next broadcast, the players and the rooms use it. The
/// decisions are the Rust core's (`probeLocalRelay`, `LocalRelayPrompt`);
/// this holds the app's state around them.
///
/// - Only a **fresh** offer starts a switch (item 43.1): a held PoP lasts
///   the offer's validity and nothing remembered dials on its own.
/// - The candidates are probed in parallel, pinned (item 43.2).
/// - The first dial may be refused before the local-network prompt is
///   answered, so it is retried once after it, and never while the app is
///   not active (TN3179, item 44).
/// - A PoP that never answers is left for this app session (item 43.5).
/// - "Use gawk.ioio.fi instead" holds for the stream it was picked on
///   (item 43.6, OD18): that stream's redials stay on home without `lr=1`,
///   and the next stream (`enterStream`) or the next broadcast follows the
///   PoP's offer again. It lives in memory only; nothing is stored.
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
    /// The PoP handle each home last offered (item 43.7: `lr=1` stops once
    /// the app gave up on it).
    @ObservationIgnored private var lastOffered: [String: String] = [:]
    @ObservationIgnored private var probing: (offer: LocalRelayOffer, home: String)?
    @ObservationIgnored private var prompt: LocalRelayPrompt?
    @ObservationIgnored private var active = true
    @ObservationIgnored private var live = false
    @ObservationIgnored private let defaults: UserDefaults
    /// Tests replace the probe.
    @ObservationIgnored var probe: @Sendable (LocalRelayOffer) async -> LocalRelaySwitch? = { offer in
        await Task.detached { probeLocalRelay(offer: offer) }.value
    }
    /// A player's or room's hook: a PoP answered, so it reconnects there
    /// with the same code (item 43.3: one freeze, once).
    @ObservationIgnored var onSwitch: (@MainActor (Held) -> Void)?
    /// The stream the app's sessions are for (OD18), and the PoP the user
    /// chose "Use … instead" on while there: one, in memory.
    @ObservationIgnored private var currentStream: String?
    @ObservationIgnored private var optedOut: (handle: String, stream: String)?

    /// Where builds before OD18 stored the opt-outs per PoP handle.
    /// Cleared at launch and never read.
    static let legacyOptOutKey = "localRelayOptOuts"
    private static let wallHiddenKey = "localRelayWallHidden"

    /// The streams `enterStream` names: a broadcast, a player's code, a
    /// room's code (the web's `enterStream` keys).
    static let broadcastStream = "broadcast"
    static func viewerStream(_ code: String) -> String { "viewer:\(code)" }
    static func roomStream(_ code: String) -> String { "room:\(code)" }

    init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
        defaults.removeObject(forKey: Self.legacyOptOutKey)
    }

    var wallHidden: Set<String> { Set(defaults.stringArray(forKey: Self.wallHiddenKey) ?? []) }

    /// The PoP opted out of on the current stream, if any.
    var optedOutHandle: String? { optedOut?.handle }

    /// Whether `handle` was opted out of on `stream` (the current stream
    /// when nil). An opt-out picked on one stream never steers another:
    /// a room's "Use … instead" leaves the next broadcast on the PoP.
    private func isOptedOut(_ handle: String, on stream: String? = nil) -> Bool {
        guard let o = optedOut, o.handle == handle else { return false }
        return o.stream == (stream ?? currentStream)
    }

    /// A screen names the stream it dials before its first session: the
    /// redials of one stream name the same one; any other ends the
    /// opt-out, so its offer is followed again (OD18).
    func enterStream(_ key: String) {
        guard key != currentStream else { return }
        currentStream = key
        if let o = optedOut, o.stream != key { optedOut = nil }
    }

    /// The held PoP, while its offer is valid.
    func current(now: Date = .now) -> Held? {
        guard let held, held.until > now else { return nil }
        return held
    }

    /// The chip: "Assembly 2026 · local relay" (§7.3) while `home` is the
    /// selected server, or nil.
    func chip(home: String) -> String? {
        current().flatMap {
            $0.home == home && !isOptedOut($0.handle, on: Self.broadcastStream)
                ? "\($0.name) · local relay" : nil
        }
    }

    /// A fresh offer from `home`, from a probe or a live session.
    func consider(_ offer: LocalRelayOffer, live: Bool, home: String) {
        lastOffered[home] = offer.handle
        guard !unreachable.contains(offer.handle) else { return }
        // Renewing the held PoP comes before the opt-out: an opt-out keeps
        // it for the other streams (`target` and `chip` skip it on the
        // opted-out one), and a renewal never switches anything.
        if var h = held, h.handle == offer.handle, h.home == home, h.until > .now {
            h.until = .now.addingTimeInterval(TimeInterval(offer.validitySecs))
            held = h
            if live { nextBroadcast = true }
            return
        }
        guard !isOptedOut(offer.handle), probing == nil else { return }
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

    /// The chip's "Use gawk.ioio.fi instead", picked on `stream`: held for
    /// that stream only, in memory (OD18). `pop` is the PoP the session is
    /// on, when it has one: a room or player watching the PoP doesn't say
    /// `lr=1` to home, so `held` can expire under it. The held PoP stays
    /// for other streams; `target` and `chip` skip it on this one.
    func optOut(stream: String, pop: Held? = nil) {
        guard let h = pop ?? held else { return }
        enterStream(stream)
        optedOut = (h.handle, stream)
        lastOffered[h.home] = h.handle
        if stream == Self.broadcastStream { nextBroadcast = false }
    }

    /// Hide (`true`) or Show on `handle`'s party wall, remembered for the
    /// next publish there (item 34).
    func setWallHidden(_ hidden: Bool, handle: String) {
        var s = wallHidden
        if hidden { s.insert(handle) } else { s.remove(handle) }
        defaults.set(Array(s).sorted(), forKey: Self.wallHiddenKey)
    }

    /// The PoP refused the session: home for the rest of the app session.
    /// `pop` is the PoP the session was on, which `held` may have outlived.
    func refused(pop: Held? = nil) {
        guard let h = pop ?? held else { return }
        unreachable.insert(h.handle)
        lastOffered[h.home] = h.handle
        if held?.handle == h.handle { held = nil }
    }

    /// Where the next broadcast (or a new player) dials: the held PoP, or
    /// home, with `lr=1` unless the app gave up on the PoP that home last
    /// offered (opted out, or unreachable): only clients that will follow
    /// an offer say it, since home holds their demand for one (item 43.7).
    /// `stream` is the stream it is for, when the caller hasn't entered it
    /// yet (a broadcast enters it once started).
    func target(home: String, homeCertHashes: [String], stream: String? = nil) -> Target {
        if let pop = current(), pop.home == home, !isOptedOut(pop.handle, on: stream) {
            return Target(
                url: pop.url, certHashes: pop.certHashes, followOffers: false,
                wallHidden: wallHidden.contains(pop.handle), pop: pop)
        }
        return Target(
            url: home, certHashes: homeCertHashes, followOffers: followsOffers(home: home, stream: stream),
            wallHidden: false, pop: nil)
    }

    /// Whether a session toward `home` would follow an offer (`lr=1`).
    func followsOffers(home: String, stream: String? = nil) -> Bool {
        guard let last = lastOffered[home] else { return true }
        return !unreachable.contains(last) && !isOptedOut(last, on: stream)
    }

    /// A broadcast started: what was said about the next one is spent. An
    /// opt-out picked on the Broadcast tab holds for this broadcast.
    func broadcastStarted() {
        nextBroadcast = false
        enterStream(Self.broadcastStream)
    }

    /// The broadcast ended: an opt-out picked for it is spent, and the next
    /// broadcast follows the PoP's offer again (OD18).
    func broadcastEnded() {
        if let o = optedOut, o.stream == Self.broadcastStream, currentStream == Self.broadcastStream {
            optedOut = nil
        }
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
            guard let found, found.handle == offer.handle else {
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
            // Opted out on this stream while the probe ran: the PoP answered,
            // so it is held for the other streams, but this one stays home.
            // Not a failure: that would count toward the one retry and could
            // leave the PoP unreachable on every stream (OD18 is per stream).
            if self.isOptedOut(offer.handle) { return }
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
