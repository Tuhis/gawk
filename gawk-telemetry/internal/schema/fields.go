package schema

// The known-field tables (docs/33 D15). These exist for ONE purpose: to keep a
// wrongly-typed value out of anything that feeds percentile math or a
// diagnose() predicate. A string "30", a null, or a non-finite number in an
// fps series is exactly the failure this table prevents.
//
// It is deliberately NOT a closed list. A field absent here is kept verbatim
// and counted as unknown, so a gawk-app that ships a new field keeps reporting
// against a service that has never heard of it — version skew is permanent,
// not transient, and a closed list would lose telemetry precisely during a
// deploy (D15).
//
// Adding a field here is how it becomes typed and queryable. Removing one is
// never necessary: a field that no client sends any more simply stops
// appearing.
//
// Source of truth for the names: gawk-app/src/transport/viewer.ts
// (ViewerStats), gawk-app/src/transport/broadcaster.ts (BroadcastStats), and
// gawk-broadcast/internal/engine/stats.go (engine.Stats, which marshals with
// Go's default capitalized names — hence both spellings below for the fields
// the native broadcaster also reports).

// ViewerFields types what a browser viewer reports.
var ViewerFields = map[string]Kind{
	// Reassembler / delivery funnel. ViewerStats EXTENDS ReassemblerStats, so
	// every field of that interface belongs here too — the four counters
	// below were missing until the e2e pass measured a real client and found
	// them arriving as "unknown" on every sample.
	"framesCompleted":            KindNumber,
	"badDatagrams":               KindNumber,
	"duplicateChunks":            KindNumber,
	"duplicateConfigs":           KindNumber,
	"framesAssembled":            KindNumber,
	"framesDroppedIncomplete":    KindNumber,
	"framesDroppedLate":          KindNumber,
	"datagramsReceived":          KindNumber,
	"receivedFps":                KindNumber,
	"decodedFrames":              KindNumber,
	"decoderQueueDepth":          KindNumber,
	"decoderFps":                 KindNumber,
	"renderedFps":                KindNumber,
	"configsApplied":             KindNumber,
	"framesDiscardedAwaitingKey": KindNumber,
	"lastDecodeLatencyMs":        KindNumber,
	"isHardwareAccelerated":      KindBool,
	"frameWidth":                 KindNumber,
	"frameHeight":                KindNumber,
	"keyframeStreamsReceived":    KindNumber,
	"reorderGapResyncs":          KindNumber,
	"reorderKeyframeWaitDrops":   KindNumber,
	"reorderBuffered":            KindNumber,
	"videoBytesReceived":         KindNumber,

	// Placement (R10) — the "did the fast path actually engage?" answers.
	"renderer":        KindString,
	"pipelineContext": KindString,
	"transport":       KindString,

	// Stall indicators.
	"timeSinceLastFrameMs":   KindNumber,
	"lastKeyframeAgeMs":      KindNumber,
	"timeSinceLastInboundMs": KindNumber,

	// R5 latency + drift.
	"liveEdgeDriftMs": KindNumber,
	"capToRenderMs":   KindNumber,
	"timeSyncRttMs":   KindNumber,

	// R12 playout + jitter.
	"playoutOffsetMs":       KindNumber,
	"playoutMode":           KindString,
	"presentation":          KindString,
	"interpolation":         KindString,
	"renderCadenceStdDevMs": KindNumber,
	"renderCadenceP95Ms":    KindNumber,
	"arrivalJitterMs":       KindNumber,
	"decodeJitterMs":        KindNumber,

	// R19/R21 delivery.
	"deliveryMode":          KindString,
	"dvrBufferMs":           KindNumber,
	"carrierStreams":        KindNumber,
	"carrierRecords":        KindNumber,
	"carrierStreamsAborted": KindNumber,

	// R18.
	"viewerCount": KindNumber,

	// R15 audio.
	"audioState":           KindString,
	"audioPacketsReceived": KindNumber,
	"audioPacketsDecoded":  KindNumber,
	"audioBytesReceived":   KindNumber,
	"audioCodec":           KindString,
	"audioSampleRate":      KindNumber,
	"audioChannels":        KindNumber,
	"avSkewMs":             KindNumber,
	"avMaster":             KindString,
	// docs/20 finding 13 (#152): the ratio at which the audio playhead is
	// actually advancing. Typed here because a *stalled* playhead is what made
	// avSkewMs unbelievable — the very metric docs/33 §1.1 cites as R28's
	// motivating open question — so it must be queryable, not an unknown.
	"avPlayheadAdvance":        KindNumber,
	"videoScheduleBaseEpochMs": KindNumber,
	"audioBuffer":              KindObject,

	// R9 connection health (null in every shipping browser today — docs/13 D7).
	"connection": KindObject,

	// R16/R22 presentation.
	"presentationMux":     KindObject,
	"presentationSurface": KindObject,
	"featureGates":        KindAny,
}

