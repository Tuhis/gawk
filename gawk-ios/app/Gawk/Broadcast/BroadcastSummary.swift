import Foundation

/// After End (docs/70 B4, D14, K5): how long it was live, the most who
/// watched at once, and the average upload, kept from the session's events
/// as docs/60 DR3 has.
struct BroadcastSummary: Equatable {
    let timeLive: TimeInterval
    let mostWatching: UInt32
    /// Bits per second; `nil` when no second of upload was sampled.
    let averageUploadBps: UInt64?
}

/// Builds the summary as the broadcast runs. Pure, so the numbers are
/// tested without a relay.
struct SummaryTracker: Equatable {
    private(set) var liveSince: Date?
    private var mostWatching: UInt32 = 0
    private var uploadTotal: Double = 0
    private var uploadSamples = 0

    /// The first Live starts the clock; a reconnect's Live doesn't restart it.
    mutating func live(at date: Date) {
        if liveSince == nil { liveSince = date }
    }

    mutating func viewers(_ n: UInt32) {
        mostWatching = max(mostWatching, n)
    }

    /// One second's send rate.
    mutating func upload(bps: UInt64) {
        uploadTotal += Double(bps)
        uploadSamples += 1
    }

    /// The summary at `end`; `nil` for a broadcast that never went live.
    func summary(at end: Date) -> BroadcastSummary? {
        guard let liveSince else { return nil }
        return BroadcastSummary(
            timeLive: max(0, end.timeIntervalSince(liveSince)),
            mostWatching: mostWatching,
            averageUploadBps: uploadSamples > 0 ? UInt64(uploadTotal / Double(uploadSamples)) : nil
        )
    }
}

/// The broadcaster's words for numbers: the desktop's (`shell.rs`
/// `format_duration`, `fmt_mbps`).
enum BroadcastText {
    /// "42 seconds", "1 minute", "42 minutes", "1 h 12 min".
    static func duration(_ t: TimeInterval) -> String {
        let secs = Int(max(0, t))
        switch secs {
        case 0..<60: return "\(secs) second\(secs == 1 ? "" : "s")"
        case 60..<3600:
            let m = secs / 60
            return "\(m) minute\(m == 1 ? "" : "s")"
        default: return "\(secs / 3600) h \((secs / 60) % 60) min"
        }
    }

    /// "6.2 Mbps", or "850 kbps" under one.
    static func rate(_ bps: UInt64) -> String {
        bps >= 1_000_000
            ? String(format: "%.1f Mbps", Double(bps) / 1e6)
            : String(format: "%.0f kbps", Double(bps) / 1e3)
    }

    /// A cap in Mbps: "8", "2.5".
    static func mbps(_ bps: UInt32) -> String {
        let m = Double(bps) / 1e6
        return abs(m - m.rounded()) < 0.05 ? String(format: "%.0f", m) : String(format: "%.1f", m)
    }

    /// Live's Quality row (D11): what's encoded, then the encoder and cap.
    static func liveQuality(_ c: BroadcastCounters) -> (title: String, detail: String)? {
        guard c.width > 0, c.fps > 0 else { return nil }
        return ("\(c.width)×\(c.height) · \(c.fps) fps", "H.264 hardware · up to \(mbps(c.peakBitrateBps)) Mbps")
    }
}
