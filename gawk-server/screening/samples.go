package screening

import (
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
)

// Batch limits. A healthy sampler sends one batch every 5 s holding at most
// one sample per screened broadcast per interval, far below these.
const (
	MaxBatchSessions = 4096
	MaxBatchSamples  = 16384
)

// SampleBatch is the POST /api/v1/screening/samples body (D20): one sampler's
// samples since its previous batch, grouped by publisher session.
//
// gawk-admin, not the sampler, numbers the samples. Each session reports how
// many samples it TOOK in this batch and, for every sample it SENDS, that
// sample's 1-based position among them; gawk-admin numbers a sent sample as
// the session's count before the batch plus its position, and adds Taken to
// the count, under a row lock. A sample is a scored frame: a failed fetch,
// decode or inference is neither counted nor sent, and a 304 counts with the
// previous score.
type SampleBatch struct {
	Schema    string `json:"schema"`
	SamplerID string `json:"samplerId"`
	// Mode is the sampler's mode, recorded with each session for the
	// Calibration view; samples are stored in both modes (D20).
	Mode string `json:"mode"`
	// Floor is -sample-floor: only samples where some label in Labels
	// scores at or above it are sent. Replay refuses a threshold below the
	// highest floor among the sessions it reads (D21).
	Floor float64 `json:"floor"`
	// Labels is -labels: the label set every sample's Scores covers.
	Labels []string `json:"labels"`
	// Rule is the sampler's window rule (-threshold, -window, -window-hits),
	// reported so gawk-admin draws and reasons with the threshold that was in
	// force rather than a copy of it in its own config (docs/72 D1). Absent
	// from a sampler older than R70; gawk-admin then knows no threshold.
	Rule     *BatchRule       `json:"rule,omitempty"`
	Sessions []SessionSamples `json:"sessions"`
}

// BatchRule is a WindowRule as a sample batch reports it.
type BatchRule struct {
	Threshold  float64 `json:"threshold"`
	Window     int     `json:"window"`
	WindowHits int     `json:"windowHits"`
}

// WindowRule is the rule in the sampler's own terms.
func (r BatchRule) WindowRule() WindowRule {
	return WindowRule{Threshold: r.Threshold, Size: r.Window, Hits: r.WindowHits}
}

// SessionSamples is one publisher session's share of a batch.
type SessionSamples struct {
	BroadcastID    string `json:"broadcastId"`
	BroadcastKey   string `json:"broadcastKey"`
	PublisherEpoch string `json:"publisherEpoch"`
	// Taken is how many samples this batch scored for the session, sent or
	// not. At least 1: a session with nothing scored is left out.
	Taken int `json:"taken"`
	// FirstAt and LastAt bound the scored samples, for the broadcast-hours
	// denominator.
	FirstAt time.Time `json:"firstAt"`
	LastAt  time.Time `json:"lastAt"`
	// Samples are the sent ones, in position order.
	Samples []Sample `json:"samples"`
}

// Sample is one stored sample.
type Sample struct {
	// Position is the sample's 1-based place among the session's Taken.
	Position int       `json:"position"`
	At       time.Time `json:"at"`
	// Scores holds every label of the batch's Labels: the label's highest
	// detection confidence in the frame, 0 when it was not detected.
	Scores map[string]float64 `json:"scores"`
}

// SamplesResponse answers a stored batch.
type SamplesResponse struct {
	// Stored is how many samples were written.
	Stored int `json:"stored"`
}

