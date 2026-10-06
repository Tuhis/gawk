import Foundation

/// What a typed code turned out to be (docs/70 D4).
enum JoinTarget: Equatable {
    case room
    /// Not a room: the player, which says "Streamer offline" if it's
    /// nothing at all.
    case broadcast
}

/// A room session dialed only to ask whether `code` is a room.
@MainActor
protocol RoomProbe: AnyObject {
    func stop()
}

/// Starts a probe for `code` and calls `answer` once with what it learnt:
/// `true` for a room state, `false` for a refusal or an end.
typealias RoomProbeStarter = @MainActor (_ code: String, _ answer: @escaping @MainActor (Bool) -> Void) -> RoomProbe

/// The web's `#/join` (`JoinResolver.tsx`), for the Watch code box (docs/70
/// D4, K7): a room session is the probe. A room state means a room; a
/// refusal or an end means it isn't one; neither within the guard means the
/// player too. The probe is stopped before the answer is returned, and the
/// screen it leads to opens its own session.
@MainActor
enum JoinResolver {
    nonisolated static let guardTime: Duration = .seconds(8)

    static func resolve(
        code: String, guardTime: Duration = guardTime, start: RoomProbeStarter
    ) async -> JoinTarget {
        let state = State()
        return await withCheckedContinuation { continuation in
            state.continuation = continuation
            state.probe = start(code) { isRoom in state.finish(isRoom ? .room : .broadcast) }
            // An answer from inside `start` came before the probe was kept.
            if state.done { state.probe?.stop() }
            state.timer = Task {
                try? await Task.sleep(for: guardTime)
                state.finish(.broadcast)
            }
        }
    }

    @MainActor
    private final class State {
        var continuation: CheckedContinuation<JoinTarget, Never>?
        var probe: RoomProbe?
        var timer: Task<Void, Never>?
        var done = false

        func finish(_ target: JoinTarget) {
            guard !done else { return }
            done = true
            timer?.cancel()
            probe?.stop()
            continuation?.resume(returning: target)
            continuation = nil
        }
    }
}

/// The probe the app uses: the core's `RoomWatcher`, joined with no
/// nickname as the web's is.
@MainActor
final class CoreRoomProbe: RoomProbe {
    private var watcher: RoomWatcher?

    init(relay: String, code: String, insecure: Bool, answer: @escaping @MainActor (Bool) -> Void) {
        watcher = RoomWatcher.start(
            relayUrl: relay, code: code, nickname: "", insecure: insecure,
            listener: ProbeListener(answer: answer))
    }

    func stop() {
        watcher?.stop()
        watcher = nil
    }

    static func starter(relay: String, insecure: Bool) -> RoomProbeStarter {
        { code, answer in CoreRoomProbe(relay: relay, code: code, insecure: insecure, answer: answer) }
    }
}

/// The watcher's callbacks, on the room's thread, hopping to the main actor.
private final class ProbeListener: RoomListener, @unchecked Sendable {
    private let answer: @MainActor (Bool) -> Void

    init(answer: @escaping @MainActor (Bool) -> Void) {
        self.answer = answer
    }

    func onRoom(room: RoomView) {
        Task { @MainActor [answer] in answer(true) }
    }

    func onReconnecting(attempt: UInt32) {}

    func onEnded(reason: String) {
        Task { @MainActor [answer] in answer(false) }
    }
}
