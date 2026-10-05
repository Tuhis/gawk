@testable import Gawk
import CoreMedia
import CoreVideo
import VideoToolbox
import XCTest

/// The player's sample-buffer builders (docs/67 D15, D16), on real bytes.
final class MediaSamplesTests: XCTestCase {
    // MARK: H.264

    /// The IO4 fixture's NAL units, split from Annex-B.
    private func fixtureNals() throws -> [Data] {
        let url = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .appendingPathComponent("../../rust/crates/viewer/tests/fixtures/h264-320x240.h264")
            .standardizedFileURL
        return annexBNals(try Data(contentsOf: url))
    }

    private func annexBNals(_ b: Data) -> [Data] {
        let bytes = [UInt8](b)
        var starts: [(at: Int, len: Int)] = []
        var i = 0
        while i + 3 <= bytes.count {
            if bytes[i] == 0, bytes[i + 1] == 0, bytes[i + 2] == 1 {
                starts.append((i + 3, 3))
                i += 3
            } else {
                i += 1
            }
        }
        var nals: [Data] = []
        for (n, s) in starts.enumerated() {
            var end = n + 1 < starts.count ? starts[n + 1].at - 3 : bytes.count
            // A 4-byte start code leaves its leading zero on the previous NAL.
            while end > s.at, bytes[end - 1] == 0 { end -= 1 }
            nals.append(Data(bytes[s.at..<end]))
        }
        return nals
    }

    private func nalType(_ nal: Data) -> UInt8 { (nal.first ?? 0) & 0x1f }

    private func lengthPrefixed(_ nals: [Data]) -> Data {
        var out = Data()
        for nal in nals {
            var len = UInt32(nal.count).bigEndian
            out.append(Data(bytes: &len, count: 4))
            out.append(nal)
        }
        return out
    }

    private func fixtureFormat() throws -> H264Format {
        let nals = try fixtureNals()
        let sps = try XCTUnwrap(nals.first { nalType($0) == 7 })
        let pps = try XCTUnwrap(nals.first { nalType($0) == 8 })
        return H264Format(sps: [sps], pps: [pps], nalLengthSize: 4)
    }

    func testTheFixturesParameterSetsDescribeA320x240Stream() throws {
        let desc = try MediaSamples.h264FormatDescription(fixtureFormat())
        XCTAssertEqual(CMFormatDescriptionGetMediaSubType(desc), kCMVideoCodecType_H264)
        let dims = CMVideoFormatDescriptionGetDimensions(desc)
        XCTAssertEqual(dims.width, 320)
        XCTAssertEqual(dims.height, 240)
        var count = 0
        var nalHeader: Int32 = 0
        CMVideoFormatDescriptionGetH264ParameterSetAtIndex(
            desc, parameterSetIndex: 0, parameterSetPointerOut: nil, parameterSetSizeOut: nil,
            parameterSetCountOut: &count, nalUnitHeaderLengthOut: &nalHeader)
        XCTAssertEqual(count, 2)
        XCTAssertEqual(nalHeader, 4)
    }

    func testMissingParameterSetsAreRefused() {
        XCTAssertThrowsError(try MediaSamples.h264FormatDescription(
            H264Format(sps: [], pps: [Data([0x68])], nalLengthSize: 4))) {
            XCTAssertEqual($0 as? MediaSampleError, .noParameterSets)
        }
    }

    func testAnH264SampleCarriesItsPtsAndSyncAttachments() throws {
        let desc = try MediaSamples.h264FormatDescription(fixtureFormat())
        let data = Data([0, 0, 0, 2, 0x41, 0x9a])
        let pts = CMTime(value: 123_456, timescale: 1000)

        let delta = try MediaSamples.h264SampleBuffer(
            data: data, format: desc, pts: pts, keyframe: false, displayImmediately: false)
        XCTAssertEqual(delta.presentationTimeStamp, pts)
        XCTAssertEqual(delta.numSamples, 1)
        XCTAssertEqual(delta.totalSampleSize, data.count)
        XCTAssertEqual(delta.sampleAttachments[0][.notSync] as? Bool, true)
        XCTAssertNil(delta.sampleAttachments[0][.displayImmediately])

        let key = try MediaSamples.h264SampleBuffer(
            data: data, format: desc, pts: pts, keyframe: true, displayImmediately: true)
        XCTAssertNil(key.sampleAttachments[0][.notSync])
        XCTAssertEqual(key.sampleAttachments[0][.displayImmediately] as? Bool, true)
        let bytes = try XCTUnwrap(key.dataBuffer).dataBytes()
        XCTAssertEqual(bytes, data)
    }

