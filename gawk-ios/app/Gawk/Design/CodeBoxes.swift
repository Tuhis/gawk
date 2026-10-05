import SwiftUI

/// Six 48 × 58 pt boxes, 8 pt apart (docs/70 §3.3). They shrink together on
/// a narrow screen and never grow: the code keeps its size at any Dynamic
/// Type setting (D27).
private struct CodeBoxRow<Box: View>: View {
    @ViewBuilder var box: (Int) -> Box

    var body: some View {
        HStack(spacing: 8) {
            ForEach(0..<BroadcastCode.length, id: \.self) { i in
                box(i)
                    .frame(maxWidth: 48)
                    .aspectRatio(48 / 58, contentMode: .fit)
            }
        }
        .frame(maxWidth: 48 * 6 + 8 * 5)
    }
}

private struct CodeBox: View {
    let character: Character?
    var stroke: Color
    var fill: Color = Theme.s2
    var ring = false
    var caret = false
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var blink = false

    var body: some View {
        let shape = RoundedRectangle(cornerRadius: 12, style: .continuous)
        ZStack {
            shape.fill(fill)
            shape.strokeBorder(stroke, lineWidth: ring ? 1.5 : 1)
            if ring {
                shape.inset(by: -3).strokeBorder(Theme.accentSoft, lineWidth: 3)
            }
            if let character {
                Text(String(character))
                    .font(Theme.codeBox)
                    .foregroundStyle(Theme.text)
                    .minimumScaleFactor(0.6)
            } else if caret {
                Capsule()
                    .fill(Theme.accent)
                    .frame(width: 2, height: 26)
                    .opacity(blink ? 0 : 1)
                    .onAppear {
                        guard !reduceMotion else { return }
                        withAnimation(.easeInOut(duration: 0.5).repeatForever()) { blink = true }
                    }
            }
        }
    }
}

/// The join card's code entry (docs/70 D3): six boxes drawn over the
/// sanitizing ``CodeField``, which takes the taps, the keyboard and
/// VoiceOver, so the boxes are read as one element (D27).
struct CodeBoxesInput: View {
    @Binding var code: String
    @Binding var isFocused: Bool
    var onSubmit: () -> Void

    var body: some View {
        let chars = Array(code)
        CodeBoxRow { i in
            let filled = i < chars.count
            let active = isFocused && i == chars.count
            CodeBox(
                character: filled ? chars[i] : nil,
                stroke: active ? Theme.accent : filled ? Theme.faint : Theme.border,
                ring: active,
                caret: active
            )
        }
        .allowsHitTesting(false)
        .accessibilityHidden(true)
        .background {
            CodeField(text: $code, isFocused: $isFocused, onSubmit: onSubmit)
        }
    }
}

/// Live's code (docs/70 D11): the same boxes, filled, tinted while copied.
struct CodeBoxesDisplay: View {
    let code: String
    var copied = false

    var body: some View {
        let chars = Array(code)
        CodeBoxRow { i in
            CodeBox(
                character: i < chars.count ? chars[i] : nil,
                stroke: copied ? Theme.accentLine : Theme.faint,
                fill: copied ? Theme.accentSoft : Theme.s2
            )
        }
        .animation(.easeOut(duration: 0.2), value: copied)
    }
}

#Preview("Code boxes") {
    @Previewable @State var code = "K7X"
    @Previewable @State var focused = true
    VStack(spacing: 24) {
        CodeBoxesInput(code: $code, isFocused: $focused) {}
        CodeBoxesDisplay(code: "K7XQ2M")
        CodeBoxesDisplay(code: "K7XQ2M", copied: true)
    }
    .padding()
    .background(Theme.bg)
    .preferredColorScheme(.dark)
}
