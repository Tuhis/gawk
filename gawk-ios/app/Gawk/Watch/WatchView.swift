import SwiftUI
import UIKit

/// The Watch tab (docs/70 A1, D3): the web landing's join card, then the
/// rooms you go back to. A code is joined as the web's `#/join` does (D4).
struct WatchView: View {
    @Environment(AppSettings.self) private var settings
    @Environment(AppRouter.self) private var router
    @State private var code = ""
    @State private var codeFocused = false

    var body: some View {
        NavigationStack {
            List {
                Section {
                    VStack(spacing: 16) {
                        if let notice = router.linkNotice {
                            Banner(kind: .info, systemImage: "link", title: notice) {
                                Button("Dismiss") { router.linkNotice = nil }
                                    .buttonStyle(.gawkTintedCompact)
                            }
                            .accessibilityIdentifier("link.notice")
                        }
                        ServerNotices()
                        joinCard
                    }
                    .readableWidth()
                    .listRowBackground(Color.clear)
                    .listRowInsets(EdgeInsets(top: 4, leading: 16, bottom: 8, trailing: 16))
                }
                rooms
            }
            .gawkList(background: false)
            .background(alignment: .top) {
                ZStack(alignment: .top) {
                    Theme.bg
                    Glow()
                }
                .ignoresSafeArea()
            }
            .navigationTitle("Watch")
            .scrollDismissesKeyboard(.interactively)
        }
    }

    private var joinCard: some View {
        VStack(alignment: .leading, spacing: 18) {
            VStack(alignment: .leading, spacing: 6) {
                Text("Join a stream")
                    .font(.title2.bold())
                    .foregroundStyle(Theme.text)
                    .fixedSize(horizontal: false, vertical: true)
                Text("Type the code you were sent. Room codes work here too.")
                    .font(.subheadline)
                    .foregroundStyle(Theme.muted)
                    .fixedSize(horizontal: false, vertical: true)
            }
            CodeBoxesInput(code: $code, isFocused: $codeFocused) { join() }
                .frame(maxWidth: .infinity)
            HStack(spacing: 12) {
                PasteButton(payloadType: String.self) { strings in
                    guard let first = strings.first else { return }
                    code = BroadcastCode.sanitize(first)
                }
                .buttonBorderShape(.capsule)
                .labelStyle(.titleAndIcon)
                .tint(Theme.s3)
                .accessibilityIdentifier("watch.paste")
                Button {
                    join()
                } label: {
                    if router.resolving != nil {
                        HStack(spacing: 8) {
                            ProgressView().tint(Theme.bg)
                            Text("Looking up…")
                        }
                    } else {
                        Text("Join")
                    }
                }
                .buttonStyle(.gawkPrimary)
                .disabled(!BroadcastCode.isValid(code) || router.resolving != nil)
                .accessibilityIdentifier("watch.join")
            }
        }
        .padding(20)
        // Tinted toward s1: the glow behind mustn't lift the glass so far
        // that muted text on it drops under 4.5:1 (D27).
        // Glass with a solid s1 ground under the text: on bare glass over
        // the glow, muted text fell under 4.5:1 (D27, docs/70 §10).
        .background(Theme.s1.opacity(0.92), in: .rect(cornerRadius: 28, style: .continuous))
        .glassEffect(.regular, in: .rect(cornerRadius: 28, style: .continuous))
    }

    @ViewBuilder private var rooms: some View {
        let list = settings.yourRooms
        if !list.isEmpty {
            Section {
                ForEach(list) { record in
                    Button {
                        router.open(.room(code: record.code, relay: nil, nick: nil))
                    } label: {
                        ListRow(
                            systemImage: record.saved ? "star.fill" : "square.grid.2x2",
                            title: record.title,
                            subtitle: record.name.isEmpty ? nil : record.code
                        ) {
                            Image(systemName: "chevron.right")
                                .font(.footnote.weight(.semibold))
                                .foregroundStyle(Theme.faint)
                        }
                    }
                    .accessibilityLabel(record.saved ? "\(record.title), saved" : record.title)
                    .swipeActions(edge: .leading) {
                        Button(record.saved ? "Unsave" : "Save", systemImage: record.saved ? "star.slash" : "star") {
                            settings.setSaved(record, !record.saved)
                        }
                        .tint(Theme.accent)
                    }
                    .swipeActions(edge: .trailing) {
                        Button("Remove", systemImage: "trash", role: .destructive) {
                            settings.removeRoom(record)
                        }
                    }
                    .gawkRow()
                }
            } header: {
                SectionHeader("Your rooms")
            } footer: {
                SectionFooter("A gawk link opens here and starts playing.")
            }
        } else {
            Section {
            } footer: {
                SectionFooter("A gawk link opens here and starts playing.")
            }
        }
    }

    /// The keyboard goes away so the player has the screen. At once, not on
    /// the next update: an alert presented meanwhile (D21) would bring it
    /// back when dismissed.
    private func join() {
        guard BroadcastCode.isValid(code) else { return }
        codeFocused = false
        UIApplication.shared.sendAction(#selector(UIResponder.resignFirstResponder), to: nil, from: nil, for: nil)
        router.join(code: code, relay: settings.relayURL, insecure: settings.insecure)
    }
}

/// The web landing's blurred accent glow, behind the join card.
private struct Glow: View {
    var body: some View {
        Ellipse()
            .fill(Theme.accent.opacity(0.18))
            .frame(width: 340, height: 220)
            .blur(radius: 80)
            .offset(y: 60)
            .allowsHitTesting(false)
            .accessibilityHidden(true)
    }
}
