import Foundation
import Observation

/// One player's state (docs/67 D15, D22; docs/70 D5): the stream it plays,
/// where from, and what the core says about it.
@MainActor
@Observable
final class WatchModel {
    let code: String
    /// The relay dialed: the server picked in Settings, or a link's, or
    /// the party's local relay the player switched to (R74).
    private(set) var relayUrl: String
    private(set) var insecure: Bool
    /// The relay's pins (R74; empty: the system store).
    private(set) var certHashes: [String]
    /// `lr=1` on the subscribe: watching on home, following offers.
    private(set) var followOffers: Bool
    /// The party's local relay this player watches on, when it switched.
    private(set) var localRelay: LocalRelaySwitcher.Held?
    /// R74: a fresh offer home sent on this player's session.
    @ObservationIgnored var onOffer: (@MainActor (LocalRelayOffer) -> Void)?
    /// R74 (docs/76 item 43.5): the PoP never played the stream, so the
    /// player went back to home; the switch leaves that PoP for the app
    /// session.
    @ObservationIgnored var onLocalRelayFailed: (@MainActor () -> Void)?

    /// Where a player dials: home, kept while it watches on a PoP so a
    /// PoP that never plays falls back there.
    struct Endpoint: Equatable {
        var url: String
        var insecure: Bool
        var certHashes: [String]
    }

    @ObservationIgnored private var home: Endpoint?
    @ObservationIgnored private var liveOnLocalRelay = false

    var preset: Preset = .balanced {
        didSet { engine?.setPreset(preset) }
    }

    private(set) var engine: PlayerEngine?
    private(set) var pip: PictureInPicture?
    private(set) var status: ViewerStatus?
    private(set) var stats: ViewerStats?
    private(set) var unsupportedCodec: String?
    private var isForeground = true

    init(
        code: String, relayUrl: String, insecure: Bool,
        certHashes: [String] = [], followOffers: Bool = false,
        localRelay: LocalRelaySwitcher.Held? = nil, home: Endpoint? = nil
    ) {
        self.code = code
        self.relayUrl = relayUrl
        self.insecure = insecure
        self.certHashes = certHashes
        self.followOffers = followOffers
        self.localRelay = localRelay
        self.home = home
    }

    /// R74 (docs/76 item 43.3): a party's local relay answered, so the
    /// player watches the same code there: one reconnect, once.
    func switchTo(_ pop: LocalRelaySwitcher.Held) {
        guard localRelay == nil, engine != nil else { return }
        home = Endpoint(url: relayUrl, insecure: insecure, certHashes: certHashes)
        liveOnLocalRelay = false
        relayUrl = pop.url
        certHashes = pop.certHashes
        insecure = false
        followOffers = false
        localRelay = pop
        watch()
    }

    /// Starts (or restarts, for Try again) the player.
    func watch() {
        stop()
        let sender = WeakEngine()
        let engine = PlayerEngine { [weak self] event in
            Task { @MainActor in self?.apply(event, from: sender.engine) }
        }
        sender.engine = engine
        let pip = PictureInPicture(layer: engine.displayLayer)
        pip.onActiveChange = { [weak self] _ in self?.updateVideoEnabled() }
        self.engine = engine
        self.pip = pip
        status = .connecting
        engine.start(
            ViewerOptions(
                relayUrl: relayUrl, broadcastId: code, preset: preset, insecure: insecure,
                certHashes: certHashes, followOffers: followOffers))
    }

    /// Leaves: the player and its session stop (the player's X).
    func stop() {
        pip?.stop()
        engine?.stop()
        engine = nil
        pip = nil
        status = nil
        stats = nil
        unsupportedCodec = nil
    }

    /// D22: video keeps rendering only while it can be seen, in the app or
    /// in PiP; audio plays regardless.
    func setForeground(_ foreground: Bool) {
        isForeground = foreground
        updateVideoEnabled()
    }

    private func updateVideoEnabled() {
        engine?.setVideoEnabled(isForeground || (pip?.isActive ?? false))
    }

