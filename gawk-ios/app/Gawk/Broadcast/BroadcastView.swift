import SwiftUI
import UIKit

/// The Broadcast tab (docs/70 B1–B4, D10–D14): one button, then one code.
struct BroadcastView: View {
    @Environment(AppSettings.self) private var settings
    @Environment(BroadcastSession.self) private var session
    @Environment(Capture.self) private var capture
    @Environment(AppRouter.self) private var router
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var showRoomPicker = false
    @State private var showRoomManager = false
    @State private var roomKey = ""
    @State private var linkPrompt: LinkPrompt?

    var body: some View {
        NavigationStack {
            Group {
                if session.isActive {
                    LiveView(showRoomPicker: $showRoomPicker, showRoomManager: $showRoomManager)
                } else {
                    ready
                }
            }
            .navigationTitle("Broadcast")
            .background(Theme.bg)
        }
        .sheet(isPresented: $showRoomPicker) { RoomPickerSheet() }
        .sheet(isPresented: $showRoomManager) { RoomManagerSheet() }
        .alert("This room needs a key to add your stream", isPresented: needsKey) {
            TextField("Room key", text: $roomKey)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
            Button("Join") {
                session.joinWithKey(roomKey)
                roomKey = ""
            }
            Button("Not now", role: .cancel) { session.leaveRoom() }
        }
        .alert("Couldn't go live", isPresented: refused, presenting: session.refusal) { refusal in
            if refusal.status == 401 {
                Button("Edit secret") {
                    router.editServer(AppRouter.route(forRelay: settings.relayURL))
                }
                Button("Cancel", role: .cancel) {}
            } else {
                Button("OK", role: .cancel) {}
            }
        } message: { refusal in
            Text(refusal.reason)
        }
        .alert(linkPrompt?.title ?? "", isPresented: linkPromptShown, presenting: linkPrompt) { prompt in
            Button(prompt.accept) { apply(prompt) }
            Button("Not now", role: .cancel) {}
        } message: { prompt in
            Text(prompt.body)
        }
        .onChange(of: router.broadcastPrefill, initial: true) { _, prefill in
            guard let prefill else { return }
            router.broadcastPrefill = nil
            take(prefill)
        }
    }

    // MARK: Ready (B1, B4)

    private var ready: some View {
        List {
            Section {
                VStack(spacing: 12) {
                    if let notice = router.linkNotice {
                        Banner(kind: .info, systemImage: "link", title: notice) {
                            Button("Dismiss") { router.linkNotice = nil }.buttonStyle(.gawkTintedCompact)
                        }
                        .accessibilityIdentifier("link.notice")
                    }
                    ServerNotices()
                    if let summary = session.summary {
                        SummaryCard(summary: summary) {
                            session.dismissSummary()
                        } newCode: {
                            session.useNewCodeNextTime(relay: settings.relayURL)
                        }
                    } else if case .ended(let reason?) = session.phase {
                        Banner(kind: .warning, systemImage: "exclamationmark.triangle", title: reason)
                    }
                    if let failure = session.failure {
                        Banner(kind: .warning, systemImage: "exclamationmark.triangle", title: failure)
                    }
                    if !settings.sawCaptureNote {
                        // D10, revising docs/67 D18's copy.
                        Banner(
                            kind: .info, systemImage: "bell.slash",
                            title: "Your whole screen is broadcast, with its sound.",
                            message: "Notifications show too, so turn on a Focus to keep them off the stream."
                        ) {
                            Button("Got it") { settings.sawCaptureNote = true }
                                .buttonStyle(.gawkTintedCompact)
                                .accessibilityIdentifier("broadcast.gotIt")
                        }
                    }
                }
                .readableWidth()
                .listRowBackground(Color.clear)
                .listRowInsets(EdgeInsets(top: 0, leading: 16, bottom: 0, trailing: 16))
            }
            Section {
                QualityRow()
            } header: {
                SectionHeader("Quality")
            } footer: {
                SectionFooter(settings.quality.line)
            }
            Section {
                RoomRow(showRoomPicker: $showRoomPicker, showRoomManager: $showRoomManager)
            } header: {
                SectionHeader("Room")
            }
        }
        .gawkList()
        .safeAreaInset(edge: .bottom) { goLive }
    }

