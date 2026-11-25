import XCTest
@testable import Gawk

/// How many times a scripted probe ran.
@MainActor
final class Calls {
    private(set) var n = 0
    func bump() { n += 1 }
}

/// R74 PP14 (docs/76 §6.12): the switch to a party's local relay, with the
/// probe scripted. The TN3179 retry itself is the Rust core's
/// (`LocalRelayPrompt`, tested there); these pin what the app does with it.
@MainActor
final class LocalRelayTests: XCTestCase {
    private let home = "https://api.gawk.ioio.fi:4433"

    private func make() -> LocalRelaySwitcher {
        LocalRelaySwitcher(defaults: UserDefaults(suiteName: "fi.ioio.gawk.tests.\(UUID().uuidString)")!)
    }

    private let offer = LocalRelayOffer(
        handle: "6f1c2d3e-4a5b-4c6d-8e7f-0123456789ab", name: "Assembly 2026", validitySecs: 3600,
        candidates: [LocalRelayCandidate(url: "https://192.168.1.20:4433", certHashes: [String(repeating: "07", count: 32)])])

    private var found: LocalRelaySwitch {
        LocalRelaySwitch(
            handle: offer.handle, name: offer.name, url: "https://192.168.1.20:4433",
            certHashes: [String(repeating: "07", count: 32)], rttMs: 2)
    }

    /// Long enough for the core's 3 s retry delay.
    private func settle(_ done: () -> Bool) async throws {
        for _ in 0..<600 where !done() {
            try await Task.sleep(for: .milliseconds(10))
        }
    }

    /// item 43.3: a candidate that answers takes the next broadcast,
    /// pinned and without lr=1; home is dialed with lr=1 otherwise.
    func testAnAnsweringCandidateTakesTheNextBroadcast() async throws {
        let s = make()
        XCTAssertTrue(s.target(home: home, homeCertHashes: []).followOffers)
        let answer = found
        s.probe = { _ in answer }
        s.consider(offer, live: false, home: home)
        try await settle { s.chip(home: home) != nil }
        XCTAssertEqual(s.chip(home: home), "Assembly 2026 · local relay")
        let t = s.target(home: home, homeCertHashes: [])
        XCTAssertEqual(t.url, "https://192.168.1.20:4433")
        XCTAssertEqual(t.certHashes, answer.certHashes)
        XCTAssertFalse(t.followOffers)
        XCTAssertFalse(s.nextBroadcast)
        // Another server picked in Settings is not this PoP's home.
        let other = s.target(home: "https://relay.example.net:4433", homeCertHashes: [])
        XCTAssertNil(other.pop)
        XCTAssertEqual(other.url, "https://relay.example.net:4433")
    }

    /// item 43.3: an offer while live is for the next broadcast.
    func testAnOfferWhileLiveIsForTheNextBroadcast() async throws {
        let s = make()
        let answer = found
        s.probe = { _ in answer }
        s.consider(offer, live: true, home: home)
        try await settle { s.nextBroadcast }
        XCTAssertTrue(s.nextBroadcast)
        XCTAssertEqual(
            LocalRelayCopy.nextBroadcast("Assembly 2026"),
            "Assembly 2026 has a local relay. Your next broadcast will use it.")
        s.broadcastStarted()
        XCTAssertFalse(s.nextBroadcast)
    }

    /// items 43.5, 43.6: failing twice (the prompt's one retry) leaves the
    /// PoP for the app session; an opt-out is remembered by handle.
    func testFailureAndOptOutStayHome() async throws {
        let s = make()
        let calls = Calls()
        s.probe = { _ in
            await calls.bump()
            return nil
        }
        s.consider(offer, live: false, home: home)
        try await settle { calls.n >= 2 }
        try await Task.sleep(for: .milliseconds(50))
        XCTAssertEqual(calls.n, 2, "the first dial and one retry, never a loop")
        s.consider(offer, live: false, home: home)
        try await Task.sleep(for: .milliseconds(50))
        XCTAssertEqual(calls.n, 2, "unreachable for this app session")
        // item 43.7: home holds an lr=1 session's demand for an offer, so
        // an app that gave up on the PoP it was offered stops saying lr=1.
        XCTAssertFalse(s.target(home: home, homeCertHashes: []).followOffers)
        XCTAssertTrue(s.target(home: "https://relay.example.net:4433", homeCertHashes: []).followOffers,
                      "another home offered nothing it gave up on")

        let o = make()
        let answer = found
        o.probe = { _ in answer }
        o.consider(offer, live: false, home: home)
        try await settle { o.chip(home: home) != nil }
        o.optOut(stream: LocalRelaySwitcher.broadcastStream)
        XCTAssertNil(o.chip(home: home))
        XCTAssertEqual(o.optedOutHandle, offer.handle)
        XCTAssertFalse(o.target(home: home, homeCertHashes: []).followOffers, "opted out: no lr=1")
        o.consider(offer, live: false, home: home)
        try await Task.sleep(for: .milliseconds(50))
        XCTAssertNil(o.chip(home: home), "opted out")
    }

