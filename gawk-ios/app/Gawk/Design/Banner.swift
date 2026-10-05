import SwiftUI

/// A notice at the top of the page (docs/70 §3.3, OD8): info in the accent,
/// a warning in amber. `title` leads in bold and `message` runs on after it;
/// the actions sit inside, small.
struct Banner<Actions: View>: View {
    enum Kind {
        case info
        case warning
    }

    let kind: Kind
    let systemImage: String
    let title: String
    var message: String?
    @ViewBuilder var actions: Actions

    var body: some View {
        HStack(alignment: .top, spacing: 12) {
            Image(systemName: systemImage)
                .font(.system(size: 17, weight: .semibold))
                .foregroundStyle(kind == .info ? Theme.accentText : Theme.warnText)
                .frame(width: 22)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 12) {
                Text("\(Text(title).fontWeight(.semibold).foregroundStyle(Theme.text))\(Text(message.map { " " + $0 } ?? "").foregroundStyle(Theme.muted))")
                    .font(.subheadline)
                    .fixedSize(horizontal: false, vertical: true)
                actions
            }
            Spacer(minLength: 0)
        }
        .padding(16)
        .background(kind == .info ? Theme.accentSoft : Theme.warnSoft, in: .rect(cornerRadius: 20, style: .continuous))
        .overlay(
            RoundedRectangle(cornerRadius: 20, style: .continuous)
                .strokeBorder(kind == .info ? Theme.accentLine : Theme.warnLine)
        )
        .accessibilityElement(children: .contain)
    }
}

extension Banner where Actions == EmptyView {
    init(kind: Kind, systemImage: String, title: String, message: String? = nil) {
        self.init(kind: kind, systemImage: systemImage, title: title, message: message) { EmptyView() }
    }
}

#Preview("Banners") {
    VStack(spacing: 16) {
        Banner(
            kind: .info, systemImage: "bell.slash",
            title: "Your whole screen is broadcast, with its sound.",
            message: "Notifications show too, so turn on a Focus to keep them off the stream."
        ) {
            Button("Got it") {}.buttonStyle(.gawkTintedCompact)
        }
        Banner(
            kind: .warning, systemImage: "wifi.slash",
            title: "Can't reach relay.example.net.",
            message: "You may be offline, or UDP to port 4433 may be blocked."
        ) {
            Button("Try again") {}.buttonStyle(.gawkGlass)
        }
    }
    .padding()
    .background(Theme.bg)
    .preferredColorScheme(.dark)
}
