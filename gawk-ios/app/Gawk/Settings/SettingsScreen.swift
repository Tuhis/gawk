import SwiftUI

/// Settings (docs/70 D1 board, D22): the servers with their probe status,
/// the nickname, the diagnostics opt-in and About. Every server has Edit,
/// and a publish secret lives there (docs/64 D15).
struct SettingsScreen: View {
    @Environment(AppSettings.self) private var settings
    @Environment(ServerProbes.self) private var probes
    @Environment(AppRouter.self) private var router
    let identity: IdentityStore
    @State private var path: [ServerRoute] = []

    var body: some View {
        @Bindable var settings = settings
        NavigationStack(path: $path) {
            List {
                Section {
                    serverRow(name: AppSettings.defaultServerName, url: coreInfo().defaultRelayUrl, selected: settings.selectedURL.isEmpty) {
                        settings.selectedURL = ""
                    }
                    ForEach(settings.servers) { s in
                        serverRow(name: s.name, url: s.url, selected: settings.selectedURL == s.url) {
                            settings.selectedURL = s.url
                        }
                    }
                    Button {
                        path.append(.add)
                    } label: {
                        ListRow(systemImage: "plus", title: "Add a server", action: true)
                    }
                    .accessibilityIdentifier("settings.addServer")
                    .gawkRow()
                } header: {
                    SectionHeader("Server")
                } footer: {
                    SectionFooter("The default is the official gawk fleet. A publish secret is only sent to the server it was saved for.")
                }
                Section {
                    HStack(spacing: 12) {
                        RowIcon(systemImage: "person")
                        TextField("Nickname", text: $settings.nickname)
                            .textInputAutocapitalization(.words)
                            .autocorrectionDisabled()
                            .foregroundStyle(Theme.text)
                            .accessibilityIdentifier("settings.nickname")
                    }
                    .frame(minHeight: 38)
                    .gawkRow()
                } header: {
                    SectionHeader("You")
                } footer: {
                    SectionFooter("Shown to others in a room.")
                }
                Section {
                    Toggle(isOn: $settings.telemetry) {
                        ListRow(systemImage: "chart.bar", title: "Send diagnostics")
                    }
                    .accessibilityIdentifier("settings.telemetry")
                    .gawkRow()
                } header: {
                    SectionHeader("Privacy")
                } footer: {
                    SectionFooter("Off by default. Session statistics go to the server's operator, never what's on your screen.")
                }
                #if DEBUG
                Section {
                    Toggle(isOn: $settings.insecure) {
                        ListRow(systemImage: "hammer", title: "Accept a local relay's dev certificate")
                    }
                    .gawkRow()
                } header: {
                    SectionHeader("Development")
                }
                #endif
                Section {
                    ListRow(systemImage: "info.circle", title: "Version") {
                        Text(coreInfo().version)
                    }
                    .accessibilityElement(children: .combine)
                    .gawkRow()
                    Link(destination: URL(string: "https://gawk.ioio.fi/#/terms")!) {
                        ListRow(systemImage: "doc.text", title: "Terms of use") {
                            Image(systemName: "arrow.up.forward")
                                .font(.footnote.weight(.semibold))
                                .foregroundStyle(Theme.faint)
                        }
                    }
                    .gawkRow()
                } header: {
                    SectionHeader("About")
                }
            }
            .gawkList()
            .navigationTitle("Settings")
            .navigationDestination(for: ServerRoute.self) { route in
                EditServerScreen(route: route, identity: identity) { path.removeAll() }
            }
        }
        .onChange(of: router.settingsRoute, initial: true) { _, route in
            guard let route else { return }
            path = [route]
            router.settingsRoute = nil
        }
    }

