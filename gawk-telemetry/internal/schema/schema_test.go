package schema

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// ViewerStats extends ReassemblerStats, so every field of that interface must
// be typed here too. Four of them were missing until the R28 e2e pass measured
// a real client and found them arriving as "unknown" on every sample — which
// works (D15 keeps unknowns verbatim) but leaves them out of every numeric
// query, which is the whole point of typing a field.
//
// The list is transcribed from gawk-app/src/transport/reassembler.ts. An
// unknown field is fine; a field of a KNOWN interface missing from the table
// is drift.
func TestReassemblerStatsAreFullyTyped(t *testing.T) {
	for _, f := range []string{
		"datagramsReceived", "badDatagrams", "duplicateChunks", "duplicateConfigs",
		"framesCompleted", "framesDroppedIncomplete", "framesDroppedLate",
		"audioPacketsReceived", "audioBytesReceived",
	} {
		if _, ok := ViewerFields[f]; !ok {
			t.Errorf("ReassemblerStats field %q is not typed in ViewerFields", f)
		}
	}
}

// The audio fields R15's finding-13 fix added (#152) must be typed. They exist
// to make `avSkewMs` believable — and `avSkewMs` over-reporting on long
// sessions is the open question docs/33 §1.1 cites as R28's motivation — so
// arriving as untyped unknowns would leave the one thing this store was built
// to answer out of every numeric query.
func TestFinding13AudioFieldsAreTyped(t *testing.T) {
	for _, f := range []string{"avSkewMs", "avMaster", "avPlayheadAdvance"} {
		if _, ok := ViewerFields[f]; !ok {
			t.Errorf("viewer audio field %q is not typed", f)
		}
	}
	for _, f := range []string{"audioEncodeLagMs", "audioAnchorReanchors"} {
		if _, ok := BroadcasterFields[f]; !ok {
			t.Errorf("broadcaster audio field %q is not typed", f)
		}
	}
}

// The funnel fields every diagnose() rule reads must be typed, or the rule
// silently lands in "unavailable" instead of firing.
func TestRuleInputFieldsAreTyped(t *testing.T) {
	viewer := []string{
		"receivedFps", "decoderFps", "renderedFps", "timeSinceLastFrameMs",
		"timeSinceLastInboundMs", "reorderGapResyncs", "keyframeStreamsReceived",
		"playoutOffsetMs", "isHardwareAccelerated", "decoderQueueDepth",
		"deliveryMode", "audioPacketsReceived",
	}
	for _, f := range viewer {
		if _, ok := ViewerFields[f]; !ok {
			t.Errorf("viewer rule input %q is not typed", f)
		}
	}
	for _, f := range []string{"captureFps", "encoderFps", "sentFps", "encoderQueueDepth"} {
		if _, ok := BroadcasterFields[f]; !ok {
			t.Errorf("broadcaster rule input %q is not typed", f)
		}
	}
}

// truncate() bounds a known string field's stored length. A byte slice can
// land inside a multi-byte rune, storing invalid UTF-8 that json.Marshal
// then silently rewrites to U+FFFD — corrupting the value rather than
// merely shortening it. Built so the emoji's first byte lands exactly on the
// cut, the same construction as the ingest package's clip() test.
func TestTruncateStopsOnARuneBoundary(t *testing.T) {
	s := strings.Repeat("a", MaxStringLen-1) + "世" + strings.Repeat("z", 10)
	var an Anomalies
	got := truncate(s, &an)
	if len(got) > MaxStringLen {
		t.Fatalf("truncate exceeded MaxStringLen: len=%d, value=%q", len(got), got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncate produced invalid UTF-8: %q", got)
	}
	if want := strings.Repeat("a", MaxStringLen-1); got != want {
		t.Errorf("truncate(...) = %q, want %q (the split rune dropped whole)", got, want)
	}
	if an.Coerced != 1 {
		t.Errorf("Coerced = %d, want 1", an.Coerced)
	}
}

// sanitizeObject drops fields beyond MaxFieldsPerObject. Which ones survive
// must be reproducible: this service exists to diagnose OTHER systems, and
// an oversized payload whose survivor set changes from one request to the
// next for byte-identical input would make its own output the unreliable
// variable. Go's map iteration order is randomized per range statement (not
// merely per process), so the pre-fix unordered walk could — and, run
// enough times, did — pick a different set of ~512 survivors out of 603
// candidate fields on every call.
func TestSanitizeObjectOversizeSurvivorsAreDeterministic(t *testing.T) {
	known := map[string]Kind{
		"receivedFps": KindNumber,
		"decoderFps":  KindNumber,
		"audioCodec":  KindString,
	}
	obj := map[string]any{
		"receivedFps": 30.0,
		"decoderFps":  60.0,
		"audioCodec":  "opus",
	}
	const extra = 600
	for i := 0; i < extra; i++ {
		obj[fmt.Sprintf("unknownField%04d", i)] = float64(i)
	}
	if len(obj) <= MaxFieldsPerObject {
		t.Fatalf("test object has %d fields, want more than MaxFieldsPerObject (%d)", len(obj), MaxFieldsPerObject)
	}

	var first []string
	for i := 0; i < 30; i++ {
		var an Anomalies
		got := sanitizeObject(obj, known, 1, &an)
		gotKeys := sortedKeys(got)
		if first == nil {
			first = gotKeys
			continue
		}
		if !equalStrings(first, gotKeys) {
			t.Fatalf("run %d produced a different survivor set than run 0", i)
		}
	}

	survivors := map[string]bool{}
	for _, k := range first {
		survivors[k] = true
	}
	// The known, typed fields — what diagnose() and the rollup actually
	// read — must survive ahead of any unknown field.
	for k := range known {
		if !survivors[k] {
			t.Errorf("known field %q did not survive an oversized object", k)
		}
	}
	// Unknown fields fill whatever budget remains, in a fixed (lexical)
	// order, so the cut point itself is reproducible.
	wantUnknown := MaxFieldsPerObject - len(known)
	for i := 0; i < wantUnknown; i++ {
		name := fmt.Sprintf("unknownField%04d", i)
		if !survivors[name] {
			t.Errorf("%s should have survived within the budget, was dropped", name)
		}
	}
	if cut := fmt.Sprintf("unknownField%04d", wantUnknown); survivors[cut] {
		t.Errorf("%s should have been cut by the budget, survived", cut)
	}
	if len(first) != MaxFieldsPerObject {
		t.Errorf("survivor count = %d, want exactly MaxFieldsPerObject (%d)", len(first), MaxFieldsPerObject)
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
