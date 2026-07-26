// Package schema implements R28's validation stance (docs/33 D15).
//
// Validation is not one policy. A batch carries two kinds of data with
// opposite requirements, and applying one stance to both breaks something
// either way:
//
//   - **The envelope is protocol: strict, reject on violation.** It is small,
//     stable, security-relevant, and it is the same posture `wire` takes.
//   - **The stats payload is data: tolerant reader, never reject.** Strictness
//     there would actively hurt, for a structural reason rather than a
//     stylistic one — **version skew is permanent, not transient**. Stats
//     objects come from a browser SPA a viewer may have loaded hours ago, and
//     `ViewerStats` has grown in R5, R9, R10, R12, R15, R16, R18, R19, R21 and
//     R22. A closed field list would mean shipping a gawk-app with a new field
//     rejects every batch from updated clients until the service is
//     redeployed, and an old open tab forever — losing telemetry exactly
//     during a deploy, which is when it is most wanted.
//
// So: known fields are typed (a wrongly-typed value is dropped and counted,
// never fatal — a string "30" must never become a data point in an fps
// series); unknown fields survive verbatim and become queryable the day the
// service learns their name; and structural bounds are enforced regardless of
// types, which is the part that actually protects the disk and the JSON parser
// and, being type-agnostic, does not rot as the stats objects grow.
//
// Schema quality is a signal, not an error: the coerced/dropped/unknown tally
// rides the rollup as `schemaAnomalies`, so "this client is sending nonsense"
// becomes a diagnosable fact rather than a silent hole. Both directions are
// useful — a spike of *unknown* fields means clients are running ahead of the
// service, a spike of *dropped* ones means a real client bug.
package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// Structural bounds. These protect the disk and the JSON parser and are
// enforced regardless of any field's type, which is what keeps them from
// rotting as the stats objects grow.
const (
	// MaxSamplesPerBatch bounds one batch's sample array. The browser flushes
	// ~5 samples per 10 s batch and caps its own buffer at 64; this leaves
	// generous room for a client catching up after a network blip.
	MaxSamplesPerBatch = 256
	// MaxEventsPerBatch bounds one batch's event array.
	MaxEventsPerBatch = 512
	// MaxFieldsPerObject bounds one stats object's key count at every level.
	// ViewerStats has ~80 fields; this leaves room for several milestones'
	// growth and still refuses a generated blob.
	MaxFieldsPerObject = 512
	// MaxDepth bounds nesting inside a stats object. ViewerStats nests two
	// deep (audioBuffer, presentationMux); 8 is far beyond any plausible
	// growth and well inside what a JSON parser handles comfortably.
	MaxDepth = 8
	// MaxStringLen truncates any string value. Long strings in a stats object
	// are codec names and error messages, not documents.
	MaxStringLen = 512
	// MaxBodyBytes is the largest ingest body accepted, uncompressed. A
	// well-behaved 10 s batch is ~7.5 KB; this is two orders of magnitude of
	// headroom and still bounds what one request can cost.
	MaxBodyBytes = 1 << 20
)

// Kind is the expected type of a known stats field.
type Kind uint8

const (
	// KindNumber is a finite JSON number. Non-finite values (the JSON encoders
	// that emit NaN/Infinity, and anything a decoder turns into one) are
	// dropped: a NaN entering a percentile series poisons every quantile
	// computed from it.
	KindNumber Kind = iota
	// KindBool is a JSON boolean.
	KindBool
	// KindString is a JSON string, truncated to MaxStringLen.
	KindString
	// KindObject is a nested object, itself validated field-by-field.
	KindObject
	// KindAny accepts whatever shape arrives, still subject to the structural
	// bounds. For fields whose type genuinely varies.
	KindAny
)

// Anomalies is the per-session data-quality tally that rides the rollup row
// (docs/33 §4.5). A verdict computed over a session with a high count is a
// verdict to distrust, and this is what makes that visible.
type Anomalies struct {
	// Dropped counts known fields removed because their value could not be
	// used: wrong type, a non-finite number, or a structure over the bounds.
	// A spike here is a real client bug.
	Dropped int `json:"dropped"`
	// Coerced counts values kept but modified to fit — today, strings
	// truncated to MaxStringLen.
	Coerced int `json:"coerced"`
	// Unknown counts fields this build has no type for. They are KEPT
	// verbatim; a spike here means clients are running ahead of the service,
	// which is information, not an error.
	Unknown int `json:"unknown"`
}

