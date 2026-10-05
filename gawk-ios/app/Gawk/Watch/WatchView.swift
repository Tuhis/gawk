import AVFoundation
import SwiftUI
import UIKit

/// The Watch screen (docs/67 D15, D20, D22): a code, the player, its status
/// and stats. Landscape with a player up is fullscreen.
struct WatchView: View {
    @State private var model = WatchModel()
    @Environment(AppSettings.self) private var settings
    @State private var showStats = false
    @Environment(\.scenePhase) private var scenePhase

    var body: some View {
        GeometryReader { geo in
            let landscape = geo.size.width > geo.size.height
            if let engine = model.engine, landscape {
                PlayerView(layer: engine.displayLayer)
                    .ignoresSafeArea()
                    .background(.black)
                    .statusBarHidden()
                    .toolbar(.hidden, for: .tabBar)
            } else {
                NavigationStack {
                    form
                        .navigationTitle("Watch")
                        .onAppear { syncServer() }
                }
            }
        }
        .onChange(of: scenePhase) { _, phase in
            // .inactive is a transition (the app switcher, a PiP start);
            // only .background means nobody sees the inline player.
            if phase == .background {
                model.setForeground(false)
            } else if phase == .active {
                model.setForeground(true)
            }
        }
        .sheet(isPresented: $showStats) {
            StatsSheet(stats: model.stats, counters: model.engine?.snapshot())
                .presentationDetents([.medium])
        }
    }

    private var form: some View {
        Form {
            if let engine = model.engine {
                Section {
                    PlayerView(layer: engine.displayLayer)
                        .aspectRatio(16 / 9, contentMode: .fit)
                        .background(.black)
                        .listRowInsets(EdgeInsets())
                    if let status = model.status {
                        Text(WatchStatusText.describe(status))
                            .font(.subheadline)
                            .foregroundStyle(statusColor(status))
                            .accessibilityIdentifier("watch.status")
                    }
                    if let codec = model.unsupportedCodec {
                        Text("This player can't play this stream's video format (\(codec)).")
                            .font(.subheadline)
                            .foregroundStyle(.red)
                    }
                } header: {
                    Text(model.watchingCode ?? "")
                } footer: {
                    HStack {
                        Button("Stats") { showStats = true }
                        Spacer()
                        if model.pip?.isSupported == true {
                            Button("Picture in Picture") { model.pip?.start() }
                        }
                        Spacer()
                        Button("Stop", role: .destructive) { model.stop() }
                    }
                    .buttonStyle(.borderless)
                    .padding(.top, 4)
                }
            }

            Section {
                CodeField(text: $model.code) { startWatching() }
                Button("Watch") { startWatching() }
                    .disabled(!model.canWatch)
                    .accessibilityIdentifier("watch.go")
            } header: {
                Text("Broadcast code")
            } footer: {
                Text("The six-character code the streamer shares.")
            }

            Section("Playback") {
                Picker("Latency", selection: $model.preset) {
                    Text("Balanced").tag(Preset.balanced)
                    Text("Lowest latency").tag(Preset.lowestLatency)
                }
                .pickerStyle(.segmented)
            }

            Section("Server") {
                LabeledContent("Relay", value: model.relayUrl)
                #if DEBUG
                TextField("Relay override (debug)", text: $model.relayOverride)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                    .keyboardType(.URL)
                    .accessibilityIdentifier("watch.relay")
                Toggle("Insecure (dev certs)", isOn: $model.insecure)
                    .accessibilityIdentifier("watch.insecure")
                #endif
            }
        }
    }

    /// The keyboard goes away so the player has the screen.
    private func startWatching() {
        syncServer()
        UIApplication.shared.sendAction(
            #selector(UIResponder.resignFirstResponder), to: nil, from: nil, for: nil)
        model.watch()
    }

    /// Watch dials the server picked in Settings, as Broadcast does; the
    /// debug override field only overrides it.
    private func syncServer() {
        model.selectedRelayUrl = settings.relayURL
        model.selectedInsecure = settings.insecure
    }

    private func statusColor(_ status: ViewerStatus) -> Color {
        switch status {
        case .live: .green
        case .ended: .red
        default: .secondary
        }
    }
}

/// The display layer, hosted in a view that keeps it sized (D15).
struct PlayerView: UIViewRepresentable {
    let layer: AVSampleBufferDisplayLayer

    func makeUIView(context: Context) -> LayerHostView {
        let view = LayerHostView()
        view.backgroundColor = .black
        view.host(layer)
        return view
    }

    func updateUIView(_ view: LayerHostView, context: Context) {
        view.host(layer)
    }

    /// The landscape and portrait layouts each have a PlayerView, and one
    /// layer can only have one superlayer. Whichever view is in a window
    /// holds it, and a view leaving its window lets go only if it still
    /// holds it, so a stale view torn down after a rotation can't take the
    /// layer back from the one on screen.
    final class LayerHostView: UIView {
        private var wanted: CALayer?

        func host(_ layer: CALayer) {
            guard wanted !== layer else { return }
            wanted = layer
            attach()
        }

        override func didMoveToWindow() {
            super.didMoveToWindow()
            if window == nil {
                if let wanted, wanted.superlayer === layer { wanted.removeFromSuperlayer() }
            } else {
                attach()
            }
        }

        private func attach() {
            guard window != nil, let wanted, wanted.superlayer !== layer else { return }
            layer.addSublayer(wanted)
            setNeedsLayout()
        }

        override func layoutSubviews() {
            super.layoutSubviews()
            guard let wanted, wanted.superlayer === layer else { return }
            CATransaction.begin()
            CATransaction.setDisableActions(true)
            wanted.frame = bounds
            CATransaction.commit()
        }
    }
}

/// The stats sheet: the core's numbers (G3, G8) and the renderer's.
struct StatsSheet: View {
    let stats: ViewerStats?
    let counters: PlayerEngine.Counters?

    var body: some View {
        NavigationStack {
            List {
                if let s = stats {
                    Section("Playout") {
                        row("Offset", ms(s.offsetMs))
                        row("Jitter", s.jitterMs.map(ms) ?? "—")
                        row("Round trip", s.rttMs.map(ms) ?? "—")
                        row("Viewers", s.viewerCount.map(String.init) ?? "—")
                    }
                    Section("Frames") {
                        row("Completed", "\(s.framesCompleted)")
                        row("Dropped", "\(s.framesDropped)")
                        row("Recovered by parity", "\(s.framesRecoveredByParity)")
                        row("Gap resyncs", "\(s.gapResyncs)")
                        row("Drops to live", "\(s.dropsToLive)")
                    }
                } else {
                    Text("No stats yet.")
                }
                if let c = counters {
                    Section("Renderer") {
                        row("Video samples", "\(c.videoEnqueued)")
                        row("Audio blocks", "\(c.audioEnqueued)")
                        row("Video dropped", "\(c.videoDropped)")
                        row("Renderer resyncs", "\(c.rendererResyncs)")
                    }
                }
            }
            .navigationTitle("Stats")
            .navigationBarTitleDisplayMode(.inline)
        }
    }

    private func row(_ label: String, _ value: String) -> some View {
        LabeledContent(label, value: value)
    }

    private func ms(_ v: Double) -> String { String(format: "%.0f ms", v) }
}
