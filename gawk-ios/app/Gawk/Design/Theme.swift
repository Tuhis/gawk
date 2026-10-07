import SwiftUI

/// The gawk tokens (docs/70 §3.1): the web app's, copied, not re-picked
/// (docs/60 D2), and the desktop's additions from `main.slint`'s `T`.
///
/// Every value is written exactly as its source writes it, and the comment
/// after it names that source: `scripts/check-theme.sh` reads both and fails
/// when they differ (K10). Dark only (docs/70 D2).
enum Theme {
    /// Every screen's ground; the players are pure black.
    static let bg = Color(css: "#0a0b0d") // global.css --bg
    /// Grouped lists, cards.
    static let s1 = Color(css: "#121317") // global.css --surface-1
    /// Code boxes, lists inside a sheet, the stats drawer's tiles.
    static let s2 = Color(css: "#171920") // global.css --surface-2
    /// Row icon tiles.
    static let s3 = Color(css: "#1d2029") // main.slint s3
    static let border = Color(css: "#262a33") // global.css --border
    static let borderSoft = Color(css: "#1c2028") // global.css --border-soft

    /// Text, and the primary button's fill.
    static let text = Color(css: "#e8eaed") // global.css --text
    static let muted = Color(css: "#9aa1ad") // global.css --muted
    /// Chevrons and placeholders only: it fails 4.5:1 for body text.
    static let faint = Color(css: "#626873") // global.css --faint

    static let accent = Color(css: "#6b8afe") // global.css --accent
    static let accentText = Color(css: "#aebeff") // main.slint accent-text
    static let accentSoft = Color(css: "rgba(107, 138, 254, 0.14)") // global.css --accent-soft
    static let accentLine = Color(css: "#6b8afe59") // main.slint accent-line

    static let live = Color(css: "#e5484d") // global.css --live
    static let liveSoft = Color(css: "#e5484d24") // main.slint live-soft
    static let liveText = Color(css: "#ff8589") // main.slint live-text
    static let dangerLine = Color(css: "#e5484d80") // main.slint danger-line
    static let dangerText = Color(css: "#f0686c") // main.slint danger-text

    static let ok = Color(css: "#30a46c") // global.css --ok
    static let okText = Color(css: "#8fdcb2") // main.slint ok-text

    static let warn = Color(css: "#d0a215") // global.css --warning
    static let warnSoft = Color(css: "#d0a21517") // main.slint warn-soft
    static let warnLine = Color(css: "#d0a21552") // main.slint warn-line
    static let warnText = Color(css: "#e6c14d") // main.slint warn-text

    // MARK: Motion (docs/70 §3.4)

    /// Controls over video hide after this long without a touch: the web's
    /// `CONTROL_IDLE_MS` (`ViewerScreen.tsx`, `RoomScreen.tsx`).
    static let controlIdle: Duration = .seconds(3)

    /// The web's `--dur` on `--ease`. Callers drop it under Reduce Motion.
    static let fade = Animation.timingCurve(0.2, 0, 0, 1, duration: 0.22)

    // MARK: Type (docs/70 §3.2)

    /// A code in the six boxes.
    static let codeBox = Font.system(size: 28, weight: .semibold, design: .monospaced)
    /// The code pill's code.
    static let codePill = Font.system(size: 12, weight: .semibold, design: .monospaced)
    /// A join link written out.
    static let link = Font.system(size: 13, design: .monospaced)
}

extension Color {
    /// A colour from a CSS literal as the token sources write it: `#rrggbb`,
    /// `#rrggbbaa` or `rgba(r, g, b, a)`, in sRGB. Anything else is a
    /// programming error, caught by the theme tests.
    init(css: String) {
        guard let c = CSSColor(css) else {
            assertionFailure("not a CSS colour: \(css)")
            self = .clear
            return
        }
        self = Color(.sRGB, red: c.red, green: c.green, blue: c.blue, opacity: c.alpha)
    }
}

/// A parsed CSS colour literal, 0…1 per channel.
struct CSSColor: Equatable {
    let red: Double
    let green: Double
    let blue: Double
    let alpha: Double

    init(red: Double, green: Double, blue: Double, alpha: Double) {
        (self.red, self.green, self.blue, self.alpha) = (red, green, blue, alpha)
    }

    init?(_ css: String) {
        let s = css.trimmingCharacters(in: .whitespaces).lowercased()
        if s.hasPrefix("#") {
            let hex = s.dropFirst()
            guard hex.count == 6 || hex.count == 8, let v = UInt64(hex, radix: 16) else { return nil }
            let rgba = hex.count == 6 ? v << 8 | 0xff : v
            func byte(_ shift: UInt64) -> Double { Double((rgba >> shift) & 0xff) / 255 }
            self.init(red: byte(24), green: byte(16), blue: byte(8), alpha: byte(0))
        } else if s.hasPrefix("rgba("), s.hasSuffix(")") {
            let parts = s.dropFirst(5).dropLast().split(separator: ",")
                .map { $0.trimmingCharacters(in: .whitespaces) }
            guard parts.count == 4,
                let r = Double(parts[0]), let g = Double(parts[1]), let b = Double(parts[2]),
                let a = Double(parts[3]),
                [r, g, b].allSatisfy({ (0...255).contains($0) }), (0...1).contains(a)
            else { return nil }
            self.init(red: r / 255, green: g / 255, blue: b / 255, alpha: a)
        } else {
            return nil
        }
    }
}
