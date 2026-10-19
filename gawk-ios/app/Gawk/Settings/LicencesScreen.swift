import SwiftUI

/// Settings → Open-source licences (R72, docs/74 D3): every third-party
/// package in the app with its licence and copyright, where the MPL sources
/// are, and each licence text a tap away. The notices are
/// `gawk-ios/THIRD-PARTY-NOTICES.md`, embedded in the core and parsed there
/// (D2), so the page lists exactly what the generator found linked.
struct LicencesScreen: View {
    @State private var notices: Notices?

    var body: some View {
        List {
            if let notices {
                Section {
                    paragraph("gawk is open source under the Apache License 2.0. It is built on the open-source software below, each under its own licence.")
                    ForEach(Array(notices.intro.components(separatedBy: "\n\n").enumerated()), id: \.offset) { _, text in
                        paragraph(text)
                    }
                }
                Section {
                    ForEach(Array(notices.packages.enumerated()), id: \.offset) { _, package in
                        PackageRow(package: package).gawkRow()
                    }
                } header: {
                    SectionHeader("Packages (\(notices.packages.count))")
                }
                if !notices.sources.isEmpty {
                    Section {
                        ForEach(Array(notices.sources.enumerated()), id: \.offset) { _, source in
                            if let url = URL(string: source.url) {
                                Link(destination: url) {
                                    VStack(alignment: .leading, spacing: 2) {
                                        Text(source.package).foregroundStyle(Theme.text)
                                        Text(source.url)
                                            .font(.footnote)
                                            .foregroundStyle(Theme.accentText)
                                            .lineLimit(1)
                                            .truncationMode(.middle)
                                    }
                                }
                                .gawkRow()
                            }
                        }
                    } header: {
                        SectionHeader("Source code")
                    } footer: {
                        SectionFooter("These packages are under the Mozilla Public License 2.0. Their source code, unmodified, is at these addresses.")
                    }
                }
                Section {
                    ForEach(Array(notices.texts.enumerated()), id: \.offset) { index, text in
                        NavigationLink {
                            LicenceTextScreen(text: text)
                        } label: {
                            VStack(alignment: .leading, spacing: 2) {
                                Text(text.license).foregroundStyle(Theme.text)
                                Text(text.appliesTo)
                                    .font(.footnote)
                                    .foregroundStyle(Theme.muted)
                                    .lineLimit(2)
                            }
                        }
                        .accessibilityIdentifier("licences.text.\(index)")
                        .gawkRow()
                    }
                } header: {
                    SectionHeader("Licence texts")
                }
            }
        }
        .gawkList()
        .navigationTitle("Open-source licences")
        .navigationBarTitleDisplayMode(.inline)
        // Parsed on the first open, not at launch: few users look.
        .task {
            if notices == nil { notices = thirdPartyNotices() }
        }
    }

    private func paragraph(_ text: String) -> some View {
        Text(text)
            .font(.footnote)
            .foregroundStyle(Theme.muted)
            .gawkRow()
    }
}

/// One package: its name and version, its licence, and its copyright lines.
private struct PackageRow: View {
    let package: NoticePackage

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                Text(package.version.isEmpty ? package.name : "\(package.name) \(package.version)")
                    .foregroundStyle(Theme.text)
                Spacer(minLength: 8)
                Text(package.license)
                    .font(.footnote)
                    .foregroundStyle(Theme.accentText)
                    .multilineTextAlignment(.trailing)
            }
            ForEach(Array(package.copyright.enumerated()), id: \.offset) { _, line in
                Text(line)
                    .font(.footnote)
                    .foregroundStyle(Theme.muted)
            }
        }
        .accessibilityElement(children: .combine)
    }
}

/// One licence text, whole and selectable.
struct LicenceTextScreen: View {
    let text: NoticeText

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                Text("Applies to: \(text.appliesTo)")
                    .font(.footnote)
                    .foregroundStyle(Theme.muted)
                Text(text.body)
                    .font(.footnote.monospaced())
                    .foregroundStyle(Theme.text)
                    .textSelection(.enabled)
            }
            .padding()
            .readableWidth()
        }
        .background(Theme.bg)
        .navigationTitle(text.license)
        .navigationBarTitleDisplayMode(.inline)
    }
}
