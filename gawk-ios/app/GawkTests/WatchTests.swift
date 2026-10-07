@testable import Gawk
import AVFoundation
import XCTest

/// The Watch screen's and the player's pure pieces (docs/67 D20, docs/70
/// D4, D5, D7, D9).
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

    func testTheReconnectingChipNamesTheDrain() {
        XCTAssertEqual(WatchStatusText.reconnecting(draining: true), "Stream server is updating")
        XCTAssertEqual(WatchStatusText.reconnecting(draining: false), "RECONNECTING")
        XCTAssertEqual(WatchStatusText.connecting("K7XQ2M"), "Connecting to K7XQ2M…")
    }

    /// D9: every terminal state's card, and Try again everywhere but a
    /// moderator's end.
    func testTheCardsForAStreamThatIsntThere() {
        let offline = WatchStatusText.card(.notFound, code: "K7XQ2M")
        XCTAssertEqual(offline?.title, "Streamer offline")
        XCTAssertEqual(offline?.body, "No one is streaming at code K7XQ2M right now.")
        XCTAssertEqual(offline?.canRetry, true)
        let ended = WatchStatusText.card(.broadcastEnded, code: "K7XQ2M")
        XCTAssertEqual(ended?.title, "Broadcast ended")
        XCTAssertEqual(ended?.body, "The stream is over.")
        let banned = WatchStatusText.card(.terminatedByOperator, code: "K7XQ2M")
        XCTAssertEqual(banned?.body, "Broadcast ended by a moderator.")
        XCTAssertEqual(banned?.canRetry, false, "a moderator's end has Close only")
        XCTAssertNil(WatchStatusText.card(.stopped, code: "K7XQ2M"), "our own stop shows nothing")
        let vp9 = WatchStatusText.unsupported("VP9")
        XCTAssertEqual(vp9.body, "This player can't play this stream's video format (VP9).")
        XCTAssertFalse(vp9.canRetry)
    }

    /// D5a, OD12: the pill copies the join link, the one Live's Copy link
    /// copies, and the room's chip the room link.
    func testThePillsCopyTheLinks() {
        XCTAssertEqual(watchLink(broadcastId: "K7XQ2M"), "https://gawk.ioio.fi/#/view/K7XQ2M")
        XCTAssertEqual(roomLink(code: "K7XQ2M", grant: nil), "https://gawk.ioio.fi/#/room/K7XQ2M")
    }

    /// D7: values formatted before display, "—" for unknown.
    func testTheStatsDrawersNumbers() {
        let none = StatsText.rows(nil, nil)
        XCTAssertEqual(none.tiles.map(\.value), ["—", "—", "—", "—"])
        let s = ViewerStats(
            offsetMs: 82.4, jitterMs: 3.6, rttMs: nil, framesCompleted: 1234, framesDropped: 2,
            framesRecoveredByParity: 1, gapResyncs: 0, dropsToLive: 0, viewerCount: 12)
        let rows = StatsText.rows(s, PlayerEngine.Counters(videoEnqueued: 1200, audioEnqueued: 900))
        XCTAssertEqual(rows.tiles.map(\.label), ["Playout delay", "Jitter", "Round trip", "Watching"])
        XCTAssertEqual(rows.tiles.map(\.value), ["82 ms", "4 ms", "—", "12"])
        XCTAssertEqual(rows.frames.first?.value, 1234.formatted())
        XCTAssertEqual(rows.renderer.map(\.label), ["Video samples", "Audio blocks", "Video dropped", "Renderer resyncs"])
        XCTAssertTrue(rows.plainText.contains("Playout delay: 82 ms"))
        XCTAssertTrue(rows.plainText.contains("Renderer resyncs: 0"))
    }

    /// Restarting the player (Try again) stops the old one, whose core still
    /// reports `Ended(Stopped)` afterwards; that must not land on the new
    /// session.
    @MainActor
    func testAStoppedPlayersEventsDontReachTheNextSession() throws {
        let model = WatchModel(code: "ABC234", relayUrl: "https://127.0.0.1:9", insecure: true)
        model.watch()
        let old = try XCTUnwrap(model.engine)
        model.watch()
        model.apply(.status(.ended(reason: .stopped)), from: old)
        model.apply(.stats(ViewerStats(
            offsetMs: 0, jitterMs: nil, rttMs: nil, framesCompleted: 1, framesDropped: 0,
            framesRecoveredByParity: 0, gapResyncs: 0, dropsToLive: 0, viewerCount: nil)), from: old)
        XCTAssertEqual(model.status, .connecting)
        XCTAssertNil(model.stats)
        model.apply(.status(.live), from: model.engine)
        XCTAssertEqual(model.status, .live)
        XCTAssertTrue(model.isLive)
        model.stop()
    }

    /// The player's X: the engine and its session stop.
    @MainActor
    func testStopReleasesTheEngine() {
        let model = WatchModel(code: "ABC234", relayUrl: "https://127.0.0.1:9", insecure: true)
        model.watch()
        XCTAssertNotNil(model.engine)
        model.stop()
        XCTAssertNil(model.engine)
        XCTAssertNil(model.pip)
        XCTAssertNil(model.status)
    }

    /// D8: PiP keeps starting on its own when the app leaves with the
    /// player up.
    @MainActor
    func testPictureInPictureStartsAutomaticallyFromInline() throws {
        let pip = PictureInPicture(layer: AVSampleBufferDisplayLayer())
        try XCTSkipUnless(pip.isSupported, "no PiP on this device")
        XCTAssertTrue(pip.startsAutomaticallyFromInline)
    }

    /// The idle rule (§3.4): the controls hide after the idle time unless
    /// pinned, and a tap brings them back.
    @MainActor
    func testControlsHideAfterTheIdleTime() async throws {
        let controls = ControlsVisibility()
        controls.idle = .milliseconds(200)
        controls.show()
        XCTAssertTrue(controls.visible)
        try await Task.sleep(for: .milliseconds(400))
        XCTAssertFalse(controls.visible)
        controls.toggle()
        XCTAssertTrue(controls.visible)
        controls.pinned = true
        try await Task.sleep(for: .milliseconds(400))
        XCTAssertTrue(controls.visible, "pinned stays")
        controls.pinned = false
        try await Task.sleep(for: .milliseconds(400))
        XCTAssertFalse(controls.visible)
    }
}
