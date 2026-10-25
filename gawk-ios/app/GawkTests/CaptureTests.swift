import XCTest
@testable import Gawk

/// Capture follows the broadcast: when the core ends it, the sources stop,
/// and a new Start never leaves the old source running beside the new one.
@MainActor
final class CaptureTests: XCTestCase {
    private func make() -> (Capture, BroadcastSession, AppSettings) {
        let identity = IdentityStore(service: "fi.ioio.gawk.tests.\(UUID().uuidString)")
        let suite = "fi.ioio.gawk.tests.\(UUID().uuidString)"
        let settings = AppSettings(defaults: UserDefaults(suiteName: suite)!)
        return (Capture(identity: identity), BroadcastSession(identity: identity), settings)
    }

    func testTheCoreEndingTheBroadcastStopsCapture() {
        let (capture, session, settings) = make()
        capture.start(session: session, settings: settings)
        XCTAssertTrue(capture.isCapturing)
        session.apply(.ended(error: "refused", reclaimStatus: 401))
        XCTAssertFalse(capture.isCapturing, "the source outlived the broadcast")
        capture.stop(session)
    }

    /// docs/79 D9: the user stopping the recording from the system UI (no
    /// reason) is a deliberate stop and ends the broadcast for its viewers;
    /// a capture failure leaves it to the relay's grace.
    func testAUserStoppedRecordingEndsTheBroadcast() {
        let (capture, session, settings) = make()
        capture.start(session: session, settings: settings)
        capture.captureEnded(nil, session)
        XCTAssertTrue(session.endedBroadcast, "a stopped recording kept the relay's grace")
        XCTAssertFalse(capture.isCapturing)
    }

    func testACaptureFailureKeepsTheGrace() {
        let (capture, session, settings) = make()
        capture.start(session: session, settings: settings)
        capture.captureEnded("The stream stopped.", session)
        XCTAssertFalse(session.endedBroadcast, "a capture failure ended the broadcast")
        XCTAssertFalse(capture.isCapturing)
    }

    func testStartingAgainStopsTheOldSource() throws {
        let (capture, session, settings) = make()
        capture.start(session: session, settings: settings)
        let first = try XCTUnwrap(capture.testSource)
        session.apply(.ended(error: nil, reclaimStatus: nil))
        capture.start(session: session, settings: settings)
        XCTAssertFalse(first.isRunning, "the old source still runs")
        XCTAssertTrue(capture.isCapturing)
        capture.stop(session)
    }
}
