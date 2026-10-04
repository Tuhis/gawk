import XCTest
@testable import Gawk

/// The Add sheet is also how a server is renamed (the URL is the key), and
/// a rename with the secret left blank must keep the saved secret.
@MainActor
final class SettingsTests: XCTestCase {
    func testRenamingAServerKeepsItsSecret() {
        let identity = IdentityStore(service: "fi.ioio.gawk.tests.\(UUID().uuidString)")
        let settings = AppSettings(defaults: UserDefaults(suiteName: "fi.ioio.gawk.tests.\(UUID().uuidString)")!)
        let url = "https://relay.example:4433"
        saveServer(settings: settings, identity: identity, name: "home", url: url, secret: "s3cret")
        XCTAssertEqual(identity.secret(relay: url), "s3cret", "the Keychain works: sign the test host")
        saveServer(settings: settings, identity: identity, name: "lan", url: url, secret: "")
        XCTAssertEqual(settings.servers.map(\.name), ["lan"])
        XCTAssertEqual(identity.secret(relay: url), "s3cret")
        identity.setSecret("", relay: url)
    }
}
