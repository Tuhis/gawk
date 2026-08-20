// Package relayscan enumerates the relay fleet for the admin portal (R39,
// docs/42 D12, §4.5).
//
// It resolves the relay's headless metrics Service to pod IPs and scrapes each
// pod's credential-gated /internal/admin/broadcasts and /internal/admin/config.
// This is the same discovery shape gawk-telemetry's relayscrape already
// proves in production (an injectable resolver plus an injectable HTTP client,
// so tests need neither DNS nor a network), and it was chosen over listing
// broadcast Leases because Leases exist only in cluster mode and carry no
// stats.
//
// Three properties are load-bearing:
//
//   - **One dead pod degrades itself, never the aggregate.** A pod that times
//     out becomes `reachable: false` in the relays view and contributes no
//     broadcasts; every other pod's data is returned unchanged. The portal's
//     job is to let an operator act during trouble, which is exactly when a
//     pod is likely to be unreachable.
//   - **Results are cached for at most 2 s.** The broadcasts view auto-refreshes
//     every 5 s and several handlers consult the same snapshot within one
//     request; without a cache, one operator with a browser open would put a
//     steady scrape load on every relay pod's ops listener.
//   - **Raw broadcast IDs live here.** These endpoints are the scoped
//     relaxation of the raw-ID invariant (D8) — ClusterIP-only and
//     credential-gated. Nothing in this package logs an ID or an IP above
//     Debug, and nothing hands either to a webhook.
package relayscan

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Response schema identifiers from docs/42 §4.5. They are checked rather than
// assumed: a relay from a future release that renames the shape should degrade
// to "unreachable-ish" rather than be silently misread.
const (
	SchemaBroadcasts = "gawk.admin.broadcasts.v1"
	SchemaConfig     = "gawk.admin.config.v1"
)

// DefaultCacheTTL is the aggregate cache window (§4.7: "≤2 s cache").
const DefaultCacheTTL = 2 * time.Second

// DefaultTimeout bounds one pod's scrape. Deliberately short: a pod that
// cannot answer promptly is a pod whose numbers would be stale anyway, and a
// hung scrape must never hold an operator's kill dialog open.
const DefaultTimeout = 3 * time.Second

// Broadcast is one broadcast as one pod sees it — the `broadcasts[]` element
// of gawk.admin.broadcasts.v1.
type Broadcast struct {
	// ID is the RAW broadcast ID (D8: permitted on this credential-gated
	// surface, the portal, and Postgres — nowhere else).
	ID string `json:"id"`
	// Key is Registry.ObfuscateID(ID): the HMAC'd form, safe to export, and
	// what the telemetry UI keys its broadcast view by.
	Key                   string `json:"key"`
	Role                  string `json:"role"`
	PublisherActive       bool   `json:"publisherActive"`
	PublisherRemoteIP     string `json:"publisherRemoteIp"`
	PublisherSessionID    string `json:"publisherSessionId"`
	StartedAt             string `json:"startedAt"`
	ViewersLocal          int    `json:"viewersLocal"`
	ViewersGlobal         int    `json:"viewersGlobal"`
	GraceRemainingSeconds int    `json:"graceRemainingSeconds"`
	DVRBytes              int64  `json:"dvrBytes"`
}

// BroadcastsResponse is GET /internal/admin/broadcasts.
type BroadcastsResponse struct {
	Schema     string      `json:"schema"`
	Pod        string      `json:"pod"`
	Broadcasts []Broadcast `json:"broadcasts"`
}

// ConfigResponse is GET /internal/admin/config. Config is decoded
// structurally: the relay's sanitized config is a map of knob names to values
// and the portal renders it as a table, so knowing the field set would buy
// nothing and would break on every new relay flag.
type ConfigResponse struct {
	Schema  string         `json:"schema"`
	Pod     string         `json:"pod"`
	Version string         `json:"version"`
	Config  map[string]any `json:"config"`
}

// Pod is one relay pod's scrape result.
type Pod struct {
	// Name is what the pod reported; the scrape address when it reported
	// nothing (an unreachable pod still needs a stable row in the UI).
	Name string
	Addr string
	// Reachable reflects the broadcasts endpoint: the one that decides whether
	// this pod's broadcasts are known.
	Reachable bool
	// Err is why the broadcasts scrape failed, for the relays view.
	Err string
	// Version and Config come from the config endpoint, which is scraped
	// independently — a config hiccup must not hide live broadcasts.
	Version    string
	Config     map[string]any
	ConfigErr  string
	Broadcasts []Broadcast
}

