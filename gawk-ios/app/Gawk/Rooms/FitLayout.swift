import CoreGraphics
import Foundation

/// R83 Fit (docs/87): the room layout that follows the streams' shapes. A
/// pure function from each tile's aspect ratio and the space's size to one
/// rectangle per tile, so as much of the screen as the shapes allow is
/// video. A line-for-line port of the web app's `fitLayout.ts`, so both
/// clients place the same room the same way.
///
/// Candidates (D3), in two families:
/// - rows: order-preserving groups of consecutive tiles laid out as rows
///   (or, transposed, as columns), each sized "justified" (a row's tiles
///   share a height and fill the width, then the block scales to fit the
///   height) or "balanced" (each row capped at an equal share of the
///   height). Every split up to `exhaustiveMax` tiles, the even splits
///   above it.
/// - grid: equal cells for every column count, each tile the video's
///   rectangle centred in its cell.
///
/// The score is Σ ln(tile area) (D4); the block is centred (D6).
enum FitLayout {
    enum Sizing: Equatable, Sendable { case justified, balanced }

    enum Arrangement: Equatable, Sendable {
        case rows(transpose: Bool, groups: [Int], sizing: Sizing)
        case grid(cols: Int)
    }

    struct Result: Equatable, Sendable {
        var rects: [CGRect]
        var arrangement: Arrangement?
        var score: Double
        /// What the arrangement was chosen for: D5 keeps it only while these
        /// match.
        var aspects: [Double]

        static let empty = Result(rects: [], arrangement: nil, score: -.infinity, aspects: [])
    }

    static let defaultAspect = 16.0 / 9.0
    static let minAspect = 1.0 / 4.0
    static let maxAspect = 4.0
    static let defaultGap: CGFloat = 2
    /// Every order-preserving split is tried up to this many tiles.
    static let exhaustiveMax = 10
    /// D5: a new best must beat the arrangement in use by more than 3 % in
    /// geometric-mean tile area.
    static let stickiness = log(1.03)
    /// D7: a tile's shape is taken only when it moves by more than this.
    static let aspectDeadband = 0.02

    static func aspectChanged(_ current: Double?, _ next: Double) -> Bool {
        guard let current else { return true }
        return abs(next / current - 1) > aspectDeadband
    }

    static func clampAspect(_ a: Double) -> Double {
        guard a.isFinite, a > 0 else { return defaultAspect }
        return min(maxAspect, max(minAspect, a))
    }

    static func score(_ rects: [CGRect]) -> Double {
        var s = 0.0
        for r in rects {
            let area = Double(r.width * r.height)
            guard area > 0 else { return -.infinity }
            s += log(area)
        }
        return s
    }

    static func coverage(_ rects: [CGRect], in size: CGSize) -> Double {
        guard size.width > 0, size.height > 0 else { return 0 }
        return rects.reduce(0.0) { $0 + Double($1.width * $1.height) } / Double(size.width * size.height)
    }

    /// The web's Grid: ceil(√n) columns. The comparison baseline and D5's
    /// floor.
    static func gridColumns(_ n: Int) -> Int {
        max(1, Int(Double(n).squareRoot().rounded(.up)))
    }

    static func gridLayout(_ aspects: [Double], in size: CGSize, gap: CGFloat = defaultGap) -> [CGRect] {
        guard !aspects.isEmpty else { return [] }
        return placeGrid(aspects.map(clampAspect), size: size, gap: gap, cols: gridColumns(aspects.count)) ?? []
    }

    private static func placeGrid(_ aspects: [Double], size: CGSize, gap: CGFloat, cols: Int) -> [CGRect]? {
        let n = aspects.count
        let rows = (n + cols - 1) / cols
        let cw = Double(size.width - CGFloat(cols - 1) * gap) / Double(cols)
        let ch = Double(size.height - CGFloat(rows - 1) * gap) / Double(rows)
        guard cw > 0, ch > 0 else { return nil }
        return aspects.enumerated().map { i, a in
            let col = i % cols
            let row = i / cols
            let wide = cw / ch > a
            let w = wide ? ch * a : cw
            let h = wide ? ch : cw / a
            let x = Double(col) * (cw + Double(gap)) + (cw - w) / 2
            let y = Double(row) * (ch + Double(gap)) + (ch - h) / 2
            return CGRect(x: x, y: y, width: w, height: h)
        }
    }

    /// Row heights for rows of consecutive tiles, or nil when the space
    /// can't hold them. `spans[r]` is the sum of the row's shapes.
    private static func rowHeights(
        spans: [Double], groups: [Int], W: Double, H: Double, gap: Double, sizing: Sizing
    ) -> [Double]? {
        let R = groups.count
        let avail = H - Double(R - 1) * gap
        guard avail > 0 else { return nil }
        var heights = [Double](repeating: 0, count: R)
        var sum = 0.0
        for r in 0..<R {
            let free = W - Double(groups[r] - 1) * gap
            guard free > 0 else { return nil }
            heights[r] = free / spans[r]
            sum += heights[r]
        }
        switch sizing {
        case .justified:
            let scale = min(1, avail / sum)
            for r in 0..<R { heights[r] *= scale }
        case .balanced:
            let budget = avail / Double(R)
            for r in 0..<R { heights[r] = min(heights[r], budget) }
        }
        return heights
    }

