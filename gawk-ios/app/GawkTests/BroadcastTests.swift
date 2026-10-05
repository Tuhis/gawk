@testable import Gawk
import XCTest

/// Broadcast's pure pieces and its room handling (docs/70 D10–D18, D25,
/// K2, K5, K6), without a relay.
@MainActor
final class BroadcastTests: XCTestCase {
    // MARK: The summary (K5)

    func testTheSummarysThreeNumbers() {
        var t = SummaryTracker()
        let t0 = Date(timeIntervalSince1970: 1_000)
        XCTAssertNil(t.summary(at: t0), "never live: no summary")
        t.live(at: t0)
        t.viewers(3)
        t.viewers(5)
        t.viewers(2)
        t.upload(bps: 4_000_000)
        t.upload(bps: 6_000_000)
        // A reconnect's Live doesn't restart the clock.
        t.live(at: t0.addingTimeInterval(300))
        let s = t.summary(at: t0.addingTimeInterval(754))
        XCTAssertEqual(s, BroadcastSummary(timeLive: 754, mostWatching: 5, averageUploadBps: 5_000_000))
        XCTAssertEqual(BroadcastText.duration(754), "12 minutes")

        var quiet = SummaryTracker()
        quiet.live(at: t0)
        XCTAssertNil(quiet.summary(at: t0.addingTimeInterval(1))?.averageUploadBps, "no sample: unknown, not zero")
    }

    func testTheBroadcastersWords() {
        XCTAssertEqual(BroadcastText.duration(1), "1 second")
        XCTAssertEqual(BroadcastText.duration(42), "42 seconds")
        XCTAssertEqual(BroadcastText.duration(60), "1 minute")
        XCTAssertEqual(BroadcastText.duration(4320), "1 h 12 min")
        XCTAssertEqual(BroadcastText.rate(6_240_000), "6.2 Mbps")
        XCTAssertEqual(BroadcastText.rate(850_000), "850 kbps")
        XCTAssertEqual(BroadcastText.mbps(8_000_000), "8")
        XCTAssertEqual(BroadcastText.mbps(2_500_000), "2.5")
        let c = BroadcastCounters(
            pushed: 1, admitted: 1, droppedNoContent: 0, droppedOverRate: 0, droppedBackpressure: 0,
            encoded: 1, width: 1920, height: 884, fps: 60, peakBitrateBps: 8_000_000,
            uploadAvailable: true, uploadBps: 6_000_000, uplinkWarning: false)
        XCTAssertEqual(BroadcastText.liveQuality(c)?.title, "1920×884 · 60 fps")
        XCTAssertEqual(BroadcastText.liveQuality(c)?.detail, "H.264 hardware · up to 8 Mbps")
    }

    /// D12: Auto is docs/67 D19's rule.
    func testQualityChoices() {
        XCTAssertEqual(QualityChoice.auto.resolve(expensivePath: true), .cellular)
        XCTAssertEqual(QualityChoice.auto.resolve(expensivePath: false), .standard)
        XCTAssertEqual(QualityChoice.standard.resolve(expensivePath: true), .standard)
        XCTAssertEqual(QualityChoice.cellular.resolve(expensivePath: false), .cellular)
        XCTAssertEqual(QualityChoice.auto.line, "1080p · 60 fps on Wi-Fi, 720p · 30 on cellular")
    }

    // MARK: The session

    private let relay = "https://127.0.0.1:9"

    private func session() -> (BroadcastSession, IdentityStore) {
        let identity = IdentityStore(service: "fi.ioio.gawk.tests.\(UUID().uuidString)")
        return (BroadcastSession(identity: identity), identity)
    }

    private func started() -> (BroadcastSession, IdentityStore) {
        initializeCore()
        let (s, identity) = session()
        s.start(relayURL: relay, secret: "", quality: .standard, nickname: "", telemetry: false, insecure: true)
        return (s, identity)
    }

