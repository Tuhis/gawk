import XCTest
@testable import Gawk

/// ScreenCaptureKit's callbacks, as `ScreenCapture` acts on them: the
/// system's own stop ends the broadcast, and nothing after our stop can
/// start capture again or end it twice.
final class CaptureEventsTests: XCTestCase {
    func testThePickerStartsAStream() {
        var e = CaptureEvents()
        XCTAssertEqual(e.handle(.picked), .startStream)
        // Re-picking mid-capture replaces the stream.
        XCTAssertEqual(e.handle(.picked), .startStream)
    }

    func testTheSystemsStopEndsTheBroadcast() {
        var e = CaptureEvents()
        _ = e.handle(.picked)
        XCTAssertEqual(e.handle(.streamStopped(nil)), .end(nil))
        var c = CaptureEvents()
        _ = c.handle(.picked)
        XCTAssertEqual(c.handle(.pickerCancelled), .end(nil))
    }

    func testAFailureEndsItWithTheReason() {
        var e = CaptureEvents()
        XCTAssertEqual(e.handle(.pickerFailed("no")), .end("no"))
        var s = CaptureEvents()
        _ = s.handle(.picked)
        XCTAssertEqual(s.handle(.startFailed("busy")), .end("busy"))
    }

    func testNothingActsAfterOurStop() {
        var e = CaptureEvents()
        _ = e.handle(.picked)
        e.stop()
        XCTAssertEqual(e.handle(.picked), .ignore, "a late picker update restarted capture")
        XCTAssertEqual(e.handle(.pickerCancelled), .ignore)
        XCTAssertEqual(e.handle(.streamStopped(nil)), .ignore)
        XCTAssertEqual(e.handle(.startFailed("x")), .ignore)
    }

    func testAnEndIsReportedOnce() {
        var e = CaptureEvents()
        _ = e.handle(.picked)
        XCTAssertEqual(e.handle(.streamStopped("lost")), .end("lost"))
        XCTAssertEqual(e.handle(.pickerCancelled), .ignore)
        XCTAssertEqual(e.handle(.picked), .ignore)
    }
}
