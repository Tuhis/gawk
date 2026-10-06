import XCTest

/// What the UI tests are pointed at, passed by xcodebuild with the
/// `TEST_RUNNER_` prefix (docs/67 D26, docs/70 §8). A test that needs one
/// that isn't set skips.
///
/// - `GAWK_UI_RELAY_URL`, `GAWK_UI_SECRET`: a local dev relay (insecure),
///   selected in Settings and named by every link (`link(_:)`).
/// - `GAWK_UI_BROADCAST_ID`: a broadcast on it (`gawk-devpub`).
/// - `GAWK_UI_ROOM_CODE`: a room on it with `gawk-devpub` streams attached.
/// - `GAWK_UI_OUT`: a directory for screenshots (optional).
enum UIEnv {
    private static var env: [String: String] { ProcessInfo.processInfo.environment }

    static func value(_ name: String) -> String? {
        env[name].flatMap { $0.isEmpty ? nil : $0 }
    }

    static func require(_ names: String...) throws -> [String] {
        let values = names.compactMap(value)
        guard values.count == names.count else {
            throw XCTSkip("\(names.joined(separator: ", ")) not set")
        }
        return values
    }

    /// `link` on the local relay. A link with no `relay=` is the default
    /// fleet (docs/68 D1), so every watch and room link a test opens names
    /// its server.
    static func link(_ link: String) -> String {
        guard let relay = value("GAWK_UI_RELAY_URL"),
              let encoded = relay.addingPercentEncoding(withAllowedCharacters: .alphanumerics)
        else { return link }
        return link + (link.contains("?") ? "&" : "?") + "relay=\(encoded)"
    }
}

/// How long one UI step may take to show. CI's macOS runners are slow and
/// share their few cores with the relay, the fixture publishers and the
/// Simulator, so what takes half a second on a desk can take several seconds
/// there. A wait ends as soon as its condition holds, so a long bound costs a
/// passing run nothing; a short one only buys flakes.
enum UIWait {
    static let step: TimeInterval = 15
    /// A step through the relay: going live, joining, a stream starting to
    /// play. It dials, waits for a keyframe and decodes, so on a loaded
    /// runner it takes far longer than a UI step.
    static let media: TimeInterval = 60
}

extension XCUIElement {
    /// Whether this element can take a tap now. While it is still appearing
    /// (a menu opening, a sheet sliding up) it has no frame yet, and asking
    /// `isHittable` then fails the test from inside XCTest ("Activation point
    /// invalid") instead of answering false (CI, 2026-10-06). The frame is
    /// checked first, so that state reads as "not yet".
    @MainActor
    var canTakeTap: Bool {
        guard exists else { return false }
        let f = frame
        guard !f.isNull, !f.isInfinite, f.minX.isFinite, f.minY.isFinite, f.width > 0, f.height > 0 else {
            return false
        }
        return isHittable
    }

    /// Whether this field holds the keyboard.
    @MainActor
    var hasKeyboardFocus: Bool {
        (value(forKey: "hasKeyboardFocus") as? Bool) ?? false
    }

    /// Taps this field until it holds the keyboard. A tap that lands while
    /// its sheet or alert is still animating in does nothing, and typing
    /// then fails with "Neither element nor any descendant has keyboard
    /// focus" (CI, 2026-10-06). A field that already has focus isn't tapped
    /// again, so the caret stays where typing left it.
    @MainActor
    func focus(timeout: TimeInterval = UIWait.step, file: StaticString = #filePath, line: UInt = #line) {
        let deadline = Date().addingTimeInterval(timeout)
        repeat {
            if hasKeyboardFocus { return }
            if canTakeTap { tap() }
            RunLoop.current.run(until: Date().addingTimeInterval(0.5))
        } while Date() < deadline
        XCTFail("\(self) never took the keyboard", file: file, line: line)
    }

    /// Types `text` into this field once it holds the keyboard.
    @MainActor
    func enter(_ text: String, file: StaticString = #filePath, line: UInt = #line) {
        focus(file: file, line: line)
        typeText(text)
    }
}

extension XCTestCase {
    /// The app from a clean start (`-gawkReset`), on the local relay when
    /// one is set.
    @MainActor
    func launchApp(reset: Bool = true, relay: Bool = true, extra: [String] = []) -> XCUIApplication {
        let app = XCUIApplication()
        var args: [String] = reset ? ["-gawkReset"] : []
        if relay, let url = UIEnv.value("GAWK_UI_RELAY_URL") {
            args += ["-gawkRelay", url]
            if let secret = UIEnv.value("GAWK_UI_SECRET") { args += ["-gawkSecret", secret] }
        }
        app.launchArguments = args + extra
        app.launch()
        return app
    }

