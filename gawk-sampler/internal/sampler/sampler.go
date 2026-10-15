// Package sampler is gawk-sampler's engine (docs/71): it keeps this shard's
// target list from gawk-admin, samples every screened broadcast's cached
// keyframe on a rotation, scores it, runs each publisher session's D7 window,
// posts content flags on trips and clears, and batches the samples gawk-admin
// stores for calibration.
//
// Three rules shape it:
//
//   - **Every screened broadcast stays in the rotation** (D6). The rotation
//     is paced at -interval / N and a dispatch waits for a free worker, so
//     when the pool cannot keep up every broadcast's interval stretches by the
//     same factor and the effective-interval gauge says so. Nothing is ever
//     skipped to catch up.
//   - **A sample is a scored frame** (D20). A 404, a network error, a decode
//     failure or timeout, or an inference error leaves the window, `recent`
//     and the batch count untouched; a 304 counts, repeating the score of the
//     frame it names.
//   - **No frame bytes leave the sample** (D8). A keyframe and its decoded
//     frame live in one sample goroutine's locals and nowhere else: not in a
//     log line, an error, a metric, a POST or the broadcast state.
package sampler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-sampler/internal/admin"
	"github.com/Tuhis/gawk/gawk-sampler/internal/bitstream"
	"github.com/Tuhis/gawk/gawk-sampler/internal/decode"
	"github.com/Tuhis/gawk/gawk-sampler/internal/relay"
	"github.com/Tuhis/gawk/gawk-server/screening"
)

// Admin is the gawk-admin client the engine uses (*admin.Client).
type Admin interface {
	Targets(ctx context.Context, shard, of int) (screening.TargetsResponse, error)
	PostFlag(ctx context.Context, f screening.ContentFlag) (screening.FlagResponse, error)
	PostSamples(ctx context.Context, b screening.SampleBatch) error
}

// Keyframes fetches a cached keyframe (*relay.Fetcher).
type Keyframes interface {
	Fetch(ctx context.Context, relayBase, id, etag string) (relay.Keyframe, error)
}

// Decoder turns a keyframe into a model-ready RGB frame (*decode.Decoder).
type Decoder interface {
	Decode(ctx context.Context, in bitstream.Input) ([]byte, error)
}

// Scorer scores one decoded frame: every configured label's score.
type Scorer interface {
	Scores(rgb []byte) (map[string]float64, error)
}

// Options configure an Engine.
type Options struct {
	Mode       string
	SamplerID  string
	ShardIndex int
	ShardCount int
	// Labels are the flagged labels (D7); every sample's scores cover them.
	Labels        []string
	Rule          screening.WindowRule
	ReportSamples int
	Floor         float64

	Interval       time.Duration
	Workers        int
	TargetsRefresh time.Duration
	// BatchInterval is how often samples are sent (D20: every 5 s); zero
	// means 5 s.
	BatchInterval time.Duration
	// PostTimeout bounds one call to gawk-admin; zero means 10 s.
	PostTimeout time.Duration
	// PausedLogEvery is how often a paused sampler says so (D12: once a
	// minute); zero means one minute.
	PausedLogEvery time.Duration

	Admin     Admin
	Keyframes Keyframes
	Decoder   Decoder
	Scorer    Scorer
	Metrics   *Metrics
	Log       *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
}

// Engine runs the sampler. Build it with New; Run it once.
type Engine struct {
	opts Options
	log  *slog.Logger

	mu   sync.Mutex
	cond *sync.Cond // signalled when a broadcast's sample finishes
	bcs  map[string]*broadcast
	// rotation is the order screened broadcasts are sampled in: new ones are
	// appended, so a refresh never reshuffles who is next.
	rotation  []*broadcast
	cursor    int
	flagRoute bool
	paused    bool
	acc       map[string]*screening.SessionSamples // by publisher epoch
	stopping  bool

	wake    chan struct{}
	sem     chan struct{}
	samples sync.WaitGroup

	lastPausedLog  time.Time
	targetsFailing bool
}

