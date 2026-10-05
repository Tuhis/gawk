import AVFoundation
import AVKit

/// Picture-in-Picture from the player's display layer (docs/67 D22).
///
/// The content source is the `AVSampleBufferDisplayLayer` itself, so PiP
/// shows exactly what the inline player shows, from the same renderer and
/// synchronizer. The playback delegate describes a live stream: never
/// paused and an infinite time range, so the PiP window has no scrubber and
/// no skip buttons. PiP starts automatically when the app leaves the
/// foreground while playing.
@MainActor
final class PictureInPicture: NSObject {
    private var controller: AVPictureInPictureController?
    /// Whether PiP is (about to be) showing; the Watch screen reads it to
    /// decide whether video keeps rendering in the background.
    private(set) var isActive = false
    var onActiveChange: ((Bool) -> Void)?

    init(layer: AVSampleBufferDisplayLayer) {
        super.init()
        guard AVPictureInPictureController.isPictureInPictureSupported() else { return }
        let source = AVPictureInPictureController.ContentSource(
            sampleBufferDisplayLayer: layer, playbackDelegate: self)
        let controller = AVPictureInPictureController(contentSource: source)
        controller.delegate = self
        controller.canStartPictureInPictureAutomaticallyFromInline = true
        controller.requiresLinearPlayback = true
        self.controller = controller
    }

    var isSupported: Bool { controller != nil }

    /// docs/70 D8: PiP starts on its own when the app leaves with the
    /// player up.
    var startsAutomaticallyFromInline: Bool {
        controller?.canStartPictureInPictureAutomaticallyFromInline ?? false
    }

    func start() { controller?.startPictureInPicture() }
    func stop() { controller?.stopPictureInPicture() }

    private func setActive(_ active: Bool) {
        guard active != isActive else { return }
        isActive = active
        onActiveChange?(active)
    }
}

extension PictureInPicture: AVPictureInPictureSampleBufferPlaybackDelegate {
    nonisolated func pictureInPictureController(
        _ controller: AVPictureInPictureController, setPlaying playing: Bool
    ) {
        // Live: there is nothing to pause to. The window's button shows
        // "playing" because isPlaybackPaused always says so.
    }

    nonisolated func pictureInPictureControllerTimeRangeForPlayback(
        _ controller: AVPictureInPictureController
    ) -> CMTimeRange {
        // Apple's convention for live content: an infinite range, no scrubber.
        CMTimeRange(start: .negativeInfinity, duration: .positiveInfinity)
    }

    nonisolated func pictureInPictureControllerIsPlaybackPaused(
        _ controller: AVPictureInPictureController
    ) -> Bool {
        false
    }

    nonisolated func pictureInPictureController(
        _ controller: AVPictureInPictureController,
        didTransitionToRenderSize newRenderSize: CMVideoDimensions
    ) {}

    nonisolated func pictureInPictureController(
        _ controller: AVPictureInPictureController,
        skipByInterval skipInterval: CMTime,
        completion completionHandler: @escaping @Sendable () -> Void
    ) {
        completionHandler()
    }
}

extension PictureInPicture: AVPictureInPictureControllerDelegate {
    nonisolated func pictureInPictureControllerWillStartPictureInPicture(
        _ controller: AVPictureInPictureController
    ) {
        MainActor.assumeIsolated { setActive(true) }
    }

    nonisolated func pictureInPictureControllerDidStopPictureInPicture(
        _ controller: AVPictureInPictureController
    ) {
        MainActor.assumeIsolated { setActive(false) }
    }

    nonisolated func pictureInPictureController(
        _ controller: AVPictureInPictureController,
        failedToStartPictureInPictureWithError error: any Error
    ) {
        MainActor.assumeIsolated { setActive(false) }
    }
}
