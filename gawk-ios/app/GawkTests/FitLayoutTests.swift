@testable import Gawk
import XCTest

/// R83 Fit (docs/87): the Swift port of the web's `fitLayout.ts`, held to
/// the same cases and to §12's table, so both clients place a room alike.
@MainActor
final class FitLayoutTests: XCTestCase {
    private let ultrawide = 32.0 / 9.0
    private let wide = 64.0 / 27.0
    private let hd = 16.0 / 9.0
    private let tall = 16.0 / 10.0
    private let phone = 9.0 / 19.5

    private func assertSane(_ rects: [CGRect], _ aspects: [Double], in size: CGSize, file: StaticString = #filePath, line: UInt = #line) {
        let eps = 0.01
        for (i, r) in rects.enumerated() {
            XCTAssertGreaterThanOrEqual(r.minX, -eps, file: file, line: line)
            XCTAssertGreaterThanOrEqual(r.minY, -eps, file: file, line: line)
            XCTAssertLessThanOrEqual(r.maxX, size.width + eps, file: file, line: line)
            XCTAssertLessThanOrEqual(r.maxY, size.height + eps, file: file, line: line)
            XCTAssertLessThan(abs(Double(r.width / r.height) / aspects[i] - 1), 0.02, "tile \(i) keeps its shape", file: file, line: line)
        }
        for i in rects.indices {
            for j in rects.indices where j > i {
                XCTAssertFalse(rects[i].insetBy(dx: eps, dy: eps).intersects(rects[j].insetBy(dx: eps, dy: eps)), "tiles \(i) and \(j) overlap", file: file, line: line)
            }
        }
    }

    /// The owner's report: two 32:9 streams stack on a 16:9 screen and fill
    /// it, where a side-by-side grid uses a quarter.
    func testTwoUltrawidesStack() {
        let size = CGSize(width: 1920, height: 1080)
        let aspects = [ultrawide, ultrawide]
        let rects = FitLayout.layout(aspects, in: size).rects
        assertSane(rects, aspects, in: size)
        XCTAssertGreaterThanOrEqual(FitLayout.coverage(rects, in: size), 0.95)
        XCTAssertGreaterThan(rects[1].minY, rects[0].maxY - 1, "one above the other")
        XCTAssertLessThan(FitLayout.coverage(FitLayout.gridLayout(aspects, in: size), in: size), 0.26)
    }

    /// docs/87 §12: coverage, Grid → Fit, ten mixes at four sizes, the same
    /// numbers the web's table test checks.
    func testTwelveTable() {
        let stages = [CGSize(width: 1920, height: 1080), CGSize(width: 1580, height: 1080),
                      CGSize(width: 2560, height: 1080), CGSize(width: 390, height: 844)]
        let table: [([Double], [Int], [Int])] = [
            ([ultrawide, ultrawide], [25, 21, 33, 6], [100, 82, 75, 26]),
            ([wide, wide], [37, 31, 50, 10], [66, 81, 50, 39]),
            ([hd, hd], [50, 41, 67, 13], [50, 61, 67, 52]),
            ([ultrawide, hd], [37, 31, 50, 10], [75, 81, 56, 39]),
            ([hd, hd, hd], [75, 62, 56, 19], [75, 71, 89, 78]),
            ([ultrawide, ultrawide, hd], [50, 41, 52, 13], [83, 67, 82, 52]),
            ([hd, phone], [51, 52, 53, 31], [79, 65, 94, 79]),
            ([hd, hd, hd, hd], [100, 82, 75, 26], [100, 82, 75, 95]),
            ([ultrawide, hd, wide, tall, hd], [48, 40, 64, 12], [91, 89, 85, 87]),
            ([hd, hd, hd, phone], [81, 69, 61, 44], [81, 94, 64, 77]),
        ]
        var above = 0
        var equal = 0
        for (aspects, grid, fit) in table {
            for (s, size) in stages.enumerated() {
                let g = Int((100 * FitLayout.coverage(FitLayout.gridLayout(aspects, in: size), in: size)).rounded())
                let rects = FitLayout.layout(aspects, in: size).rects
                assertSane(rects, aspects, in: size)
                let f = Int((100 * FitLayout.coverage(rects, in: size)).rounded())
                XCTAssertEqual(g, grid[s], "grid \(aspects) @ \(size)")
                XCTAssertEqual(f, fit[s], "fit \(aspects) @ \(size)")
                if f > g { above += 1 } else if f == g { equal += 1 }
            }
        }
        XCTAssertEqual(above, 32)
        XCTAssertEqual(equal, 8)
    }

    /// Never below Grid, fresh or with a previous arrangement (D3, D5).
    func testNeverBelowGrid() {
        var seed: UInt64 = 7
        func rnd() -> Double {
            seed = (seed &* 6364136223846793005 &+ 1442695040888963407)
            return Double(seed >> 11) / Double(1 << 53)
        }
        for _ in 0..<400 {
            let n = 1 + Int(rnd() * 12)
            let aspects = (0..<n).map { _ in 0.3 + rnd() * 3.5 }
            let size = CGSize(width: 300 + rnd() * 2600, height: 300 + rnd() * 1400)
            let fit = FitLayout.layout(aspects, in: size)
            XCTAssertGreaterThanOrEqual(fit.score, FitLayout.score(FitLayout.gridLayout(aspects, in: size)) - 1e-9)
            let size2 = CGSize(width: size.width * (0.85 + rnd() * 0.3), height: size.height * (0.85 + rnd() * 0.3))
            let kept = FitLayout.layout(aspects, in: size2, previous: fit)
            XCTAssertGreaterThanOrEqual(kept.score, FitLayout.score(FitLayout.gridLayout(aspects, in: size2)) - 1e-9)
        }
    }

