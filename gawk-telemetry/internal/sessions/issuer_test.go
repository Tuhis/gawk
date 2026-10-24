package sessions

import (
	"encoding/json"
	"testing"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/store"
)

// R77 (docs/78 D3): a session a PoP served carries its issuer on every
// stored line and on its rollup row, and a timeline read back from disk
// recovers it — the same path the room key takes. A home session carries
// none, so its stored lines are unchanged.
func TestIssuerIsStoredOnEveryLineAndOnTheRollup(t *testing.T) {
	h := newHarness(t)
	const issuer = "a1b2c3"

	b := batch(0, true, fpsSamples(0, 60, 60), nil)
	b.Issuer = issuer
	if err := h.writer.Accept(b); err != nil {
		t.Fatal(err)
	}
	ref := store.SessionRef{Date: "2026-07-26", BroadcastKey: testBroadcast, SessionID: testSession}
	lines, err := h.store.ReadSession(ref)
	if err != nil {
		t.Fatal(err)
	}
	for i, ln := range lines {
		var rec Record
		if err := json.Unmarshal(ln, &rec); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if rec.Issuer != issuer {
			t.Errorf("line %d (%s) issuer = %q, want %q", i, rec.Kind, rec.Issuer, issuer)
		}
	}
	if len(h.rows) != 1 || h.rows[0].Issuer != issuer {
		t.Fatalf("rollup rows = %+v, want one row issued by %s", h.rows, issuer)
	}
	if in := ParseTimeline(Live{Ref: ref}, lines); in.Issuer != issuer {
		t.Errorf("ParseTimeline from disk: issuer = %q, want %q", in.Issuer, issuer)
	}
}
