import SwiftUI
import UIKit

/// The broadcast-code field (docs/67 D20): every edit is normalized by
/// ``BroadcastCode/sanitize(_:)`` before it reaches the screen, as the SPA's
/// segmented input does.
///
/// A UIKit field because SwiftUI's can't do this reliably: rewriting its
/// text from a binding or `onChange` races fast typing (keystrokes landing
/// between the edit and the rewrite survive un-normalized), and a rejected
/// keystroke that leaves the model unchanged never re-renders the field at
/// all. The delegate decides each edit before it's applied.
struct CodeField: UIViewRepresentable {
    @Binding var text: String
    var onSubmit: () -> Void

    func makeUIView(context: Context) -> UITextField {
        let field = UITextField()
        field.delegate = context.coordinator
        field.placeholder = "Code"
        field.font = .monospacedSystemFont(
            ofSize: UIFont.preferredFont(forTextStyle: .title2).pointSize, weight: .regular)
        field.adjustsFontForContentSizeCategory = true
        field.autocapitalizationType = .allCharacters
        field.autocorrectionType = .no
        field.spellCheckingType = .no
        field.returnKeyType = .go
        field.accessibilityIdentifier = "watch.code"
        field.setContentHuggingPriority(.defaultLow, for: .horizontal)
        return field
    }

    func updateUIView(_ field: UITextField, context: Context) {
        context.coordinator.parent = self
        if field.text != text { field.text = text }
    }

    func makeCoordinator() -> Coordinator { Coordinator(self) }

    final class Coordinator: NSObject, UITextFieldDelegate {
        var parent: CodeField

        init(_ parent: CodeField) { self.parent = parent }

        func textField(
            _ field: UITextField,
            shouldChangeCharactersIn range: NSRange,
            replacementString string: String
        ) -> Bool {
            let current = field.text ?? ""
            guard let r = Range(range, in: current) else { return false }
            let clean = BroadcastCode.sanitize(current.replacingCharacters(in: r, with: string))
            field.text = clean
            parent.text = clean
            return false
        }

        func textFieldShouldReturn(_ field: UITextField) -> Bool {
            parent.onSubmit()
            return true
        }
    }
}
