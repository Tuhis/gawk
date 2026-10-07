import XCTest

/// Types into every text field the way a person does, one key at a time
/// with corrections, under the Main Thread Checker that Xcode's Run adds
/// (crashing on a report), and asserts the app survives each step. A crash
/// the owner hit typing a relay URL found no test that typed key by key;
/// these do. No relay needed.
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
        app.launchArguments = ["-gawkReset"]
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

    /// docs/70 D3: the six boxes over the sanitizing field.
    func testTypingACodeIntoTheBoxes() {
        let code = app.textFields["watch.code"]
        XCTAssertTrue(code.waitForExistence(timeout: UIWait.step))
        focus(code)
        typeSlowly("ty94b", into: code)
        backspace(2, in: code)
        typeSlowly("4bp-x7", into: code)
        XCTAssertEqual(code.value as? String, "TY94BP")
        XCTAssertTrue(app.buttons["watch.join"].isEnabled)
        typeSlowly("z", into: code)
        XCTAssertEqual(code.value as? String, "TY94BP", "six at most")
        backspace(6, in: code)
        XCTAssertFalse(app.buttons["watch.join"].isEnabled)
    }

    func testTypingANewServerInSettings() {
        app.tabBars.buttons["Settings"].tap()
        let add = app.buttons["settings.addServer"]
        XCTAssertTrue(add.waitForExistence(timeout: UIWait.step))
        add.tap()
        let name = app.textFields["server.name"]
        XCTAssertTrue(name.waitForExistence(timeout: UIWait.step))
        focus(name)
        typeSlowly("local", into: name)
        let url = app.textFields["server.url"]
        focus(url)
        typeSlowly("127.0.0.1:4499", into: url)
        let secret = app.textFields["server.secret"]
        focus(secret)
        typeSlowly("smoke", into: secret)
        XCTAssertEqual(app.state, .runningForeground)
    }

    func testTypingARoomInTheRoomSheet() {
        app.tabBars.buttons["Broadcast"].tap()
        let add = app.buttons["broadcast.room"]
        XCTAssertTrue(add.waitForExistence(timeout: UIWait.step))
        add.tap()
        let room = app.textFields["room.input"]
        XCTAssertTrue(room.waitForExistence(timeout: UIWait.step))
        focus(room)
        typeSlowly("lan-party", into: room)
        backspace(3, in: room)
        XCTAssertEqual(app.state, .runningForeground)
    }
}
