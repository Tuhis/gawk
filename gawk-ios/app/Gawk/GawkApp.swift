import SwiftUI

@main
struct GawkApp: App {
    @State private var settings: AppSettings
    @State private var broadcast: BroadcastSession
    @State private var capture: Capture
    @State private var router = AppRouter()
    @State private var probes = ServerProbes()
    private let identity: IdentityStore

    init() {
        // The engine's identity before anything dials (docs/67 D5).
        initializeCore()
        let identity = IdentityStore()
        self.identity = identity
        #if DEBUG
        DebugLaunch.resetIfAsked()
        #endif
        let settings = AppSettings()
        #if DEBUG
        DebugLaunch.apply(to: settings, identity: identity)
        #endif
        _settings = State(initialValue: settings)
        let broadcast = BroadcastSession(identity: identity)
        let capture = Capture(identity: identity)
        // docs/70 D15: the Live Activity follows the broadcast, and its End
        // reaches this capture and session.
        broadcast.activity = LiveActivity()
        LiveActivityBridge.shared.attach(capture: capture, session: broadcast)
        // K12: the rooms a broadcast joins are Your rooms too.
        broadcast.onRoomSeen = { [settings] room, relay in
            settings.noteRoom(room.code, name: room.displayName, server: relay)
        }
        _broadcast = State(initialValue: broadcast)
        _capture = State(initialValue: capture)
    }

    var body: some Scene {
        WindowGroup {
            RootView(identity: identity)
                .environment(settings)
                .environment(broadcast)
                .environment(capture)
                .environment(router)
                .environment(probes)
                // docs/70 D2: dark only, on the gawk tokens.
                .preferredColorScheme(.dark)
                .tint(Theme.accent)
                .onOpenURL { router.open(url: $0) }
        }
    }
}

/// Three tabs (docs/70 D1) under the full-screen players.
struct RootView: View {
    let identity: IdentityStore
    @Environment(AppRouter.self) private var router
    @Environment(BroadcastSession.self) private var broadcast

    var body: some View {
        @Bindable var router = router
        TabView(selection: $router.tab) {
            Tab("Watch", systemImage: "play.rectangle", value: AppRouter.Tab.watch) {
                WatchView()
            }
            Tab("Broadcast", systemImage: "dot.radiowaves.left.and.right", value: AppRouter.Tab.broadcast) {
                BroadcastView()
            }
            Tab("Settings", systemImage: "gearshape", value: AppRouter.Tab.settings) {
                SettingsScreen(identity: identity)
            }
        }
        .background(Theme.bg)
        .fullScreenCover(item: $router.screen) { screen in
            Group {
                switch screen {
                case .player(let code, let relay):
                    PlayerScreen(code: code, linkRelay: relay)
                case .room(let code, let relay, let nick):
                    RoomPlayerScreen(code: code, linkRelay: relay, linkNick: nick)
                }
            }
            .preferredColorScheme(.dark)
        }
        .alert(
            "You're live",
            isPresented: Binding(
                get: { router.confirmWhileLive != nil },
                set: { if !$0 { router.confirmWhileLive = nil } })
        ) {
            Button("Watch anyway") { router.confirm() }
            Button("Cancel", role: .cancel) { router.confirmWhileLive = nil }
        } message: {
            Text("Everything on screen is broadcast, so this stream would be too.")
        }
        .onChange(of: broadcast.isActive, initial: true) { _, active in
            router.isBroadcasting = active
        }
    }
}

#if DEBUG
/// Launch arguments the UI tests start the app with (docs/67 D26, phase
/// S): `-gawkReset` starts from no settings and an empty Keychain, and
/// `-gawkRelay <url>` (with `-gawkSecret <secret>`) selects a local relay
/// with its dev certificate accepted. Debug builds only.
@MainActor
enum DebugLaunch {
    private static var args: [String] { ProcessInfo.processInfo.arguments }

    static func resetIfAsked() {
        guard args.contains("-gawkReset"), let id = Bundle.main.bundleIdentifier else { return }
        UserDefaults.standard.removePersistentDomain(forName: id)
    }

    static func apply(to settings: AppSettings, identity: IdentityStore) {
        // No stored broadcast, secret or room key from an earlier run.
        if args.contains("-gawkReset") { identity.removeAll() }
        if let i = args.firstIndex(of: "-gawkRelay"), i + 1 < args.count {
            let url = args[i + 1]
            settings.addServer(name: "local", url: url)
            settings.selectedURL = url
            settings.insecure = true
            if let j = args.firstIndex(of: "-gawkSecret"), j + 1 < args.count {
                identity.setSecret(args[j + 1], relay: url)
            }
        }
        if let i = args.firstIndex(of: "-gawkNickname"), i + 1 < args.count {
            settings.nickname = args[i + 1]
        }
    }
}
#endif
