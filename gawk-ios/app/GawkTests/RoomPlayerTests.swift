@testable import Gawk
import XCTest

/// The room player's decode budget and layout (docs/70 D19, D26; docs/67
/// D21). The layout rules need nothing; the playing rules run against a
/// local relay with `room-fixtures.sh`'s five-stream room
/// (`GAWK_SMOKE_RELAY_URL`, `GAWK_SMOKE_ROOM_CODE`), skipped otherwise.
@MainActor
final class RoomPlayerTests: XCTestCase {
    /// D19, D26: one column up to four in portrait and two above; 2 × 2 up
    /// to four in landscape and three above; an iPad one more above four.
    func testGridColumns() {
        typealias S = RoomPlayerScreen
        XCTAssertEqual(S.columns(count: 1, landscape: false, wide: false), 1)
        XCTAssertEqual(S.columns(count: 4, landscape: false, wide: false), 1)
        XCTAssertEqual(S.columns(count: 5, landscape: false, wide: false), 2)
        XCTAssertEqual(S.columns(count: 10, landscape: false, wide: false), 2)
        XCTAssertEqual(S.columns(count: 4, landscape: true, wide: false), 2, "2 × 2 at four")
        XCTAssertEqual(S.columns(count: 5, landscape: true, wide: false), 3)
        XCTAssertEqual(S.columns(count: 5, landscape: false, wide: true), 3, "iPad portrait")
        XCTAssertEqual(S.columns(count: 5, landscape: true, wide: true), 4, "iPad landscape")
    }

    /// At most four play in Grid; a tap on a paused one swaps out the one
    /// that has played longest; Focus decodes only the focused stream.
    func testFourPlayAndFocusDecodesOnlyTheFocusedStream() async throws {
        let env = ProcessInfo.processInfo.environment
        guard let relay = env["GAWK_SMOKE_RELAY_URL"], !relay.isEmpty,
              let code = env["GAWK_SMOKE_ROOM_CODE"], !code.isEmpty else {
            throw XCTSkip("GAWK_SMOKE_RELAY_URL and GAWK_SMOKE_ROOM_CODE are not set")
        }
        initializeCore()
        let model = RoomPlayerModel(code: code, relayUrl: relay, insecure: true, nickname: "tests")
        model.start()
        defer { model.stop() }
        for _ in 0..<300 where model.tiles.count < 5 {
            try await Task.sleep(for: .milliseconds(50))
        }
        XCTAssertEqual(model.tiles.count, 5, "the fixture's five streams")
        XCTAssertEqual(model.playing.count, RoomPlayerModel.maxPlaying)
        let order = model.playing
        let paused = try XCTUnwrap(model.tiles.map(\.broadcastId).first { !order.contains($0) })

        // Every playing stream gets video.
        try await waitForVideo(model, order)

        model.tap(paused)
        XCTAssertTrue(model.isPlaying(paused))
        XCTAssertFalse(model.isPlaying(order[0]), "the one that played longest was swapped out")
        XCTAssertEqual(model.playing.count, RoomPlayerModel.maxPlaying)

        model.focus(order[1])
        XCTAssertEqual(model.layout, .focus)
        XCTAssertEqual(model.playing, [order[1]])
        try await Task.sleep(for: .seconds(1))
        let before = counts(model)
        try await Task.sleep(for: .seconds(2))
        let after = counts(model)
        for (id, n) in after {
            if id == order[1] {
                XCTAssertGreaterThan(n, before[id] ?? 0, "the focused stream decodes")
            } else {
                XCTAssertEqual(n, before[id] ?? 0, "\(id) is held, not decoded")
            }
        }
    }

    private func counts(_ model: RoomPlayerModel) -> [String: Int] {
        model.engines.mapValues { $0.snapshot().videoEnqueued }
    }

    private func waitForVideo(_ model: RoomPlayerModel, _ ids: [String]) async throws {
        for _ in 0..<400 {
            let c = counts(model)
            if ids.allSatisfy({ (c[$0] ?? 0) > 0 }) { return }
            try await Task.sleep(for: .milliseconds(50))
        }
        XCTFail("not every playing stream got video: \(counts(model))")
    }
}
