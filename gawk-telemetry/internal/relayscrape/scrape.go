// Package relayscrape polls each relay pod's ops /statusz and derives the
// relay-side half of every session (docs/33 D5 + TM4).
//
// **The relay is scraped, and never pushes.** That is not fastidiousness: the
// relay process carries the media hot path for every broadcast on the pod, and
// R19's finding 12 and R21's whole design are about how a queue in that
// process becomes a stall in someone's video. The one thing this design must
// never do is put a telemetry backpressure path inside it.
//
// Three consequences are accepted and recorded rather than hidden:
//
//   - **Sessions shorter than the scrape interval are invisible to the relay
//     side.** Their rollups are client-only and carry `relayCoverage: "none"`,
//     so a verdict never silently rests on a relay view that does not exist.
//   - **Close-time folded totals are missed.** A subscriber's final counters
//     fold into hub totals on close; the last pre-close scrape is the record,
//     and the delta is bounded by the interval.
//   - **Per-pod scraping needs per-pod addresses.** The metrics Service is a
//     ClusterIP and load-balances, so scraping it hits one random pod. The
//     relay chart gains an optional HEADLESS companion Service whose DNS A
//     records enumerate the pods; this resolves it each interval.
//
// The join is `sessionId`, which exists on both sides only because of TM1.
package relayscrape

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

// DefaultInterval is how often each pod is polled. 5 s bounds the close-time
// fold delta and keeps a live dashboard's relay side under half its own poll
// period.
const DefaultInterval = 5 * time.Second

// Statusz is the subset of the relay's GET /statusz this service reads.
//
// It is a deliberate SUBSET, decoded structurally rather than by importing the
// relay's types: `hub.RegistryStats` lives under gawk-server/internal/ and is
// unimportable by construction, and /statusz is a JSON API whose field names
// are already a contract (R9 shipped it, R17/R19/R21 extended it additively).
// A field this service does not know about is simply not read — which is the
// same skew tolerance D15 demands everywhere else.
type Statusz struct {
	Broadcasts map[string]Broadcast `json:"broadcasts"`
}

// Broadcast is one broadcast's relay-side view on one pod.
type Broadcast struct {
	PublisherActive    bool   `json:"publisherActive"`
	PublisherSessionID string `json:"publisherSessionId"`
	Role               string `json:"role"`
	Subscribers        int    `json:"subscribers"`
	EdgeSessions       int    `json:"edgeSessions"`
	ViewersGlobal      uint32 `json:"viewersGlobal"`

	FramesRelayed             uint64 `json:"framesRelayed"`
	DatagramsRelayed          uint64 `json:"datagramsRelayed"`
	DatagramsDropped          uint64 `json:"datagramsDropped"`
	BadDatagrams              uint64 `json:"badDatagrams"`
	BandwidthDroppedDatagrams uint64 `json:"bandwidthDroppedDatagrams"`
	IngressFramesLost         uint64 `json:"ingressFramesLost"`
	IngressChunksLost         uint64 `json:"ingressChunksLost"`
	KeyframeStreamsIn         uint64 `json:"keyframeStreamsIn"`
	KeyframeStreamsSent       uint64 `json:"keyframeStreamsSent"`
	KeyframeStreamsDropped    uint64 `json:"keyframeStreamsDropped"`
	SendErrors                uint64 `json:"sendErrors"`
	ReliableSubscribers       int    `json:"reliableSubscribers"`
	CarrierRecordsDropped     uint64 `json:"carrierRecordsDropped"`
	CarrierQueueOverflow      uint64 `json:"carrierQueueOverflow"`
	DVRSubscribers            int    `json:"dvrSubscribers"`
	DVRResyncs                uint64 `json:"dvrResyncs"`

	SubscriberDetails []Subscriber `json:"subscriberDetails"`
}

// Subscriber is one subscriber's relay-side view. `SessionID` is TM1's join
// key — the field whose absence made the whole per-viewer question
// unanswerable before R28.
type Subscriber struct {
	Key                   string `json:"key"`
	SessionID             string `json:"sessionId"`
	QueueDepth            int    `json:"queueDepth"`
	Dropped               uint64 `json:"dropped"`
	SendErrors            uint64 `json:"sendErrors"`
	KeyframesSent         uint64 `json:"keyframesSent"`
	KeyframesDropped      uint64 `json:"keyframesDropped"`
	Internal              bool   `json:"internal"`
	Reliable              bool   `json:"reliable"`
	CarrierStreams        uint64 `json:"carrierStreams"`
	CarrierRecords        uint64 `json:"carrierRecords"`
	CarrierRecordsDropped uint64 `json:"carrierRecordsDropped"`
	CarrierQueueOverflow  uint64 `json:"carrierQueueOverflow"`
	DVR                   bool   `json:"dvr"`
	DVRBufferMs           int    `json:"dvrBufferMs"`
	DVRLagMs              int64  `json:"dvrLagMs"`
	DVRGopSeq             int64  `json:"dvrGopSeq"`
	DVRResyncs            uint64 `json:"dvrResyncs"`
}

