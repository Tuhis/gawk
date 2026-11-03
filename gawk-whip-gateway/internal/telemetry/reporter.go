// Package telemetry is the gateway's R28 reporter (docs/83 WG11): one per
// WHIP session, posting the same ingest envelope the browser and the native
// broadcasters post, so an OBS broadcast gets a broadcaster lane in
// gawk-telemetry like any other.
//
// It is the Go broadcaster's reporter (docs/33 TM2), which R56 LX9 removed
// with that binary, restored here with two changes: the client class and OS
// are options (the gateway reports itself, and the SENDER's OS class), and
// the byte budget is the native engine's 16 MiB (docs/77 D12) — a gateway
// session is as long as a native one. The same four properties hold:
//
//   - Zero PII: the obfuscated broadcast key from the hello, never the raw
//     joinable ID or the stream key; a coarse client class, never an
//     address or a user-agent string.
//   - Never on the media hot path: Report() only appends to a small buffer
//     under a short lock, and every send happens on the reporter's own
//     goroutine. Nothing here can block the session loop.
//   - Off means off: with no hello, a hello with enabled=false, or no ingest
//     URL (configured or relay-advertised), the reporter issues zero HTTP
//     requests.
//   - A truncated session says so: the byte budget degrades to events-only and
//     marks it, so a clipped session never reads as a complete short one.
package telemetry

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

const (
	// FlushInterval matches the browser collector's: ~10 s of samples is a
	// few KB, small enough that a failed batch costs nothing.
	FlushInterval = 10 * time.Second
	// SessionByteBudget bounds one session's uncompressed JSON: the native
	// engine's (docs/77 D12), about five hours at the fleet's 2 s cadence.
	SessionByteBudget = 16 << 20
	// MaxSendAttempts before a batch is dropped for good. Telemetry has no
	// delivery guarantee and must not behave as if it does.
	MaxSendAttempts = 3
	retryBase       = 2 * time.Second
	// Bounds on what one batch may hold, so a wedged endpoint cannot turn the
	// reporter into the memory leak it exists to help diagnose.
	maxPendingSamples = 64
	maxPendingEvents  = 256
	sendTimeout       = 10 * time.Second
	// Depth of the outbound queue. Batches are sent by ONE goroutine so they
	// arrive in the order they were produced — a `final` batch overtaking an
	// earlier one would tell the service a session ended while data for it was
	// still in flight. A full queue sheds the OLDEST batch, keeping the window
	// near live for the same reason the sample buffer does.
	sendQueueDepth = 8
)

// Sample is one stats observation, timestamped relative to session start.
type Sample struct {
	TMs   int64 `json:"tMs"`
	Stats any   `json:"stats"`
}

// Event is something a sample grid cannot represent — a reconnect, a close
// code, a cascade fallback. These carry the story.
type Event struct {
	TMs    int64  `json:"tMs"`
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
}

// AppInfo identifies the client that produced a batch. `Version` is the schema
// version (docs/33 D15) and `Browser` is a client CLASS — "gawk-whip-gateway"
// here, "Chrome 152" from a browser — so one field answers "what produced
// this?" across every producer.
type AppInfo struct {
	Version string `json:"version"`
	Surface string `json:"surface"`
	Browser string `json:"browser"`
	OS      string `json:"os"`
}

// Batch is the ingest envelope. Field-for-field identical to the browser's.
type Batch struct {
	V            int      `json:"v"`
	Token        string   `json:"token"`
	Role         string   `json:"role"`
	BroadcastKey string   `json:"broadcastKey"`
	Seq          int      `json:"seq"`
	Final        bool     `json:"final"`
	App          AppInfo  `json:"app"`
	StartedAtMs  int64    `json:"startedAtMs"`
	Samples      []Sample `json:"samples"`
	Events       []Event  `json:"events"`
	Truncated    bool     `json:"truncated,omitempty"`
}

