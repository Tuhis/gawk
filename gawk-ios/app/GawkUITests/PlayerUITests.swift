import XCTest

/// The player (docs/70 IX3) on a live broadcast: video only until a tap,
/// the code pill, the settings menu, the stats drawer and the server chip.
/// Needs `GAWK_UI_RELAY_URL` and `GAWK_UI_BROADCAST_ID`.
@MainActor
final class PlayerUITests: XCTestCase {
    private var app: XCUIApplication!
    private var code = ""

    override func setUp() async throws {
        continueAfterFailure = false
        XCUIDevice.shared.orientation = .portrait
        code = try UIEnv.require("GAWK_UI_RELAY_URL", "GAWK_UI_BROADCAST_ID")[1]
        // The controls stay up long enough to be worked without racing the
        // idle hide, except in the test of the idle hide itself.
        let idle = name.contains("HideAfterThreeSeconds") ? [] : ["-gawkControlIdle", "30"]
        app = launchApp(extra: idle)
    }

    /// Opens the stream and shows its controls.
    private func watch(_ link: String? = nil, reveal: Bool = true) {
        openLink(link ?? UIEnv.link("gawk://watch/\(code)"), in: app)
        XCTAssertTrue(app.descendants(matching: .any)["player.live"].waitForExistence(timeout: UIWait.media), "live")
        if reveal { revealControls(app) }
    }

    private var close: XCUIElement { app.buttons["player.close"] }

    /// D5: the controls hide 3 s after the last touch, and a tap shows them.
    /// XCUITest's own latency only lengthens what it measures, so the hide
    /// is timed from the tap for the lower bound: at least 2.5 s (not at
    /// once). The upper bound is timed from when the controls were seen,
    /// which is after the app's timer started, so a slow runner's delay in
    /// seeing them doesn't count against it: hidden at most 6 s later.
    func testTheControlsHideAfterThreeSecondsAndATapShowsThem() {
        watch(reveal: false)
        XCTAssertTrue(wait(for: close, hittable: false, timeout: UIWait.step), "hidden after the idle time")
        shot("player-video-only", app)
        let tapped = Date()
        app.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.5)).tap()
        XCTAssertTrue(wait(for: close, timeout: UIWait.step), "a tap shows them")
        let seen = Date()
        shot("player-controls", app)
        XCTAssertTrue(wait(for: close, hittable: false, timeout: UIWait.step), "and they hide again")
        let hidden = Date()
        XCTAssertGreaterThanOrEqual(hidden.timeIntervalSince(tapped), 2.5, "they stayed for the idle time")
        XCTAssertLessThanOrEqual(hidden.timeIntervalSince(seen), 6, "they hid after the idle time")
    }

    /// D5a, OD4: a tap on the pill shows the check, and the code stays in
    /// place. (The runner may not read the app's pasteboard; that the pill
    /// copies the join link is `WatchTests`'.)
    func testTheCodePillCopiesAndKeepsTheCode() {
        watch()
        let pill = app.buttons["player.codePill"]
        XCTAssertEqual(pill.label, "Copy link to \(code)")
        pill.tap()
        XCTAssertTrue(pill.staticTexts[code].exists, "the code is still shown")
        XCTAssertFalse(app.staticTexts["Copied"].exists)
        XCTAssertTrue(pill.images["checkmark"].exists, "the check")
        shot("player-copied", app)
    }

    /// D6: Latency applies at once, Share is there, and Copy link isn't.
    func testTheSettingsMenu() {
        watch()
        app.buttons["player.settings"].tap()
        XCTAssertTrue(app.buttons["Lowest latency"].waitForExistence(timeout: UIWait.step))
        XCTAssertTrue(app.buttons["Share…"].exists)
        XCTAssertTrue(app.buttons["Stats"].exists)
        XCTAssertFalse(app.buttons["Copy link"].exists, "Copy link is the pill's")
        shot("player-menu", app)
        tapMenuItem("Lowest latency", in: app)
        revealControls(app)
        app.buttons["player.settings"].tap()
        XCTAssertTrue(app.buttons["Lowest latency"].waitForExistence(timeout: UIWait.step))
        XCTAssertTrue(app.buttons["Lowest latency"].isSelected, "the preset stuck")
    }

    /// D7: Stats opens small with the video still taking touches, and pulls
    /// all the way out.
    func testStatsOpensSmallAndPullsToFull() {
        watch()
        app.buttons["player.settings"].tap()
        tapMenuItem("Stats", in: app)
        XCTAssertTrue(app.staticTexts["Playout delay"].waitForExistence(timeout: UIWait.step))
        XCTAssertFalse(app.staticTexts["Frames"].exists, "small: the tiles only")
        shot("player-stats-small", app)
        // Above the drawer the video still takes a tap (controls toggle).
        let top = app.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.3))
        top.tap()
        XCTAssertTrue(app.staticTexts["Playout delay"].exists, "the drawer stayed")
        XCTAssertTrue(app.buttons["player.close"].exists, "and the player under it")
        let drawer = app.descendants(matching: .any)["stats.drawer"]
        drawer.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.05))
            .press(forDuration: 0.1, thenDragTo: app.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.05)))
        let frames = app.staticTexts.matching(NSPredicate(format: "label ==[c] %@", "Frames")).firstMatch
        XCTAssertTrue(frames.waitForExistence(timeout: UIWait.step))
        XCTAssertTrue(app.buttons["stats.copy"].exists)
        shot("player-stats-full", app)
    }

    /// X leaves: back on Watch, nothing playing.
    func testCloseLeavesThePlayer() {
        watch()
        close.tap()
        let gone = expectation(for: NSPredicate(format: "exists == false"), evaluatedWith: close)
        wait(for: [gone], timeout: UIWait.step)
        XCTAssertFalse(app.descendants(matching: .any)["player.live"].exists)
        XCTAssertTrue(wait(for: app.buttons["watch.join"]), "back on Watch")
    }

    /// K11: a link's non-default relay (the local one) shows as a chip
    /// under the top row.
    func testALinksServerShowsAsAChip() {
        watch()
        XCTAssertTrue(app.descendants(matching: .any)["player.serverChip"].exists)
        shot("player-server-chip", app)
    }

    func testLandscape() {
        watch()
        XCUIDevice.shared.orientation = .landscapeLeft
        defer { XCUIDevice.shared.orientation = .portrait }
        sleep(2)
        revealControls(app)
        XCTAssertGreaterThan(app.windows.firstMatch.frame.width, app.windows.firstMatch.frame.height, "turned")
        shot("player-landscape", app)
    }
}
