// Package rules is docs/13's bottleneck playbook as code (docs/33 D6).
//
// The playbook is a 14-row symptom → discriminating-signals → verdict table.
// It is already written, already correct, and has been the actual debugging
// procedure since R9. This executes it.
//
// Two stances are load-bearing and neither is decoration:
//
//   - **Relay numbers anchor; client numbers are testimony (D7).** This is a
//     correctness stance, not a security one — nobody is forging telemetry.
//     The point was earned the hard way in docs/20 finding 7: **a wedged
//     client's own accounting is the least reliable evidence in the system,
//     and it is exactly what a wedged client sends.** `queuedMs` was a shadow
//     of the worklet's real depth; it was confidently wrong; it drove the drop
//     decisions. So every rule declares its evidence's provenance, a rule
//     resting only on client evidence caps its own confidence, and where the
//     two sides disagree **the disagreement is itself a finding**.
//   - **Missing signals are reported, never assumed.** A rule whose required
//     signals are absent does not fire and does not silently vote; it lands in
//     `unavailable`, so a verdict never rests on evidence that does not exist.
//
// Output is a RANKED LIST of candidates with evidence, never a single asserted
// answer — and never the underlying series (D10). An MCP surface that returns
// raw samples is worse than today's copy-paste, because it spends context to
// arrive at the same place.
package rules

import (
	"fmt"
	"sort"
)

// Provenance says where a piece of evidence came from. It is what caps a
// verdict's confidence (D7).
type Provenance string

const (
	// FromRelay is a number the relay itself counted. The anchor.
	FromRelay Provenance = "relay"
	// FromClient is a number a client reported about itself. Testimony.
	FromClient Provenance = "client"
	// FromDerived is computed from more than one source.
	FromDerived Provenance = "derived"
)

// Severity is the four-state health model shared with the live dashboard
// (docs/33 §4.8.3). Deliberately few.
type Severity string

const (
	// SeverityOK is a healthy stream.
	SeverityOK Severity = "ok"
	// SeverityWarn is degraded but working.
	SeverityWarn Severity = "warn"
	// SeverityBad is broken from a viewer's point of view.
	SeverityBad Severity = "bad"
	// SeverityUnknown is "no evidence". It is NEVER ok — painting an absence
	// of evidence as green is the one thing an ops view must not do.
	SeverityUnknown Severity = "unknown"
)

// Rank orders severities for sorting. Unknown sits between ok and warn: it is
// worth looking at, but a stream known to be bad outranks one nobody can see.
func (s Severity) Rank() int {
	switch s {
	case SeverityBad:
		return 3
	case SeverityWarn:
		return 2
	case SeverityUnknown:
		return 1
	default:
		return 0
	}
}

// Evidence is one number a rule fired on, with its provenance. This is what
// makes a verdict inspectable instead of asserted.
type Evidence struct {
	Signal string  `json:"signal"`
	Value  float64 `json:"value"`
	// Text carries evidence whose value is a WORD rather than a number — a
	// codec, an acceleration mode, a delivery mode. Config is half of what
	// explains a stream, and before this the only way to cite it was to encode
	// it into a numeric's Comparison, which made the evidence unparseable by
	// anything but a human. Additive and omitempty, so stored verdicts from
	// older releases still load (D4).
	Text       string     `json:"text,omitempty"`
	Unit       string     `json:"unit,omitempty"`
	From       Provenance `json:"from"`
	Comparison string     `json:"comparison,omitempty"`
}

// Finding is one fired rule.
type Finding struct {
	ID       string   `json:"id"`
	Verdict  string   `json:"verdict"`
	Severity Severity `json:"severity"`
	// Confidence is 0..1, CAPPED when only client-provenance evidence fired
	// (D7). A verdict that no relay number corroborates cannot be as certain
	// as one that a relay counter confirms.
	Confidence float64    `json:"confidence"`
	Evidence   []Evidence `json:"evidence"`
	// Action is what docs/13's playbook says to do about it.
	Action string `json:"action,omitempty"`
}