    /// The fixture's first access unit, built the way the player builds it,
    /// decodes in VideoToolbox to a 320×240 picture: the sample buffer is
    /// what a decoder expects, not just well-formed.
    func testTheFixturesKeyframeDecodes() throws {
        let nals = try fixtureNals()
        let format = try fixtureFormat()
        let desc = try MediaSamples.h264FormatDescription(format)
        // The first access unit: everything up to the second AUD, minus the
        // AUD and parameter sets, which the format description carries.
        let au = nals.drop(while: { nalType($0) == 9 }).prefix(while: { nalType($0) != 9 })
            .filter { ![7, 8, 9].contains(nalType($0)) }
        XCTAssertTrue(au.contains { nalType($0) == 5 }, "the first access unit is an IDR")
        let sample = try MediaSamples.h264SampleBuffer(
            data: lengthPrefixed(Array(au)), format: desc, pts: .zero,
            keyframe: true, displayImmediately: false)

        var session: VTDecompressionSession?
        XCTAssertEqual(VTDecompressionSessionCreate(
            allocator: nil, formatDescription: desc, decoderSpecification: nil,
            imageBufferAttributes: nil, outputCallback: nil, decompressionSessionOut: &session), noErr)
        let decoder = try XCTUnwrap(session)
        defer { VTDecompressionSessionInvalidate(decoder) }
        nonisolated(unsafe) var decoded: (OSStatus, Int, Int)?
        let status = VTDecompressionSessionDecodeFrame(
            decoder, sampleBuffer: sample, flags: [], infoFlagsOut: nil
        ) { status, _, image, _, _ in
            decoded = (status, image.map(CVPixelBufferGetWidth) ?? 0, image.map(CVPixelBufferGetHeight) ?? 0)
        }
        XCTAssertEqual(status, noErr)
        VTDecompressionSessionWaitForAsynchronousFrames(decoder)
        let (decodeStatus, width, height) = try XCTUnwrap(decoded)
        XCTAssertEqual(decodeStatus, noErr)
        XCTAssertEqual(width, 320)
        XCTAssertEqual(height, 240)
    }

    // MARK: NV12

    /// A 5×3 frame (odd both ways) with padded source strides: every byte
    /// lands in its plane at the destination's stride, and chroma has
    /// ceil(3 / 2) = 2 rows of ceil(5 / 2) = 3 Cb/Cr pairs.
    func testNv12LandsInBothPlanesAtTheBuffersStrides() throws {
        let (w, h, yStride, uvStride) = (5, 3, 8, 7)
        let y = Data((0..<(yStride * h)).map { UInt8($0) })
        let uv = Data((0..<(uvStride * 2)).map { UInt8(100 + $0) })
        let frame = Nv12Frame(
            width: UInt32(w), height: UInt32(h), y: y, yStride: UInt32(yStride),
            uv: uv, uvStride: UInt32(uvStride), timestampUs: 1, presentAtMs: 0)

        let pool = Nv12PixelBufferPool()
        let buffer = try pool.pixelBuffer(for: frame)
        XCTAssertEqual(CVPixelBufferGetPixelFormatType(buffer), kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange)
        XCTAssertEqual(CVPixelBufferGetWidth(buffer), w)
        XCTAssertEqual(CVPixelBufferGetHeight(buffer), h)
        XCTAssertNotNil(CVPixelBufferGetIOSurface(buffer), "IOSurface-backed (D15)")
        XCTAssertEqual(CVPixelBufferGetPlaneCount(buffer), 2)
        XCTAssertEqual(CVPixelBufferGetHeightOfPlane(buffer, 1), 2)

        CVPixelBufferLockBaseAddress(buffer, .readOnly)
        defer { CVPixelBufferUnlockBaseAddress(buffer, .readOnly) }
        func row(_ plane: Int, _ r: Int, _ n: Int) throws -> [UInt8] {
            let base = try XCTUnwrap(CVPixelBufferGetBaseAddressOfPlane(buffer, plane))
            let stride = CVPixelBufferGetBytesPerRowOfPlane(buffer, plane)
            let p = (base + r * stride).assumingMemoryBound(to: UInt8.self)
            return Array(UnsafeBufferPointer(start: p, count: n))
        }
        for r in 0..<h {
            XCTAssertEqual(try row(0, r, w), Array(y[(r * yStride)..<(r * yStride + w)]), "luma row \(r)")
        }
        for r in 0..<2 {
            XCTAssertEqual(try row(1, r, 6), Array(uv[(r * uvStride)..<(r * uvStride + 6)]), "chroma row \(r)")
        }
    }

