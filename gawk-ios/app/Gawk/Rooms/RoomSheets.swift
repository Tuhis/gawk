import SwiftUI

/// Add to a room (docs/70 C3, D16), the desktop's sheet (docs/60 D8): a
/// code or a link, a new room, or one of Your rooms. A pasted link's `?rt=`
/// grant is kept, bound to this server (docs/68 D5a).
struct RoomPickerSheet: View {
    @Environment(AppSettings.self) private var settings
    @Environment(BroadcastSession.self) private var session
    @Environment(Capture.self) private var capture
    @Environment(\.dismiss) private var dismiss
    @State private var input = ""
    @State private var invalid = false

    var body: some View {
        NavigationStack {
            List {
                Section {
                    HStack(spacing: 10) {
                        // Wraps rather than clips at large text sizes (D27);
                        // Return still joins.
                        TextField("Room code or link", text: $input, axis: .vertical)
                            .lineLimit(1...4)
                            .textInputAutocapitalization(.never)
                            .autocorrectionDisabled()
                            .submitLabel(.join)
                            .onChange(of: input) { _, value in
                                guard value.contains("\n") else { return }
                                input = value.replacingOccurrences(of: "\n", with: "")
                                join()
                            }
                            .foregroundStyle(Theme.text)
                            .accessibilityIdentifier("room.input")
                        PasteButton(payloadType: String.self) { strings in
                            input = strings.first?.trimmingCharacters(in: .whitespacesAndNewlines) ?? input
                        }
                        .labelStyle(.iconOnly)
                        .buttonBorderShape(.circle)
                        .tint(Theme.s3)
                    }
                    .frame(minHeight: 38)
                    .gawkRow()
                    if !input.isEmpty {
                        Button("Join \(parseRoomInput(input: input)?.code ?? input)", action: join)
                            .foregroundStyle(Theme.accentText)
                            .accessibilityIdentifier("room.join")
                            .gawkRow()
                    }
                } footer: {
                    if invalid {
                        Text("That isn't a room code or a room link.").font(.footnote).foregroundStyle(Theme.warnText)
                    }
                }
                Section {
                    Button {
                        session.chooseRoom(.create)
                        dismiss()
                    } label: {
                        ListRow(systemImage: "plus", title: "Create a new room", subtitle: "You get a code for friends to join", action: true)
                    }
                    .accessibilityIdentifier("room.create")
                    .gawkRow()
                }
                let rooms = settings.yourRooms
                if !rooms.isEmpty {
                    Section {
                        ForEach(rooms) { record in
                            Button {
                                choose(code: record.code, grant: nil)
                            } label: {
                                ListRow(
                                    systemImage: record.saved ? "star.fill" : "square.grid.2x2",
                                    title: record.title,
                                    subtitle: record.name.isEmpty ? nil : record.code)
                            }
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
                        if !session.isActive { SectionFooter("Your stream joins the room when you go live.") }
                    }
                } else if !session.isActive {
                    Section {} footer: { SectionFooter("Your stream joins the room when you go live.") }
                }
            }
            .gawkList()
            .navigationTitle("Add to a room")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Close", systemImage: "xmark") { dismiss() }
                }
            }
        }
        .presentationDetents([.medium, .large])
        .presentationSizing(.form)
    }

    private func join() {
        guard let parsed = parseRoomInput(input: input) else {
            invalid = true
            return
        }
        choose(code: parsed.code, grant: parsed.grant)
    }

    /// The room becomes the pending one (joined now if live). A link's
    /// grant wins over a key on file, and is stored for this server only.
    private func choose(code: String, grant: RoomGrant?) {
        let relay = settings.relayURL
        let identity = capture.identity
        switch grant {
        case .creator(let token): identity.setRoomCredential(token, .creatorToken, relay: relay, code: code)
        case .attach(let key): identity.setRoomCredential(key, .attachKey, relay: relay, code: code)
        case nil: break
        }
        session.chooseRoom(.join(code: code))
        settings.noteRoom(code, server: relay)
        dismiss()
    }
}

