import XCTest

/// The session the owner walked through, in order, against a local relay:
/// add the relay in Settings, start the debug test broadcast, let it run,
/// then type a relay override on the Watch screen and watch the broadcast.
/// Needs `GAWK_UI_RELAY_URL` (and `GAWK_UI_SECRET`); skipped otherwise.
@MainActor
final class OwnerFlowUITests: XCTestCase {
    /// Watch dials the server picked in Settings: no URL typed on the Watch
    /// screen at all. Needs `GAWK_UI_BROADCAST_ID` too.
    func testWatchUsesTheServerPickedInSettings() throws {
        let env = ProcessInfo.processInfo.environment
        guard let relay = env["GAWK_UI_RELAY_URL"], !relay.isEmpty,
              let code = env["GAWK_UI_BROADCAST_ID"], !code.isEmpty else {
            throw XCTSkip("GAWK_UI_RELAY_URL and GAWK_UI_BROADCAST_ID are not set")
        }
        continueAfterFailure = false
        let app = XCUIApplication()
        app.launch()
        app.tabBars.buttons["Settings"].tap()
        let dev = app.switches["Accept a local relay's dev certificate"]
        if !dev.waitForExistence(timeout: 3) { app.swipeUp() }
        if dev.value as? String != "1" { dev.switches.firstMatch.tap() }
        app.swipeDown()
        app.buttons["Add a server…"].tap()
        app.textFields["Name"].tap()
        app.textFields["Name"].typeText("local")
        let url = app.textFields.element(boundBy: 1)
        url.tap()
        url.typeText(String(relay.dropFirst("https://".count)))
        app.buttons["Add"].tap()
        app.staticTexts["local"].firstMatch.tap()

        app.tabBars.buttons["Watch"].tap()
        let field = app.textFields["watch.code"]
        XCTAssertTrue(field.waitForExistence(timeout: 5))
        field.tap()
        field.typeText(code)
        app.buttons["watch.go"].tap()
        let status = app.staticTexts["watch.status"]
        expectation(for: NSPredicate(format: "label == %@", "Live"), evaluatedWith: status)
        waitForExpectations(timeout: 30)
    }

    func testSettingsThenTestBroadcastThenWatch() throws {
        let env = ProcessInfo.processInfo.environment
        guard let relay = env["GAWK_UI_RELAY_URL"], !relay.isEmpty else {
            throw XCTSkip("GAWK_UI_RELAY_URL is not set")
        }
        continueAfterFailure = false
        let app = XCUIApplication()
        app.launch()
        let out = env["GAWK_UI_OUT"].map { URL(fileURLWithPath: $0) }
        func shot(_ name: String) {
            guard let out else { return }
            try? XCUIScreen.main.screenshot().pngRepresentation
                .write(to: out.appendingPathComponent(name + ".png"))
        }
        func alive(_ what: String) {
            XCTAssertEqual(app.state, .runningForeground, "the app died: \(what)")
        }

        // Settings: dev certificate on, add the local relay, select it.
        app.tabBars.buttons["Settings"].tap()
        let dev = app.switches["Accept a local relay's dev certificate"]
        if !dev.waitForExistence(timeout: 3) { app.swipeUp() }
        if dev.value as? String != "1" { dev.switches.firstMatch.tap() }
        alive("dev certificate toggle")
        app.swipeDown()
        app.buttons["Add a server…"].tap()
        app.textFields["Name"].tap()
        app.textFields["Name"].typeText("local")
        let url = app.textFields.element(boundBy: 1)
        url.tap()
        url.typeText(String(relay.dropFirst("https://".count)))
        let secret = app.textFields["Publish secret (optional)"]
        secret.tap()
        secret.typeText(env["GAWK_UI_SECRET"] ?? "")
        app.buttons["Add"].tap()
        alive("adding the server")
        app.staticTexts["local"].firstMatch.tap()
        alive("selecting the server")

        // Broadcast: the test broadcast, until it has a code, then a while.
        app.tabBars.buttons["Broadcast"].tap()
        let test = app.buttons["Test broadcast"]
        if !test.waitForExistence(timeout: 3) { app.swipeUp() }
        shot("1-settings-done-broadcast-tab")
        test.tap()
        sleep(3)
        shot("2-after-test-broadcast")
        let copyLink = app.buttons["Copy link"]
        let live = copyLink.waitForExistence(timeout: 20)
        shot("3-broadcast")
        XCTAssertTrue(live, "the broadcast went live")
        for i in 0..<20 {
            sleep(1)
            alive("broadcasting, \(i + 1) s")
        }

        // Watch: type the relay override key by key, then watch.
        app.tabBars.buttons["Watch"].tap()
        let field = app.textFields["watch.relay"]
        if !field.waitForExistence(timeout: 3) { app.swipeUp() }
        field.tap()
        for ch in relay {
            field.typeText(String(ch))
            alive("typing \(ch) into the relay override")
        }
        for i in 0..<10 {
            sleep(1)
            alive("after typing, \(i + 1) s")
        }
    }
}
