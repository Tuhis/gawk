import AVFoundation
import CoreMedia
import os

/// What the player tells the UI. Delivered on the player's queue; the UI
/// hops to the main actor itself.
enum PlayerEvent: Sendable {
    case status(ViewerStatus)
    case stats(ViewerStats)
    case unsupportedCodec(String)
}

/// The native player (docs/67 D15, D22): the viewer core's output, presented
/// by AVFoundation.
///
/// - **Clock.** One `AVSampleBufferRenderSynchronizer` drives the display
///   layer's `AVSampleBufferVideoRenderer` and an
///   `AVSampleBufferAudioRenderer`, both fed through iOS 27's receivers
///   (`sampleBufferReceiver(adding:)`). It's started on the host clock, media
///   time equal to host time, and every item's `presentAtMs` is mapped onto
///   the host clock (``HostClockMapping``) and from there onto the
///   synchronizer's timebase with `CMSyncConvertTime`. With an audio renderer
///   attached the timebase follows the audio device's clock, so the
///   conversion, not the start, is what keeps PTS on the timeline that's
///   actually presenting; A/V sync is the synchronizer's (OD7).
/// - **Video.** H.264 is enqueued compressed and decoded by the renderer;
///   VP8/VP9 arrive decoded as NV12 and go through an IOSurface pool.
/// - **Drop to live.** Every 16 ms the frame on screen is reported to
///   the core (`Viewer.presented`), which owns D15's 2 × offset rule and
///   answers with a flush.
/// - **Backpressure.** A video path that stays behind, a renderer holding
///   more frames than any offset explains, or one that fails asks the core
///   to resync (``RendererBackpressure``).
///
/// Everything mutable is confined to `queue`; the core's callbacks arrive on
/// its own thread and hop there first.
final class PlayerEngine: ViewerListener, @unchecked Sendable {
    /// The layer the UI shows and PiP takes its content from (D22).
    let displayLayer: AVSampleBufferDisplayLayer

    private let synchronizer = AVSampleBufferRenderSynchronizer()
    private let video: AVSampleBufferVideoRenderer.Receiver
    private let audio: AVSampleBufferAudioRenderer.Receiver
    private let queue = DispatchQueue(label: "fi.ioio.gawk.player", qos: .userInteractive)
    private let onEvent: @Sendable (PlayerEvent) -> Void
    private let log = Logger(subsystem: "fi.ioio.gawk", category: "player")
    private var renderingEvents: Task<Void, Never>?

    // Confined to `queue`.
    private var viewer: Viewer?
    private var timer: DispatchSourceTimer?
    private var mapping = HostClockMapping.sample()
    private var preset: Preset = .balanced
    private var h264Format: CMVideoFormatDescription?
    private var needsKeyframe = true
    private let pool = Nv12PixelBufferPool()
    private var audioFormat: (rate: UInt32, channels: UInt8, description: CMAudioFormatDescription)?
    private var presentation = PresentationLog()
    private var lastReported: UInt64?
    private var lateness: Double = 0
    private var backpressure = RendererBackpressure()
    private var videoEnabled = true
    private var counters = Counters()
    private var lastStats: ViewerStats?
    private var ticks = 0

    /// What the engine has done, for the stats sheet and the smoke test.
    struct Counters: Equatable, Sendable {
        var videoEnqueued = 0
        var audioEnqueued = 0
        var videoDropped = 0
        var rendererResyncs = 0
        var flushes = 0
    }

    @MainActor
    init(onEvent: @escaping @Sendable (PlayerEvent) -> Void) {
        let layer = AVSampleBufferDisplayLayer()
        layer.videoGravity = .resizeAspect
        displayLayer = layer
        video = synchronizer.sampleBufferReceiver(adding: layer.sampleBufferRenderer)
        audio = synchronizer.sampleBufferReceiver(adding: AVSampleBufferAudioRenderer())
        self.onEvent = onEvent
        // Live media never has "sufficient data" ahead of it: waiting for it
        // would hold the clock until audio arrived, and forever on a
        // broadcast without audio.
        synchronizer.delaysRateChangeUntilHasSufficientMediaData = false
        let events = video.renderingEventsAfterFinishedEnqueuing
        renderingEvents = Task { [weak self] in
            for await event in events {
                guard let engine = self else { return }
                engine.queue.async { engine.handle(event) }
            }
        }
    }

    deinit {
        renderingEvents?.cancel()
    }

    // MARK: Control (any thread)

    /// Starts watching. The audio session is `.playback` so audio carries on
    /// in PiP and behind a locked screen (D22).
    func start(_ options: ViewerOptions) {
        queue.async { [self] in
            guard viewer == nil else { return }
            // Off the main thread: activation blocks on the audio server.
            Self.activateAudioSession()
            preset = options.preset
            let now = CMClockGetTime(CMClockGetHostTimeClock())
            synchronizer.setRate(1, time: now, atHostTime: now)
            mapping = HostClockMapping.sample()
            startTimer()
            viewer = Viewer.start(options: options, listener: self)
        }
    }

