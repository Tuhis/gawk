import XCTest

/// Watch and joining (docs/70 IX2): the code box, the resolver's three
/// outcomes, and `gawk://` links. The relay-backed tests need
/// `GAWK_UI_RELAY_URL` and a broadcast or room on it; see ``UIEnv``.
@MainActor
final class JoinUITests: XCTestCase {
    override func setUp() async throws {
        continueAfterFailure = false
        XCUIDevice.shared.orientation = .portrait
    }

    func testSixCharactersEnableJoinAndPasteFillsTheBoxes() {
        let app = launchApp(relay: false)
        let join = app.buttons["watch.join"]
        XCTAssertTrue(join.waitForExistence(timeout: 10))
        XCTAssertFalse(join.isEnabled)
        // Lower case and a forbidden letter on purpose: the box normalizes
        // as the SPA's does.
        typeCode("k7xo2", in: app)
        XCTAssertEqual(app.textFields["watch.code"].value as? String, "K7X2")
        XCTAssertFalse(join.isEnabled, "four characters")
        app.textFields["watch.code"].typeText("mq")
        XCTAssertEqual(app.textFields["watch.code"].value as? String, "K7X2MQ")
        XCTAssertTrue(join.isEnabled, "six characters")
        shot("join-six", app)

        app.textFields["watch.code"].typeText(String(repeating: XCUIKeyboardKey.delete.rawValue, count: 6))
        UIPasteboard.general.string = " ab-cd 23 "
        app.buttons["watch.paste"].tap()
        XCTAssertEqual(app.textFields["watch.code"].value as? String, "ABCD23")
        XCTAssertTrue(join.isEnabled)
    }

    func testABroadcastCodeOpensThePlayer() throws {
        let code = try UIEnv.require("GAWK_UI_RELAY_URL", "GAWK_UI_BROADCAST_ID")[1]
        let app = launchApp()
        typeCode(code, in: app)
        app.buttons["watch.join"].tap()
        XCTAssertTrue(app.descendants(matching: .any)["player.live"].waitForExistence(timeout: 20), "the player went live")
        shot("join-broadcast", app)
    }

    func testARoomCodeOpensTheRoomPlayer() throws {
        let code = try UIEnv.require("GAWK_UI_RELAY_URL", "GAWK_UI_ROOM_CODE")[1]
        let app = launchApp()
        typeCode(code, in: app)
        app.buttons["watch.join"].tap()
        XCTAssertTrue(app.descendants(matching: .any)["room.streaming"].waitForExistence(timeout: 20), "the room player")
        shot("join-room", app)
    }

    /// Neither a room nor a broadcast: the player says so (D4, D9).
    func testACodeThatIsNeitherOpensTheOfflineCard() throws {
        _ = try UIEnv.require("GAWK_UI_RELAY_URL")
        let app = launchApp()
        typeCode("ZZZZZ2", in: app)
        app.buttons["watch.join"].tap()
        let card = app.descendants(matching: .any)["player.card"]
        XCTAssertTrue(card.waitForExistence(timeout: 20))
        XCTAssertTrue(app.staticTexts["Streamer offline"].exists)
        XCTAssertTrue(app.staticTexts["No one is streaming at code ZZZZZ2 right now."].exists)
        shot("join-offline", app)
    }

    /// A server that never answers: the 8 s guard (or the dial's own
    /// failure, whichever is first) opens the player rather than spinning.
    func testAServerThatNeverAnswersStillOpensThePlayer() {
        let app = launchApp(relay: false, extra: ["-gawkRelay", "https://192.0.2.1:4433"])
        typeCode("K7XQ2M", in: app)
        app.buttons["watch.join"].tap()
        let opened = app.buttons["player.close"].waitForExistence(timeout: 20)
        shot("join-unreachable", app)
        XCTAssertTrue(opened, "the player opened within the guard")
    }

    func testAWatchLinkOpensThePlayer() throws {
        let code = try UIEnv.require("GAWK_UI_RELAY_URL", "GAWK_UI_BROADCAST_ID")[1]
        let app = launchApp()
        openLink(UIEnv.link("gawk://watch/\(code)"), in: app)
        XCTAssertTrue(app.descendants(matching: .any)["player.live"].waitForExistence(timeout: 20))
    }

    func testARoomLinkOpensTheRoomPlayer() throws {
        let code = try UIEnv.require("GAWK_UI_RELAY_URL", "GAWK_UI_ROOM_CODE")[1]
        let app = launchApp()
        openLink(UIEnv.link("gawk://room/\(code)"), in: app)
        XCTAssertTrue(app.descendants(matching: .any)["room.streaming"].waitForExistence(timeout: 20))
    }

    /// A broadcast link fills Broadcast in and never starts anything
    /// (docs/68 D4).
    func testABroadcastLinkPrefillsWithoutStarting() {
        let app = launchApp(relay: false)
        openLink("gawk://broadcast?room=lan-party", in: app)
        XCTAssertTrue(app.staticTexts["lan-party"].waitForExistence(timeout: 10), "the room is pending")
        XCTAssertTrue(app.buttons["broadcast.goLive"].exists)
        XCTAssertFalse(app.descendants(matching: .any)["broadcast.live"].exists, "nothing started")
        shot("link-broadcast", app)
    }

    /// A dropped parameter is named, never its value (docs/68 D1).
    func testADroppedParameterIsNamedWithoutItsValue() {
        let app = launchApp(relay: false)
        openLink("gawk://broadcast?room=lan-party&secret=hunter2", in: app)
        let notice = app.descendants(matching: .any)["link.notice"]
        XCTAssertTrue(notice.waitForExistence(timeout: 10))
        let text = app.staticTexts.matching(NSPredicate(format: "label CONTAINS 'secret'")).firstMatch
        XCTAssertTrue(text.exists, "the notice names the parameter")
        XCTAssertFalse(app.staticTexts.matching(NSPredicate(format: "label CONTAINS 'hunter2'")).firstMatch.exists)
    }
}