// Report is diagnose()'s whole output. It carries verdicts and evidence,
// never the underlying series.
type Report struct {
	// Subject is the sessionId or broadcastKey analysed.
	Subject string `json:"subject"`
	Scope   string `json:"scope"` // "session" | "broadcast"
	// Healthy is true when no rule fired. It is reported POSITIVELY with the
	// checks that passed (owner decision, §8): returning nothing is honest but
	// reads as a failure, and gives the caller no way to tell a clean session
	// from an analysis that never ran.
	Healthy bool `json:"healthy"`
	// Findings, ranked worst-first.
	Findings []Finding `json:"findings"`
	// Passed names the rules that were evaluated and did not fire — the basis
	// for believing a "healthy" verdict.
	Passed []string `json:"passed,omitempty"`
	// Unavailable names rules that could not be evaluated, with the signal
	// that was missing. A verdict never silently rests on a signal that does
	// not exist.
	Unavailable []Missing `json:"unavailable,omitempty"`
	// Caveats are reasons to distrust this report as a whole: a high schema
	// anomaly count, a truncated session, missing relay coverage.
	Caveats []string `json:"caveats,omitempty"`
	// DashboardURL lets a machine hand a human something to LOOK at rather
	// than a wall of numbers.
	DashboardURL string `json:"dashboardUrl,omitempty"`
}

// Missing is one rule that could not run.
type Missing struct {
	ID      string   `json:"id"`
	Signals []string `json:"missingSignals"`
}

// Facts is everything a rule may read about one subject: the client's own
// numbers, the relay's, and the fleet's for comparison. Every lookup is
// explicitly "present or not" — there is no zero value standing in for
// "unknown", because that is precisely how a rule fires on evidence that does
// not exist.
type Facts struct {
	Subject string
	Scope   string
	Role    string

	client map[string]float64
	relay  map[string]float64
	fleet  map[string]float64
	text   map[string]string

	// Caveats accumulated by the caller (anomaly counts, coverage).
	Caveats []string
}

// NewFacts builds an empty fact set.
func NewFacts(subject, scope, role string) *Facts {
	return &Facts{
		Subject: subject, Scope: scope, Role: role,
		client: map[string]float64{},
		relay:  map[string]float64{},
		fleet:  map[string]float64{},
		text:   map[string]string{},
	}
}

// SetClient records a client-reported number (testimony).
func (f *Facts) SetClient(name string, v float64) { f.client[name] = v }

// SetRelay records a relay-counted number (the anchor).
func (f *Facts) SetRelay(name string, v float64) { f.relay[name] = v }

// SetFleet records the fleet median of a signal, for comparison.
func (f *Facts) SetFleet(name string, v float64) { f.fleet[name] = v }

// SetText records a configuration string (delivery mode, renderer…).
func (f *Facts) SetText(name, v string) { f.text[name] = v }

// Text reads a configuration string.
func (f *Facts) Text(name string) (string, bool) { v, ok := f.text[name]; return v, ok }

// Client reads a client-reported number.
func (f *Facts) Client(name string) (float64, bool) { v, ok := f.client[name]; return v, ok }

// Relay reads a relay-counted number.
func (f *Facts) Relay(name string) (float64, bool) { v, ok := f.relay[name]; return v, ok }

// Fleet reads a fleet median.
func (f *Facts) Fleet(name string) (float64, bool) { v, ok := f.fleet[name]; return v, ok }

// Names lists every fact present, qualified by side. It exists so a producer
// can be checked against the inventory a rule is allowed to require
// (ProducibleFacts): review finding 5 was a rule requiring a signal nothing
// anywhere produced, which is invisible at runtime — the rule simply reads
// `unavailable` forever, honestly and uselessly.
func (f *Facts) Names() []string {
	out := make([]string, 0, len(f.client)+len(f.relay)+len(f.fleet)+len(f.text))
	for _, pair := range []struct {
		side string
		m    map[string]float64
	}{{"client", f.client}, {"relay", f.relay}, {"fleet", f.fleet}} {
		for k := range pair.m {
			out = append(out, pair.side+"."+k)
		}
	}
	for k := range f.text {
		out = append(out, "text."+k)
	}
	sort.Strings(out)
	return out
}

