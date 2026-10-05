import CoreMedia

/// What's on screen, for D15's drop-to-live rule (docs/67 D15).
///
/// The core decides when the player has fallen too far behind live by
/// comparing the newest frame it received with the one on screen, and only
/// the player knows the latter. Every video item enqueued is logged as its
/// PTS and its broadcast timestamp; the frame on screen at synchronizer time
/// `now` is the newest whose PTS isn't in the future.
///
/// Frames the player deliberately doesn't render (in the background without
/// PiP, D22) are logged all the same: the position is still the live one,
/// and a log that stopped advancing would read as "behind" and make the core
/// drop to live over and over.
struct PresentationLog {
    private var entries: [(pts: CMTime, timestampUs: UInt64)] = []

    /// Logs one enqueued frame. Enqueue order is presentation order.
    mutating func record(pts: CMTime, timestampUs: UInt64) {
        entries.append((pts, timestampUs))
    }

    /// The timestamp of the frame on screen at `now`, or nil before the
    /// first frame's PTS. Older entries are dropped as they're passed.
    mutating func onScreen(at now: CMTime) -> UInt64? {
        guard let last = entries.lastIndex(where: { $0.pts <= now }) else { return nil }
        let shown = entries[last].timestampUs
        entries.removeFirst(last)
        return shown
    }

    /// Frames enqueued but not yet due: the renderer's queue depth.
    func pending(after now: CMTime) -> Int {
        entries.reduce(0) { $0 + ($1.pts > now ? 1 : 0) }
    }

    mutating func removeAll() {
        entries.removeAll(keepingCapacity: true)
    }
}

/// The SPA's decoder-backpressure resync, for a renderer (docs/67 D15: "a
/// deep renderer queue requests the same resync").
///
/// The web resyncs when its decoder's queue grows; here the equivalents are
/// a video path that stays behind (frames reaching the renderer already past
/// their presentation time: the player's own queue, or the decode in front
/// of it, isn't keeping up), or a renderer holding more frames than any
/// playout offset explains. Either asks the core to resync, which flushes
/// both renderers and restarts at the next keyframe.
///
/// iOS 27 deprecates the renderer's `isReadyForMoreMediaData` with the rest
/// of the pre-receiver API, and its receiver's `enqueueImmediately` never
/// refuses; lateness at enqueue is the signal that remains, and the one that
/// measures what the viewer sees.
struct RendererBackpressure {
    /// A frame this late at enqueue is behind, in either preset: Balanced
    /// schedules ahead, and Lowest latency's frames are due on arrival.
    static let lateLimit: Double = 0.25
    /// How long the path may stay behind before we give up on its queue: a
    /// burst after a hiccup catches up in less.
    static let behindLimit: Double = 0.5
    /// More undisplayed frames than this is a queue no offset explains: the
    /// largest Balanced offset (350 ms) is 21 frames at 60 fps.
    static let maxPending = 48
    /// One resync is a keyframe wait; asking again before it lands helps
    /// nothing.
    static let cooldown: Double = 1.0

    private var behindSince: Double?
    private var lastResync: Double?

    /// One observation at host time `now` (seconds): how late the newest
    /// frame was when it was enqueued, and how many are queued. True means
    /// resync now.
    mutating func observe(lateness: Double, pending: Int, now: Double) -> Bool {
        if lateness <= Self.lateLimit {
            behindSince = nil
        } else if behindSince == nil {
            behindSince = now
        }
        let stuck = behindSince.map { now - $0 >= Self.behindLimit } ?? false
        guard stuck || pending > Self.maxPending else { return false }
        if let lastResync, now - lastResync < Self.cooldown { return false }
        lastResync = now
        behindSince = nil
        return true
    }

    mutating func reset() {
        behindSince = nil
    }
}
