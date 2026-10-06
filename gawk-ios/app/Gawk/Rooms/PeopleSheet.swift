import SwiftUI

/// People (docs/70 C2c, D20): the web room's people panel as a sheet
/// (`RoomPanel.tsx`, without its chat placeholder). A tap on a stream
/// focuses it; your own row's Edit renames you in the room and in Settings.
/// This is where the web's "Watching as …" bar went (OD9).
struct PeopleSheet: View {
    let model: RoomPlayerModel
    @Environment(AppSettings.self) private var settings
    @Environment(\.dismiss) private var dismiss
    @State private var editing = false
    @State private var newName = ""

    /// The wire's `MaxRoomNicknameLen`, in UTF-8 bytes.
    static let maxNicknameBytes = 32

    var body: some View {
        NavigationStack {
            List {
                if let room = model.room {
                    Section {
                        ForEach(room.tiles, id: \.broadcastId) { tile in
                            Button {
                                model.focus(tile.broadcastId)
                                dismiss()
                            } label: {
                                HStack(spacing: 12) {
                                    Circle().fill(tile.live ? Theme.live : Theme.warn).frame(width: 8, height: 8)
                                    Text(tile.label.isEmpty ? tile.broadcastId : tile.label)
                                        .foregroundStyle(Theme.text)
                                    Spacer(minLength: 8)
                                    Label("\(tile.viewerCount)", systemImage: "eye")
                                        .font(.footnote.monospacedDigit())
                                        .foregroundStyle(Theme.muted)
                                        .accessibilityLabel("\(tile.viewerCount) watching")
                                }
                            }
                            .accessibilityIdentifier("people.stream.\(tile.broadcastId)")
                            .gawkRow()
                        }
                    } header: {
                        SectionHeader("Streaming")
                    }
                    Section {
                        // You first, where Edit is in reach; then the
                        // relay's order.
                        ForEach(room.people.filter { $0.id == room.yourId } + room.people.filter { $0.id != room.yourId }, id: \.id) { p in
                            HStack(spacing: 12) {
                                HStack(spacing: 12) {
                                    Avatars(people: [p])
                                    VStack(alignment: .leading, spacing: 2) {
                                        Text(p.id == room.yourId ? "\(p.nickname) (you)" : p.nickname)
                                            .foregroundStyle(Theme.text)
                                        Text(p.streaming ? "Streaming" : "Watching")
                                            .font(.footnote)
                                            .foregroundStyle(Theme.muted)
                                    }
                                }
                                .accessibilityElement(children: .combine)
                                Spacer(minLength: 8)
                                if p.id == room.yourId {
                                    Button("Edit") {
                                        newName = p.nickname
                                        editing = true
                                    }
                                    .buttonStyle(.borderless)
                                    .foregroundStyle(Theme.accentText)
                                    .accessibilityLabel("Edit your nickname")
                                    .accessibilityIdentifier("people.edit")
                                }
                            }
                            .gawkRow()
                        }
                    } header: {
                        SectionHeader("People")
                    }
                }
            }
            .gawkList()
            .navigationTitle(model.room.map { $0.displayName.isEmpty ? $0.code : $0.displayName } ?? model.code)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .principal) {
                    VStack(spacing: 0) {
                        Text(model.room.map { $0.displayName.isEmpty ? $0.code : $0.displayName } ?? model.code)
                            .font(.headline)
                        Text("Room \(model.room?.code ?? model.code)")
                            .font(.caption)
                            .foregroundStyle(Theme.muted)
                    }
                    .accessibilityElement(children: .combine)
                }
                ToolbarItem(placement: .cancellationAction) {
                    Button("Close", systemImage: "xmark") { dismiss() }
                }
            }
            .alert("Your nickname", isPresented: $editing) {
                TextField("Nickname", text: $newName)
                    .textInputAutocapitalization(.words)
                    .accessibilityIdentifier("people.nickname")
                Button("Save") { rename() }
                Button("Cancel", role: .cancel) {}
            } message: {
                Text("Shown to others in the room.")
            }
        }
        .presentationDetents([.medium, .large])
        .presentationSizing(.form)
    }

    /// The room hears it at once (SetNickname, 0x03), and it becomes
    /// Settings' nickname.
    private func rename() {
        let name = Self.clamp(newName.trimmingCharacters(in: .whitespacesAndNewlines))
        guard !name.isEmpty else { return }
        model.setNickname(name)
        settings.nickname = name
    }

    /// At most `maxNicknameBytes` of UTF-8, cut on a character boundary.
    static func clamp(_ name: String) -> String {
        var out = ""
        for ch in name {
            if out.utf8.count + String(ch).utf8.count > maxNicknameBytes { break }
            out.append(ch)
        }
        return out
    }
}
