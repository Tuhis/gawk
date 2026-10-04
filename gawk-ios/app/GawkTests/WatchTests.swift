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
}