    /// One server (D22): a tap selects it, its status says whether it
    /// answers, and the info button opens Edit.
    private func serverRow(name: String, url: String, selected: Bool, select: @escaping () -> Void) -> some View {
        let state = probes.state(url)
        return HStack(spacing: 12) {
            Button(action: select) {
                HStack(spacing: 12) {
                    RowIcon(systemImage: "server.rack")
                    VStack(alignment: .leading, spacing: 3) {
                        Text(name)
                            .font(.body)
                            .foregroundStyle(Theme.text)
                        ProbeStatus(state: state)
                    }
                    Spacer(minLength: 8)
                    if selected {
                        Image(systemName: "checkmark")
                            .font(.body.weight(.semibold))
                            .foregroundStyle(Theme.accent)
                    }
                }
                .contentShape(.rect)
            }
            .buttonStyle(.plain)
            .accessibilityElement(children: .combine)
            .accessibilityAddTraits(selected ? .isSelected : [])
            .accessibilityIdentifier("server.row.\(name)")
            Button {
                path.append(url == coreInfo().defaultRelayUrl ? .builtIn : .saved(url))
            } label: {
                Image(systemName: "info.circle")
                    .font(.title3)
                    .foregroundStyle(Theme.accent)
            }
            .buttonStyle(.borderless)
            .accessibilityLabel("Edit \(name)")
            .accessibilityIdentifier("server.info.\(name)")
        }
        .frame(minHeight: 38)
        .gawkRow()
        .task(id: url) { probes.refresh(url, insecure: settings.insecure) }
    }
}

/// A server's probe result as the row shows it: a dot and the words (D22).
struct ProbeStatus: View {
    let state: ServerProbes.State?

    var body: some View {
        HStack(spacing: 6) {
            switch state {
            case .reachable:
                Circle().fill(Theme.ok).frame(width: 7, height: 7)
            case .unreachable:
                Circle().fill(Theme.warn).frame(width: 7, height: 7)
            case .checking, nil:
                EmptyView()
            }
            Text(ServerProbes.describe(state))
                .font(.footnote)
                .foregroundStyle(state == .unreachable ? Theme.warnText : Theme.muted)
        }
    }
}

/// Which server Edit shows.
enum ServerRoute: Hashable {
    /// The compiled-in fleet: Name and Relay locked.
    case builtIn
    case saved(String)
    /// Add a server: an empty page.
    case add
}

/// Edit server (docs/70 D2, D23): the header, Name and Relay (locked for the
/// built-in server), the publish secret, and Delete for a saved one.
struct EditServerScreen: View {
    let route: ServerRoute
    let identity: IdentityStore
    var done: () -> Void
    @Environment(AppSettings.self) private var settings
    @Environment(ServerProbes.self) private var probes
    @State private var name = ""
    @State private var url = "https://"
    @State private var secret = ""
    @State private var loaded = false

    private var locked: Bool { route == .builtIn }

    /// The URL the secret and the probe belong to.
    private var key: String {
        switch route {
        case .builtIn: coreInfo().defaultRelayUrl
        case .saved(let u): u
        case .add: url.trimmingCharacters(in: .whitespaces)
        }
    }

    private var canSave: Bool {
        let u = url.trimmingCharacters(in: .whitespaces)
        return u.hasPrefix("https://") && u.count > "https://".count
    }

