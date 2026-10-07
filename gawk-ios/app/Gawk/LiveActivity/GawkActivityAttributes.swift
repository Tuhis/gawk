import ActivityKit
import Foundation

/// The broadcast's Live Activity (docs/70 D15, K9), compiled into the app
/// and the `GawkLiveActivity` widget extension. The extension only draws
/// what the app sends it: no media, no Keychain, no gawk Rust code.
struct GawkActivityAttributes: ActivityAttributes {
    /// What changes while live.
    struct ContentState: Codable, Hashable {
        var viewers: UInt32
        var reconnecting: Bool
    }

    /// The broadcast's code, its join link, and when it went live (the
    /// LIVE chip's timer).
    var code: String
    var link: String
    var since: Date
}
