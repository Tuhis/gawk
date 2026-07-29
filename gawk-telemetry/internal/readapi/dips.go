package readapi

// TH9's dip explainer (docs/36 UD21).
//
// Turning *"your fps dipped"* into *"and keyframe drops went 0 → 7 while
// ingress loss went 0 → 1.2 %"*, plus a check of whether other viewers on the
// same broadcast dipped in the same window.
//
// # The line this file must not cross
//
// Two viewers dipping together is **evidence about a shared leg, not proof of
// one**. Everything here reports CO-OCCURRENCE and states its own confidence;
// nothing here says "because". With a single reporting viewer there is no
// correlation to draw at all, and saying so is the honest output — not a
// coincidence dressed as a finding. The wording is an acceptance criterion, not
// an intention, which is why the sentences are built here rather than left to
// the UI to compose.

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/rollup"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/schema"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/sessions"
)

// MoverThreshold is how far a signal must move inside a dip to be worth naming.
//
// A gauge must shift by a fifth of its own out-of-window level; a counter must
// advance at 1.5× the rate the rest of the session ran at. Both are
// SELF-relative for the same reason DipRatio is: an absolute floor accuses a
// slow stream forever and misses a fast one collapsing.
const (
	MoverGaugeRatio   = 0.2
	MoverCounterRatio = 1.5
	// MaxMovers bounds one explanation. Ranked by magnitude, so the cut is
	// always from the bottom — and stated, so a reader knows more moved.
	MaxMovers = 12
)

// DipReport explains one session's dips.
type DipReport struct {
	SessionID    string           `json:"sessionId"`
	BroadcastKey string           `json:"broadcastKey"`
	Role         string           `json:"role"`
	Primary      string           `json:"primary"`
	Episodes     []DipExplanation `json:"episodes"`
	// Note carries the reason there is nothing to explain, which is never the
	// same as "nothing was wrong" (UD10).
	Note string `json:"note,omitempty"`
}

// DipExplanation is one episode, explained.
type DipExplanation struct {
	FromMs     int64   `json:"fromMs"`
	ToMs       int64   `json:"toMs"`
	DurationMs float64 `json:"durationMs"`
	WorstValue float64 `json:"worstValue"`
	Baseline   float64 `json:"baseline"`
	// Movers are every signal that moved materially inside the window, ranked
	// by magnitude, client and relay together.
	Movers        []Mover `json:"movers,omitempty"`
	MoversOmitted int     `json:"moversOmitted,omitempty"`
	// Correlation is the cross-session half. Always present, because "there was
	// nobody else to compare with" is an answer.
	Correlation Correlation `json:"correlation"`
}

// Mover is one signal that moved inside a dip.
type Mover struct {
	Signal string `json:"signal"`
	// From/To are the readings at the window's edges (counters) or the
	// out-of-window and in-window levels (gauges). Both, because "drops rose by
	// 7" and "drops went 0 → 7" are different amounts of information.
	From float64 `json:"from"`
	To   float64 `json:"to"`
	// Magnitude is the ranking key: how far this moved relative to its own
	// ordinary level. Dimensionless on purpose — it is the only way to rank a
	// frame count beside a loss percentage without inventing a common unit.
	Magnitude float64 `json:"magnitude"`
	Unit      string  `json:"unit,omitempty"`
	Semantic  string  `json:"semantic"`
	// From which side. A relay-provenance mover is worth more than a client one
	// for exactly the reason D7 gives, and the chip on screen says which.
	Provenance string `json:"provenance"`
}

// Correlation is the co-occurrence check across the broadcast.
type Correlation struct {
	// PeersReporting is how many OTHER sessions on this broadcast had samples
	// overlapping the window at all. Zero is the honest common case.
	PeersReporting int       `json:"peersReporting"`
	PeersDipped    int       `json:"peersDipped"`
	Peers          []PeerDip `json:"peers,omitempty"`
	// Confidence is 0..1 and is about the CORRELATION, not about the dip. With
	// one reporting viewer it is 0 and the statement says why.
	Confidence float64 `json:"confidence"`
	// Statement is co-occurrence wording, never causal. Built here so the rule
	// against manufacturing a causal story is enforced in one place.
	Statement string `json:"statement"`
}

