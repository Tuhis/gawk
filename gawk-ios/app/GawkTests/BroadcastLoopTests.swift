@testable import Gawk
import XCTest

/// docs/67 IO2, phase S: D27's test source through the real broadcast
/// pipeline (the core's `Broadcaster`, VideoToolbox, the engine) to a local
/// relay, watched back by the core's own `Viewer`. Needs a relay: set
/// `GAWK_SMOKE_RELAY_URL` (and `GAWK_SMOKE_SECRET` if it requires one) via
/// `TEST_RUNNER_`-prefixed environment; skipped otherwise.
@MainActor
final class BroadcastLoopTests: XCTestCase {
    func testTheTestBroadcastPlaysBackAndRotates() async throws {
        let env = ProcessInfo.processInfo.environment
        guard let relay = env["GAWK_SMOKE_RELAY_URL"], !relay.isEmpty else {
            throw XCTSkip("GAWK_SMOKE_RELAY_URL not set")
        }
        initializeCore()
        let identity = IdentityStore(service: "fi.ioio.gawk.tests.\(UUID().uuidString)")
        // Live, with a code. On a fresh CI runner VM the relay has answered
        // the Simulator's first QUIC handshake 14-31 s late and then never
        // started the session, though the connection stayed up (2026-10-04,
        // in the recorded packets); a later dial in the same run took 0.1 s.
        // Once QUIC's keepalive holds a connection open, a dial has no bound
        // of its own, so each attempt gets a fresh session and 30 s, and one
        // that hasn't gone live by then is abandoned. A local run goes live
        // in well under a second.
        var session = BroadcastSession(identity: identity)
        var abandoned: [BroadcastSession] = []
        var code = ""
        var attempts: [String] = []
        attempt: for n in 0..<4 {
            if n > 0 {
                session.stop()
                abandoned.append(session)
                session = BroadcastSession(identity: identity)
            }
            session.start(
                relayURL: relay, secret: env["GAWK_SMOKE_SECRET"] ?? "", quality: .cellular,
                room: "", nickname: "", telemetry: false, insecure: true
            )
            for _ in 0..<600 {
                if case .live(let c, _) = session.phase { code = c; break attempt }
                if case .ended(let reason) = session.phase {
                    attempts.append(reason ?? "-")
                    continue attempt
                }
                try await Task.sleep(for: .milliseconds(50))
            }
            attempts.append("still \(session.phase) after 30 s")
        }
        withExtendedLifetime(abandoned) {}
        XCTAssertEqual(code.count, 6, "a code; attempts: \(attempts)")

        // The test source, turning a quarter every 2 s.
        let source = TestBroadcastSource(sink: session.media, rotateEvery: 2)
        source.start()
        defer { source.stop(); session.stop() }

        let seen = Seen()
        let viewer = Viewer.start(
            options: ViewerOptions(relayUrl: relay, broadcastId: code, preset: .lowestLatency, insecure: true),
            listener: seen
        )
        defer { viewer.stop() }
        // Long enough for two orientations and the encoder rebuild between.
        for _ in 0..<900 {
            if seen.snapshot().formats >= 2 && seen.snapshot().samples >= 10 { break }
            try await Task.sleep(for: .milliseconds(50))
        }
        let s = seen.snapshot()
        let counters = session.counters().map { "\($0)" } ?? "none"
        XCTAssertNil(session.failure, "the pipeline failed")
        // The Simulator encodes in software, slowly (docs/67 §12): the bar is
        // that the path works, not its rate, which D26 leaves to devices.
        XCTAssertGreaterThanOrEqual(s.samples, 10, "frames played back; pipeline: \(counters)")
        XCTAssertGreaterThanOrEqual(s.formats, 2, "the rotation reached the viewer as a new format")
        XCTAssertGreaterThan(s.audio, 0, "the tone played back")
        if case .live(let again, _) = session.phase {
            XCTAssertEqual(again, code, "a rotation kept the code")
        }
    }
}

/// Counts what the viewer hands over.
private final class Seen: ViewerListener, @unchecked Sendable {
    private let lock = NSLock()
    private var samples = 0
    private var formats = 0
    private var audio = 0

    func snapshot() -> (samples: Int, formats: Int, audio: Int) {
        lock.withLock { (samples, formats, audio) }
    }

    func onStatus(status: ViewerStatus) {}
    func onH264(sample: H264Sample) {
        lock.withLock {
            samples += 1
            if sample.format != nil { formats += 1 }
        }
    }
    func onNv12(frame: Nv12Frame) {}
    func onAudio(pcm: PcmBlock) { lock.withLock { audio += 1 } }
    func onFlush() {}
    func onUnsupportedCodec(codec: String) {}
    func onStats(stats: ViewerStats) {}
}
