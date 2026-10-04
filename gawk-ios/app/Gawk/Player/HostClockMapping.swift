import CoreMedia

/// Maps the viewer core's clock onto the host clock (docs/67 D15).
///
/// Every media item from the core carries `presentAtMs` on the clock
/// `viewerClockMs()` reads: Rust's `Instant`, which on Apple platforms is
/// `mach_absolute_time`, the same timeline `CMClockGetHostTimeClock()`
/// reports. The two differ only by the core's epoch, so one paired sample is
/// the whole mapping: `host = hostAtSample + (presentAtMs − viewerAtSample)`.
/// The player re-samples on every flush anyway, which costs nothing and
/// keeps the mapping honest if that equivalence ever stopped holding.
struct HostClockMapping: Equatable, Sendable {
    /// The core's clock at the sample, in milliseconds.
    let viewerMs: Double
    /// The host clock at the same instant, in seconds.
    let hostSeconds: Double

    /// The host time at which an item stamped `presentAtMs` is due.
    func hostTime(forPresentAtMs presentAtMs: Double) -> CMTime {
        let seconds = hostSeconds + (presentAtMs - viewerMs) / 1000
        return CMTime(seconds: seconds, preferredTimescale: Self.timescale)
    }

    /// The inverse, for the stats and the tests.
    func presentAtMs(forHostTime time: CMTime) -> Double {
        viewerMs + (time.seconds - hostSeconds) * 1000
    }

    /// Nanoseconds: the host clock's own resolution, and fine enough that
    /// rounding never reorders two frames 1 µs apart.
    static let timescale: CMTimeScale = 1_000_000_000

    /// Samples both clocks together. The host clock is read on either side
    /// of the core's and the midpoint taken, so the call's own cost (a
    /// UniFFI hop) splits evenly instead of biasing every PTS one way.
    static func sample(
        viewerClock: () -> Double = viewerClockMs,
        hostClock: () -> CMTime = { CMClockGetTime(CMClockGetHostTimeClock()) }
    ) -> HostClockMapping {
        let before = hostClock().seconds
        let viewer = viewerClock()
        let after = hostClock().seconds
        return HostClockMapping(viewerMs: viewer, hostSeconds: (before + after) / 2)
    }
}
