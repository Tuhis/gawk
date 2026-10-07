import SwiftUI

/// The room player (docs/70 C1, C2, C1b, C2b; D19): the single-stream
/// player with more tiles, laid out like the web room screen (OD9). Black,
/// tiles edge to edge with 2 pt gaps, and the same glass controls that hide
/// after 3 s.
struct RoomPlayerScreen: View {
    let code: String
    let linkRelay: String?
    let linkNick: String?
    @Environment(AppSettings.self) private var settings
    @Environment(AppRouter.self) private var router
    @Environment(\.horizontalSizeClass) private var sizeClass
    @State private var model: RoomPlayerModel?
    @State private var controls = ControlsVisibility()
    @State private var showPeople = false

    var body: some View {
        GeometryReader { geo in
            let landscape = geo.size.width > geo.size.height
            ZStack {
                Color.black.ignoresSafeArea()
                if let model {
                    content(model, landscape: landscape)
                    chrome(model, landscape: landscape)
                    if let reason = model.endedReason {
                        PlayerCard(
                            card: .init(systemImage: "square.grid.2x2", title: "Room closed", body: reason, canRetry: true),
                            close: close, retry: model.start)
                    } else if model.room == nil {
                        VStack(spacing: 14) {
                            ProgressView().controlSize(.large).tint(Theme.text)
                            Text("Joining \(code)…").font(.subheadline).foregroundStyle(Theme.muted)
                        }
                        .accessibilityElement(children: .combine)
                    }
                }
            }
        }
        .statusBarHidden(!controls.visible)
        .persistentSystemOverlays(controls.visible ? .automatic : .hidden)
        .onAppear(perform: start)
        .onDisappear { model?.stop() }
        .onChange(of: model?.room == nil || model?.endedReason != nil) { _, waiting in
            controls.pinned = waiting || showPeople
        }
        .onChange(of: showPeople) { _, open in
            controls.pinned = open || model?.room == nil
        }
        .sheet(isPresented: $showPeople) {
            if let model { PeopleSheet(model: model) }
        }
    }

    private func start() {
        guard model == nil else { return }
        let relay = linkRelay ?? settings.relayURL
        let m = RoomPlayerModel(
            code: code, relayUrl: relay, insecure: settings.insecure,
            nickname: linkNick ?? settings.nickname)
        // Your rooms (K12): the room moves to the front once, then keeps
        // the name the relay gives it.
        m.onRoom = { [settings] room, first in
            if first {
                settings.noteRoom(room.code, name: room.displayName, server: relay)
            } else {
                settings.nameRoom(room.code, name: room.displayName, server: relay)
            }
        }
        model = m
        controls.pinned = true
        m.start()
    }

    private func close() {
        model?.stop()
        router.screen = nil
    }

    // MARK: Layout (D19, D26)

    /// Grid columns: portrait is one column up to four streams and two
    /// above; landscape 2 × 2 up to four and three above. An iPad adds one
    /// above four (D26).
    static func columns(count: Int, landscape: Bool, wide: Bool) -> Int {
        if count <= 1 { return 1 }
        if count <= 4 { return landscape ? 2 : (wide ? 2 : 1) }
        let base = landscape ? 3 : 2
        return wide ? base + 1 : base
    }

    /// The largest 16:9 tile that lets `count` tiles in `columns` fit
    /// `space` whole. The grid never scrolls: when the rows are too tall
    /// for the screen the tiles shrink, and the black around them is the
    /// letterbox.
    static func tileSize(count: Int, columns: Int, in space: CGSize, spacing: CGFloat = 2) -> CGSize {
        let cols = max(columns, 1)
        let rows = max((count + cols - 1) / cols, 1)
        let byWidth = (space.width - CGFloat(cols - 1) * spacing) / CGFloat(cols)
        let byHeight = (space.height - CGFloat(rows - 1) * spacing) / CGFloat(rows) * 16 / 9
        let width = max(min(byWidth, byHeight), 0)
        return CGSize(width: width, height: width * 9 / 16)
    }

    @ViewBuilder private func content(_ model: RoomPlayerModel, landscape: Bool) -> some View {
        switch model.layout {
        case .grid: grid(model, landscape: landscape)
        case .focus: focus(model, landscape: landscape)
        }
    }

