@testable import Gawk
import CoreMedia
import XCTest

/// The player's clock mapping, presented reporting and backpressure rule
/// (docs/67 D15).
final class PlayoutTests: XCTestCase {
    // MARK: HostClockMapping

    func testPresentAtMapsByTheSampledDifference() {
        let m = HostClockMapping(viewerMs: 1_000, hostSeconds: 50_000)
        XCTAssertEqual(m.hostTime(forPresentAtMs: 1_000).seconds, 50_000, accuracy: 1e-9)
        XCTAssertEqual(m.hostTime(forPresentAtMs: 1_150).seconds, 50_000.150, accuracy: 1e-9)
        XCTAssertEqual(m.hostTime(forPresentAtMs: 990).seconds, 49_999.990, accuracy: 1e-9)
        XCTAssertEqual(m.presentAtMs(forHostTime: CMTime(seconds: 50_000.034, preferredTimescale: 1_000_000_000)),
                       1_034, accuracy: 1e-6)
    }

    func testTheSampleTakesTheMidpointOfTheHostReadings() {
        var hostReads = [CMTime(seconds: 10, preferredTimescale: 1000), CMTime(seconds: 10.002, preferredTimescale: 1000)]
        let m = HostClockMapping.sample(viewerClock: { 777 }, hostClock: { hostReads.removeFirst() })
        XCTAssertEqual(m.viewerMs, 777)
        XCTAssertEqual(m.hostSeconds, 10.001, accuracy: 1e-9)
    }

    /// The real clocks: the core's clock (Rust `Instant`) and the host clock
    /// are both `mach_absolute_time`, so two samples a while apart describe
    /// the same mapping, to well under a frame.
    func testTheCoresClockRunsOnTheHostClock() {
        let first = HostClockMapping.sample()
        Thread.sleep(forTimeInterval: 0.2)
        let second = HostClockMapping.sample()
        let predicted = first.hostTime(forPresentAtMs: second.viewerMs).seconds
        XCTAssertEqual(predicted, second.hostSeconds, accuracy: 0.001)
    }

    func testPtsKeepMicrosecondOrder() {
        let m = HostClockMapping(viewerMs: 0, hostSeconds: 1_000_000)
        let a = m.hostTime(forPresentAtMs: 16.666)
        let b = m.hostTime(forPresentAtMs: 16.667)
        XCTAssertLessThan(a, b)
    }

    // MARK: PresentationLog

    private func t(_ s: Double) -> CMTime { CMTime(seconds: s, preferredTimescale: 1000) }

    func testOnScreenIsTheNewestFrameAlreadyDue() {
        var log = PresentationLog()
        XCTAssertNil(log.onScreen(at: t(1)))
        log.record(pts: t(1.0), timestampUs: 100)
        log.record(pts: t(1.1), timestampUs: 200)
        log.record(pts: t(1.2), timestampUs: 300)
        XCTAssertNil(log.onScreen(at: t(0.9)), "nothing is due yet")
        XCTAssertEqual(log.pending(after: t(0.9)), 3)
        XCTAssertEqual(log.onScreen(at: t(1.15)), 200)
        XCTAssertEqual(log.pending(after: t(1.15)), 1)
        // What's been passed is forgotten; the frame on screen stays.
        XCTAssertEqual(log.onScreen(at: t(1.15)), 200)
        XCTAssertEqual(log.onScreen(at: t(5)), 300)
        log.removeAll()
        XCTAssertNil(log.onScreen(at: t(5)))
    }

    /// D15's drop-to-live rule fires when the newest frame received runs
    /// more than 2 × offset ahead of the one reported on screen. On a clean
    /// 30 fps stream that gap is the offset plus the frame interval plus how
    /// stale the report is, so the report cadence must keep it under 2 ×
    /// offset even near the 50 ms floor, or the player drops to live on a
    /// healthy stream (seen in the Simulator at a 100 ms cadence and a
    /// ~100 ms offset).
    func testTheReportCadenceNeverLooksBehindLiveOnACleanStream() {
        let offset = 0.060
        let frame = 1.0 / 30
        let cadence = PlayerEngine.tickInterval
        var log = PresentationLog()
        var reported: UInt64?
        var worst = 0.0
        var nextFrame = 0.0
        var newest: UInt64 = 0
        var tick = 0.0
        while tick < 5 {
            // Frames received up to `tick`, each due `offset` after arrival.
            while nextFrame <= tick {
                newest = UInt64(nextFrame * 1_000_000)
                log.record(pts: t(nextFrame + offset), timestampUs: newest)
                nextFrame += frame
            }
            if let shown = log.onScreen(at: t(tick)) { reported = shown }
            // The core checks on its own tick, any time before our next
            // report: the newest frame by then arrived just before it.
            if let reported, tick > 1 {
                let atCheck = (((tick + cadence) / frame).rounded(.up) - 1) * frame
                worst = max(worst, atCheck - Double(reported) / 1_000_000)
            }
            tick += cadence
        }
        XCTAssertLessThan(worst, 2 * offset, "worst gap \(worst) s at a \(cadence) s cadence")
    }

    // MARK: RendererBackpressure

    func testAQueueDeeperThanAnyOffsetResyncs() {
        var bp = RendererBackpressure()
        XCTAssertFalse(bp.observe(lateness: 0, pending: RendererBackpressure.maxPending, now: 0))
        XCTAssertTrue(bp.observe(lateness: 0, pending: RendererBackpressure.maxPending + 1, now: 0.1))
    }

    func testAPathThatStaysBehindResyncsAfterTheLimit() {
        var bp = RendererBackpressure()
        XCTAssertFalse(bp.observe(lateness: 0.3, pending: 0, now: 10))
        XCTAssertFalse(bp.observe(lateness: 0.4, pending: 0, now: 10.4))
        XCTAssertTrue(bp.observe(lateness: 0.3, pending: 0, now: 10.5))
    }

    func testABriefLateBurstIsNormal() {
        var bp = RendererBackpressure()
        XCTAssertFalse(bp.observe(lateness: 0.4, pending: 0, now: 0))
        XCTAssertFalse(bp.observe(lateness: 0.01, pending: 0, now: 0.3))
        XCTAssertFalse(bp.observe(lateness: 0.4, pending: 0, now: 0.4))
        XCTAssertFalse(bp.observe(lateness: 0.4, pending: 0, now: 0.8))
    }

    func testResyncsAreRateLimited() {
        var bp = RendererBackpressure()
        XCTAssertTrue(bp.observe(lateness: 0, pending: 100, now: 0))
        XCTAssertFalse(bp.observe(lateness: 0, pending: 100, now: 0.5))
        XCTAssertTrue(bp.observe(lateness: 0, pending: 100, now: 1.0))
    }
}