// Add folds another tally in.
func (a *Anomalies) Add(b Anomalies) {
	a.Dropped += b.Dropped
	a.Coerced += b.Coerced
	a.Unknown += b.Unknown
}

// Total is the sum of all three, for a quick "is this session's data
// trustworthy?" read.
func (a Anomalies) Total() int { return a.Dropped + a.Coerced + a.Unknown }

// ErrTooManyFields and friends are structural violations. Unlike a typed-field
// problem these are NOT tolerated silently at the top level of a batch — the
// bounds exist to protect the process, so a batch that blows them is rejected.
var (
	ErrTooManySamples = errors.New("schema: too many samples in batch")
	ErrTooManyEvents  = errors.New("schema: too many events in batch")
	ErrBodyTooLarge   = errors.New("schema: body exceeds the size limit")
)

// SanitizeStats walks one stats object, applying the tolerant-payload rules,
// and returns the cleaned object plus the anomalies found. It NEVER returns an
// error: a stats object is data, and the only outcome is a (possibly smaller)
// object.
//
// `known` maps field names to expected kinds. A nil map means "nothing is
// known", which degrades gracefully to keeping everything verbatim under the
// structural bounds — the correct behaviour for a service that has not learned
// this producer's shape yet.
func SanitizeStats(obj map[string]any, known map[string]Kind) (map[string]any, Anomalies) {
	var an Anomalies
	out := sanitizeObject(obj, known, 1, &an)
	return out, an
}

func sanitizeObject(obj map[string]any, known map[string]Kind, depth int, an *Anomalies) map[string]any {
	if depth > MaxDepth {
		// The whole subtree goes, counted once. Deeper than this is either a
		// generated blob or a bug, and either way nothing downstream reads it.
		an.Dropped++
		return nil
	}
	out := make(map[string]any, len(obj))
	kept := 0
	for k, v := range obj {
		if kept >= MaxFieldsPerObject {
			an.Dropped++
			continue
		}
		kind, isKnown := known[k]
		if !isKnown {
			// Unknown fields survive VERBATIM — that is the whole point (D15).
			// They still pass through the structural walk, which is what
			// protects the store from a nested blob arriving under a new name.
			if v == nil {
				// Absence again: a null under an unfamiliar name says nothing
				// about whether clients are running ahead of the service.
				continue
			}
			cleaned, ok := sanitizeAny(v, depth, an)
			if !ok {
				continue
			}
			// Counted only at the TOP level of a stats object. A nested
			// object's children are not independently "unknown" — counting
			// them would make one new nested field look like ten, and the
			// tally is meant to answer "are clients ahead of us?", not "how
			// deep is this tree?".
			if depth == 1 {
				an.Unknown++
			}
			out[k] = cleaned
			kept++
			continue
		}
		cleaned, status := sanitizeTyped(v, kind, depth, an)
		switch status {
		case fieldBad:
			// Dropped, counted, and NOT fatal. Crucially it also never reaches
			// a numeric series: a string "30" in an fps field is exactly the
			// case this exists to stop.
			an.Dropped++
			continue
		case fieldAbsent:
			// An explicit null is the client saying "I do not have this", not
			// a client bug — and the stats objects are FULL of legitimately
			// nullable fields (avSkewMs with no audio, connection because no
			// browser ships getStats(), renderedFps on the main-thread path).
			// Counting those as anomalies made a healthy session's tally read
			// as nonsense: the e2e pass produced 70 "dropped" fields across 13
			// samples, which distrustReason() then reported as a likely client
			// bug. Absence is omitted from the stored object and counted as
			// nothing.
			continue
		}
		out[k] = cleaned
		kept++
	}
	return out
}

// fieldStatus is the three-way outcome of typing one known field. The middle
// state is the one that matters: "absent" is not "bad", and conflating them
// turns every nullable field into a data-quality alarm.
type fieldStatus uint8

const (
	fieldOK fieldStatus = iota
	// fieldAbsent: the client explicitly sent null. Omitted, not counted.
	fieldAbsent
	// fieldBad: a non-null value of the wrong type. Dropped AND counted.
	fieldBad
)