// broadcast is one screened broadcast's state, guarded by Engine.mu.
type broadcast struct {
	id, key, relay string
	muted, exempt  bool

	// epoch is the publisher session the window, recent and lastScores
	// belong to; retired are sessions this one replaced, whose late samples
	// are discarded rather than allowed to reset the state back.
	epoch   string
	retired []string
	window  *screening.Window
	recs    []record // the window's samples, oldest first
	recent  []float64
	// lastScores and etag are the last fetched frame's: a 304 for etag
	// repeats lastScores.
	lastScores map[string]float64
	etag       string
	// pendingClear is a cleared POST that has not landed yet: retried on the
	// next sample while the window stays clear.
	pendingClear bool

	busy         bool
	gone         bool
	lastDispatch time.Time
	interval     time.Duration
}

type record struct {
	at     time.Time
	score  float64
	labels map[string]float64
}

// New builds an engine.
func New(opts Options) (*Engine, error) {
	if err := opts.Rule.Validate(); err != nil {
		return nil, err
	}
	switch {
	case !screening.ValidMode(opts.Mode):
		return nil, fmt.Errorf("sampler: mode %q", opts.Mode)
	case len(opts.Labels) == 0:
		return nil, errors.New("sampler: no labels")
	case opts.Workers < 1 || opts.Interval <= 0 || opts.TargetsRefresh <= 0:
		return nil, errors.New("sampler: workers, interval and targets refresh must be positive")
	case opts.ReportSamples < 1 || opts.ReportSamples > screening.MaxRecent:
		return nil, fmt.Errorf("sampler: report samples %d", opts.ReportSamples)
	case opts.Admin == nil || opts.Keyframes == nil || opts.Decoder == nil || opts.Scorer == nil:
		return nil, errors.New("sampler: Admin, Keyframes, Decoder and Scorer are required")
	}
	if err := screening.ValidateShard(opts.ShardIndex, opts.ShardCount); err != nil {
		return nil, err
	}
	if opts.BatchInterval <= 0 {
		opts.BatchInterval = 5 * time.Second
	}
	if opts.PostTimeout <= 0 {
		opts.PostTimeout = 10 * time.Second
	}
	if opts.PausedLogEvery <= 0 {
		opts.PausedLogEvery = time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	e := &Engine{
		opts: opts,
		log:  opts.Log,
		bcs:  make(map[string]*broadcast),
		acc:  make(map[string]*screening.SessionSamples),
		wake: make(chan struct{}, 1),
		sem:  make(chan struct{}, opts.Workers),
	}
	e.cond = sync.NewCond(&e.mu)
	opts.Metrics.setWorkers(opts.Workers, opts.Interval.Seconds())
	// A flag-mode sampler has no route until gawk-admin says it has one: it
	// starts paused rather than sampling into a POST that would 503.
	e.updatePausedLocked()
	return e, nil
}

// Run samples until ctx ends, then waits for in-flight samples and sends a
// last batch.
func (e *Engine) Run(ctx context.Context) error {
	var loops sync.WaitGroup
	loops.Add(3)
	go func() { defer loops.Done(); e.targetsLoop(ctx) }()
	go func() { defer loops.Done(); e.dispatchLoop(ctx) }()
	go func() { defer loops.Done(); e.batchLoop(ctx) }()
	<-ctx.Done()
	e.mu.Lock()
	e.stopping = true
	e.cond.Broadcast()
	e.mu.Unlock()
	loops.Wait()
	e.samples.Wait()
	// The batch loop flushed on its way out; anything a sample finishing
	// after that added goes now, on a fresh deadline.
	flushCtx, cancel := context.WithTimeout(context.Background(), e.opts.PostTimeout)
	defer cancel()
	e.flush(flushCtx)
	return nil
}

func (e *Engine) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------------------
// Targets (D2, D11, D12, D22)
// ---------------------------------------------------------------------------

func (e *Engine) targetsLoop(ctx context.Context) {
	t := time.NewTicker(e.opts.TargetsRefresh)
	defer t.Stop()
	for {
		e.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (e *Engine) refresh(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, e.opts.PostTimeout)
	tr, err := e.opts.Admin.Targets(cctx, e.opts.ShardIndex, e.opts.ShardCount)
	cancel()
	now := e.opts.Now()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		e.opts.Metrics.refreshed(false, 0)
		e.opts.Metrics.postError("targets")
		// The last list stays in force: the relay route still answers while
		// gawk-admin is away. Said once per outage, not every five seconds.
		e.mu.Lock()
		first := !e.targetsFailing
		e.targetsFailing = true
		e.mu.Unlock()
		if first {
			e.log.Warn("target list unavailable; keeping the last one", "err", err)
		}
		return
	}
	e.opts.Metrics.refreshed(true, float64(now.Unix()))

	e.mu.Lock()
	if e.targetsFailing {
		e.log.Info("target list available again")
		e.targetsFailing = false
	}
	e.flagRoute = tr.FlagRoute
	seen := make(map[string]bool, len(tr.Targets))
	screened, exempt, muted := 0, 0, 0
	for _, t := range tr.Targets {
		seen[t.BroadcastID] = true
		b := e.bcs[t.BroadcastID]
		if b == nil {
			b = &broadcast{id: t.BroadcastID}
			e.bcs[t.BroadcastID] = b
		}
		b.key, b.relay, b.muted = t.BroadcastKey, t.Relay, t.Muted
		if t.Exempt != b.exempt || b.window == nil {
			// Becoming exempt drops the window (an exempt broadcast is never
			// sampled); leaving exemption starts a fresh one.
			b.exempt = t.Exempt
			b.resetLocked(e, t.PublisherEpoch)
		} else if t.PublisherEpoch != "" && t.PublisherEpoch != b.epoch && !slices.Contains(b.retired, t.PublisherEpoch) {
			// The listing saw a new session before a keyframe did: drop the
			// old window early (D7). A retired epoch is an older listing
			// than what a keyframe already showed, and is ignored.
			b.resetLocked(e, t.PublisherEpoch)
		}
		switch {
		case b.exempt:
			exempt++
		default:
			screened++
			if b.muted {
				muted++
			}
		}
	}
	// A broadcast that left the list — its publisher went away, the session
	// ended, or it moved shard — gets no further POST, and its window and
	// recent are gone: if it returns, it starts empty (D2).
	for id, b := range e.bcs {
		if !seen[id] {
			b.gone = true
			delete(e.bcs, id)
		}
	}
	e.rebuildRotationLocked()
	e.updatePausedLocked()
	e.opts.Metrics.setTargets(screened, exempt, muted)
	e.publishTrippedLocked()
	paused := e.paused
	if paused && now.Sub(e.lastPausedLog) >= e.opts.PausedLogEvery {
		e.lastPausedLog = now
		e.log.Warn("flag mode with no webhook that delivers content_flag.raised: sampling nothing until one exists (D12)",
			"targets", len(tr.Targets))
	}
	e.mu.Unlock()
	e.signal()
}

// rebuildRotationLocked keeps the rotation's order and membership in step
// with the broadcast map: gone and exempt broadcasts leave, new ones join at
// the end.
func (e *Engine) rebuildRotationLocked() {
	keep := e.rotation[:0:0]
	in := make(map[*broadcast]bool, len(e.rotation))
	for i, b := range e.rotation {
		if b.gone || b.exempt {
			if i < e.cursor {
				e.cursor--
			}
			continue
		}
		keep = append(keep, b)
		in[b] = true
	}
	var added []*broadcast
	for _, b := range e.bcs {
		if !b.exempt && !in[b] {
			added = append(added, b)
		}
	}
	// Map order is random; a stable order keeps tests and rotations
	// reproducible.
	sort.Slice(added, func(i, j int) bool { return added[i].id < added[j].id })
	e.rotation = append(keep, added...)
	if e.cursor < 0 || e.cursor >= len(e.rotation) {
		e.cursor = 0
	}
}

// updatePausedLocked applies D12: a flag-mode sampler with no flag route
// samples nothing. Entering the pause drops every window — sampling stops,
// and a window resumed an hour later would mix two different broadcasts'
// worth of evidence.
func (e *Engine) updatePausedLocked() {
	paused := e.opts.Mode == screening.ModeFlag && !e.flagRoute
	if paused && !e.paused {
		for _, b := range e.bcs {
			b.resetLocked(e, b.epoch)
		}
		e.publishTrippedLocked()
	}
	e.paused = paused
	e.opts.Metrics.setPaused(paused)
}

// resetLocked starts a new session's state: an empty window, no recent, no
// frame to repeat. The old epoch is retired so a sample of it still in
// flight cannot bring it back.
func (b *broadcast) resetLocked(e *Engine, epoch string) {
	if b.epoch != "" && b.epoch != epoch {
		b.retired = append(b.retired, b.epoch)
		if len(b.retired) > 4 {
			b.retired = b.retired[len(b.retired)-4:]
		}
	}
	b.epoch = epoch
	b.window = screening.NewWindow(e.opts.Rule)
	b.recs = nil
	b.recent = nil
	b.lastScores = nil
	b.etag = ""
	b.pendingClear = false
}

func (e *Engine) publishTrippedLocked() {
	n := 0
	for _, b := range e.bcs {
		if b.window != nil && b.window.Tripped() {
			n++
		}
	}
	e.opts.Metrics.setTripped(n)
}

// ---------------------------------------------------------------------------
// Scheduling (D6)
// ---------------------------------------------------------------------------

func (e *Engine) dispatchLoop(ctx context.Context) {
	var last time.Time
	for {
		e.mu.Lock()
		n := len(e.rotation)
		idle := n == 0 || e.paused
		e.mu.Unlock()
		if idle {
			last = time.Time{}
			select {
			case <-ctx.Done():
				return
			case <-e.wake:
			case <-time.After(time.Second):
			}
			continue
		}
		// Spread the rotation across the interval rather than bursting it.
		if !last.IsZero() {
			step := e.opts.Interval / time.Duration(n)
			if wait := time.Until(last.Add(step)); wait > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			}
		}
		// A dispatch waits for a free worker: under overload this is where
		// the rotation slows, evenly for everyone.
		select {
		case <-ctx.Done():
			return
		case e.sem <- struct{}{}:
		}
		b := e.next()
		if b == nil {
			<-e.sem
			continue
		}
		last = time.Now()
		e.opts.Metrics.busy(1)
		e.samples.Add(1)
		go func() {
			defer e.samples.Done()
			defer func() { <-e.sem; e.opts.Metrics.busy(-1) }()
			e.sample(ctx, b)
		}()
	}
}

