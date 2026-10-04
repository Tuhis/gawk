@testable import Gawk
import XCTest

/// "Stop broadcasting" ends a live broadcast: the path the button takes
/// (`Capture.stop`), with D27's test source feeding a real `Broadcaster`
/// against a local relay. Needs `GAWK_SMOKE_RELAY_URL` (and
/// `GAWK_SMOKE_SECRET`) via `TEST_RUNNER_`-prefixed environment; skipped
/// otherwise.
@MainActor
final class StopBroadcastTests: XCTestCase {
    func testStopBroadcastingEndsALiveBroadcast() async throws {
        let env = ProcessInfo.processInfo.environment
        guard let relay = env["GAWK_SMOKE_RELAY_URL"], !relay.isEmpty else {
            throw XCTSkip("GAWK_SMOKE_RELAY_URL not set")
        }
        initializeCore()
        let identity = IdentityStore(service: "fi.ioio.gawk.tests.\(UUID().uuidString)")
        identity.setSecret(env["GAWK_SMOKE_SECRET"] ?? "", relay: relay)
        let settings = AppSettings(defaults: UserDefaults(suiteName: "fi.ioio.gawk.tests.\(UUID().uuidString)")!)
        settings.addServer(name: "local", url: relay)
        settings.selectedURL = relay
        settings.insecure = true
        let capture = Capture(identity: identity)
        let session = BroadcastSession(identity: identity)

        capture.start(session: session, settings: settings, room: "", test: true)
        var live = false
        for _ in 0..<300 {
            if case .live = session.phase { live = true; break }
            if case .ended(let reason) = session.phase {
                XCTFail("ended before going live: \(reason ?? "-")")
                return
            }
            try await Task.sleep(for: .milliseconds(50))
        }
        guard live else {
            capture.stop(session)
            XCTFail("never went live: \(session.phase)")
            return
        }
        // Frames through the encoder, so a stop meets a running pipeline
        // (the Simulator's software encoder takes a while to start).
        for _ in 0..<300 {
            if (session.counters()?.encoded ?? 0) >= 1 { break }
            try await Task.sleep(for: .milliseconds(50))
        }
        XCTAssertGreaterThanOrEqual(session.counters()?.encoded ?? 0, 1, "the pipeline never encoded")
        let source = try XCTUnwrap(capture.testSource)

        // The button.
        capture.stop(session)
        XCTAssertFalse(source.isRunning, "the source outlived the stop")
        XCTAssertEqual(session.phase, .stopping, "Stop gave no feedback")
        var ended = false
        for _ in 0..<100 {
            if case .ended = session.phase { ended = true; break }
            try await Task.sleep(for: .milliseconds(50))
        }
        XCTAssertTrue(ended, "still \(session.phase) 5 s after Stop")
        XCTAssertFalse(session.isActive)
    }
}
