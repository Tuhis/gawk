#if DEBUG
import CoreVideo
import Foundation

/// Debug builds only: what the capture delivers and what conversion makes
/// of it, shown on the live Broadcast screen. The first device run sent
/// black frames (2026-10-05); this tells a black capture from a black
/// conversion. Brightness is the mean luma of a 16 × 16 sample grid, read
/// every 30th frame.
final class CaptureDiagnostics: @unchecked Sendable {
    static let shared = CaptureDiagnostics()

    private let lock = NSLock()
    private var frames = 0
    private var statuses: [Int: Int] = [:]
    private var format = "-"
    private var size = "-"
    private var sourceLuma: String = "-"
    private var convertedLuma: String = "-"
    private var converted = 0
    private var dropped = 0

    func reset() {
        lock.withLock {
            frames = 0
            statuses = [:]
            format = "-"
            size = "-"
            sourceLuma = "-"
            convertedLuma = "-"
            converted = 0
            dropped = 0
        }
    }

    func noteSource(_ buffer: CVPixelBuffer, status: Int) {
        let sample = lock.withLock { () -> Bool in
            frames += 1
            statuses[status, default: 0] += 1
            format = Self.fourCC(CVPixelBufferGetPixelFormatType(buffer))
            size = "\(CVPixelBufferGetWidth(buffer))×\(CVPixelBufferGetHeight(buffer))"
            return status == 0 && frames % 30 == 1
        }
        guard sample else { return }
        let luma = Self.luma(buffer)
        lock.withLock { sourceLuma = luma }
    }

    func noteConverted(_ buffer: CVPixelBuffer?) {
        guard let buffer else {
            lock.withLock { dropped += 1 }
            return
        }
        let sample = lock.withLock { () -> Bool in
            converted += 1
            return converted % 30 == 1
        }
        guard sample else { return }
        let luma = Self.luma(buffer)
        lock.withLock { convertedLuma = luma }
    }

    var summary: String {
        lock.withLock {
            let s = statuses.sorted { $0.key < $1.key }.map { "\($0.key):\($0.value)" }.joined(separator: " ")
            return """
                capture \(format) \(size), \(frames) frames, status \(s)
                converted \(converted), dropped \(dropped)
                brightness: capture \(sourceLuma), converted \(convertedLuma)
                """
        }
    }

    private static func fourCC(_ t: OSType) -> String {
        let bytes = [24, 16, 8, 0].map { UInt8((t >> $0) & 0xFF) }
        let s = String(bytes: bytes, encoding: .ascii) ?? ""
        return s.allSatisfy({ $0.isASCII && !$0.isWhitespace || $0 == " " }) && !s.isEmpty ? "'\(s)'" : String(t)
    }

    /// Mean of a 16 × 16 grid over the luma plane (or the green channel of a
    /// packed BGRA buffer), 0-255; "no CPU access" when the buffer can't be
    /// read (a compressed format).
    private static func luma(_ b: CVPixelBuffer) -> String {
        guard CVPixelBufferLockBaseAddress(b, .readOnly) == kCVReturnSuccess else { return "lock failed" }
        defer { CVPixelBufferUnlockBaseAddress(b, .readOnly) }
        let planar = CVPixelBufferIsPlanar(b)
        let base = planar ? CVPixelBufferGetBaseAddressOfPlane(b, 0) : CVPixelBufferGetBaseAddress(b)
        guard let base else { return "no CPU access" }
        let w = planar ? CVPixelBufferGetWidthOfPlane(b, 0) : CVPixelBufferGetWidth(b)
        let h = planar ? CVPixelBufferGetHeightOfPlane(b, 0) : CVPixelBufferGetHeight(b)
        let stride = planar ? CVPixelBufferGetBytesPerRowOfPlane(b, 0) : CVPixelBufferGetBytesPerRow(b)
        let bpp = planar ? 1 : max(1, stride / max(1, w))
        let p = base.assumingMemoryBound(to: UInt8.self)
        var sum = 0
        var n = 0
        for gy in 0..<16 {
            let y = (h - 1) * gy / 15
            for gx in 0..<16 {
                let x = (w - 1) * gx / 15
                // Planar: the Y byte. Packed BGRA: the G byte.
                sum += Int(p[y * stride + x * bpp + (planar ? 0 : 1)])
                n += 1
            }
        }
        return String(sum / max(1, n))
    }
}
#endif