// next picks the rotation's next broadcast and marks it busy. A broadcast
// whose previous sample is still running is waited for, not skipped: its
// samples must apply in order, and skipping it would silently stretch its
// interval alone. Nil when the rotation changed under the wait.
func (e *Engine) next() *broadcast {
	e.mu.Lock()
	defer e.mu.Unlock()
	for {
		if e.stopping || e.paused || len(e.rotation) == 0 {
			return nil
		}
		if e.cursor >= len(e.rotation) {
			e.cursor = 0
		}
		b := e.rotation[e.cursor]
		if !b.busy {
			e.cursor = (e.cursor + 1) % len(e.rotation)
			now := e.opts.Now()
			if !b.lastDispatch.IsZero() {
				b.interval = now.Sub(b.lastDispatch)
			}
			b.lastDispatch = now
			b.busy = true
			e.publishEffectiveLocked()
			return b
		}
		e.cond.Wait()
		if b.gone || b.exempt {
			continue
		}
	}
}

// publishEffectiveLocked exports the longest current per-broadcast interval.
func (e *Engine) publishEffectiveLocked() {
	longest := time.Duration(0)
	for _, b := range e.rotation {
		longest = max(longest, b.interval)
	}
	e.opts.Metrics.setEffective(longest.Seconds())
}

