#if DEBUG
import CoreMedia
import CoreVideo
import Foundation

/// D27's synthetic broadcast source, debug builds only: what phase S
/// broadcasts in the Simulator, where ScreenCaptureKit doesn't exist (V-12).
///
/// Frames come in **panel** orientation (a portrait phone panel, 1320 ×
/// 2868) with an orientation, as iOS 27's capture delivers them, and turn a
/// quarter every `rotateEvery` seconds, so the rotation path (D9) runs
/// exactly as on a device. The content is a moving luma ramp with a frame
/// counter band; the audio a 440 Hz tone, 48 kHz stereo float, in 20 ms
/// buffers. Both are stamped on the host clock like capture's.
final class TestBroadcastSource: @unchecked Sendable {
    private let sink: MediaSink
    private let queue = DispatchQueue(label: "gawk.test-source")
    private var timer: DispatchSourceTimer?
    private var frame: UInt64 = 0
    private var pool: CVPixelBufferPool?
    private var phase: Double = 0
    private var audioFormat: CMAudioFormatDescription?
    let width = 1320
    let height = 2868
    let fps = 30
    let rotateEvery: UInt64

    init(sink: MediaSink, rotateEvery seconds: UInt64 = 10) {
        self.sink = sink
        rotateEvery = seconds
    }

    func start() {
        let attrs: [String: Any] = [
            kCVPixelBufferPixelFormatTypeKey as String: kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange,
            kCVPixelBufferWidthKey as String: width,
            kCVPixelBufferHeightKey as String: height,
            kCVPixelBufferIOSurfacePropertiesKey as String: [String: Any](),
        ]
        CVPixelBufferPoolCreate(nil, nil, attrs as CFDictionary, &pool)
        var asbd = AudioStreamBasicDescription(
            mSampleRate: 48_000,
            mFormatID: kAudioFormatLinearPCM,
            mFormatFlags: kAudioFormatFlagIsFloat | kAudioFormatFlagIsPacked,
            mBytesPerPacket: 8, mFramesPerPacket: 1, mBytesPerFrame: 8,
            mChannelsPerFrame: 2, mBitsPerChannel: 32, mReserved: 0
        )
        CMAudioFormatDescriptionCreate(
            allocator: nil, asbd: &asbd, layoutSize: 0, layout: nil,
            magicCookieSize: 0, magicCookie: nil, extensions: nil,
            formatDescriptionOut: &audioFormat
        )
        let t = DispatchSource.makeTimerSource(queue: queue)
        t.schedule(deadline: .now(), repeating: .milliseconds(1000 / fps))
        t.setEventHandler { [weak self] in self?.tick() }
        t.resume()
        timer = t
    }

    var isRunning: Bool { timer != nil }

    func stop() {
        timer?.cancel()
        timer = nil
    }

    /// The rotation this frame needs: a quarter turn more every
    /// `rotateEvery` seconds, as a phone turned by hand.
    func rotation(at frame: UInt64) -> FrameRotation {
        switch (frame / (rotateEvery * UInt64(fps))) % 4 {
        case 0: .r0
        case 1: .r90
        case 2: .r180
        default: .r270
        }
    }

    private func tick() {
        let now = CMClockGetTime(CMClockGetHostTimeClock())
        if let pixels = makeFrame() {
            sink.video(pixels, rotation: rotation(at: frame), pts: now, status: 0)
        }
        // Audio: one frame interval's worth, as 20 ms-ish buffers.
        if let sample = makeTone(frames: 48_000 / fps, pts: now) {
            sink.audio(sample, pts: now)
        }
        frame += 1
    }

    private func makeFrame() -> CVPixelBuffer? {
        guard let pool else { return nil }
        var out: CVPixelBuffer?
        CVPixelBufferPoolCreatePixelBuffer(nil, pool, &out)
        guard let pb = out else { return nil }
        CVPixelBufferLockBaseAddress(pb, [])
        defer { CVPixelBufferUnlockBaseAddress(pb, []) }
        let shift = Int(frame % 256)
        for plane in 0..<2 {
            guard let base = CVPixelBufferGetBaseAddressOfPlane(pb, plane) else { continue }
            let stride = CVPixelBufferGetBytesPerRowOfPlane(pb, plane)
            let rows = CVPixelBufferGetHeightOfPlane(pb, plane)
            let bytes = base.assumingMemoryBound(to: UInt8.self)
            let bandTop = (Int(frame) * 8) % rows
            for r in 0..<rows {
                let row = bytes + r * stride
                if plane == 0 {
                    // A ramp that moves, and a bright band whose position
                    // counts frames, so motion and order both show. One
                    // memset per row: a debug build fills a 1320 × 2868
                    // frame per pixel at ~3 fps.
                    let band = r >= bandTop && r < bandTop + 24
                    memset(row, band ? 235 : Int32(16 + (r + shift) % 200), stride)
                } else {
                    memset(row, 128, stride)
                }
            }
        }
        return pb
    }

    private func makeTone(frames: Int, pts: CMTime) -> CMSampleBuffer? {
        guard let audioFormat else { return nil }
        var samples = [Float](repeating: 0, count: frames * 2)
        let step = 2 * Double.pi * 440 / 48_000
        for i in 0..<frames {
            let v = Float(sin(phase) * 0.25)
            samples[2 * i] = v
            samples[2 * i + 1] = v
            phase += step
        }
        phase = phase.truncatingRemainder(dividingBy: 2 * .pi)
        let byteCount = samples.count * MemoryLayout<Float>.size
        var block: CMBlockBuffer?
        CMBlockBufferCreateWithMemoryBlock(
            allocator: nil, memoryBlock: nil, blockLength: byteCount,
            blockAllocator: nil, customBlockSource: nil, offsetToData: 0,
            dataLength: byteCount, flags: 0, blockBufferOut: &block
        )
        guard let block else { return nil }
        samples.withUnsafeBytes { raw in
            _ = CMBlockBufferReplaceDataBytes(
                with: raw.baseAddress!, blockBuffer: block, offsetIntoDestination: 0, dataLength: byteCount
            )
        }
        var sample: CMSampleBuffer?
        CMAudioSampleBufferCreateReadyWithPacketDescriptions(
            allocator: nil, dataBuffer: block, formatDescription: audioFormat,
            sampleCount: frames, presentationTimeStamp: pts,
            packetDescriptions: nil, sampleBufferOut: &sample
        )
        return sample
    }
}
#endif
