// Package live is the in-memory projection behind the TM8 dashboard's `/live`
// endpoint (docs/33 §4.8.2).
//
// **Disk is for history; the live page never reads a session file.** The relay
// scraper refreshes the relay side every ~5 s and ingest refreshes the client
// side as batches land (~10 s), so the two halves of a row have DIFFERENT
// freshness — and the UI says so rather than painting them as one instant.
//
// Two honesty rules are structural here, not cosmetic:
//
//   - A viewer whose client telemetry has gone quiet while the relay still
//     sees the subscriber is **stale**.
//   - A subscriber that never reported at all (an old client, a blocked
//     endpoint) is **unknown**.
//
// Neither is ever `ok`. Painting an absence of evidence as green is the one
// thing an ops dashboard must not do.
package live

import (
	"sort"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/ingest"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/relayscrape"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rules"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/schema"
)

// Freshness bounds. A client is expected every ~10 s (its flush cadence) and
// the relay every scrape interval; these are the "we have not heard in a
// while" points, generous enough that one missed batch is not an alarm.
const (
	ClientStaleAfter = 30 * time.Second
	RelayStaleAfter  = 20 * time.Second
	// EndedRetention is how long an ended broadcast stays in the recent group.
	EndedRetention = 6 * time.Hour
	// MaxRecentEnded bounds the recessed history group.
	MaxRecentEnded = 10
	// EscalateSamples is the hysteresis: a state must hold for this many
	// consecutive evaluations to escalate. A single-sample blip must not
	// light the page up.
	EscalateSamples = 2
	// ClearDwell is how long a cleared fault persists before it disappears.
	// The asymmetry is deliberate for an ops view: problems should appear
	// promptly and must not vanish before the human finishes looking at them.
	ClearDwell = 15 * time.Second
)

// SessionView is one row in the broadcast detail table — a broadcaster or a
// viewer, distinguished by Role. They are the same kind of thing (both are
// sessions with tokens), which is why they share one table.
type SessionView struct {
	SessionID    string          `json:"sessionId"`
	BroadcastKey string          `json:"broadcastKey"`
	Role         string          `json:"role"`
	Browser      string          `json:"browser,omitempty"`
	OS           string          `json:"os,omitempty"`
	AppVersion   string          `json:"appVersion,omitempty"`
	StartedAtMs  int64           `json:"startedAtMs,omitempty"`
	Severity     rules.Severity  `json:"severity"`
	Verdict      string          `json:"verdict,omitempty"`
	Findings     []rules.Finding `json:"findings,omitempty"`
	// Freshness is reported per SIDE because the two are refreshed by
	// different mechanisms at different rates. Presenting them as one instant
	// would be a lie a viewer's absence could hide behind.
	ClientAgeMs int64  `json:"clientAgeMs"`
	RelayAgeMs  int64  `json:"relayAgeMs"`
	ClientState string `json:"clientState"` // reporting | stale | unknown
	RelayState  string `json:"relayState"`  // observed | stale | unknown
	// Metrics is the small curated set a row shows without expanding.
	Metrics map[string]float64 `json:"metrics,omitempty"`
	Config  map[string]string  `json:"config,omitempty"`
}

// BroadcastView is one row in the fleet view.
type BroadcastView struct {
	BroadcastKey string         `json:"broadcastKey"`
	Lifecycle    string         `json:"lifecycle"` // live | away | ended
	Severity     rules.Severity `json:"severity"`
	// WorstViewer is the worst severity among this broadcast's viewers, which
	// is what an operator scanning the fleet actually needs.
	WorstViewer rules.Severity     `json:"worstViewer"`
	Viewers     int                `json:"viewers"`
	UptimeMs    int64              `json:"uptimeMs"`
	EndedAgoMs  int64              `json:"endedAgoMs,omitempty"`
	Pod         string             `json:"pod,omitempty"`
	Role        string             `json:"role,omitempty"`
	Findings    []rules.Finding    `json:"findings,omitempty"`
	Sessions    []SessionView      `json:"sessions,omitempty"`
	Metrics     map[string]float64 `json:"metrics,omitempty"`
}

