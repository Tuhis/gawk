import SwiftUI
import UIKit

/// A 30 pt glass capsule with 13 pt text (docs/70 §3.3): LIVE, RECONNECTING,
/// the viewer and streaming counts, and the code pill.
struct Chip<Content: View>: View {
    var tint: Color?
    @ViewBuilder var content: Content

    var body: some View {
        HStack(spacing: 6) { content }
            .font(.footnote.weight(.semibold))
            .foregroundStyle(Theme.text)
            .padding(.horizontal, 11)
            .frame(height: 30)
            .glassEffect(tint.map { Glass.regular.tint($0) } ?? .regular, in: .capsule)
    }
}

/// LIVE, red; with `since`, the time live counting up after it. A red dot
/// and `liveText` on a `liveSoft` ground: white on `live` itself is 3.4:1,
/// under D27's 4.5 (docs/70 §10).
struct LiveChip: View {
    var since: Date?

    var body: some View {
        Chip(tint: Theme.liveSoft) {
            Circle().fill(Theme.live).frame(width: 7, height: 7)
            Text("LIVE").fontWeight(.bold).tracking(0.6)
            if let since {
                Text(since, style: .timer)
                    .monospacedDigit()
                    .foregroundStyle(Theme.text)
                    .accessibilityLabel(Text("live for \(Text(since, style: .relative))"))
            }
        }
        .foregroundStyle(Theme.liveText)
        .accessibilityElement(children: .combine)
    }
}

/// The amber stand-in for LIVE while a session reconnects (docs/70 D5,
/// D13), in the warning tokens for the same reason as LIVE's.
struct ReconnectingChip: View {
    var text = "RECONNECTING"

    var body: some View {
        Chip(tint: Theme.warnSoft) {
            ProgressView().controlSize(.mini).tint(Theme.warnText)
            Text(text).fontWeight(.bold).tracking(0.6)
        }
        .foregroundStyle(Theme.warnText)
        .accessibilityElement(children: .combine)
    }
}

/// An eye and a number: how many are watching.
struct ViewerCountChip: View {
    let count: UInt32
    var dimmed = false

    var body: some View {
        Chip {
            Image(systemName: "eye")
            Text("\(count)").monospacedDigit()
        }
        .opacity(dimmed ? 0.5 : 1)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("\(count) watching")
    }
}

/// The copied state every copy shares (docs/70 §3.4, OD4): a check in place
/// of the copy icon and the accent tint, for 1.5 s, with the copied text
/// never replacing the code.
struct CopyFeedback: Equatable {
    static let duration: Duration = .milliseconds(1500)

    private(set) var copiedAt: ContinuousClock.Instant?

    mutating func note(at now: ContinuousClock.Instant) {
        copiedAt = now
    }

    func isCopied(at now: ContinuousClock.Instant) -> Bool {
        guard let copiedAt else { return false }
        return now - copiedAt < Self.duration
    }

    /// The copy icon, or the check while copied.
    static func symbol(copied: Bool) -> String {
        copied ? "checkmark" : "doc.on.doc"
    }
}

/// Copies, announces and times the copied state for one control.
@MainActor
@Observable
final class Copier {
    private(set) var isCopied = false
    /// Bumped on every copy, for `.sensoryFeedback`.
    private(set) var count = 0
    @ObservationIgnored private var feedback = CopyFeedback()
    @ObservationIgnored private var reset: Task<Void, Never>?

    /// Puts `text` on the pasteboard and posts `announcement` ("Link
    /// copied") for VoiceOver.
    func copy(_ text: String, announcement: String) {
        UIPasteboard.general.string = text
        feedback.note(at: .now)
        isCopied = true
        count += 1
        AccessibilityNotification.Announcement(announcement).post()
        reset?.cancel()
        reset = Task { [weak self] in
            try? await Task.sleep(for: CopyFeedback.duration)
            guard let self, !Task.isCancelled else { return }
            self.isCopied = self.feedback.isCopied(at: .now)
        }
    }
}

/// The stream's code at the top right of the player (docs/70 D5a): the code
/// in mono and a copy icon. A tap copies the **link**, as the room's code
/// chip does (OD12); the code stays where it is (OD4).
struct CodePill: View {
    let code: String
    /// What a tap copies.
    let link: String
    var onCopy: () -> Void = {}
    @State private var copier = Copier()
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        Button {
            copier.copy(link, announcement: "Link copied")
            onCopy()
        } label: {
            HStack(spacing: 6) {
                Text(code)
                    .font(Theme.codePill)
                    .tracking(12 * 0.04)
                Image(systemName: CopyFeedback.symbol(copied: copier.isCopied))
                    .font(.system(size: 11, weight: .semibold))
                    .foregroundStyle(copier.isCopied ? Theme.accentText : Theme.muted)
                    .contentTransition(reduceMotion ? .identity : .symbolEffect(.replace))
            }
            .foregroundStyle(Theme.text)
            .padding(.horizontal, 11)
            .frame(height: 30)
            .glassEffect(copier.isCopied ? Glass.regular.tint(Theme.accentSoft) : .regular, in: .capsule)
            .overlay(Capsule().strokeBorder(copier.isCopied ? Theme.accentLine : .clear))
            .animation(reduceMotion ? nil : .spring(duration: 0.2), value: copier.isCopied)
        }
        .buttonStyle(.plain)
        .sensoryFeedback(.success, trigger: copier.count)
        .accessibilityLabel("Copy link to \(code)")
        .accessibilityIdentifier("player.codePill")
    }
}

#Preview("Chips") {
    VStack(alignment: .leading, spacing: 12) {
        HStack {
            LiveChip()
            ViewerCountChip(count: 12)
        }
        LiveChip(since: .now.addingTimeInterval(-754))
        ReconnectingChip()
        CodePill(code: "K7XQ2M", link: "https://gawk.ioio.fi/#/view/K7XQ2M")
        Chip { Text("3 streaming") }
    }
    .padding()
    .background(.black)
    .preferredColorScheme(.dark)
}
