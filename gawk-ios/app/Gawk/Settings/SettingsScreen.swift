import SwiftUI

/// Settings (docs/67 D23): R37's server picker with per-server secrets,
/// the diagnostics opt-in, the nickname, and the terms.
struct SettingsScreen: View {
    @Environment(AppSettings.self) private var settings
    let identity: IdentityStore
    @State private var addingServer = false

    var body: some View {
        @Bindable var settings = settings
        NavigationStack {
            Form {
                Section {
                    ServerRow(name: "gawk (default)", url: coreInfo().defaultRelayUrl,
                              selected: settings.selectedURL.isEmpty, identity: identity,
                              insecure: false) {
                        settings.selectedURL = ""
                    }
                    ForEach(settings.servers) { s in
                        ServerRow(name: s.name, url: s.url, selected: settings.selectedURL == s.url,
                                  identity: identity, insecure: settings.insecure) {
                            settings.selectedURL = s.url
                        }
                    }
                    .onDelete { offsets in
                        for i in offsets { settings.removeServer(settings.servers[i].url) }
                    }
                    Button("Add a server…") { addingServer = true }
                } header: {
                    Text("Server")
                } footer: {
                    Text("The default is the official gawk fleet. A secret is only ever sent to the server it was saved for.")
                }
                Section {
                    TextField("Nickname", text: $settings.nickname)
                } header: {
                    Text("You")
                } footer: {
                    Text("Shown to others in a room, and on your broadcast's tile.")
                }
                Section {
                    Toggle("Send diagnostics", isOn: $settings.telemetry)
                } footer: {
                    Text("Off by default. Session statistics, no screen content, go to the server's diagnostics service.")
                }
                #if DEBUG
                Section("Development") {
                    Toggle("Accept a local relay's dev certificate", isOn: $settings.insecure)
                }
                #endif
                Section("About") {
                    LabeledContent("Version", value: coreInfo().version)
                    Link("Terms of use", destination: URL(string: "https://gawk.ioio.fi/#/terms")!)
                }
            }
            .navigationTitle("Settings")
            .sheet(isPresented: $addingServer) {
                AddServerSheet(identity: identity)
            }
        }
    }
}

/// One server: selectable, with its probe result and its secret.
private struct ServerRow: View {
    let name: String
    let url: String
    let selected: Bool
    let identity: IdentityStore
    let insecure: Bool
    let select: () -> Void
    @State private var probe: ServerProbe?
    @State private var secret = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Button(action: select) {
                HStack {
                    VStack(alignment: .leading) {
                        Text(name)
                        Text(url).font(.footnote).foregroundStyle(.secondary)
                        // The operator's name beside the host, never in
                        // place of it (docs/40 F6).
                        if case .reachable(let rtt, let operatorName) = probe {
                            Text([operatorName, "\(rtt) ms"].compactMap { $0 }.joined(separator: " · "))
                                .font(.caption).foregroundStyle(.secondary)
                        } else if probe == .unreachable {
                            Text("Unreachable").font(.caption).foregroundStyle(.red)
                        }
                    }
                    Spacer()
                    if selected { Image(systemName: "checkmark") }
                }
            }
            .buttonStyle(.plain)
            if selected {
                SecureField("Publish secret (if the server needs one)", text: $secret)
                    .onSubmit { identity.setSecret(secret, relay: url) }
                    .onChange(of: secret) { identity.setSecret(secret, relay: url) }
            }
        }
        .task(id: url) {
            secret = identity.secret(relay: url)
            let (u, i) = (url, insecure)
            probe = await Task.detached { probeServer(relayUrl: u, insecure: i) }.value
        }
    }
}

private struct AddServerSheet: View {
    @Environment(AppSettings.self) private var settings
    @Environment(\.dismiss) private var dismiss
    let identity: IdentityStore
    @State private var name = ""
    @State private var url = "https://"
    @State private var secret = ""

    var body: some View {
        NavigationStack {
            Form {
                TextField("Name", text: $name)
                TextField("https://relay.example.com:4433", text: $url)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                    .keyboardType(.URL)
                SecureField("Publish secret (optional)", text: $secret)
            }
            .navigationTitle("Add a server")
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Add") {
                        saveServer(settings: settings, identity: identity, name: name, url: url, secret: secret)
                        dismiss()
                    }
                    .disabled(!url.hasPrefix("https://") || url.count <= 8)
                }
            }
        }
    }
}

/// Adds a server, or renames one already saved under the same URL, with the
/// secret typed in the sheet. A blank secret keeps the saved one: a rename
/// must not delete it (the row's own field is where a secret is cleared).
@MainActor
func saveServer(settings: AppSettings, identity: IdentityStore, name: String, url: String, secret: String) {
    settings.addServer(name: name, url: url)
    if !secret.isEmpty {
        identity.setSecret(secret, relay: url.trimmingCharacters(in: .whitespaces))
    }
}
