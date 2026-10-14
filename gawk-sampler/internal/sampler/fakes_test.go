package sampler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-sampler/internal/admin"
	"github.com/Tuhis/gawk/gawk-sampler/internal/bitstream"
	"github.com/Tuhis/gawk/gawk-sampler/internal/bitstream/bitstreamtest"
	"github.com/Tuhis/gawk/gawk-sampler/internal/relay"
	"github.com/Tuhis/gawk/gawk-server/screening"
)

const label = "FEMALE_GENITALIA_EXPOSED"

// fakeAdmin is gawk-admin: a settable target list and recorded POSTs.
type fakeAdmin struct {
	mu        sync.Mutex
	targets   []screening.Target
	flagRoute bool
	polls     [][2]int
	flags     []screening.ContentFlag
	batches   []screening.SampleBatch
	flagErr   error
	batchErr  error
	raised    map[string]bool
}

func (a *fakeAdmin) Targets(_ context.Context, shard, of int) (screening.TargetsResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.polls = append(a.polls, [2]int{shard, of})
	return screening.TargetsResponse{Schema: screening.SchemaTargets, Shard: shard, Of: of, FlagRoute: a.flagRoute,
		Targets: append([]screening.Target(nil), a.targets...)}, nil
}

func (a *fakeAdmin) PostFlag(_ context.Context, f screening.ContentFlag) (screening.FlagResponse, error) {
	if err := f.Validate(); err != nil {
		return screening.FlagResponse{}, fmt.Errorf("fakeAdmin: 400: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.flagErr != nil {
		return screening.FlagResponse{}, a.flagErr
	}
	a.flags = append(a.flags, f)
	if a.raised == nil {
		a.raised = map[string]bool{}
	}
	switch {
	case f.State == screening.StateCleared:
		a.raised[f.BroadcastID] = false
		return screening.FlagResponse{Status: screening.FlagStatusResolved}, nil
	case !a.raised[f.BroadcastID]:
		a.raised[f.BroadcastID] = true
		return screening.FlagResponse{Status: screening.FlagStatusRaised, FlagID: 1}, nil
	}
	return screening.FlagResponse{Status: screening.FlagStatusUpdated, FlagID: 1}, nil
}

func (a *fakeAdmin) PostSamples(_ context.Context, b screening.SampleBatch) error {
	if err := b.Validate(); err != nil {
		return fmt.Errorf("fakeAdmin: 400: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.batchErr != nil {
		return a.batchErr
	}
	a.batches = append(a.batches, b)
	return nil
}

func (a *fakeAdmin) takeFlags() []screening.ContentFlag {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.flags
	a.flags = nil
	return f
}

// step is one scripted fetch outcome.
type step struct {
	score float64 // the frame's FEMALE_GENITALIA_EXPOSED score
	epoch string  // defaults to the broadcast's current scripted epoch
	err   error   // a fetch error (relay.ErrNotFound, …)
	bad   bool    // a frame the decoder refuses
	same  bool    // the relay answers 304 when asked with the last ETag
}

// fakeRelay serves scripted keyframes. A frame's payload encodes its score in
// its last byte, and a distinctive marker so the D8 test can look for it.
type fakeRelay struct {
	mu      sync.Mutex
	scripts map[string][]step
	epochs  map[string]string
	frameID map[string]uint32
	fetches map[string]int
	delay   time.Duration
}

const frameMarker = "FRAME-BYTES-MUST-NOT-LEAK"

func newFakeRelay() *fakeRelay {
	return &fakeRelay{scripts: map[string][]step{}, epochs: map[string]string{}, frameID: map[string]uint32{}, fetches: map[string]int{}}
}

func (r *fakeRelay) script(id, epoch string, steps ...step) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.epochs[id] = epoch
	r.scripts[id] = append(r.scripts[id], steps...)
}

func scores(vs ...float64) []step {
	out := make([]step, len(vs))
	for i, v := range vs {
		out[i] = step{score: v}
	}
	return out
}

func (r *fakeRelay) Fetch(ctx context.Context, _, id, etag string) (relay.Keyframe, error) {
	r.mu.Lock()
	r.fetches[id]++
	delay := r.delay
	var s step
	if q := r.scripts[id]; len(q) > 0 {
		s = q[0]
		if len(q) > 1 {
			r.scripts[id] = q[1:]
		}
	}
	if s.epoch != "" {
		r.epochs[id] = s.epoch
	}
	epoch := r.epochs[id]
	if !s.same {
		r.frameID[id]++
	}
	fid := r.frameID[id]
	r.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return relay.Keyframe{}, ctx.Err()
		}
	}
	if s.err != nil {
		return relay.Keyframe{}, s.err
	}
	tag := screening.KeyframeETag(epoch, fid)
	if s.same && etag == tag {
		return relay.Keyframe{NotModified: true, Epoch: epoch, ETag: tag, Codec: "h264"}, nil
	}
	payload := append([]byte{0, 0, 0, 1}, frameMarker...)
	if s.bad {
		payload = append(payload, 0xff, 0xff)
	} else {
		payload = append(payload, 0, byte(s.score*100+0.5))
	}
	return relay.Keyframe{Frame: bitstreamtest.StreamFrame(fid, "", nil, payload), Epoch: epoch, ETag: tag, Codec: "h264"}, nil
}

// fakeDecoder passes the payload through as the "decoded frame", refusing
// the scripted bad ones.
type fakeDecoder struct{}

var errBadFrame = errors.New("decode: failed: Invalid data found when processing input")

func (fakeDecoder) Decode(_ context.Context, in bitstream.Input) ([]byte, error) {
	if len(in.Data) >= 2 && in.Data[len(in.Data)-2] == 0xff {
		return nil, errBadFrame
	}
	return in.Data, nil
}

// fakeScorer reads the score the fake relay encoded.
type fakeScorer struct{}

func (fakeScorer) Scores(rgb []byte) (map[string]float64, error) {
	return map[string]float64{label: float64(rgb[len(rgb)-1]) / 100, "ANUS_EXPOSED": 0, "MALE_GENITALIA_EXPOSED": 0}, nil
}

var _ = admin.ErrNoFlagRoute
