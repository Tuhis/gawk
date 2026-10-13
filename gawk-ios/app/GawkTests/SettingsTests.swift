import XCTest
@testable import Gawk

/// The server picker's rules (docs/70 D22, D23; docs/40).
@MainActor
final class SettingsTests: XCTestCase {
    private func make() -> (AppSettings, IdentityStore) {
        let identity = IdentityStore(service: "fi.ioio.gawk.tests.\(UUID().uuidString)")
        let settings = AppSettings(defaults: UserDefaults(suiteName: "fi.ioio.gawk.tests.\(UUID().uuidString)")!)
        return (settings, identity)
    }

    /// Add is also how a server is renamed (the URL is the key), and a
    /// rename with the secret left blank must keep the saved secret.
    func testRenamingAServerKeepsItsSecret() {
        let (settings, identity) = make()
        let url = "https://relay.example:4433"
        saveServer(settings: settings, identity: identity, name: "home", url: url, secret: "s3cret")
        XCTAssertEqual(identity.secret(relay: url), "s3cret", "the Keychain works: sign the test host")
        saveServer(settings: settings, identity: identity, name: "lan", url: url, secret: "")
        XCTAssertEqual(settings.servers.map(\.name), ["lan"])
        XCTAssertEqual(identity.secret(relay: url), "s3cret")
        identity.setSecret("", relay: url)
    }

    /// A secret saved for one server is never sent to another: a start
    /// presents the selected server's own, and the default fleet's is its
    /// own too.
    func testASecretGoesOnlyToItsServer() {
        let (settings, identity) = make()
        let capture = Capture(identity: identity)
        let home = "https://home.example:4433"
        let lan = "https://lan.example:4433"
        saveServer(settings: settings, identity: identity, name: "home", url: home, secret: "home-secret")
        saveServer(settings: settings, identity: identity, name: "lan", url: lan, secret: "")
        settings.selectedURL = home
        XCTAssertEqual(capture.publishSecret(settings), "home-secret")
        settings.selectedURL = lan
        XCTAssertEqual(capture.publishSecret(settings), "", "lan has none, and gets none of home's")
        settings.selectedURL = ""
        XCTAssertEqual(capture.publishSecret(settings), "", "nor does the default fleet")
        identity.setSecret("", relay: home)
    }

    /// Edit changing a saved server's relay moves the server, keeps its
    /// selection and its rooms.
    func testMovingAServer() {
        let (settings, _) = make()
        let old = "https://old.example:4433"
        let new = "https://new.example:4433"
        settings.addServer(name: "home", url: old)
        settings.selectedURL = old
        settings.noteRoom("LANPTY", server: old)
        settings.moveServer(from: old, to: new, name: "home")
        XCTAssertEqual(settings.servers, [Server(name: "home", url: new)])
        XCTAssertEqual(settings.selectedURL, new)
        XCTAssertEqual(settings.yourRooms.map(\.code), ["LANPTY"])
    }

    /// D22: the row's probe words.
    func testProbeWords() {
        XCTAssertEqual(ServerProbes.describe(nil), "Checking…")
        XCTAssertEqual(ServerProbes.describe(.reachable(rttMs: 24, operatorName: nil)), "Connected · 24 ms")
        XCTAssertEqual(ServerProbes.describe(.reachable(rttMs: 24, operatorName: "Home")), "Connected · 24 ms · Home")
        XCTAssertEqual(ServerProbes.describe(.unreachable), "Can't reach server")
        XCTAssertEqual(ServerNotices.port("https://relay.example.net:4433"), 4433)
        XCTAssertEqual(ServerNotices.port("https://relay.example.net"), 443)
    }

    /// R40 (docs/71 D13): the probe's screening notice is kept per server,
    /// replaced by the next answer, and dropped when the server can't be
    /// reached.
    func testTheProbeKeepsTheScreeningNotice() async throws {
        let probes = ServerProbes()
        let url = "https://relay.example.net:4433"
        var answer: ServerProbe = .reachable(rttMs: 5, name: nil, screens: true)
        probes.probe = { [answer] _, _ in answer }
        probes.refresh(url, insecure: false)
        try await settle { probes.screens(url) }
        XCTAssertTrue(probes.screens(url))
        XCTAssertFalse(probes.screens("https://other.example:4433"))

        answer = .unreachable
        probes.probe = { [answer] _, _ in answer }
        probes.refresh(url, insecure: false)
        try await settle { !probes.screens(url) }
        XCTAssertFalse(probes.screens(url))
    }

    /// The mark shows when either the probe or the publish session says so,
    /// and not otherwise; it reads as the web and desktop apps' does.
    func testTheScreeningMarkFollowsEitherSource() {
        XCTAssertFalse(ScreeningMark.shown(probed: false, session: false))
        XCTAssertTrue(ScreeningMark.shown(probed: true, session: false))
        XCTAssertTrue(ScreeningMark.shown(probed: false, session: true))
        XCTAssertEqual(ScreeningMark.label, "Content screening")
        XCTAssertEqual(ScreeningMark.text, "This server automatically screens streams for explicit content.")
    }

    /// Waits (briefly) for a main-actor condition a detached probe settles.
    private func settle(_ done: () -> Bool) async throws {
        for _ in 0..<200 where !done() {
            try await Task.sleep(for: .milliseconds(10))
        }
    }
}