    /// Stops watching and releases the core's session.
    func stop() {
        queue.async { [self] in
            viewer?.stop()
            viewer = nil
            timer?.cancel()
            timer = nil
            renderingEvents?.cancel()
            synchronizer.setRate(0, time: synchronizer.currentTime())
            flushRenderers()
        }
    }

    func setPreset(_ preset: Preset) {
        queue.async { [self] in
            self.preset = preset
            viewer?.setPreset(preset: preset)
        }
    }

    /// D22: in the background without PiP, audio only. Video items are still
    /// logged as presented (see ``PresentationLog``) but not enqueued, and
    /// the layer is flushed, since a backgrounded layer fails anyway.
    func setVideoEnabled(_ enabled: Bool) {
        queue.async { [self] in
            guard enabled != videoEnabled else { return }
            videoEnabled = enabled
            if !enabled {
                video.flush()
            }
            // H.264 restarts at the next keyframe; a decoded frame is whole.
            needsKeyframe = true
            lateness = 0
            backpressure.reset()
        }
    }

    /// A consistent snapshot, for the stats sheet and the smoke test.
    func snapshot() -> Counters {
        queue.sync { counters }
    }

    // MARK: ViewerListener (the core's thread)

    func onStatus(status: ViewerStatus) {
        queue.async { [self] in onEvent(.status(status)) }
    }

    func onStats(stats: ViewerStats) {
        queue.async { [self] in
            lastStats = stats
            onEvent(.stats(stats))
        }
    }

    func onUnsupportedCodec(codec: String) {
        queue.async { [self] in onEvent(.unsupportedCodec(codec)) }
    }

    func onFlush() {
        queue.async { [self] in flushRenderers() }
    }

    func onH264(sample: H264Sample) {
        queue.async { [self] in enqueueH264(sample) }
    }

    func onNv12(frame: Nv12Frame) {
        queue.async { [self] in enqueueNv12(frame) }
    }

    func onAudio(pcm: PcmBlock) {
        queue.async { [self] in enqueueAudio(pcm) }
    }

    // MARK: Media (queue)

    private func pts(_ presentAtMs: Double) -> CMTime {
        let host = mapping.hostTime(forPresentAtMs: presentAtMs)
        return CMSyncConvertTime(host, from: CMClockGetHostTimeClock(), to: synchronizer.timebase)
    }

    private func enqueueH264(_ sample: H264Sample) {
        if let format = sample.format {
            do {
                h264Format = try MediaSamples.h264FormatDescription(format)
            } catch {
                log.error("H.264 format description: \(String(describing: error))")
                h264Format = nil
            }
        }
        let pts = pts(sample.presentAtMs)
        presentation.record(pts: pts, timestampUs: sample.timestampUs)
        guard videoEnabled else { return }
        if needsKeyframe && !sample.keyframe {
            counters.videoDropped += 1
            return
        }
        guard let format = h264Format else {
            // Nothing to decode with until the next keyframe's parameter sets.
            counters.videoDropped += 1
            return
        }
        do {
            let buffer = try MediaSamples.h264SampleBuffer(
                data: sample.data, format: format, pts: pts,
                keyframe: sample.keyframe, displayImmediately: preset == .lowestLatency)
            needsKeyframe = false
            enqueueVideo(buffer, pts: pts)
        } catch {
            log.error("H.264 sample: \(String(describing: error))")
            counters.videoDropped += 1
            requestResync()
        }
    }

    private func enqueueNv12(_ frame: Nv12Frame) {
        let pts = pts(frame.presentAtMs)
        presentation.record(pts: pts, timestampUs: frame.timestampUs)
        guard videoEnabled else { return }
        do {
            let pixels = try pool.pixelBuffer(for: frame)
            let buffer = try MediaSamples.videoSampleBuffer(
                pixelBuffer: pixels, pts: pts, displayImmediately: preset == .lowestLatency)
            enqueueVideo(buffer, pts: pts)
        } catch {
            log.error("NV12 frame \(frame.width)x\(frame.height): \(String(describing: error))")
            counters.videoDropped += 1
        }
    }

    private func enqueueVideo(_ buffer: sending CMSampleBuffer, pts: CMTime) {
        lateness = (synchronizer.currentTime() - pts).seconds
        switch video.enqueueImmediately(CMReadySampleBuffer(unsafeBuffer: buffer)) {
        case .enqueued:
            counters.videoEnqueued += 1
        case .enqueuedWithDecodeFailures(let errors):
            // A frame that didn't decode breaks the chain after it.
            counters.videoEnqueued += 1
            log.error("video decode failed: \(String(describing: errors))")
            requestResync()
        case .cancelledDueToFlush:
            counters.videoDropped += 1
        case .cancelledDueToFlushRequiredToResume(let error), .cancelledDueToError(let error):
            counters.videoDropped += 1
            log.notice("video renderer needs a flush: \(String(describing: error))")
            requestResync()
        @unknown default:
            counters.videoDropped += 1
        }
    }

