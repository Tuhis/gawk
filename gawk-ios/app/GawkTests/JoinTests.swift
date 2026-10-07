@testable import Gawk
import XCTest

/// Joining (docs/70 D4, K7, K8, K12, D21): the resolver's three outcomes,
/// room records and their migration, and what links open.
@MainActor
final class JoinTests: XCTestCase {
    /// A probe the test answers by hand.
    private final class FakeProbe: RoomProbe {
        var stopped = false
        var answer: (@MainActor (Bool) -> Void)?
        func stop() { stopped = true }
    }

    private func resolve(guardTime: Duration = .seconds(8), _ script: @escaping @MainActor (FakeProbe) -> Void) async -> (JoinTarget, FakeProbe) {
        let probe = FakeProbe()
        let target = await JoinResolver.resolve(code: "K7XQ2M", guardTime: guardTime) { _, answer in
            probe.answer = answer
            script(probe)
            return probe
        }
        return (target, probe)
    }

    func testARoomStateMeansARoom() async {
        let (target, probe) = await resolve { p in Task { p.answer?(true) } }
        XCTAssertEqual(target, .room)
        XCTAssertTrue(probe.stopped, "the probe is closed before the hop")
    }

    func testARefusalMeansThePlayer() async {
        let (target, probe) = await resolve { p in Task { p.answer?(false) } }
        XCTAssertEqual(target, .broadcast)
        XCTAssertTrue(probe.stopped)
    }

    /// An answer from inside `start` is honoured too.
    func testAnImmediateAnswerIsKept() async {
        let (target, probe) = await resolve { p in p.answer?(true) }
        XCTAssertEqual(target, .room)
        XCTAssertTrue(probe.stopped)
    }

    /// The guard: no answer in time opens the player, and a late answer
    /// changes nothing.
    func testSilenceOpensThePlayerAtTheGuard() async {
        XCTAssertEqual(JoinResolver.guardTime, .seconds(8), "the web's guard")
        let started = ContinuousClock.now
        let (target, probe) = await resolve(guardTime: .milliseconds(300)) { _ in }
        XCTAssertEqual(target, .broadcast)
        XCTAssertGreaterThanOrEqual(ContinuousClock.now - started, .milliseconds(300))
        XCTAssertTrue(probe.stopped)
        probe.answer?(true)
    }

    // MARK: Room records (K12)

    private func settings() -> AppSettings {
        AppSettings(defaults: UserDefaults(suiteName: "fi.ioio.gawk.tests.\(UUID().uuidString)")!)
    }

    func testAPreR68ListBecomesUnsavedRecentRooms() {
        let defaults = UserDefaults(suiteName: "fi.ioio.gawk.tests.\(UUID().uuidString)")!
        defaults.set(["lan-party", "K7XQ2M"], forKey: "recentRooms")
        let s = AppSettings(defaults: defaults)
        XCTAssertEqual(s.yourRooms.map(\.code), ["lan-party", "K7XQ2M"], "the old order")
        XCTAssertTrue(s.yourRooms.allSatisfy { !$0.saved })
        XCTAssertEqual(s.yourRooms.first?.server, coreInfo().defaultRelayUrl)
        XCTAssertNotNil(defaults.data(forKey: "recentRooms"), "rewritten as records at once")
        XCTAssertEqual(AppSettings(defaults: defaults).yourRooms, s.yourRooms, "and read back")
    }

    func testRoomsAreSavedFirstCappedAndKeepTheirStar() {
        let s = settings()
        let here = s.relayURL
        let t0 = Date(timeIntervalSince1970: 1_000_000)
        for i in 0..<10 {
            s.noteRoom("ROOM\(i)", server: here, at: t0.addingTimeInterval(Double(i)))
        }
        XCTAssertEqual(s.yourRooms.count, AppSettings.maxRooms)
        XCTAssertEqual(s.yourRooms.first?.code, "ROOM9", "most recent first")
        let oldest = s.yourRooms.last!
        s.setSaved(oldest, true)
        s.noteRoom("NEW", server: here, at: t0.addingTimeInterval(20))
        XCTAssertEqual(s.yourRooms.first?.code, oldest.code, "saved first")
        XCTAssertTrue(s.yourRooms.contains { $0.code == "NEW" })
        XCTAssertEqual(s.yourRooms.count, AppSettings.maxRooms, "an unsaved one made room")
        // Joining a saved room again keeps its star and learns its name.
        s.noteRoom(oldest.code, name: "LAN party", server: here, at: t0.addingTimeInterval(30))
        let again = s.yourRooms.first { $0.code == oldest.code }
        XCTAssertEqual(again?.saved, true)
        XCTAssertEqual(again?.title, "LAN party")
        s.removeRoom(again!)
        XCTAssertFalse(s.yourRooms.contains { $0.code == oldest.code })
    }