// Placement is one pod's view of a broadcast in the aggregate.
type Placement struct {
	Pod          string
	Role         string
	ViewersLocal int
}

// Aggregate is one broadcast merged across every pod carrying it.
type Aggregate struct {
	ID                string
	Key               string
	PublisherActive   bool
	PublisherRemoteIP string
	StartedAt         string
	ViewersGlobal     int
	Pods              []Placement
}

// Snapshot is one whole-fleet view.
type Snapshot struct {
	At   time.Time
	Pods []Pod
	// Broadcasts are sorted by ID so the portal's table does not reshuffle
	// between refreshes.
	Broadcasts []Aggregate
	// PodsResolved / PodsAnswered make partial coverage visible rather than
	// letting an empty answer look like an empty fleet.
	PodsResolved int
	PodsAnswered int
}

// Broadcast finds one aggregate by raw ID.
func (s Snapshot) Broadcast(id string) (Aggregate, bool) {
	for _, b := range s.Broadcasts {
		if strings.EqualFold(b.ID, id) {
			return b, true
		}
	}
	return Aggregate{}, false
}

// Resolver returns the current pod addresses to scrape ("10.42.0.7:2112").
type Resolver func(ctx context.Context) ([]string, error)

// Options configure a Scanner.
type Options struct {
	// Resolve enumerates relay pods. Required.
	Resolve Resolver
	// Token is the relay's -admin-api-token, sent as a bearer credential.
	// Without it the relay answers 404 (the surface stays dark, §4.3).
	Token    string
	Client   *http.Client
	Log      *slog.Logger
	Now      func() time.Time
	CacheTTL time.Duration
}

// Scanner scrapes the fleet on demand, behind a short cache.
type Scanner struct {
	opts Options

	mu     sync.Mutex
	cached *Snapshot
	// inflight collapses concurrent misses into one fleet scrape: several
	// handlers in one request cycle must not each fan out to every pod.
	inflight *scanCall
}

type scanCall struct {
	done chan struct{}
	snap Snapshot
	err  error
}

// New builds a Scanner.
func New(opts Options) (*Scanner, error) {
	if opts.Resolve == nil {
		return nil, fmt.Errorf("relayscan: Options.Resolve is required")
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = DefaultCacheTTL
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: DefaultTimeout}
	}
	return &Scanner{opts: opts}, nil
}

// Snapshot returns the fleet view, scraping only when the cached one has aged
// past the TTL.
func (s *Scanner) Snapshot(ctx context.Context) (Snapshot, error) {
	now := s.opts.Now()

	s.mu.Lock()
	if s.cached != nil && now.Sub(s.cached.At) < s.opts.CacheTTL {
		snap := *s.cached
		s.mu.Unlock()
		return snap, nil
	}
	if call := s.inflight; call != nil {
		s.mu.Unlock()
		select {
		case <-call.done:
			return call.snap, call.err
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		}
	}
	call := &scanCall{done: make(chan struct{})}
	s.inflight = call
	s.mu.Unlock()

	call.snap, call.err = s.scan(ctx, now)

	s.mu.Lock()
	if call.err == nil {
		snap := call.snap
		s.cached = &snap
	}
	s.inflight = nil
	s.mu.Unlock()
	close(call.done)
	return call.snap, call.err
}

// Invalidate drops the cache. Called after a mutation so the operator's next
// refresh shows the effect of what they just did rather than a stale row.
func (s *Scanner) Invalidate() {
	s.mu.Lock()
	s.cached = nil
	s.mu.Unlock()
}

// scan fans out to every resolved pod. Only a resolution failure fails the
// whole call: a pod-level failure is data, not an error.
func (s *Scanner) scan(ctx context.Context, now time.Time) (Snapshot, error) {
	addrs, err := s.opts.Resolve(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("relayscan: resolve relay pods: %w", err)
	}
	sort.Strings(addrs)

	pods := make([]Pod, len(addrs))
	var wg sync.WaitGroup
	for i, addr := range addrs {
		wg.Add(1)
		go func(i int, addr string) {
			defer wg.Done()
			pods[i] = s.scrapePod(ctx, addr)
		}(i, addr)
	}
	wg.Wait()

	snap := Snapshot{At: now, Pods: pods, PodsResolved: len(addrs)}
	for _, p := range pods {
		if p.Reachable {
			snap.PodsAnswered++
		}
	}
	snap.Broadcasts = aggregate(pods)
	return snap, nil
}

