import Foundation
import Observation

/// Owns the capture side of a broadcast: ScreenCaptureKit on a device, the
/// D27 test source in debug builds, and the path monitor (D19).
@MainActor
@Observable
final class Capture {
    /// The Keychain: secrets, identities and room credentials.
    @ObservationIgnored let identity: IdentityStore
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

    /// Whether Go live uses D27's test source: a debug build with no screen
    /// to capture (the Simulator), or one launched with `-gawkTestSource`.
    var usesTestSource: Bool {
        #if DEBUG
        !screenAvailable || ProcessInfo.processInfo.arguments.contains("-gawkTestSource")
        #else
        false
        #endif
    }

    /// The rung a choice means on the current path (docs/67 D19).
    func resolve(_ choice: QualityChoice) -> Quality {
        choice.resolve(expensivePath: path.isExpensive)
    }

    /// The secret a start presents: the selected server's own, never one
    /// saved for another (docs/40, docs/70 D23).
    func publishSecret(_ settings: AppSettings) -> String {
        identity.secret(relay: settings.relayURL)
    }

    func start(session: BroadcastSession, settings: AppSettings) {
        // A source left from an earlier broadcast must not feed this one, or
        // end it from its own stop callback.
        stopSources()
        #if DEBUG
        CaptureDiagnostics.shared.reset()
        #endif
        let test = usesTestSource
        session.start(
            relayURL: settings.relayURL,
            secret: publishSecret(settings),
            quality: resolve(settings.quality),
            nickname: settings.nickname,
            telemetry: settings.telemetry,
            insecure: settings.insecure,
            captureSource: test ? "test-source" : "screencapturekit"
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

    /// End, and capture ending on its own. The session goes first: the
    /// broadcast must end even if tearing capture down misbehaves.
    func stop(_ session: BroadcastSession) {
        live = nil
        session.stop()
        stopSources()
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
