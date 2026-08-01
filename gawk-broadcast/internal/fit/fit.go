// Package fit is the aspect-preserving geometry the encode resolution is
// derived from (R35, docs/39 D2 — ported from docs/38 D11's amendment).
//
// The configured resolution is a **bounding box**, never a stretch target: the
// source's aspect ratio is fitted inside it, one side pinned to the box and the
// other shrunk. Before R35 the Linux pipeline pinned exact `width=W,height=H`
// caps, so a 4:3 window or a 21:9 desktop went out to viewers vertically
// squashed — silently, because nothing in the chain considers a stretch an
// error.
//
// This is a deliberate second implementation of
// `gawk-broadcast-windows/crates/capture/src/fit.rs`, not a shared one: the two
// broadcasters are different languages in different modules, and the geometry
// is twenty lines. What must not drift is the *answers*, so this package's
// tests restate the Rust module's golden cases verbatim (fit_test.go) — the
// same discipline the wire mirrors use.
package fit

// Within fits a src×src source inside a boxW×boxH bounding box, preserving the
// source aspect ratio exactly: one output side equals its bound, the other is
// ≤ its bound. Results are floored to even (H.264 4:2:0 chroma subsampling
// needs even dimensions — the same guarantee the ladder math makes in docs/08)
// with a floor of 2. Degenerate inputs — any zero or negative, which is what a
// portal that omitted `size` looks like after parsing — fall back to the box
// itself, i.e. exactly the pre-R35 behavior.
func Within(srcW, srcH, boxW, boxH int) (int, int) {
	if srcW <= 0 || srcH <= 0 || boxW <= 0 || boxH <= 0 {
		return evenFloor(boxW), evenFloor(boxH)
	}
	// int64 throughout: the products below are ~10^7 for real screens, but a
	// portal is free to report anything and an overflow here would produce a
	// plausible-looking wrong resolution rather than a crash.
	sw, sh := int64(srcW), int64(srcH)
	bw, bh := int64(boxW), int64(boxH)
	// Source wider (relative to the box) ⇒ width pins to the box and the height
	// derives; otherwise the height pins. The derivation rounds to nearest —
	// the even floor below is the only lossy step.
	var w, h int64
	if sw*bh >= sh*bw {
		w, h = bw, (bw*sh+sw/2)/sw
	} else {
		w, h = (bh*sw+sh/2)/sh, bh
	}
	return evenFloor(int(w)), evenFloor(int(h))
}

// evenFloor rounds down to the nearest even value, never below 2.
func evenFloor(v int) int {
	if v < 2 {
		return 2
	}
	return v &^ 1
}
