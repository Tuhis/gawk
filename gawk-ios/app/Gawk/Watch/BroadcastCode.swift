/// The Watch screen's code entry (docs/67 D20): the SPA's rules
/// (`gawk-app/src/lib/broadcastId.ts`), so a code that joins on the web joins
/// here and nothing else does.
///
/// The alphabet is the relay's (`gawk-server/internal/broadcastid`
/// `Alphabet`, restated in `gawk_wire::BROADCAST_ID_ALPHABET`):
/// six characters, no 0/O/1/I/L. This is its one Swift definition.
enum BroadcastCode {
    static let alphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
    static let length = 6

    /// Upper-cases and keeps only alphabet characters, capped at the code
    /// length: what the field shows as the user types or pastes.
    static func sanitize(_ raw: String) -> String {
        var out = ""
        for ch in raw.uppercased() where alphabet.contains(ch) {
            out.append(ch)
            if out.count == length { break }
        }
        return out
    }

    static func isValid(_ code: String) -> Bool {
        code.count == length && sanitize(code) == code
    }
}
