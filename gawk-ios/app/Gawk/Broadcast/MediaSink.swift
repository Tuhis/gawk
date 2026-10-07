import CoreMedia
import Foundation

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
        let list = AudioBufferList.allocate(maximumBuffers: bufferCount)
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
