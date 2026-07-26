package schema

import "testing"

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
