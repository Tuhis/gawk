import SwiftUI

/// A list section's header: footnote semibold, upper case, tracked 0.12 em,
/// in `muted` (docs/70 §3.2), the desktop's kicker where iOS puts headers.
struct SectionHeader: View {
    let title: String

    init(_ title: String) { self.title = title }

    var body: some View {
        Text(title)
            .font(.footnote.weight(.semibold))
            .tracking(13 * 0.12)
            .textCase(.uppercase)
            .foregroundStyle(Theme.muted)
            .accessibilityAddTraits(.isHeader)
    }
}

/// A list footer in `muted`.
struct SectionFooter: View {
    let text: String

    init(_ text: String) { self.text = text }

    var body: some View {
        Text(text)
            .font(.footnote)
            .foregroundStyle(Theme.muted)
    }
}

/// A row's 30 pt icon tile: `s3` with a `muted` glyph, or the accent for an
/// action row.
struct RowIcon: View {
    let systemImage: String
    var action = false

    var body: some View {
        Image(systemName: systemImage)
            .font(.system(size: 14, weight: .semibold))
            .foregroundStyle(action ? Theme.accentText : Theme.muted)
            .frame(width: 30, height: 30)
            .background(action ? Theme.accentSoft : Theme.s3, in: .rect(cornerRadius: 8, style: .continuous))
            .accessibilityHidden(true)
    }
}

/// One row of a grouped list: an icon tile, a title with an optional line
/// under it, and a trailing value. 54 pt tall at the default size, taller as
/// the text grows.
struct ListRow<Trailing: View>: View {
    let systemImage: String
    let title: String
    var subtitle: String?
    var action = false
    @ViewBuilder var trailing: Trailing

    var body: some View {
        HStack(spacing: 12) {
            RowIcon(systemImage: systemImage, action: action)
            VStack(alignment: .leading, spacing: 2) {
                Text(title)
                    .font(.body)
                    .foregroundStyle(action ? Theme.accentText : Theme.text)
                if let subtitle {
                    Text(subtitle)
                        .font(.footnote)
                        .foregroundStyle(Theme.muted)
                }
            }
            Spacer(minLength: 8)
            trailing
                .font(.body)
                .foregroundStyle(Theme.muted)
        }
        .frame(minHeight: 54 - 16)
    }
}

extension ListRow where Trailing == EmptyView {
    init(systemImage: String, title: String, subtitle: String? = nil, action: Bool = false) {
        self.init(systemImage: systemImage, title: title, subtitle: subtitle, action: action) {
            EmptyView()
        }
    }
}

extension View {
    /// A system grouped list on the gawk surfaces: `bg` behind (unless the
    /// page draws its own), `s1` rows and `borderSoft` hairlines (docs/70
    /// §3.3), at most 640 pt wide and centred (D26: iPad).
    func gawkList(background: Bool = true) -> some View {
        self
            .listStyle(.insetGrouped)
            .scrollContentBackground(.hidden)
            .listRowSeparatorTint(Theme.borderSoft)
            .readableWidth()
            .background(background ? Theme.bg : .clear)
    }

    /// A row on a gawk list.
    func gawkRow() -> some View {
        self
            .listRowBackground(Theme.s1)
            .listRowSeparatorTint(Theme.borderSoft)
    }

    /// Page content at most 640 pt wide, centred (docs/70 D26: iPad).
    func readableWidth() -> some View {
        frame(maxWidth: 640).frame(maxWidth: .infinity)
    }
}

#Preview("List") {
    List {
        Section {
            ListRow(systemImage: "server.rack", title: "gawk", subtitle: "Connected · 24 ms") {
                Image(systemName: "checkmark").foregroundStyle(Theme.accent)
            }
            .gawkRow()
            ListRow(systemImage: "plus", title: "Add a server", action: true).gawkRow()
        } header: {
            SectionHeader("Server")
        } footer: {
            SectionFooter("The default is the official gawk fleet.")
        }
    }
    .gawkList()
    .preferredColorScheme(.dark)
}
