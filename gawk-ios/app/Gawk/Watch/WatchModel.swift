import Foundation
import Observation

/// The Watch screen's state (docs/67 D15, D20, D22): one player at a time.
@MainActor
@Observable
final class WatchModel {
    var code = ""
    var preset: Preset = .balanced {
        didSet { engine?.setPreset(preset) }
    }
    #if DEBUG
    /// A local relay for the Simulator (phase S, D26). Empty means the
    /// compiled-in default.
    var relayOverride = ""
    /// Skip certificate verification, for a dev relay's self-signed cert.
    var insecure = false
    #endif

    private(set) var engine: PlayerEngine?
    private(set) var pip: PictureInPicture?
    private(set) var watchingCode: String?
    private(set) var status: ViewerStatus?
    private(set) var stats: ViewerStats?
    private(set) var unsupportedCodec: String?
    private var isForeground = true

    let defaultRelayUrl = coreInfo().defaultRelayUrl

    var canWatch: Bool { BroadcastCode.isValid(code) }

    var relayUrl: String {
        #if DEBUG
        let override = relayOverride.trimmingCharacters(in: .whitespaces)
        if !override.isEmpty { return override }
        #endif
        return defaultRelayUrl
    }

    private var isInsecure: Bool {
        #if DEBUG
        return insecure
        #else
        return false
        #endif
    }

    func watch() {
        guard canWatch else { return }
        stop()
        let engine = PlayerEngine { [weak self] event in
            Task { @MainActor in self?.apply(event) }
        }
        let pip = PictureInPicture(layer: engine.displayLayer)
        pip.onActiveChange = { [weak self] _ in self?.updateVideoEnabled() }
        self.engine = engine
        self.pip = pip
        watchingCode = code
        status = .connecting
        engine.start(ViewerOptions(
            relayUrl: relayUrl, broadcastId: code, preset: preset, insecure: isInsecure))
    }

    func stop() {
        pip?.stop()
        engine?.stop()
        engine = nil
        pip = nil
        watchingCode = nil
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

    private func apply(_ event: PlayerEvent) {
        guard engine != nil else { return }
        switch event {
        case .status(let s): status = s
        case .stats(let s): stats = s
        case .unsupportedCodec(let c): unsupportedCodec = c
        }
    }
}

/// The status line's words. The SPA's copy where it has some
/// (`useViewerConnection.ts`, `ViewerScreen.tsx`).
enum WatchStatusText {
    static func describe(_ status: ViewerStatus) -> String {
        switch status {
        case .connecting:
            return "Connecting…"
        case .live:
            return "Live"
        case .reconnecting(let attempt, _, let draining):
            // A 4002 drain is a planned relay rollout with an instant retry.
            return draining
                ? "Stream server is updating — reconnecting…"
                : "Reconnecting — attempt \(attempt)…"
        case .ended(let reason):
            switch reason {
            case .broadcastEnded: return "Broadcast ended. The stream is over."
            case .terminatedByOperator: return "Broadcast ended by a moderator."
            case .notFound: return "Streamer offline. No one is streaming at this code right now."
            case .gaveUp: return "Lost the stream. The streamer may have gone offline."
            case .stopped: return "Stopped."
            }
        }
    }
}
