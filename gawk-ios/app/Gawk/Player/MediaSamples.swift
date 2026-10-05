import CoreMedia
import CoreVideo
import Foundation

/// Why a media item couldn't become a sample buffer. The player drops the
/// item (and, for video, resyncs at the next keyframe) rather than enqueue
/// something broken: favor dropped frames over corrupted ones (docs/67 D13).
enum MediaSampleError: Error, Equatable {
    case noParameterSets
    case formatDescription(OSStatus)
    case emptySample
    case shortPlane(plane: Int, have: Int, need: Int)
    case pixelBufferPool(CVReturn)
    case pixelBuffer(CVReturn)
    case badAudio(sampleRate: UInt32, channels: UInt8, samples: Int)
}

/// Builds the `CMSampleBuffer`s the renderers take from what the core hands
/// over (docs/67 D15, D16). Pure: no renderer, no clock, so each builder is
/// unit-tested on its own.
enum MediaSamples {
    // MARK: H.264, enqueued compressed (D15)

    /// The format description for one set of parameter sets: what
    /// `AVSampleBufferVideoRenderer` decodes the following samples with.
    static func h264FormatDescription(_ format: H264Format) throws -> CMVideoFormatDescription {
        let sets = format.sps + format.pps
        guard !format.sps.isEmpty, !format.pps.isEmpty, sets.allSatisfy({ !$0.isEmpty }) else {
            throw MediaSampleError.noParameterSets
        }
        // One contiguous copy, so every pointer stays valid for the one call.
        var joined = Data()
        var offsets: [Int] = []
        for set in sets {
            offsets.append(joined.count)
            joined.append(set)
        }
        let sizes = sets.map(\.count)
        var description: CMFormatDescription?
        let status = joined.withUnsafeBytes { raw -> OSStatus in
            guard let base = raw.bindMemory(to: UInt8.self).baseAddress else {
                return kCMFormatDescriptionError_InvalidParameter
            }
            let pointers = offsets.map { UnsafePointer(base + $0) }
            return CMVideoFormatDescriptionCreateFromH264ParameterSets(
                allocator: kCFAllocatorDefault,
                parameterSetCount: sets.count,
                parameterSetPointers: pointers,
                parameterSetSizes: sizes,
                nalUnitHeaderLength: Int32(format.nalLengthSize),
                formatDescriptionOut: &description
            )
        }
        guard status == noErr, let description else {
            throw MediaSampleError.formatDescription(status)
        }
        return description
    }

    /// One compressed access unit (length-prefixed NALs) at `pts`. A
    /// non-keyframe is marked not-sync so the renderer never starts decoding
    /// on it; `displayImmediately` is the Lowest-latency preset (D15).
    static func h264SampleBuffer(
        data: Data,
        format: CMVideoFormatDescription,
        pts: CMTime,
        keyframe: Bool,
        displayImmediately: Bool
    ) throws -> CMSampleBuffer {
        guard !data.isEmpty else { throw MediaSampleError.emptySample }
        let block = try CMBlockBuffer(length: data.count, flags: .assureMemoryNow)
        try data.withUnsafeBytes { try block.replaceDataBytes(with: $0) }
        let sample = try CMSampleBuffer(
            dataBuffer: block,
            formatDescription: format,
            numSamples: 1,
            sampleTimings: [timing(pts)],
            sampleSizes: [data.count]
        )
        mark(sample, sync: keyframe, displayImmediately: displayImmediately)
        return sample
    }

    // MARK: VP8/VP9, decoded by libvpx (D15)

    /// Copies one NV12 frame into `buffer`, honouring both sides' strides.
    /// The size is the frame's own (CLAUDE.md: trust the frame in hand); the
    /// caller's pool must already be at that size.
    static func copy(_ frame: Nv12Frame, into buffer: CVPixelBuffer) throws {
        let width = Int(frame.width)
        let height = Int(frame.height)
        let chromaRows = (height + 1) / 2
        let yStride = Int(frame.yStride)
        let uvStride = Int(frame.uvStride)
        // A row carries `width` luma bytes and `2 × ceil(width / 2)` chroma.
        let yRow = width
        let uvRow = 2 * ((width + 1) / 2)
        let yNeed = yStride * (height - 1) + yRow
        let uvNeed = uvStride * (chromaRows - 1) + uvRow
        guard yStride >= yRow, frame.y.count >= yNeed else {
            throw MediaSampleError.shortPlane(plane: 0, have: frame.y.count, need: yNeed)
        }
        guard uvStride >= uvRow, frame.uv.count >= uvNeed else {
            throw MediaSampleError.shortPlane(plane: 1, have: frame.uv.count, need: uvNeed)
        }
        CVPixelBufferLockBaseAddress(buffer, [])
        defer { CVPixelBufferUnlockBaseAddress(buffer, []) }
        copyPlane(frame.y, stride: yStride, rowBytes: yRow, rows: height, into: buffer, plane: 0)
        copyPlane(frame.uv, stride: uvStride, rowBytes: uvRow, rows: chromaRows, into: buffer, plane: 1)
    }

