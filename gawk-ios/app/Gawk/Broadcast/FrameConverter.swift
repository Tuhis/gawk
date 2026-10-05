import CoreVideo
import Foundation
import VideoToolbox

/// Turns a captured frame into what the encoder takes (docs/67 D8, D9):
/// scaled and converted to `420v` at the rung's size, then turned upright
/// when the capture arrived in panel orientation.
///
/// Two VideoToolbox sessions, because `VTPixelTransferSession` scales and
/// converts but cannot rotate and `VTPixelRotationSession` rotates but does
/// not scale: an upright capture takes one pass, a rotated one two, scaling
/// first so the rotation works on the smaller frame. Output buffers are
/// IOSurface-backed and pooled per stage, so a quarter turn's two sizes
/// never evict each other and only a stage's own size change rebuilds it.
final class FrameConverter {
    private var transfer: VTPixelTransferSession?
    private var rotation: VTPixelRotationSession?
    private var scalePool = Pool()
    private var rotatePool = Pool()
    /// Pools built so far; a steady stream builds none after its first frame.
    private(set) var poolsCreated = 0

    /// One stage's output pool and the size it was built for.
    private struct Pool {
        var width = 0
        var height = 0
        var pool: CVPixelBufferPool?
    }

    deinit {
        if let transfer { VTPixelTransferSessionInvalidate(transfer) }
        if let rotation { VTPixelRotationSessionInvalidate(rotation) }
    }

    /// `source` scaled and turned per `plan`, or `nil` when VideoToolbox
    /// refuses (the frame is then dropped: favor dropped frames).
    func convert(_ source: CVPixelBuffer, plan: FramePlan) -> CVPixelBuffer? {
        let quarterTurn = plan.rotation == .r90 || plan.rotation == .r270
        // The plan's size is upright; before a quarter turn it is transposed.
        let (w, h) = quarterTurn
            ? (Int(plan.height), Int(plan.width))
            : (Int(plan.width), Int(plan.height))
        guard let scaled = scale(source, width: w, height: h) else { return nil }
        guard plan.rotation != .r0 else { return scaled }
        return rotate(scaled, by: plan.rotation, width: Int(plan.width), height: Int(plan.height))
    }

    private func scale(_ source: CVPixelBuffer, width: Int, height: Int) -> CVPixelBuffer? {
        if transfer == nil {
            var session: VTPixelTransferSession?
            guard VTPixelTransferSessionCreate(allocator: nil, pixelTransferSessionOut: &session) == noErr
            else { return nil }
            transfer = session
        }
        guard let transfer, let out = buffer(&scalePool, width: width, height: height) else { return nil }
        guard VTPixelTransferSessionTransferImage(transfer, from: source, to: out) == noErr else {
            return nil
        }
        return out
    }

    private func rotate(
        _ source: CVPixelBuffer, by r: FrameRotation, width: Int, height: Int
    ) -> CVPixelBuffer? {
        if rotation == nil {
            var session: VTPixelRotationSession?
            guard VTPixelRotationSessionCreate(nil, &session) == noErr
            else { return nil }
            rotation = session
        }
        guard let rotation, let out = buffer(&rotatePool, width: width, height: height) else { return nil }
        let angle: CFString
        switch r {
        case .r90: angle = kVTRotation_CW90
        case .r180: angle = kVTRotation_180
        case .r270: angle = kVTRotation_CCW90
        case .r0: return source
        }
        VTSessionSetProperty(rotation, key: kVTPixelRotationPropertyKey_Rotation, value: angle)
        guard VTPixelRotationSessionRotateImage(rotation, source, out) == noErr else {
            return nil
        }
        return out
    }

    private func buffer(_ stage: inout Pool, width: Int, height: Int) -> CVPixelBuffer? {
        if stage.pool == nil || stage.width != width || stage.height != height {
            // One live size per stage: a rotation or rung change retires the
            // old pool's buffers as they come back.
            let attrs: [String: Any] = [
                kCVPixelBufferPixelFormatTypeKey as String:
                    kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange,
                kCVPixelBufferWidthKey as String: width,
                kCVPixelBufferHeightKey as String: height,
                kCVPixelBufferIOSurfacePropertiesKey as String: [String: Any](),
            ]
            var pool: CVPixelBufferPool?
            CVPixelBufferPoolCreate(nil, nil, attrs as CFDictionary, &pool)
            stage = Pool(width: width, height: height, pool: pool)
            poolsCreated += 1
        }
        guard let pool = stage.pool else { return nil }
        var out: CVPixelBuffer?
        CVPixelBufferPoolCreatePixelBuffer(nil, pool, &out)
        return out
    }
}

/// `CGImagePropertyOrientation` (what `SCStreamFrameInfoVideoOrientation`
/// carries on iOS 27) as the turn that makes the frame upright, or `nil`
/// for a value that says nothing (face up/down keep the last, D9).
func frameRotation(fromImageOrientation raw: UInt32) -> FrameRotation? {
    switch raw {
    case 1: return .r0 // up
    case 3: return .r180 // down
    case 6: return .r90 // right: the content is turned 90° counter-clockwise
    case 8: return .r270 // left
    default: return nil // mirrored variants never come from a screen
    }
}

/// A `CVPixelBuffer`'s address for the core, valid while the caller holds
/// the buffer (docs/67 D3: an opaque handle, no object model across).
func pixelBufferHandle(_ buffer: CVPixelBuffer) -> UInt64 {
    UInt64(UInt(bitPattern: Unmanaged.passUnretained(buffer).toOpaque()))
}
