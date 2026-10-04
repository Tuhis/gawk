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

    func stop() {
        let s = stream
        stream = nil
        Task { try? await s?.stopCapture() }
        SCContentSharingPicker.shared.isActive = false
        SCContentSharingPicker.shared.remove(self)
    }

    private func start(filter: SCContentFilter) {
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
        output.onStop = { [weak self] reason in
            Task { @MainActor in self?.onStop?(reason) }
        }
        do {
            try stream.addStreamOutput(output, type: .screen, sampleHandlerQueue: videoQueue)
            try stream.addStreamOutput(output, type: .audio, sampleHandlerQueue: audioQueue)
        } catch {
            onStop?(error.localizedDescription)
            return
        }
        self.stream = stream
        self.output = output
        Task {
            do {
                try await stream.startCapture()
            } catch {
                self.onStop?(error.localizedDescription)
            }
        }
    }

    /// The stream's outputs and delegate, off the main actor.
    private final class Output: NSObject, SCStreamOutput, SCStreamDelegate, @unchecked Sendable {
        let sink: MediaSink
        weak var stream: SCStream?
        var onStop: ((String?) -> Void)?
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
        Task { @MainActor in self.onStop?(nil) }
    }

    nonisolated func contentSharingPicker(
        _ picker: SCContentSharingPicker, didUpdateWith filter: SCContentFilter, for stream: SCStream?
    ) {
        // The filter is handed over once and only read on the main actor.
        nonisolated(unsafe) let filter = filter
        Task { @MainActor in self.start(filter: filter) }
    }

    nonisolated func contentSharingPickerStartDidFailWithError(_ error: any Error) {
        Task { @MainActor in self.onStop?(error.localizedDescription) }
    }
}
#endif
