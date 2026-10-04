import XCTest

/// The Watch screen driven through its UI against a local relay (docs/67
/// D26, IO5's Simulator criteria): type the code, point at the dev relay,
/// watch, and record what the screen shows. Runs only when xcodebuild
/// passes the relay and the code (`TEST_RUNNER_` prefix), skipped otherwise:
///
/// - `GAWK_UI_RELAY_URL`, `GAWK_UI_BROADCAST_ID`: what to watch (insecure).
/// - `GAWK_UI_OUT`: a directory for the screenshots (optional).
/// - `GAWK_UI_SCENARIO`: `watch` (default: two screenshots a second apart,
///   then landscape for fullscreen), `pip` (then home: PiP over the home
///   screen) or `background` (then home with PiP off: audio only).
/// - `GAWK_UI_HOLD_S`: seconds to keep the app up at the end, for probes
///   from outside (a relay outage, `log stream`).
@MainActor
final class WatchUITests: XCTestCase {
    func testWatchingFromTheWatchScreen() throws {
        let env = ProcessInfo.processInfo.environment
        guard let relay = env["GAWK_UI_RELAY_URL"], !relay.isEmpty,
              let code = env["GAWK_UI_BROADCAST_ID"], !code.isEmpty else {
            throw XCTSkip("GAWK_UI_RELAY_URL and GAWK_UI_BROADCAST_ID are not set")
        }
        let scenario = env["GAWK_UI_SCENARIO"] ?? "watch"
        let out = env["GAWK_UI_OUT"].map { URL(fileURLWithPath: $0) }
        let hold = Double(env["GAWK_UI_HOLD_S"] ?? "") ?? 0
        continueAfterFailure = false
        XCUIDevice.shared.orientation = .portrait

        let app = XCUIApplication()
        app.launch()

        let relayField = app.textFields["watch.relay"]
        XCTAssertTrue(relayField.waitForExistence(timeout: 10))
        relayField.tap()
        relayField.typeText(relay + "\n")
        let insecure = app.switches["watch.insecure"]
        if insecure.value as? String != "1" {
            insecure.switches.firstMatch.tap()
        }
        let codeField = app.textFields["watch.code"]
        codeField.tap()
        // Lower case on purpose: the field normalizes as the SPA's does.
        codeField.typeText(code.lowercased())
        XCTAssertEqual(codeField.value as? String, code)
        app.buttons["watch.go"].tap()

        let status = app.staticTexts["watch.status"]
        let live = NSPredicate(format: "label == %@", "Live")
        expectation(for: live, evaluatedWith: status)
        waitForExpectations(timeout: 15)

        // Let the playout settle, then two frames a second apart.
        sleep(3)
        save(app, "portrait-1", to: out)
        sleep(1)
        save(app, "portrait-2", to: out)

        switch scenario {
        case "pip":
            XCUIDevice.shared.press(.home)
            sleep(3)
            save(nil, "pip-1", to: out)
            sleep(1)
            save(nil, "pip-2", to: out)
        case "background":
            XCUIDevice.shared.press(.home)
            sleep(3)
            save(nil, "background", to: out)
        default:
            XCUIDevice.shared.orientation = .landscapeLeft
            sleep(3)
            save(app, "landscape-1", to: out)
            sleep(1)
            save(app, "landscape-2", to: out)
        }
        if hold > 0 {
            Thread.sleep(forTimeInterval: hold)
            save(nil, "after-hold", to: out)
        }
        if scenario == "watch" {
            XCUIDevice.shared.orientation = .portrait
        }
    }

    private func save(_ app: XCUIApplication?, _ name: String, to dir: URL?) {
        let shot = app?.screenshot() ?? XCUIScreen.main.screenshot()
        let attachment = XCTAttachment(screenshot: shot)
        attachment.name = name
        attachment.lifetime = .keepAlways
        add(attachment)
        guard let dir else { return }
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        try? shot.pngRepresentation.write(to: dir.appendingPathComponent("\(name).png"))
    }
}
