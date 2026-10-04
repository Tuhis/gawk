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

    func testTheBundleIdMatchesDocs67D28() {
        XCTAssertEqual(Bundle.main.bundleIdentifier, "fi.ioio.gawk")
    }
}