// EffectiveIntervals reports each rotation member's last measured interval
// (tests and diagnostics).
func (e *Engine) EffectiveIntervals() map[string]time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]time.Duration, len(e.rotation))
	for _, b := range e.rotation {
		out[b.id] = b.interval
	}
	return out
}

// ---------------------------------------------------------------------------
// One sample
// ---------------------------------------------------------------------------

func (e *Engine) sample(ctx context.Context, b *broadcast) {
	defer func() {
		e.mu.Lock()
		b.busy = false
		e.cond.Broadcast()
		e.mu.Unlock()
	}()

	e.mu.Lock()
	relayBase, id, key := b.relay, b.id, b.key
	etag := ""
	if b.lastScores != nil {
		etag = b.etag
	}
	e.mu.Unlock()

	start := time.Now()
	k, err := e.opts.Keyframes.Fetch(ctx, relayBase, id, etag)
	e.opts.Metrics.stage("fetch", time.Since(start).Seconds())
	if err != nil {
		if errors.Is(err, relay.ErrNotFound) {
			e.opts.Metrics.failure("not_found")
		} else if ctx.Err() == nil {
			e.opts.Metrics.failure("fetch")
			e.log.Debug("keyframe fetch failed", "broadcast_key", key, "err", err)
		}
		return
	}

	var scores map[string]float64
	if k.NotModified {
		e.mu.Lock()
		if b.epoch == k.Epoch && b.lastScores != nil {
			scores = cloneScores(b.lastScores)
		}
		e.mu.Unlock()
		if scores == nil {
			// The state was reset while the request was out; there is no
			// score left to repeat, so this was no sample.
			e.opts.Metrics.failure("stale")
			return
		}
	} else {
		scores, err = e.score(ctx, k, key)
		if err != nil {
			return
		}
	}
	e.opts.Metrics.stage("total", time.Since(start).Seconds())

	f, post := e.apply(b, k, scores, e.opts.Now())
	if post {
		e.postFlag(ctx, b, f)
	}
}

