import XCTest

/// Broadcast (docs/70 IX4) on docs/67 D27's test source and a local relay:
/// Go live, the code, Quality while live, the Upload row, the summary
/// after End, and the code kept or renewed. Needs `GAWK_UI_RELAY_URL`.
@MainActor
final class BroadcastUITests: XCTestCase {
    private var app: XCUIApplication!

    override func setUp() async throws {
        continueAfterFailure = false
        XCUIDevice.shared.orientation = .portrait
        _ = try UIEnv.require("GAWK_UI_RELAY_URL")
        app = launchApp()
        app.tabBars.buttons["Broadcast"].tap()
    }

    /// Goes live and returns the code.
    private func goLive() -> String {
        let go = app.buttons["broadcast.goLive"]
        XCTAssertTrue(go.waitForExistence(timeout: 10))
        go.tap()
        let codeBoxes = app.buttons["broadcast.code"]
        XCTAssertTrue(codeBoxes.waitForExistence(timeout: 40), "live, with a code")
        let label = codeBoxes.label
        let code = label.replacingOccurrences(of: "Code ", with: "").replacingOccurrences(of: ", copy", with: "")
        XCTAssertEqual(code.count, 6, label)
        return code
    }

    private func end() {
        app.buttons["broadcast.end"].tap()
        XCTAssertTrue(app.descendants(matching: .any)["broadcast.summary"].waitForExistence(timeout: 15), "the summary")
    }

    /// B1 → B2: the first-run notice, Go live, the code; a tap on the boxes
    /// copies the code (the copied state shows).
    func testGoLiveShowsTheCodeAndATapCopiesIt() {
        XCTAssertTrue(app.buttons["broadcast.gotIt"].waitForExistence(timeout: 10), "first run: the notice")
        shot("broadcast-ready", app)
        _ = goLive()
        XCTAssertTrue(app.descendants(matching: .any)["broadcast.live"].exists)
        let boxes = app.buttons["broadcast.code"]
        boxes.tap()
        XCTAssertEqual(boxes.value as? String, "Copied")
        XCTAssertTrue(app.buttons["broadcast.copyLink"].exists)
        shot("broadcast-live", app)
        end()
    }

    /// D11: the Upload row shows a rate once a second's been measured.
    func testTheUploadRowShowsARate() {
        _ = goLive()
        let upload = app.descendants(matching: .any)["broadcast.upload"]
        let rate = NSPredicate(format: "label CONTAINS 'bps'")
        expectation(for: rate, evaluatedWith: upload)
        waitForExpectations(timeout: 20)
        end()
    }

    /// D12: Quality changed while live keeps the code.
    func testQualityChangesWhileLiveOnTheSameCode() {
        let code = goLive()
        app.buttons["broadcast.quality"].tap()
        let standard = app.buttons["Standard"]
        XCTAssertTrue(standard.waitForExistence(timeout: 5))
        standard.tap()
        sleep(4)
        XCTAssertEqual(app.buttons["broadcast.code"].label, "Code \(code), copy", "the same code")
        XCTAssertFalse(app.descendants(matching: .any)["broadcast.reconnecting"].exists, "nothing narrated")
        shot("broadcast-quality", app)
        end()
    }

    /// B4, D14, K6: End shows the summary's three numbers; Go live again
    /// gets the same code; after "Use a new code next time" a new one.
    func testEndThenAgainThenANewCode() {
        let first = goLive()
        sleep(3)
        end()
        let summary = app.descendants(matching: .any)["broadcast.summary"]
        for label in ["time live", "most watching", "average upload"] {
            XCTAssertTrue(summary.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", label)).firstMatch.exists, label)
        }
        XCTAssertTrue(app.buttons["Go live again"].exists || app.staticTexts["Go live again"].exists)
        XCTAssertTrue(app.staticTexts["Viewers keep code \(first) for a few minutes."].exists)
        shot("broadcast-ended", app)

        XCTAssertEqual(goLive(), first, "Go live again reclaims the code")
        end()

        app.buttons["broadcast.newCode"].tap()
        XCTAssertNotEqual(goLive(), first, "a new code after Use a new code next time")
        end()
    }
}
