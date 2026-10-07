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
        // The engine abandons a dial attempt still unanswered after 5 s and
        // starts over on a fresh endpoint, for 30 s in all, so one session
        // either goes live or ends within that. A local run goes live in
        // well under a second.
        let session = BroadcastSession(identity: identity)
        session.start(
            relayURL: relay, secret: env["GAWK_SMOKE_SECRET"] ?? "", quality: .cellular,
            nickname: "", telemetry: false, insecure: true
        )
        var code = ""
        for _ in 0..<700 {
            if case .live(let c, _) = session.phase { code = c; break }
            if case .ended = session.phase { break }
            try await Task.sleep(for: .milliseconds(50))
        }
        guard code.count == 6 else {
            session.stop()
            XCTFail("never went live: \(session.phase), \(session.failure ?? "no failure")")
            return
        }

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
        // Long enough for two orientations and the encoder rebuild between;
        // a passing CI run gets there in under 10 s.
        for _ in 0..<400 {
            if seen.snapshot().formats >= 2 && seen.snapshot().samples >= 10 { break }
            try await Task.sleep(for: .milliseconds(50))
        }
        let s = seen.snapshot()
        let counters = session.currentCounters().map { "\($0)" } ?? "none"
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

    /// docs/70 D12, IX4: Quality changed while live restarts the publish
    /// leg on the same code and token, says nothing about it, and a viewer
    /// picks up the new rung's stream.
    func testQualityChangesWhileLiveOnTheSameCode() async throws {
        let env = ProcessInfo.processInfo.environment
        guard let relay = env["GAWK_SMOKE_RELAY_URL"], !relay.isEmpty else {
            throw XCTSkip("GAWK_SMOKE_RELAY_URL not set")
        }
        initializeCore()
        let identity = IdentityStore(service: "fi.ioio.gawk.tests.\(UUID().uuidString)")
        let session = BroadcastSession(identity: identity)
        session.start(
            relayURL: relay, secret: env["GAWK_SMOKE_SECRET"] ?? "", quality: .cellular,
            nickname: "", telemetry: false, insecure: true
        )
        var code = ""
        for _ in 0..<700 {
            if case .live(let c, _) = session.phase { code = c; break }
            if case .ended = session.phase { break }
            try await Task.sleep(for: .milliseconds(50))
        }
        guard code.count == 6 else {
            session.stop()
            XCTFail("never went live: \(session.phase)")
            return
        }
        // Turning, as in the test above: the Simulator's software encoder
        // has stalled for good on its first session (2026-10-05), and a
        // turn builds a new one. Devices take the real encoder; the core's
        // `publish_relay` test runs this change on VideoToolbox.
        let source = TestBroadcastSource(sink: session.media, rotateEvery: 2)
        source.start()
        defer { source.stop(); session.stop() }
        // The token arrives beside the code; the restart waits for both.
        var token = ""
        for _ in 0..<100 {
            if let id = identity.load(relay: relay) { token = id.token; break }
            try await Task.sleep(for: .milliseconds(50))
        }
        XCTAssertFalse(token.isEmpty, "the identity was stored")

        let seen = Seen()
        let viewer = Viewer.start(
            options: ViewerOptions(relayUrl: relay, broadcastId: code, preset: .lowestLatency, insecure: true),
            listener: seen
        )
        defer { viewer.stop() }
        // The Simulator's software encoder can take a while to come up the
        // first time in a process.
        for _ in 0..<800 {
            if seen.snapshot().samples >= 10 { break }
            try await Task.sleep(for: .milliseconds(50))
        }
        let before = seen.snapshot()
        let counters = session.currentCounters().map { "\($0)" } ?? "none"
        XCTAssertGreaterThanOrEqual(before.samples, 10, "playing before the change; pipeline: \(counters)")

        var phases: [BroadcastPhase] = []
        session.setQuality(.standard)
        for _ in 0..<800 {
            phases.append(session.phase)
            let now = seen.snapshot()
            if now.formats > before.formats && now.samples >= before.samples + 10 { break }
            try await Task.sleep(for: .milliseconds(50))
        }
        let after = seen.snapshot()
        XCTAssertGreaterThan(after.formats, before.formats, "the viewer re-primed on the new config")
        XCTAssertGreaterThanOrEqual(after.samples, before.samples + 10, "and kept playing")
        XCTAssertFalse(phases.contains { $0.isResuming }, "no narration: \(phases)")
        XCTAssertEqual(session.phase, .live(code: code, joinLink: watchLink(broadcastId: code)), "the same code")
        XCTAssertEqual(identity.load(relay: relay)?.code, code)
        XCTAssertEqual(identity.load(relay: relay)?.token, token, "the same token")
        XCTAssertEqual(session.quality, .standard)
        // The new rung is what's encoded: Standard's frame rate and cap.
        var rung: (UInt32, UInt32) = (0, 0)
        for _ in 0..<200 {
            if let c = session.currentCounters(), c.fps == 60 { rung = (c.fps, c.peakBitrateBps); break }
            try await Task.sleep(for: .milliseconds(50))
        }
        XCTAssertEqual(rung.0, 60)
        XCTAssertEqual(rung.1, 8_000_000)
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
