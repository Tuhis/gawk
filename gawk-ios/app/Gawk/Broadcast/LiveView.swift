import SwiftUI
import UIKit

/// Live (docs/70 B2, B3; D11, D13): the code first, then the stream rows,
/// then the room, with End pinned.
struct LiveView: View {
    @Environment(BroadcastSession.self) private var session
    @Environment(Capture.self) private var capture
    @Binding var showRoomPicker: Bool
    @Binding var showRoomManager: Bool
    @State private var copier = Copier()
    @State private var linkCopier = Copier()

    var body: some View {
        List {
            Section {
                VStack(alignment: .leading, spacing: 18) {
                    if case .resuming = session.phase {
                        Banner(
                            kind: .warning, systemImage: "wifi.exclamationmark",
                            title: "Connection lost.",
                            message: "Your viewers keep the code while gawk reconnects.")
                            .accessibilityIdentifier("broadcast.reconnecting")
                    }
                    status
                    code
                }
                .readableWidth()
                .listRowBackground(Color.clear)
                .listRowInsets(EdgeInsets(top: 0, leading: 16, bottom: 8, trailing: 16))
            }
            Section {
                QualityRow()
                upload
            } header: {
                SectionHeader("Stream")
            }
            Section {
                RoomRow(showRoomPicker: $showRoomPicker, showRoomManager: $showRoomManager)
            } header: {
                SectionHeader("Room")
            }
            #if DEBUG
            Section {
                TimelineView(.periodic(from: .now, by: 1)) { _ in
                    Text(CaptureDiagnostics.shared.summary)
                        .font(.caption.monospaced())
                        .foregroundStyle(Theme.muted)
                        .textSelection(.enabled)
                }
                .gawkRow()
            } header: {
                SectionHeader("Capture (debug)")
            }
            #endif
        }
        .gawkList()
        .safeAreaInset(edge: .bottom) { end }
    }

    /// LIVE with its clock and the viewer count; amber while reconnecting.
    @ViewBuilder private var status: some View {
        HStack(spacing: 8) {
            switch session.phase {
            case .live:
                LiveChip(since: session.liveSince).accessibilityIdentifier("broadcast.live")
            case .resuming:
                ReconnectingChip()
            case .stopping:
                Chip { ProgressView().controlSize(.mini).tint(Theme.text); Text("Ending…") }
            default:
                Chip { ProgressView().controlSize(.mini).tint(Theme.text); Text("Going live…") }
                    .accessibilityIdentifier("broadcast.connecting")
            }
            if session.liveSince != nil {
                ViewerCountChip(count: session.viewerCount, dimmed: session.phase.isResuming)
            }
        }
    }

    /// "Your code", the boxes, the link, Copy link and Share (D11).
    @ViewBuilder private var code: some View {
        if let code = session.liveCode, session.liveSince != nil {
            let link = watchLink(broadcastId: code)
            VStack(alignment: .leading, spacing: 14) {
                SectionHeader("Your code")
                Button {
                    copier.copy(code, announcement: "Code copied")
                } label: {
                    CodeBoxesDisplay(code: code, copied: copier.isCopied)
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.plain)
                .sensoryFeedback(.success, trigger: copier.count)
                .contextMenu {
                    Button("Copy code", systemImage: "doc.on.doc") { copier.copy(code, announcement: "Code copied") }
                    Button("Copy link", systemImage: "link") { linkCopier.copy(link, announcement: "Link copied") }
                    if let url = URL(string: link) { ShareLink("Share", item: url) }
                }
                .accessibilityElement(children: .ignore)
                .accessibilityLabel("Code \(code), copy")
                .accessibilityValue(copier.isCopied ? "Copied" : "")
                .accessibilityAddTraits(.isButton)
                .accessibilityIdentifier("broadcast.code")
                Text(link)
                    .font(Theme.link)
                    .foregroundStyle(Theme.muted)
                    .lineLimit(1)
                    .truncationMode(.middle)
                    .frame(maxWidth: .infinity)
                    .textSelection(.enabled)
                    .accessibilityLabel("Join link")
                    .accessibilityValue(link)
                HStack(spacing: 10) {
                    Button {
                        linkCopier.copy(link, announcement: "Link copied")
                    } label: {
                        Label("Copy link", systemImage: CopyFeedback.symbol(copied: linkCopier.isCopied))
                    }
                    .buttonStyle(.gawkPrimary)
                    .sensoryFeedback(.success, trigger: linkCopier.count)
                    .accessibilityIdentifier("broadcast.copyLink")
                    if let url = URL(string: link) {
                        ShareLink(item: url) { GlassCircleLabel(systemImage: "square.and.arrow.up") }
                            .accessibilityLabel("Share")
                    }
                }
                Text("Switch to your game. gawk keeps broadcasting in the background.")
                    .font(.footnote)
                    .foregroundStyle(Theme.muted)
            }
        } else {
            HStack(spacing: 12) {
                ProgressView().tint(Theme.muted)
                Text(session.phase == .stopping ? "Ending the broadcast…" : "Getting your code…")
                    .font(.subheadline)
                    .foregroundStyle(Theme.muted)
            }
            .frame(maxWidth: .infinity, minHeight: 120)
        }
    }

    /// The send rate with Steady or the warning (D11), or the reconnect
    /// attempt (D13).
    private var upload: some View {
        let c = session.counters
        // ok: nil while measuring, so no dot claims anything yet.
        let (value, line, ok): (String, String, Bool?) = {
            if case .resuming(let attempt) = session.phase {
                return ("—", "Reconnecting · attempt \(attempt)", false)
            }
            guard let c, c.uploadAvailable else { return ("—", "Measuring…", nil) }
            return (BroadcastText.rate(c.uploadBps), c.uplinkWarning ? "Upload is struggling" : "Upload steady", !c.uplinkWarning)
        }()
        return ListRow(systemImage: "arrow.up", title: value) {
            HStack(spacing: 6) {
                if let ok {
                    Circle().fill(ok ? Theme.ok : Theme.warn).frame(width: 7, height: 7)
                }
                Text(line)
                    .font(.footnote)
                    .foregroundStyle(ok == false ? Theme.warnText : Theme.muted)
            }
        }
        .accessibilityElement(children: .combine)
        .accessibilityIdentifier("broadcast.upload")
        .gawkRow()
    }

    /// End (D11): a danger outline, no confirmation; Go live again undoes it.
    private var end: some View {
        Button("End broadcast") { capture.stop(session) }
            .buttonStyle(.gawkDangerPinned)
            .disabled(session.phase == .stopping)
            .accessibilityIdentifier("broadcast.end")
            .readableWidth()
            .padding(.horizontal, 16)
            .padding(.vertical, 10)
            .background { PinnedGround() }
    }
}
