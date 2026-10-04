import CoreVideo
import XCTest
@testable import Gawk

/// The converter pools its output buffers per stage; a quarter turn needs
/// two sizes per frame, and neither may evict the other (docs/67 D9).
final class FrameConverterTests: XCTestCase {
    private func source(width: Int, height: Int) -> CVPixelBuffer {
        var out: CVPixelBuffer?
        let attrs: [String: Any] = [kCVPixelBufferIOSurfacePropertiesKey as String: [String: Any]()]
        CVPixelBufferCreate(
            nil, width, height, kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange,
            attrs as CFDictionary, &out
        )
        return out!
    }

    func testAQuarterTurnBuildsEachPoolOnce() {
        let converter = FrameConverter()
        let frame = source(width: 1920, height: 884)
        let plan = FramePlan(rotation: .r90, width: 884, height: 1920)
        for _ in 0..<10 {
            let out = converter.convert(frame, plan: plan)
            XCTAssertEqual(out.map(CVPixelBufferGetWidth), 884)
            XCTAssertEqual(out.map(CVPixelBufferGetHeight), 1920)
        }
        XCTAssertEqual(converter.poolsCreated, 2, "one scale pool and one rotate pool")
    }

    func testANewRungSizeRebuildsThePool() {
        let converter = FrameConverter()
        let frame = source(width: 1280, height: 720)
        _ = converter.convert(frame, plan: FramePlan(rotation: .r0, width: 1280, height: 720))
        _ = converter.convert(frame, plan: FramePlan(rotation: .r0, width: 1280, height: 720))
        _ = converter.convert(frame, plan: FramePlan(rotation: .r0, width: 960, height: 540))
        XCTAssertEqual(converter.poolsCreated, 2)
    }
}
