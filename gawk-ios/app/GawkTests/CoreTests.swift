@testable import Gawk
import XCTest

/// The Swift side of the UniFFI boundary (docs/67 D3): the core links, loads
/// and answers in the Simulator with the iOS identity injected (D5).
final class CoreTests: XCTestCase {
    func testTheCoreSpeaksAsGawkIos() {
        initializeCore()
        let info = coreInfo()
        XCTAssertEqual(info.distribution, "gawk-ios")
        XCTAssertEqual(info.origin, "gawk://ios")
        XCTAssertEqual(info.defaultRelayUrl, "https://api.gawk.ioio.fi:4433")
        XCTAssertEqual(info.defaultAppUrl, "https://gawk.ioio.fi")
    }

    /// R72 (docs/74 D2): the app carries its own notices across the
    /// boundary, the C libraries and the MPL sources included.
    func testTheCoreCarriesTheAppsNotices() {
        let notices = thirdPartyNotices()
        XCTAssertGreaterThan(notices.packages.count, 100)
        for native in ["libopus", "libvpx"] {
            XCTAssertTrue(notices.packages.contains { $0.name == native }, native)
        }
        XCTAssertTrue(notices.sources.contains { $0.package.hasPrefix("uniffi ") })
        XCTAssertFalse(notices.texts.isEmpty)
    }

    func testTheBundleIdMatchesDocs67D28() {
        XCTAssertEqual(Bundle.main.bundleIdentifier, "fi.ioio.gawk")
    }
}