    /// OD18 (item 43.6): "Use … instead" holds for the stream it was picked
    /// on, never longer, and is never stored. The next stream, or the next
    /// broadcast, follows the PoP's offer again; a build that stored it
    /// per handle left a key that is cleared and never read.
    func testAnOptOutLastsTheStreamItWasPickedOnAndIsNeverStored() async throws {
        let suite = "fi.ioio.gawk.tests.\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suite)!
        defaults.set([offer.handle], forKey: "localRelayOptOuts")
        let s = LocalRelaySwitcher(defaults: defaults)
        XCTAssertNil(defaults.object(forKey: "localRelayOptOuts"), "the stored key is cleared")
        XCTAssertNil(s.optedOutHandle, "and never read")

        let calls = Calls()
        let answer = found
        s.probe = { _ in
            await calls.bump()
            return answer
        }
        let room = LocalRelaySwitcher.roomStream("lan-party")
        s.enterStream(room)
        s.consider(offer, live: false, home: home)
        try await settle { s.chip(home: home) != nil }
        s.optOut(stream: room)
        XCTAssertNil(defaults.object(forKey: "localRelayOptOuts"), "nothing is stored")
        // The same stream's redials stay on home, without lr=1.
        s.enterStream(room)
        XCTAssertFalse(s.followsOffers(home: home))
        s.consider(offer, live: false, home: home)
        try await Task.sleep(for: .milliseconds(50))
        XCTAssertEqual(calls.n, 1)
        // Another stream follows the offer again.
        s.enterStream(LocalRelaySwitcher.viewerStream("K7XQ2M"))
        XCTAssertNil(s.optedOutHandle)
        XCTAssertTrue(s.followsOffers(home: home))
        s.consider(offer, live: false, home: home)
        try await settle { s.chip(home: home) != nil }
        XCTAssertEqual(calls.n, 2)

        // The broadcast's opt-out ends with the broadcast.
        s.optOut(stream: LocalRelaySwitcher.broadcastStream)
        s.broadcastStarted()
        XCTAssertEqual(s.optedOutHandle, offer.handle, "the broadcast it was picked for")
        s.broadcastEnded()
        XCTAssertNil(s.optedOutHandle, "the next broadcast follows the offer")
        XCTAssertNil(LocalRelaySwitcher(defaults: defaults).optedOutHandle, "a new app session starts with none")
        UserDefaults.standard.removePersistentDomain(forName: suite)
    }

    /// TN3179 (item 44): while the app is not active nothing dials; coming
    /// back does.
    func testABackgroundedAppNeverDials() async throws {
        let s = make()
        let calls = Calls()
        let answer = found
        s.probe = { _ in
            await calls.bump()
            return answer
        }
        s.setActive(false)
        s.consider(offer, live: false, home: home)
        try await Task.sleep(for: .milliseconds(50))
        XCTAssertEqual(calls.n, 0)
        s.setActive(true)
        try await settle { s.chip(home: home) != nil }
        XCTAssertEqual(calls.n, 1)
    }

    /// item 43.5: a PoP that ends the session before it ever played sends
    /// the player home once; an end after it played is the stream's own.
    func testAPlayerFallsBackHomeOnlyWhenThePoPNeverPlayed() {
        XCTAssertTrue(WatchModel.fallsBackHome(.ended(reason: .notFound), wasLive: false))
        XCTAssertTrue(WatchModel.fallsBackHome(.ended(reason: .gaveUp), wasLive: false))
        XCTAssertFalse(WatchModel.fallsBackHome(.ended(reason: .notFound), wasLive: true))
        XCTAssertFalse(WatchModel.fallsBackHome(.ended(reason: .broadcastEnded), wasLive: false))
        XCTAssertFalse(WatchModel.fallsBackHome(.ended(reason: .stopped), wasLive: false))
        XCTAssertFalse(WatchModel.fallsBackHome(.live, wasLive: false))
    }

    /// item 43.5: a broadcast the PoP ends before it goes live leaves that
    /// PoP for the app session; the next start is home's, with lr=1.
    func testABroadcastThePoPRefusesLeavesItForTheAppSession() async throws {
        initializeCore()
        let s = make()
        let answer = found
        s.probe = { _ in answer }
        s.consider(offer, live: false, home: home)
        try await settle { s.chip(home: home) != nil }
        let session = BroadcastSession(
            identity: IdentityStore(service: "fi.ioio.gawk.tests.\(UUID().uuidString)"))
        let t = s.target(home: home, homeCertHashes: [])
        XCTAssertNotNil(t.pop)
        // A closed local port stands in for the PoP: no LAN dial from a test.
        session.start(
            relayURL: "https://127.0.0.1:9", secret: "", quality: .standard, nickname: "",
            telemetry: false, insecure: true)
        session.localRelay = t.pop
        session.onLocalRelayFailed = { [s] in s.refused() }
        session.apply(.ended(error: "403", reclaimStatus: 403))
        session.stop()
        XCTAssertNil(s.chip(home: home))
        let next = s.target(home: home, homeCertHashes: [])
        XCTAssertNil(next.pop)
        XCTAssertFalse(next.followOffers, "item 43.7: no lr=1 for a PoP this app gave up on")
        s.consider(offer, live: false, home: home)
        try await Task.sleep(for: .milliseconds(50))
        XCTAssertNil(s.chip(home: home), "unreachable for this app session")
    }

    /// The 4012 copy is the shared engine's (docs/76 item 28).
    func testThePartyFullCopy() {
        XCTAssertEqual(
            WatchStatusText.reconnecting(draining: false, partyFull: true),
            "This stream is at a LAN party with limited internet. Outside viewing is full right now; we'll keep trying.")
        XCTAssertEqual(WatchStatusText.reconnecting(draining: false), "RECONNECTING")
    }

    /// A gawk://server link's confirm names the host, not only the label.
    func testTheServerOfferConfirmNamesTheHost() {
        let offer = AppRouter.ServerOffer(
            name: "Assembly 2026", url: "https://192.168.1.20:4433", certHashes: ["aa"])
        let text = SettingsScreen.serverOfferMessage(offer)
        XCTAssertTrue(text.contains("“Assembly 2026” at 192.168.1.20"), text)
        XCTAssertTrue(text.contains("trust only the certificate"), text)
    }
}