    /// D25: a start refused before it went live is an alert with the reason,
    /// and a refused secret is the one that offers Edit secret.
    func testARefusedStartIsAnAlert() {
        let (s, _) = started()
        s.apply(.ended(error: "401", reclaimStatus: 401))
        XCTAssertEqual(s.refusal?.reason, "The server refused the publish secret.")
        XCTAssertEqual(s.refusal?.status, 401)
        s.clearRefusal()
        XCTAssertNil(s.refusal)
    }

    /// An end after going live is the summary, not a refusal.
    func testAnEndAfterLiveIsTheSummary() {
        let (s, _) = started()
        s.apply(.live(code: "AB2CD3", joinLink: "https://gawk.ioio.fi/#/view/AB2CD3"))
        s.setViewers(4)
        s.apply(.ended(error: nil, reclaimStatus: nil))
        XCTAssertNil(s.refusal)
        XCTAssertEqual(s.summary?.mostWatching, 4)
        XCTAssertEqual(s.lastCode, "AB2CD3", "Go live again names the code viewers keep")
    }

    /// K6: "Use a new code next time" forgets this server's identity.
    func testUseANewCodeNextTimeForgetsTheIdentity() {
        let (s, identity) = session()
        identity.save(relay: relay, code: "AB2CD3", token: "aa")
        XCTAssertNotNil(identity.load(relay: relay), "the Keychain works: sign the test host")
        s.useNewCodeNextTime(relay: relay)
        XCTAssertNil(identity.load(relay: relay))
        XCTAssertNil(s.lastCode)
    }

    /// K2: a minted room becomes this broadcast's room, its creator grant
    /// kept for this server only.
    func testAMintedRoomKeepsItsGrant() {
        let (s, identity) = started()
        s.chooseRoom(.create)
        s.applyRoom(.created(code: "K7XQ2M", creatorTokenHex: "ab"))
        XCTAssertEqual(s.pendingRoom, .join(code: "K7XQ2M", attachKey: "", creatorToken: "ab"))
        XCTAssertEqual(identity.roomCredential(.creatorToken, relay: relay, code: "K7XQ2M"), "ab")
        XCTAssertEqual(identity.roomCredential(.creatorToken, relay: "https://elsewhere:4433", code: "K7XQ2M"), "")
        s.stop()
    }

    /// docs/60 D10: removed by the creator is a card, and out of the room.
    func testRemovedByTheCreatorIsACard() {
        let (s, _) = started()
        s.chooseRoom(.join(code: "LANPTY", attachKey: "", creatorToken: ""))
        s.applyRoom(.detached(reason: "removed", byCreator: true))
        XCTAssertEqual(s.roomCard?.title, "Your stream was removed from LANPTY")
        XCTAssertNil(s.pendingRoom)
        XCTAssertNil(s.room)
        s.stop()
    }

    /// A room someone else ended is a card; one we left is quiet; one that
    /// never let us in is a status line.
    func testRoomEnds() {
        let (s, _) = started()
        s.chooseRoom(.join(code: "LANPTY", attachKey: "", creatorToken: ""))
        s.applyRoom(.ended(reason: "the room couldn't be found"))
        XCTAssertEqual(s.roomStatus, "Couldn't join the room: the room couldn't be found.")
        XCTAssertNil(s.roomCard)

        s.chooseRoom(.join(code: "LANPTY", attachKey: "", creatorToken: ""))
        s.leaveRoom()
        s.applyRoom(.detached(reason: "left", byCreator: false))
        s.applyRoom(.ended(reason: "left"))
        XCTAssertNil(s.roomCard)
        XCTAssertNil(s.pendingRoom, "Leave returns to Not in a room")
        s.stop()
    }

    func testTheEndedCardSaysTheStreamCarriesOn() {
        let card = BroadcastSession.endedCard(code: "LANPTY", reason: "the creator ended the room")
        XCTAssertEqual(card.title, "Room LANPTY is over")
        XCTAssertTrue(card.body.hasPrefix("The creator ended the room. Your stream is still live"))
    }
}
