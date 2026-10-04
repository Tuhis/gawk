import SwiftUI
import UIKit

/// The Broadcast screen (docs/67 D18, D21, D23; UX flow §7).
struct BroadcastView: View {
    @Environment(AppSettings.self) private var settings
    @Environment(BroadcastSession.self) private var session
    @Environment(Capture.self) private var capture
    @State private var room = ""
    @State private var copied: String?

    var body: some View {
        NavigationStack {
            Form {
                if !settings.isDefaultRelay {
                    ServerStrip(name: settings.relayName, url: settings.relayURL)
                }
                switch session.phase {
                case .idle, .ended:
                    setup
                case .connecting:
                    Section { ProgressView("Connecting…") }
                case .resuming(let attempt):
                    Section { ProgressView("Reconnecting (attempt \(attempt))…") }
                    stopSection
                case .live(let code, let link):
                    live(code: code, link: link)
                    stopSection
                }
            }
            .navigationTitle("Broadcast")
        }
    }

    @ViewBuilder private var setup: some View {
        if case .ended(let reason?) = session.phase {
            Section { Label(reason, systemImage: "exclamationmark.triangle") }
        }
        if let failure = session.failure {
            Section { Label(failure, systemImage: "exclamationmark.triangle") }
        }
        if !settings.sawCaptureNote {
            Section {
                // D18: ReplayKit's successor captures everything on screen.
                Label(
                    "Everything on your screen is broadcast, notifications included. Turn on a Focus to keep them off the stream.",
                    systemImage: "bell.slash"
                )
                Button("Got it") { settings.sawCaptureNote = true }
            }
        }
        Section("Room") {
            TextField("Room code (optional)", text: $room)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
            if !settings.recentRooms.isEmpty {
                ScrollView(.horizontal, showsIndicators: false) {
                    HStack {
                        ForEach(settings.recentRooms, id: \.self) { r in
                            Button(r) { room = r }.buttonStyle(.bordered)
                        }
                    }
                }
            }
        }
        Section {
            Button {
                start(test: false)
            } label: {
                Label("Start broadcasting", systemImage: "dot.radiowaves.left.and.right")
            }
            .disabled(!capture.screenAvailable)
            if !capture.screenAvailable {
                Text("Screen broadcasting needs a device: the Simulator has no screen capture.")
                    .font(.footnote)
                    .foregroundStyle(.secondary)
            }
            #if DEBUG
            Button {
                start(test: true)
            } label: {
                Label("Test broadcast", systemImage: "testtube.2")
            }
            #endif
        } footer: {
            Text("Broadcasting to \(settings.relayName).")
        }
    }

    @ViewBuilder private func live(code: String, link: String) -> some View {
        Section {
            HStack {
                Text(code)
                    .font(.system(size: 40, weight: .bold, design: .monospaced))
                    .textSelection(.enabled)
                Spacer()
                Label("\(session.viewerCount)", systemImage: "eye")
                    .accessibilityLabel("\(session.viewerCount) watching")
            }
            Button("Copy link") { copy(link, as: "link") }
            Button("Copy code") { copy(code, as: "code") }
            ShareLink(item: URL(string: link) ?? URL(string: "https://gawk.ioio.fi")!) {
                Label("Share", systemImage: "square.and.arrow.up")
            }
            if let copied {
                Text("Copied the \(copied).").font(.footnote).foregroundStyle(.secondary)
            }
        } header: {
            Text("Live")
        }
        if let roomText = session.roomText {
            Section("Room") { Text(roomText) }
        }
    }

    private var stopSection: some View {
        Section {
            Button("Stop broadcasting", role: .destructive) { capture.stop(session) }
        }
    }

    private func start(test: Bool) {
        settings.sawCaptureNote = true
        let r = room.trimmingCharacters(in: .whitespaces)
        if !r.isEmpty { settings.noteRoom(r) }
        capture.start(session: session, settings: settings, room: r, test: test)
    }

    private func copy(_ text: String, as what: String) {
        UIPasteboard.general.string = text
        copied = what
    }
}

/// docs/40's persistent strip: which non-default server this is.
struct ServerStrip: View {
    let name: String
    let url: String

    var body: some View {
        Section {
            Label {
                VStack(alignment: .leading) {
                    Text("Using \(name)")
                    Text(url).font(.footnote).foregroundStyle(.secondary)
                }
            } icon: {
                Image(systemName: "server.rack")
            }
        }
    }
}

/// Owns the capture side of a broadcast: ScreenCaptureKit on a device, the
/// D27 test source in debug builds, and the path monitor (D19).
@MainActor
@Observable
final class Capture {
    @ObservationIgnored private let identity: IdentityStore
    @ObservationIgnored private let path = PathMonitor()
    #if canImport(ScreenCaptureKit)
    @ObservationIgnored private var screen: ScreenCapture?
    #endif
    #if DEBUG
    @ObservationIgnored private var test: TestBroadcastSource?
    /// The running test source, for tests.
    var testSource: TestBroadcastSource? { test }
    #endif

    /// Whether a source is feeding the session.
    var isCapturing: Bool {
        #if DEBUG
        if test != nil { return true }
        #endif
        #if canImport(ScreenCaptureKit)
        if screen != nil { return true }
        #endif
        return false
    }

    /// The live session, for path changes (D19).
    @ObservationIgnored private weak var live: BroadcastSession?

    init(identity: IdentityStore) {
        self.identity = identity
        // One monitor for the app's life: an NWPathMonitor can't restart
        // once cancelled, and `isExpensive` must be known before a start.
        path.start { [weak self] in
            Task { @MainActor in self?.live?.pathChanged() }
        }
    }

    /// Whether this build can capture the screen (not in the Simulator).
    var screenAvailable: Bool {
        #if canImport(ScreenCaptureKit)
        true
        #else
        false
        #endif
    }

    func start(session: BroadcastSession, settings: AppSettings, room: String, test: Bool) {
        // A source left from an earlier broadcast must not feed this one, or
        // end it from its own stop callback.
        stopSources()
        // D19: an expensive path picks the Cellular rung at start, and
        // never switches mid-broadcast.
        let quality: Quality = path.isExpensive ? .cellular : .standard
        session.start(
            relayURL: settings.relayURL,
            secret: identity.secret(relay: settings.relayURL),
            quality: quality,
            room: room,
            nickname: settings.nickname,
            telemetry: settings.telemetry,
            insecure: settings.insecure
        )
        live = session
        // The core can end the broadcast itself (a refusal, an operator, an
        // error); capture goes with it.
        session.onEnded = { [weak self] in self?.stopSources() }
        #if DEBUG
        if test {
            let source = TestBroadcastSource(sink: session.media)
            source.start()
            self.test = source
            return
        }
        #endif
        #if canImport(ScreenCaptureKit)
        let screen = ScreenCapture(sink: session.media)
        self.screen = screen
        screen.present { [weak self, weak session, weak screen] reason in
            // The capture ended: by the user, the system or an error (D7).
            guard let self, let session, let screen, self.screen === screen else { return }
            session.noteCaptureEnded(reason)
            self.stop(session)
        }
        #endif
    }

    func stop(_ session: BroadcastSession) {
        stopSources()
        live = nil
        session.stop()
    }

    /// Stops capture only; the session is the caller's.
    private func stopSources() {
        #if DEBUG
        test?.stop()
        test = nil
        #endif
        #if canImport(ScreenCaptureKit)
        screen?.stop()
        screen = nil
        #endif
    }
}