// Observation is one stored relay-side record. Written per broadcast and per
// subscriber, so a session's two views join on `sessionId` with no index.
type Observation struct {
	// Kind is "broadcast" or "subscriber".
	Kind         string `json:"kind"`
	AtMs         int64  `json:"atMs"`
	Pod          string `json:"pod"`
	Role         string `json:"role"` // origin | edge
	BroadcastKey string `json:"broadcastKey"`
	// SessionID is empty on broadcast records and on edge sessions (which are
	// never issued a telemetry identity — an edge is plumbing, not a client).
	SessionID  string          `json:"sessionId,omitempty"`
	Broadcast  *Broadcast      `json:"broadcast,omitempty"`
	Subscriber *Subscriber     `json:"subscriber,omitempty"`
	Extra      json.RawMessage `json:"-"`
}

// Round is one scrape pass over the whole fleet.
//
// `Complete` is the field that matters, and it exists for exactly one reason:
// the live projection reads a broadcast's ABSENCE from a round as "this
// broadcast is over" (a GC'd hub disappears from /statusz, while a broadcaster
// merely away stays listed with publisherActive=false through the R1 grace).
// That inference is only sound when every resolved pod actually answered. A
// partial round — one pod timing out, a rollout mid-flight, a resolution that
// came back short — must never end anything, or a five-second blip would sweep
// the dashboard into the ended group and take every live problem with it.
type Round struct {
	AtMs         int64
	Observations []Observation
	// Complete is true only when PodsAnswered == Pods and Pods > 0.
	Complete     bool
	Pods         int
	PodsAnswered int
}

// Sink stores observations. Implemented by the store writer and by the TM8
// live projection.
type Sink interface {
	StoreRelay(date string, pod string, lines [][]byte) error
	// ObserveRelay refreshes the live projection with one whole round.
	ObserveRelay(r Round)
}

// Resolver returns the current set of pod addresses to scrape.
type Resolver func(ctx context.Context) ([]string, error)

// Options configure a Scraper.
type Options struct {
	// Resolve returns pod addresses ("10.42.0.7:2112"). Required.
	Resolve  Resolver
	Sink     Sink
	Client   *http.Client
	Log      *slog.Logger
	Now      func() time.Time
	Interval time.Duration
}

// Scraper polls the fleet.
type Scraper struct {
	opts Options
	log  *slog.Logger

	mu   sync.Mutex
	last map[string]time.Time // sessionId → last time the relay saw it
}

// New builds a scraper.
func New(opts Options) (*Scraper, error) {
	if opts.Resolve == nil {
		return nil, fmt.Errorf("relayscrape: Resolve is required")
	}
	if opts.Sink == nil {
		return nil, fmt.Errorf("relayscrape: Sink is required")
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Client == nil {
		// Deliberately short: a pod that cannot answer inside one interval is
		// a pod whose numbers would be stale anyway, and a hung scrape must
		// never delay the next round.
		opts.Client = &http.Client{Timeout: opts.Interval}
	}
	return &Scraper{opts: opts, log: opts.Log, last: map[string]time.Time{}}, nil
}

// Run polls until the context ends.
func (s *Scraper) Run(ctx context.Context) {
	t := time.NewTicker(s.opts.Interval)
	defer t.Stop()
	s.ScrapeOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.ScrapeOnce(ctx)
		}
	}
}

// ScrapeOnce polls every resolved pod concurrently and stores what it finds.
// A pod that disappears mid-round costs only its own records: the others are
// stored regardless, which is what keeps one dead pod from blinding the fleet.
func (s *Scraper) ScrapeOnce(ctx context.Context) {
	addrs, err := s.opts.Resolve(ctx)
	if err != nil {
		s.log.Warn("relay scrape: pod resolution failed", "err", err)
		return
	}
	if len(addrs) == 0 {
		return
	}
	sort.Strings(addrs)

	now := s.opts.Now()
	date := now.UTC().Format("2006-01-02")

	var wg sync.WaitGroup
	var mu sync.Mutex
	var all []Observation
	answered := 0

	for _, addr := range addrs {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			obs, err := s.scrapePod(ctx, addr, now)
			if err != nil {
				s.log.Debug("relay scrape: pod failed", "addr", addr, "err", err)
				return
			}
			// Counted BEFORE the emptiness check: a pod carrying no broadcasts
			// answered, and its empty answer is exactly the evidence that the
			// broadcasts it used to carry are gone. Treating it as a failure
			// would make the fleet's quietest moment its least trustworthy one.
			mu.Lock()
			answered++
			all = append(all, obs...)
			mu.Unlock()
			if len(obs) == 0 {
				return
			}
			lines := make([][]byte, 0, len(obs))
			for _, o := range obs {
				b, err := json.Marshal(o)
				if err != nil {
					continue
				}
				lines = append(lines, b)
			}
			if err := s.opts.Sink.StoreRelay(date, podName(addr), lines); err != nil {
				s.log.Warn("relay scrape: store failed", "addr", addr, "err", err)
			}
			mu.Lock()
			all = append(all, obs...)
			mu.Unlock()
		}(addr)
	}
	wg.Wait()

	s.noteSeen(all, now)
	if s.opts.Sink != nil {
		s.opts.Sink.ObserveRelay(Round{
			AtMs:         now.UnixMilli(),
			Observations: all,
			Complete:     answered == len(addrs),
			Pods:         len(addrs),
			PodsAnswered: answered,
		})
	}
}