// Options configures a Reporter. A zero URL disables it entirely.
type Options struct {
	// URL is the ingest endpoint, e.g. https://gawk.example.com/api/telemetry/v1/ingest.
	// Empty disables the reporter unless the relay advertises one.
	URL string
	// Version is this build's version string; it doubles as the schema version.
	Version string
	// ClientClass is app.browser, the producer's client class
	// ("gawk-whip-gateway" when empty).
	ClientClass string
	// OS is app.os: the coarse OS class of the machine the stream comes
	// from ("Windows", "macOS", "Linux"), or "" when unknown.
	OS     string
	Client *http.Client
	Log    *slog.Logger
	// now is injectable so tests drive time without wall clocks.
	now func() time.Time
	// afterFinish runs inside Close between its Finish and closed being
	// set: the window a racing Begin can land in (tests only).
	afterFinish func(*Reporter)
	// afterTake runs inside Flush between taking a batch (its seq) and
	// enqueueing it (tests only).
	afterTake func(final bool)
	// afterFinalFlush runs inside Finish right after its final Flush
	// (tests only).
	afterFinalFlush func()
}

// Reporter batches stats and events for one broadcaster session at a time.
// Safe for concurrent use: Report/Event may be called from the engine's stats
// goroutine while the flush loop runs.
type Reporter struct {
	opts Options
	log  *slog.Logger

	mu sync.Mutex
	// url is Options.URL.
	url string
	// advertisedURL is the relay-advertised ingest endpoint (R37, wire 0x12,
	// docs/40 §4.10). Non-empty, it wins over url (D15): the relay minted the
	// session's token, so it is the one party that can name a destination
	// where that token verifies. The shells clear it at session start and set
	// it when the engine dispatches a 0x12.
	advertisedURL    string
	token            string
	broadcastKey     string
	reportInterval   time.Duration
	startedAt        time.Time
	startedAtUnixMs  int64
	samples          []Sample
	events           []Event
	seq              int
	bytesUsed        int
	truncated        bool
	lastSampleAt     time.Time
	haveLastSampleAt bool

	cancel context.CancelFunc
	wg     sync.WaitGroup

	// sendCh serializes outbound batches through one goroutine (see
	// sendQueueDepth). Created in New and closed exactly once by Close:
	// buffered and non-blocking on the producer side, so a stalled endpoint
	// can never block a caller on the media path. sendMu orders enqueue
	// against that close, and closed makes every call after Close a no-op —
	// a gateway session closes its reporter while its engine may still
	// deliver a hello (WG11).
	sendCh    chan outbound
	sendMu    sync.Mutex
	closed    bool
	closeOnce sync.Once
}

// outbound is one batch bound to the endpoint that was configured when it was
// produced. Carrying the URL rather than reading it at send time is what keeps
// a repoint from misdelivering the previous session's final batch: its token is
// an HMAC only the old collector can verify, so sending it to the new one is
// both a wasted request and the wrong session's data at the wrong address.
type outbound struct {
	url  string
	body []byte
}

// New builds a Reporter. It never returns nil; a Reporter with no URL is inert.
func New(opts Options) *Reporter {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: sendTimeout}
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.ClientClass == "" {
		opts.ClientClass = "gawk-whip-gateway"
	}
	r := &Reporter{opts: opts, log: opts.Log, url: opts.URL, sendCh: make(chan outbound, sendQueueDepth)}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for o := range r.sendCh {
			r.deliver(o)
		}
	}()
	return r
}

// Enabled reports whether this reporter can currently send anything — a
// configured URL or a relay-advertised one.
func (r *Reporter) Enabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.effectiveURL() != ""
}

// effectiveURL is the endpoint batches go to right now: the relay-advertised
// URL when one exists, the configured one otherwise (docs/40 D15). Callers
// hold r.mu.
func (r *Reporter) effectiveURL() string {
	if r.advertisedURL != "" {
		return r.advertisedURL
	}
	return r.url
}

// SetAdvertisedURL records the relay-advertised ingest endpoint (R37, wire
// 0x12); "" clears it — a new session against a relay that advertises nothing
// must not inherit the previous relay's endpoint. It governs what is
// produced from here on: batches already queued keep the endpoint they
// were produced for.
//
// The 0x12 stream races the 0x0D hello (separate uni streams, no ordering),
// so this may legitimately arrive after Begin — which is why Begin adopts the
// session identity even with no endpoint yet, and the endpoint check happens
// per flush.
func (r *Reporter) SetAdvertisedURL(u string) {
	r.mu.Lock()
	changed := r.advertisedURL != u
	r.advertisedURL = u
	r.mu.Unlock()
	if changed && u != "" {
		// Out loud, like every other repoint: diagnostics landing at a
		// different operator's collector should be readable in this process's
		// own logs, not just inferred from the wire.
		r.log.Info("relay advertised its telemetry ingest endpoint; reporting there", "url", u)
	}
}