    /// An event from the current engine; a stopped one still reports its
    /// `Ended(Stopped)`, and that belongs to no session on screen.
    func apply(_ event: PlayerEvent, from sender: PlayerEngine?) {
        guard let engine, sender === engine else { return }
        switch event {
        case .status(let s):
            if localRelay != nil {
                if s == .live { liveOnLocalRelay = true }
                if let home, Self.fallsBackHome(s, wasLive: liveOnLocalRelay) {
                    fallBack(to: home)
                    return
                }
            }
            status = s
        case .stats(let s):
            stats = s
            // R74: the offer rides the session; the stats tick reads it.
            if followOffers, let offer = engine.takeLocalRelayOffer() { onOffer?(offer) }
        case .unsupportedCodec(let c): unsupportedCodec = c
        }
    }

    /// R74 (docs/76 item 43.5): a PoP that ended the session before it ever
    /// played (it lost the stream, or it stopped answering) sends the
    /// player back to home, once. An end after it played is the stream's.
    nonisolated static func fallsBackHome(_ s: ViewerStatus, wasLive: Bool) -> Bool {
        guard !wasLive, case .ended(let reason) = s else { return false }
        return reason == .notFound || reason == .gaveUp
    }

    private func fallBack(to home: Endpoint) {
        localRelay = nil
        self.home = nil
        relayUrl = home.url
        insecure = home.insecure
        certHashes = home.certHashes
        // No lr=1 back home: this player just gave up on the PoP home
        // offered, and home would hold its demand for an offer it will not
        // follow (item 43.7).
        followOffers = false
        onLocalRelayFailed?()
        watch()
    }

    /// Live and playing: the controls may hide (docs/70 D5).
    var isLive: Bool { status == .live && unsupportedCodec == nil }
}

/// The engine an event came from, held weakly: the engine owns the closure
/// that reads it.
private final class WeakEngine: @unchecked Sendable {
    weak var engine: PlayerEngine?
}

/// The player's words. The web viewer's copy where it has some
/// (`useViewerConnection.ts`, `ViewerScreen.tsx`).
enum WatchStatusText {
    /// The amber chip's words while reconnecting (docs/70 D5): a 4002 drain
    /// is a planned relay rollout with an instant retry.
    /// R74 4012 (docs/76 item 28): a LAN party's outside viewing is full,
    /// and the player keeps trying every 20 s with the shared copy.
    static func reconnecting(draining: Bool, partyFull: Bool = false) -> String {
        if partyFull { return popUplinkFullMessage() }
        return draining ? "Stream server is updating" : "RECONNECTING"
    }

    static func connecting(_ code: String) -> String {
        "Connecting to \(code)…"
    }

    /// The card over black for a terminal state (docs/70 D9): a title, a
    /// body, and whether Try again is offered.
    struct Card: Equatable {
        let systemImage: String
        let title: String
        let body: String
        let canRetry: Bool
    }

    static func card(_ reason: ViewerEnd, code: String) -> Card? {
        switch reason {
        case .notFound:
            Card(
                systemImage: "antenna.radiowaves.left.and.right.slash", title: "Streamer offline",
                body: "No one is streaming at code \(code) right now.", canRetry: true)
        case .broadcastEnded:
            Card(systemImage: "stop.circle", title: "Broadcast ended", body: "The stream is over.", canRetry: true)
        case .terminatedByOperator:
            Card(
                systemImage: "hand.raised", title: "Broadcast ended",
                body: "Broadcast ended by a moderator.", canRetry: false)
        case .gaveUp:
            Card(
                systemImage: "wifi.exclamationmark", title: "Lost the stream",
                body: "The streamer may have gone offline.", canRetry: true)
        case .stopped:
            nil
        }
    }

    /// R78 (docs/79 D8): the broadcaster stopped (4014). The player keeps
    /// dialling the code, so the card says it comes back, and there is no
    /// Try again: the core is already trying.
    static let stopped = Card(
        systemImage: "stop.circle", title: "Broadcast ended",
        body: "The broadcaster stopped. Keep this open and the stream comes back if they start again on this code.",
        canRetry: false)

    static func unsupported(_ codec: String) -> Card {
        Card(
            systemImage: "film", title: "Can't play this stream",
            body: "This player can't play this stream's video format (\(codec)).", canRetry: false)
    }
}
