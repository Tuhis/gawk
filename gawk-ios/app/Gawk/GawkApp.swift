import SwiftUI

@main
struct GawkApp: App {
    init() {
        // The engine's identity before anything dials (docs/67 D5).
        initializeCore()
    }

    var body: some Scene {
        WindowGroup {
            RootView()
        }
    }
}

/// The app's four screens (docs/67 D1). IO1 lays them down; the chunks that
/// own each one fill it (Watch IO5/IO6, Broadcast IO3, Rooms IO6).
struct RootView: View {
    var body: some View {
        TabView {
            Tab("Watch", systemImage: "play.rectangle") {
                PlaceholderView(title: "Watch")
            }
            Tab("Broadcast", systemImage: "dot.radiowaves.left.and.right") {
                PlaceholderView(title: "Broadcast")
            }
            Tab("Rooms", systemImage: "square.grid.2x2") {
                PlaceholderView(title: "Rooms")
            }
            Tab("Settings", systemImage: "gearshape") {
                SettingsView()
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

struct SettingsView: View {
    private let info = coreInfo()

    var body: some View {
        NavigationStack {
            List {
                Section("Server") {
                    LabeledContent("Default relay", value: info.defaultRelayUrl)
                }
                Section("About") {
                    LabeledContent("Version", value: info.version)
                    LabeledContent("Distribution", value: info.distribution)
                }
            }
            .navigationTitle("Settings")
        }
    }
}