// Begin adopts a session identity from wire 0x0D. A reconnect is a NEW relay
// session with a NEW token, so this closes the previous session with a final
// batch and starts a fresh one — two transport sessions must never merge into
// one row.
func (r *Reporter) Begin(h wire.TelemetryHello) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return
	}
	// Both ways of declining to report are logged, and that is not decoration.
	// A reporter that goes quiet looks exactly like a client that crashed, a
	// relay with no key, or a batch the network ate — and telling those apart
	// after the fact meant reading relay pod logs against stored sessions
	// (2026-07-27). Each of these lines is that investigation, pre-answered.
	if !r.Enabled() {
		// Not a hard stop (R37, docs/40 §4.10): the relay may advertise its
		// ingest endpoint on its own uni stream (wire 0x12), which can arrive
		// after this hello. Adopt the identity and collect into the bounded
		// buffers; Flush sends nothing until an endpoint exists, so with no
		// advertisement this session still makes zero requests.
		r.log.Info("telemetry hello received but no ingest URL is configured; nothing is sent unless the relay advertises an endpoint")
	}
	if !h.Enabled {
		// A fleet with telemetry off. Drop anything buffered and go inert
		// rather than queueing into a void.
		r.log.Info("relay reports fleet telemetry is off; this session will not report")
		r.stopLoop()
		r.mu.Lock()
		r.token, r.samples, r.events = "", nil, nil
		r.mu.Unlock()
		return
	}
	r.Finish()

	interval := time.Duration(h.ReportIntervalMs) * time.Millisecond
	if interval < 250*time.Millisecond {
		interval = 250 * time.Millisecond
	}
	now := r.opts.now()

	r.mu.Lock()
	if r.closed {
		// Close ran since the check at the top: stay inert.
		r.mu.Unlock()
		return
	}
	r.token = hex.EncodeToString(h.Token)
	r.broadcastKey = hex.EncodeToString(h.BroadcastKey)
	r.reportInterval = interval
	r.startedAt = now
	r.startedAtUnixMs = now.UnixMilli()
	r.samples, r.events = nil, nil
	r.seq, r.bytesUsed = 0, 0
	r.truncated = false
	r.haveLastSampleAt = false
	url := r.effectiveURL()
	r.mu.Unlock()

	r.startLoop()

	// The session id, derived from the token's nonce exactly as the relay
	// derives it — so this line names the same row /statusz and the dashboard
	// show, and "my broadcast is not in the list" stops being an investigation.
	// The token itself is a bearer credential and is never logged.
	sessionID, err := wire.TelemetrySessionID(h.Token)
	if err != nil {
		sessionID = "?"
	}
	// The endpoint too: a relay-advertised one replaces the configured one,
	// and a NetworkPolicy that opens only the configured one would drop
	// every batch silently (PR #699 review).
	r.log.Info("telemetry session started", "session_id", sessionID, "report_interval", interval, "url", url)
}

// Report records one stats observation, decimated to the relay-requested
// cadence, and reports whether it was kept. Cheap enough to call from the
// session loop.
func (r *Reporter) Report(stats any) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.token == "" || r.truncated {
		return false
	}
	now := r.opts.now()
	if r.haveLastSampleAt && now.Sub(r.lastSampleAt) < r.reportInterval {
		return false
	}
	r.lastSampleAt = now
	r.haveLastSampleAt = true
	r.samples = append(r.samples, Sample{TMs: now.Sub(r.startedAt).Milliseconds(), Stats: stats})
	// Shed the OLDEST: a wedged sender is exactly when the recent samples are
	// the interesting ones.
	if n := len(r.samples); n > maxPendingSamples {
		r.samples = append(r.samples[:0], r.samples[n-maxPendingSamples:]...)
	}
	return true
}

