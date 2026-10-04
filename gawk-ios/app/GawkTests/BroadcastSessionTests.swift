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
}
