import AppIntents
import Foundation
#if GAWK_APP
import UIKit
#endif

// The Live Activity's buttons (docs/70 D15). A `LiveActivityIntent` runs in
// the app's process, so only the app's build does anything; the widget
// extension compiles these for its buttons and never performs them.

/// End: the same stop as the app's End broadcast.
struct EndBroadcastIntent: LiveActivityIntent {
    static let title: LocalizedStringResource = "End broadcast"

    func perform() async throws -> some IntentResult {
        #if GAWK_APP
        await LiveActivityBridge.shared.endBroadcast()
        #endif
        return .result()
    }
}

/// Copy link: the join link on the pasteboard.
struct CopyLinkIntent: LiveActivityIntent {
    static let title: LocalizedStringResource = "Copy link"

    @Parameter(title: "Link")
    var link: String

    init() {}

    init(link: String) {
        self.link = link
    }

    func perform() async throws -> some IntentResult {
        #if GAWK_APP
        let link = link
        await MainActor.run { UIPasteboard.general.string = link }
        #endif
        return .result()
    }
}