/// The room sheet (docs/70 C5, D18): who's streaming and watching, the
/// room link, and leaving. Its creator can remove a stream and end the
/// room. There is no "Watch the room": the phone broadcasts its whole
/// screen, so a room player opened while live would send the room, a
/// mirror of this stream included, to everyone watching.
struct RoomManagerSheet: View {
    @Environment(BroadcastSession.self) private var session
    @Environment(\.dismiss) private var dismiss
    @State private var copier = Copier()

    var body: some View {
        NavigationStack {
            if let room = session.room {
                List {
                    Section {
                        VStack(alignment: .leading, spacing: 12) {
                            Text(room.creator ? "Room \(room.code) · You made this room" : "Room \(room.code)")
                                .font(.subheadline)
                                .foregroundStyle(Theme.muted)
                            Button {
                                copier.copy(roomLink(code: room.code, grant: nil), announcement: "Room link copied")
                            } label: {
                                Label("Copy room link", systemImage: CopyFeedback.symbol(copied: copier.isCopied))
                            }
                            .buttonStyle(.gawkTinted)
                            .sensoryFeedback(.success, trigger: copier.count)
                        }
                        .listRowBackground(Color.clear)
                    }
                    Section {
                        ForEach(room.tiles, id: \.broadcastId) { tile in
                            HStack(spacing: 12) {
                                Circle().fill(tile.live ? Theme.live : Theme.warn).frame(width: 8, height: 8)
                                VStack(alignment: .leading, spacing: 2) {
                                    Text(tile.label.isEmpty ? tile.broadcastId : tile.label).foregroundStyle(Theme.text)
                                    Text(tile.live ? "Live" : "Away").font(.footnote).foregroundStyle(Theme.muted)
                                }
                                Spacer(minLength: 8)
                                Label("\(tile.viewerCount)", systemImage: "eye")
                                    .font(.footnote.monospacedDigit())
                                    .foregroundStyle(Theme.muted)
                                    .accessibilityLabel("\(tile.viewerCount) watching")
                                if room.creator, tile.broadcastId != session.liveCode {
                                    Button("Remove") { session.removeFromRoom(tile.broadcastId) }
                                        .buttonStyle(.borderless)
                                        .foregroundStyle(Theme.dangerText)
                                        .accessibilityIdentifier("room.remove.\(tile.broadcastId)")
                                }
                            }
                            .gawkRow()
                        }
                    } header: {
                        SectionHeader("Streaming")
                    }
                    let watching = room.people.filter { !$0.streaming }
                    if !watching.isEmpty {
                        Section {
                            ForEach(watching, id: \.id) { p in
                                HStack(spacing: 12) {
                                    Avatars(people: [p])
                                    Text(p.id == room.yourId ? "\(p.nickname) (you)" : p.nickname).foregroundStyle(Theme.text)
                                }
                                .gawkRow()
                            }
                        } header: {
                            SectionHeader("Watching")
                        }
                    }
                    Section {
                        VStack(spacing: 10) {
                            Button("Leave room") {
                                session.leaveRoom()
                                dismiss()
                            }
                            .buttonStyle(.gawkGlass)
                            .frame(maxWidth: .infinity)
                            .accessibilityIdentifier("room.leave")
                            if room.creator {
                                Button("End room for everyone") {
                                    session.endRoom()
                                    dismiss()
                                }
                                .buttonStyle(.gawkDanger)
                                .accessibilityIdentifier("room.end")
                            }
                        }
                        .listRowBackground(Color.clear)
                    }
                }
                .gawkList()
                .navigationTitle(room.displayName.isEmpty ? room.code : room.displayName)
                .navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) {
                        Button("Close", systemImage: "xmark") { dismiss() }
                    }
                }
            } else {
                ContentUnavailableView("Not in a room", systemImage: "square.grid.2x2")
                    .onAppear { dismiss() }
            }
        }
        .presentationDetents([.medium, .large])
        .presentationSizing(.form)
    }
}