    private func enqueueAudio(_ block: PcmBlock) {
        let buffer: CMSampleBuffer
        do {
            let format: CMAudioFormatDescription
            if let cached = audioFormat, cached.rate == block.sampleRate, cached.channels == block.channels {
                format = cached.description
            } else {
                format = try MediaSamples.pcmDescription(sampleRate: block.sampleRate, channels: block.channels)
                audioFormat = (block.sampleRate, block.channels, format)
            }
            buffer = try MediaSamples.pcmSampleBuffer(block, format: format, pts: pts(block.presentAtMs))
        } catch {
            // Audio never fails a broadcast (R25 Decision 6).
            log.error("PCM block: \(String(describing: error))")
            return
        }
        switch audio.enqueueImmediately(CMReadySampleBuffer(unsafeBuffer: buffer)) {
        case .enqueued, .enqueuedWithSuggestedFlush:
            // A suggested flush (an output route change) is a moment of
            // stale audio on a live stream, not worth a gap.
            counters.audioEnqueued += 1
        case .cancelledDueToFlush:
            break
        case .cancelledDueToError(let error):
            log.error("audio renderer: \(String(describing: error))")
            audio.flush()
        @unknown default:
            break
        }
    }

    /// A rendering failure the renderer reports after the enqueue returned
    /// (e.g. decoding interrupted in the background).
    private func handle(_ event: AVSampleBufferVideoRenderer.Receiver.RenderingEvent) {
        switch event {
        case .didFailToDecode(let errors):
            log.error("video decode failed: \(String(describing: errors))")
        case .requiresFlushToResumeDecoding(let error), .failed(let error):
            log.notice("video renderer needs a flush: \(String(describing: error))")
        @unknown default:
            break
        }
        guard viewer != nil, videoEnabled else { return }
        requestResync()
    }

    private func flushRenderers() {
        video.flush()
        audio.flush()
        presentation.removeAll()
        lastReported = nil
        needsKeyframe = true
        lateness = 0
        backpressure.reset()
        counters.flushes += 1
        // A free moment to re-pair the clocks.
        mapping = HostClockMapping.sample()
    }

    /// Asks the core to resync; its flush follows, but the renderers are
    /// flushed now so nothing more from the stuck queue is shown meanwhile.
    private func requestResync() {
        counters.rendererResyncs += 1
        flushRenderers()
        viewer?.resync()
    }

    // MARK: The tick (queue)

    /// How often the frame on screen is reported: the SPA's 16 ms reorder
    /// tick. The core reads the report against 2 × offset, so its staleness
    /// eats into a margin that is only one offset wide.
    static let tickInterval: Double = 0.016

    private func startTimer() {
        let timer = DispatchSource.makeTimerSource(queue: queue)
        timer.schedule(
            deadline: .now(), repeating: Self.tickInterval, leeway: .milliseconds(2))
        timer.setEventHandler { [weak self] in self?.tick() }
        timer.resume()
        self.timer = timer
    }

    private func tick() {
        let now = synchronizer.currentTime()
        ticks += 1
        if ticks % 300 == 0 {
            // Every 5 s, for `log stream` in the Simulator (phase S evidence).
            log.notice("""
                player video=\(self.counters.videoEnqueued) audio=\(self.counters.audioEnqueued) \
                dropped=\(self.counters.videoDropped) resyncs=\(self.counters.rendererResyncs) \
                flushes=\(self.counters.flushes) videoEnabled=\(self.videoEnabled) \
                offsetMs=\(self.lastStats?.offsetMs ?? -1) dropsToLive=\(self.lastStats?.dropsToLive ?? 0)
                """)
        }
        if let shown = presentation.onScreen(at: now), shown != lastReported {
            lastReported = shown
            viewer?.presented(timestampUs: shown)
        }
        guard videoEnabled else { return }
        if backpressure.observe(
            lateness: lateness,
            pending: presentation.pending(after: now),
            now: CMClockGetTime(CMClockGetHostTimeClock()).seconds
        ) {
            log.notice("video renderer backpressure (late \(self.lateness) s): resync")
            requestResync()
        }
    }

    // MARK: Audio session (D22)

    private static func activateAudioSession() {
        let session = AVAudioSession.sharedInstance()
        do {
            try session.setCategory(.playback, mode: .moviePlayback)
            try session.setActive(true)
        } catch {
            Logger(subsystem: "fi.ioio.gawk", category: "player")
                .error("audio session: \(String(describing: error))")
        }
    }
}
