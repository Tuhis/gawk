import Foundation
import Observation

/// One player's state (docs/67 D15, D22; docs/70 D5): the stream it plays,
/// where from, and what the core says about it.
@MainActor
@Observable
final class WatchModel {
    let code: String
    /// The relay dialed: the server picked in Settings, or a link's.
    let relayUrl: String
    let insecure: Bool

    var preset: Preset = .balanced {
        didSet { engine?.setPreset(preset) }
    }

    private(set) var engine: PlayerEngine?
    private(set) var pip: PictureInPicture?
    private(set) var status: ViewerStatus?
    private(set) var stats: ViewerStats?
    private(set) var unsupportedCodec: String?
    private var isForeground = true

    init(code: String, relayUrl: String, insecure: Bool) {
        self.code = code
        self.relayUrl = relayUrl
        self.insecure = insecure
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
        engine.start(ViewerOptions(relayUrl: relayUrl, broadcastId: code, preset: preset, insecure: insecure))
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
        case .status(let s): status = s
        case .stats(let s): stats = s
        case .unsupportedCodec(let c): unsupportedCodec = c
        }
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
    static func reconnecting(draining: Bool) -> String {
        draining ? "Stream server is updating" : "RECONNECTING"
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

    static func unsupported(_ codec: String) -> Card {
        Card(
            systemImage: "film", title: "Can't play this stream",
            body: "This player can't play this stream's video format (\(codec)).", canRetry: false)
    }
}
