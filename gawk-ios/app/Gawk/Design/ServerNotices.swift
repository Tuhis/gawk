import SwiftUI

/// docs/40's persistent strip (docs/70 D24): which non-default server the
/// page uses, with a way to change it.
struct ServerStrip: View {
    let name: String
    let url: String
    var change: (() -> Void)?

    var body: some View {
        HStack(spacing: 12) {
            RowIcon(systemImage: "server.rack")
            VStack(alignment: .leading, spacing: 2) {
                Text("Using \(name)")
                    .font(.subheadline.weight(.semibold))
                    .foregroundStyle(Theme.text)
                Text(hostOf(url))
                    .font(Theme.link)
                    .foregroundStyle(Theme.muted)
                    .lineLimit(1)
                    .truncationMode(.middle)
            }
            Spacer(minLength: 8)
            if let change {
                Button(action: change) {
                    HStack(spacing: 2) {
                        Text("Change")
                        Image(systemName: "chevron.right").font(.footnote.weight(.semibold))
                    }
                    .font(.subheadline.weight(.semibold))
                    .foregroundStyle(Theme.accentText)
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Change server")
            }
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
        .background(Theme.s1, in: .rect(cornerRadius: 16, style: .continuous))
        .overlay(RoundedRectangle(cornerRadius: 16, style: .continuous).strokeBorder(Theme.borderSoft))
        .accessibilityElement(children: .combine)
    }
}

/// The selected server's notices for Watch and Broadcast (docs/70 D24,
/// OD8): the unreachable banner at the top, then the strip when the server
/// isn't the default fleet. A failed probe never disables anything: it can
/// be wrong about the path a publish or a watch would take (docs/64 D2).
struct ServerNotices: View {
    @Environment(AppSettings.self) private var settings
    @Environment(ServerProbes.self) private var probes
    @Environment(AppRouter.self) private var router

    var body: some View {
        let url = settings.relayURL
        VStack(spacing: 12) {
            if probes.state(url) == .unreachable {
                Banner(
                    kind: .warning, systemImage: "wifi.slash",
                    title: "Can't reach \(hostOf(url)).",
                    message: "You may be offline, or UDP to port \(Self.port(url)) may be blocked."
                ) {
                    Button("Try again") { probes.refresh(url, insecure: settings.insecure) }
                        .buttonStyle(.gawkGlass)
                }
                .accessibilityIdentifier("server.unreachable")
            }
            if !settings.isDefaultRelay {
                ServerStrip(name: settings.relayName, url: url) { router.tab = .settings }
                    .accessibilityIdentifier("server.strip")
            }
        }
        .task(id: url) { probes.refresh(url, insecure: settings.insecure) }
    }

    static func port(_ url: String) -> Int {
        URLComponents(string: url)?.port ?? 443
    }
}

#Preview("Server strip") {
    ServerStrip(name: "Home relay", url: "https://relay.example.net:4433") {}
        .padding()
        .background(Theme.bg)
        .preferredColorScheme(.dark)
}