    private static func placeRows(
        _ aspects: [Double], size: CGSize, gap: CGFloat, groups: [Int], transpose: Bool, sizing: Sizing
    ) -> [CGRect]? {
        // In the transposed frame a "row" is a column: swap the space and
        // invert the shapes, then swap each rectangle back.
        let W = Double(transpose ? size.height : size.width)
        let H = Double(transpose ? size.width : size.height)
        let g = Double(gap)
        let A = transpose ? aspects.map { 1 / $0 } : aspects
        var spans: [Double] = []
        var start = 0
        for m in groups {
            spans.append(A[start..<(start + m)].reduce(0, +))
            start += m
        }
        guard let rowH = rowHeights(spans: spans, groups: groups, W: W, H: H, gap: g, sizing: sizing) else {
            return nil
        }
        let total = rowH.reduce(0, +) + Double(groups.count - 1) * g
        var rects: [CGRect] = []
        var y = (H - total) / 2
        start = 0
        for (r, m) in groups.enumerated() {
            let h = rowH[r]
            var x = (W - (spans[r] * h + Double(m - 1) * g)) / 2
            for i in start..<(start + m) {
                let w = A[i] * h
                rects.append(transpose
                    ? CGRect(x: y, y: x, width: h, height: w)
                    : CGRect(x: x, y: y, width: w, height: h))
                x += w + g
            }
            y += h + g
            start += m
        }
        return rects
    }

    /// One arrangement in a given space.
    static func place(_ aspects: [Double], in size: CGSize, gap: CGFloat = defaultGap, _ arrangement: Arrangement) -> [CGRect]? {
        switch arrangement {
        case .grid(let cols):
            placeGrid(aspects, size: size, gap: gap, cols: cols)
        case .rows(let transpose, let groups, let sizing):
            placeRows(aspects, size: size, gap: gap, groups: groups, transpose: transpose, sizing: sizing)
        }
    }

    /// Group sizes for every order-preserving split of n tiles (n ≤
    /// `exhaustiveMax`), or only the even splits into k groups above it.
    private static func splits(_ n: Int) -> [[Int]] {
        var out: [[Int]] = []
        if n <= exhaustiveMax {
            for mask in 0..<(1 << (n - 1)) {
                var groups: [Int] = []
                var run = 1
                for i in 1..<n {
                    if mask & (1 << (i - 1)) != 0 {
                        groups.append(run)
                        run = 1
                    } else {
                        run += 1
                    }
                }
                groups.append(run)
                out.append(groups)
            }
            return out
        }
        for k in 1...n {
            let base = n / k
            let extra = n % k
            out.append((0..<k).map { base + ($0 < extra ? 1 : 0) })
        }
        return out
    }

    /// The rows family scored without building rectangles: a tile's area is
    /// A·h² in either frame, so Σ ln(area) = Σ ln A + Σ_r m_r · 2 ln h_r.
    private static func bestRows(_ aspects: [Double], size: CGSize, gap: CGFloat) -> (Arrangement, Double)? {
        let all = splits(aspects.count)
        var best: (Arrangement, Double)?
        for transpose in [false, true] {
            let W = Double(transpose ? size.height : size.width)
            let H = Double(transpose ? size.width : size.height)
            let A = transpose ? aspects.map { 1 / $0 } : aspects
            var prefix = [0.0]
            var lnSum = 0.0
            for a in A {
                prefix.append(prefix[prefix.count - 1] + a)
                lnSum += log(a)
            }
            for groups in all {
                var spans: [Double] = []
                var start = 0
                for m in groups {
                    spans.append(prefix[start + m] - prefix[start])
                    start += m
                }
                for sizing in [Sizing.justified, .balanced] {
                    guard let heights = rowHeights(
                        spans: spans, groups: groups, W: W, H: H, gap: Double(gap), sizing: sizing)
                    else { continue }
                    var score = lnSum
                    for r in groups.indices { score += Double(groups[r]) * 2 * log(heights[r]) }
                    if best == nil || score > best!.1 {
                        best = (.rows(transpose: transpose, groups: groups, sizing: sizing), score)
                    }
                }
            }
        }
        return best
    }

    static func layout(
        _ rawAspects: [Double], in size: CGSize, gap: CGFloat = defaultGap, previous: Result? = nil
    ) -> Result {
        let n = rawAspects.count
        guard n > 0, size.width > 0, size.height > 0 else { return .empty }
        let aspects = rawAspects.map(clampAspect)

        var best: Result?
        if let rows = bestRows(aspects, size: size, gap: gap),
           let rects = place(aspects, in: size, gap: gap, rows.0) {
            best = Result(rects: rects, arrangement: rows.0, score: score(rects), aspects: aspects)
        }
        for cols in 1...n {
            guard let rects = placeGrid(aspects, size: size, gap: gap, cols: cols) else { continue }
            let s = score(rects)
            if best == nil || s > best!.score {
                best = Result(rects: rects, arrangement: .grid(cols: cols), score: s, aspects: aspects)
            }
        }
        guard let best, best.score > -.infinity else {
            return Result(rects: [], arrangement: nil, score: -.infinity, aspects: aspects)
        }

        // D5: same tiles and shapes, only the space changed: keep the
        // arrangement in use unless the best beats it by more than
        // `stickiness` per tile, or it has fallen below Grid here.
        if let prev = previous, let arrangement = prev.arrangement, prev.aspects == aspects,
           let rects = place(aspects, in: size, gap: gap, arrangement) {
            let s = score(rects)
            let floor = score(placeGrid(aspects, size: size, gap: gap, cols: gridColumns(n)) ?? [])
            if s >= best.score - stickiness * Double(n), s >= floor {
                return Result(rects: rects, arrangement: arrangement, score: s, aspects: aspects)
            }
        }
        return best
    }
}
