@testable import Gawk
import XCTest

/// The Watch smoke test (docs/67 D24): the real player engine, through the
/// real core, against a local relay with a broadcast on it. It runs when
/// both variables are set, which xcodebuild passes with the `TEST_RUNNER_`
/// prefix:
///
///     TEST_RUNNER_GAWK_SMOKE_RELAY_URL=https://127.0.0.1:4433 \
///     TEST_RUNNER_GAWK_SMOKE_BROADCAST_ID=ABC234 xcodebuild … test
///
/// and is skipped otherwise. The relay is a dev relay, so it dials insecure.
@MainActor
final class WatchSmokeTests: XCTestCase {
    func testTheEngineGoesLiveAndEnqueuesVideo() throws {
        let env = ProcessInfo.processInfo.environment
        guard let relay = env["GAWK_SMOKE_RELAY_URL"], !relay.isEmpty,
              let id = env["GAWK_SMOKE_BROADCAST_ID"], !id.isEmpty else {
            throw XCTSkip("GAWK_SMOKE_RELAY_URL and GAWK_SMOKE_BROADCAST_ID are not set")
        }
        initializeCore()
        let live = expectation(description: "status reaches Live")
        live.assertForOverFulfill = false
        let statuses = StatusLog()
        let engine = PlayerEngine { event in
            if case .status(let s) = event {
                statuses.append(s)
                if s == .live { live.fulfill() }
            }
        }
        engine.start(ViewerOptions(relayUrl: relay, broadcastId: id, preset: .balanced, insecure: true))
        defer { engine.stop() }

        let deadline = Date().addingTimeInterval(15)
        wait(for: [live], timeout: 10)
        var counters = engine.snapshot()
        while counters.videoEnqueued < 30, Date() < deadline {
            RunLoop.current.run(until: Date().addingTimeInterval(0.25))
            counters = engine.snapshot()
        }
        XCTAssertGreaterThanOrEqual(
            counters.videoEnqueued, 30,
            "video samples enqueued within 20 s; statuses \(statuses.all), counters \(counters)")
        print("GAWK_SMOKE counters=\(counters) statuses=\(statuses.all)")
    }
}

/// Statuses, appended from the player's queue and read from the test's.
private final class StatusLog: @unchecked Sendable {
    private let lock = NSLock()
    private var items: [ViewerStatus] = []

    func append(_ s: ViewerStatus) { lock.withLock { items.append(s) } }
    var all: [ViewerStatus] { lock.withLock { items } }
}
