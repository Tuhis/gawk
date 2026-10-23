import SwiftUI

/// The player (docs/70 A2, A3, A7; D5–D9): only the video until a tap shows
/// the glass controls over it, as on YouTube and Twitch (OD1). Full screen
/// over the tabs, black, aspect-fit, in either orientation.
struct PlayerScreen: View {
    let code: String
    /// A link's server, the default fleet when it named none, or `nil` for
    /// Settings' choice (`AppRouter.Screen`).
    let linkRelay: String?
    @Environment(AppSettings.self) private var settings
    @Environment(AppRouter.self) private var router
    @Environment(LocalRelaySwitcher.self) private var switcher
    @Environment(\.scenePhase) private var scenePhase
    @State private var model: WatchModel?
    @State private var controls = ControlsVisibility()
    @State private var showStats = false

    var body: some View {
        GeometryReader { geo in
            let landscape = geo.size.width > geo.size.height
            ZStack {
                Color.black.ignoresSafeArea()
                if let engine = model?.engine {
                    // With Stats out, the video moves up out of the drawer's
                    // way: its tiles sit under the video (D7).
                    PlayerView(layer: engine.displayLayer)
                        .padding(.bottom, showStats && !landscape ? StatsDrawer.smallHeight : 0)
                        .animation(Theme.fade, value: showStats)
                        .ignoresSafeArea()
                        .accessibilityLabel("Video")
                        .accessibilityAddTraits(.isImage)
                }
                Color.clear
                    .contentShape(.rect)
                    .ignoresSafeArea()
                    .onTapGesture { controls.toggle() }
                    .accessibilityHidden(true)
                if let model {
                    centre(model)
                    chrome(model, landscape: landscape)
                }
            }
        }
        .statusBarHidden(!controls.visible)
        .persistentSystemOverlays(controls.visible ? .automatic : .hidden)
        .onAppear(perform: start)
        .onDisappear {
            model?.stop()
            switcher.onSwitch = nil
        }
        .onChange(of: model?.isLive ?? false) { _, live in controls.pinned = !live || showStats }
        .onChange(of: showStats) { _, open in controls.pinned = open || !(model?.isLive ?? false) }
        .onChange(of: scenePhase) { _, phase in
            // .inactive is a transition (the app switcher, a PiP start);
            // only .background means nobody sees the player.
            if phase == .background {
                model?.setForeground(false)
            } else if phase == .active {
                model?.setForeground(true)
            }
        }
        .sheet(isPresented: $showStats) {
            if let model { StatsDrawer(model: model) }
        }
    }

    private func start() {
        guard model == nil else { return }
        let m: WatchModel
        if linkRelay == nil || linkRelay == settings.relayURL {
            // R74 (docs/76 §6.12): on home, a held party relay takes the
            // player at once; otherwise it watches home with lr=1, and a
            // fresh offer on its session switches it, once.
            let t = switcher.target(home: settings.relayURL, homeCertHashes: settings.relayCertHashes)
            m = WatchModel(
                code: code, relayUrl: t.url, insecure: t.pop == nil ? settings.insecure : false,
                certHashes: t.certHashes, followOffers: t.followOffers, localRelay: t.pop,
                home: WatchModel.Endpoint(
                    url: settings.relayURL, insecure: settings.insecure, certHashes: settings.relayCertHashes))
            m.onOffer = { [switcher, home = settings.relayURL] offer in
                switcher.consider(offer, live: false, home: home)
            }
            m.onLocalRelayFailed = { [switcher] in switcher.refused() }
            switcher.onSwitch = { [weak m, home = settings.relayURL] pop in
                if pop.home == home { m?.switchTo(pop) }
            }
        } else {
            m = WatchModel(
                code: code, relayUrl: linkRelay ?? settings.relayURL, insecure: settings.insecure,
                certHashes: settings.certHashes(for: linkRelay ?? settings.relayURL))
        }
        model = m
        controls.pinned = true
        m.watch()
    }

    private func close() {
        model?.stop()
        router.screen = nil
    }

    /// What sits in the middle: connecting, or the card for a stream that
    /// isn't there (D9).
    @ViewBuilder private func centre(_ model: WatchModel) -> some View {
        if let codec = model.unsupportedCodec {
            PlayerCard(card: WatchStatusText.unsupported(codec), close: close, retry: model.watch)
        } else if case .ended(let reason) = model.status, let card = WatchStatusText.card(reason, code: code) {
            PlayerCard(card: card, close: close, retry: model.watch)
        } else if model.status == .connecting {
            VStack(spacing: 14) {
                ProgressView().controlSize(.large).tint(Theme.text)
                Text(WatchStatusText.connecting(code))
                    .font(.subheadline)
                    .foregroundStyle(Theme.muted)
            }
            .accessibilityElement(children: .combine)
            .accessibilityIdentifier("player.connecting")
        }
    }

    private func chrome(_ model: WatchModel, landscape: Bool) -> some View {
        PlayerChrome(controls: controls, bottomHidden: showStats) {
            GlassCircleButton(systemImage: "xmark", label: "Close", action: close)
                .accessibilityIdentifier("player.close")
            statusChip(model)
            if let n = model.stats?.viewerCount {
                ViewerCountChip(count: n)
            }
        } topTrailing: {
            CodePill(code: code, link: watchLink(broadcastId: code), onCopy: controls.touch)
            if model.pip?.isSupported == true {
                GlassCircleButton(systemImage: "pip.enter", label: "Picture in Picture") {
                    model.pip?.start()
                }
                .accessibilityIdentifier("player.pip")
            }
        } below: {
            if let chip = AppRouter.chip(for: linkRelay) {
                ServerChip(url: chip)
            }
        } bottomLeading: {
            EmptyView()
        } bottomTrailing: {
            PlayerSettingsMenu(
                preset: Binding(get: { model.preset }, set: { model.preset = $0; controls.touch() }),
                shareURL: watchLink(broadcastId: code)
            ) {
                showStats = true
            }
            FullScreenButton(isLandscape: landscape)
        }
    }

    @ViewBuilder private func statusChip(_ model: WatchModel) -> some View {
        switch model.status {
        case .live:
            LiveChip().accessibilityIdentifier("player.live")
        case .reconnecting(_, _, let draining, let partyFull):
            ReconnectingChip(text: WatchStatusText.reconnecting(draining: draining, partyFull: partyFull))
                .accessibilityIdentifier("player.reconnecting")
        default:
            EmptyView()
        }
    }
}

/// The player's settings (docs/70 A4, D6): Latency, applied at once; Share…;
/// and, on a single stream, Stats. Copy link is the code pill's (OD12).
struct PlayerSettingsMenu: View {
    @Binding var preset: Preset
    let shareURL: String
    var stats: (() -> Void)?

    var body: some View {
        Menu {
            Section("Latency") {
                Picker("Latency", selection: $preset) {
                    Text("Balanced").tag(Preset.balanced)
                    Text("Lowest latency").tag(Preset.lowestLatency)
                }
                .pickerStyle(.inline)
            }
            if let url = URL(string: shareURL) {
                ShareLink("Share…", item: url)
            }
            if let stats {
                Button("Stats", systemImage: "chart.bar", action: stats)
            }
        } label: {
            GlassCircleLabel(systemImage: "gearshape")
        }
        .menuOrder(.fixed)
        .accessibilityLabel("Settings")
        // The latency in force, so VoiceOver reads it without the menu
        // open, and a UI test can see that a choice took.
        .accessibilityValue(preset == .lowestLatency ? "Lowest latency" : "Balanced")
        .accessibilityIdentifier("player.settings")
    }
}
