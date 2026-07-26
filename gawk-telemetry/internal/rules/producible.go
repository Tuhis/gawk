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

	// --- client, broadcaster -------------------------------------------------
	"client.captureFps":        both,
	"client.encoderFps":        both,
	"client.sentFps":           both,
	"client.encoderQueueDepth": both,
	"client.EncoderFps":        live,
	"client.SentFps":           live,

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
	// A read-only tag would go here if a fact were ever derivable from a
	// stored session but not from a scrape. None is today.
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
