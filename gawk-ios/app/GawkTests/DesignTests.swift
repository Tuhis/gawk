@testable import Gawk
import UIKit
import XCTest

/// The design foundation's pure pieces (docs/70 IX1).
final class DesignTests: XCTestCase {
    func testTokenLiteralsParseAsTheirSourcesWriteThem() {
        XCTAssertEqual(CSSColor("#0a0b0d"), CSSColor(red: 10 / 255, green: 11 / 255, blue: 13 / 255, alpha: 1))
        XCTAssertEqual(CSSColor("#6b8afe59")?.alpha, 0x59 / 255)
        XCTAssertEqual(
            CSSColor("rgba(107, 138, 254, 0.14)"),
            CSSColor(red: 107 / 255, green: 138 / 255, blue: 254 / 255, alpha: 0.14))
        XCTAssertNil(CSSColor("#0a0b0"))
        XCTAssertNil(CSSColor("rgba(300, 0, 0, 1)"))
        XCTAssertNil(CSSColor("purple"))
    }

    /// §3.4: the check shows at once and the copy icon is back after 1.5 s.
    func testTheCopiedStateLastsOneAndAHalfSeconds() {
        let t0 = ContinuousClock.now
        var feedback = CopyFeedback()
        XCTAssertFalse(feedback.isCopied(at: t0))
        feedback.note(at: t0)
        XCTAssertTrue(feedback.isCopied(at: t0))
        XCTAssertTrue(feedback.isCopied(at: t0 + .milliseconds(1499)))
        XCTAssertFalse(feedback.isCopied(at: t0 + .milliseconds(1500)))
        XCTAssertEqual(CopyFeedback.symbol(copied: true), "checkmark")
        XCTAssertEqual(CopyFeedback.symbol(copied: false), "doc.on.doc")
    }

    /// The copier copies, shows the check at once and lets it go after the
    /// duration. That the pill's code stays in place while copied (OD4) is
    /// `PlayerUITests`' to show, on the pill itself.
    @MainActor
    func testTheCopierCopiesAndLapses() async throws {
        let copier = Copier()
        copier.copy("https://gawk.ioio.fi/#/view/K7XQ2M", announcement: "Link copied")
        XCTAssertTrue(copier.isCopied)
        XCTAssertEqual(UIPasteboard.general.string, "https://gawk.ioio.fi/#/view/K7XQ2M")
        try await Task.sleep(for: CopyFeedback.duration + .milliseconds(300))
        XCTAssertFalse(copier.isCopied)
    }
}