    private static func copyPlane(
        _ source: Data, stride: Int, rowBytes: Int, rows: Int,
        into buffer: CVPixelBuffer, plane: Int
    ) {
        guard let dest = CVPixelBufferGetBaseAddressOfPlane(buffer, plane) else { return }
        let destStride = CVPixelBufferGetBytesPerRowOfPlane(buffer, plane)
        let destRows = CVPixelBufferGetHeightOfPlane(buffer, plane)
        let bytes = min(rowBytes, destStride)
        source.withUnsafeBytes { src in
            guard let base = src.baseAddress else { return }
            for row in 0..<min(rows, destRows) {
                (dest + row * destStride).copyMemory(from: base + row * stride, byteCount: bytes)
            }
        }
    }

    /// A decoded frame as a sample buffer at `pts`, with a format description
    /// made from the buffer itself, so a size change carries its own.
    static func videoSampleBuffer(
        pixelBuffer: CVPixelBuffer,
        pts: CMTime,
        displayImmediately: Bool
    ) throws -> CMSampleBuffer {
        let format = try CMVideoFormatDescription(imageBuffer: pixelBuffer)
        let sample = try CMSampleBuffer(
            imageBuffer: pixelBuffer,
            formatDescription: format,
            sampleTiming: timing(pts)
        )
        mark(sample, sync: true, displayImmediately: displayImmediately)
        return sample
    }

    // MARK: Audio, decoded by libopus (D16)

    /// LPCM, interleaved native-endian `Float32`: what the core's Opus decoder
    /// writes.
    static func pcmDescription(sampleRate: UInt32, channels: UInt8) throws -> CMAudioFormatDescription {
        let bytesPerFrame = UInt32(channels) * UInt32(MemoryLayout<Float32>.size)
        let asbd = AudioStreamBasicDescription(
            mSampleRate: Float64(sampleRate),
            mFormatID: kAudioFormatLinearPCM,
            mFormatFlags: kAudioFormatFlagIsFloat | kAudioFormatFlagIsPacked,
            mBytesPerPacket: bytesPerFrame,
            mFramesPerPacket: 1,
            mBytesPerFrame: bytesPerFrame,
            mChannelsPerFrame: UInt32(channels),
            mBitsPerChannel: 32,
            mReserved: 0
        )
        return try CMAudioFormatDescription(audioStreamBasicDescription: asbd)
    }

    /// One decoded packet at `pts`; its frame count is
    /// `samples / channels`.
    static func pcmSampleBuffer(
        _ block: PcmBlock,
        format: CMAudioFormatDescription,
        pts: CMTime
    ) throws -> CMSampleBuffer {
        let channels = Int(block.channels)
        guard block.sampleRate > 0, channels > 0, !block.samples.isEmpty,
              block.samples.count % channels == 0 else {
            throw MediaSampleError.badAudio(
                sampleRate: block.sampleRate, channels: block.channels, samples: block.samples.count)
        }
        let frames = block.samples.count / channels
        let length = block.samples.count * MemoryLayout<Float32>.size
        let data = try CMBlockBuffer(length: length, flags: .assureMemoryNow)
        try block.samples.withUnsafeBytes { try data.replaceDataBytes(with: $0) }
        return try CMSampleBuffer(
            dataBuffer: data,
            formatDescription: format,
            numSamples: frames,
            presentationTimeStamp: pts,
            packetDescriptions: []
        )
    }

    // MARK: Shared

    private static func timing(_ pts: CMTime) -> CMSampleTimingInfo {
        CMSampleTimingInfo(duration: .invalid, presentationTimeStamp: pts, decodeTimeStamp: .invalid)
    }

    private static func mark(_ sample: CMSampleBuffer, sync: Bool, displayImmediately: Bool) {
        guard !sample.sampleAttachments.isEmpty else { return }
        if !sync {
            sample.sampleAttachments[0][.notSync] = true
            sample.sampleAttachments[0][.dependsOnOthers] = true
        }
        if displayImmediately {
            sample.sampleAttachments[0][.displayImmediately] = true
        }
    }
}

/// An IOSurface-backed pool of NV12 buffers at one size (docs/67 D15),
/// rebuilt whenever a frame arrives at another: a VP9 broadcaster's
/// rotation (D9) changes the size mid-stream.
final class Nv12PixelBufferPool {
    private var pool: CVPixelBufferPool?
    private(set) var width = 0
    private(set) var height = 0

    /// A filled buffer for `frame`, from a pool at the frame's own size.
    func pixelBuffer(for frame: Nv12Frame) throws -> CVPixelBuffer {
        let pool = try self.pool(width: Int(frame.width), height: Int(frame.height))
        var buffer: CVPixelBuffer?
        let status = CVPixelBufferPoolCreatePixelBuffer(kCFAllocatorDefault, pool, &buffer)
        guard status == kCVReturnSuccess, let buffer else {
            throw MediaSampleError.pixelBuffer(status)
        }
        try MediaSamples.copy(frame, into: buffer)
        return buffer
    }

    private func pool(width: Int, height: Int) throws -> CVPixelBufferPool {
        if let pool, width == self.width, height == self.height { return pool }
        let attributes: [CFString: Any] = [
            kCVPixelBufferPixelFormatTypeKey: kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange,
            kCVPixelBufferWidthKey: width,
            kCVPixelBufferHeightKey: height,
            kCVPixelBufferIOSurfacePropertiesKey: [CFString: Any](),
        ]
        var created: CVPixelBufferPool?
        let status = CVPixelBufferPoolCreate(
            kCFAllocatorDefault, nil, attributes as CFDictionary, &created)
        guard status == kCVReturnSuccess, let created else {
            throw MediaSampleError.pixelBufferPool(status)
        }
        pool = created
        self.width = width
        self.height = height
        return created
    }
}
