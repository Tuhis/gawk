import XCTest

/// docs/70 IX9, D27: `performAccessibilityAudit()` passes on every screen,
/// and the lists lay out at the largest accessibility size (AX5) without
/// cutting a label off. The players need `GAWK_UI_RELAY_URL` with
/// `GAWK_UI_BROADCAST_ID` and `GAWK_UI_ROOM_CODE`.
@MainActor
final class AccessibilityUITests: XCTestCase {
    override func setUp() async throws {
        continueAfterFailure = true
        XCUIDevice.shared.orientation = .portrait
    }

    /// Every audit but the two about text size, which
    /// `testTheListsAtTheLargestTextSize` runs at the real size: at the
    /// default size the auditor guesses at larger ones, and flags list
    /// headers and footers that its own resize scrolled out of view.
    ///
    /// Contrast is skipped where the auditor can't judge it: over video,
    /// where every control is glass on whatever the stream shows (`overVideo`),
    /// on inactive controls (WCAG 1.4.3 exempts them), and on rows the tab
    /// bar or a pinned button covers until they're scrolled up.
    ///
    /// Where the stream is in view (`overVideo`, or a sheet below it:
    /// `videoInView`), text the auditor reads off the picture with no element
    /// behind it is the stream's: the test video (ffmpeg's `testsrc2`) draws
    /// its own clock and frame number. A room's tiles left in view above a
    /// sheet are video too, so their name chips are judged as over video:
    /// their contrast is whatever the stream draws behind them, and a tile
    /// still waiting for its first frame (a loaded runner) fails it. A tile
    /// (one element) is matched by its identifier. Its chip is a node the
    /// auditor finds with no identifier, so it's matched by place, inside
    /// the part of a tile above the sheet: a tile can run under the sheet's
    /// top edge, and the sheet's own header must not borrow its exemption
    /// (review of #480).
    private func audit(
        _ app: XCUIApplication, _ screen: String, overVideo: Bool = false, videoInView: Bool = false,
        file: StaticString = #filePath, line: UInt = #line
    ) {
        let floor = chromeTop(app)
        let video = overVideo || videoInView
        let tilesAboveSheet = videoInView ? self.tilesAboveSheet(app) : []
        do {
            try app.performAccessibilityAudit(for: .all.subtracting([.dynamicType, .textClipped])) { issue in
                if video, issue.auditType == .elementDetection, issue.element == nil { return true }
                if issue.auditType == .contrast {
                    if overVideo { return true }
                    guard let element = issue.element else { return false }
                    if !element.isEnabled { return true }
                    if element.frame.maxY > floor { return true }
                    if videoInView, element.identifier.hasPrefix("room.tile.") { return true }
                    if tilesAboveSheet.contains(where: { $0.contains(element.frame) }) { return true }
                }
                print("AUDIT \(screen) [\(issue.auditType.rawValue)]: \(issue.compactDescription) | \(issue.detailedDescription) | \(issue.element?.label ?? "-") \(issue.element?.frame ?? .zero)")
                return false
            }
        } catch {
            XCTFail("\(screen): \(error)", file: file, line: line)
        }
    }

    /// The room tiles' frames, each cut off at the open sheet's top: the
    /// lowest navigation bar, as the sheet's sits below any behind it.
    /// With no bar found, nothing is left, so a miss only makes the audit
    /// stricter.
    private func tilesAboveSheet(_ app: XCUIApplication) -> [CGRect] {
        let sheetTop = app.navigationBars.allElementsBoundByIndex.map(\.frame.minY).max() ?? 0
        return app.descendants(matching: .any)
            .matching(NSPredicate(format: "identifier BEGINSWITH 'room.tile.'"))
            .allElementsBoundByIndex
            .compactMap { tile in
                // Edges compared, not a height: a CGRect with a negative
                // height normalizes to the strip below the sheet's top.
                let f = tile.frame
                let bottom = min(f.maxY, sheetTop)
                guard bottom > f.minY else { return nil }
                return CGRect(x: f.minX, y: f.minY, width: f.width, height: bottom - f.minY)
            }
    }

    /// Where the bottom chrome starts: the tab bar, a pinned button with
    /// its fade, or the home indicator's strip.
    private func chromeTop(_ app: XCUIApplication) -> CGFloat {
        var top = app.windows.firstMatch.frame.maxY - 34
        let bar = app.tabBars.firstMatch
        if canTap(bar) { top = min(top, bar.frame.minY) }
        for id in ["broadcast.goLive", "broadcast.end"] {
            let pinned = app.buttons[id]
            if canTap(pinned) { top = min(top, pinned.frame.minY - 18) }
        }
        return top
    }