// Validate checks a batch. Every failure wraps ErrInvalid.
func (b *SampleBatch) Validate() error {
	if b.Schema != SchemaSamples {
		return invalid("schema %q, want %q", b.Schema, SchemaSamples)
	}
	if err := validateSamplerID(b.SamplerID); err != nil {
		return err
	}
	if !ValidMode(b.Mode) {
		return invalid("mode %q, want %q or %q", b.Mode, ModeObserve, ModeFlag)
	}
	if !validScore(b.Floor) {
		return invalid("floor %v outside 0-1", b.Floor)
	}
	if err := ValidateLabels(b.Labels); err != nil {
		return err
	}
	if b.Rule != nil {
		if err := b.Rule.WindowRule().Validate(); err != nil {
			return err
		}
		// The sampler refuses to start with its threshold under its floor;
		// a batch saying otherwise was not written by one.
		if b.Rule.Threshold < b.Floor {
			return invalid("rule threshold %v below floor %v", b.Rule.Threshold, b.Floor)
		}
	}
	if len(b.Sessions) > MaxBatchSessions {
		return invalid("%d sessions, max %d", len(b.Sessions), MaxBatchSessions)
	}
	known := make(map[string]bool, len(b.Labels))
	for _, l := range b.Labels {
		known[l] = true
	}
	total := 0
	seen := make(map[string]bool, len(b.Sessions))
	for i := range b.Sessions {
		s := &b.Sessions[i]
		if _, err := broadcastid.Normalize(s.BroadcastID); err != nil {
			return invalid("sessions[%d].broadcastId is not a broadcast ID", i)
		}
		if err := validateBroadcastKey(s.BroadcastKey); err != nil {
			return err
		}
		if err := validateEpoch(s.PublisherEpoch); err != nil {
			return err
		}
		if seen[s.PublisherEpoch] {
			return invalid("sessions[%d] repeats publisherEpoch %q", i, s.PublisherEpoch)
		}
		seen[s.PublisherEpoch] = true
		if s.Taken < 1 {
			return invalid("sessions[%d].taken %d, want at least 1", i, s.Taken)
		}
		if s.FirstAt.IsZero() || s.LastAt.IsZero() || s.LastAt.Before(s.FirstAt) {
			return invalid("sessions[%d] has no valid firstAt/lastAt", i)
		}
		if len(s.Samples) > s.Taken {
			return invalid("sessions[%d] sends %d samples of %d taken", i, len(s.Samples), s.Taken)
		}
		total += len(s.Samples)
		if total > MaxBatchSamples {
			return invalid("more than %d samples", MaxBatchSamples)
		}
		prev := 0
		for j := range s.Samples {
			sm := &s.Samples[j]
			if sm.Position <= prev || sm.Position > s.Taken {
				return invalid("sessions[%d].samples[%d].position %d out of order or past taken", i, j, sm.Position)
			}
			prev = sm.Position
			if sm.At.IsZero() {
				return invalid("sessions[%d].samples[%d] has no time", i, j)
			}
			if len(sm.Scores) != len(b.Labels) {
				return invalid("sessions[%d].samples[%d] scores %d labels, want %d", i, j, len(sm.Scores), len(b.Labels))
			}
			above := false
			for l, v := range sm.Scores {
				if !known[l] {
					return invalid("sessions[%d].samples[%d] scores unknown label %q", i, j, l)
				}
				if !validScore(v) {
					return invalid("sessions[%d].samples[%d] score %v outside 0-1", i, j, v)
				}
				if v >= b.Floor {
					above = true
				}
			}
			if !above {
				return invalid("sessions[%d].samples[%d] is below the floor and must not be sent", i, j)
			}
		}
	}
	return nil
}

// MaxReplayRange bounds a replay's time range; samples are pruned after
// -screening-sample-retention (30 d) anyway.
const MaxReplayRange = 92 * 24 * time.Hour

// MaxReplayTrips caps the trips a replay lists. The count beyond it is still
// exact; only the list is truncated.
const MaxReplayTrips = 1000

// ReplayRequest is the POST /api/v1/screening/replay body (D21): a candidate
// window rule and the stored data to run it over.
type ReplayRequest struct {
	Threshold  float64 `json:"threshold"`
	Window     int     `json:"window"`
	WindowHits int     `json:"windowHits"`
	// Labels restricts the score to a subset of the stored labels; empty
	// means every stored label.
	Labels []string  `json:"labels,omitempty"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
}

// Rule is the request's window rule.
func (r ReplayRequest) Rule() WindowRule {
	return WindowRule{Threshold: r.Threshold, Size: r.Window, Hits: r.WindowHits}
}

// Validate checks the request's own shape. The floor check needs the stored
// data and is gawk-admin's.
func (r ReplayRequest) Validate() error {
	if err := r.Rule().Validate(); err != nil {
		return err
	}
	if len(r.Labels) > 0 {
		if err := validateLabels(r.Labels, "labels"); err != nil {
			return err
		}
	}
	if r.From.IsZero() || r.To.IsZero() || !r.From.Before(r.To) {
		return invalid("from must be before to")
	}
	if r.To.Sub(r.From) > MaxReplayRange {
		return invalid("range longer than %s", MaxReplayRange)
	}
	return nil
}

// ReplayResponse answers a replay.
type ReplayResponse struct {
	Schema  string        `json:"schema"`
	Request ReplayRequest `json:"request"`
	// Floor is the highest floor among the replayed sessions.
	Floor float64 `json:"floor"`
	// Sessions, SamplesTaken and SamplesStored size the data replayed.
	Sessions      int   `json:"sessions"`
	SamplesTaken  int64 `json:"samplesTaken"`
	SamplesStored int64 `json:"samplesStored"`
	// BroadcastHours is the sum of the sessions' sampled spans.
	BroadcastHours float64 `json:"broadcastHours"`
	// WouldTrips counts every trip; WouldTripsPer100h normalizes it (0 when
	// there are no broadcast-hours).
	WouldTrips        int     `json:"wouldTrips"`
	WouldTripsPer100h float64 `json:"wouldTripsPer100h"`
	// Trips lists them, newest first, at most MaxReplayTrips; Truncated says
	// the list was cut.
	Trips     []ReplayTrip `json:"trips"`
	Truncated bool         `json:"truncated"`
}

// ReplayTrip is one would-have-tripped episode.
type ReplayTrip struct {
	BroadcastKey   string    `json:"broadcastKey"`
	BroadcastID    string    `json:"broadcastId"`
	PublisherEpoch string    `json:"publisherEpoch"`
	At             time.Time `json:"at"`
	PeakScore      float64   `json:"peakScore"`
}