// PeerDip is one other session's behaviour in the same window.
type PeerDip struct {
	SessionID string `json:"sessionId"`
	Role      string `json:"role"`
	Reporting bool   `json:"reporting"`
	Dipped    bool   `json:"dipped"`
	// WorstValue is that peer's lowest primary reading inside the window.
	WorstValue float64 `json:"worstValue,omitempty"`
	Baseline   float64 `json:"baseline,omitempty"`
}

// ExplainDips builds TH9's report for one session.
func (a *API) ExplainDips(sessionID string, since time.Time) (*DipReport, error) {
	ref, err := a.store.FindSession(sessionID)
	if err != nil {
		return nil, err
	}
	lines, err := a.store.ReadSession(ref)
	if err != nil {
		return nil, err
	}
	in := sessions.ParseTimeline(sessions.Live{Ref: ref}, lines)
	primary, counters := rollup.PrimarySeries(in.Role)
	rep := &DipReport{
		SessionID: sessionID, BroadcastKey: in.BroadcastKey,
		Role: in.Role, Primary: primary, Episodes: []DipExplanation{},
	}
	eps, spans := rollup.DetectEpisodeSpans(in.Samples, primary, counters)
	if eps == nil {
		// The detector's own guards: too few samples, or a baseline too low to
		// have a meaningful steady state. "We could not look" is not "nothing
		// was wrong", and the note is what keeps the two apart.
		rep.Note = "this session has no baseline to judge dips against — too few samples, or a rate too low for a halving to mean anything"
		return rep, nil
	}
	if len(spans) == 0 {
		rep.Note = fmt.Sprintf("no dips: this session held its baseline of %.0f %s throughout", eps.Baseline, primary)
		return rep, nil
	}

	offset := clockOffset(lines, in.StartedAtMs)
	at := func(tMs float64) int64 { return in.StartedAtMs + int64(tMs) + int64(offset) }
	relayLines := a.relayLinesForSpan(in.StartedAtMs, in.EndedAtMs)
	peers := a.peerSessions(in.BroadcastKey, sessionID, since)

	for _, sp := range spans {
		ex := DipExplanation{
			FromMs: at(sp.StartMs), ToMs: at(sp.EndMs),
			DurationMs: sp.DurationMs, WorstValue: sp.WorstValue, Baseline: sp.Baseline,
		}
		movers := clientMovers(in.Samples, sp, primary)
		movers = append(movers, relayMovers(relayLines, sessionID, at(sp.StartMs), at(sp.EndMs))...)
		sort.SliceStable(movers, func(i, j int) bool { return movers[i].Magnitude > movers[j].Magnitude })
		if len(movers) > MaxMovers {
			ex.MoversOmitted = len(movers) - MaxMovers
			movers = movers[:MaxMovers]
		}
		ex.Movers = movers
		ex.Correlation = correlate(peers, at(sp.StartMs), at(sp.EndMs))
		rep.Episodes = append(rep.Episodes, ex)
	}
	return rep, nil
}

