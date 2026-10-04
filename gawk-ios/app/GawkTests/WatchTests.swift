@testable import Gawk
import XCTest

/// The Watch screen's pure pieces (docs/67 D20).
final class WatchTests: XCTestCase {
    func testCodesFollowTheSpasRules() {
        XCTAssertEqual(BroadcastCode.sanitize("abc234"), "ABC234")
        // No 0/O/1/I/L, nothing beyond six, separators dropped.
        XCTAssertEqual(BroadcastCode.sanitize("o0-1il ab cd ef gh"), "ABCDEF")
        XCTAssertTrue(BroadcastCode.isValid("ABC234"))
        XCTAssertFalse(BroadcastCode.isValid("abc234"), "valid means already normalized")
        XCTAssertFalse(BroadcastCode.isValid("ABC23"))
        XCTAssertFalse(BroadcastCode.isValid("ABC2340"))
        XCTAssertFalse(BroadcastCode.isValid("ABCD1O"))
    }

    func testTheStatusLineNamesTheDrainAndTheEnd() {
        XCTAssertEqual(WatchStatusText.describe(.reconnecting(attempt: 2, delayMs: 0, draining: true)),
                       "Stream server is updating — reconnecting…")
        XCTAssertEqual(WatchStatusText.describe(.reconnecting(attempt: 2, delayMs: 500, draining: false)),
                       "Reconnecting — attempt 2…")
        XCTAssertEqual(WatchStatusText.describe(.ended(reason: .terminatedByOperator)),
                       "Broadcast ended by a moderator.")
        XCTAssertEqual(WatchStatusText.describe(.live), "Live")
    }

    /// Watching a new code stops the old player, whose core still reports
    /// `Ended(Stopped)` afterwards; that must not land on the new session.
    @MainActor
    func testAStoppedPlayersEventsDontReachTheNextSession() throws {
        let model = WatchModel()
        model.relayOverride = "https://127.0.0.1:9"
        model.code = "ABC234"
        model.watch()
        let old = try XCTUnwrap(model.engine)
        model.code = "DEF567"
        model.watch()
        model.apply(.status(.ended(reason: .stopped)), from: old)
        model.apply(.stats(ViewerStats(
            offsetMs: 0, jitterMs: nil, rttMs: nil, framesCompleted: 1, framesDropped: 0,
            framesRecoveredByParity: 0, gapResyncs: 0, dropsToLive: 0, viewerCount: nil)), from: old)
        XCTAssertEqual(model.status, .connecting)
        XCTAssertNil(model.stats)
        model.apply(.status(.live), from: model.engine)
        XCTAssertEqual(model.status, .live)
        model.stop()
    }
}
