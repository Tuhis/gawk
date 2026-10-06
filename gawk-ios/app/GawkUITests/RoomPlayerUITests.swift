import XCTest

/// The room player (docs/70 IX6) on `room-fixtures.sh`'s rooms: Grid at
/// five, the swap, Focus, People and the nickname, and an away stream.
/// Needs `GAWK_UI_RELAY_URL` and `GAWK_UI_ROOM_CODE` (and
/// `GAWK_UI_AWAY_ROOM` for the away card).
@MainActor
final class RoomPlayerUITests: XCTestCase {
    private var app: XCUIApplication!

    override func setUp() async throws {
        continueAfterFailure = false
        XCUIDevice.shared.orientation = .portrait
        _ = try UIEnv.require("GAWK_UI_RELAY_URL", "GAWK_UI_ROOM_CODE")
        app = launchApp(extra: ["-gawkControlIdle", "60", "-gawkNickname", "Tester"])
    }

    private func open(_ code: String, streams: Int) {
        openLink(UIEnv.link("gawk://room/\(code)"), in: app)
        let count = app.descendants(matching: .any)["room.streaming"]
        expectation(for: NSPredicate(format: "label == %@", "\(streams) streaming"), evaluatedWith: count)
        waitForExpectations(timeout: UIWait.media)
    }

    /// The first element whose label starts with `prefix` (rows combine
    /// their texts: "P1, Streaming").
    private func labelled(_ prefix: String) -> XCUIElement {
        app.descendants(matching: .any).matching(NSPredicate(format: "label BEGINSWITH %@", prefix)).firstMatch
    }

    /// Your own row, under `name` or the relay's numbered variant of it.
    private func yours(_ name: String) -> XCUIElement {
        app.descendants(matching: .any).matching(
            NSPredicate(format: "label BEGINSWITH %@ AND label CONTAINS '(you)'", name)).firstMatch
    }

    private var tiles: XCUIElementQuery {
        app.descendants(matching: .any).matching(NSPredicate(format: "identifier BEGINSWITH 'room.tile.'"))
    }

    private func playing() -> [String] {
        tiles.allElementsBoundByIndex.filter { $0.label.hasSuffix(", playing") }.map(\.identifier)
    }

    /// D19: five streams in two columns, four playing; a tap on the paused
    /// one plays it and pauses the one that played longest.
    func testGridPlaysFourOfFive() throws {
        open(try UIEnv.require("GAWK_UI_ROOM_CODE")[0], streams: 5)
        XCTAssertEqual(tiles.count, 5)
        XCTAssertEqual(Set(tiles.allElementsBoundByIndex.map { $0.frame.minX }).count, 2, "two columns")
        let before = playing()
        XCTAssertEqual(before.count, 4, "four play at once")
        XCTAssertTrue(app.staticTexts["room.hint"].exists || app.descendants(matching: .any)["room.hint"].exists)
        shot("room-grid", app)

        let paused = try XCTUnwrap(tiles.allElementsBoundByIndex.first { $0.label.hasSuffix("Tap to play") })
        let pausedId = paused.identifier
        paused.tap()
        let after = playing()
        XCTAssertTrue(after.contains(pausedId), "the tapped one plays")
        XCTAssertFalse(after.contains(before[0]), "the oldest was paused")
        XCTAssertEqual(after.count, 4)
    }

    /// A tap on the video brings the controls back, as a tap on the black
    /// around it does (the owner, review of #475).
    func testATapOnTheVideoShowsTheControls() throws {
        app.terminate()
        app = launchApp(extra: ["-gawkControlIdle", "4", "-gawkNickname", "Tester"])
        open(try UIEnv.require("GAWK_UI_ROOM_CODE")[0], streams: 5)
        let close = app.buttons["room.close"]
        XCTAssertTrue(wait(for: close, hittable: false, timeout: UIWait.step), "hidden after the idle time")
        let playing = try XCTUnwrap(tiles.allElementsBoundByIndex.first { $0.label.hasSuffix(", playing") })
        playing.tap()
        XCTAssertTrue(wait(for: close, timeout: UIWait.step), "a tap on the video shows them")
        shot("room-tap-video", app)
    }

    /// D19: Focus shows the strip under the focused stream.
    func testFocusShowsTheStrip() throws {
        open(try UIEnv.require("GAWK_UI_ROOM_CODE")[0], streams: 5)
        app.buttons["room.layout.focus"].tap()
        XCTAssertTrue(app.descendants(matching: .any)["room.strip"].waitForExistence(timeout: UIWait.step))
        XCTAssertEqual(playing().count, 1, "only the focused stream plays")
        shot("room-focus", app)
    }

    /// D20: People lists the room; Edit renames you in the room (the roster
    /// says so) and in Settings.
    func testPeopleAndANewNickname() throws {
        open(try UIEnv.require("GAWK_UI_ROOM_CODE")[0], streams: 5)
        app.buttons["room.people"].tap()
        // All the way out: the list is lazy, and rows below the fold
        // don't exist yet.
        XCTAssertTrue(labelled("P1").waitForExistence(timeout: UIWait.step))
        labelled("P1").swipeUp()
        // The relay keeps names unique, and an earlier test's "Tester" may
        // still be leaving: yours can come back as "Tester 2".
        XCTAssertTrue(yours("Tester").waitForExistence(timeout: UIWait.step), "your row")
        for n in ["P1", "P2", "P3", "P4", "P5"] {
            XCTAssertTrue(labelled(n).exists, n)
        }
        shot("room-people", app)
        app.buttons["people.edit"].tap()
        // An alert's field keeps no SwiftUI identifier.
        let field = app.alerts.textFields.firstMatch
        XCTAssertTrue(field.waitForExistence(timeout: UIWait.step), "the nickname alert")
        field.clearAndType("Kuusi")
        app.alerts.buttons["Save"].tap()
        XCTAssertTrue(yours("Kuusi").waitForExistence(timeout: UIWait.step), "the roster renamed you")
        app.buttons["Close"].firstMatch.tap()
        app.buttons["room.close"].tap()
        app.tabBars.buttons["Settings"].tap()
        XCTAssertEqual(app.textFields["settings.nickname"].value as? String, "Kuusi")
    }

    /// D19: an away stream shows its card.
    func testAnAwayStreamShowsItsCard() throws {
        let away = try UIEnv.require("GAWK_UI_AWAY_ROOM")[0]
        openLink(UIEnv.link("gawk://room/\(away)"), in: app)
        XCTAssertTrue(
            app.descendants(matching: .any).matching(NSPredicate(format: "label CONTAINS 'Mika, away'")).firstMatch
                .waitForExistence(timeout: UIWait.media))
        shot("room-away", app)
    }
}

extension XCUIElement {
    /// Replaces a field's text.
    @MainActor
    func clearAndType(_ text: String) {
        focus()
        let current = (value as? String) ?? ""
        typeText(String(repeating: XCUIKeyboardKey.delete.rawValue, count: current.count) + text)
    }
}
