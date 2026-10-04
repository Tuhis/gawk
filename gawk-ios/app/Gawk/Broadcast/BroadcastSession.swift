import CoreMedia
import Foundation
import Observation

/// What the Broadcast screen shows (docs/67 D7, D18).
enum BroadcastPhase: Equatable {
    case idle
    case connecting
    case live(code: String, joinLink: String)
    case resuming(attempt: UInt32)
    case ended(reason: String?)
}

/// One broadcast as the UI sees it: the core's `Broadcaster` behind a
/// main-actor model. Capture (ScreenCaptureKit on a device, D27's test
/// source in debug builds) feeds it frames and audio from its own queues.
@MainActor
@Observable
final class BroadcastSession {
    private(set) var phase: BroadcastPhase = .idle
    private(set) var viewerCount: UInt32 = 0
    private(set) var roomText: String?
    private(set) var failure: String?

    /// The core object; capture threads read it through `media`.
    @ObservationIgnored private var broadcaster: Broadcaster?
    @ObservationIgnored let media = MediaSink()
    @ObservationIgnored private let identity: IdentityStore

    init(identity: IdentityStore) {
        self.identity = identity
    }

    var isActive: Bool {
        switch phase {
        case .idle, .ended: false
        default: true
        }
    }

    /// Starts publishing. A stored identity for this relay is reclaimed
    /// (R17), so a restart within the grace keeps the code (G6).
    func start(
        relayURL: String, secret: String, quality: Quality, room: String,
        nickname: String, telemetry: Bool, insecure: Bool
    ) {
        guard !isActive else { return }
        let stored = identity.load(relay: relayURL)
        let options = BroadcastOptions(
            relayUrl: relayURL,
            publishSecret: secret,
            broadcastId: stored?.code ?? "",
            resumeTokenHex: stored?.token ?? "",
            quality: quality,
            roomCode: room,
            roomAttachSecret: "",
            nickname: nickname,
            insecure: insecure
        )
        _ = telemetry // reports land with the broadcaster's telemetry (IO7)
        failure = nil
        phase = .connecting
        let listener = Listener(session: self, relay: relayURL, identity: identity)
        let b = Broadcaster.start(options: options, listener: listener)
        broadcaster = b
        media.attach(b)
    }

    func stop() {
        media.attach(nil)
        broadcaster?.stop()
        broadcaster = nil
    }

    /// Where frames went so far (the live status and diagnostics).
    func counters() -> BroadcastCounters? {
        broadcaster?.counters()
    }

    /// D19: the network path changed while live.
    func pathChanged() {
        broadcaster?.pathChanged()
    }

    fileprivate func apply(_ status: BroadcastStatus) {
        switch status {
        case .connecting: phase = .connecting
        case .live(let code, let joinLink): phase = .live(code: code, joinLink: joinLink)
        case .resuming(let attempt): phase = .resuming(attempt: attempt)
        case .ended(let error, let reclaimStatus):
            media.attach(nil)
            broadcaster = nil
            phase = .ended(reason: Self.reason(error: error, reclaimStatus: reclaimStatus))
        }
    }

    fileprivate func setViewers(_ n: UInt32) { viewerCount = n }
    fileprivate func setRoom(_ text: String) { roomText = text }
    fileprivate func setFailure(_ text: String) { failure = text }

    /// The user-facing sentence for an end (docs/40 statuses, R17).
    static func reason(error: String?, reclaimStatus: UInt16?) -> String? {
        switch reclaimStatus {
        case 401: return "The server refused the publish secret."
        case 404: return "The code expired; start again for a new one."
        case 429: return "The server is full right now."
        case 451: return "An operator ended this broadcast."
        default: return error
        }
    }

    /// The core's callbacks, hopping to the main actor.
    private final class Listener: BroadcastListener, @unchecked Sendable {
        private weak var session: BroadcastSession?
        private let relay: String
        private let identity: IdentityStore

        @MainActor init(session: BroadcastSession, relay: String, identity: IdentityStore) {
            self.session = session
            self.relay = relay
            self.identity = identity
        }

        func onStatus(status: BroadcastStatus) {
            Task { @MainActor [weak session] in session?.apply(status) }
        }

        func onIdentity(code: String, resumeTokenHex: String) {
            identity.save(relay: relay, code: code, token: resumeTokenHex)
        }