// Snapshot is what `/live` returns.
type Snapshot struct {
	AtMs int64 `json:"atMs"`
	// Live and Ended are NEVER interleaved (owner decision, §4.8.1): the
	// grouping IS the precedence, because only a live problem can still be
	// acted on. A live `warn` therefore outranks an ended `bad` by position,
	// while the ended row keeps its own hue.
	Live  []BroadcastView `json:"live"`
	Ended []BroadcastView `json:"ended"`
}

type sessionState struct {
	view        SessionView
	lastClient  time.Time
	lastRelay   time.Time
	relayFacts  map[string]float64
	clientFacts map[string]float64
	textFacts   map[string]string
	// Hysteresis state.
	pendingSeverity rules.Severity
	pendingCount    int
	heldSeverity    rules.Severity
	heldSince       time.Time
}

type broadcastState struct {
	key        string
	firstSeen  time.Time
	lastSeen   time.Time
	pod        string
	role       string
	relayFacts map[string]float64
	sessions   map[string]*sessionState
}

// Projection holds the fleet's current state.
type Projection struct {
	mu     sync.Mutex
	now    func() time.Time
	bcasts map[string]*broadcastState
	ended  []BroadcastView
	rs     []rules.Rule
}

// New builds an empty projection.
func New(now func() time.Time) *Projection {
	if now == nil {
		now = time.Now
	}
	return &Projection{now: now, bcasts: map[string]*broadcastState{}, rs: rules.Playbook()}
}

// ObserveClient folds one accepted batch into the projection. Called on the
// ingest goroutine, so it does only map work.
func (p *Projection) ObserveClient(a ingest.Accepted, browser, os, appVersion string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()

	b := p.broadcast(a.BroadcastKey, now)
	s, ok := b.sessions[a.SessionID]
	if !ok {
		s = &sessionState{
			view: SessionView{
				SessionID: a.SessionID, BroadcastKey: a.BroadcastKey, Role: a.Role,
				Browser: browser, OS: os, AppVersion: appVersion, StartedAtMs: a.StartedAtMs,
			},
			clientFacts: map[string]float64{},
			relayFacts:  map[string]float64{},
			textFacts:   map[string]string{},
		}
		b.sessions[a.SessionID] = s
	}
	s.lastClient = now
	b.lastSeen = now

	// The LAST sample in the batch is "now" for this session.
	if n := len(a.Samples); n > 0 {
		stats := a.Samples[n-1].Stats
		for _, f := range liveNumericFields {
			if v, ok := schema.Number(stats, f); ok {
				s.clientFacts[f] = v
			}
		}
		if ab := schema.Nested(stats, "audioBuffer"); ab != nil {
			if v, ok := schema.Number(ab, "overflowDrops"); ok {
				s.clientFacts["audioOverflowDrops"] = v
			}
			if v, ok := schema.Number(ab, "gapsConcealed"); ok {
				s.clientFacts["audioGapsConcealed"] = v
			}
		}
		if hw, ok := schema.Bool(stats, "isHardwareAccelerated"); ok {
			s.clientFacts["isHardwareAccelerated"] = boolToF(hw)
		}
		for _, f := range liveTextFields {
			if v, ok := schema.String(stats, f); ok {
				s.textFacts[f] = v
			}
		}
	}
}