    private func grid(_ model: RoomPlayerModel, landscape: Bool) -> some View {
        let tiles = model.tiles
        let n = Self.columns(count: tiles.count, landscape: landscape, wide: sizeClass == .regular)
        return VStack(spacing: 8) {
            RoomGridLayout(columns: n, spacing: 2) {
                ForEach(tiles, id: \.broadcastId) { tile in
                    RoomTileView(tile: tile, model: model, controls: controls)
                }
            }
            if tiles.count > RoomPlayerModel.maxPlaying {
                Text("Four play at once. Tap a paused stream to play it instead.")
                    .font(.footnote)
                    .foregroundStyle(Theme.muted)
                    .multilineTextAlignment(.center)
                    .padding(.horizontal, 24)
                    .fixedSize(horizontal: false, vertical: true)
                    .accessibilityIdentifier("room.hint")
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .ignoresSafeArea(edges: landscape ? .all : [])
        .contentShape(.rect)
        .onTapGesture { controls.toggle() }
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("room.grid")
    }

    private func focus(_ model: RoomPlayerModel, landscape: Bool) -> some View {
        VStack(spacing: 2) {
            Spacer(minLength: 0)
            if let tile = model.focused {
                RoomTileView(tile: tile, model: model, controls: controls, large: true)
                    .aspectRatio(16 / 9, contentMode: .fit)
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            }
            Spacer(minLength: 0)
            let others = model.tiles.filter { $0.broadcastId != model.focused?.broadcastId }
            if !others.isEmpty {
                ScrollView(.horizontal) {
                    HStack(spacing: 2) {
                        ForEach(others, id: \.broadcastId) { tile in
                            RoomTileView(tile: tile, model: model, controls: controls)
                                .aspectRatio(16 / 9, contentMode: .fit)
                                .frame(height: landscape ? 72 : 96)
                        }
                    }
                }
                .scrollIndicators(.hidden)
                .padding(.bottom, landscape ? 60 : 64)
                .accessibilityIdentifier("room.strip")
            }
        }
        .contentShape(.rect)
        .onTapGesture { controls.toggle() }
    }

    // MARK: Controls

    private func chrome(_ model: RoomPlayerModel, landscape: Bool) -> some View {
        PlayerChrome(controls: controls) {
            GlassCircleButton(systemImage: "xmark", label: "Leave the room", action: close)
                .accessibilityIdentifier("room.close")
            if model.reconnectAttempt != nil {
                ReconnectingChip()
            } else if model.room != nil {
                Chip { Text("\(model.streamingCount) streaming").monospacedDigit() }
                    .accessibilityElement(children: .combine)
                    .accessibilityIdentifier("room.streaming")
            }
        } topTrailing: {
            if let room = model.room {
                CodePill(code: room.code, link: roomLink(code: room.code, grant: nil), onCopy: controls.touch)
                    .accessibilityLabel("Copy room link")
                GlassCircleButton(systemImage: "person.2", label: "People") { showPeople = true }
                    .accessibilityIdentifier("room.people")
            }
        } below: {
            if let chip = AppRouter.chip(for: linkRelay) { ServerChip(url: chip) }
        } bottomLeading: {
            if model.room != nil {
                LayoutToggle(layout: Binding(get: { model.layout }, set: { model.layout = $0; controls.touch() }))
            }
        } bottomTrailing: {
            PlayerSettingsMenu(
                preset: Binding(get: { model.preset }, set: { model.preset = $0; controls.touch() }),
                shareURL: roomLink(code: model.room?.code ?? code, grant: nil))
            FullScreenButton(isLandscape: landscape)
        }
    }
}

/// The room grid (D19): rows of 16:9 tiles, sized by
/// `RoomPlayerScreen.tileSize` to fit the space it's offered and centred in
/// it, so it never needs to scroll. A short last row keeps to the left, as
/// a grid's does.
struct RoomGridLayout: Layout {
    let columns: Int
    let spacing: CGFloat

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let space = proposal.replacingUnspecifiedDimensions(by: CGSize(width: 393, height: 852))
        return gridSize(count: subviews.count, in: space)
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        let tile = RoomPlayerScreen.tileSize(count: subviews.count, columns: columns, in: bounds.size, spacing: spacing)
        let grid = gridSize(count: subviews.count, in: bounds.size)
        let origin = CGPoint(x: bounds.midX - grid.width / 2, y: bounds.midY - grid.height / 2)
        let cols = max(columns, 1)
        for (i, subview) in subviews.enumerated() {
            let x = origin.x + CGFloat(i % cols) * (tile.width + spacing)
            let y = origin.y + CGFloat(i / cols) * (tile.height + spacing)
            subview.place(at: CGPoint(x: x, y: y), proposal: ProposedViewSize(tile))
        }
    }

    private func gridSize(count: Int, in space: CGSize) -> CGSize {
        guard count > 0 else { return .zero }
        let tile = RoomPlayerScreen.tileSize(count: count, columns: columns, in: space, spacing: spacing)
        let cols = min(max(columns, 1), count)
        let rows = (count + max(columns, 1) - 1) / max(columns, 1)
        return CGSize(
            width: CGFloat(cols) * tile.width + CGFloat(cols - 1) * spacing,
            height: CGFloat(rows) * tile.height + CGFloat(rows - 1) * spacing)
    }
}

/// Grid / Focus, a glass segmented control with icon and label (docs/70
/// §3.3, OD9).
struct LayoutToggle: View {
    @Binding var layout: RoomPlayerModel.Layout

