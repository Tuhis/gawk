import AVFoundation
import Observation
import SwiftUI
import UIKit

/// When the glass controls over video show (docs/70 D5, §3.4): a tap shows
/// them, and they hide after `Theme.controlIdle` without a touch, the web's
/// `CONTROL_IDLE_MS`. Pinned, they stay: while a stream isn't playing, and
/// while a sheet is up.
@MainActor
@Observable
final class ControlsVisibility {
    private(set) var visible = true
    @ObservationIgnored var idle: Duration = ControlsVisibility.defaultIdle

    /// `Theme.controlIdle`; a debug build's UI tests may stretch it
    /// (`-gawkControlIdle <seconds>`) to work the controls without racing it.
    static var defaultIdle: Duration {
        #if DEBUG
        let args = ProcessInfo.processInfo.arguments
        if let i = args.firstIndex(of: "-gawkControlIdle"), i + 1 < args.count, let s = Double(args[i + 1]) {
            return .seconds(s)
        }
        #endif
        return Theme.controlIdle
    }
    @ObservationIgnored private var timer: Task<Void, Never>?

    var pinned = false {
        didSet {
            if pinned { visible = true }
            schedule()
        }
    }

    /// A tap on the video.
    func toggle() {
        if visible && !pinned {
            visible = false
            timer?.cancel()
        } else {
            show()
        }
    }

    func show() {
        visible = true
        schedule()
    }

    /// Any touch on a control keeps them up for another idle period.
    func touch() {
        if visible { schedule() }
    }

    private func schedule() {
        timer?.cancel()
        guard visible, !pinned else { return }
        let idle = idle
        timer = Task { [weak self] in
            try? await Task.sleep(for: idle)
            guard !Task.isCancelled, let self, !self.pinned else { return }
            self.visible = false
        }
    }
}

/// The glass controls' frame over a player (docs/70 D5, D19): four corners,
/// no bar behind any of them (OD2), faded together. Hidden controls are
/// transparent and let touches through, so an open menu keeps its anchor.
struct PlayerChrome<TopLeading: View, TopTrailing: View, Below: View, BottomLeading: View, BottomTrailing: View>: View {
    let controls: ControlsVisibility
    /// The bottom row steps aside for a drawer over it (Stats).
    var bottomHidden = false
    @ViewBuilder var topLeading: TopLeading
    @ViewBuilder var topTrailing: TopTrailing
    /// Under the top row: the server chip (K11).
    @ViewBuilder var below: Below
    @ViewBuilder var bottomLeading: BottomLeading
    @ViewBuilder var bottomTrailing: BottomTrailing
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        GlassEffectContainer(spacing: 8) {
            VStack(spacing: 8) {
                HStack(alignment: .top, spacing: 8) {
                    HStack(spacing: 8) { topLeading }
                    Spacer(minLength: 8)
                    HStack(spacing: 8) { topTrailing }
                }
                HStack {
                    below
                    Spacer()
                }
                Spacer()
                HStack(alignment: .bottom, spacing: 8) {
                    HStack(spacing: 8) { bottomLeading }
                    Spacer(minLength: 8)
                    HStack(spacing: 8) { bottomTrailing }
                }
                .opacity(bottomHidden ? 0 : 1)
                .allowsHitTesting(!bottomHidden)
            }
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 8)
        .opacity(controls.visible ? 1 : 0)
        .allowsHitTesting(controls.visible)
        .animation(reduceMotion ? nil : Theme.fade, value: controls.visible)
    }
}

/// Full screen (docs/70 D5): portrait turns the window to landscape, and
/// landscape back.
@MainActor
func toggleFullScreen(isLandscape: Bool) {
    guard let scene = UIApplication.shared.connectedScenes.compactMap({ $0 as? UIWindowScene }).first else {
        return
    }
    let target: UIInterfaceOrientationMask = isLandscape ? .portrait : .landscapeRight
    scene.requestGeometryUpdate(.iOS(interfaceOrientations: target)) { _ in }
}

/// The full-screen button's symbol and label for the current orientation.
struct FullScreenButton: View {
    let isLandscape: Bool

    var body: some View {
        GlassCircleButton(
            systemImage: isLandscape
                ? "arrow.down.right.and.arrow.up.left" : "arrow.up.left.and.arrow.down.right",
            label: isLandscape ? "Exit full screen" : "Full screen"
        ) {
            toggleFullScreen(isLandscape: isLandscape)
        }
        .accessibilityIdentifier("player.fullscreen")
    }
}

/// A link's server, pinned under the top row (docs/70 D4, K11).
struct ServerChip: View {
    let url: String

    var body: some View {
        Chip {
            Image(systemName: "server.rack")
            Text(hostOf(url)).font(Theme.link).lineLimit(1)
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("Server \(hostOf(url))")
        .accessibilityIdentifier("player.serverChip")
    }
}

/// The display layer, hosted in a view that keeps it sized (docs/67 D15).
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

    /// One layer can only have one superlayer, and a rotation or a layout
    /// change can build a new host before the old one goes. Whichever view
    /// is in a window holds it, and a view leaving its window lets go only
    /// if it still holds it, so a stale view torn down afterwards can't take
    /// the layer back from the one on screen.
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

/// The card over black for a stream that isn't there (docs/70 D9), centred,
/// in glass.
struct PlayerCard: View {
    let card: WatchStatusText.Card
    var close: () -> Void
    var retry: () -> Void

    var body: some View {
        VStack(spacing: 14) {
            Image(systemName: card.systemImage)
                .font(.system(size: 30, weight: .semibold))
                .foregroundStyle(Theme.muted)
                .accessibilityHidden(true)
            Text(card.title)
                .font(.title3.bold())
                .foregroundStyle(Theme.text)
            Text(card.body)
                .font(.subheadline)
                .foregroundStyle(Theme.muted)
                .multilineTextAlignment(.center)
            HStack(spacing: 12) {
                Button("Close", action: close)
                    .buttonStyle(.gawkGlass)
                    .accessibilityIdentifier("player.cardClose")
                if card.canRetry {
                    Button("Try again", action: retry)
                        .buttonStyle(.gawkPrimary)
                        .accessibilityIdentifier("player.retry")
                }
            }
            .padding(.top, 4)
        }
        .padding(24)
        .frame(maxWidth: 360)
        .glassEffect(.regular, in: .rect(cornerRadius: 28, style: .continuous))
        .padding(24)
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("player.card")
    }
}