        func onViewerCount(count: UInt32) {
            Task { @MainActor [weak session] in session?.setViewers(count) }
        }

        func onRoom(text: String) {
            Task { @MainActor [weak session] in session?.setRoom(text) }
        }

        func onFailure(text: String) {
            Task { @MainActor [weak session] in session?.setFailure(text) }
        }
    }
}

/// Where capture pushes media, from any queue. Holds the live
/// `Broadcaster`, if any, and the converter that makes frames upright.
final class MediaSink: @unchecked Sendable {
    private let lock = NSLock()
    private var broadcaster: Broadcaster?
    private let converter = FrameConverter()

    func attach(_ b: Broadcaster?) {
        lock.withLock { broadcaster = b }
    }

    var isLive: Bool { lock.withLock { broadcaster != nil } }

    /// One captured frame: `rotation` is what it needs to be upright
    /// (`nil`: unknown, keep the last), `pts` on the host clock, `status`
    /// the raw `SCFrameStatus` (0 = complete).
    func video(_ buffer: CVPixelBuffer, rotation: FrameRotation?, pts: CMTime, status: Int) {
        lock.lock()
        defer { lock.unlock() }
        guard let b = broadcaster else { return }
        let pts100 = Self.ticks(pts)
        let plan = b.plan(
            width: UInt32(CVPixelBufferGetWidth(buffer)),
            height: UInt32(CVPixelBufferGetHeight(buffer)),
            rotation: rotation,
            pts100ns: pts100
        )
        guard status == 0 else {
            // Status-only frames still teach the pipeline (D7's suspension).
            b.pushVideo(pixelBuffer: pixelBufferHandle(buffer), pts100ns: pts100, status: Int64(status))
            return
        }
        guard let upright = converter.convert(buffer, plan: plan) else { return }
        withExtendedLifetime(upright) {
            b.pushVideo(pixelBuffer: pixelBufferHandle(upright), pts100ns: pts100, status: 0)
        }
    }

    /// One audio buffer with its ASBD (D11: the format is read, never
    /// assumed).
    func audio(_ sample: CMSampleBuffer, pts: CMTime) {
        guard let b = lock.withLock({ broadcaster }) else { return }
        guard let desc = CMSampleBufferGetFormatDescription(sample),
            let asbd = CMAudioFormatDescriptionGetStreamBasicDescription(desc)?.pointee
        else { return }
        let planar = asbd.mFormatFlags & kAudioFormatFlagIsNonInterleaved != 0
        let channels = Int(asbd.mChannelsPerFrame)
        let bufferCount = planar ? channels : 1
        var list = AudioBufferList.allocate(maximumBuffers: bufferCount)
        defer { list.unsafeMutablePointer.deallocate() }
        var block: CMBlockBuffer?
        let status = CMSampleBufferGetAudioBufferListWithRetainedBlockBuffer(
            sample,
            bufferListSizeNeededOut: nil,
            bufferListOut: list.unsafeMutablePointer,
            bufferListSize: AudioBufferList.sizeInBytes(maximumBuffers: bufferCount),
            blockBufferAllocator: nil,
            blockBufferMemoryAllocator: nil,
            flags: kCMSampleBufferFlag_AudioBufferList_Assure16ByteAlignment,
            blockBufferOut: &block
        )
        guard status == noErr else { return }
        let buffers: [Data] = list.map { buf in
            guard let p = buf.mData else { return Data() }
            return Data(bytes: p, count: Int(buf.mDataByteSize))
        }
        b.pushAudio(
            format: AudioDescription(
                formatId: asbd.mFormatID,
                formatFlags: asbd.mFormatFlags,
                sampleRate: asbd.mSampleRate,
                channels: asbd.mChannelsPerFrame,
                bitsPerChannel: asbd.mBitsPerChannel
            ),
            buffers: buffers,
            frames: UInt32(CMSampleBufferGetNumSamples(sample)),
            pts100ns: Self.ticks(pts)
        )
    }

    /// The capture resumed after a pause: re-prime with an IDR.
    func forceIDR() {
        lock.withLock { broadcaster }?.forceIdr()
    }

    static func ticks(_ t: CMTime) -> Int64 {
        guard t.isValid, t.timescale > 0 else { return hostTime100ns() }
        return Int64((Double(t.value) / Double(t.timescale) * 10_000_000).rounded())
    }
}