// clientMovers ranks the client-side signals that moved inside one dip.
//
// Counters and gauges are compared differently because they mean different
// things: a counter's question is "did it advance faster than usual", a gauge's
// is "did its level change". Applying either test to the other kind produces
// confident nonsense, which is the failure this whole item is trying to reduce
// rather than add to.
func clientMovers(samples []rollup.Sample, sp rollup.Span, primary string) []Mover {
	inside, outside := splitByWindow(samples, sp.StartMs, sp.EndMs)
	if len(inside) == 0 {
		return nil
	}
	catalogue := map[string]schema.Field{}
	for _, f := range schema.Catalogue() {
		catalogue[f.Name] = f
	}
	windowMs := sp.EndMs - sp.StartMs
	sessionMs := 0.0
	if n := len(samples); n > 1 {
		sessionMs = samples[n-1].TMs - samples[0].TMs
	}

	var out []Mover
	for name := range observedNumericFields(samples) {
		if name == primary {
			// The primary rate is what DEFINED the dip. Listing it as evidence
			// that the dip happened is circular, and it crowds out the signals
			// that actually localize the cause.
			continue
		}
		f, known := catalogue[name]
		sem := "gauge"
		if known {
			sem = string(f.Semantic)
		}
		switch sem {
		case string(schema.SemCounter):
			from, okFrom := valueAt(samples, sp.StartMs, name, true)
			to, okTo := valueAt(samples, sp.EndMs, name, false)
			if !okFrom || !okTo || to < from {
				continue
			}
			delta := to - from
			if delta <= 0 {
				continue
			}
			// Expected advance if this counter had ticked at the session's own
			// average rate through the window. A counter that did exactly that
			// did not "move" — it kept doing what it was doing.
			total, okTotal := counterTotal(samples, name)
			expected := 0.0
			if okTotal && sessionMs > 0 && windowMs > 0 {
				expected = total * (windowMs / sessionMs)
			}
			mag := math.Inf(1)
			if expected > 0 {
				mag = delta / expected
			}
			if expected > 0 && mag < MoverCounterRatio {
				continue
			}
			if math.IsInf(mag, 1) {
				// No baseline rate at all: the counter did nothing all session
				// and then moved. That is the strongest shape there is, and it
				// is ranked by the raw advance so two of them still order.
				mag = 10 + delta
			}
			out = append(out, Mover{
				Signal: name, From: from, To: to, Magnitude: mag,
				Unit: f.Unit, Semantic: "counter", Provenance: "client",
			})
		case string(schema.SemGauge):
			inLvl, okIn := medianOf(inside, name)
			outLvl, okOut := medianOf(outside, name)
			if !okIn || !okOut {
				continue
			}
			denom := math.Abs(outLvl)
			if denom < 1e-9 {
				denom = 1e-9
			}
			mag := math.Abs(inLvl-outLvl) / denom
			if mag < MoverGaugeRatio {
				continue
			}
			out = append(out, Mover{
				Signal: name, From: outLvl, To: inLvl, Magnitude: mag,
				Unit: f.Unit, Semantic: "gauge", Provenance: "client",
			})
		}
	}
	return out
}

