import Foundation
import Observation

/// A room being watched (docs/70 D19, D20; docs/67 D21): the core's
/// `RoomWatcher` for the roster, and a player per stream, at most four
/// playing at once.
@MainActor
@Observable
final class RoomPlayerModel {
    enum Layout: String, CaseIterable {
        case grid = "Grid"
        case focus = "Focus"
    }

    /// docs/67 D21's decode budget.
    static let maxPlaying = 4

    let code: String
    let relayUrl: String
    let insecure: Bool
    private(set) var nickname: String

    private(set) var room: RoomView?
    private(set) var reconnectAttempt: UInt32?
    /// Over for good, with the core's reason.
    private(set) var endedReason: String?

    var layout: Layout = .grid {
        didSet { reconcile() }
    }
    /// The stream Focus shows; the first one until a tap picks another.
    var focusedId: String? {
        didSet { reconcile() }
    }
    var preset: Preset = .balanced {
        didSet { engines.values.forEach { $0.setPreset(preset) } }
    }

    /// One engine per stream that has played, kept while the stream is in
    /// the room so a paused tile holds its last frame.
    private(set) var engines: [String: PlayerEngine] = [:]
    /// The streams playing, oldest swap first.
    private(set) var playing: [String] = []

    @ObservationIgnored private var watcher: RoomWatcher?
    /// Called with each new room picture, and whether it's the first.
    @ObservationIgnored var onRoom: ((_ room: RoomView, _ first: Bool) -> Void)?

    init(code: String, relayUrl: String, insecure: Bool, nickname: String) {
        self.code = code
        self.relayUrl = relayUrl
        self.insecure = insecure
        self.nickname = nickname
    }

    var tiles: [RoomTile] { room?.tiles ?? [] }

    /// Streams, counting away ones, as the web's count does.
    var streamingCount: Int { tiles.count }

    var focused: RoomTile? {
        tiles.first { $0.broadcastId == focusedId } ?? tiles.first
    }

    func isPlaying(_ id: String) -> Bool { playing.contains(id) }

    func start() {
        guard watcher == nil else { return }
        endedReason = nil
        let listener = Listener(model: self)
        watcher = RoomWatcher.start(
            relayUrl: relayUrl, code: code, nickname: nickname, insecure: insecure, listener: listener)
    }

    /// Leaves the room: the roster session and every player stop.
    func stop() {
        watcher?.stop()
        watcher = nil
        engines.values.forEach { $0.stop() }
        engines = [:]
        playing = []
    }

    /// A tap on a tile: Focus focuses it; Grid swaps a paused one in for the
    /// one that has played longest (D19).
    func tap(_ id: String) {
        switch layout {
        case .focus:
            focusedId = id
        case .grid:
            guard !playing.contains(id) else { return }
            if playing.count >= Self.maxPlaying {
                pauseEngine(playing.removeFirst())
            }
            playing.append(id)
            play(id)
        }
    }

    /// People's Edit (D20): the room hears the new name.
    func setNickname(_ name: String) {
        nickname = name
        watcher?.setNickname(nickname: name)
    }

    /// People's tap on a stream: Focus on it.
    func focus(_ id: String) {
        focusedId = id
        layout = .focus
    }

    func apply(_ room: RoomView) {
        let first = self.room == nil
        self.room = room
        reconnectAttempt = nil
        onRoom?(room, first)
        reconcile()
    }

    func reconnecting(_ attempt: UInt32) {
        reconnectAttempt = attempt
    }

    func ended(_ reason: String) {
        endedReason = reason
        stop()
    }

    /// Which streams play: in Grid the ones already playing that are still
    /// here, topped up in room order to four; in Focus the focused one only.
    private func reconcile() {
        let ids = tiles.map(\.broadcastId)
        for gone in engines.keys where !ids.contains(gone) {
            engines.removeValue(forKey: gone)?.stop()
        }
        let want: [String]
        switch layout {
        case .focus:
            want = focused.map { [$0.broadcastId] } ?? []
        case .grid:
            var kept = playing.filter(ids.contains)
            for id in ids where kept.count < Self.maxPlaying && !kept.contains(id) {
                kept.append(id)
            }
            want = kept
        }
        for id in playing where !want.contains(id) { pauseEngine(id) }
        for id in want where !playing.contains(id) { play(id) }
        playing = want
    }

    private func play(_ id: String) {
        if let engine = engines[id] {
            engine.start(options(id))
            return
        }
        // A tile's own state is the roster's (live or away), so the
        // player's events have nowhere to go.
        let engine = PlayerEngine { _ in }
        engines[id] = engine
        engine.start(options(id))
    }

    private func pauseEngine(_ id: String) {
        engines[id]?.pause()
    }

    private func options(_ id: String) -> ViewerOptions {
        ViewerOptions(relayUrl: relayUrl, broadcastId: id, preset: preset, insecure: insecure)
    }

    /// The core's roster callbacks, hopping to the main actor.
    private final class Listener: RoomListener, @unchecked Sendable {
        private weak var model: RoomPlayerModel?

        @MainActor init(model: RoomPlayerModel) { self.model = model }

        func onRoom(room: RoomView) {
            Task { @MainActor [weak model] in model?.apply(room) }
        }

        func onReconnecting(attempt: UInt32) {
            Task { @MainActor [weak model] in model?.reconnecting(attempt) }
        }

        func onEnded(reason: String) {
            Task { @MainActor [weak model] in model?.ended(reason) }
        }
    }
}