    /// Pinned above the tab bar (D10, D14).
    private var goLive: some View {
        let again = session.summary != nil && session.lastCode != nil
        return VStack(spacing: 8) {
            Button {
                settings.sawCaptureNote = true
                capture.start(session: session, settings: settings)
            } label: {
                HStack(spacing: 10) {
                    Circle().fill(Theme.live).frame(width: 10, height: 10)
                    Text(again ? "Go live again" : "Go live")
                }
            }
            .buttonStyle(.gawkPrimaryPinned)
            .disabled(!capture.screenAvailable && !capture.usesTestSource)
            .accessibilityIdentifier("broadcast.goLive")
            Text(again ? "Viewers keep code \(session.lastCode ?? "") for a few minutes." : "You'll get a code to send to friends.")
                .font(.footnote)
                .foregroundStyle(Theme.muted)
        }
        .readableWidth()
        .padding(.horizontal, 16)
        .padding(.vertical, 10)
        .background { PinnedGround() }
    }

    private var needsKey: Binding<Bool> {
        Binding(get: { session.roomNeedsKey }, set: { _ in })
    }

    private var refused: Binding<Bool> {
        Binding(get: { session.refusal != nil }, set: { if !$0 { session.clearRefusal() } })
    }

    private var linkPromptShown: Binding<Bool> {
        Binding(get: { linkPrompt != nil }, set: { if !$0 { linkPrompt = nil } })
    }

    // MARK: A gawk://broadcast link (docs/70 D4, docs/68 D4–D5)

    /// What a link asks that needs a click: a server not yet saved, or a
    /// room or nickname while live (docs/68 G4, G5).
    private enum LinkPrompt: Equatable {
        case addServer(String)
        case joinLive(room: String?, nick: String?)

        var title: String {
            switch self {
            case .addServer(let url): "This link uses the server \(hostOf(url))"
            case .joinLive(let room?, _): "Add your stream to \(room) now?"
            case .joinLive(nil, let nick): "Use the nickname \(nick ?? "") now?"
            }
        }

        var body: String {
            switch self {
            case .addServer: "Add it and switch to it? It's saved with no publish secret; you're asked for one if it needs it."
            case .joinLive(.some, _): "The link names a room. Your broadcast joins it now."
            case .joinLive(nil, _): "The link renames your stream."
            }
        }

        var accept: String {
            switch self {
            case .addServer: "Add and switch"
            case .joinLive(.some, _): "Join"
            case .joinLive(nil, _): "Use it"
            }
        }
    }

    /// Fills Broadcast in from a link and never starts anything.
    private func take(_ prefill: AppRouter.BroadcastPrefill) {
        if session.isActive {
            if prefill.room != nil || prefill.nick != nil {
                linkPrompt = .joinLive(room: prefill.room, nick: prefill.nick)
            }
            return
        }
        if let relay = prefill.relay {
            if settings.servers.contains(where: { $0.url == relay }) {
                settings.selectedURL = relay
            } else {
                linkPrompt = .addServer(relay)
            }
        } else {
            settings.selectedURL = ""
        }
        if let nick = prefill.nick { settings.nickname = nick }
        if let room = prefill.room { choose(room: room) }
    }

    private func apply(_ prompt: LinkPrompt) {
        switch prompt {
        case .addServer(let url):
            settings.addServer(name: hostOf(url), url: url)
            settings.selectedURL = url
        case .joinLive(let room, let nick):
            if let nick {
                settings.nickname = nick
                session.setNickname(nick)
            }
            if let room { choose(room: room) }
        }
    }