    /// A room exists on one server: Your rooms lists the selected one's.
    func testYourRoomsAreTheSelectedServers() {
        let s = settings()
        s.noteRoom("HOME", server: "https://relay.example:4433")
        s.noteRoom("FLEET", server: s.relayURL)
        XCTAssertEqual(s.yourRooms.map(\.code), ["FLEET"])
        s.addServer(name: "home", url: "https://relay.example:4433")
        s.selectedURL = "https://relay.example:4433"
        XCTAssertEqual(s.yourRooms.map(\.code), ["HOME"])
    }

    // MARK: Links (K8) and the live guard (D21)

    private let fleet = coreInfo().defaultRelayUrl

    func testLinksOpenTheRightScreen() {
        let router = AppRouter()
        router.open(url: URL(string: "gawk://watch/K7XQ2M")!)
        XCTAssertEqual(router.screen, .player(code: "K7XQ2M", relay: fleet))
        router.screen = nil
        router.open(url: URL(string: "gawk://room/lan-party?nick=Ann&relay=https%3A%2F%2Frelay.example%3A4433")!)
        XCTAssertEqual(router.screen, .room(code: "lan-party", relay: "https://relay.example:4433", nick: "Ann"))
        router.screen = nil
        router.open(url: URL(string: "gawk://broadcast?room=lan-party&nick=Sam")!)
        XCTAssertNil(router.screen, "a broadcast link opens no player")
        XCTAssertEqual(router.tab, .broadcast)
        XCTAssertEqual(router.broadcastPrefill, .init(room: "lan-party", nick: "Sam", relay: nil))
    }

    /// A link names its server completely: no `relay=` is the default
    /// fleet, never the server picked in Settings (docs/68 D1).
    func testALinkWithoutARelayIsTheDefaultFleet() {
        let router = AppRouter()
        let encoded = fleet.addingPercentEncoding(withAllowedCharacters: .alphanumerics)!
        router.open(url: URL(string: "gawk://watch/K7XQ2M?relay=\(encoded)")!)
        XCTAssertEqual(router.screen, .player(code: "K7XQ2M", relay: fleet))
        router.screen = nil
        router.open(url: URL(string: "gawk://room/lan-party")!)
        XCTAssertEqual(router.screen, .room(code: "lan-party", relay: fleet, nick: nil))
        XCTAssertNil(AppRouter.chip(for: fleet), "K11: no chip for the default fleet")
        XCTAssertEqual(AppRouter.chip(for: "https://relay.example:4433"), "https://relay.example:4433")
    }

    func testADroppedParameterIsNamedNotShown() {
        let router = AppRouter()
        router.open(url: URL(string: "gawk://broadcast?room=lan-party&secret=hunter2")!)
        let notice = router.linkNotice ?? ""
        XCTAssertTrue(notice.contains("secret"), notice)
        XCTAssertFalse(notice.contains("hunter2"), notice)
    }

    /// D21: while live, a screen waits for "Watch anyway".
    func testWhileLiveAPlayerAsksFirst() {
        let router = AppRouter()
        router.isBroadcasting = true
        router.open(url: URL(string: "gawk://watch/K7XQ2M")!)
        XCTAssertNil(router.screen)
        XCTAssertEqual(router.confirmWhileLive, .screen(.player(code: "K7XQ2M", relay: fleet)))
        router.confirm()
        XCTAssertEqual(router.screen, .player(code: "K7XQ2M", relay: fleet))

        router.screen = nil
        router.join(code: "K7XQ2M", relay: "https://127.0.0.1:9", insecure: true)
        XCTAssertEqual(router.confirmWhileLive, .join(code: "K7XQ2M", relay: "https://127.0.0.1:9", insecure: true))
        XCTAssertNil(router.resolving, "nothing is dialed before the answer")
        router.confirmWhileLive = nil
    }
}
