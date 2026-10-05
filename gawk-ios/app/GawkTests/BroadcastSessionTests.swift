import XCTest
@testable import Gawk

/// A refused reclaim must not leave the dead identity behind: every later
/// Start would send it again and be refused again (R17).
@MainActor
final class BroadcastSessionTests: XCTestCase {
    private let relay = "https://127.0.0.1:9"

    private func endedWith(_ status: UInt16) -> IdentityStore.Identity? {
        let identity = IdentityStore(service: "fi.ioio.gawk.tests.\(UUID().uuidString)")
        identity.save(relay: relay, code: "AB2CD3", token: "aa")
        // An unsigned test host has no Keychain, and every load is nil.
        XCTAssertNotNil(identity.load(relay: relay), "the Keychain works: sign the test host")
        let session = BroadcastSession(identity: identity)
        session.start(
            relayURL: relay, secret: "", quality: .standard, room: "",
            nickname: "", telemetry: false, insecure: true
        )
        session.apply(.ended(error: nil, reclaimStatus: status))
        session.stop()
        defer { identity.forgetIdentity(relay: relay) }
        return identity.load(relay: relay)
    }

    func testARefusedReclaimForgetsTheIdentity() {
        for status: UInt16 in [403, 404, 409, 451] {
            XCTAssertNil(endedWith(status), "reclaim refused with \(status)")
        }
    }

    func testAWrongSecretOrAFullServerKeepsTheIdentity() {
        for status: UInt16 in [401, 429] {
            XCTAssertEqual(endedWith(status)?.code, "AB2CD3", "ended with \(status)")
        }
    }

    // MARK: Stop

    private func session() -> BroadcastSession {
        BroadcastSession(identity: IdentityStore(service: "fi.ioio.gawk.tests.\(UUID().uuidString)"))
    }

    func testStopAnswersAtOnceAndALateLiveDoesNotUndoIt() {
        let s = session()
        s.apply(.live(code: "AB2CD3", joinLink: "https://gawk.ioio.fi/#/AB2CD3"))
        s.stop()
        XCTAssertEqual(s.phase, .stopping, "Stop left the live screen up")
        XCTAssertTrue(s.isActive, "a new Start must wait for this one to end")
        s.apply(.live(code: "AB2CD3", joinLink: "https://gawk.ioio.fi/#/AB2CD3"))
        s.apply(.resuming(attempt: 1))
        XCTAssertEqual(s.phase, .stopping)
        s.apply(.ended(error: nil, reclaimStatus: nil))
        XCTAssertEqual(s.phase, .ended(reason: nil))
    }

    /// The core winds down on its own thread; one that never reports back
    /// must not leave the screen on "Stopping…" for good, and says so.
    func testAStopTheCoreNeverConfirmsEndsWithAReason() async throws {
        let s = session()
        s.stopGrace = .milliseconds(200)
        var ended = false
        s.onEnded = { ended = true }
        s.apply(.live(code: "AB2CD3", joinLink: "https://gawk.ioio.fi/#/AB2CD3"))
        s.stop()
        try await Task.sleep(for: .milliseconds(600))
        guard case .ended(let reason?) = s.phase else {
            return XCTFail("still \(s.phase)")
        }
        XCTAssertFalse(reason.isEmpty)
        XCTAssertTrue(ended, "capture outlived the stop")
    }

    /// Stop while the dial is still out: no relay answers at port 9.
    func testStopWhileConnectingEnds() async throws {
        initializeCore()
        let s = session()
        s.start(
            relayURL: relay, secret: "", quality: .standard, room: "",
            nickname: "", telemetry: false, insecure: true
        )
        XCTAssertEqual(s.phase, .connecting)
        s.stop()
        for _ in 0..<40 {
            if case .ended = s.phase { break }
            try await Task.sleep(for: .milliseconds(50))
        }
        XCTAssertEqual(s.phase, .ended(reason: nil), "Stop while connecting")
    }
}