// ObserveRelay folds a scrape round into the projection.
func (p *Projection) ObserveRelay(obs []relayscrape.Observation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()

	for _, o := range obs {
		b := p.broadcast(o.BroadcastKey, now)
		b.lastSeen = now
		if o.Pod != "" {
			b.pod = o.Pod
		}
		if o.Role != "" {
			b.role = o.Role
		}
		switch o.Kind {
		case "broadcast":
			if o.Broadcast == nil {
				continue
			}
			bc := o.Broadcast
			b.relayFacts["publisherActive"] = boolToF(bc.PublisherActive)
			b.relayFacts["subscribers"] = float64(bc.Subscribers)
			b.relayFacts["viewersGlobal"] = float64(bc.ViewersGlobal)
			b.relayFacts["framesRelayed"] = float64(bc.FramesRelayed)
			b.relayFacts["datagramsDropped"] = float64(bc.DatagramsDropped)
			b.relayFacts["bandwidthDroppedDatagrams"] = float64(bc.BandwidthDroppedDatagrams)
			b.relayFacts["ingressFramesLost"] = float64(bc.IngressFramesLost)
			b.relayFacts["keyframeStreamsIn"] = float64(bc.KeyframeStreamsIn)
			if bc.FramesRelayed > 0 {
				b.relayFacts["ingressLossRatio"] =
					float64(bc.IngressFramesLost) / float64(bc.IngressFramesLost+bc.FramesRelayed)
			} else {
				b.relayFacts["ingressLossRatio"] = 0
			}
			// Dropping subscribers, for the "all of them" discriminator.
			dropping := 0
			for _, sub := range bc.SubscriberDetails {
				if !sub.Internal && sub.Dropped > 0 {
					dropping++
				}
			}
			b.relayFacts["subscribersDropping"] = float64(dropping)
			b.relayFacts["subscribersFleetTotal"] = float64(bc.Subscribers)
		case "subscriber":
			if o.Subscriber == nil || o.SessionID == "" {
				continue
			}
			s, ok := b.sessions[o.SessionID]
			if !ok {
				// The relay sees a subscriber the client side never reported.
				// That is a REAL state (an old client, a blocked endpoint) and
				// must render as `unknown`, never be dropped on the floor.
				s = &sessionState{
					view: SessionView{
						SessionID: o.SessionID, BroadcastKey: o.BroadcastKey, Role: "viewer",
					},
					clientFacts: map[string]float64{},
					relayFacts:  map[string]float64{},
					textFacts:   map[string]string{},
				}
				b.sessions[o.SessionID] = s
			}
			sub := o.Subscriber
			s.lastRelay = now
			s.relayFacts["subscriberDropped"] = float64(sub.Dropped)
			s.relayFacts["queueDepth"] = float64(sub.QueueDepth)
			s.relayFacts["keyframesDropped"] = float64(sub.KeyframesDropped)
			s.relayFacts["carrierQueueOverflow"] = float64(sub.CarrierQueueOverflow)
			s.relayFacts["carrierRecordsDropped"] = float64(sub.CarrierRecordsDropped)
			s.relayFacts["dvrResyncs"] = float64(sub.DVRResyncs)
			s.relayFacts["dvrLagMs"] = float64(sub.DVRLagMs)
		}
	}

	// Peer medians, for the leg-B contrast. Computed here because it is a
	// cross-session fact no single observation carries.
	for _, b := range p.bcasts {
		var drops []float64
		for _, s := range b.sessions {
			if v, ok := s.relayFacts["subscriberDropped"]; ok {
				drops = append(drops, v)
			}
		}
		if len(drops) == 0 {
			continue
		}
		sort.Float64s(drops)
		med := drops[len(drops)/2]
		for _, s := range b.sessions {
			s.relayFacts["peerMedianDropped"] = med
			s.relayFacts["ingressLossRatio"] = b.relayFacts["ingressLossRatio"]
			s.relayFacts["publisherActive"] = b.relayFacts["publisherActive"]
		}
	}
}

func (p *Projection) broadcast(key string, now time.Time) *broadcastState {
	b, ok := p.bcasts[key]
	if !ok {
		b = &broadcastState{
			key: key, firstSeen: now, relayFacts: map[string]float64{},
			sessions: map[string]*sessionState{},
		}
		p.bcasts[key] = b
	}
	return b
}

// EndSession removes a finished session from the live view.
func (p *Projection) EndSession(broadcastKey, sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b, ok := p.bcasts[broadcastKey]; ok {
		delete(b.sessions, sessionID)
	}
}

