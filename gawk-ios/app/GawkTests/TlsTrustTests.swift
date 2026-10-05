import XCTest
@testable import Gawk

/// A relay with a publicly trusted certificate passes TLS on iOS. Before
/// R65's fix every certificate failed with `UnknownIssuer`:
/// rustls-native-certs has no iOS backend. Dials the compiled-in default
/// fleet, so it needs the internet and is opt-in: set
/// `TEST_RUNNER_GAWK_TLS_CHECK=1`. Any refusal after TLS (the fleet's
/// allowed origins answer 403 until they list `gawk://ios`) is a pass.
@MainActor
final class TlsTrustTests: XCTestCase {
    func testTheDefaultFleetsCertificateIsTrusted() async throws {
        guard ProcessInfo.processInfo.environment["GAWK_TLS_CHECK"] == "1" else {
            throw XCTSkip("set TEST_RUNNER_GAWK_TLS_CHECK=1 to dial the default fleet")
        }
        initializeCore()
        let identity = IdentityStore(service: "fi.ioio.gawk.tests.\(UUID().uuidString)")
        let session = BroadcastSession(identity: identity)
        session.start(
            relayURL: coreInfo().defaultRelayUrl, secret: "", quality: .cellular,
            room: "", nickname: "", telemetry: false, insecure: false
        )
        defer { session.stop() }
        for _ in 0..<300 {
            if case .live = session.phase { return }
            if case .ended(let reason) = session.phase {
                XCTAssertFalse(
                    (reason ?? "").localizedCaseInsensitiveContains("certificate"),
                    "TLS failed: \(reason ?? "-")"
                )
                return
            }
            try await Task.sleep(for: .milliseconds(50))
        }
        XCTFail("neither live nor ended in 15 s: \(session.phase)")
    }
}
