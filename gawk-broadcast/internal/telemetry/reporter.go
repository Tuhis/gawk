// Package telemetry is the native broadcaster's R28 reporter (docs/33 TM2).
//
// It is the Go counterpart of gawk-app's TelemetryCollector and produces the
// SAME ingest envelope — one service parses one shape, and a second shape
// would have to be kept in sync forever. Like the browser side it adds no
// measurement: engine.Stats already exists, and this is a pipe.
//
// The same four properties hold, for the same reasons (docs/33 D8/D9):
//
//   - Zero PII: the obfuscated broadcast key from the hello, never the raw
//     joinable ID; a coarse client class, never a machine name.
//   - Never on the media hot path: Report() only appends to a small buffer
//     under a short lock, and every send happens on the reporter's own
//     goroutine. Nothing here can block the encode or send path.
//   - Off means off: with no hello, a hello with enabled=false, or no
//     configured ingest URL, the reporter issues zero HTTP requests.
//   - A truncated session says so: the byte budget degrades to events-only and
//     marks it, so a clipped session never reads as a complete short one.
//
// The ingest URL is NOT derivable here. The browser defaults to a same-origin
// path on the page it was served from; a native binary has only the relay
// origin, which is a different host by construction (the relay is a UDP
// LoadBalancer, the frontend an Ingress — the same reason config.AppURL exists
// separately). So the operator supplies it, and unset means off.
package telemetry

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

const (
	// FlushInterval matches the browser collector's: ~10 s of samples is a
	// few KB, small enough that a failed batch costs nothing.
	FlushInterval = 10 * time.Second
	// SessionByteBudget bounds one session's uncompressed JSON. Same value and
	// same reasoning as the browser side.
	SessionByteBudget = 4 << 20
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
// version (docs/33 D15) and `Browser` is a client CLASS — "gawk-broadcast" for
// this binary, "Chrome 152" from a browser — so one field answers "what
// produced this?" across both producers.
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
	// Empty disables the reporter — it then makes no requests at all. This is
	// the starting value; SetURL repoints it when the settings change.
	URL string
	// Version is this build's version string; it doubles as the schema version.
	Version string
	Client  *http.Client
	Log     *slog.Logger
	// now is injectable so tests drive time without wall clocks.
	now func() time.Time
}

// Reporter batches stats and events for one broadcaster session at a time.
// Safe for concurrent use: Report/Event may be called from the engine's stats
// goroutine while the flush loop runs.
type Reporter struct {
	opts Options
	log  *slog.Logger

	mu sync.Mutex
	// url is Options.URL after any SetURL. It lives under the lock because the
	// settings UI can repoint it between sessions while the sender goroutine is
	// still draining the previous one's queue.
	url              string
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
	// sendQueueDepth). Created in New and closed exactly once by Close, so its
	// lifetime needs no lock: buffered and non-blocking on the producer side,
	// so a stalled endpoint can never block a caller on the media path.
	sendCh    chan outbound
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

// Enabled reports whether this reporter can currently send anything.
func (r *Reporter) Enabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.url != ""
}

// SetURL repoints the reporter, "" turning it off. The settings UI owns this
// value and the reporter outlives any one broadcast, so the endpoint has to be
// re-readable rather than fixed at construction — otherwise a user who edits
// the field has to restart the app for it to mean anything.
//
// It governs what is produced from here on. Batches already queued keep the
// endpoint they were produced for and still go there; turning telemetry off
// stops the next session, it does not retroactively unsend the last one.
func (r *Reporter) SetURL(u string) {
	r.mu.Lock()
	r.url = u
	r.mu.Unlock()
}

// Begin adopts a session identity from wire 0x0D. A reconnect is a NEW relay
// session with a NEW token, so this closes the previous session with a final
// batch and starts a fresh one — two transport sessions must never merge into
// one row.
func (r *Reporter) Begin(h wire.TelemetryHello) {
	if !r.Enabled() {
		return
	}
	if !h.Enabled {
		// A fleet with telemetry off. Drop anything buffered and go inert
		// rather than queueing into a void.
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
	r.token = hex.EncodeToString(h.Token)
	r.broadcastKey = hex.EncodeToString(h.BroadcastKey)
	r.reportInterval = interval
	r.startedAt = now
	r.startedAtUnixMs = now.UnixMilli()
	r.samples, r.events = nil, nil
	r.seq, r.bytesUsed = 0, 0
	r.truncated = false
	r.haveLastSampleAt = false
	r.mu.Unlock()

	r.startLoop()
}

// Report records one stats observation, decimated to the relay-requested
// cadence. Cheap enough to call from the engine's stats callback.
func (r *Reporter) Report(stats any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.token == "" || r.truncated {
		return
	}
	now := r.opts.now()
	if r.haveLastSampleAt && now.Sub(r.lastSampleAt) < r.reportInterval {
		return
	}
	r.lastSampleAt = now
	r.haveLastSampleAt = true
	r.samples = append(r.samples, Sample{TMs: now.Sub(r.startedAt).Milliseconds(), Stats: stats})
	// Shed the OLDEST: a wedged sender is exactly when the recent samples are
	// the interesting ones.
	if n := len(r.samples); n > maxPendingSamples {
		r.samples = append(r.samples[:0], r.samples[n-maxPendingSamples:]...)
	}
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
	batch, ok := r.take(final)
	if !ok {
		return
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
	url := r.url
	r.mu.Unlock()

	if crossed {
		// Degrade rather than stop, and say so OUT LOUD. Setting the flag
		// alone is not enough: once samples stop, every later flush would be
		// empty and return early, so the marker would never reach the wire and
		// a clipped session would read as one that simply ended here.
		r.Event("telemetry-budget-exhausted", "events only from here")
	}
	if url == "" {
		return
	}
	r.enqueue(outbound{url: url, body: body})
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
	r.stopLoop()
	r.mu.Lock()
	r.token = ""
	r.mu.Unlock()
}

// Close ends the session and waits for in-flight sends. Called on shutdown;
// safe to call more than once.
func (r *Reporter) Close() {
	r.Finish()
	r.closeOnce.Do(func() { close(r.sendCh) })
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
			Browser: "gawk-broadcast",
			OS:      clientOS(),
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
	return b, true
}

// enqueue hands a batch to the single sender goroutine. Never blocks: a
// stalled endpoint must not stall a caller that may be on the media path.
func (r *Reporter) enqueue(o outbound) {
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
	r.cancel = cancel
	r.mu.Unlock()
	r.wg.Add(1)
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

// clientOS is the coarse OS class, matching the browser collector's vocabulary
// so one query groups both producers.
func clientOS() string {
	switch runtime.GOOS {
	case "linux":
		return "Linux"
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	default:
		return runtime.GOOS
	}
}