// NoteEnded moves a broadcast into the recessed history group with its stored
// verdict (owner decision, §4.8.1). Served from there rather than
// recomputed — it shows the FINAL verdict, and it costs nothing.
func (p *Projection) NoteEnded(v BroadcastView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v.Lifecycle = "ended"
	p.ended = append([]BroadcastView{v}, p.ended...)
	if len(p.ended) > MaxRecentEnded {
		p.ended = p.ended[:MaxRecentEnded]
	}
}

// Snapshot renders the current fleet, evaluating the SAME rules diagnose()
// uses (§4.8.3). One engine: two disagreeing truths about one stream would be
// worse than no dashboard at all.
func (p *Projection) Snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := Snapshot{AtMs: now.UnixMilli()}

	for _, b := range p.bcasts {
		bv := BroadcastView{
			BroadcastKey: b.key,
			Pod:          b.pod,
			Role:         b.role,
			UptimeMs:     now.Sub(b.firstSeen).Milliseconds(),
			Metrics:      copyF(b.relayFacts),
		}
		// Lifecycle is carried SEPARATELY from severity: a broadcaster in the
		// R1 grace period is `away`, which is not a fault.
		switch {
		case b.relayFacts["publisherActive"] == 1:
			bv.Lifecycle = "live"
		case now.Sub(b.lastSeen) < RelayStaleAfter:
			bv.Lifecycle = "away"
		default:
			bv.Lifecycle = "away"
		}

		bf := rules.NewFacts(b.key, "broadcast", "")
		for k, v := range b.relayFacts {
			bf.SetRelay(k, v)
		}
		rep := rules.Evaluate(bf, p.rs)
		bv.Severity = rep.Severity()
		bv.Findings = rep.Findings

		worst := rules.SeverityOK
		haveViewer := false
		for _, s := range b.sessions {
			sv := p.renderSession(s, now)
			bv.Sessions = append(bv.Sessions, sv)
			if sv.Role == "viewer" {
				haveViewer = true
				bv.Viewers++
				if sv.Severity.Rank() > worst.Rank() {
					worst = sv.Severity
				}
			}
		}
		if !haveViewer {
			worst = rules.SeverityUnknown
		}
		bv.WorstViewer = worst
		// The broadcaster is pinned first: it is the same kind of row, but it
		// is the one whose problems explain everyone else's.
		sort.SliceStable(bv.Sessions, func(i, j int) bool {
			a, c := bv.Sessions[i], bv.Sessions[j]
			if (a.Role == "broadcaster") != (c.Role == "broadcaster") {
				return a.Role == "broadcaster"
			}
			if a.Severity.Rank() != c.Severity.Rank() {
				return a.Severity.Rank() > c.Severity.Rank()
			}
			return a.SessionID < c.SessionID
		})
		out.Live = append(out.Live, bv)
	}

	// Severity, NOT recency, sorts the live group: problems float to the top
	// and a healthy fleet is a short quiet list.
	sort.SliceStable(out.Live, func(i, j int) bool {
		a, b := out.Live[i], out.Live[j]
		ar := maxRank(a.Severity, a.WorstViewer)
		br := maxRank(b.Severity, b.WorstViewer)
		if ar != br {
			return ar > br
		}
		return a.BroadcastKey < b.BroadcastKey
	})

	cutoff := now.Add(-EndedRetention).UnixMilli()
	for _, e := range p.ended {
		if e.EndedAgoMs > 0 && now.UnixMilli()-e.EndedAgoMs < cutoff {
			continue
		}
		out.Ended = append(out.Ended, e)
	}
	return out
}