    var body: some View {
        List {
            if route != .add {
                Section {
                    HStack(spacing: 14) {
                        Image(systemName: "server.rack")
                            .font(.title2)
                            .foregroundStyle(Theme.muted)
                            .frame(width: 52, height: 52)
                            .background(Theme.s3, in: .rect(cornerRadius: 14, style: .continuous))
                        VStack(alignment: .leading, spacing: 4) {
                            Text(name).font(.title3.bold()).foregroundStyle(Theme.text)
                            ProbeStatus(state: probes.state(key))
                        }
                    }
                    .listRowBackground(Color.clear)
                }
            }
            Section {
                field("Name", text: $name, identifier: "server.name")
                field("https://relay.example.com:4433", label: "Relay", text: $url, identifier: "server.url")
                    .keyboardType(.URL)
            } header: {
                SectionHeader("Server")
            }
            Section {
                HStack(spacing: 12) {
                    RowIcon(systemImage: "key")
                    // A server's API key, not an account password: a
                    // SecureField makes iOS offer to save it to Passwords in
                    // a sheet over the app. Redacted in screenshots and the
                    // app switcher instead.
                    TextField("Publish secret", text: $secret)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .privacySensitive()
                        .foregroundStyle(Theme.text)
                        .accessibilityIdentifier("server.secret")
                    PasteButton(payloadType: String.self) { strings in
                        secret = strings.first?.trimmingCharacters(in: .whitespacesAndNewlines) ?? secret
                    }
                    .labelStyle(.iconOnly)
                    .buttonBorderShape(.circle)
                    .tint(Theme.s3)
                }
                .frame(minHeight: 38)
                .gawkRow()
            } header: {
                SectionHeader("Publish secret")
            } footer: {
                SectionFooter("Only needed if the server asks for one. It stays in your Keychain and is sent to this server only.")
            }
            if case .saved = route {
                Section {
                    Button(role: .destructive) {
                        settings.removeServer(key)
                        identity.setSecret("", relay: key)
                        done()
                    } label: {
                        Text("Delete server").foregroundStyle(Theme.dangerText)
                    }
                    .accessibilityIdentifier("server.delete")
                    .gawkRow()
                }
            }
        }
        .gawkList()
        .navigationTitle(route == .add ? "Add a server" : "Edit server")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            if route != .builtIn {
                ToolbarItem(placement: .confirmationAction) {
                    Button(route == .add ? "Add" : "Save") { save() }
                        .disabled(!canSave)
                        .accessibilityIdentifier("server.save")
                }
            }
        }
        .onAppear(perform: load)
        // The built-in server has only its secret to edit, so it saves as
        // it's typed; the others save with the button.
        .onChange(of: secret) { _, s in
            if route == .builtIn { identity.setSecret(s, relay: key) }
        }
    }

    @ViewBuilder
    private func field(_ placeholder: String, label: String? = nil, text: Binding<String>, identifier: String) -> some View {
        HStack(spacing: 12) {
            Text(label ?? placeholder)
                .foregroundStyle(Theme.text)
                .frame(width: 64, alignment: .leading)
            if locked {
                Text(text.wrappedValue)
                    .foregroundStyle(Theme.muted)
                    .lineLimit(1)
                    .truncationMode(.middle)
                    .frame(maxWidth: .infinity, alignment: .leading)
                Image(systemName: "lock")
                    .foregroundStyle(Theme.faint)
                    .accessibilityLabel("Locked")
            } else {
                TextField(placeholder, text: text)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                    .foregroundStyle(Theme.text)
                    .accessibilityIdentifier(identifier)
            }
        }
        .frame(minHeight: 38)
        .modifier(LockedRow(locked: locked, identifier: identifier))
        .gawkRow()
    }

    private func load() {
        guard !loaded else { return }
        loaded = true
        switch route {
        case .builtIn:
            name = AppSettings.defaultServerName
            url = coreInfo().defaultRelayUrl
        case .saved(let u):
            name = settings.servers.first { $0.url == u }?.name ?? u
            url = u
        case .add:
            break
        }
        secret = route == .add ? "" : identity.secret(relay: key)
    }

    private func save() {
        let newURL = url.trimmingCharacters(in: .whitespaces)
        if case .saved(let old) = route, old != newURL {
            settings.moveServer(from: old, to: newURL, name: name)
            identity.setSecret("", relay: old)
        }
        saveServer(settings: settings, identity: identity, name: name, url: newURL, secret: secret)
        // Edit showed the stored secret, so a blank field clears it; Add
        // starts blank, and blank there keeps one (saveServer).
        if case .saved = route, secret.isEmpty { identity.setSecret("", relay: newURL) }
        done()
    }
}

/// A locked row reads as one element ("Name, gawk, Locked"); an editable
/// one leaves its field to be found and typed into.
private struct LockedRow: ViewModifier {
    let locked: Bool
    let identifier: String

    func body(content: Content) -> some View {
        if locked {
            content
                .accessibilityElement(children: .combine)
                .accessibilityIdentifier(identifier)
        } else {
            content
        }
    }
}

/// Adds a server, or renames one already saved under the same URL, with the
/// secret typed on the page. A blank secret keeps the saved one: a rename
/// must not delete it (Edit's own field is where a secret is cleared).
@MainActor
func saveServer(settings: AppSettings, identity: IdentityStore, name: String, url: String, secret: String) {
    settings.addServer(name: name, url: url)
    if !secret.isEmpty {
        identity.setSecret(secret, relay: url.trimmingCharacters(in: .whitespaces))
    }
}
