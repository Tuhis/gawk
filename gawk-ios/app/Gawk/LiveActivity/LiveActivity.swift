import ActivityKit
import Foundation
import os

/// Starts, updates and ends the broadcast's Live Activity (docs/70 D15):
/// started when a broadcast goes live, updated on viewer-count changes at
/// most once a second, ended with the broadcast.
@MainActor
final class LiveActivity: BroadcastActivity {
    /// D15's throttle.
    static let minUpdateInterval: Duration = .seconds(1)

    private(set) var activity: Activity<GawkActivityAttributes>?
    private var state = GawkActivityAttributes.ContentState(viewers: 0, reconnecting: false)
    private var lastUpdate: ContinuousClock.Instant?
    private var pending: Task<Void, Never>?
    private let log = Logger(subsystem: "fi.ioio.gawk", category: "live-activity")

    func started(code: String, link: String, since: Date) {
        ended()
        guard ActivityAuthorizationInfo().areActivitiesEnabled else { return }
        state = .init(viewers: 0, reconnecting: false)
        do {
            activity = try Activity.request(
                attributes: GawkActivityAttributes(code: code, link: link, since: since),
                content: ActivityContent(state: state, staleDate: nil),
                pushType: nil)
            lastUpdate = .now
        } catch {
            log.error("Live Activity: \(String(describing: error))")
        }
    }

    func update(reconnecting: Bool, viewers: UInt32) {
        let next = GawkActivityAttributes.ContentState(viewers: viewers, reconnecting: reconnecting)
        guard activity != nil, next != state else { return }
        state = next
        // At most one update a second: a burst of joins becomes its last
        // count, sent when the second is up.
        guard pending == nil else { return }
        let wait = lastUpdate.map { Self.minUpdateInterval - (.now - $0) } ?? .zero
        pending = Task { [weak self] in
            if wait > .zero { try? await Task.sleep(for: wait) }
            guard let self, !Task.isCancelled, let id = self.activity?.id else { return }
            self.pending = nil
            self.lastUpdate = .now
            await Self.update(id: id, state: self.state)
        }
    }

    func ended() {
        pending?.cancel()
        pending = nil
        guard let id = activity?.id else { return }
        activity = nil
        let state = state
        Task { await Self.end(id: id, state: state) }
    }

    // `Activity` isn't Sendable: only its id and the state leave the main
    // actor, and the activity is looked up where it's used.

    private nonisolated static func find(_ id: String) -> Activity<GawkActivityAttributes>? {
        Activity<GawkActivityAttributes>.activities.first { $0.id == id }
    }

    private nonisolated static func update(id: String, state: GawkActivityAttributes.ContentState) async {
        await find(id)?.update(ActivityContent(state: state, staleDate: nil))
    }

    private nonisolated static func end(id: String, state: GawkActivityAttributes.ContentState) async {
        await find(id)?.end(ActivityContent(state: state, staleDate: nil), dismissalPolicy: .immediate)
    }
}

/// What the Live Activity's End reaches in the app (D15): the capture and
/// the session the app is running.
@MainActor
final class LiveActivityBridge {
    static let shared = LiveActivityBridge()

    private weak var capture: Capture?
    private weak var session: BroadcastSession?

    func attach(capture: Capture, session: BroadcastSession) {
        self.capture = capture
        self.session = session
    }

    /// End from the Lock Screen or the Island: the app's own End.
    func endBroadcast() {
        guard let capture, let session, session.isActive else { return }
        capture.stop(session)
    }
}
