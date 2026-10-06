@testable import Gawk
import ActivityKit
import XCTest

/// The Live Activity (docs/70 IX8, D15) in the Simulator: a test broadcast
/// starts it, a viewer joining updates its count, End from its intent ends
/// the broadcast, and the activity ends with it. Needs
/// `GAWK_SMOKE_RELAY_URL` (and `GAWK_SMOKE_SECRET`); skipped otherwise.
@MainActor
final class LiveActivityTests: XCTestCase {
    private func activities() -> [Activity<GawkActivityAttributes>] {
        Activity<GawkActivityAttributes>.activities.filter { $0.activityState == .active }
    }

    private func until(_ what: String, timeout: Duration = .seconds(30), _ done: () -> Bool) async throws {
        let deadline = ContinuousClock.now + timeout
        while !done() {
            guard ContinuousClock.now < deadline else { return XCTFail("timed out: \(what)") }
            try await Task.sleep(for: .milliseconds(100))
        }
    }

    func testTheActivityFollowsTheBroadcast() async throws {
        let env = ProcessInfo.processInfo.environment
        guard let relay = env["GAWK_SMOKE_RELAY_URL"], !relay.isEmpty else {
            throw XCTSkip("GAWK_SMOKE_RELAY_URL not set")
        }
        try XCTSkipUnless(ActivityAuthorizationInfo().areActivitiesEnabled, "Live Activities are off here")
        initializeCore()
        let identity = IdentityStore(service: "fi.ioio.gawk.tests.\(UUID().uuidString)")
        let settings = AppSettings(defaults: UserDefaults(suiteName: "fi.ioio.gawk.tests.\(UUID().uuidString)")!)
        settings.addServer(name: "local", url: relay)
        settings.selectedURL = relay
        settings.insecure = true
        identity.setSecret(env["GAWK_SMOKE_SECRET"] ?? "", relay: relay)
        let capture = Capture(identity: identity)
        let session = BroadcastSession(identity: identity)
        let activity = LiveActivity()
        session.activity = activity
        LiveActivityBridge.shared.attach(capture: capture, session: session)

        capture.start(session: session, settings: settings)
        try await until("live") { if case .live = session.phase { true } else { false } }
        guard case .live(let code, let link) = session.phase else { return }
        try await until("an activity") { activities().count == 1 }
        let started = try XCTUnwrap(activities().first)
        XCTAssertEqual(started.attributes.code, code)
        XCTAssertEqual(started.attributes.link, link)
        XCTAssertEqual(started.content.state.viewers, 0)

        // A viewer joins: the relay's count reaches the activity.
        let viewer = Viewer.start(
            options: ViewerOptions(relayUrl: relay, broadcastId: code, preset: .balanced, insecure: true),
            listener: NoViewer())
        defer { viewer.stop() }
        try await until("the count") { activities().first?.content.state.viewers == 1 }

        // End, from the activity's own button.
        _ = try await EndBroadcastIntent().perform()
        try await until("the broadcast ended") { if case .ended = session.phase { true } else { false } }
        try await until("the activity ended") { activities().isEmpty }
    }
}

private final class NoViewer: ViewerListener, @unchecked Sendable {
    func onStatus(status: ViewerStatus) {}
    func onH264(sample: H264Sample) {}
    func onNv12(frame: Nv12Frame) {}
    func onAudio(pcm: PcmBlock) {}
    func onFlush() {}
    func onUnsupportedCodec(codec: String) {}
    func onStats(stats: ViewerStats) {}
}
