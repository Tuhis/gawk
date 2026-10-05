/// What `ScreenCapture` does with each ScreenCaptureKit callback, kept out
/// of the device-only file so the Simulator can test it (V-12).
///
/// The picker and the stream report from framework queues, and each report
/// hops to the main actor in a task of its own, so reports can arrive after
/// our own stop. Once stopped, nothing may start a stream again or report a
/// second end: a picker update delivered late would otherwise restart
/// capture behind a broadcast that has ended, and the system's sharing
/// indicator would stay on.
struct CaptureEvents {
    enum Event: Equatable {
        /// The picker delivered content (`didUpdateWith`).
        case picked
        /// The picker closed without a choice (`didCancelFor`), which is
        /// also how the system's own sharing UI can end a share.
        case pickerCancelled
        /// The picker couldn't open.
        case pickerFailed(String)
        /// The running stream stopped (`didStopWithError`): `nil` when the
        /// user stopped it from the system UI.
        case streamStopped(String?)
        /// `startCapture` (or adding an output) failed.
        case startFailed(String)
    }

    enum Action: Equatable {
        case startStream
        /// Capture is over: end the broadcast, showing the reason if any.
        case end(String?)
        case ignore
    }

    private(set) var isStopped = false

    /// Our own stop (the Stop button, or the broadcast ending).
    mutating func stop() {
        isStopped = true
    }

    mutating func handle(_ event: Event) -> Action {
        guard !isStopped else { return .ignore }
        switch event {
        case .picked:
            return .startStream
        case .pickerCancelled:
            isStopped = true
            return .end(nil)
        case .pickerFailed(let why), .startFailed(let why):
            isStopped = true
            return .end(why)
        case .streamStopped(let why):
            isStopped = true
            return .end(why)
        }
    }
}
