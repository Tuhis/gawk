import XCTest

/// Settings and the edges (docs/70 IX7): selecting a server, Edit, the
/// locked built-in server, the strip, the unreachable banner, and a refused
/// start. The relay-backed ones need `GAWK_UI_RELAY_URL`.
@MainActor
final class SettingsUITests: XCTestCase {
    override func setUp() async throws {
        continueAfterFailure = false
        XCUIDevice.shared.orientation = .portrait
    }

    private func isSelected(_ row: XCUIElement) -> Bool {
        row.isSelected || (row.value as? String) == "Selected"
    }

    /// D22: a tap selects a server; the strip follows on Watch and
    /// Broadcast (D24).
    func testATapSelectsAServerAndTheStripFollows() throws {
        _ = try UIEnv.require("GAWK_UI_RELAY_URL")
        let app = launchApp()
        XCTAssertTrue(app.descendants(matching: .any)["server.strip"].waitForExistence(timeout: UIWait.step), "local: the strip on Watch")
        app.tabBars.buttons["Broadcast"].tap()
        XCTAssertTrue(app.descendants(matching: .any)["server.strip"].waitForExistence(timeout: UIWait.step), "and on Broadcast")
        shot("settings-strip-broadcast", app)

        app.tabBars.buttons["Settings"].tap()
        let fleet = app.buttons["server.row.gawk"]
        XCTAssertTrue(fleet.waitForExistence(timeout: UIWait.step))
        fleet.tap()
        XCTAssertTrue(isSelected(fleet))
        XCTAssertFalse(isSelected(app.buttons["server.row.local"]))
        shot("settings", app)
        app.tabBars.buttons["Watch"].tap()
        XCTAssertFalse(app.descendants(matching: .any)["server.strip"].waitForExistence(timeout: 2), "the default fleet: no strip")
    }

    /// D23: info opens Edit; the built-in server's Name and Relay are
    /// locked, and a saved one's aren't.
    func testInfoOpensEditAndTheBuiltInServerIsLocked() throws {
        _ = try UIEnv.require("GAWK_UI_RELAY_URL")
        let app = launchApp()
        app.tabBars.buttons["Settings"].tap()
        app.buttons["server.info.gawk"].tap()
        XCTAssertTrue(app.navigationBars["Edit server"].waitForExistence(timeout: UIWait.step))
        let name = app.descendants(matching: .any)["server.name"]
        XCTAssertTrue(name.label.contains("Locked"), name.label)
        XCTAssertFalse(app.textFields["Name"].exists, "no field to type a name into")
        XCTAssertTrue(app.textFields["server.secret"].exists, "the secret is editable")
        XCTAssertFalse(app.buttons["server.delete"].exists, "the fleet can't be deleted")
        shot("settings-edit-builtin", app)
        app.navigationBars.buttons.firstMatch.tap()

        app.buttons["server.info.local"].tap()
        XCTAssertTrue(app.navigationBars["Edit server"].waitForExistence(timeout: UIWait.step))
        XCTAssertTrue(app.textFields["Name"].exists, "a saved server's name is editable")
        XCTAssertTrue(app.buttons["server.delete"].exists)
        shot("settings-edit-saved", app)
    }

    /// R72 (docs/74 D3): Settings lists the open-source licences, and a
    /// licence text opens whole.
    func testSettingsShowsTheOpenSourceLicences() {
        let app = launchApp(relay: false)
        app.tabBars.buttons["Settings"].tap()
        let row = app.buttons["settings.licences"]
        for _ in 0..<4 where !row.isHittable { app.swipeUp() }
        row.tap()
        XCTAssertTrue(app.navigationBars["Open-source licences"].waitForExistence(timeout: UIWait.step))
        shot("settings-licences", app)
        let text = app.buttons["licences.text.0"]
        for _ in 0..<40 where !text.exists { app.swipeUp() }
        text.tap()
        XCTAssertTrue(app.staticTexts.containing(NSPredicate(format: "label BEGINSWITH 'Applies to: '")).firstMatch.waitForExistence(timeout: UIWait.step))
        shot("settings-licence-text", app)
    }

    /// D24: an unreachable server shows the banner at the top, and Go live
    /// stays enabled.
    func testAnUnreachableServerShowsTheBannerAndGoLiveStaysEnabled() {
        let app = launchApp(relay: false, extra: ["-gawkRelay", "https://127.0.0.1:1"])
        XCTAssertTrue(app.descendants(matching: .any)["server.unreachable"].waitForExistence(timeout: UIWait.media), "on Watch")
        app.tabBars.buttons["Broadcast"].tap()
        XCTAssertTrue(app.descendants(matching: .any)["server.unreachable"].waitForExistence(timeout: UIWait.media), "on Broadcast")
        XCTAssertTrue(app.buttons["broadcast.goLive"].isEnabled, "the probe can be wrong: Go live stays")
        shot("settings-unreachable", app)
    }

    /// D25: a refused secret is "Couldn't go live" with Edit secret, which
    /// opens the server's Edit page.
    func testAWrongSecretIsCouldntGoLiveWithEditSecret() throws {
        let relay = try UIEnv.require("GAWK_UI_RELAY_URL")[0]
        let app = launchApp(relay: false, extra: ["-gawkRelay", relay, "-gawkSecret", "wrong"])
        app.tabBars.buttons["Broadcast"].tap()
        app.buttons["broadcast.goLive"].tap()
        let alert = app.alerts["Couldn't go live"]
        XCTAssertTrue(alert.waitForExistence(timeout: UIWait.media))
        XCTAssertTrue(alert.staticTexts["The server refused the publish secret."].exists)
        shot("settings-refused", app)
        alert.buttons["Edit secret"].tap()
        XCTAssertTrue(app.navigationBars["Edit server"].waitForExistence(timeout: UIWait.step))
        XCTAssertEqual(app.textFields["server.secret"].value as? String, "wrong")
    }
}