// score converts, decodes and scores one fetched keyframe. Failures are
// counted by stage and logged without a byte of the frame.
func (e *Engine) score(ctx context.Context, k relay.Keyframe, key string) (map[string]float64, error) {
	in, err := bitstream.FromStreamFrame(k.Frame, k.Codec)
	if err != nil {
		e.opts.Metrics.failure("convert")
		e.log.Debug("keyframe not convertible", "broadcast_key", key, "codec", k.Codec, "err", err)
		return nil, err
	}
	t := time.Now()
	rgb, err := e.opts.Decoder.Decode(ctx, in)
	e.opts.Metrics.stage("decode", time.Since(t).Seconds())
	if err != nil {
		if errors.Is(err, decode.ErrTimeout) {
			e.opts.Metrics.failure("decode_timeout")
		} else {
			e.opts.Metrics.failure("decode")
		}
		if ctx.Err() == nil {
			e.log.Debug("keyframe decode failed", "broadcast_key", key, "codec", in.Codec, "err", err)
		}
		return nil, err
	}
	t = time.Now()
	scores, err := e.opts.Scorer.Scores(rgb)
	e.opts.Metrics.stage("inference", time.Since(t).Seconds())
	if err != nil {
		e.opts.Metrics.failure("inference")
		e.log.Warn("inference failed", "broadcast_key", key, "err", err)
		return nil, err
	}
	return scores, nil
}

