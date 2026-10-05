import SwiftUI
import UIKit

/// Stats, a drawer with two heights (docs/70 A5, A5b, D7, OD5). Pulled out
/// a little, four tiles sit under the video, which keeps playing and takes
/// touches above it; all the way, it adds every frame and renderer counter
/// and a Copy button. Nothing here is measured anew: it's the core's
/// `ViewerStats` and the renderer's counters.
struct StatsDrawer: View {
    let model: WatchModel
    @State private var detent: PresentationDetent = StatsDrawer.small
    @State private var copier = Copier()

    /// The small detent: the four tiles, under the video.
    static let smallHeight: CGFloat = 270
    static let small = PresentationDetent.height(smallHeight)

    var body: some View {
        TimelineView(.periodic(from: .now, by: 1)) { _ in
            let rows = StatsText.rows(model.stats, model.engine?.snapshot())
            ScrollView {
                VStack(alignment: .leading, spacing: 20) {
                    header(rows)
                    LazyVGrid(columns: [GridItem(.flexible(), spacing: 10), GridItem(.flexible(), spacing: 10)], spacing: 10) {
                        ForEach(rows.tiles, id: \.label) { tile($0) }
                    }
                    if detent == .large {
                        group("Frames", rows.frames)
                        group("Renderer", rows.renderer)
                    }
                }
                .padding(20)
            }
            .scrollDisabled(detent != .large)
        }
        .presentationDetents([StatsDrawer.small, .large], selection: $detent)
        .presentationBackgroundInteraction(.enabled(upThrough: StatsDrawer.small))
        .presentationDragIndicator(.visible)
        .accessibilityIdentifier("stats.drawer")
    }

    private func header(_ rows: StatsText.Rows) -> some View {
        HStack {
            Text("Stats")
                .font(.headline)
                .foregroundStyle(Theme.text)
            Spacer()
            if detent == .large {
                Button {
                    copier.copy(rows.plainText, announcement: "Stats copied")
                } label: {
                    Label("Copy", systemImage: CopyFeedback.symbol(copied: copier.isCopied))
                }
                .buttonStyle(.gawkTintedCompact)
                .accessibilityIdentifier("stats.copy")
            }
        }
    }

    private func tile(_ item: StatsText.Item) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(item.label)
                .font(.footnote)
                .foregroundStyle(Theme.muted)
            Text(item.value)
                .font(.title3.weight(.semibold).monospacedDigit())
                .foregroundStyle(Theme.text)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(14)
        .background(Theme.s2, in: .rect(cornerRadius: 14, style: .continuous))
        .accessibilityElement(children: .combine)
    }

    private func group(_ title: String, _ items: [StatsText.Item]) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            SectionHeader(title)
            VStack(spacing: 0) {
                ForEach(Array(items.enumerated()), id: \.offset) { i, item in
                    HStack {
                        Text(item.label).foregroundStyle(Theme.text)
                        Spacer()
                        Text(item.value).monospacedDigit().foregroundStyle(Theme.muted)
                    }
                    .font(.subheadline)
                    .padding(.horizontal, 14)
                    .padding(.vertical, 11)
                    .accessibilityElement(children: .combine)
                    if i < items.count - 1 {
                        Divider().overlay(Theme.border).padding(.leading, 14)
                    }
                }
            }
            .background(Theme.s2, in: .rect(cornerRadius: 14, style: .continuous))
        }
    }
}

/// The drawer's words and numbers, formatted before display; "—" is
/// unknown (docs/70 D7).
enum StatsText {
    struct Item: Equatable {
        let label: String
        let value: String
    }

    struct Rows: Equatable {
        let tiles: [Item]
        let frames: [Item]
        let renderer: [Item]

        /// What Copy puts on the pasteboard: every number, one per line.
        var plainText: String {
            (tiles + frames + renderer).map { "\($0.label): \($0.value)" }.joined(separator: "\n")
        }
    }

    static let unknown = "—"

    static func ms(_ v: Double?) -> String {
        v.map { String(format: "%.0f ms", $0) } ?? unknown
    }

    static func count<N: BinaryInteger>(_ v: N?) -> String {
        v.map { $0.formatted() } ?? unknown
    }

    static func rows(_ s: ViewerStats?, _ c: PlayerEngine.Counters?) -> Rows {
        Rows(
            tiles: [
                Item(label: "Playout delay", value: ms(s?.offsetMs)),
                Item(label: "Jitter", value: ms(s?.jitterMs)),
                Item(label: "Round trip", value: ms(s?.rttMs)),
                Item(label: "Watching", value: count(s?.viewerCount)),
            ],
            frames: [
                Item(label: "Completed", value: count(s?.framesCompleted)),
                Item(label: "Dropped", value: count(s?.framesDropped)),
                Item(label: "Recovered by parity", value: count(s?.framesRecoveredByParity)),
                Item(label: "Gap resyncs", value: count(s?.gapResyncs)),
                Item(label: "Drops to live", value: count(s?.dropsToLive)),
            ],
            renderer: [
                Item(label: "Video samples", value: count(c?.videoEnqueued)),
                Item(label: "Audio blocks", value: count(c?.audioEnqueued)),
                Item(label: "Video dropped", value: count(c?.videoDropped)),
                Item(label: "Renderer resyncs", value: count(c?.rendererResyncs)),
            ]
        )
    }
}