    var body: some View {
        HStack(spacing: 2) {
            segment(.grid, symbol: "square.grid.2x2")
            segment(.focus, symbol: "rectangle.split.3x1")
        }
        .padding(3)
        .glassEffect(.regular, in: .capsule)
        .accessibilityElement(children: .contain)
        .accessibilityLabel("Layout")
    }

    private func segment(_ value: RoomPlayerModel.Layout, symbol: String) -> some View {
        let on = layout == value
        return Button {
            layout = value
        } label: {
            Label(value.rawValue, systemImage: symbol)
                .font(.footnote.weight(.semibold))
                .foregroundStyle(on ? Theme.text : Theme.muted)
                .padding(.horizontal, 12)
                .frame(height: 32)
                .background(on ? Theme.text.opacity(0.14) : .clear, in: .capsule)
                .contentShape(.capsule)
        }
        .buttonStyle(.plain)
        .accessibilityAddTraits(on ? .isSelected : [])
        .accessibilityIdentifier("room.layout.\(value.rawValue.lowercased())")
    }
}

/// One stream in the room (D19): its video, its name chip, and the away
/// card or the play badge over it.
struct RoomTileView: View {
    let tile: RoomTile
    let model: RoomPlayerModel
    let controls: ControlsVisibility
    var large = false

    /// The tile's label, or its code when the broadcaster gave no name.
    private var name: String { tile.label.isEmpty ? tile.broadcastId : tile.label }

    var body: some View {
        let playing = model.isPlaying(tile.broadcastId)
        ZStack(alignment: .topLeading) {
            Color(white: 0.06)
            if let engine = model.engines[tile.broadcastId] {
                PlayerView(layer: engine.displayLayer)
            }
            if !tile.live {
                VStack(spacing: 4) {
                    Text("\(name) is away")
                        .font(large ? .headline : .footnote.weight(.semibold))
                        .foregroundStyle(Theme.text)
                    Text("Their stream comes back here on its own.")
                        .font(large ? .subheadline : .caption2)
                        .foregroundStyle(Theme.muted)
                        .multilineTextAlignment(.center)
                }
                .padding(8)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .background(.black.opacity(0.55))
            } else if !playing, model.layout == .grid {
                // Grid only: in Focus a tap focuses, it doesn't play.
                Image(systemName: "play.fill")
                    .font(.system(size: large ? 22 : 15, weight: .semibold))
                    .foregroundStyle(Theme.text)
                    .frame(width: large ? 52 : 38, height: large ? 52 : 38)
                    .glassEffect(.regular, in: .circle)
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                    .accessibilityHidden(true)
            }
            nameChip
                .padding(6)
        }
        .clipped()
        .contentShape(.rect)
        // A tap that swaps or focuses a stream shows the controls with it;
        // one on a stream already playing or in focus is a tap on the
        // video, and shows or hides them as on a single stream.
        .onTapGesture {
            if model.tap(tile.broadcastId) {
                controls.show()
            } else {
                controls.toggle()
            }
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(accessibilityText(playing: playing))
        .accessibilityAddTraits(.isButton)
        .accessibilityIdentifier("room.tile.\(tile.broadcastId)")
    }

    private var nameChip: some View {
        HStack(spacing: 5) {
            Circle()
                .fill(tile.live ? Theme.live : Theme.warn)
                .frame(width: 7, height: 7)
            Text(name)
                .lineLimit(1)
        }
        .font(.caption.weight(.semibold))
        .foregroundStyle(Theme.text)
        .padding(.horizontal, 8)
        .frame(height: 24)
        .glassEffect(.regular, in: .capsule)
    }

    private func accessibilityText(playing: Bool) -> String {
        let name = self.name
        if !tile.live { return "\(name), away" }
        if playing { return "\(name), playing" }
        return model.layout == .grid ? "\(name), paused. Tap to play" : "\(name). Tap to focus"
    }
}