func (p *Projection) renderSession(s *sessionState, now time.Time) SessionView {
	v := s.view
	v.Metrics = copyF(s.clientFacts)
	v.Config = copyS(s.textFacts)

	v.ClientAgeMs = ageMs(s.lastClient, now)
	v.RelayAgeMs = ageMs(s.lastRelay, now)
	v.ClientState = freshness(s.lastClient, now, ClientStaleAfter)
	v.RelayState = freshness(s.lastRelay, now, RelayStaleAfter)
	if v.RelayState == "reporting" {
		v.RelayState = "observed"
	}

	f := rules.NewFacts(v.SessionID, "session", v.Role)
	for k, val := range s.clientFacts {
		f.SetClient(k, val)
	}
	for k, val := range s.relayFacts {
		f.SetRelay(k, val)
	}
	for k, val := range s.textFacts {
		f.SetText(k, val)
	}
	rep := rules.Evaluate(f, p.rs)
	v.Findings = rep.Findings
	v.Verdict = rep.Summary()

	// Hysteresis applies to the RULE-DERIVED severity, which is a judgement
	// over noisy measurements and can flap. It deliberately does NOT apply to
	// the freshness override below: "this client has been silent for 30 s" is
	// a clock reading, not a measurement, and there is no blip to filter out.
	// Delaying it would mean showing a viewer as healthy after it demonstrably
	// stopped reporting — the exact failure the override exists to prevent.
	v.Severity = p.hysteresis(s, rep.Severity(), now)

	// A client that never reported, or that has gone quiet, is NEVER ok — no
	// matter what the rules made of the numbers the relay can still see.
	switch v.ClientState {
	case "unknown":
		v.Severity = rules.SeverityUnknown
	case "stale":
		if v.Severity == rules.SeverityOK {
			v.Severity = rules.SeverityUnknown
		}
	}
	return v
}

// hysteresis follows the project's dwell instinct (R4, R27): a state must hold
// for EscalateSamples consecutive evaluations to escalate, and clears only
// after ClearDwell. The asymmetry is deliberate for an ops view — problems
// should appear promptly and must not vanish before the human finishes looking
// at them.
func (p *Projection) hysteresis(s *sessionState, computed rules.Severity, now time.Time) rules.Severity {
	if s.heldSeverity == "" {
		s.heldSeverity = computed
		s.heldSince = now
		s.pendingSeverity = computed
		s.pendingCount = 1
		return computed
	}
	if computed == s.heldSeverity {
		s.pendingSeverity = computed
		s.pendingCount = 0
		s.heldSince = now
		return s.heldSeverity
	}
	if computed != s.pendingSeverity {
		s.pendingSeverity = computed
		s.pendingCount = 1
	} else {
		s.pendingCount++
	}

	worse := computed.Rank() > s.heldSeverity.Rank()
	if worse {
		if s.pendingCount >= EscalateSamples {
			s.heldSeverity = computed
			s.heldSince = now
			s.pendingCount = 0
		}
		return s.heldSeverity
	}
	// Improving: hold the old (worse) state through the dwell.
	if now.Sub(s.heldSince) >= ClearDwell {
		s.heldSeverity = computed
		s.heldSince = now
		s.pendingCount = 0
	}
	return s.heldSeverity
}

func freshness(last, now time.Time, staleAfter time.Duration) string {
	if last.IsZero() {
		return "unknown"
	}
	if now.Sub(last) > staleAfter {
		return "stale"
	}
	return "reporting"
}

func ageMs(last, now time.Time) int64 {
	if last.IsZero() {
		return -1
	}
	return now.Sub(last).Milliseconds()
}

func maxRank(a, b rules.Severity) int {
	if a.Rank() > b.Rank() {
		return a.Rank()
	}
	return b.Rank()
}

func boolToF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func copyF(m map[string]float64) map[string]float64 {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyS(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// The curated set a live row carries. Deliberately small: the live page is a
// scan surface, and the full firehose is a different question (D10).
var liveNumericFields = []string{
	"receivedFps", "decoderFps", "renderedFps", "decoderQueueDepth",
	"timeSinceLastFrameMs", "timeSinceLastInboundMs", "lastKeyframeAgeMs",
	"capToRenderMs", "liveEdgeDriftMs", "playoutOffsetMs", "arrivalJitterMs",
	"reorderGapResyncs", "keyframeStreamsReceived", "avSkewMs", "viewerCount",
	"audioPacketsReceived",
	"captureFps", "encoderFps", "sentFps", "encoderQueueDepth",
	"EncoderFps", "SentFps",
}

var liveTextFields = []string{
	"deliveryMode", "playoutMode", "renderer", "pipelineContext", "transport",
	"audioState", "autoRung", "Encoder", "Codec",
}
