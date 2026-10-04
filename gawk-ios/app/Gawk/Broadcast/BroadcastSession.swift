import CoreMedia
import Foundation
import Observation

/// What the Broadcast screen shows (docs/67 D7, D18).
enum BroadcastPhase: Equatable {
    case idle
    case connecting
    case live(code: String, joinLink: String)
    case resuming(attempt: UInt32)
    /// Stop was asked for; the core is winding the session down.
    case stopping
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
    /// Called when the broadcast ends, whoever ended it: capture stops.
    @ObservationIgnored var onEnded: (() -> Void)?
    @ObservationIgnored private let identity: IdentityStore
    /// The relay the current broadcast publishes to, whose identity it holds.
    @ObservationIgnored private var relay: String?
    /// Which broadcast is current: a stopped one's late reports are dropped.
    @ObservationIgnored private var generation = 0
    /// How long a stop waits for the core to confirm before the screen
    /// gives up on it and says so.
    @ObservationIgnored var stopGrace: Duration = .seconds(5)

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
            insecure: insecure,
            telemetry: telemetry
        )
        failure = nil
        phase = .connecting
        relay = relayURL
        generation += 1
        let listener = Listener(
            session: self, generation: generation, relay: relayURL, identity: identity
        )
        let b = Broadcaster.start(options: options, listener: listener)
        broadcaster = b
        media.attach(b)
    }

    /// Ends the broadcast. The screen leaves the live state at once
    /// (`.stopping`); `.ended` follows when the core has closed the session,
    /// or after `stopGrace` with a reason if it never confirms.
    func stop() {
        media.attach(nil)
        broadcaster?.stop()
        broadcaster = nil
        switch phase {
        case .idle, .ended, .stopping: return
        default: break
        }
        phase = .stopping
        let stopped = generation
        Task { [weak self, stopGrace] in
            try? await Task.sleep(for: stopGrace)
            guard let self, self.generation == stopped, self.phase == .stopping else { return }
            self.finish(reason: "The app stopped broadcasting, but the server didn't confirm it.")
        }
    }

    /// Where frames went so far (the live status and diagnostics).
    func counters() -> BroadcastCounters? {
        broadcaster?.counters()
    }

    /// The capture ended on its own (the system, or an error); the reason
    /// stays on screen after the broadcast stops (D7).
    func noteCaptureEnded(_ reason: String?) {
        if let reason { failure = reason }
    }

    /// D19: the network path changed while live.
    func pathChanged() {
        broadcaster?.pathChanged()
    }

    func apply(_ status: BroadcastStatus) {
        switch status {
        case .ended(let error, let reclaimStatus):
            if let relay, Self.refusesIdentity(reclaimStatus) {
                identity.forgetIdentity(relay: relay)
            }
            finish(reason: Self.reason(error: error, reclaimStatus: reclaimStatus))
        // A stopping broadcast only ends: a report the core sent before it
        // saw the stop must not bring the live screen back.
        case _ where phase == .stopping: break
        case .connecting: phase = .connecting
        case .live(let code, let joinLink): phase = .live(code: code, joinLink: joinLink)
        case .resuming(let attempt): phase = .resuming(attempt: attempt)
        }
    }

    /// A report from the core's listener: only the current broadcast's count.
    fileprivate func apply(_ status: BroadcastStatus, generation: Int) {
        guard generation == self.generation else { return }
        apply(status)
    }

    private func finish(reason: String?) {
        media.attach(nil)
        broadcaster = nil
        onEnded?()
        phase = .ended(reason: reason)
    }

    fileprivate func setViewers(_ n: UInt32) { viewerCount = n }
    fileprivate func setRoom(_ text: String) { roomText = text }
    fileprivate func setFailure(_ text: String) { failure = text }

    /// A reclaim the relay will never accept: the code expired (404), the
    /// token was refused (403), the slot is taken (409) or an operator ended
    /// it (451). Forgetting it lets the next Start mint a new code. 401 is a
    /// wrong secret and 429 a full server; the identity may still be good.
    static func refusesIdentity(_ reclaimStatus: UInt16?) -> Bool {
        [403, 404, 409, 451].contains(reclaimStatus)
    }

    /// The user-facing sentence for an end (docs/40 statuses, R17).
    static func reason(error: String?, reclaimStatus: UInt16?) -> String? {
        switch reclaimStatus {
        case 401: return "The server refused the publish secret."
        case 403: return "The server wouldn't resume this broadcast; start again for a new code."
        case 404: return "The code expired; start again for a new one."
        case 409: return "This code is live elsewhere; start again for a new one."
        case 429: return "The server is full right now."
        case 451: return "An operator ended this broadcast."
        default: return error
        }
    }

    /// The core's callbacks, hopping to the main actor.
    private final class Listener: BroadcastListener, @unchecked Sendable {
        private weak var session: BroadcastSession?
        private let generation: Int
        private let relay: String
        private let identity: IdentityStore

        @MainActor init(session: BroadcastSession, generation: Int, relay: String, identity: IdentityStore) {
            self.session = session
            self.generation = generation
            self.relay = relay
            self.identity = identity
        }

        func onStatus(status: BroadcastStatus) {
            let generation = generation
            Task { @MainActor [weak session] in session?.apply(status, generation: generation) }
        }

        func onIdentity(code: String, resumeTokenHex: String) {
            identity.save(relay: relay, code: code, token: resumeTokenHex)
        }

        func onViewerCount(count: UInt32) {
            let generation = generation
            Task { @MainActor [weak session] in
                guard let session, session.generation == generation else { return }
                session.setViewers(count)
            }
        }

        func onRoom(text: String) {
            let generation = generation
            Task { @MainActor [weak session] in
                guard let session, session.generation == generation else { return }
                session.setRoom(text)
            }
        }

        func onFailure(text: String) {
            let generation = generation
            Task { @MainActor [weak session] in
                guard let session, session.generation == generation else { return }
                session.setFailure(text)
            }
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
        #if DEBUG
        CaptureDiagnostics.shared.noteSource(buffer, status: status)
        #endif
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
        let upright = converter.convert(buffer, plan: plan)
        #if DEBUG
        CaptureDiagnostics.shared.noteConverted(upright)
        #endif
        guard let upright else { return }
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
