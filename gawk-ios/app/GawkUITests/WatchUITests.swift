import XCTest

/// The Watch tab driven through its UI against a local relay (docs/67 D26,
/// IO5's Simulator criteria, on docs/70's screens): type the code, join,
/// and record what the player shows. Needs `GAWK_UI_RELAY_URL` and
/// `GAWK_UI_BROADCAST_ID` (see ``UIEnv``); skipped otherwise.
///
/// - `GAWK_UI_SCENARIO`: `watch` (default: two screenshots a second apart,
///   then landscape), `pip` (then home: PiP over the home screen) or
///   `background` (then home with PiP off: audio only).
/// - `GAWK_UI_HOLD_S`: seconds to keep the app up at the end, for probes
///   from outside (a relay outage, `log stream`).
@MainActor
final class WatchUITests: XCTestCase {
    func testWatchingFromTheWatchTab() throws {
        let code = try UIEnv.require("GAWK_UI_RELAY_URL", "GAWK_UI_BROADCAST_ID")[1]
        let scenario = UIEnv.value("GAWK_UI_SCENARIO") ?? "watch"
        let hold = Double(UIEnv.value("GAWK_UI_HOLD_S") ?? "") ?? 0
        continueAfterFailure = false
        XCUIDevice.shared.orientation = .portrait

        let app = launchApp()
        // Lower case on purpose: the box normalizes as the SPA's does.
        typeCode(code.lowercased(), in: app)
        XCTAssertEqual(app.textFields["watch.code"].value as? String, code)
        app.buttons["watch.join"].tap()
        XCTAssertTrue(app.descendants(matching: .any)["player.live"].waitForExistence(timeout: 20), "live")

        // Let the playout settle, then two frames a second apart.
        sleep(3)
        shot("portrait-1", app)
        sleep(1)
        shot("portrait-2", app)

        switch scenario {
        case "pip":
            XCUIDevice.shared.press(.home)
            sleep(3)
            shot("pip-1")
            sleep(1)
            shot("pip-2")
        case "background":
            XCUIDevice.shared.press(.home)
            sleep(3)
            shot("background")
        default:
            XCUIDevice.shared.orientation = .landscapeLeft
            sleep(3)
            shot("landscape-1", app)
            sleep(1)
            shot("landscape-2", app)
        }
        if hold > 0 {
            Thread.sleep(forTimeInterval: hold)
            shot("after-hold")
        }
        if scenario == "watch" {
            XCUIDevice.shared.orientation = .portrait
        }
    }
}
