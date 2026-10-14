package screening

import (
	"math"
	"time"
	"unicode/utf8"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
)

// ContentFlag is the POST /api/v1/content-flags body: the
// `gawk.content-flag.v1` schema docs/42 §4.11 froze, with the additive fields
// docs/71 gives it. Scores and metadata only — never a frame (D8).
type ContentFlag struct {
	Schema string `json:"schema"`
	// BroadcastID is the raw broadcast ID the window belongs to.
	BroadcastID string `json:"broadcastId"`
	// PublisherEpoch is the publisher session the window was built over
	// (R40 addition). gawk-admin answers a tripped POST for a session that is
	// no longer the broadcast's active one with 202 ended (D2's backstop).
	PublisherEpoch string `json:"publisherEpoch"`
	SamplerID      string `json:"samplerId"`
	// Mode is ModeObserve or ModeFlag, required (D19).
	Mode string `json:"mode"`
	// State is StateTripped or StateCleared (D9).
	State  string        `json:"state"`
	Window WindowSummary `json:"window"`
	Scores Scores        `json:"scores"`
	// Labels are the flagged labels that fired in the window, and
	// LabelCounts how many of its samples each fired in (D8: "the labels
	// that fired and their counts"). LabelCounts is an R40 addition.
	Labels      []string       `json:"labels"`
	LabelCounts map[string]int `json:"labelCounts,omitempty"`
	// Recent is the session's last -report-samples per-sample scores, oldest
	// first, each the max over the flagged labels — below-floor scores
	// included (D14). Empty on a cleared POST and after a reset.
	Recent  []float64 `json:"recent,omitempty"`
	Summary string    `json:"summary,omitempty"`
}

// WindowSummary summarizes the sampler's window at the time of the POST.
type WindowSummary struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// Samples is how many samples the window holds, FlaggedSamples how many
	// of them scored at or above the threshold.
	Samples        int `json:"samples"`
	FlaggedSamples int `json:"flaggedSamples"`
}

// Scores are the window's max and mean sample score.
type Scores struct {
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
}

// FlagResponse is the body of every non-error content-flag POST answer.
type FlagResponse struct {
	// Status is one of the FlagStatus* values.
	Status string `json:"status"`
	// FlagID is the content_flags row the POST raised, updated or resolved;
	// absent when it touched none.
	FlagID int64 `json:"flagId,omitempty"`
}

// Content-flag POST outcomes (D9, D10, D2). The HTTP status travels with
// each: raised 201, updated and resolved 200, the rest 202.
const (
	// FlagStatusRaised: a tripped POST with no unresolved flag raised one.
	FlagStatusRaised = "raised"
	// FlagStatusUpdated: a tripped POST updated the unresolved flag.
	FlagStatusUpdated = "updated"
	// FlagStatusResolved: a cleared POST resolved the unresolved flag.
	FlagStatusResolved = "resolved"
	// FlagStatusMuted: the broadcast is muted by a dismissal; nothing
	// changed (flag mode only — observe POSTs ignore mutes).
	FlagStatusMuted = "muted"
	// FlagStatusEnded: the POST names a publisher session that is not the
	// broadcast's active one; nothing changed.
	FlagStatusEnded = "ended"
	// FlagStatusIgnored: a cleared POST with no unresolved flag to resolve.
	FlagStatusIgnored = "ignored"
)