// Event records something the sample grid cannot represent.
func (r *Reporter) Event(kind, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.token == "" {
		return
	}
	r.events = append(r.events, Event{
		TMs:    r.opts.now().Sub(r.startedAt).Milliseconds(),
		Kind:   truncateStr(kind, 64),
		Detail: truncateStr(detail, 256),
	})
	if n := len(r.events); n > maxPendingEvents {
		r.events = append(r.events[:0], r.events[n-maxPendingEvents:]...)
	}
}

// Flush sends whatever is pending. `final` marks the session's last batch so
// the service can finalize without waiting out an idle timeout.
func (r *Reporter) Flush(final bool) {
	r.mu.Lock()
	url := r.effectiveURL()
	r.mu.Unlock()
	// No endpoint, configured or advertised: nothing could be delivered, so
	// keep the (bounded, oldest-shed) buffers instead of draining them into a
	// void — a 0x12 advertisement arriving moments from now still gets the
	// session's recent window. With none ever arriving this is the docs/40
	// §4.10 guard: batches that could only be rejected are never sent.
	if url == "" {
		return
	}
	// sendMu spans take (which numbers the batch) through enqueue, so
	// batches reach the sender in seq order: a final batch can never
	// overtake one taken before it. deliver never takes sendMu.
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	batch, ok := r.take(final)
	if !ok {
		return
	}
	if r.opts.afterTake != nil {
		r.opts.afterTake(final)
	}
	body, err := json.Marshal(batch)
	if err != nil {
		// A stats value that will not serialize is a bug in the caller, not a
		// reason to retry: drop the batch.
		r.log.Debug("telemetry batch not serializable; dropped", "err", err)
		return
	}

	r.mu.Lock()
	r.bytesUsed += len(body)
	crossed := !r.truncated && r.bytesUsed >= SessionByteBudget
	if crossed {
		r.truncated = true
	}
	r.mu.Unlock()

	if crossed {
		// Degrade rather than stop, and say so OUT LOUD. Setting the flag
		// alone is not enough: once samples stop, every later flush would be
		// empty and return early, so the marker would never reach the wire and
		// a clipped session would read as one that simply ended here.
		r.Event("telemetry-budget-exhausted", "events only from here")
	}
	r.enqueueLocked(outbound{url: url, body: body})
}

// Finish ends the current session: a final batch, then the flush loop stops
// and the identity is forgotten. Safe to call repeatedly.
func (r *Reporter) Finish() {
	r.mu.Lock()
	active := r.token != ""
	r.mu.Unlock()
	if !active {
		return
	}
	r.Flush(true)
	if r.opts.afterFinalFlush != nil {
		r.opts.afterFinalFlush()
	}
	r.stopLoop()
	r.mu.Lock()
	r.token = ""
	r.mu.Unlock()
}

// Close ends the session and waits for in-flight sends. Called on shutdown;
// safe to call more than once.
func (r *Reporter) Close() {
	r.Finish()
	if r.opts.afterFinish != nil {
		r.opts.afterFinish(r)
	}
	r.mu.Lock()
	r.closed = true
	r.token = ""
	r.mu.Unlock()
	// A Begin racing Finish may have started a loop after Finish's
	// stopLoop; startLoop can't start one now that closed is set, so
	// cancel any that did.
	r.stopLoop()
	r.closeOnce.Do(func() {
		r.sendMu.Lock()
		close(r.sendCh)
		r.sendMu.Unlock()
	})
	r.wg.Wait()
}