func sanitizeTyped(v any, kind Kind, depth int, an *Anomalies) (any, fieldStatus) {
	// Null is absence for EVERY kind. ViewerStats declares dozens of fields as
	// `T | null` by design, and a client sending one is reporting correctly.
	if v == nil {
		return nil, fieldAbsent
	}
	switch kind {
	case KindNumber:
		f, ok := toNumber(v)
		if !ok {
			return nil, fieldBad
		}
		return f, fieldOK
	case KindBool:
		b, ok := v.(bool)
		if !ok {
			return nil, fieldBad
		}
		return b, fieldOK
	case KindString:
		s, ok := v.(string)
		if !ok {
			return nil, fieldBad
		}
		return truncate(s, an), fieldOK
	case KindObject:
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fieldBad
		}
		return sanitizeObject(m, nil, depth+1, an), fieldOK
	default:
		cleaned, ok := sanitizeAny(v, depth, an)
		if !ok {
			return nil, fieldBad
		}
		return cleaned, fieldOK
	}
}

// sanitizeAny walks a value of unconstrained type, enforcing only the
// structural bounds. Returns false when the value must be dropped entirely
// (already counted by the caller's path).
func sanitizeAny(v any, depth int, an *Anomalies) (any, bool) {
	switch t := v.(type) {
	case nil, bool:
		return v, true
	case json.Number, float64:
		f, ok := toNumber(t)
		if !ok {
			an.Dropped++
			return nil, false
		}
		return f, true
	case string:
		return truncate(t, an), true
	case map[string]any:
		if depth+1 > MaxDepth {
			an.Dropped++
			return nil, false
		}
		return sanitizeObject(t, nil, depth+1, an), true
	case []any:
		if depth+1 > MaxDepth {
			an.Dropped++
			return nil, false
		}
		out := make([]any, 0, len(t))
		for i, e := range t {
			if i >= MaxFieldsPerObject {
				an.Dropped++
				break
			}
			cleaned, ok := sanitizeAny(e, depth+1, an)
			if !ok {
				continue
			}
			out = append(out, cleaned)
		}
		return out, true
	default:
		// json.Unmarshal into `any` produces nothing else; a future decoder
		// that does is not something to guess about.
		an.Dropped++
		return nil, false
	}
}

// toNumber converts a decoded JSON number to a usable float64, rejecting the
// values that must never enter a series.
//
// Stats objects are decoded with UseNumber, so numbers arrive as json.Number
// (an unconverted string) rather than float64. That is deliberate: Go's JSON
// decoder ERRORS on a number that overflows float64, and letting that error
// escape would make one absurd value inside a payload reject the entire batch
// — precisely the strictness D15 rules out for payload data. Deferring the
// conversion to here turns it into a drop-and-count, which is the whole
// tolerant-payload stance.
func toNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	case json.Number:
		f, err := n.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

func truncate(s string, an *Anomalies) string {
	if len(s) <= MaxStringLen {
		return s
	}
	an.Coerced++
	return s[:MaxStringLen]
}

// Number reads a known-numeric field from a sanitized stats object. The second
// result is false when the field is absent — which, after sanitizing, is the
// ONLY way a numeric field can fail to be a finite number. That is what lets
// the rollup be typed by construction (D15): a value that could not be
// computed is absent, never a coerced guess.
func Number(stats map[string]any, field string) (float64, bool) {
	v, ok := stats[field]
	if !ok {
		return 0, false
	}
	return toNumber(v)
}

// String reads a known-string field from a sanitized stats object.
func String(stats map[string]any, field string) (string, bool) {
	v, ok := stats[field]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// Bool reads a known-boolean field from a sanitized stats object.
func Bool(stats map[string]any, field string) (bool, bool) {
	v, ok := stats[field]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// Nested returns a nested object field (e.g. audioBuffer), or nil.
func Nested(stats map[string]any, field string) map[string]any {
	m, _ := stats[field].(map[string]any)
	return m
}

// DescribeKinds renders the known-field table for diagnostics/docs.
func DescribeKinds(known map[string]Kind) string {
	names := make([]string, 0, len(known))
	for k := range known {
		names = append(names, k)
	}
	return fmt.Sprintf("%d known fields: %s", len(names), strings.Join(names, ", "))
}
