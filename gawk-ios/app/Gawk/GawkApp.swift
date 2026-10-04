import SwiftUI

@main
struct GawkApp: App {
    @State private var settings: AppSettings
    @State private var broadcast: BroadcastSession
    @State private var capture: Capture
    private let identity: IdentityStore

    init() {
        // The engine's identity before anything dials (docs/67 D5).
        initializeCore()
        let identity = IdentityStore()
        self.identity = identity
        _settings = State(initialValue: AppSettings())
        _broadcast = State(initialValue: BroadcastSession(identity: identity))
        _capture = State(initialValue: Capture(identity: identity))
    }

    var body: some Scene {
        WindowGroup {
            RootView(identity: identity)
                .environment(settings)
                .environment(broadcast)
                .environment(capture)
        }
    }
}

/// The app's four screens (docs/67 D1). Watch is IO5/IO6's and Rooms IO6's;
/// they fill in as those land.
struct RootView: View {
    let identity: IdentityStore

    var body: some View {
        TabView {
            Tab("Watch", systemImage: "play.rectangle") {
                WatchView()
            }
            Tab("Broadcast", systemImage: "dot.radiowaves.left.and.right") {
                BroadcastView()
            }
            Tab("Rooms", systemImage: "square.grid.2x2") {
                PlaceholderView(title: "Rooms")
            }
            Tab("Settings", systemImage: "gearshape") {
                SettingsScreen(identity: identity)
            }
        }
    }
}

struct PlaceholderView: View {
    let title: String

    var body: some View {
        NavigationStack {
            ContentUnavailableView(title, systemImage: "hammer")
                .navigationTitle(title)
        }
    }
}