    func testTheTabs() {
        let app = launchApp(relay: false)
        XCTAssertTrue(app.buttons["watch.join"].waitForExistence(timeout: UIWait.step))
        audit(app, "Watch")
        app.tabBars.buttons["Broadcast"].tap()
        XCTAssertTrue(app.buttons["broadcast.goLive"].waitForExistence(timeout: UIWait.step))
        audit(app, "Broadcast")
        app.buttons["broadcast.room"].tap()
        XCTAssertTrue(app.textFields["room.input"].waitForExistence(timeout: UIWait.step))
        audit(app, "Add to a room")
        app.buttons["Close"].firstMatch.tap()
        app.tabBars.buttons["Settings"].tap()
        XCTAssertTrue(app.buttons["server.info.gawk"].waitForExistence(timeout: UIWait.step))
        audit(app, "Settings")
        app.buttons["server.info.gawk"].tap()
        XCTAssertTrue(app.navigationBars["Edit server"].waitForExistence(timeout: UIWait.step))
        audit(app, "Edit server")
    }

    func testLiveAndTheRoomSheet() throws {
        _ = try UIEnv.require("GAWK_UI_RELAY_URL")
        let app = launchApp()
        app.tabBars.buttons["Broadcast"].tap()
        app.buttons["broadcast.room"].tap()
        app.buttons["room.create"].tap()
        app.buttons["broadcast.goLive"].tap()
        XCTAssertTrue(app.buttons["broadcast.manageRoom"].waitForExistence(timeout: UIWait.media))
        audit(app, "Live")
        app.buttons["broadcast.manageRoom"].tap()
        XCTAssertTrue(app.buttons["room.leave"].waitForExistence(timeout: UIWait.step))
        audit(app, "Room sheet")
        app.buttons["Close"].firstMatch.tap()
        app.buttons["broadcast.end"].tap()
        XCTAssertTrue(app.descendants(matching: .any)["broadcast.summary"].waitForExistence(timeout: 15))
        audit(app, "Ended")
    }

    func testThePlayers() throws {
        let vars = try UIEnv.require("GAWK_UI_RELAY_URL", "GAWK_UI_BROADCAST_ID", "GAWK_UI_ROOM_CODE")
        let app = launchApp(extra: ["-gawkControlIdle", "120"])
        openLink(UIEnv.link("gawk://watch/\(vars[1])"), in: app)
        XCTAssertTrue(app.descendants(matching: .any)["player.live"].waitForExistence(timeout: UIWait.media))
        revealControls(app)
        audit(app, "Player", overVideo: true)
        app.buttons["player.settings"].tap()
        tapMenuItem("Stats", in: app)
        XCTAssertTrue(app.descendants(matching: .any)["stats.drawer"].waitForExistence(timeout: UIWait.step))
        audit(app, "Stats", videoInView: true)
        app.swipeDown()
        app.buttons["player.close"].tap()

        openLink(UIEnv.link("gawk://room/\(vars[2])"), in: app)
        XCTAssertTrue(app.buttons["room.people"].waitForExistence(timeout: UIWait.media))
        audit(app, "Room player", overVideo: true)
        app.buttons["room.people"].tap()
        XCTAssertTrue(app.buttons["people.edit"].waitForExistence(timeout: UIWait.step))
        audit(app, "People", videoInView: true)
    }

    /// AX5: the lists grow and no label is cut off.
    func testTheListsAtTheLargestTextSize() {
        let app = XCUIApplication()
        app.launchArguments = ["-gawkReset", "-UIPreferredContentSizeCategoryName", "UICTContentSizeCategoryAccessibilityXXXL"]
        app.launch()
        func check(_ screen: String) {
            do {
                try app.performAccessibilityAudit(for: [.dynamicType, .textClipped]) { issue in
                    print("AUDIT AX5 \(screen): \(issue.compactDescription) | \(issue.element?.label ?? "-") \(issue.element?.frame ?? .zero)")
                    return false
                }
            } catch {
                XCTFail("AX5 \(screen): \(error)")
            }
        }
        XCTAssertTrue(app.buttons["watch.join"].waitForExistence(timeout: UIWait.step))
        check("Watch")
        shot("ax5-watch", app)
        app.tabBars.buttons["Broadcast"].tap()
        check("Broadcast")
        shot("ax5-broadcast", app)
        // At AX5 the Room row is below the fold. It's brought up in short
        // drags that leave the list still, until it clears the tab bar and
        // the pinned Go live: a swipe's fling can still be moving the list
        // when the tap lands, and then the tap only stops it.
        let room = app.buttons["broadcast.room"]
        let window = app.windows.firstMatch
        for _ in 0..<12 where !(canTap(room) && room.frame.maxY <= chromeTop(app)) {
            window.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.6)).press(
                forDuration: 0.05, thenDragTo: window.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.4)),
                withVelocity: .slow, thenHoldForDuration: 0.3)
        }
        room.tap()
        XCTAssertTrue(app.textFields["room.input"].waitForExistence(timeout: UIWait.step))
        check("Add to a room")
        shot("ax5-room-sheet", app)
        app.buttons["Close"].firstMatch.tap()
        app.tabBars.buttons["Settings"].tap()
        check("Settings")
        shot("ax5-settings", app)
        app.buttons["server.info.gawk"].tap()
        check("Edit server")
    }
}