func cloneScores(s map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

// apply records one sample in the broadcast's session and decides what to
// POST: after every sample while tripped, once on the clear, and a retry of
// a cleared POST that has not landed (D9).
func (e *Engine) apply(b *broadcast, k relay.Keyframe, scores map[string]float64, at time.Time) (screening.ContentFlag, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if b.gone || b.exempt || e.paused || slices.Contains(b.retired, k.Epoch) {
		// It left the list, became exempt, the sampler paused, or this
		// frame belongs to a session already replaced: the sample is
		// discarded, uncounted.
		return screening.ContentFlag{}, false
	}
	if k.Epoch != b.epoch {
		b.resetLocked(e, k.Epoch)
	}
	if !k.NotModified {
		b.lastScores, b.etag = cloneScores(scores), k.ETag
		e.opts.Metrics.sample("scored")
	} else {
		e.opts.Metrics.sample("unchanged")
	}

	score := screening.SampleScore(scores, e.opts.Labels)
	tr := b.window.Observe(score)
	b.recs = append(b.recs, record{at: at, score: score, labels: scores})
	if len(b.recs) > e.opts.Rule.Size {
		b.recs = b.recs[len(b.recs)-e.opts.Rule.Size:]
	}
	b.recent = append(b.recent, score)
	if len(b.recent) > e.opts.ReportSamples {
		b.recent = b.recent[len(b.recent)-e.opts.ReportSamples:]
	}
	e.accumulateLocked(b, scores, at)

	var state string
	switch {
	case b.window.Tripped():
		state = screening.StateTripped
		b.pendingClear = false
		if tr == screening.Tripped {
			e.log.Info("window tripped", "broadcast_key", b.key, "mode", e.opts.Mode, "score", score)
		}
	case tr == screening.Cleared:
		state = screening.StateCleared
		b.pendingClear = true
		e.log.Info("window cleared", "broadcast_key", b.key, "mode", e.opts.Mode)
	case b.pendingClear:
		state = screening.StateCleared
	default:
		e.publishTrippedLocked()
		return screening.ContentFlag{}, false
	}
	e.publishTrippedLocked()
	return e.flagLocked(b, state), true
}

// flagLocked builds the content flag for the broadcast's current window.
func (e *Engine) flagLocked(b *broadcast, state string) screening.ContentFlag {
	f := screening.ContentFlag{
		Schema:         screening.SchemaContentFlag,
		BroadcastID:    b.id,
		PublisherEpoch: b.epoch,
		SamplerID:      e.opts.SamplerID,
		Mode:           e.opts.Mode,
		State:          state,
		Labels:         []string{},
	}
	if len(b.recs) > 0 {
		f.Window.Start, f.Window.End = b.recs[0].at, b.recs[len(b.recs)-1].at
	}
	f.Window.Samples = len(b.recs)
	counts := map[string]int{}
	sum := 0.0
	for _, r := range b.recs {
		sum += r.score
		f.Scores.Max = math.Max(f.Scores.Max, r.score)
		if r.score >= e.opts.Rule.Threshold {
			f.Window.FlaggedSamples++
		}
		for _, l := range e.opts.Labels {
			if r.labels[l] >= e.opts.Rule.Threshold {
				counts[l]++
			}
		}
	}
	if len(b.recs) > 0 {
		f.Scores.Mean = sum / float64(len(b.recs))
	}
	for _, l := range e.opts.Labels {
		if counts[l] > 0 {
			f.Labels = append(f.Labels, l)
		}
	}
	if len(counts) > 0 {
		f.LabelCounts = counts
	}
	if state == screening.StateTripped {
		f.Recent = append([]float64(nil), b.recent...)
		f.Summary = fmt.Sprintf("sustained explicit signal: %d of %d samples at or above %.2f, max %.2f",
			f.Window.FlaggedSamples, f.Window.Samples, e.opts.Rule.Threshold, f.Scores.Max)
	} else {
		f.Summary = fmt.Sprintf("signal cleared: no sample of the last %d at or above %.2f", f.Window.Samples, e.opts.Rule.Threshold)
	}
	return f
}

// postFlag sends one content flag and acts on the answer.
func (e *Engine) postFlag(ctx context.Context, b *broadcast, f screening.ContentFlag) {
	cctx, cancel := context.WithTimeout(ctx, e.opts.PostTimeout)
	resp, err := e.opts.Admin.PostFlag(cctx, f)
	cancel()
	switch {
	case errors.Is(err, admin.ErrNoFlagRoute):
		// gawk-admin lost its pager between two target refreshes: pause now
		// rather than keep scoring into a refusal (D12).
		e.opts.Metrics.flagPost(f.State, "no_flag_route")
		e.mu.Lock()
		e.flagRoute = false
		e.updatePausedLocked()
		e.mu.Unlock()
		return
	case err != nil:
		e.opts.Metrics.postError("content_flags")
		if ctx.Err() == nil {
			e.log.Warn("content flag not delivered", "broadcast_key", b.key, "state", f.State, "err", err)
		}
		return
	}
	e.opts.Metrics.flagPost(f.State, resp.Status)
	if f.State == screening.StateCleared {
		e.mu.Lock()
		if b.epoch == f.PublisherEpoch && !b.window.Tripped() {
			b.pendingClear = false
		}
		e.mu.Unlock()
	}
	if resp.Status == screening.FlagStatusRaised {
		e.log.Info("content flag raised", "broadcast_key", b.key, "mode", f.Mode, "flag_id", resp.FlagID)
	}
}

// ---------------------------------------------------------------------------
// Sample batches (D20)
// ---------------------------------------------------------------------------

// accumulateLocked counts one sample into its session's pending batch entry
// and keeps it when some flagged label reaches the floor. Positions are
// 1-based among the samples the entry has taken; gawk-admin turns them into
// per-session sequence numbers.
func (e *Engine) accumulateLocked(b *broadcast, scores map[string]float64, at time.Time) {
	s := e.acc[b.epoch]
	if s == nil {
		s = &screening.SessionSamples{
			BroadcastID:    b.id,
			BroadcastKey:   b.key,
			PublisherEpoch: b.epoch,
			FirstAt:        at,
			Samples:        []screening.Sample{},
		}
		e.acc[b.epoch] = s
	}
	s.Taken++
	s.LastAt = at
	keep := false
	for _, l := range e.opts.Labels {
		if scores[l] >= e.opts.Floor {
			keep = true
			break
		}
	}
	if keep {
		stored := make(map[string]float64, len(e.opts.Labels))
		for _, l := range e.opts.Labels {
			stored[l] = scores[l]
		}
		s.Samples = append(s.Samples, screening.Sample{Position: s.Taken, At: at, Scores: stored})
	}
}

func (e *Engine) batchLoop(ctx context.Context) {
	t := time.NewTicker(e.opts.BatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.flush(ctx)
		}
	}
}

