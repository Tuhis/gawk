import XCTest

/// The session the owner walks through, in order, against a local relay,
/// on docs/70's screens: add the relay in Settings, go live on D27's test
/// source, then watch that broadcast from the Watch tab by its code.
/// Needs `GAWK_UI_RELAY_URL` (and `GAWK_UI_SECRET`); skipped otherwise.
@MainActor
final class OwnerFlowUITests: XCTestCase {
    func testSettingsThenGoLiveThenWatch() throws {
        let relay = try UIEnv.require("GAWK_UI_RELAY_URL")[0]
        continueAfterFailure = false
        XCUIDevice.shared.orientation = .portrait
        let app = launchApp(relay: false)
        func alive(_ what: String) {
            XCTAssertEqual(app.state, .runningForeground, "the app died: \(what)")
        }

        // Settings: the dev certificate on, then add the local relay.
        app.tabBars.buttons["Settings"].tap()
        let dev = app.switches["Accept a local relay's dev certificate"]
        for _ in 0..<3 where !dev.isHittable { app.swipeUp() }
        if dev.value as? String != "1" { dev.switches.firstMatch.tap() }
        alive("dev certificate toggle")
        for _ in 0..<3 where !app.buttons["settings.addServer"].isHittable { app.swipeDown() }
        app.buttons["settings.addServer"].tap()
        let name = app.textFields["server.name"]
        XCTAssertTrue(name.waitForExistence(timeout: 5))
        name.tap()
        name.typeText("local")
        let url = app.textFields["server.url"]
        url.tap()
        url.typeText(String(relay.dropFirst("https://".count)))
        let secret = app.textFields["server.secret"]
        secret.tap()
        secret.typeText(UIEnv.value("GAWK_UI_SECRET") ?? "")
        app.buttons["server.save"].tap()
        alive("adding the server")
        let local = app.buttons["server.row.local"]
        XCTAssertTrue(local.waitForExistence(timeout: 5))
        local.tap()
        alive("selecting the server")
        shot("owner-1-settings", app)

        // Broadcast: Go live, until it has a code, then a while.
        app.tabBars.buttons["Broadcast"].tap()
        app.buttons["broadcast.goLive"].tap()
        let code = app.buttons["broadcast.code"]
        XCTAssertTrue(code.waitForExistence(timeout: 40), "the broadcast went live")
        let id = code.label.replacingOccurrences(of: "Code ", with: "").replacingOccurrences(of: ", copy", with: "")
        shot("owner-2-live", app)
        for i in 0..<10 {
            sleep(1)
            alive("broadcasting, \(i + 1) s")
        }

        // Watch it: D21 asks first, because the phone is live.
        app.tabBars.buttons["Watch"].tap()
        typeCode(id, in: app)
        app.buttons["watch.join"].tap()
        let ask = app.alerts["You're live"]
        XCTAssertTrue(ask.waitForExistence(timeout: 5))
        ask.buttons["Watch anyway"].tap()
        XCTAssertTrue(app.descendants(matching: .any)["player.live"].waitForExistence(timeout: 30), "watching")
        shot("owner-3-watching", app)
        alive("watching")
    }
}
