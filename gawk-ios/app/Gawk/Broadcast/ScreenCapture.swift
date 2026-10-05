#if canImport(ScreenCaptureKit)
import CoreMedia
import Foundation
import ScreenCaptureKit

/// The device screen through iOS 27's ScreenCaptureKit, in the app's own
/// process (docs/67 OD17, D6, D18). The system picker chooses the display;
/// frames and audio go to the `MediaSink` from capture queues.
///
/// The Simulator SDK has no ScreenCaptureKit (V-12), so this compiles for
/// devices only; phase S broadcasts from D27's test source instead.
@MainActor
final class ScreenCapture: NSObject {
    private let sink: MediaSink
    private var stream: SCStream?
    private let videoQueue = DispatchQueue(label: "gawk.capture.video")
    private let audioQueue = DispatchQueue(label: "gawk.capture.audio")
    private var onStop: ((String?) -> Void)?
    private var output: Output?
    /// What each callback may still do (`CaptureEvents`).
    private var events = CaptureEvents()

    init(sink: MediaSink) {
        self.sink = sink
    }

    /// Presents the system picker for the display (D18) with the microphone
    /// and camera controls off (OD8, OD1). `onStop` reports the end of
    /// capture, with a reason unless the user stopped it.
    func present(onStop: @escaping (String?) -> Void) {
        self.onStop = onStop
        let picker = SCContentSharingPicker.shared
        var config = SCContentSharingPickerConfiguration()
        config.showsMicrophoneControl = false
        config.showsCameraControl = false
        picker.defaultConfiguration = config
        picker.add(self)
        picker.isActive = true
        picker.present(using: .display)
    }

    /// Our stop. Callbacks already on their way to the main actor find
    /// `events` stopped and do nothing: no restarted stream, no second end.
    func stop() {
        events.stop()
        onStop = nil
        retireStream()
        SCContentSharingPicker.shared.isActive = false
        SCContentSharingPicker.shared.remove(self)
    }

    /// Every ScreenCaptureKit report, on the main actor.
    private func handle(_ event: CaptureEvents.Event, filter: SCContentFilter? = nil) {
        switch events.handle(event) {
        case .ignore:
            break
        case .startStream:
            if let filter { start(filter: filter) }
        case .end(let reason):
            // The system's stop (its sharing UI, Control Center) or a
            // failure: capture is over, and the broadcast with it.
            let report = onStop
            stop()
            report?(reason)
        }
    }

    /// Stops the running stream, if any, and detaches its output first, so
    /// a stream we stopped can't report an end of capture or feed the sink.
    private func retireStream() {
        output?.onStop = nil
        output?.isRetired = true
        let s = stream
        stream = nil
        output = nil
        Task { try? await s?.stopCapture() }
    }

    /// Captures `filter`. The picker stays active for the whole broadcast,
    /// so the user can change the shared content mid-capture; iOS has no
    /// `updateContentFilter`/`updateConfiguration` (macOS-only in the iOS 27
    /// SDK), so the running stream is replaced, never run beside a new one.
    private func start(filter: SCContentFilter) {
        retireStream()
        let config = SCStreamConfiguration()
        // D8: the size is the rung's, but iOS has no scalesToFit or
        // pixelFormat, so FrameConverter does the real work (V-6).
        config.width = Int(filter.contentRect.width * CGFloat(filter.pointPixelScale))
        config.height = Int(filter.contentRect.height * CGFloat(filter.pointPixelScale))
        // D11: ask for the lane's format; the shim reads what really comes.
        config.capturesAudio = true
        config.sampleRate = 48_000
        config.channelCount = 2
        config.excludesCurrentProcessAudio = true
        let output = Output(sink: sink)
        let stream = SCStream(filter: filter, configuration: config, delegate: output)
        output.stream = stream
        output.onStop = { [weak self, weak stream] reason in
            Task { @MainActor in
                // A replaced stream's end is not the capture's.
                guard let self, let stream, self.stream === stream else { return }
                self.handle(.streamStopped(reason))
            }
        }
        do {
            try stream.addStreamOutput(output, type: .screen, sampleHandlerQueue: videoQueue)
            try stream.addStreamOutput(output, type: .audio, sampleHandlerQueue: audioQueue)
        } catch {
            handle(.startFailed(error.localizedDescription))
            return
        }
        self.stream = stream
        self.output = output
        Task {
            do {
                try await stream.startCapture()
            } catch {
                // Only this stream's failure counts: one retired meanwhile
                // (a re-pick, or our stop) failing to start is no news.
                guard self.stream === stream else { return }
                self.handle(.startFailed(error.localizedDescription))
            }
        }
    }

