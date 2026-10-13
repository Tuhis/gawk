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
    /// Whether this field holds the keyboard.
    @MainActor
    var hasKeyboardFocus: Bool {
        (value(forKey: "hasKeyboardFocus") as? Bool) ?? false
    }
}

extension XCTestCase {
    /// Whether `element` can take a tap now. Mid-animation (a menu opening,
    /// a sheet sliding up, controls fading in) `isHittable` can find no
    /// activation point, and XCTest then fails the test from inside the
    /// query instead of answering false (CI, 2026-10-06). A frame that isn't
    /// there yet reads as "not yet" before asking; a frame that is there
    /// can still have no hit point while the view fades in (opacity and
    /// `allowsHitTesting`, as the player's controls), so that one failure,
    /// and only it, is absorbed as an expected one and read as "not yet".
    /// `continueAfterFailure` is on for the probe: with it off, even an
    /// expected failure ends the test there, silently, as a pass.
    @MainActor
    func canTap(_ element: XCUIElement) -> Bool {
        guard element.exists else { return false }
        let f = element.frame
        guard !f.isNull, !f.isInfinite, f.minX.isFinite, f.minY.isFinite, f.width > 0, f.height > 0 else {
            return false
        }
        let keep = continueAfterFailure
        continueAfterFailure = true
        defer { continueAfterFailure = keep }
        let options = XCTExpectedFailure.Options()
        options.isStrict = false
        options.issueMatcher = { $0.compactDescription.contains("Failed to determine hittability") }
        var hittable = false
        XCTExpectFailure("an element mid-animation has no hit point yet", options: options) {
            hittable = element.isHittable
        }
        return hittable
    }

    /// Taps `field` until it holds the keyboard. A tap that lands while its
    /// sheet or alert is still animating in does nothing, and typing then
    /// fails with "Neither element nor any descendant has keyboard focus"
    /// (CI, 2026-10-06). A field that already has focus isn't tapped again,
    /// so the caret stays where typing left it.
    @MainActor
    func focus(_ field: XCUIElement, timeout: TimeInterval = UIWait.step, file: StaticString = #filePath, line: UInt = #line) {
        let deadline = Date().addingTimeInterval(timeout)
        repeat {
            if field.hasKeyboardFocus { return }
            if canTap(field) { field.tap() }
            RunLoop.current.run(until: Date().addingTimeInterval(0.5))
        } while Date() < deadline
        // A tap can itself take the whole budget on a loaded runner (15 s
        // waiting for the app to idle while the keyboard came up, CI
        // 2026-10-06), so its outcome is read once more before failing.
        if field.hasKeyboardFocus { return }
        XCTFail("\(field) never took the keyboard", file: file, line: line)
    }

    /// Types `text` into `field` once it holds the keyboard.
    @MainActor
    func enter(_ text: String, into field: XCUIElement, file: StaticString = #filePath, line: UInt = #line) {
        focus(field, file: file, line: line)
        field.typeText(text)
    }

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
        enter(code, into: field)
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

    /// Opens the menu `opener` shows until `item` is in it: a tap on the
    /// opener can be dropped on a slow runner like any other. An open that
    /// is only slow gets 5 s before the next tap, which would close it.
    @MainActor
    func openMenu(_ opener: XCUIElement, showing item: XCUIElement, file: StaticString = #filePath, line: UInt = #line) {
        let deadline = Date().addingTimeInterval(UIWait.step)
        repeat {
            if item.exists { return }
            if canTap(opener) {
                opener.tap()
                if item.waitForExistence(timeout: 5) { return }
            } else {
                RunLoop.current.run(until: Date().addingTimeInterval(0.25))
            }
        } while Date() < deadline
        if item.exists { return }
        XCTFail("\(opener) never showed \(item)", file: file, line: line)
    }

    /// Chooses the item labelled `label` in the menu `opener` opens, and
    /// returns once `chosen` holds: the item's effect, not the menu's state,
    /// is what says the choice took. On a slow runner a tap on a settled item
    /// can be dropped with the menu left open (CI's recording, 2026-10-06),
    /// and a tap that took can leave the item in the hierarchy, taking no
    /// taps, for 10 s and more while the menu closes (CI, 2026-10-13); a
    /// menu stalled open can look the same. So an item that can take a tap
    /// again is tapped again, a menu that closed without the effect is opened
    /// again, and anything else is waited out. While the menu is still
    /// appearing its items have no frame ({inf, inf}) and `isHittable` fails
    /// the test outright (CI, 2026-10-06), hence `canTap`.
    @MainActor
    func choose(
        _ label: String, from opener: XCUIElement, in app: XCUIApplication, until chosen: () -> Bool,
        file: StaticString = #filePath, line: UInt = #line
    ) {
        let items = app.buttons.matching(NSPredicate(format: "label == %@", label))
        let deadline = Date().addingTimeInterval(2 * UIWait.step)
        repeat {
            if chosen() { return }
            if let item = items.allElementsBoundByIndex.first(where: { canTap($0) }) {
                item.tap()
                let shown = Date().addingTimeInterval(5)
                while Date() < shown {
                    if chosen() { return }
                    RunLoop.current.run(until: Date().addingTimeInterval(0.25))
                }
            } else if !items.firstMatch.exists {
                openMenu(opener, showing: items.firstMatch, file: file, line: line)
            } else {
                RunLoop.current.run(until: Date().addingTimeInterval(0.25))
            }
        } while Date() < deadline
        if chosen() { return }
        XCTFail("choosing \"\(label)\" never took", file: file, line: line)
    }

    /// Shows a player's controls if they've hidden (docs/70 D5: they go
    /// 3 s after the last touch), with a tap on the video.
    @MainActor
    func revealControls(_ app: XCUIApplication, probe: String = "player.close") {
        let control = app.buttons[probe]
        // A tap that lands while they fade out can hide them again, so it
        // takes a second try at most.
        for _ in 0..<3 where control.exists && !canTap(control) {
            app.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.45)).tap()
            if wait(for: control, timeout: 1.5) { break }
        }
        XCTAssertTrue(wait(for: control, timeout: UIWait.step), "the controls show")
    }

    /// Waits for `element` to exist and take a tap, or not. Polled through
    /// `canTap`: a predicate on `hittable` fails the test while the
    /// element has no frame yet.
    @MainActor
    @discardableResult
    func wait(for element: XCUIElement, hittable: Bool = true, timeout: TimeInterval = UIWait.step) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        repeat {
            if canTap(element) == hittable { return true }
            RunLoop.current.run(until: Date().addingTimeInterval(0.2))
        } while Date() < deadline
        return false
    }
}