// flush sends every pending session entry in one batch. On failure the entries
// go back, merged in front of anything taken since, so the next flush sends
// one consistent count; past the batch size limits they are dropped and
// counted rather than held without bound.
func (e *Engine) flush(ctx context.Context) {
	e.mu.Lock()
	pending := e.acc
	e.acc = make(map[string]*screening.SessionSamples)
	e.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	sessions := make([]screening.SessionSamples, 0, len(pending))
	sent := 0
	for _, s := range pending {
		sessions = append(sessions, *s)
		sent += len(s.Samples)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].FirstAt.Before(sessions[j].FirstAt) })
	batch := screening.SampleBatch{
		Schema:    screening.SchemaSamples,
		SamplerID: e.opts.SamplerID,
		Mode:      e.opts.Mode,
		Floor:     e.opts.Floor,
		Labels:    e.opts.Labels,
		// The rule travels with the samples so gawk-admin draws the threshold
		// that was in force (docs/72 D1).
		Rule: &screening.BatchRule{
			Threshold: e.opts.Rule.Threshold, Window: e.opts.Rule.Size, WindowHits: e.opts.Rule.Hits,
		},
		Sessions: sessions,
	}
	cctx, cancel := context.WithTimeout(ctx, e.opts.PostTimeout)
	err := e.opts.Admin.PostSamples(cctx, batch)
	cancel()
	if err == nil {
		e.opts.Metrics.sent(sent)
		return
	}
	e.opts.Metrics.postError("samples")
	if ctx.Err() == nil {
		e.log.Warn("sample batch not delivered; retrying with the next", "sessions", len(sessions), "err", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	total, count := 0, 0
	for epoch, p := range pending {
		if cur := e.acc[epoch]; cur != nil {
			for _, s := range cur.Samples {
				s.Position += p.Taken
				p.Samples = append(p.Samples, s)
			}
			p.Taken += cur.Taken
			p.LastAt = cur.LastAt
		}
		e.acc[epoch] = p
	}
	for _, s := range e.acc {
		total += len(s.Samples)
		count++
	}
	if count > screening.MaxBatchSessions || total > screening.MaxBatchSamples {
		e.opts.Metrics.dropped(total)
		e.log.Error("gawk-admin unreachable past the batch limit: dropping pending samples", "samples", total, "sessions", count)
		e.acc = make(map[string]*screening.SessionSamples)
	}
}