// Signal names in a rule's `Requires` are qualified: "relay.x" or "client.x".
// Qualifying them is what makes "which side is missing?" answerable — an
// unqualified name would hide the difference between "the relay is not
// scraped" and "this client stopped reporting", which are completely
// different problems.
func (f *Facts) has(qualified string) bool {
	side, name, ok := splitSignal(qualified)
	if !ok {
		return false
	}
	switch side {
	case "relay":
		_, present := f.relay[name]
		return present
	case "client":
		_, present := f.client[name]
		return present
	case "fleet":
		_, present := f.fleet[name]
		return present
	case "text":
		_, present := f.text[name]
		return present
	}
	return false
}

func splitSignal(q string) (side, name string, ok bool) {
	for i := 0; i < len(q); i++ {
		if q[i] == '.' {
			return q[:i], q[i+1:], true
		}
	}
	return "", "", false
}

// Rule is one playbook row.
type Rule struct {
	// ID is stable: it appears in stored verdicts, so renaming one breaks the
	// "has this got better since the R15 fix?" query the rollups exist for.
	ID string
	// Scope limits a rule to sessions of one role, or to a broadcast.
	Scope string // "viewer" | "broadcaster" | "broadcast" | "any"
	// Requires are the QUALIFIED signals the predicate reads. A rule whose
	// requirements are not all present does not run, and says so.
	Requires []string
	// Verdict is the playbook's conclusion, in the playbook's words.
	Verdict string
	// Action is what to do about it.
	Action string
	// Eval returns the finding, or nil if the rule did not fire.
	Eval func(f *Facts) *Finding

	// --- UD20: read-only transparency (docs/36 TH6) -----------------------
	//
	// A rule's thresholds and reasoning were previously legible only by reading
	// this package. The catalogue makes them a surface — read-only, because a
	// stored verdict was computed under the thresholds of ITS day, and an
	// editable threshold would make history and live disagree unless every
	// verdict recorded the config it ran under.

	// Why is one paragraph on what this rule is looking for and why that
	// signature means what it claims. Written for the operator reading the
	// catalogue at 21:00, not for a reviewer of this file.
	Why string
	// Thresholds are the constants the predicate actually compares against.
	// They MUST be the package constants themselves, never re-typed literals:
	// a second copy is exactly the drift D15 names, and here it would put a
	// number on screen that no verdict was ever computed with.
	Thresholds []Threshold
}

// Threshold is one named constant a rule compares against.
type Threshold struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit,omitempty"`
	Note  string  `json:"note,omitempty"`
}

// RuleDoc is one catalogue entry — a Rule without its closure, so it can be
// serialized.
type RuleDoc struct {
	ID       string   `json:"id"`
	Scope    string   `json:"scope"`
	Verdict  string   `json:"verdict"`
	Action   string   `json:"action,omitempty"`
	Why      string   `json:"why,omitempty"`
	Requires []string `json:"requires"`
	// Provenance is which sides this rule's inputs come from, derived from
	// Requires rather than declared — so it cannot disagree with what the rule
	// actually reads. A rule with no relay input has its confidence capped
	// (D7), and the catalogue says so before anyone has to wonder.
	Provenance []string    `json:"provenance"`
	Thresholds []Threshold `json:"thresholds,omitempty"`
	// ClientOnly repeats D7's cap explicitly: this rule can never claim more
	// than clientOnlyConfidenceCap, because no relay number can corroborate it.
	ClientOnly bool `json:"clientOnly"`
	// MaxConfidence is that cap as a number.
	MaxConfidence float64 `json:"maxConfidence"`
}