    private func choose(room: String) {
        let relay = settings.relayURL
        let key = capture.identity.roomCredential(.attachKey, relay: relay, code: room)
        let token = capture.identity.roomCredential(.creatorToken, relay: relay, code: room)
        session.chooseRoom(.join(code: room, attachKey: key, creatorToken: token))
        settings.noteRoom(room, server: relay)
    }
}

/// The Quality row (D10–D12): the choice on Ready, what's encoded on Live,
/// with the menu either way. A change while live restarts the publish leg
/// on the same code.
struct QualityRow: View {
    @Environment(AppSettings.self) private var settings
    @Environment(BroadcastSession.self) private var session
    @Environment(Capture.self) private var capture

    var body: some View {
        @Bindable var settings = settings
        Menu {
            Picker("Quality", selection: $settings.quality) {
                ForEach(QualityChoice.allCases, id: \.self) { choice in
                    Text(choice.title).tag(choice)
                }
            }
            .pickerStyle(.inline)
        } label: {
            if session.isActive, let live = session.counters.flatMap(BroadcastText.liveQuality) {
                ListRow(systemImage: "sparkles.tv", title: live.title, subtitle: live.detail) {
                    menuValue
                }
            } else {
                ListRow(systemImage: "sparkles.tv", title: "Quality") { menuValue }
            }
        }
        .accessibilityIdentifier("broadcast.quality")
        .gawkRow()
        .onChange(of: settings.quality) { _, choice in
            if session.isActive { session.setQuality(capture.resolve(choice)) }
        }
    }

    private var menuValue: some View {
        HStack(spacing: 4) {
            Text(settings.quality.title)
            Image(systemName: "chevron.up.chevron.down").font(.footnote)
        }
        .foregroundStyle(Theme.muted)
    }
}

/// The Room row on Ready and Live (D10, D14, D17): Add, a pending room, or
/// the room card.
struct RoomRow: View {
    @Environment(BroadcastSession.self) private var session
    @Binding var showRoomPicker: Bool
    @Binding var showRoomManager: Bool

    var body: some View {
        if let card = session.roomCard {
            VStack(alignment: .leading, spacing: 8) {
                Text(card.title).font(.headline).foregroundStyle(Theme.text)
                Text(card.body).font(.subheadline).foregroundStyle(Theme.muted)
                Button("Dismiss") { session.roomCard = nil }
                    .buttonStyle(.gawkTintedCompact)
            }
            .padding(.vertical, 6)
            .gawkRow()
        }
        if let room = session.room {
            RoomCardRow(room: room, manage: { showRoomManager = true })
        } else if let pending = session.pendingRoom {
            HStack {
                ListRow(
                    systemImage: "square.grid.2x2",
                    title: pending.code ?? "A new room",
                    subtitle: session.isActive ? (session.roomStatus ?? "Joining…") : "Joins when you go live")
                Button {
                    session.clearPendingRoom()
                } label: {
                    Image(systemName: "xmark.circle.fill")
                        .foregroundStyle(Theme.muted)
                        .frame(width: 44, height: 44)
                        .contentShape(.rect)
                }
                .buttonStyle(.borderless)
                .accessibilityLabel("Don't join")
            }
            .gawkRow()
        } else {
            Button {
                showRoomPicker = true
            } label: {
                ListRow(systemImage: "square.grid.2x2", title: "Room", subtitle: session.roomStatus) {
                    HStack(spacing: 2) {
                        Text("Add")
                        Image(systemName: "chevron.right").font(.footnote.weight(.semibold))
                    }
                    .foregroundStyle(Theme.accentText)
                }
            }
            .accessibilityIdentifier("broadcast.room")
            .gawkRow()
        }
    }
}

/// Live in a room (C4, D17): its name, who's in it, Manage, Copy room link
/// and Leave.
struct RoomCardRow: View {
    @Environment(BroadcastSession.self) private var session
    let room: RoomView
    var manage: () -> Void
    @State private var copier = Copier()

