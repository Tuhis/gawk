import SwiftUI
import UIKit

/// The text field under the six code boxes (docs/67 D20, docs/70 D3): every
/// edit is normalized by ``BroadcastCode/sanitize(_:)`` before it reaches
/// the screen, as the SPA's segmented input does. Its text and caret are
/// clear; ``CodeBoxesInput`` draws the boxes over it.
///
/// A UIKit field because SwiftUI's can't do this reliably: rewriting its
/// text from a binding or `onChange` races fast typing (keystrokes landing
/// between the edit and the rewrite survive un-normalized), and a rejected
/// keystroke that leaves the model unchanged never re-renders the field at
/// all. The delegate decides each edit before it's applied.
struct CodeField: UIViewRepresentable {
    @Binding var text: String
    @Binding var isFocused: Bool
    var identifier = "watch.code"
    var onSubmit: () -> Void

    func makeUIView(context: Context) -> UITextField {
        let field = UITextField()
        field.delegate = context.coordinator
        field.textColor = .clear
        field.tintColor = .clear
        field.backgroundColor = .clear
        field.autocapitalizationType = .allCharacters
        field.autocorrectionType = .no
        field.spellCheckingType = .no
        field.smartInsertDeleteType = .no
        field.keyboardType = .asciiCapable
        field.returnKeyType = .join
        field.accessibilityIdentifier = identifier
        field.accessibilityLabel = "Code"
        field.addTarget(context.coordinator, action: #selector(Coordinator.focusChanged(_:)), for: .editingDidBegin)
        field.addTarget(context.coordinator, action: #selector(Coordinator.focusChanged(_:)), for: .editingDidEnd)
        field.setContentHuggingPriority(.defaultLow, for: .horizontal)
        return field
    }

    func updateUIView(_ field: UITextField, context: Context) {
        context.coordinator.parent = self
        if field.text != text { field.text = text }
        // Only a request to drop the keyboard is pushed down: the field
        // itself reports focus, so pushing `true` back would fight a tap.
        if !isFocused, field.isFirstResponder {
            DispatchQueue.main.async { field.resignFirstResponder() }
        }
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

        /// The caret lives at the end: the boxes fill left to right, and an
        /// edit in the middle of text nobody can see would be a surprise.
        func textFieldDidChangeSelection(_ field: UITextField) {
            let end = field.endOfDocument
            if field.selectedTextRange?.start != end || field.selectedTextRange?.isEmpty == false {
                field.selectedTextRange = field.textRange(from: end, to: end)
            }
        }

        func textFieldShouldReturn(_ field: UITextField) -> Bool {
            parent.onSubmit()
            return true
        }

        @objc func focusChanged(_ field: UITextField) {
            let focused = field.isFirstResponder
            if parent.isFocused != focused { parent.isFocused = focused }
        }
    }
}
