import SwiftUI

// The app's buttons (docs/70 §3.3, after docs/64 D4): one primary per
// screen, glass for everything secondary, tinted for a gentle action, and a
// danger outline for ending something others depend on.

/// The near-white capsule: the next step, one per screen. 52 pt tall, 56
/// pinned above the tab bar.
struct PrimaryButtonStyle: ButtonStyle {
    var pinned = false
    @Environment(\.isEnabled) private var isEnabled

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.headline)
            .foregroundStyle(Theme.bg)
            .frame(maxWidth: .infinity, minHeight: pinned ? 56 : 52)
            .padding(.horizontal, 20)
            .background(Theme.text, in: .capsule)
            .opacity(isEnabled ? (configuration.isPressed ? 0.85 : 1) : 0.4)
            .contentShape(.capsule)
    }
}

/// `accentSoft` fill, `accentLine` border, `accentText` label.
struct TintedButtonStyle: ButtonStyle {
    var compact = false

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(compact ? .subheadline.weight(.semibold) : .headline)
            .foregroundStyle(Theme.accentText)
            .frame(maxWidth: compact ? nil : .infinity, minHeight: compact ? 34 : 48)
            .padding(.horizontal, compact ? 14 : 20)
            .background(Theme.accentSoft, in: .capsule)
            .overlay(Capsule().strokeBorder(Theme.accentLine))
            .opacity(configuration.isPressed ? 0.75 : 1)
            .contentShape(.capsule)
    }
}

/// Ends something others depend on, and is never the loudest thing there.
struct DangerOutlineButtonStyle: ButtonStyle {
    var pinned = false

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.headline)
            .foregroundStyle(Theme.dangerText)
            .frame(maxWidth: .infinity, minHeight: pinned ? 56 : 48)
            .padding(.horizontal, 20)
            .overlay(Capsule().strokeBorder(Theme.dangerLine, lineWidth: 1.5))
            .opacity(configuration.isPressed ? 0.7 : 1)
            .contentShape(.capsule)
    }
}

/// A secondary action as a Liquid Glass capsule.
struct GlassCapsuleButtonStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.subheadline.weight(.semibold))
            .foregroundStyle(Theme.text)
            .frame(minHeight: 44)
            .padding(.horizontal, 16)
            // A solid s3 ground under the label keeps it legible on any
            // backdrop (D27); the glass is its rim. Plain glass, not
            // `.interactive()`: interactive glass takes the touch itself and
            // the button's action never runs.
            .background(Theme.s3, in: .capsule)
            .glassEffect(.regular, in: .capsule)
            .scaleEffect(configuration.isPressed ? 0.96 : 1)
            .opacity(configuration.isPressed ? 0.8 : 1)
            .contentShape(.capsule)
    }
}

/// A quiet action with no ground: Leave beside Copy room link (docs/70
/// D17), "Use a new code next time" in the summary.
struct GhostButtonStyle: ButtonStyle {
    var color = Theme.text

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.subheadline.weight(.semibold))
            .foregroundStyle(color)
            .frame(minHeight: 44)
            .padding(.horizontal, 12)
            .opacity(configuration.isPressed ? 0.6 : 1)
            .contentShape(.rect)
    }
}

extension ButtonStyle where Self == GhostButtonStyle {
    static var gawkGhost: GhostButtonStyle { GhostButtonStyle() }
    static var gawkGhostAccent: GhostButtonStyle { GhostButtonStyle(color: Theme.accentText) }
}

extension ButtonStyle where Self == PrimaryButtonStyle {
    static var gawkPrimary: PrimaryButtonStyle { PrimaryButtonStyle() }
    static var gawkPrimaryPinned: PrimaryButtonStyle { PrimaryButtonStyle(pinned: true) }
}

extension ButtonStyle where Self == TintedButtonStyle {
    static var gawkTinted: TintedButtonStyle { TintedButtonStyle() }
    static var gawkTintedCompact: TintedButtonStyle { TintedButtonStyle(compact: true) }
}

extension ButtonStyle where Self == DangerOutlineButtonStyle {
    static var gawkDanger: DangerOutlineButtonStyle { DangerOutlineButtonStyle() }
    static var gawkDangerPinned: DangerOutlineButtonStyle { DangerOutlineButtonStyle(pinned: true) }
}

extension ButtonStyle where Self == GlassCapsuleButtonStyle {
    static var gawkGlass: GlassCapsuleButtonStyle { GlassCapsuleButtonStyle() }
}

/// A 44 pt glass circle with one symbol: X, Picture in Picture, settings,
/// full screen. Icon-only, so the label is required (docs/70 D27).
struct GlassCircleButton: View {
    let systemImage: String
    let label: String
    var action: () -> Void

    var body: some View {
        Button(action: action) {
            GlassCircleLabel(systemImage: systemImage)
        }
        .buttonStyle(GlassPressStyle())
        .accessibilityLabel(label)
    }
}

/// A glass label's press feedback, which plain glass doesn't give.
private struct GlassPressStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .scaleEffect(configuration.isPressed ? 0.94 : 1)
            .opacity(configuration.isPressed ? 0.8 : 1)
    }
}

/// The circle itself, for a `Menu` or `ShareLink` label.
struct GlassCircleLabel: View {
    let systemImage: String

    var body: some View {
        Image(systemName: systemImage)
            .font(.system(size: 17, weight: .semibold))
            .foregroundStyle(Theme.text)
            .frame(width: 44, height: 44)
            // Not `.interactive()`: see GlassCapsuleButtonStyle.
            .glassEffect(.regular, in: .circle)
            .contentShape(.circle)
    }
}

/// The ground under a button pinned above the tab bar: solid `bg`, with a
/// short fade above it so the list scrolls away under it, and nothing shows
/// through an outline button.
struct PinnedGround: View {
    var body: some View {
        Theme.bg
            .overlay(alignment: .top) {
                LinearGradient(colors: [Theme.bg.opacity(0), Theme.bg], startPoint: .top, endPoint: .bottom)
                    .frame(height: 18)
                    .offset(y: -18)
            }
            .ignoresSafeArea(edges: .bottom)
    }
}

#Preview("Buttons") {
    VStack(spacing: 14) {
        Button("Join") {}.buttonStyle(.gawkPrimary)
        Button("Join") {}.buttonStyle(.gawkPrimary).disabled(true)
        Button {} label: {
            Label("Go live", systemImage: "circle.fill")
        }
        .buttonStyle(.gawkPrimaryPinned)
        Button("Got it") {}.buttonStyle(.gawkTintedCompact)
        Button("Copy room link") {}.buttonStyle(.gawkTinted)
        Button("End broadcast") {}.buttonStyle(.gawkDanger)
        HStack {
            Button("Paste") {}.buttonStyle(.gawkGlass)
            GlassCircleButton(systemImage: "xmark", label: "Close") {}
            GlassCircleButton(systemImage: "pip.enter", label: "Picture in Picture") {}
        }
    }
    .padding()
    .background(Theme.bg)
    .preferredColorScheme(.dark)
}
