import XCTest

/// Types into every text field the way a person does, one key at a time
/// with corrections, under the Main Thread Checker that Xcode's Run adds
/// (crashing on a report), and asserts the app survives each step. A crash
/// the owner hit typing a relay URL into the Watch screen found no test
/// that typed key by key; these do. No relay needed.
@MainActor
final class TypingUITests: XCTestCase {
    private var app: XCUIApplication!

    override func setUp() async throws {
        continueAfterFailure = false
        app = XCUIApplication()
        let checker = "/Applications/Xcode.app/Contents/Developer/usr/lib/libMainThreadChecker.dylib"
        if FileManager.default.fileExists(atPath: checker) {
            app.launchEnvironment["DYLD_INSERT_LIBRARIES"] = checker
            app.launchEnvironment["MTC_CRASH_ON_REPORT"] = "1"
        }
        app.launch()
    }

    /// One key at a time, with a pause, checking the app after each.
    private func typeSlowly(_ text: String, into field: XCUIElement, file: StaticString = #filePath, line: UInt = #line) {
        for ch in text {
            field.typeText(String(ch))
            Thread.sleep(forTimeInterval: 0.05)
            XCTAssertEqual(app.state, .runningForeground, "the app died typing \(ch)", file: file, line: line)
        }
    }

    private func backspace(_ n: Int, in field: XCUIElement) {
        field.typeText(String(repeating: XCUIKeyboardKey.delete.rawValue, count: n))
        XCTAssertEqual(app.state, .runningForeground, "the app died deleting")
    }

    func testTypingARelayOverrideOnTheWatchScreen() {
        app.tabBars.buttons["Watch"].tap()
        let relay = app.textFields["watch.relay"]
        if !relay.waitForExistence(timeout: 5) {
            app.swipeUp()
        }
        XCTAssertTrue(relay.waitForExistence(timeout: 5), "the relay override field")
        relay.tap()
        typeSlowly("https://127.0.0.1:4498", into: relay)
        backspace(1, in: relay)
        typeSlowly("9", into: relay)
        XCTAssertEqual(relay.value as? String, "https://127.0.0.1:4499")
        app.switches["watch.insecure"].switches.firstMatch.tap()
        let code = app.textFields["watch.code"]
        code.tap()
        typeSlowly("ty94b", into: code)
        backspace(2, in: code)
        typeSlowly("4bp", into: code)
        XCTAssertEqual(app.state, .runningForeground)
    }

    func testTypingANewServerInSettings() {
        app.tabBars.buttons["Settings"].tap()
        let add = app.buttons["Add a server…"]
        XCTAssertTrue(add.waitForExistence(timeout: 5))
        add.tap()
        let name = app.textFields["Name"]
        XCTAssertTrue(name.waitForExistence(timeout: 5))
        name.tap()
        typeSlowly("local", into: name)
        let url = app.textFields.element(boundBy: 1)
        url.tap()
        typeSlowly("127.0.0.1:4499", into: url)
        let secret = app.textFields["Publish secret (optional)"]
        secret.tap()
        typeSlowly("smoke", into: secret)
        XCTAssertEqual(app.state, .runningForeground)
    }

    func testTypingARoomOnTheBroadcastScreen() {
        app.tabBars.buttons["Broadcast"].tap()
        let room = app.textFields["Room code (optional)"]
        XCTAssertTrue(room.waitForExistence(timeout: 5))
        room.tap()
        typeSlowly("lan-party", into: room)
        backspace(3, in: room)
        XCTAssertEqual(app.state, .runningForeground)
    }
}