    /// A screenshot, attached to the result and written to `GAWK_UI_OUT`.
    @MainActor
    func shot(_ name: String, _ app: XCUIApplication? = nil) {
        let image = app?.screenshot() ?? XCUIScreen.main.screenshot()
        let attachment = XCTAttachment(screenshot: image)
        attachment.name = name
        attachment.lifetime = .keepAlways
        add(attachment)
        guard let dir = UIEnv.value("GAWK_UI_OUT").map({ URL(fileURLWithPath: $0) }) else { return }
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        try? image.pngRepresentation.write(to: dir.appendingPathComponent("\(name).png"))
    }

    /// Types `code` into the Watch box, a key at a time.
    @MainActor
    func typeCode(_ code: String, in app: XCUIApplication) {
        let field = app.textFields["watch.code"]
        XCTAssertTrue(field.waitForExistence(timeout: UIWait.step), "the code box")
        field.enter(code)
    }

    /// Opens a `gawk://` link the way another app would, accepting iOS's
    /// "Open in gawk?" if it asks. The system's opener hands it to the
    /// running app; `XCUIApplication.open` can launch a fresh one.
    @MainActor
    func openLink(_ link: String, in app: XCUIApplication) {
        XCUIDevice.shared.system.open(URL(string: link)!)
        let springboard = XCUIApplication(bundleIdentifier: "com.apple.springboard")
        let open = springboard.buttons.matching(NSPredicate(format: "label IN %@", ["Open", "Avaa"])).firstMatch
        if open.waitForExistence(timeout: 1) { open.tap() }
    }

    /// Taps an open menu's item once it can take the tap. While the menu is
    /// still appearing, its items have no frame yet ({inf, inf}); a tap then
    /// lands nowhere and the menu stays open, and `isHittable` fails the test
    /// outright (CI, 2026-10-06), hence `canTakeTap`. Even a menu that has
    /// settled can drop a tap on a slow runner: the item was hittable, the
    /// tap was sent, and the menu stayed open (CI's recording, 2026-10-06).
    /// A chosen item closes the menu, so the tap is repeated until it does.
    @MainActor
    func tapMenuItem(_ label: String, in app: XCUIApplication, file: StaticString = #filePath, line: UInt = #line) {
        let items = app.buttons.matching(NSPredicate(format: "label == %@", label))
        func open() -> XCUIElement? { items.allElementsBoundByIndex.first(where: \.canTakeTap) }
        let deadline = Date().addingTimeInterval(UIWait.step)
        repeat {
            if let item = open() {
                item.tap()
                let settle = Date().addingTimeInterval(3)
                while Date() < settle, open() != nil {
                    RunLoop.current.run(until: Date().addingTimeInterval(0.2))
                }
                if open() == nil { return }
            }
            RunLoop.current.run(until: Date().addingTimeInterval(0.2))
        } while Date() < deadline
        XCTFail("no \"\(label)\" menu item took a tap", file: file, line: line)
    }

    /// Shows a player's controls if they've hidden (docs/70 D5: they go
    /// 3 s after the last touch), with a tap on the video.
    @MainActor
    func revealControls(_ app: XCUIApplication, probe: String = "player.close") {
        let control = app.buttons[probe]
        // A tap that lands while they fade out can hide them again, so it
        // takes a second try at most.
        for _ in 0..<3 where control.exists && !control.canTakeTap {
            app.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.45)).tap()
            if wait(for: control, timeout: 1.5) { break }
        }
        XCTAssertTrue(wait(for: control, timeout: UIWait.step), "the controls show")
    }

    /// Waits for `element` to exist and take a tap, or not. Polled through
    /// `canTakeTap`: a predicate on `hittable` fails the test while the
    /// element has no frame yet.
    @MainActor
    @discardableResult
    func wait(for element: XCUIElement, hittable: Bool = true, timeout: TimeInterval = UIWait.step) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        repeat {
            if element.canTakeTap == hittable { return true }
            RunLoop.current.run(until: Date().addingTimeInterval(0.2))
        } while Date() < deadline
        return false
    }
}