    var body: some View {
        Button(action: manage) {
            HStack(spacing: 12) {
                RowIcon(systemImage: "square.grid.2x2", action: true)
                VStack(alignment: .leading, spacing: 2) {
                    Text(room.displayName.isEmpty ? room.code : room.displayName)
                        .foregroundStyle(Theme.text)
                    Text(RoomText.counts(room))
                        .font(.footnote)
                        .foregroundStyle(Theme.muted)
                }
                Spacer(minLength: 8)
                Avatars(people: Array(room.people.prefix(3)))
                HStack(spacing: 2) {
                    Text("Manage")
                    Image(systemName: "chevron.right").font(.footnote.weight(.semibold))
                }
                .foregroundStyle(Theme.accentText)
            }
        }
        .accessibilityIdentifier("broadcast.manageRoom")
        .gawkRow()
        HStack(spacing: 10) {
            Button {
                copier.copy(roomLink(code: room.code, grant: nil), announcement: "Room link copied")
            } label: {
                Label("Copy room link", systemImage: CopyFeedback.symbol(copied: copier.isCopied))
            }
            .buttonStyle(.gawkTinted)
            .sensoryFeedback(.success, trigger: copier.count)
            Button("Leave") { session.leaveRoom() }
                .buttonStyle(.gawkGhost)
                .accessibilityIdentifier("broadcast.leaveRoom")
        }
        .listRowBackground(Color.clear)
        .listRowInsets(EdgeInsets(top: 4, leading: 0, bottom: 4, trailing: 0))
    }
}

/// Up to three people as initials.
struct Avatars: View {
    let people: [RoomPerson]

    var body: some View {
        HStack(spacing: -8) {
            ForEach(people, id: \.id) { p in
                Text(RoomText.initial(p.nickname))
                    .font(.caption2.weight(.bold))
                    .foregroundStyle(Theme.accentText)
                    .frame(width: 24, height: 24)
                    .background(Theme.s3, in: .circle)
                    .overlay(Circle().strokeBorder(Theme.s1, lineWidth: 2))
            }
        }
        .accessibilityHidden(true)
    }
}

/// A room's words.
enum RoomText {
    /// "3 streaming · 3 watching" (D17), away streams counted as the web
    /// counts them.
    static func counts(_ room: RoomView) -> String {
        let watching = room.people.filter { !$0.streaming }.count
        return "\(room.tiles.count) streaming · \(watching) watching"
    }

    static func initial(_ name: String) -> String {
        name.first.map { String($0).uppercased() } ?? "?"
    }
}

/// After End (B4, D14): the summary as an info banner at the top.
struct SummaryCard: View {
    let summary: BroadcastSummary
    var dismiss: () -> Void
    var newCode: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Broadcast ended")
                .font(.headline)
                .foregroundStyle(Theme.text)
            HStack(alignment: .top, spacing: 10) {
                tile(BroadcastText.duration(summary.timeLive), "time live")
                tile("\(summary.mostWatching)", "most watching")
                tile(summary.averageUploadBps.map(BroadcastText.rate) ?? "—", "average upload")
            }
            HStack(spacing: 10) {
                Button("Dismiss", action: dismiss)
                    .buttonStyle(.gawkTintedCompact)
                Button("Use a new code next time", action: newCode)
                    .buttonStyle(.gawkGhostAccent)
                    .accessibilityIdentifier("broadcast.newCode")
            }
        }
        .padding(16)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Theme.accentSoft, in: .rect(cornerRadius: 20, style: .continuous))
        .overlay(RoundedRectangle(cornerRadius: 20, style: .continuous).strokeBorder(Theme.accentLine))
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("broadcast.summary")
    }

    private func tile(_ value: String, _ label: String) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(value)
                .font(.headline.monospacedDigit())
                .foregroundStyle(Theme.text)
            Text(label)
                .font(.caption)
                .foregroundStyle(Theme.muted)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .accessibilityElement(children: .combine)
    }
}