    /// D5's floor: a phone and a 16:9 stream in one row at 1580 pt is within
    /// 3 % of the best at 1501 pt but below Grid there, so it gives way.
    func testFloorDropsAKeptArrangement() {
        let aspects = [phone, hd]
        let prev = FitLayout.layout(aspects, in: CGSize(width: 1580, height: 1080))
        let size = CGSize(width: 1501, height: 1080)
        let kept = FitLayout.place(aspects, in: size, prev.arrangement!)!
        let fresh = FitLayout.layout(aspects, in: size)
        let grid = FitLayout.score(FitLayout.gridLayout(aspects, in: size))
        XCTAssertGreaterThanOrEqual(FitLayout.score(kept), fresh.score - FitLayout.stickiness * 2)
        XCTAssertLessThan(FitLayout.score(kept), grid)
        XCTAssertEqual(FitLayout.layout(aspects, in: size, previous: prev), fresh)
    }

    /// D5: a small resize keeps the arrangement; new shapes start fresh.
    func testStickiness() {
        let aspects = [hd, hd, hd]
        let first = FitLayout.layout(aspects, in: CGSize(width: 2200, height: 1080))
        var flip: CGFloat = 0
        for w in stride(from: 2200.0, through: 2800, by: 5) {
            if FitLayout.layout(aspects, in: CGSize(width: w, height: 1080)).arrangement != first.arrangement {
                flip = w
                break
            }
        }
        XCTAssertGreaterThan(flip, 0)
        XCTAssertEqual(FitLayout.layout(aspects, in: CGSize(width: flip, height: 1080), previous: first).arrangement, first.arrangement)
        let changed = [ultrawide, hd, hd]
        XCTAssertEqual(FitLayout.layout(changed, in: CGSize(width: 2400, height: 1080), previous: first),
                       FitLayout.layout(changed, in: CGSize(width: 2400, height: 1080)))
    }

    /// A phone in portrait: two 16:9 streams stack and fill the width.
    func testPhonePortrait() {
        let size = CGSize(width: 393, height: 760)
        let rects = FitLayout.layout([hd, hd], in: size).rects
        assertSane(rects, [hd, hd], in: size)
        XCTAssertEqual(rects[0].width, 393, accuracy: 0.5)
        XCTAssertGreaterThan(rects[1].minY, rects[0].maxY - 1)
    }

    func testDegenerateInput() {
        XCTAssertTrue(FitLayout.layout([], in: CGSize(width: 100, height: 100)).rects.isEmpty)
        XCTAssertTrue(FitLayout.layout([hd], in: .zero).rects.isEmpty)
        XCTAssertEqual(FitLayout.clampAspect(0), FitLayout.defaultAspect)
        XCTAssertEqual(FitLayout.clampAspect(.nan), FitLayout.defaultAspect)
        XCTAssertEqual(FitLayout.clampAspect(100), FitLayout.maxAspect)
        let rects = FitLayout.layout([100, 0.01], in: CGSize(width: 1920, height: 1080)).rects
        assertSane(rects, [FitLayout.maxAspect, FitLayout.minAspect], in: CGSize(width: 1920, height: 1080))
    }

    /// Ten tiles is the exhaustive worst case; 40 takes the even splits.
    func testWallsLayOutQuickly() {
        for n in [10, 40] {
            let aspects = (0..<n).map { [hd, ultrawide, 4.0 / 3.0, phone][$0 % 4] }
            let start = Date()
            let rects = FitLayout.layout(aspects, in: CGSize(width: 1920, height: 1080)).rects
            XCTAssertEqual(rects.count, n)
            XCTAssertLessThan(Date().timeIntervalSince(start), 0.25, "\(n) tiles")
        }
    }

    /// D7: the model takes a stream's shape from its decoded size, and
    /// ignores a move within 2 %.
    func testModelShapeDeadband() {
        let model = RoomPlayerModel(code: "AB2CD3", relayUrl: "https://127.0.0.1:4433", insecure: true, nickname: "t")
        model.noteVideoSize("AAAAAA", width: 1920, height: 1080)
        XCTAssertEqual(model.shapes["AAAAAA"]!, hd, accuracy: 1e-9)
        model.noteVideoSize("AAAAAA", width: 1940, height: 1080)
        XCTAssertEqual(model.shapes["AAAAAA"]!, hd, accuracy: 1e-9, "a 1 % move is ignored")
        model.noteVideoSize("AAAAAA", width: 3840, height: 1080)
        XCTAssertEqual(model.shapes["AAAAAA"]!, ultrawide, accuracy: 1e-9)
        model.noteVideoSize("AAAAAA", width: 0, height: 1080)
        XCTAssertEqual(model.shapes["AAAAAA"]!, ultrawide, accuracy: 1e-9, "a zero size is no size")
    }
}