func (s *Scraper) scrapePod(ctx context.Context, addr string, now time.Time) ([]Observation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/statusz", nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.opts.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("statusz returned %d", resp.StatusCode)
	}
	var st Statusz
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&st); err != nil {
		return nil, err
	}

	pod := podName(addr)
	atMs := now.UnixMilli()
	out := make([]Observation, 0, len(st.Broadcasts)*2)
	for key, b := range st.Broadcasts {
		bc := b
		out = append(out, Observation{
			Kind: "broadcast", AtMs: atMs, Pod: pod, Role: b.Role,
			BroadcastKey: key, SessionID: b.PublisherSessionID, Broadcast: &bc,
		})
		for i := range b.SubscriberDetails {
			sub := b.SubscriberDetails[i]
			// Edge sessions are excluded from the per-viewer store entirely:
			// they are fan-out plumbing, never an audience member, and they
			// are never issued a telemetry identity in the first place.
			if sub.Internal {
				continue
			}
			out = append(out, Observation{
				Kind: "subscriber", AtMs: atMs, Pod: pod, Role: b.Role,
				BroadcastKey: key, SessionID: sub.SessionID, Subscriber: &sub,
			})
		}
	}
	return out, nil
}

func (s *Scraper) noteSeen(obs []Observation, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range obs {
		if o.SessionID != "" {
			s.last[o.SessionID] = now
		}
	}
	// Bound the map: a session unseen for an hour is one whose rollup was
	// written long ago.
	cutoff := now.Add(-time.Hour)
	for id, t := range s.last {
		if t.Before(cutoff) {
			delete(s.last, id)
		}
	}
}

// Coverage classifies how much of a session the relay side actually saw
// (docs/33 D5). This is the field that stops a verdict silently resting on a
// relay view that does not exist.
//
//   - "none": the relay never observed this session — it lived and died inside
//     one scrape interval, or telemetry started mid-session.
//   - "partial": observed, but for materially less than the session's duration.
//   - "full": observed across essentially the whole session.
func (s *Scraper) Coverage(sessionID string, sessionStart, sessionEnd time.Time) string {
	s.mu.Lock()
	_, seen := s.last[sessionID]
	s.mu.Unlock()
	if !seen {
		return "none"
	}
	return CoverageFor(seen, sessionStart, sessionEnd, s.opts.Interval)
}

// CoverageFor is the pure classifier, so the rule is testable without a live
// scraper and reusable by the join.
func CoverageFor(seen bool, start, end time.Time, interval time.Duration) string {
	if !seen {
		return "none"
	}
	d := end.Sub(start)
	// A session shorter than two intervals cannot have been sampled enough to
	// claim full coverage, even if one scrape happened to catch it.
	if d < 2*interval {
		return "partial"
	}
	return "full"
}

// podName reduces an address to a stable file name. The scrape target is a
// headless Service's A records, so this is a pod IP; ':' and '.' are not
// filename-safe across the board, so both become '-'.
func podName(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, ""
	}
	out := make([]byte, 0, len(host)+len(port)+1)
	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		default:
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "unknown"
	}
	return string(out)
}

// DNSResolver resolves a headless Service name to its member addresses each
// call, so pods appearing and disappearing during a rollout are followed
// without a restart.
func DNSResolver(host string, port int) Resolver {
	return func(ctx context.Context) ([]string, error) {
		var r net.Resolver
		ips, err := r.LookupHost(ctx, host)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(ips))
		for _, ip := range ips {
			out = append(out, net.JoinHostPort(ip, fmt.Sprint(port)))
		}
		return out, nil
	}
}

// StaticResolver scrapes a fixed address list — single-pod installs, local
// development, and tests.
func StaticResolver(addrs []string) Resolver {
	return func(context.Context) ([]string, error) {
		return append([]string(nil), addrs...), nil
	}
}
