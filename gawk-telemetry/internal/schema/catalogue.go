package schema

// The field catalogue (docs/36 UD8, TH5).
//
// `ViewerFields`/`BroadcasterFields` above say what TYPE a field is, which is
// all the ingest path needs. A chart needs more: plotting a cumulative counter
// as a line is near-useless — what an operator wants is its rate — and a bool
// drawn as a line between 0 and 1 is a lie about a state. So each field also
// carries a SEMANTIC, a unit and a sentence.
//
// This table is server-owned on purpose. **D15 exists precisely because a
// second copy of a field list drifts**, and the UI having its own would be
// exactly that copy: a field added to the tables above would then be plottable
// only after someone remembered to add it twice. The catalogue is served, the
// UI renders whatever it is given, and a field added to the tables above
// appears in the picker with no UI change at all — which is a test, not a
// hope.
//
// A field with no entry here is still catalogued: its semantic falls out of
// its Kind and it simply carries no unit or description. Omission degrades the
// tooltip, never the plottability.

import "sort"

// Semantic is how a field behaves over time, which is what decides how it may
// legitimately be drawn.
type Semantic string

const (
	// SemGauge is a value that stands on its own at each sample: a rate, a
	// depth, a latency. Plot it directly.
	SemGauge Semantic = "gauge"
	// SemCounter is cumulative and only ever rises. Its LEVEL says almost
	// nothing; its first difference is the interesting series, so the UI offers
	// it as a rate by default and the cumulative form one click away.
	SemCounter Semantic = "counter"
	// SemBool is a state. It renders as a band, never as a line between 0 and 1.
	SemBool Semantic = "bool"
	// SemText is a word — a codec, a mode, a renderer. It renders as a band of
	// labelled spans; it is never a number.
	SemText Semantic = "text"
	// SemObject is a nested structure. Not directly plottable; the UI shows it
	// as JSON, and any member worth charting gets flattened into its own field
	// by the producer (see the datagramBuffer/audioBuffer flattening in
	// readapi.factsFor).
	SemObject Semantic = "object"
)

// Field is one catalogue entry.
type Field struct {
	Name string `json:"name"`
	// Roles is every role that reports this field. A handful are reported by
	// both, and a picker scoped to one session must know which.
	Roles    []string `json:"roles"`
	Semantic Semantic `json:"semantic"`
	Unit     string   `json:"unit,omitempty"`
	Desc     string   `json:"description,omitempty"`
	// Legacy marks the capitalized spellings the native broadcaster emitted
	// before it carried JSON tags. Still typed, still queryable — sessions on
	// disk are in that spelling for the whole raw window — but grouped away
	// from the names anything new should use.
	Legacy bool `json:"legacy,omitempty"`
}

type meta struct {
	sem  Semantic
	unit string
	desc string
}

func gauge(unit, desc string) meta   { return meta{SemGauge, unit, desc} }
func counter(unit, desc string) meta { return meta{SemCounter, unit, desc} }