// relayMovers adds the relay's own counters across the same absolute window.
// Relay provenance is the anchor (D7), so these matter more than anything above
// even when their magnitude is smaller — the UI's chip is what carries that.
func relayMovers(lines [][]byte, sessionID string, fromMs, toMs int64) []Mover {
	type pair struct{ first, last float64 }
	seen := map[string]*pair{}
	record := func(name string, v float64) {
		p, ok := seen[name]
		if !ok {
			seen[name] = &pair{first: v, last: v}
			return
		}
		p.last = v
	}
	var any bool
	for _, ln := range lines {
		var o relayObservation
		if err := json.Unmarshal(ln, &o); err != nil {
			continue
		}
		if o.AtMs < fromMs || o.AtMs > toMs {
			continue
		}
		if o.SessionID == sessionID && o.Subscriber != nil {
			any = true
			record("subscriberDropped", float64(o.Subscriber.Dropped))
			record("keyframesDropped", float64(o.Subscriber.KeyframesDropped))
			record("carrierQueueOverflow", float64(o.Subscriber.CarrierQueueOverflow))
			record("carrierRecordsDropped", float64(o.Subscriber.CarrierRecordsDropped))
			record("dvrResyncs", float64(o.Subscriber.DVRResyncs))
			record("queueDepth", float64(o.Subscriber.QueueDepth))
		}
		if o.Broadcast != nil && o.Role != "edge" {
			any = true
			record("ingressFramesLost", float64(o.Broadcast.IngressFramesLost))
			record("datagramsDropped", float64(o.Broadcast.DatagramsDropped))
			record("bandwidthDroppedDatagrams", float64(o.Broadcast.BandwidthDroppedDatagrams))
		}
	}
	if !any {
		return nil
	}
	var out []Mover
	for name, p := range seen {
		if p.last <= p.first {
			continue
		}
		out = append(out, Mover{
			Signal: name, From: p.first, To: p.last,
			// Any relay-side advance inside a dip window is material by
			// construction: the scrape cadence is coarse enough that a counter
			// moving at all inside a few seconds is the event, not the noise.
			Magnitude:  5 + (p.last - p.first),
			Semantic:   "counter",
			Provenance: "relay",
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Signal < out[j].Signal })
	return out
}

// peerSession is one other participant, pre-loaded so every episode can be
// checked against it without re-reading a file per dip.
type peerSession struct {
	sessionID string
	role      string
	primary   string
	startedAt int64
	offset    float64
	samples   []rollup.Sample
	baseline  float64
}

// peerSessions loads the other sessions on the same broadcast.
func (a *API) peerSessions(broadcastKey, exclude string, since time.Time) []peerSession {
	if broadcastKey == "" {
		return nil
	}
	rows, err := a.rollups(since)
	if err != nil {
		return nil
	}
	var out []peerSession
	for _, r := range rows {
		if r.BroadcastKey != broadcastKey || r.SessionID == exclude {
			continue
		}
		if len(out) >= MaxLanes {
			break
		}
		ref, err := a.store.FindSession(r.SessionID)
		if err != nil {
			continue
		}
		lines, err := a.store.ReadSession(ref)
		if err != nil {
			continue
		}
		in := sessions.ParseTimeline(sessions.Live{Ref: ref}, lines)
		primary, _ := rollup.PrimarySeries(in.Role)
		p := peerSession{
			sessionID: r.SessionID, role: in.Role, primary: primary,
			startedAt: in.StartedAtMs, offset: clockOffset(lines, in.StartedAtMs),
			samples: in.Samples,
		}
		if b, ok := medianOf(in.Samples, primary); ok {
			p.baseline = b
		}
		out = append(out, p)
	}
	return out
}

// correlate is the co-occurrence check, and the one place the wording rule is
// enforced.
func correlate(peers []peerSession, fromMs, toMs int64) Correlation {
	c := Correlation{}
	for _, p := range peers {
		pd := PeerDip{SessionID: p.sessionID, Role: p.role, Baseline: p.baseline}
		var worst = math.Inf(1)
		for _, s := range p.samples {
			at := p.startedAt + int64(s.TMs) + int64(p.offset)
			if at < fromMs || at > toMs {
				continue
			}
			pd.Reporting = true
			if v, ok := schema.Number(s.Stats, p.primary); ok && v < worst {
				worst = v
			}
		}
		if pd.Reporting {
			c.PeersReporting++
			if !math.IsInf(worst, 1) {
				pd.WorstValue = worst
				// The SAME test the detector uses, so "dipped" means one thing
				// across the whole product rather than one thing per surface.
				if p.baseline >= rollup.MinBaselineFps && worst <= p.baseline*rollup.DipRatio {
					pd.Dipped = true
					c.PeersDipped++
				}
			}
		}
		c.Peers = append(c.Peers, pd)
	}

	switch {
	case c.PeersReporting == 0:
		c.Confidence = 0
		c.Statement = "no other session on this broadcast reported inside this window, so there is no correlation to draw — this dip is unattributed, not isolated"
	case c.PeersDipped == 0:
		// Ruling a shared leg OUT is the strongest thing this check can do, and
		// its confidence rises with how many peers were watching.
		c.Confidence = confidenceFor(c.PeersReporting)
		c.Statement = fmt.Sprintf(
			"%s reported through this window and none of them dipped — consistent with a problem on this session's own leg, not the broadcaster or the relay",
			plural(c.PeersReporting, "other session", "other sessions"))
	case c.PeersDipped == c.PeersReporting:
		c.Confidence = confidenceFor(c.PeersReporting)
		c.Statement = fmt.Sprintf(
			"all %s reporting in this window dipped at the same time — co-occurrence consistent with a shared leg (the broadcaster or the relay). It is evidence about a shared leg, not proof of one",
			plural(c.PeersReporting, "other session", "other sessions"))
	default:
		c.Confidence = confidenceFor(c.PeersReporting) * 0.7
		c.Statement = fmt.Sprintf(
			"%d of %d other sessions reporting in this window dipped at the same time — a partial co-occurrence, which is consistent with either a shared leg affecting some viewers or with separate problems coinciding",
			c.PeersDipped, c.PeersReporting)
	}
	return c
}

// confidenceFor scales with how many peers were watching. One peer is a
// coincidence you should not lean on; four agreeing is worth something. It
// never reaches 1: this is co-occurrence, and co-occurrence is never certainty.
func confidenceFor(peers int) float64 {
	switch {
	case peers <= 0:
		return 0
	case peers == 1:
		return 0.35
	case peers == 2:
		return 0.55
	case peers == 3:
		return 0.7
	default:
		return 0.8
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// relayLinesForSpan reads the relay partitions a session's span touches.
func (a *API) relayLinesForSpan(fromMs, toMs int64) [][]byte {
	var out [][]byte
	for _, date := range datesBetween(fromMs, toMs) {
		if lines, err := a.store.ReadRelay(date); err == nil {
			out = append(out, lines...)
		}
	}
	return out
}

// --- small numeric helpers -------------------------------------------------

func splitByWindow(samples []rollup.Sample, fromMs, toMs float64) (inside, outside []rollup.Sample) {
	for _, s := range samples {
		if s.TMs >= fromMs && s.TMs <= toMs {
			inside = append(inside, s)
		} else {
			outside = append(outside, s)
		}
	}
	return inside, outside
}

func observedNumericFields(samples []rollup.Sample) map[string]bool {
	out := map[string]bool{}
	for _, s := range samples {
		for k := range s.Stats {
			if _, ok := schema.Number(s.Stats, k); ok {
				out[k] = true
			}
		}
	}
	return out
}

// valueAt reads a field at the sample nearest a timestamp. `before` picks the
// last sample at or before it (a counter's reading at the dip's onset must
// include the sample that started it); otherwise the first at or after.
func valueAt(samples []rollup.Sample, tMs float64, field string, before bool) (float64, bool) {
	var best float64
	found := false
	for i, s := range samples {
		if before {
			if s.TMs > tMs {
				break
			}
			if v, ok := schema.Number(s.Stats, field); ok {
				best, found = v, true
			}
			// The sample BEFORE the first in-window one is the true baseline;
			// reading the onset sample itself would hide the advance the dip
			// caused. One step back where there is one.
			if i+1 < len(samples) && samples[i+1].TMs > tMs {
				break
			}
			continue
		}
		if s.TMs < tMs {
			continue
		}
		if v, ok := schema.Number(s.Stats, field); ok {
			return v, true
		}
	}
	if !before && !found {
		// Nothing at or after: fall back to the last reading there was, which
		// is what the counter finished on.
		for i := len(samples) - 1; i >= 0; i-- {
			if v, ok := schema.Number(samples[i].Stats, field); ok {
				return v, true
			}
		}
	}
	return best, found
}

func counterTotal(samples []rollup.Sample, field string) (float64, bool) {
	var first, last float64
	var okFirst, okLast bool
	for _, s := range samples {
		if v, ok := schema.Number(s.Stats, field); ok {
			if !okFirst {
				first, okFirst = v, true
			}
			last, okLast = v, true
		}
	}
	if !okFirst || !okLast || last < first {
		return 0, false
	}
	return last - first, true
}

func medianOf(samples []rollup.Sample, field string) (float64, bool) {
	var vals []float64
	for _, s := range samples {
		if v, ok := schema.Number(s.Stats, field); ok {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		return 0, false
	}
	return median(vals), true
}