    func testThePoolFollowsTheFramesOwnSize() throws {
        let pool = Nv12PixelBufferPool()
        func frame(_ w: Int, _ h: Int) -> Nv12Frame {
            Nv12Frame(
                width: UInt32(w), height: UInt32(h), y: Data(count: w * h), yStride: UInt32(w),
                uv: Data(count: w * h / 2), uvStride: UInt32(w), timestampUs: 0, presentAtMs: 0)
        }
        let landscape = try pool.pixelBuffer(for: frame(320, 240))
        XCTAssertEqual(CVPixelBufferGetWidth(landscape), 320)
        // A rotation (D9): the next frame is portrait, and so is its buffer.
        let portrait = try pool.pixelBuffer(for: frame(240, 320))
        XCTAssertEqual(CVPixelBufferGetWidth(portrait), 240)
        XCTAssertEqual(CVPixelBufferGetHeight(portrait), 320)
        XCTAssertEqual(pool.width, 240)
        XCTAssertEqual(pool.height, 320)

        let sample = try MediaSamples.videoSampleBuffer(
            pixelBuffer: portrait, pts: CMTime(value: 5, timescale: 1), displayImmediately: true)
        let dims = try CMVideoFormatDescriptionGetDimensions(XCTUnwrap(sample.formatDescription))
        XCTAssertEqual(dims.width, 240)
        XCTAssertEqual(dims.height, 320)
        XCTAssertEqual(sample.presentationTimeStamp, CMTime(value: 5, timescale: 1))
        XCTAssertEqual(sample.sampleAttachments[0][.displayImmediately] as? Bool, true)
        XCTAssertNil(sample.sampleAttachments[0][.notSync])
    }

    func testAShortPlaneIsRefusedNotRead() {
        let frame = Nv12Frame(
            width: 4, height: 4, y: Data(count: 15), yStride: 4,
            uv: Data(count: 8), uvStride: 4, timestampUs: 0, presentAtMs: 0)
        XCTAssertThrowsError(try Nv12PixelBufferPool().pixelBuffer(for: frame)) {
            XCTAssertEqual($0 as? MediaSampleError, .shortPlane(plane: 0, have: 15, need: 16))
        }
    }

    // MARK: PCM

    func testAPcmBlockIsOneInterleavedFloatSampleBuffer() throws {
        // One 20 ms Opus packet at 48 kHz stereo: 960 frames.
        let block = PcmBlock(
            sampleRate: 48_000, channels: 2,
            samples: (0..<1920).map { Float($0) / 1920 }, presentAtMs: 0)
        let format = try MediaSamples.pcmDescription(sampleRate: 48_000, channels: 2)
        let asbd = try XCTUnwrap(format.audioStreamBasicDescription)
        XCTAssertEqual(asbd.mSampleRate, 48_000)
        XCTAssertEqual(asbd.mFormatID, kAudioFormatLinearPCM)
        XCTAssertEqual(asbd.mFormatFlags, kAudioFormatFlagIsFloat | kAudioFormatFlagIsPacked)
        XCTAssertEqual(asbd.mChannelsPerFrame, 2)
        XCTAssertEqual(asbd.mBitsPerChannel, 32)
        XCTAssertEqual(asbd.mBytesPerFrame, 8)
        XCTAssertEqual(asbd.mFramesPerPacket, 1)

        let pts = CMTime(value: 42, timescale: 1000)
        let sample = try MediaSamples.pcmSampleBuffer(block, format: format, pts: pts)
        XCTAssertEqual(sample.numSamples, 960)
        XCTAssertEqual(sample.presentationTimeStamp, pts)
        XCTAssertEqual(sample.duration, CMTime(value: 960, timescale: 48_000))
        XCTAssertEqual(sample.totalSampleSize, 1920 * 4)
        let bytes = try XCTUnwrap(sample.dataBuffer).dataBytes()
        let floats = bytes.withUnsafeBytes { Array($0.bindMemory(to: Float.self)) }
        XCTAssertEqual(floats, block.samples)
    }

    func testARaggedPcmBlockIsRefused() throws {
        let format = try MediaSamples.pcmDescription(sampleRate: 48_000, channels: 2)
        let block = PcmBlock(sampleRate: 48_000, channels: 2, samples: [0, 0, 0], presentAtMs: 0)
        XCTAssertThrowsError(try MediaSamples.pcmSampleBuffer(block, format: format, pts: .zero))
    }
}