func (s *Scanner) scrapePod(ctx context.Context, addr string) Pod {
	p := Pod{Name: hostOf(addr), Addr: addr}

	var (
		wg    sync.WaitGroup
		bcast BroadcastsResponse
		bErr  error
		cfg   ConfigResponse
		cErr  error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		bErr = s.get(ctx, addr, "/internal/admin/broadcasts", &bcast)
		if bErr == nil && bcast.Schema != SchemaBroadcasts {
			bErr = fmt.Errorf("unexpected schema %q (want %q)", bcast.Schema, SchemaBroadcasts)
		}
	}()
	go func() {
		defer wg.Done()
		cErr = s.get(ctx, addr, "/internal/admin/config", &cfg)
		if cErr == nil && cfg.Schema != SchemaConfig {
			cErr = fmt.Errorf("unexpected schema %q (want %q)", cfg.Schema, SchemaConfig)
		}
	}()
	wg.Wait()

	if bErr != nil {
		// Debug, not Warn: the message can carry a pod address, and a rolling
		// relay deployment would otherwise spam Warn once per scrape.
		s.opts.Log.Debug("relayscan: pod broadcasts scrape failed", "addr", addr, "err", bErr)
		p.Err = bErr.Error()
	} else {
		p.Reachable = true
		p.Broadcasts = bcast.Broadcasts
		if bcast.Pod != "" {
			p.Name = bcast.Pod
		}
	}
	if cErr != nil {
		s.opts.Log.Debug("relayscan: pod config scrape failed", "addr", addr, "err", cErr)
		p.ConfigErr = cErr.Error()
	} else {
		p.Version = cfg.Version
		p.Config = cfg.Config
		if cfg.Pod != "" && !p.Reachable {
			p.Name = cfg.Pod
		}
	}
	return p
}

func (s *Scanner) get(ctx context.Context, addr, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return err
	}
	if s.opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.opts.Token)
	}
	resp, err := s.opts.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		// 404 means the relay has no credential configured (§4.3) — worth
		// saying plainly, because it is the most likely misconfiguration.
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%s returned 404: the relay has no -admin-api-token configured", path)
		}
		return fmt.Errorf("%s returned %d", path, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

// aggregate merges each pod's view of a broadcast into one row.
//
// The origin pod wins for every whole-broadcast fact (publisher liveness, the
// publisher's address, start time, the global viewer count) because it is the
// only pod that HAS them; an edge pod knows its own local subscribers and
// little else. Where no origin answered — a pod mid-restart — the first edge
// view stands in, so the broadcast is still visible and still killable.
func aggregate(pods []Pod) []Aggregate {
	byID := map[string]*Aggregate{}
	origin := map[string]bool{}
	for _, p := range pods {
		if !p.Reachable {
			continue
		}
		for _, b := range p.Broadcasts {
			agg, ok := byID[b.ID]
			if !ok {
				agg = &Aggregate{ID: b.ID, Key: b.Key}
				byID[b.ID] = agg
			}
			isOrigin := b.Role == "origin"
			if isOrigin || !origin[b.ID] {
				agg.Key = b.Key
				agg.PublisherActive = b.PublisherActive
				agg.PublisherRemoteIP = b.PublisherRemoteIP
				agg.StartedAt = b.StartedAt
				agg.ViewersGlobal = b.ViewersGlobal
			}
			if isOrigin {
				origin[b.ID] = true
			}
			agg.Pods = append(agg.Pods, Placement{Pod: p.Name, Role: b.Role, ViewersLocal: b.ViewersLocal})
		}
	}
	out := make([]Aggregate, 0, len(byID))
	for _, agg := range byID {
		sort.Slice(agg.Pods, func(i, j int) bool { return agg.Pods[i].Pod < agg.Pods[j].Pod })
		out = append(out, *agg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func hostOf(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// DNSResolver resolves the headless Service's A records to pod addresses on
// every call, so pods appearing and disappearing during a rollout are followed
// without a restart (the relayscrape pattern, docs/42 D12).
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

// StaticResolver scrapes a fixed list — single-pod installs, local
// development, and tests.
func StaticResolver(addrs ...string) Resolver {
	return func(context.Context) ([]string, error) {
		return append([]string(nil), addrs...), nil
	}
}
