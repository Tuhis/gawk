package transport

// R33 WG2 (docs/83 D3, §8 row WG2): the mint-only publish,
// CONNECT /publish?mint=1. A mint announces ID + token, creates no broadcast,
// emits no event, isn't counted in max-broadcasts, and passes every publish
// gate (secret, rate limit, IP ban, draining, CheckPublishNew). A reclaim
// with the minted token creates the broadcast.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/eventbus"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// eventLog records every event the hub would put on the bus.
type eventLog struct {
	mu  sync.Mutex
	evs []eventbus.Event
}

func (l *eventLog) hook(ev eventbus.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.evs = append(l.evs, ev)
}

func (l *eventLog) all() []eventbus.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]eventbus.Event(nil), l.evs...)
}

func (l *eventLog) count(typ string) int {
	n := 0
	for _, ev := range l.all() {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// busServer is a relay whose hub feeds an eventLog — the event bus's only
// source — and whose metrics the test can read.
type busServer struct {
	port int
	tls  *tls.Config
	reg  *hub.Registry
	srv  *Server
	sm   *metrics.ServerMetrics
	bus  *eventLog
}

func startBusServer(t *testing.T, ctx context.Context, cfg config.Config) busServer {
	t.Helper()
	cert, err := tlsutil.GenerateDevCert([]string{"localhost", "127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatalf("GenerateDevCert: %v", err)
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()

	cfg.Addr = fmt.Sprintf("127.0.0.1:%d", port)
	if cfg.MaxSubscribers == 0 {
		cfg.MaxSubscribers = 15
	}
	if cfg.MaxIdleTimeout == 0 {
		cfg.MaxIdleTimeout = 30 * time.Second
	}
	if cfg.BroadcastGrace == 0 {
		cfg.BroadcastGrace = 5 * time.Minute
	}
	bus := &eventLog{}
	reg := hub.NewRegistry(discardLog, hub.Options{
		MaxSubscribers: cfg.MaxSubscribers,
		BroadcastGrace: cfg.BroadcastGrace,
		MaxBroadcasts:  cfg.MaxBroadcasts,
		OnEvent:        bus.hook,
	})
	sm := metrics.NewServerMetrics(prometheus.NewRegistry())
	srv := New(cfg, reg, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }, discardLog, sm)
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	return busServer{
		port: port,
		tls:  &tls.Config{RootCAs: pool, ServerName: "localhost", NextProtos: []string{http3.NextProtoH3}},
		reg:  reg, srv: srv, sm: sm, bus: bus,
	}
}

// mintOnlyResult is everything a mint-only session delivered, in arrival
// order.
type mintOnlyResult struct {
	id, tokenHex string
	// order lists the wire types of the uni streams as they were read.
	order     []byte
	notice    uint32
	closeCode webtransport.SessionErrorCode
}

// readMintOnly reads every server uni stream of a mint-only session until it
// closes.
func readMintOnly(t *testing.T, ctx context.Context, sess *webtransport.Session) mintOnlyResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var res mintOnlyResult
	for {
		str, err := sess.AcceptUniStream(ctx)
		if err != nil {
			var se *webtransport.SessionError
			if !errors.As(err, &se) {
				t.Fatalf("mint-only session ended with %v, want a WebTransport session close", err)
			}
			res.closeCode = se.ErrorCode
			return res
		}
		_ = str.SetReadDeadline(time.Now().Add(2 * time.Second))
		msg, err := io.ReadAll(io.LimitReader(str, 1<<16))
		if err != nil || len(msg) < 2 {
			t.Fatalf("read uni stream: %v (% x)", err, msg)
		}
		_, typ, err := wire.PeekType(msg)
		if err != nil {
			t.Fatalf("PeekType: %v", err)
		}
		res.order = append(res.order, typ)
		switch typ {
		case wire.TypeBroadcastAnnounce:
			if res.id, err = wire.ParseBroadcastAnnounce(msg); err != nil {
				t.Fatalf("ParseBroadcastAnnounce: %v", err)
			}
		case wire.TypeResumeToken:
			tok, err := wire.ParseResumeToken(msg)
			if err != nil {
				t.Fatalf("ParseResumeToken: %v", err)
			}
			res.tokenHex = hex.EncodeToString(tok)
		case wire.TypeSessionClosing:
			if res.notice, err = wire.ParseSessionClosing(msg); err != nil {
				t.Fatalf("ParseSessionClosing: %v", err)
			}
		}
	}
}

func mintOnlyURL(port int, query string) string {
	u := fmt.Sprintf("https://127.0.0.1:%d/publish?mint=1", port)
	if query != "" {
		u += "&" + query
	}
	return u
}

// The WG2 core: ID + token announced, then the 4017 notice and close — and
// nothing else happened anywhere: no broadcast in /statusz's or the admin
// list's source, no event, no publisher bookkeeping, no broadcast-started
// metric, the "minted" outcome rather than "accepted".
func TestMintOnlyAnnouncesIDAndTokenThenCloses4017(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bs := startBusServer(t, ctx, config.Config{})

	res := readMintOnly(t, ctx, dial(t, ctx, mintOnlyURL(bs.port, ""), bs.tls))

	if res.id == "" || res.tokenHex == "" {
		t.Fatalf("mint-only delivered id %q token %q, want both", res.id, res.tokenHex)
	}
	if !bs.srv.resume.verify(res.id, res.tokenHex) {
		t.Error("the minted token does not verify for the minted ID")
	}
	if res.notice != wire.CloseCodePublishMinted {
		t.Errorf("SessionClosing = %d, want %d", res.notice, wire.CloseCodePublishMinted)
	}
	if res.closeCode != webtransport.SessionErrorCode(wire.CloseCodePublishMinted) {
		t.Errorf("close code = %d, want %d", res.closeCode, wire.CloseCodePublishMinted)
	}
	// Exactly the announce, the token and the notice, and all three before
	// the close. The relay writes them in that order, but webtransport-go
	// does not accept uni streams in open order (docs/gotchas.md), so a
	// client — and this test — dispatches by type and must read every
	// stream rather than stop at the notice.
	types := map[byte]int{}
	for _, typ := range res.order {
		types[typ]++
	}
	if len(res.order) != 3 || types[wire.TypeBroadcastAnnounce] != 1 || types[wire.TypeResumeToken] != 1 ||
		types[wire.TypeSessionClosing] != 1 {
		t.Errorf("uni streams = % x, want one announce, one token and one SessionClosing", res.order)
	}

	if st := bs.reg.Stats(); len(st.Broadcasts) != 0 {
		t.Errorf("/statusz source lists %d broadcast(s) after a mint-only", len(st.Broadcasts))
	}
	if rows := bs.reg.AdminStats(); len(rows) != 0 {
		t.Errorf("admin broadcast list has %d row(s) after a mint-only", len(rows))
	}
	if _, ok := bs.srv.PublisherRemote(res.id); ok {
		t.Error("a mint-only session was tracked as a publisher")
	}
	if evs := bs.bus.all(); len(evs) != 0 {
		t.Errorf("a mint-only emitted %d event(s): %+v", len(evs), evs)
	}
	if got := bs.sm.ConnectionCount("publish", metrics.OutcomeMinted); got != 1 {
		t.Errorf("publish/minted = %v, want 1", got)
	}
	if got := bs.sm.ConnectionCount("publish", metrics.OutcomeAccepted); got != 0 {
		t.Errorf("publish/accepted = %v, want 0 (no broadcast started)", got)
	}
	if got := bs.sm.BroadcastsStartedCount(metrics.BroadcastNew, "unknown", "unknown", "unknown"); got != 0 {
		t.Errorf("broadcasts started = %v, want 0", got)
	}
}

// Not counted against max-broadcasts: at -max-broadcasts 1, mint-only keys
// keep coming, and the one slot is still free for a real broadcast after.
func TestMintOnlyIsNotCountedAgainstMaxBroadcasts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bs := startBusServer(t, ctx, config.Config{MaxBroadcasts: 1})

	seen := map[string]bool{}
	for range 3 {
		res := readMintOnly(t, ctx, dial(t, ctx, mintOnlyURL(bs.port, ""), bs.tls))
		if res.closeCode != webtransport.SessionErrorCode(wire.CloseCodePublishMinted) {
			t.Fatalf("mint-only closed %d, want 4017", res.closeCode)
		}
		seen[res.id] = true
	}
	if len(seen) != 3 {
		t.Errorf("three mints drew %d distinct IDs", len(seen))
	}
	pub, _ := dialPublisherAndGetID(t, ctx, bs.port, bs.tls)
	defer pub.CloseWithError(0, "")
}

// The key works: a reclaim with the minted pair creates the broadcast —
// and only that fires broadcast.started.
func TestMintOnlyKeyReclaimCreatesTheBroadcast(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bs := startBusServer(t, ctx, config.Config{})

	res := readMintOnly(t, ctx, dial(t, ctx, mintOnlyURL(bs.port, ""), bs.tls))
	if bs.bus.count(events.TypeBroadcastStarted) != 0 {
		t.Fatal("broadcast.started before the reclaim")
	}
	pub := dialPublisherReclaim(t, ctx, bs.port, res.id, res.tokenHex, bs.tls)
	defer pub.CloseWithError(0, "")

	waitFor(t, 5*time.Second, func() bool {
		st, ok := bs.reg.Stats().Broadcasts[bs.reg.ObfuscateID(res.id)]
		return ok && st.PublisherActive
	}, "the reclaim created the minted broadcast")
	waitFor(t, 5*time.Second, func() bool { return bs.bus.count(events.TypeBroadcastStarted) == 1 },
		"broadcast.started on the reclaim")
}

// Every publish gate binds a mint-only exactly as it binds a mint. All of
// them answer pre-upgrade, so plain httptest requests drive them.
func TestMintOnlyPassesEveryPublishGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     config.Config
		opts    hub.Options
		setup   func(t *testing.T, srv *Server, r *hub.Registry)
		target  string
		want    int
		outcome string
	}{
		{
			name: "draining", target: "https://relay/publish?mint=1",
			setup: func(_ *testing.T, srv *Server, _ *hub.Registry) { srv.draining.Store(true) },
			want:  http.StatusServiceUnavailable, outcome: metrics.OutcomeDraining,
		},
		{
			name: "secret missing", cfg: config.Config{PublishSecret: "hunter2"},
			target: "https://relay/publish?mint=1",
			want:   http.StatusUnauthorized, outcome: metrics.OutcomeUnauthorized,
		},
		{
			name: "secret wrong", cfg: config.Config{PublishSecret: "hunter2"},
			target: "https://relay/publish?mint=1&secret=nope",
			want:   http.StatusUnauthorized, outcome: metrics.OutcomeUnauthorized,
		},
		{
			name: "IP ban", target: "https://relay/publish?mint=1",
			setup: func(t *testing.T, srv *Server, _ *hub.Registry) {
				srv.SetModeration(banSet(t, ipBan("203.0.113.0/24", "abusive host")))
			},
			want: http.StatusUnavailableForLegalReasons, outcome: metrics.OutcomeBanned,
		},
		{
			// D3: it passes CheckPublishNew, so a full relay refuses it.
			name: "at max-broadcasts", opts: hub.Options{MaxBroadcasts: 1},
			target: "https://relay/publish?mint=1",
			setup: func(t *testing.T, _ *Server, r *hub.Registry) {
				_, pub, err := r.StartPublish("")
				if err != nil {
					t.Fatalf("StartPublish: %v", err)
				}
				t.Cleanup(pub.Close)
			},
			want: http.StatusTooManyRequests, outcome: metrics.OutcomeLimitRejected,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, sm, r := newOutcomeServer(t, tc.cfg, tc.opts)
			if tc.setup != nil {
				tc.setup(t, srv, r)
			}
			w := httptest.NewRecorder()
			srv.handlePublish(w, connectReq(tc.target, nil))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
			if got := sm.ConnectionCount("publish", tc.outcome); got != 1 {
				t.Errorf("publish/%s = %v, want 1", tc.outcome, got)
			}
			if len(r.Stats().Broadcasts) > 1 {
				t.Error("a refused mint-only created a broadcast")
			}
		})
	}
}

