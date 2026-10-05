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
    private func audit(
        _ app: XCUIApplication, _ screen: String, overVideo: Bool = false,
        file: StaticString = #filePath, line: UInt = #line
    ) {
        let floor = chromeTop(app)
        do {
            try app.performAccessibilityAudit(for: .all.subtracting([.dynamicType, .textClipped])) { issue in
                if issue.auditType == .contrast {
                    if overVideo { return true }
                    guard let element = issue.element else { return false }
                    if !element.isEnabled { return true }
                    if element.frame.maxY > floor { return true }
                }
                print("AUDIT \(screen): \(issue.compactDescription) | \(issue.detailedDescription) | \(issue.element?.label ?? "-") \(issue.element?.frame ?? .zero)")
                return false
            }
        } catch {
            XCTFail("\(screen): \(error)", file: file, line: line)
        }
    }

    /// Where the bottom chrome starts: the tab bar, a pinned button with
    /// its fade, or the home indicator's strip.
    private func chromeTop(_ app: XCUIApplication) -> CGFloat {
        var top = app.windows.firstMatch.frame.maxY - 34
        let bar = app.tabBars.firstMatch
        if bar.exists, bar.isHittable { top = min(top, bar.frame.minY) }
        for id in ["broadcast.goLive", "broadcast.end"] {
            let pinned = app.buttons[id]
            if pinned.exists, pinned.isHittable { top = min(top, pinned.frame.minY - 18) }
        }
        return top
    }

    func testTheTabs() {
        let app = launchApp(relay: false)
        XCTAssertTrue(app.buttons["watch.join"].waitForExistence(timeout: 10))
        audit(app, "Watch")
        app.tabBars.buttons["Broadcast"].tap()
        XCTAssertTrue(app.buttons["broadcast.goLive"].waitForExistence(timeout: 5))
        audit(app, "Broadcast")
        app.buttons["broadcast.room"].tap()
        XCTAssertTrue(app.textFields["room.input"].waitForExistence(timeout: 5))
        audit(app, "Add to a room")
        app.buttons["Close"].firstMatch.tap()
        app.tabBars.buttons["Settings"].tap()
        XCTAssertTrue(app.buttons["server.info.gawk"].waitForExistence(timeout: 5))
        audit(app, "Settings")
        app.buttons["server.info.gawk"].tap()
        XCTAssertTrue(app.navigationBars["Edit server"].waitForExistence(timeout: 5))
        audit(app, "Edit server")
    }

    func testLiveAndTheRoomSheet() throws {
        _ = try UIEnv.require("GAWK_UI_RELAY_URL")
        let app = launchApp()
        app.tabBars.buttons["Broadcast"].tap()
        app.buttons["broadcast.room"].tap()
        app.buttons["room.create"].tap()
        app.buttons["broadcast.goLive"].tap()
        XCTAssertTrue(app.buttons["broadcast.manageRoom"].waitForExistence(timeout: 40))
        audit(app, "Live")
        app.buttons["broadcast.manageRoom"].tap()
        XCTAssertTrue(app.buttons["room.leave"].waitForExistence(timeout: 5))
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
        XCTAssertTrue(app.descendants(matching: .any)["player.live"].waitForExistence(timeout: 20))
        revealControls(app)
        audit(app, "Player", overVideo: true)
        app.buttons["player.settings"].tap()
        app.buttons["Stats"].tap()
        XCTAssertTrue(app.descendants(matching: .any)["stats.drawer"].waitForExistence(timeout: 5))
        audit(app, "Stats")
        app.swipeDown()
        app.buttons["player.close"].tap()

        openLink(UIEnv.link("gawk://room/\(vars[2])"), in: app)
        XCTAssertTrue(app.buttons["room.people"].waitForExistence(timeout: 20))
        audit(app, "Room player", overVideo: true)
        app.buttons["room.people"].tap()
        XCTAssertTrue(app.buttons["people.edit"].waitForExistence(timeout: 10))
        audit(app, "People")
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
        XCTAssertTrue(app.buttons["watch.join"].waitForExistence(timeout: 10))
        check("Watch")
        shot("ax5-watch", app)
        app.tabBars.buttons["Broadcast"].tap()
        check("Broadcast")
        shot("ax5-broadcast", app)
        // At AX5 the Room row is below the fold.
        for _ in 0..<4 where !app.buttons["broadcast.room"].isHittable { app.swipeUp() }
        app.buttons["broadcast.room"].tap()
        XCTAssertTrue(app.textFields["room.input"].waitForExistence(timeout: 5))
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