// Validate checks a content flag. Every failure wraps ErrInvalid: the whole
// class is a 400.
func (f *ContentFlag) Validate() error {
	if f.Schema != SchemaContentFlag {
		return invalid("schema %q, want %q", f.Schema, SchemaContentFlag)
	}
	if _, err := broadcastid.Normalize(f.BroadcastID); err != nil {
		return invalid("broadcastId %q is not a broadcast ID", f.BroadcastID)
	}
	if err := validateEpoch(f.PublisherEpoch); err != nil {
		return err
	}
	if err := validateSamplerID(f.SamplerID); err != nil {
		return err
	}
	if !ValidMode(f.Mode) {
		return invalid("mode %q, want %q or %q", f.Mode, ModeObserve, ModeFlag)
	}
	if f.State != StateTripped && f.State != StateCleared {
		return invalid("state %q, want %q or %q", f.State, StateTripped, StateCleared)
	}
	if f.Window.Samples < 0 || f.Window.FlaggedSamples < 0 || f.Window.FlaggedSamples > f.Window.Samples {
		return invalid("window counts %d/%d", f.Window.FlaggedSamples, f.Window.Samples)
	}
	if !f.Window.Start.IsZero() && !f.Window.End.IsZero() && f.Window.End.Before(f.Window.Start) {
		return invalid("window ends before it starts")
	}
	if !validScore(f.Scores.Max) || !validScore(f.Scores.Mean) {
		return invalid("scores outside 0-1")
	}
	if err := validateLabels(f.Labels, "labels"); err != nil {
		return err
	}
	if len(f.LabelCounts) > MaxLabels {
		return invalid("labelCounts has %d entries, max %d", len(f.LabelCounts), MaxLabels)
	}
	for label, n := range f.LabelCounts {
		if err := validateLabel(label); err != nil {
			return err
		}
		if n < 0 {
			return invalid("labelCounts[%s] is negative", label)
		}
	}
	if len(f.Recent) > MaxRecent {
		return invalid("recent has %d entries, max %d", len(f.Recent), MaxRecent)
	}
	for i, s := range f.Recent {
		if !validScore(s) {
			return invalid("recent[%d] = %v, outside 0-1", i, s)
		}
	}
	if len(f.Summary) > MaxSummaryLen || !utf8.ValidString(f.Summary) {
		return invalid("summary is not valid UTF-8 of at most %d bytes", MaxSummaryLen)
	}
	return nil
}

func validScore(s float64) bool {
	return !math.IsNaN(s) && s >= 0 && s <= 1
}

// validateEpoch accepts an opaque token of 1–MaxEpochLen bytes of
// [A-Za-z0-9._-]. The relay mints lower-case hex today; the wider set keeps
// the contract from encoding that choice.
func validateEpoch(e string) error {
	if len(e) == 0 || len(e) > MaxEpochLen {
		return invalid("publisherEpoch must be 1-%d bytes", MaxEpochLen)
	}
	for i := 0; i < len(e); i++ {
		c := e[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return invalid("publisherEpoch has byte 0x%02x", c)
		}
	}
	return nil
}

func validateSamplerID(id string) error {
	if len(id) == 0 || len(id) > MaxSamplerIDLen {
		return invalid("samplerId must be 1-%d bytes", MaxSamplerIDLen)
	}
	for i := 0; i < len(id); i++ {
		if id[i] <= 0x20 || id[i] > 0x7e {
			return invalid("samplerId has byte 0x%02x", id[i])
		}
	}
	return nil
}

func validateBroadcastKey(k string) error {
	if len(k) == 0 || len(k) > MaxBroadcastKeyLen {
		return invalid("broadcastKey must be 1-%d bytes", MaxBroadcastKeyLen)
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return invalid("broadcastKey is not lower-case hex")
		}
	}
	return nil
}

// validateLabel accepts a NudeNet-style label: 1–MaxLabelLen bytes of
// [A-Z0-9_].
func validateLabel(l string) error {
	if len(l) == 0 || len(l) > MaxLabelLen {
		return invalid("label must be 1-%d bytes", MaxLabelLen)
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return invalid("label %q has byte 0x%02x", l, c)
		}
	}
	return nil
}

func validateLabels(labels []string, field string) error {
	if len(labels) > MaxLabels {
		return invalid("%s has %d entries, max %d", field, len(labels), MaxLabels)
	}
	seen := make(map[string]bool, len(labels))
	for _, l := range labels {
		if err := validateLabel(l); err != nil {
			return err
		}
		if seen[l] {
			return invalid("%s repeats %q", field, l)
		}
		seen[l] = true
	}
	return nil
}

// ValidateLabels checks a label list the way every body field is checked:
// at most MaxLabels distinct names of [A-Z0-9_]. The sampler applies it to
// -labels at start-up.
func ValidateLabels(labels []string) error {
	if len(labels) == 0 {
		return invalid("no labels")
	}
	return validateLabels(labels, "labels")
}
