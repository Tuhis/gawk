import XCTest

/// Rooms for the broadcaster (docs/70 IX5) against a local relay with
/// `-rooms`, `scripts/static-rooms.json` and `room-fixtures.sh`'s rooms:
/// create, join by code and by a link with `?rt=`, the gated room's key,
/// the creator's Remove and End, Leave, and D21's question while live.
@MainActor
final class BroadcastRoomUITests: XCTestCase {
    private var app: XCUIApplication!

    override func setUp() async throws {
        continueAfterFailure = false
        XCUIDevice.shared.orientation = .portrait
        _ = try UIEnv.require("GAWK_UI_RELAY_URL")
        app = launchApp()
        app.tabBars.buttons["Broadcast"].tap()
    }

    private func openRoomSheet() {
        let add = app.buttons["broadcast.room"]
        XCTAssertTrue(add.waitForExistence(timeout: UIWait.step))
        add.tap()
        XCTAssertTrue(app.textFields["room.input"].waitForExistence(timeout: UIWait.step))
    }

    private func joinRoom(_ input: String) {
        openRoomSheet()
        let field = app.textFields["room.input"]
        field.enter(input + "\n")
    }

    private func goLive() {
        let go = app.buttons["broadcast.goLive"]
        XCTAssertTrue(go.waitForExistence(timeout: UIWait.step))
        go.tap()
        XCTAssertTrue(app.buttons["broadcast.code"].waitForExistence(timeout: UIWait.media), "live")
    }

    private var roomCard: XCUIElement { app.buttons["broadcast.manageRoom"] }

    private func end() {
        app.buttons["broadcast.end"].tap()
        XCTAssertTrue(app.buttons["broadcast.goLive"].waitForExistence(timeout: 15))
    }

    /// C3 → C4: Create a new room, then Go live shows the room card.
    func testCreateANewRoom() {
        openRoomSheet()
        app.buttons["room.create"].tap()
        XCTAssertTrue(app.staticTexts["A new room"].waitForExistence(timeout: UIWait.step), "pending")
        XCTAssertTrue(app.staticTexts["Joins when you go live"].exists)
        goLive()
        XCTAssertTrue(roomCard.waitForExistence(timeout: UIWait.media), "the room card")
        XCTAssertTrue(roomCard.label.contains("1 streaming"), roomCard.label)
        shot("broadcast-room-card", app)
        end()
    }

    /// D16: a code joins on Go live.
    func testACodeJoinsOnGoLive() throws {
        let room = try UIEnv.require("GAWK_UI_AWAY_ROOM")[0]
        joinRoom(room)
        XCTAssertTrue(app.staticTexts[room].waitForExistence(timeout: UIWait.step), "pending")
        goLive()
        XCTAssertTrue(roomCard.waitForExistence(timeout: UIWait.media))
        end()
    }

    /// D16: a gated static room asks for its key; the key joins it.
    func testAGatedRoomAsksForItsKey() {
        joinRoom("lan-party")
        goLive()
        let alert = app.alerts["This room needs a key to add your stream"]
        XCTAssertTrue(alert.waitForExistence(timeout: UIWait.media))
        shot("broadcast-room-key", app)
        alert.textFields.firstMatch.enter("k3y")
        alert.buttons["Join"].tap()
        XCTAssertTrue(roomCard.waitForExistence(timeout: UIWait.media), "joined with the key")
        XCTAssertTrue(roomCard.label.contains("LAN party"), roomCard.label)
        end()
    }

    /// D16: a pasted link's `?rt=` grant is honoured: an attach key joins
    /// the gated room without asking.
    func testALinksGrantJoins() {
        joinRoom("https://gawk.ioio.fi/#/room/lan-party?rt=a%3Ak3y")
        goLive()
        XCTAssertTrue(roomCard.waitForExistence(timeout: UIWait.media))
        XCTAssertFalse(app.alerts["This room needs a key to add your stream"].exists)
        end()
    }

    /// D18: the creator (by a `?rt=c:` link) removes another stream; our
    /// broadcast stays up. The sheet has no room player.
    func testTheCreatorRemovesAStream() throws {
        let vars = try UIEnv.require("GAWK_UI_CREATOR_ROOM", "GAWK_UI_CREATOR_TOKEN")
        joinRoom("https://gawk.ioio.fi/#/room/\(vars[0])?rt=c%3A\(vars[1])")
        goLive()
        XCTAssertTrue(roomCard.waitForExistence(timeout: UIWait.media))
        roomCard.tap()
        XCTAssertTrue(app.staticTexts["Room \(vars[0]) · You made this room"].waitForExistence(timeout: UIWait.step))
        XCTAssertFalse(app.buttons["Watch the room"].exists, "no room player while live")
        shot("broadcast-room-sheet", app)
        let remove = app.buttons.matching(NSPredicate(format: "identifier BEGINSWITH 'room.remove.'")).firstMatch
        XCTAssertTrue(remove.waitForExistence(timeout: UIWait.step))
        let removed = remove.identifier
        remove.tap()
        let gone = expectation(for: NSPredicate(format: "exists == false"), evaluatedWith: app.buttons[removed])
        wait(for: [gone], timeout: 15)
        app.buttons["Close"].firstMatch.tap()
        XCTAssertTrue(app.descendants(matching: .any)["broadcast.live"].exists, "still live")
        end()
    }

    /// D18: End room for everyone; the broadcast carries on, in no room.
    func testEndRoomForEveryone() {
        openRoomSheet()
        app.buttons["room.create"].tap()
        goLive()
        XCTAssertTrue(roomCard.waitForExistence(timeout: UIWait.media))
        roomCard.tap()
        app.buttons["room.end"].tap()
        XCTAssertTrue(app.buttons["broadcast.room"].waitForExistence(timeout: 15), "not in a room")
        XCTAssertTrue(app.descendants(matching: .any)["broadcast.live"].exists, "still live")
        end()
    }

    /// D17: Leave returns to Not in a room.
    func testLeaveReturnsToNotInARoom() {
        openRoomSheet()
        app.buttons["room.create"].tap()
        goLive()
        XCTAssertTrue(roomCard.waitForExistence(timeout: UIWait.media))
        app.buttons["broadcast.leaveRoom"].tap()
        XCTAssertTrue(app.buttons["broadcast.room"].waitForExistence(timeout: UIWait.step))
        end()
    }

    /// D21: while live, opening a stream or a room asks first.
    func testWhileLiveWatchingAsksFirst() {
        goLive()
        app.tabBars.buttons["Watch"].tap()
        typeCode("K7XQ2M", in: app)
        app.buttons["watch.join"].tap()
        let ask = app.alerts["You're live"]
        XCTAssertTrue(ask.waitForExistence(timeout: UIWait.step))
        shot("broadcast-live-guard", app)
        ask.buttons["Cancel"].tap()
        openLink("gawk://room/lan-party", in: app)
        let asked = app.alerts["You're live"].waitForExistence(timeout: UIWait.step)
        shot("broadcast-live-guard-link", app)
        XCTAssertTrue(asked, "a link asks too")
        app.alerts["You're live"].buttons["Cancel"].tap()
        app.tabBars.buttons["Broadcast"].tap()
        shot("broadcast-live-guard-after", app)
        end()
    }
}