// Catalogue renders the playbook as documentation (UD20).
func Catalogue(rs []Rule) []RuleDoc {
	out := make([]RuleDoc, 0, len(rs))
	for _, r := range rs {
		doc := RuleDoc{
			ID: r.ID, Scope: r.Scope, Verdict: r.Verdict, Action: r.Action,
			Why: r.Why, Requires: r.Requires, Thresholds: r.Thresholds,
			MaxConfidence: 1,
		}
		if doc.Requires == nil {
			doc.Requires = []string{}
		}
		sides := map[string]bool{}
		for _, sig := range r.Requires {
			if side, _, ok := splitSignal(sig); ok {
				sides[side] = true
			}
		}
		for _, side := range []string{"relay", "client", "fleet", "text"} {
			if sides[side] {
				doc.Provenance = append(doc.Provenance, side)
			}
		}
		if doc.Provenance == nil {
			doc.Provenance = []string{}
		}
		// A rule that requires no relay signal cannot produce relay-anchored
		// evidence, so capConfidence will always cap it. Stating that here is
		// what turns "why is this only 0.6?" from a mystery into a rule.
		if !sides["relay"] {
			doc.ClientOnly = true
			doc.MaxConfidence = clientOnlyConfidenceCap
		}
		out = append(out, doc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Trace is what one rule did on one subject (UD20's per-session trace).
type Trace struct {
	ID    string `json:"id"`
	Scope string `json:"scope"`
	// Outcome is fired | passed | unavailable | out-of-scope.
	Outcome string `json:"outcome"`
	// Read is every REQUIRED signal's value as the rule saw it. This is what
	// makes a non-firing rule explicable in terms of numbers rather than in
	// terms of trust — TH6's criterion, and the mitigation for the standing
	// risk that one engine is now wrong in more places at once.
	Read     map[string]float64 `json:"read,omitempty"`
	ReadText map[string]string  `json:"readText,omitempty"`
	Missing  []string           `json:"missing,omitempty"`
	// Severity is set when the rule fired.
	Severity Severity `json:"severity,omitempty"`
}

// EvaluateTrace is Evaluate plus a per-rule account of what happened.
//
// It runs the SAME loop rather than a parallel one: a trace produced by a
// second implementation would eventually explain a verdict the engine did not
// reach, which is worse than no trace at all.
func EvaluateTrace(f *Facts, rs []Rule) (Report, []Trace) {
	traces := make([]Trace, 0, len(rs))
	rep := Report{Subject: f.Subject, Scope: f.Scope, Caveats: f.Caveats}
	for _, r := range rs {
		t := Trace{ID: r.ID, Scope: r.Scope}
		if !scopeMatches(r.Scope, f) {
			t.Outcome = "out-of-scope"
			traces = append(traces, t)
			continue
		}
		t.Read, t.ReadText = f.readAll(r.Requires)
		var missing []string
		for _, sig := range r.Requires {
			if !f.has(sig) {
				missing = append(missing, sig)
			}
		}
		if len(missing) > 0 {
			rep.Unavailable = append(rep.Unavailable, Missing{ID: r.ID, Signals: missing})
			t.Outcome, t.Missing = "unavailable", missing
			traces = append(traces, t)
			continue
		}
		finding := r.Eval(f)
		if finding == nil {
			rep.Passed = append(rep.Passed, r.ID)
			t.Outcome = "passed"
			traces = append(traces, t)
			continue
		}
		finding.ID = r.ID
		if finding.Verdict == "" {
			finding.Verdict = r.Verdict
		}
		if finding.Action == "" {
			finding.Action = r.Action
		}
		finding.Confidence = capConfidence(finding.Confidence, finding.Evidence)
		rep.Findings = append(rep.Findings, *finding)
		t.Outcome, t.Severity = "fired", finding.Severity
		traces = append(traces, t)
	}
	rankReport(&rep)
	sort.Slice(traces, func(i, j int) bool { return traces[i].ID < traces[j].ID })
	return rep, traces
}

// readAll snapshots the values behind a rule's required signals.
func (f *Facts) readAll(requires []string) (map[string]float64, map[string]string) {
	var nums map[string]float64
	var texts map[string]string
	for _, sig := range requires {
		side, name, ok := splitSignal(sig)
		if !ok {
			continue
		}
		if side == "text" {
			if v, present := f.text[name]; present {
				if texts == nil {
					texts = map[string]string{}
				}
				texts[sig] = v
			}
			continue
		}
		var m map[string]float64
		switch side {
		case "relay":
			m = f.relay
		case "client":
			m = f.client
		case "fleet":
			m = f.fleet
		}
		if v, present := m[name]; present {
			if nums == nil {
				nums = map[string]float64{}
			}
			nums[sig] = v
		}
	}
	return nums, texts
}

// Evaluate runs the rule set over one subject and ranks the result.
func Evaluate(f *Facts, rs []Rule) Report {
	rep := Report{Subject: f.Subject, Scope: f.Scope, Caveats: f.Caveats}
	for _, r := range rs {
		if !scopeMatches(r.Scope, f) {
			continue
		}
		var missing []string
		for _, sig := range r.Requires {
			if !f.has(sig) {
				missing = append(missing, sig)
			}
		}
		if len(missing) > 0 {
			rep.Unavailable = append(rep.Unavailable, Missing{ID: r.ID, Signals: missing})
			continue
		}
		finding := r.Eval(f)
		if finding == nil {
			rep.Passed = append(rep.Passed, r.ID)
			continue
		}
		finding.ID = r.ID
		if finding.Verdict == "" {
			finding.Verdict = r.Verdict
		}
		if finding.Action == "" {
			finding.Action = r.Action
		}
		finding.Confidence = capConfidence(finding.Confidence, finding.Evidence)
		rep.Findings = append(rep.Findings, *finding)
	}

	rankReport(&rep)
	return rep
}

// rankReport is the ordering and the positive verdict, shared by Evaluate and
// EvaluateTrace.
//
// Extracted rather than duplicated because the two must produce IDENTICAL
// reports: a trace that explained a verdict the engine did not reach would be
// worse than no trace, and a test pins the two together.
func rankReport(rep *Report) {
	// Worst first, then by confidence: an operator reads top-down and must hit
	// the thing most worth acting on.
	sort.SliceStable(rep.Findings, func(i, j int) bool {
		a, b := rep.Findings[i], rep.Findings[j]
		if a.Severity.Rank() != b.Severity.Rank() {
			return a.Severity.Rank() > b.Severity.Rank()
		}
		return a.Confidence > b.Confidence
	})
	sort.Strings(rep.Passed)
	sort.Slice(rep.Unavailable, func(i, j int) bool { return rep.Unavailable[i].ID < rep.Unavailable[j].ID })

	// The positive verdict (owner decision, §8). "No issues found" plus the
	// checks that support it, so a caller can tell a clean session from an
	// analysis that never ran — which `Unavailable` right beside it makes
	// explicit.
	rep.Healthy = len(rep.Findings) == 0
}

// capConfidence enforces D7: a finding resting ONLY on client-reported
// evidence cannot claim high confidence, because a wedged client's own
// accounting is the least reliable evidence in the system.
const clientOnlyConfidenceCap = 0.6

func capConfidence(c float64, ev []Evidence) float64 {
	if c <= 0 {
		c = 0.5
	}
	if c > 1 {
		c = 1
	}
	anchored := false
	for _, e := range ev {
		if e.From == FromRelay {
			anchored = true
			break
		}
	}
	if !anchored && c > clientOnlyConfidenceCap {
		return clientOnlyConfidenceCap
	}
	return c
}

func scopeMatches(scope string, f *Facts) bool {
	switch scope {
	case "", "any":
		return true
	case "broadcast":
		return f.Scope == "broadcast"
	default:
		return f.Scope == "session" && f.Role == scope
	}
}

// Severity of a Report as a whole: its worst finding, or ok/unknown.
func (r Report) Severity() Severity {
	if len(r.Findings) > 0 {
		return r.Findings[0].Severity
	}
	// Nothing fired AND nothing could be evaluated is not health — it is an
	// absence of evidence, and must never render green.
	if len(r.Passed) == 0 {
		return SeverityUnknown
	}
	return SeverityOK
}

// Summary is a one-line human rendering, for logs and compact UI.
func (r Report) Summary() string {
	if r.Healthy {
		return fmt.Sprintf("no issues found (%d checks passed, %d unavailable)", len(r.Passed), len(r.Unavailable))
	}
	return fmt.Sprintf("%s: %s", r.Findings[0].Severity, r.Findings[0].Verdict)
}