// fieldMeta is the semantic layer over the type tables. Only the semantic is
// load-bearing — a wrong one makes a chart mislead — so the counter/gauge split
// here is pinned against the rollup's own counter lists by a test.
var fieldMeta = map[string]meta{
	// --- viewer: delivery funnel ------------------------------------------
	"receivedFps":       gauge("fps", "Frames fully reassembled from datagrams per second — the top of the viewer funnel."),
	"decoderFps":        gauge("fps", "Frames the decoder emitted per second."),
	"renderedFps":       gauge("fps", "Frames actually painted per second. Collapses to 0 in a hidden tab; read it beside documentHidden."),
	"decoderQueueDepth": gauge("frames", "Frames waiting in the decoder. A rising depth is the decoder falling behind."),
	"decodedFrames":     counter("frames", "Cumulative frames decoded."),
	"framesCompleted":   counter("frames", "Cumulative frames the reassembler completed."),
	"framesAssembled":   counter("frames", "Cumulative frames assembled from their chunks."),
	"framesDroppedIncomplete": counter("frames",
		"Frames abandoned because chunks never arrived — the direct cost of datagram loss."),
	"framesDroppedLate":          counter("frames", "Frames that arrived too late to be worth showing."),
	"framesDiscardedAwaitingKey": counter("frames", "Frames discarded while waiting for a keyframe to decode against."),
	"datagramsReceived":          counter("datagrams", "Cumulative datagrams received."),
	"badDatagrams":               counter("datagrams", "Datagrams that failed to parse. Non-zero is a wire-format or corruption problem."),
	"duplicateChunks":            counter("chunks", "Chunks received more than once."),
	"duplicateConfigs":           counter("configs", "Decoder configs received more than once."),
	"configsApplied":             counter("configs", "Decoder configurations applied. Each one is a decoder reset."),
	"videoBytesReceived":         counter("bytes", "Cumulative video bytes received."),
	"keyframeStreamsReceived":    counter("streams", "Keyframes delivered over reliable unidirectional streams."),
	"reorderGapResyncs":          counter("resyncs", "Times the reorder buffer gave up on a gap and resynchronised."),
	"reorderKeyframeWaitDrops":   counter("frames", "Frames dropped while the reorder buffer waited for a keyframe."),
	"reorderBuffered":            gauge("frames", "Frames currently held in the reorder buffer."),
	"staleChunks": counter("chunks",
		"Chunks rejected behind the emit watermark. Routine leg-skew stragglers while striped; phantom-frame evidence anywhere else."),

	// --- viewer: stalls, latency, drift ------------------------------------
	"timeSinceLastFrameMs":   gauge("ms", "How long since the last frame. Past 1000 ms this is a stall a viewer notices."),
	"lastKeyframeAgeMs":      gauge("ms", "Age of the most recent keyframe."),
	"timeSinceLastInboundMs": gauge("ms", "How long since anything at all arrived on the transport."),
	"lastDecodeLatencyMs":    gauge("ms", "Wall time the last frame spent inside the decoder."),
	"capToRenderMs":          gauge("ms", "Glass-to-glass latency: capture timestamp to paint. The number this project exists to keep small."),
	"liveEdgeDriftMs":        gauge("ms", "How far behind the live edge this viewer is running."),
	"timeSyncRttMs":          gauge("ms", "Round-trip of the clock-sync exchange; it bounds how much any cross-clock number can be trusted."),

	// --- viewer: playout + jitter ------------------------------------------
	"playoutOffsetMs":       gauge("ms", "The adaptive playout buffer's current depth. Pinned at its clamp means the controller ran out of room."),
	"renderCadenceStdDevMs": gauge("ms", "Spread of the interval between paints. Judder, measured."),
	"renderCadenceP95Ms":    gauge("ms", "95th-percentile paint interval. Invalid while the tab is hidden."),
	"arrivalJitterMs":       gauge("ms", "Spread of frame arrival intervals — the network's contribution to judder."),
	"decodeJitterMs":        gauge("ms", "Spread of decode completion intervals."),

	// --- viewer: parity + striping -----------------------------------------
	"parityChunksReceived":         counter("chunks", "Forward-parity chunks received (R29)."),
	"framesRecoveredByParity":      counter("frames", "Frames rebuilt from parity that would otherwise have been dropped."),
	"parityRecoveryFailures":       counter("frames", "Frames where parity was attempted and still failed."),
	"parityInsufficient":           counter("frames", "Frames lost holding parity too small for their erasures — the difference between 'parity failed' and 'parity was never enough'."),
	"framesSkippedWithinAllowance": counter("frames", "Frames skipped inside the configured allowance rather than being reported as loss."),
	"parityLevel":                  gauge("chunks", "Parity chunks generated per frame."),
	"parityChunksSent":             counter("chunks", "Parity chunks sent."),
	"parityBytesSent":              counter("bytes", "Bytes spent on parity."),
	"stripeActive":                 gauge("connections", "Connections currently carrying this viewer's datagrams (R30)."),
	"stripeNeeded":                 gauge("connections", "Connections the detector believes are needed. Diverging from stripeActive is caps pressure."),
	"stripeLargeLossPct":           gauge("%", "Chunk loss on large frames — the burst-threshold signature."),
	"stripeSmallLossPct":           gauge("%", "Chunk loss on small frames. Clean here while large is lossy is the shape striping exists for."),
	"stripeLargeChunks":            gauge("chunks", "Chunks in the largest recent frame."),
	"stripeLegDials":               counter("legs", "Additional connections dialled."),
	"stripeLegDeaths":              counter("legs", "Striped connections that died."),

	// --- viewer: delivery + audio ------------------------------------------
	"dvrBufferMs":           gauge("ms", "Depth of the relay-side DVR ring this viewer is reading from."),
	"carrierStreams":        counter("streams", "Carrier streams opened (resilient delivery)."),
	"carrierRecords":        counter("records", "Records delivered over carrier streams."),
	"carrierStreamsAborted": counter("streams", "Carrier streams aborted."),
	"audioPacketsReceived":  counter("packets", "Audio packets received."),
	"audioPacketsDecoded":   counter("packets", "Audio packets decoded."),
	"audioBytesReceived":    counter("bytes", "Audio bytes received."),
	"audioSampleRate":       gauge("Hz", "Audio sample rate."),
	"audioChannels":         gauge("channels", "Audio channel count."),
	"avSkewMs":              gauge("ms", "Audio/video skew. Only believable while avPlayheadAdvance is near 1."),
	"avPlayheadAdvance":     gauge("ratio", "Rate the audio playhead is advancing at. A stalled playhead makes avSkewMs meaningless."),
	"videoScheduleBaseEpochMs": gauge("ms",
		"Epoch the video schedule is anchored to."),

	// --- viewer: frame geometry + tab state --------------------------------
	"frameWidth":       gauge("px", "Width of the frames actually arriving."),
	"frameHeight":      gauge("px", "Height of the frames actually arriving."),
	"documentHiddenMs": counter("ms", "Cumulative time this tab spent in the background."),
	"viewerCount":      gauge("viewers", "Viewers the relay reports on this broadcast."),

	// --- broadcaster: send funnel ------------------------------------------
	"captureFps":            gauge("fps", "Frames arriving from the capture source per second."),
	"encoderFps":            gauge("fps", "Frames the encoder emitted per second."),
	"sentFps":               gauge("fps", "Frames put on the wire per second — the bottom of the send funnel."),
	"encodedFrames":         counter("frames", "Cumulative frames encoded."),
	"sentFrames":            counter("frames", "Cumulative frames sent."),
	"keyframes":             counter("frames", "Cumulative keyframes produced."),
	"droppedFrames":         counter("frames", "Frames dropped before the encoder."),
	"fpsGateDropped":        counter("frames", "Frames dropped by the frame-rate gate — intentional, not a fault."),
	"framesDroppedAtSend":   counter("frames", "Frames dropped at the send stage."),
	"datagramsSent":         counter("datagrams", "Cumulative datagrams sent."),
	"bytesSent":             counter("bytes", "Cumulative bytes sent."),
	"configsSent":           counter("configs", "Decoder configurations sent."),
	"keyframeStreamsSent":   counter("streams", "Keyframes sent over reliable streams."),
	"keyframeStreamsFailed": counter("streams", "Keyframe streams that failed to complete."),
	"keyframeStreamsSuperseded": counter("streams",
		"Keyframe streams abandoned because a newer keyframe replaced them. Routine."),
	"keyframeBytesSent":   counter("bytes", "Bytes sent as keyframe streams."),
	"encoderQueueDepth":   gauge("frames", "Frames waiting inside the encoder. A rising depth is the encoder losing the race."),
	"lastEncodeLatencyMs": gauge("ms", "Wall time the last frame spent inside the encoder."),
	"keyframeIntervalMs":  gauge("ms", "Interval between keyframes."),

	// --- broadcaster: ladder + targets -------------------------------------
	"autoStepDowns":    counter("steps", "Times the auto ladder stepped down."),
	"autoStepUps":      counter("steps", "Times the auto ladder stepped up."),
	"autoFps":          gauge("fps", "Frame rate the auto ladder is currently asking for."),
	"targetWidth":      gauge("px", "Width the broadcast was asked to produce."),
	"targetHeight":     gauge("px", "Height the broadcast was asked to produce."),
	"targetFps":        gauge("fps", "Frame rate the broadcast was asked to produce. Without it a shortfall is not computable."),
	"targetBitrateBps": gauge("bps", "Bitrate the broadcast was asked to produce."),
	"timeSyncOffsetUs": gauge("µs", "Clock offset the time-sync exchange settled on."),
	"resumes":          counter("resumes", "Times the native broadcaster resumed a broadcast."),

	// --- broadcaster: audio lane -------------------------------------------
	"audioEncodeLagMs":     gauge("ms", "How far behind real time the audio encoder is running."),
	"audioAnchorReanchors": counter("re-anchors", "Times the audio anchor was re-established. Each one is a discontinuity."),
	"audioEncodedPackets":  counter("packets", "Audio packets encoded."),
	"audioPacketsSent":     counter("packets", "Audio packets sent."),
	"audioPacketsDropped":  counter("packets", "Audio packets dropped before sending."),
	"audioBytesSent":       counter("bytes", "Audio bytes sent."),
	"audioConfigsSent":     counter("configs", "Audio decoder configurations sent."),
	"audioEncodedPerSec":   gauge("packets/s", "Audio packets encoded per second."),
	"audioSentPerSec":      gauge("packets/s", "Audio packets sent per second."),
	"audioBitrateBps":      gauge("bps", "Audio bitrate."),

	// --- text/config fields worth a sentence -------------------------------
	"deliveryMode":    {SemText, "", "How this viewer is being fed: datagrams, or a resilient carrier-stream mode."},
	"playoutMode":     {SemText, "", "Adaptive or fixed playout scheduling."},
	"interpolation":   {SemText, "", "Whether live-edge interpolation is engaged."},
	"presentation":    {SemText, "", "The presentation path in use."},
	"renderer":        {SemText, "", "The render path that actually engaged — the answer to 'did the fast path take?'"},
	"pipelineContext": {SemText, "", "Which context the pipeline is running in (main thread or worker)."},
	"transport":       {SemText, "", "The transport in use."},
	"audioState":      {SemText, "", "State of the audio lane."},
	"avMaster":        {SemText, "", "Which clock A/V sync is slaved to."},
	"audioCodec":      {SemText, "", "Negotiated audio codec."},
	"stripeMode":      {SemText, "", "Requested striping mode (R30): off, auto, or a fixed leg count."},
	"autoRung":        {SemText, "", "Rung of the quality ladder currently selected."},
	"autoCeiling":     {SemText, "", "Ceiling the ladder is not allowed to exceed."},
	"encoder":         {SemText, "", "Encoder element in use (native broadcaster)."},
	"capturePath":     {SemText, "", "Capture path in use (native broadcaster)."},
	"audioSource":     {SemText, "", "Audio source in use (native broadcaster)."},
	"codec":           {SemText, "", "Negotiated video codec."},
	"acceleration":    {SemText, "", "Hardware-acceleration preference the encoder was configured with."},

	// --- bools --------------------------------------------------------------
	"documentHidden":            {SemBool, "", "Whether the tab was in the background at this sample. renderedFps means nothing without it."},
	"isHardwareAccelerated":     {SemBool, "", "Whether the decoder reported itself hardware-accelerated."},
	"stripeCapable":             {SemBool, "", "Whether this client can stripe at all."},
	"autoAtFloor":               {SemBool, "", "Whether the ladder has hit its floor and has nowhere left to step down."},
	"encoderPressure":           {SemBool, "", "Whether the encoder is signalling pressure."},
	"captureFpsAvailable":       {SemBool, "", "Whether the capture source reports a frame rate at all."},
	"keyframeIntervalAvailable": {SemBool, "", "Whether the encoder reports its keyframe interval."},
	"timeSyncAvailable":         {SemBool, "", "Whether clock sync is available on this path."},
	"viewerCountAvailable":      {SemBool, "", "Whether a viewer count is available on this path."},
	"resuming":                  {SemBool, "", "Whether the native broadcaster is mid-resume."},

	// --- objects ------------------------------------------------------------
	"audioBuffer":         {SemObject, "", "Audio jitter-buffer detail. Its members are flattened into facts for the rules."},
	"connection":          {SemObject, "", "Network Information API readings. Null in every shipping browser today (docs/13 D7)."},
	"presentationMux":     {SemObject, "", "iOS fMP4 muxer state."},
	"presentationSurface": {SemObject, "", "Presentation surface state."},
	"featureGates":        {SemObject, "", "Which feature gates this client had on."},
	"intervalMin":         {SemObject, "", "The worst reading the client saw BETWEEN two emitted samples — what makes a sub-sample dip visible at all (D16)."},
}

