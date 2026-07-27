package rules

// ProducibleFacts is the inventory of fact names this service's producers can
// actually emit — the live projection (internal/live) and the read API's
// per-session assembly (internal/readapi).
//
// It exists because a rule requiring a name nothing produces fails INVISIBLY:
// the engine reports it `unavailable`, honestly and forever, and the rule looks
// like a rule. Playbook row 10 shipped that way (review finding 5) — it
// required `relay.framesRelayedPerSec`, which no producer set, so one of the
// fifteen advertised rules could never fire and `send-path-gap`'s relay anchor
// never engaged, permanently capping its confidence at 0.7.
//
// The value names WHICH producer emits it, and that is what makes the contract
// two-sided rather than decorative: the rules package asserts every rule's
// `Requires` is listed here, and each producer asserts the set it emits equals
// exactly its share of this map. So a name listed here that nothing actually
// emits fails a test, which is precisely the state row 10 shipped in.
var ProducibleFacts = map[string]string{
	// --- relay, broadcast level (live: aggregated per pod; readapi: joined) --
	"relay.publisherActive":           both,
	"relay.subscribers":               both,
	"relay.subscribersDropping":       both,
	"relay.subscribersFleetTotal":     both,
	"relay.viewersGlobal":             both,
	"relay.framesRelayed":             both,
	"relay.framesRelayedPerSec":       both,
	"relay.datagramsDropped":          both,
	"relay.bandwidthDroppedDatagrams": both,
	"relay.ingressFramesLost":         both,
	"relay.ingressLossRatio":          both,
	"relay.keyframeStreamsIn":         both,

	// --- relay, per subscriber ---------------------------------------------
	"relay.subscriberDropped":     both,
	"relay.queueDepth":            both,
	"relay.keyframesDropped":      both,
	"relay.carrierQueueOverflow":  both,
	"relay.carrierRecordsDropped": both,
	"relay.dvrResyncs":            both,
	"relay.dvrLagMs":              both,
	"relay.peerMedianDropped":     both,

	// --- client, viewer ------------------------------------------------------
	"client.receivedFps":             both,
	"client.decoderFps":              both,
	"client.renderedFps":             both,
	"client.decoderQueueDepth":       both,
	"client.timeSinceLastFrameMs":    both,
	"client.timeSinceLastInboundMs":  both,
	"client.lastKeyframeAgeMs":       live,
	"client.capToRenderMs":           live,
	"client.liveEdgeDriftMs":         live,
	"client.playoutOffsetMs":         both,
	"client.arrivalJitterMs":         live,
	"client.reorderGapResyncs":       both,
	"client.keyframeStreamsReceived": both,
	"client.avSkewMs":                live,
	"client.viewerCount":             live,
	"client.audioPacketsReceived":    both,
	"client.audioOverflowDrops":      both,
	"client.audioGapsConcealed":      both,
	"client.isHardwareAccelerated":   both,

	// --- client, dip episodes (D16) ------------------------------------------
	// Derived by the same detector over two windows: the session for readapi,
	// a rolling window for live. Both producers emit all six, or the rules
	// would fire on one surface and read `unavailable` on the other.
	"client.fpsDipEpisodes":  both,
	"client.fpsDipShare":     both,
	"client.fpsDipWorstFps":  both,
	"client.fpsDipLongestMs": both,
	"client.fpsDipResyncs":   both,
	"client.fpsDipKeyframes": both,

	// --- client, broadcaster -------------------------------------------------
	"client.captureFps":        both,
	"client.encoderFps":        both,
	"client.sentFps":           both,
	"client.encoderQueueDepth": both,
	"client.EncoderFps":        live,
	"client.SentFps":           live,

	// --- client, the configured target (D17) ---------------------------------
	// What the broadcast was ASKED to be. Every other client fact is an
	// outcome; these are what make the difference computable at all.
	"client.targetFps":        both,
	"client.targetBitrateBps": both,

	// --- text ----------------------------------------------------------------
	"text.deliveryMode":    both,
	"text.playoutMode":     both,
	"text.renderer":        both,
	"text.pipelineContext": both,
	"text.transport":       both,
	"text.audioState":      both,
	"text.autoRung":        both,
	"text.Encoder":         both,
	"text.Codec":           both,
	// D17: the encoder's committed acceleration and codec. On the live surface
	// too, because "is this broadcaster encoding in software right now?" is an
	// operator question, not just a post-mortem one.
	"text.acceleration": both,
	"text.codec":        both,

	// --- text, stored-session only -------------------------------------------
	// Config the ROLLUP derives (resolution, the target trio) or that only a
	// finished row carries. The live projection reads string fields straight
	// off the last sample and never computes these, so claiming `both` would
	// make the live producer fail its own half of the contract.
	"text.resolution":        readapi,
	"text.targetFps":         readapi,
	"text.targetBitrateKbps": readapi,
	"text.autoCeiling":       readapi,
	"text.audioCodec":        readapi,
	"text.CapturePath":       readapi,
	"text.interpolation":     readapi,
	"text.presentation":      readapi,
	"text.avMaster":          readapi,
}

// The three producer tags. `both` is the common case: the same signal is read
// live from a scrape and after the fact from the stored session.
//
// The `live`-only entries are live-row metrics the rollup does carry as series
// but the read path deliberately does not turn into facts — no rule asks for
// them, and a fact nothing consumes is weight, not coverage. If a rule ever
// requires one, this map is where that shows up as a failing test rather than
// as a verdict that silently never fires.
const (
	live = "live"
	both = "live+readapi"
	// readapi is the case this file anticipated and did not yet have: a fact
	// derivable from a stored session but not from a scrape. The rollup DERIVES
	// these (a formatted resolution, the target trio) or they are viewer config
	// the live row does not carry, so the live projection cannot produce them
	// and must not be asserted to.
	readapi = "readapi"
)

// ProducedBy returns the names one producer is expected to emit.
func ProducedBy(producer string) map[string]bool {
	out := map[string]bool{}
	for name, by := range ProducibleFacts {
		if by == producer || by == both {
			out[name] = true
		}
	}
	return out
}