// The rate limit binds it too: the burst spent, the next mint-only is a 429
// counted in gawk_rate_limited_total.
func TestMintOnlyIsRateLimited(t *testing.T) {
	srv, sm, _ := newOutcomeServer(t, config.Config{ConnRateLimit: 0.001, ConnBurstLimit: 1}, hub.Options{})
	w := httptest.NewRecorder()
	srv.handlePublish(w, connectReq("https://relay/publish?mint=1", nil)) // the burst (dies at the upgrade)
	w = httptest.NewRecorder()
	srv.handlePublish(w, connectReq("https://relay/publish?mint=1", nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if got := sm.RateLimitedCount(); got != 1 {
		t.Errorf("rate_limited = %v, want 1", got)
	}
}

// A secret-gated relay mints for the right secret, end to end.
func TestMintOnlyWithTheRightSecret(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bs := startBusServer(t, ctx, config.Config{PublishSecret: "hunter2"})
	res := readMintOnly(t, ctx, dial(t, ctx, mintOnlyURL(bs.port, "secret=hunter2"), bs.tls))
	if res.id == "" || res.closeCode != webtransport.SessionErrorCode(wire.CloseCodePublishMinted) {
		t.Fatalf("mint-only with the secret: id %q close %d", res.id, res.closeCode)
	}
}

// ?mint on a claim is malformed (400), never quietly a publish; and only
// mint=1 means mint-only.
func TestMintParamOutsideAMintIsABadRequest(t *testing.T) {
	srv, sm, r := newOutcomeServer(t, config.Config{}, hub.Options{})
	token := hex.EncodeToString(srv.resume.mint(bannedTestID))
	for _, tc := range []struct {
		name   string
		target string
		values map[string]string
	}{
		{"claim with mint=1", "https://relay/publish/" + bannedTestID + "?mint=1&resume=" + token, map[string]string{"id": bannedTestID}},
		{"mint=0", "https://relay/publish?mint=0", nil},
		{"mint without a value", "https://relay/publish?mint", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := sm.ConnectionCount("publish", metrics.OutcomeBadRequest)
			w := httptest.NewRecorder()
			srv.handlePublish(w, connectReq(tc.target, tc.values))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
			if got := sm.ConnectionCount("publish", metrics.OutcomeBadRequest) - before; got != 1 {
				t.Errorf("publish/bad_request grew by %v, want 1", got)
			}
		})
	}
	if len(r.Stats().Broadcasts) != 0 {
		t.Error("a malformed mint request created a broadcast")
	}
}

// scriptedDraw hands out ids in order, then fails.
func scriptedDraw(ids ...string) func() (string, error) {
	return func() (string, error) {
		if len(ids) == 0 {
			return "", errors.New("script exhausted")
		}
		id := ids[0]
		ids = ids[1:]
		return id, nil
	}
}

// leasedCoordinator answers Resolve with a Lease for the IDs in taken.
type leasedCoordinator struct {
	fakeCoordinator
	taken map[string]bool
}

func (c *leasedCoordinator) Resolve(_ context.Context, id string) (cluster.Origin, error) {
	if c.taken[id] {
		return cluster.Origin{Holder: "relay-1", Addr: "10.0.0.2:4433"}, nil
	}
	return cluster.Origin{}, cluster.ErrNotFound
}

// The draw's collision rules beyond the registry's own: an ID under an ID
// ban (its key could never publish) and, in cluster mode, an ID with an
// origin Lease anywhere in the fleet (a live or graced broadcast on another
// pod, whose owner the minted token would supersede) are redrawn.
func TestMintOnlyDrawSkipsBannedAndLeasedIDs(t *testing.T) {
	const leased, free = "K7XQ2M", "XYZ234"
	srv := New(config.Config{Addr: "127.0.0.1:0"}, hub.NewRegistry(discardLog, hub.Options{}), nil,
		slog.New(slog.DiscardHandler), nil)
	srv.SetModeration(banSet(t, idBan(bannedTestID, "")))

	id, err := srv.drawMintOnlyID(context.Background(), scriptedDraw(bannedTestID, free))
	if err != nil || id != free {
		t.Fatalf("draw past a banned ID = %q, %v; want %q", id, err, free)
	}

	srv.SetCluster(&leasedCoordinator{taken: map[string]bool{leased: true}}, "relay-0")
	id, err = srv.drawMintOnlyID(context.Background(), scriptedDraw(leased, bannedTestID, free))
	if err != nil || id != free {
		t.Fatalf("draw past a leased and a banned ID = %q, %v; want %q", id, err, free)
	}

	// Bounded: a fleet where every draw is taken gives up rather than loop.
	many := make([]string, mintRedraws+2)
	for i := range many {
		many[i] = leased
	}
	if id, err := srv.drawMintOnlyID(context.Background(), scriptedDraw(many...)); err == nil {
		t.Fatalf("drew %q with every draw leased", id)
	}
}

func TestNoticedCloseCodePublishMinted(t *testing.T) {
	if !noticedCloseCode(wire.CloseCodePublishMinted) {
		t.Error("4017 PublishMinted is not noticed — a browser would read a bare drop and retry the mint")
	}
}
