import UIKit
import XCTest

/// iPad (docs/70 IX9, D26): page content at most 640 pt wide and centred,
/// and the room grid using the width. Runs on an iPad Simulator only; the
/// room needs `GAWK_UI_RELAY_URL` and `GAWK_UI_ROOM_CODE`.
@MainActor
final class IPadUITests: XCTestCase {
    override func setUp() async throws {
        continueAfterFailure = false
        try XCTSkipUnless(UIDevice.current.userInterfaceIdiom == .pad, "an iPad Simulator")
        XCUIDevice.shared.orientation = .portrait
    }

    /// Every tab's content fits in 640 pt, centred in the window.
    func testContentIsAtMost640Wide() {
        let app = launchApp(relay: false)
        let window = app.windows.firstMatch.frame
        func check(_ element: XCUIElement, _ what: String) {
            XCTAssertTrue(element.waitForExistence(timeout: 10), what)
            let f = element.frame
            XCTAssertLessThanOrEqual(f.width, 640, "\(what) is \(f.width) wide")
            XCTAssertEqual(f.midX, window.midX, accuracy: 2, "\(what) is centred")
        }
        check(app.cells.firstMatch, "Watch's join card")
        shot("ipad-watch", app)
        app.buttons["Broadcast"].firstMatch.tap()
        check(app.buttons["broadcast.goLive"], "Go live")
        app.buttons["Settings"].firstMatch.tap()
        check(app.cells.containing(.button, identifier: "server.row.gawk").firstMatch, "the server list")
        shot("ipad-settings", app)
    }

    /// D19, D26: three columns above four streams in portrait.
    func testTheRoomGridUsesThreeColumnsInPortrait() throws {
        let code = try UIEnv.require("GAWK_UI_RELAY_URL", "GAWK_UI_ROOM_CODE")[1]
        let app = launchApp(extra: ["-gawkControlIdle", "60"])
        openLink(UIEnv.link("gawk://room/\(code)"), in: app)
        let tiles = app.descendants(matching: .any).matching(NSPredicate(format: "identifier BEGINSWITH 'room.tile.'"))
        let five = expectation(for: NSPredicate(format: "count == 5"), evaluatedWith: tiles)
        wait(for: [five], timeout: 20)
        XCTAssertEqual(Set(tiles.allElementsBoundByIndex.map { $0.frame.minX.rounded() }).count, 3, "three columns")
        shot("ipad-room", app)
    }
}