// BroadcasterFields types what a browser broadcaster reports.
var BroadcasterFields = map[string]Kind{
	// Send funnel (R9 D5): capture → post-gate → encoded → sent.
	"captureFps":     KindNumber,
	"encoderFps":     KindNumber,
	"sentFps":        KindNumber,
	"encodedFrames":  KindNumber,
	"keyframes":      KindNumber,
	"droppedFrames":  KindNumber,
	"fpsGateDropped": KindNumber,
	"datagramsSent":  KindNumber,
	"bytesSent":      KindNumber,
	"configsSent":    KindNumber,

	"keyframeStreamsSent":   KindNumber,
	"keyframeStreamsFailed": KindNumber,
	"keyframeBytesSent":     KindNumber,
	"encoderQueueDepth":     KindNumber,
	"lastEncodeLatencyMs":   KindNumber,

	"connection":    KindObject,
	"timeSyncRttMs": KindNumber,

	// R4 ladder / R13 ceilings.
	"autoRung":        KindString,
	"autoAtFloor":     KindBool,
	"autoStepDowns":   KindNumber,
	"autoStepUps":     KindNumber,
	"encoderPressure": KindBool,
	"autoCeiling":     KindString,
	"autoFps":         KindNumber,

	"pipelineContext": KindString,
	"viewerCount":     KindNumber,

	// R15 audio lane.
	"audioState": KindString,
	// docs/20 finding 13 (#152): send-side encode lag and anchor re-anchors.
	"audioEncodeLagMs":     KindNumber,
	"audioAnchorReanchors": KindNumber,
	"audioEncodedPackets":  KindNumber,
	"audioPacketsSent":     KindNumber,
	"audioBytesSent":       KindNumber,
	"audioConfigsSent":     KindNumber,
	"audioEncodedPerSec":   KindNumber,
	"audioSentPerSec":      KindNumber,
	"audioSampleRate":      KindNumber,
	"audioChannels":        KindNumber,
	"audioCodec":           KindString,
	"audioBitrateBps":      KindNumber,

	// The native broadcaster (R14) marshals engine.Stats with Go's default
	// capitalized field names. Typed here so one query covers both producers
	// rather than the native one reading as 40 unknown fields per sample.
	"Encoder":                   KindString,
	"Codec":                     KindString,
	"Width":                     KindNumber,
	"Height":                    KindNumber,
	"Fps":                       KindNumber,
	"BitrateBps":                KindNumber,
	"CaptureFpsAvailable":       KindBool,
	"CapturePath":               KindString,
	"EncodedFrames":             KindNumber,
	"Keyframes":                 KindNumber,
	"KeyframeIntervalAvailable": KindBool,
	"KeyframeIntervalMs":        KindNumber,
	"SentFrames":                KindNumber,
	"EncoderFps":                KindNumber,
	"SentFps":                   KindNumber,
	"DatagramsSent":             KindNumber,
	"BytesSent":                 KindNumber,
	"ConfigsSent":               KindNumber,
	"KeyframeStreamsSent":       KindNumber,
	"KeyframeStreamsFailed":     KindNumber,
	"KeyframeStreamsSuperseded": KindNumber,
	"KeyframeBytesSent":         KindNumber,
	"FramesDroppedAtSend":       KindNumber,
	"TimeSyncRttMs":             KindNumber,
	"TimeSyncOffsetUs":          KindNumber,
	"ViewerCount":               KindNumber,
}

// FieldsForRole returns the table for a role. An unrecognized role yields nil,
// which SanitizeStats treats as "nothing is known" — everything is kept
// verbatim under the structural bounds, which is the right degradation for a
// producer this build has not learned.
func FieldsForRole(role string) map[string]Kind {
	switch role {
	case "viewer":
		return ViewerFields
	case "broadcaster":
		return BroadcasterFields
	default:
		return nil
	}
}