// legacyBroadcasterSpelling marks the capitalized names the native broadcaster
// emitted before it carried JSON tags. They stay queryable — sessions already
// on disk use them — but a picker should not offer them beside the current
// spelling as though they were different measurements.
func legacyBroadcasterSpelling(name string) bool {
	if name == "" {
		return false
	}
	c := name[0]
	return c >= 'A' && c <= 'Z'
}

// lowerFirst maps a legacy capitalized spelling to its lowerCamelCase twin.
func lowerFirst(name string) string {
	if name == "" {
		return name
	}
	return string(name[0]|0x20) + name[1:]
}

// semanticFor falls back from the type table when a field has no entry above.
// A new field is therefore catalogued and plottable the moment it is typed,
// which is what stops this table from becoming the thing that must be
// remembered.
func semanticFor(k Kind) Semantic {
	switch k {
	case KindBool:
		return SemBool
	case KindString:
		return SemText
	case KindObject, KindAny:
		return SemObject
	default:
		return SemGauge
	}
}

// Catalogue returns every known field, merged across roles and sorted by name.
//
// Merged rather than served per-role because a handful of fields (timeSyncRttMs,
// viewerCount, audioState…) are reported by both, and two entries for one
// measurement is the drift this endpoint exists to prevent.
func Catalogue() []Field {
	byName := map[string]*Field{}
	add := func(role string, table map[string]Kind) {
		for name, kind := range table {
			f, ok := byName[name]
			if !ok {
				legacy := role == "broadcaster" && legacyBroadcasterSpelling(name)
				m, known := fieldMeta[name]
				// A legacy capitalized spelling is the SAME measurement as its
				// lowerCamelCase twin, so it inherits the twin's semantics
				// rather than needing its own entry. Without this a native
				// broadcaster's `EncodedFrames` would be catalogued as a gauge
				// while `encodedFrames` is a counter — one measurement, two
				// contradictory chart shapes, which is precisely the drift D15
				// names.
				if !known && legacy {
					m, known = fieldMeta[lowerFirst(name)]
				}
				sem := semanticFor(kind)
				if known && m.sem != "" {
					sem = m.sem
				}
				f = &Field{
					Name: name, Semantic: sem, Unit: m.unit, Desc: m.desc, Legacy: legacy,
				}
				byName[name] = f
			}
			f.Roles = append(f.Roles, role)
		}
	}
	add("viewer", ViewerFields)
	add("broadcaster", BroadcasterFields)

	out := make([]Field, 0, len(byName))
	for _, f := range byName {
		sort.Strings(f.Roles)
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