// take snapshots and clears the pending buffers under the lock, returning
// false when there is nothing to send (and this is not a final batch).
func (r *Reporter) take(final bool) (Batch, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.token == "" {
		return Batch{}, false
	}
	if len(r.samples) == 0 && len(r.events) == 0 && !final {
		return Batch{}, false
	}
	b := Batch{
		V:            1,
		Token:        r.token,
		Role:         string(wire.TelemetryRoleBroadcaster),
		BroadcastKey: r.broadcastKey,
		Seq:          r.seq,
		Final:        final,
		App: AppInfo{
			Version: r.opts.Version,
			Surface: "broadcaster",
			Browser: r.opts.ClientClass,
			OS:      r.opts.OS,
		},
		StartedAtMs: r.startedAtUnixMs,
		Samples:     r.samples,
		Events:      r.events,
		Truncated:   r.truncated,
	}
	if b.Samples == nil {
		b.Samples = []Sample{}
	}
	if b.Events == nil {
		b.Events = []Event{}
	}
	r.seq++
	r.samples, r.events = nil, nil
	if final {
		// The session ends with the batch that says so, under the lock that
		// numbered it: no later take or Report sees it, so nothing can
		// follow the final batch (PR #699 round 2).
		r.token = ""
	}
	return b, true
}

// enqueueLocked hands a batch to the single sender goroutine. Never blocks: a
// stalled endpoint must not stall a caller that may be on the media path.
// The caller holds sendMu.
func (r *Reporter) enqueueLocked(o outbound) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return
	}
	for {
		select {
		case r.sendCh <- o:
			return
		default:
		}
		// Full: shed the oldest queued batch and try once more. Its absence
		// shows up honestly as a gap in the `seq` sequence rather than as a
		// lie about coverage.
		select {
		case <-r.sendCh:
			r.log.Debug("telemetry send queue full; oldest batch dropped")
		default:
			// Drained by the sender in between; loop and retry the send.
		}
	}
}

// deliver posts one batch, retrying a bounded number of times before dropping
// it silently and forever. Retries block this goroutine only — which is the
// point: a queue that keeps producing while an endpoint is dead would grow
// without bound, and the bounded queue above sheds instead.
func (r *Reporter) deliver(o outbound) {
	for attempt := 1; ; attempt++ {
		done, retry := r.post(o)
		if done {
			return
		}
		if !retry || attempt >= MaxSendAttempts {
			// Dropped for good. The gap shows up as a hole in the batch `seq`
			// sequence rather than as a lie about coverage.
			r.log.Debug("telemetry batch dropped; collection continues")
			return
		}
		time.Sleep(time.Duration(attempt) * retryBase)
	}
}

// post attempts one delivery. `done` means the batch is finished with (it
// landed, or it was rejected in a way that will never succeed); `retry` says
// whether another attempt could plausibly work.
//
// A 4xx is the service rejecting THIS batch's shape or token — a bad envelope,
// an expired token — and resending the identical bytes would fail identically,
// so it is dropped immediately rather than three times. Only 5xx and transport
// errors are worth another attempt.
func (r *Reporter) post(o outbound) (done, retry bool) {
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url, bytes.NewReader(o.body))
	if err != nil {
		return true, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.opts.Client.Do(req)
	if err != nil {
		return false, true
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch {
	case resp.StatusCode < 300:
		return true, false
	case resp.StatusCode == http.StatusTooManyRequests:
		// 429 is the one 4xx that says "later, not never": the service is
		// shedding load, and the batch itself is fine. Treating it as
		// permanent — as this did — makes the two collectors disagree about
		// the same status, and drops telemetry precisely when the fleet is
		// busy enough to be worth having.
		return false, true
	case resp.StatusCode >= 500:
		return false, true
	default:
		return false, false
	}
}

func (r *Reporter) startLoop() {
	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	if r.closed {
		// Close won the race with a late Begin: no loop to wait for.
		r.mu.Unlock()
		cancel()
		return
	}
	if r.cancel != nil {
		r.cancel() // a loop from an earlier session that was never stopped
	}
	r.cancel = cancel
	// Added under the lock Close sets closed under, so a loop either
	// exists before Close waits for it or never starts.
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		t := time.NewTicker(FlushInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.Flush(false)
			}
		}
	}()
}

func (r *Reporter) stopLoop() {
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// truncateStr bounds a string to n bytes. A plain byte slice can land inside
// a multi-byte rune, storing invalid UTF-8 that json.Marshal on the ingest
// service then silently rewrites to U+FFFD — corrupting the value rather
// than merely shortening it. Trim back to the last complete rune: never past
// n, only short of it, which is fine because n is a size cap, not a target
// length (same fix as gawk-telemetry/internal/ingest's clip()).
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size != 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}