    /// The stream's outputs and delegate, off the main actor.
    private final class Output: NSObject, SCStreamOutput, SCStreamDelegate, @unchecked Sendable {
        let sink: MediaSink
        weak var stream: SCStream?
        var onStop: ((String?) -> Void)?
        /// Set (on the main actor) when the stream is replaced; samples still
        /// in flight from it (on the capture queues) are dropped, not
        /// interleaved with the new stream's.
        var isRetired: Bool {
            get { retiredLock.withLock { retired } }
            set { retiredLock.withLock { retired = newValue } }
        }
        private let retiredLock = NSLock()
        private var retired = false
        private var lastStatus: Int = 0

        init(sink: MediaSink) {
            self.sink = sink
        }

        /// PTS on the host clock (V-4): ScreenCaptureKit stamps on its
        /// synchronization clock, which is converted rather than assumed.
        private func hostTime(_ t: CMTime) -> CMTime {
            guard let clock = stream?.synchronizationClock else { return t }
            return CMSyncConvertTime(t, from: clock, to: CMClockGetHostTimeClock())
        }

        func stream(_ stream: SCStream, didOutputSampleBuffer sample: CMSampleBuffer, of type: SCStreamOutputType) {
            guard !isRetired else { return }
            switch type {
            case .screen:
                let info = (CMSampleBufferGetSampleAttachmentsArray(sample, createIfNecessary: false)
                    as? [[SCStreamFrameInfo: Any]])?.first
                let status = (info?[.status] as? Int) ?? 0
                if status == 0, lastStatus == SCFrameStatus.suspended.rawValue {
                    sink.forceIDR()
                }
                lastStatus = status
                let rotation = (info?[.videoOrientation] as? UInt32)
                    .flatMap(frameRotation(fromImageOrientation:))
                let pts = hostTime(CMSampleBufferGetPresentationTimeStamp(sample))
                guard let pixels = CMSampleBufferGetImageBuffer(sample) else {
                    return
                }
                sink.video(pixels, rotation: rotation, pts: pts, status: status)
            case .audio:
                sink.audio(sample, pts: hostTime(CMSampleBufferGetPresentationTimeStamp(sample)))
            default:
                break // the microphone is never added (OD8)
            }
        }

        func stream(_ stream: SCStream, didStopWithError error: any Error) {
            let code = (error as NSError).code
            // The user's stop (the indicator or Control Center) is not an
            // error to show; anything else, a background-mode loss included,
            // is (D7).
            onStop?(code == SCStreamError.userStopped.rawValue ? nil : error.localizedDescription)
        }
    }
}

extension ScreenCapture: SCContentSharingPickerObserver {
    nonisolated func contentSharingPicker(_ picker: SCContentSharingPicker, didCancelFor stream: SCStream?) {
        Task { @MainActor in self.handle(.pickerCancelled) }
    }

    nonisolated func contentSharingPicker(
        _ picker: SCContentSharingPicker, didUpdateWith filter: SCContentFilter, for stream: SCStream?
    ) {
        // The filter is handed over once and only read on the main actor.
        nonisolated(unsafe) let filter = filter
        Task { @MainActor in self.handle(.picked, filter: filter) }
    }

    nonisolated func contentSharingPickerStartDidFailWithError(_ error: any Error) {
        Task { @MainActor in self.handle(.pickerFailed(error.localizedDescription)) }
    }
}
#endif
