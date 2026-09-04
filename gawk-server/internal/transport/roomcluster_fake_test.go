package transport

// R42 RM3 cluster gate, handler-level, against a fake RoomCluster (docs/44
// §4.5): every routing decision roomHomedElsewhere can make — serve locally
// (held, unknown, un-normalizable, adopted), proxy (live foreign home, or a
// lost adoption race whose winner is now live), and the adoption failures
// (race lost with no live winner → 503, no CR → 404, API error → 503) —
// plus proxyRoom's own pre-upgrade answers: the home's rejection status
// forwarded verbatim, an unreachable home as 503, and the participant's
// upgrade failing after the upstream dialed (the upstream must be closed,
// not leaked). The fleet tests in roomcluster_test.go cover the piped
// session; these cover the decisions that never reach one.
//
// Also here: the proxy-aware stats source (a "proxy" row per forwarded
// room, never shadowing a "home" row) and the metrics collector reading it,
// the drain hook chaining onto ReleaseAll, and the lease-loss dispatch
// ignoring a code that does not normalize.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/roomcluster"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
)

// fakeRoomCluster is a scripted RoomCluster: what this pod holds, what the
// "informer cache" resolves, and what Adopt answers.
type fakeRoomCluster struct {
	mu       sync.Mutex
	held     map[string]int64
	homes    map[string]roomcluster.Home
	adoptErr error
	// onAdopt runs on a successful Adopt (nil adoptErr) — the seam for
	// "the winner is live now" and "adoption populated the registry".
	onAdopt  func(code string)
	adopts   []string
	released int
}

func (f *fakeRoomCluster) Holding(code string) (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	gen, ok := f.held[code]
	return gen, ok
}

func (f *fakeRoomCluster) Resolve(code string) (roomcluster.Home, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.homes[code]
	return h, ok
}

func (f *fakeRoomCluster) Adopt(_ context.Context, code string) error {
	f.mu.Lock()
	f.adopts = append(f.adopts, code)
	err := f.adoptErr
	hook := f.onAdopt
	f.mu.Unlock()
	if hook != nil {
		hook(code)
	}
	return err
}

func (f *fakeRoomCluster) ReleaseAll(context.Context) {
	f.mu.Lock()
	f.released++
	f.mu.Unlock()
}

func (f *fakeRoomCluster) setHome(code string, h roomcluster.Home) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.homes == nil {
		f.homes = map[string]roomcluster.Home{}
	}
	f.homes[code] = h
}

// dialCall is one roomProxyDialer invocation.
type dialCall struct {
	addr string
	path string
}

// scriptedDialer records calls and answers with a fixed result.
type scriptedDialer struct {
	mu     sync.Mutex
	calls  []dialCall
	status int
	up     *roomProxyUpstream
	err    error
}

func (d *scriptedDialer) dial(_ context.Context, addr, path string) (int, *roomProxyUpstream, error) {
	d.mu.Lock()
	d.calls = append(d.calls, dialCall{addr, path})
	d.mu.Unlock()
	return d.status, d.up, d.err
}

func (d *scriptedDialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

// newFakeClusterServer is newRoomOutcomeServer with the fake store and a
// scripted dialer installed in place of the real wiring.
func newFakeClusterServer(t *testing.T, fc *fakeRoomCluster, d *scriptedDialer) (*Server, *metrics.ServerMetrics, *roomsrv.Registry) {
	t.Helper()
	srv, sm, _, reg := newRoomOutcomeServer(t, config.Config{ClusterMode: true, InternalPSK: "fleet-psk", InternalServerName: "localhost"}, true, nil)
	srv.SetRoomCluster(fc, "pod-a")
	srv.roomClusterWiring().dial = d.dial
	return srv, sm, reg
}

var liveHomeB = roomcluster.Home{Kind: "dynamic", Holder: "pod-b", Addr: "10.0.0.2:4433", Generation: 3, Live: true}

// Every "serve locally" verdict: no dial happens and the request reaches
// the local join gate (a 404 from CheckJoin, since the registry is empty),
// and every "answer myself" verdict of a failed adoption.
func TestRoomHomedElsewhereVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		code       string
		setup      func(*fakeRoomCluster)
		wantStatus int
		wantDials  int
		wantAdopt  string // route/outcome counted, "" for none
	}{
		{"held here: local", "k7xq2m", func(f *fakeRoomCluster) {
			f.held = map[string]int64{"k7xq2m": 1}
			f.setHome("k7xq2m", liveHomeB) // the cache lags; holding wins
		}, http.StatusNotFound, 0, ""},
		{"no CR: local", "k7xq2m", func(*fakeRoomCluster) {}, http.StatusNotFound, 0, ""},
		{"un-normalizable code: local", "bad_code", func(f *fakeRoomCluster) {
			f.setHome("bad_code", liveHomeB)
		}, http.StatusNotFound, 0, ""},
		{"released lease: adopted, local", "k7xq2m", func(f *fakeRoomCluster) {
			f.setHome("k7xq2m", roomcluster.Home{Kind: "dynamic", Generation: 2})
		}, http.StatusNotFound, 0, "room-adopt/" + metrics.OutcomeAccepted},
		{"adoption race lost, no live winner: 503", "k7xq2m", func(f *fakeRoomCluster) {
			f.setHome("k7xq2m", roomcluster.Home{Kind: "dynamic", Holder: "pod-c", Generation: 2})
			f.adoptErr = roomcluster.ErrHeldElsewhere
		}, http.StatusServiceUnavailable, 0, "room-adopt/" + metrics.OutcomeConflict},
		{"adoption finds no CR: 404", "k7xq2m", func(f *fakeRoomCluster) {
			f.setHome("k7xq2m", roomcluster.Home{Kind: "dynamic"})
			f.adoptErr = roomcluster.ErrNotFound
		}, http.StatusNotFound, 0, "room-adopt/" + metrics.OutcomeNotFound},
		{"adoption API error: 503", "k7xq2m", func(f *fakeRoomCluster) {
			f.setHome("k7xq2m", roomcluster.Home{Kind: "dynamic"})
			f.adoptErr = fmt.Errorf("%w: etcd away", roomsrv.ErrUnavailable)
		}, http.StatusServiceUnavailable, 0, "room-adopt/" + metrics.OutcomeError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeRoomCluster{}
			tc.setup(fc)
			d := &scriptedDialer{status: http.StatusTeapot, err: errors.New("must not dial")}
			srv, sm, _ := newFakeClusterServer(t, fc, d)
			w := httptest.NewRecorder()
			srv.handleRoom(w, roomJoinReq(tc.code, ""))
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if got := d.count(); got != tc.wantDials {
				t.Errorf("dials = %d, want %d", got, tc.wantDials)
			}
			if tc.wantAdopt != "" {
				parts := strings.SplitN(tc.wantAdopt, "/", 2)
				if got := sm.ConnectionCount(parts[0], parts[1]); got != 1 {
					t.Errorf("%s = %v, want 1", tc.wantAdopt, got)
				}
			} else if got := sm.ConnectionCount("room-adopt", metrics.OutcomeAccepted) + sm.ConnectionCount("room-adopt", metrics.OutcomeConflict) +
				sm.ConnectionCount("room-adopt", metrics.OutcomeNotFound) + sm.ConnectionCount("room-adopt", metrics.OutcomeError); got != 0 {
				t.Errorf("room-adopt outcomes counted = %v, want none", got)
			}
		})
	}
}

// A live foreign home is proxied: the dial targets the lease address with
// the participant's own query forwarded plus the lease generation, and
// the home's pre-upgrade verdict comes back to the participant unchanged.
func TestRoomProxyForwardsTheHomesRejection(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusConflict, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			fc := &fakeRoomCluster{}
			fc.setHome("k7xq2m", liveHomeB)
			d := &scriptedDialer{status: status, err: fmt.Errorf("status %d", status)}
			srv, sm, _ := newFakeClusterServer(t, fc, d)
			w := httptest.NewRecorder()
			srv.handleRoom(w, roomJoinReq("K7XQ2M", "creator=00&attach=key&psk=stolen&gen=99"))
			if w.Code != status {
				t.Fatalf("status = %d, want the home's %d", w.Code, status)
			}
			if got := sm.ConnectionCount("room-proxy", metrics.OutcomeUnauthorized); got != 1 {
				t.Errorf("room-proxy/unauthorized = %v, want 1", got)
			}
			if len(fc.adopts) != 0 {
				t.Errorf("a live foreign home was adopted: %v", fc.adopts)
			}
			if d.count() != 1 {
				t.Fatalf("dials = %d, want 1", d.count())
			}
			call := d.calls[0]
			if call.addr != liveHomeB.Addr {
				t.Errorf("dialed %q, want the lease addr %q", call.addr, liveHomeB.Addr)
			}
			u, err := url.Parse(call.path)
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			if u.Path != "/internal/room/k7xq2m" || q.Get("gen") != "3" || q.Get("creator") != "00" || q.Get("attach") != "key" {
				t.Errorf("proxy path = %q, want the normalized code, gen 3 and the forwarded params", call.path)
			}
			// A participant cannot smuggle the fleet PSK or a generation
			// of its choosing through the proxy.
			if q.Has("psk") || q.Get("gen") == "99" {
				t.Errorf("proxy path forwarded the client's psk/gen: %q", call.path)
			}
		})
	}
}

// A lost adoption race whose winner is live by the time we re-resolve is
// proxied to that winner rather than answered 503.
func TestRoomProxyAfterLosingTheAdoptionRace(t *testing.T) {
	fc := &fakeRoomCluster{adoptErr: roomcluster.ErrHeldElsewhere}
	fc.setHome("k7xq2m", roomcluster.Home{Kind: "dynamic", Holder: "pod-c", Generation: 2})
	// Adopt reports a live foreign holder: the cache catches up before the
	// re-resolve.
	fc.onAdopt = func(code string) { fc.setHome(code, liveHomeB) }
	d := &scriptedDialer{status: http.StatusTooManyRequests, err: errors.New("full")}
	srv, sm, _ := newFakeClusterServer(t, fc, d)
	w := httptest.NewRecorder()
	srv.handleRoom(w, roomJoinReq("k7xq2m", ""))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the winner's 429", w.Code)
	}
	if d.count() != 1 || d.calls[0].addr != liveHomeB.Addr {
		t.Fatalf("dials = %+v, want one to the winner", d.calls)
	}
	if got := sm.ConnectionCount("room-adopt", metrics.OutcomeConflict); got != 0 {
		t.Errorf("room-adopt/conflict = %v, want 0 (the race loss was resolved by proxying)", got)
	}
}

// An unreachable home (dial error without a status) is 503 under the error
// outcome: the lease is still live by the cache's clock, so the client's
// reconnect — not this request — is what adopts.
func TestRoomProxyUnreachableHomeIs503(t *testing.T) {
	fc := &fakeRoomCluster{}
	fc.setHome("k7xq2m", liveHomeB)
	d := &scriptedDialer{err: errors.New("connection refused")}
	srv, sm, _ := newFakeClusterServer(t, fc, d)
	w := httptest.NewRecorder()
	srv.handleRoom(w, roomJoinReq("k7xq2m", ""))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if got := sm.ConnectionCount("room-proxy", metrics.OutcomeError); got != 1 {
		t.Errorf("room-proxy/error = %v, want 1", got)
	}
	if len(fc.adopts) != 0 {
		t.Errorf("an unreachable but live home was adopted: %v", fc.adopts)
	}
}

// The upstream dialed but the participant's own upgrade failed: the
// upstream session is closed (a leaked internal session would hold a
// phantom participant on the home) and the outcome is upgrade_failed.
func TestRoomProxyClosesTheUpstreamWhenTheUpgradeFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// A real session to stand in for the upstream: any route will do, the
	// point is that close() reaches a live QUIC session.
	port, clientTLS, _, _ := startTestServer(t, ctx, 1)
	tr := &webtransport.Transport{TLSClientConfig: clientTLS, QUICConfig: &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true}}
	_, upSess, err := tr.Dial(ctx, fmt.Sprintf("https://127.0.0.1:%d/echo", port), nil)
	if err != nil {
		t.Fatalf("dial the stand-in upstream: %v", err)
	}
	up := &roomProxyUpstream{sess: upSess, d: tr}

	fc := &fakeRoomCluster{}
	fc.setHome("k7xq2m", liveHomeB)
	d := &scriptedDialer{up: up}
	srv, sm, _ := newFakeClusterServer(t, fc, d)
	w := httptest.NewRecorder()
	srv.handleRoom(w, roomJoinReq("k7xq2m", ""))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if got := sm.ConnectionCount("room-proxy", metrics.OutcomeUpgradeFailed); got != 1 {
		t.Errorf("room-proxy/upgrade_failed = %v, want 1", got)
	}
	select {
	case <-upSess.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream session was not closed after the participant's upgrade failed")
	}
	if got := srv.RoomStats(); len(got) != 0 {
		t.Errorf("a proxy row was left behind: %+v", got)
	}
}

func TestRoomProxyPathStripsPSKAndGen(t *testing.T) {
	got := roomProxyPath("k7xq2m", 7, url.Values{"psk": {"stolen"}, "gen": {"1"}, "creator": {"aa"}, "name": {"n"}})
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Path != "/internal/room/k7xq2m" || q.Has("psk") || q.Get("gen") != "7" || q.Get("creator") != "aa" || q.Get("name") != "n" {
		t.Fatalf("roomProxyPath = %q", got)
	}
}

// SetRoomCluster chains onto whatever drain hook SetCluster installed:
// both run, the room release last.
func TestSetRoomClusterChainsTheDrainHook(t *testing.T) {
	srv, _, _, _ := newRoomOutcomeServer(t, config.Config{ClusterMode: true}, true, nil)
	srv.drainSleep = func(time.Duration) {}
	var order []string
	srv.onDrain = func() { order = append(order, "leases") }
	fc := &fakeRoomCluster{}
	srv.SetRoomCluster(fc, "pod-a")
	srv.drain()
	if len(order) != 1 || order[0] != "leases" {
		t.Fatalf("previous drain hook not run: %v", order)
	}
	if fc.released != 1 {
		t.Fatalf("ReleaseAll ran %d times, want 1", fc.released)
	}
	// A server whose only drain hook is the room release still runs it.
	srv2, _, _, _ := newRoomOutcomeServer(t, config.Config{ClusterMode: true}, true, nil)
	srv2.drainSleep = func(time.Duration) {}
	fc2 := &fakeRoomCluster{}
	srv2.SetRoomCluster(fc2, "pod-a")
	srv2.drain()
	if fc2.released != 1 {
		t.Fatalf("ReleaseAll without a previous hook ran %d times, want 1", fc2.released)
	}
}

// The internal route's remaining pre-upgrade verdicts, handler-level: a
// code that does not normalize is 404 after the PSK check (a fleet pod
// forwarding garbage is still a fleet pod), and a join that clears every
// gate but cannot upgrade is 403 under upgrade_failed, leaving the room
// untouched.
func TestInternalRoomBadCodeAndUpgradeFailure(t *testing.T) {
	fc := &fakeRoomCluster{held: map[string]int64{"tuhisroom": 4}}
	srv, sm, reg := newFakeClusterServer(t, fc, &scriptedDialer{})
	if err := reg.UpsertStatic(roomsrv.StaticRoom{Code: "TuhisRoom"}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	srv.handleInternalRoom(w, roomJoinReq("bad_code", "psk=fleet-psk&gen=4"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("bad code: status = %d, want 404", w.Code)
	}
	if got := sm.ConnectionCount("internal-room", metrics.OutcomeNotFound); got != 1 {
		t.Errorf("internal-room/not_found = %v, want 1", got)
	}
	w = httptest.NewRecorder()
	srv.handleInternalRoom(w, roomJoinReq("TUHISROOM", "psk=fleet-psk&gen=4"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("upgrade failure: status = %d, want 403", w.Code)
	}
	if got := sm.ConnectionCount("internal-room", metrics.OutcomeUpgradeFailed); got != 1 {
		t.Errorf("internal-room/upgrade_failed = %v, want 1", got)
	}
	if info, ok := reg.Lookup("tuhisroom"); !ok || info.Participants != 0 {
		t.Fatalf("room after the failed internal join = %+v, %v", info, ok)
	}
	// Cluster wiring present but the registry never installed: 404.
	bare, bareSM, _ := newOutcomeServer(t, config.Config{Rooms: true, ClusterMode: true, InternalPSK: "fleet-psk"}, hub.Options{})
	bare.SetRoomCluster(fc, "pod-a")
	w = httptest.NewRecorder()
	bare.handleInternalRoom(w, roomJoinReq("tuhisroom", "psk=fleet-psk&gen=4"))
	if w.Code != http.StatusNotFound || bareSM.ConnectionCount("internal-room", metrics.OutcomeNotFound) != 1 {
		t.Fatalf("registry-less internal join: status = %d, want 404 under not_found", w.Code)
	}
	// And the stats source over a registry-less server reports zero totals
	// rather than dereferencing nil.
	if tot := (roomStatsSource{bare}).TotalStats(); tot != (roomsrv.Totals{}) {
		t.Errorf("TotalStats without a registry = %+v, want zero", tot)
	}
}

// A lease-loss dispatch for a code that does not normalize is ignored
// rather than ending anything.
func TestHandleRoomLeaseLostIgnoresBadCodes(t *testing.T) {
	srv, _, _, reg := newRoomOutcomeServer(t, config.Config{}, true, nil)
	if err := reg.UpsertStatic(roomsrv.StaticRoom{Code: "TuhisRoom"}); err != nil {
		t.Fatal(err)
	}
	srv.HandleRoomLeaseLost("bad_code")
	if !reg.Has("tuhisroom") {
		t.Fatal("an unrelated bad code ended a live room")
	}
	srv.HandleRoomLeaseLost("TUHISROOM")
	if reg.Has("tuhisroom") {
		t.Fatal("the lease loss did not drop the local copy")
	}
}

// gaugeValue finds one gauge sample by family name and exact label set;
// -1 when absent (a gauge that should exist is never legitimately -1).
func gaugeValue(mfs []*dto.MetricFamily, name string, labels map[string]string) float64 {
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	metric:
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			if len(got) != len(labels) {
				continue
			}
			for k, v := range labels {
				if got[k] != v {
					continue metric
				}
			}
			return m.GetGauge().GetValue()
		}
	}
	return -1
}

// The stats source merges the registry's "home" rows with one "proxy" row
// per forwarded room, keyed by the same HMAC; a room that is both home
// here and (stale) proxied keeps its home row. The metrics collector reads
// the same source: proxied sessions fold into one gauge, home rooms get
// per-room participant/attachment gauges, and totals are the registry's.
func TestRoomStatsSourceMergesProxyRows(t *testing.T) {
	srv, _, r, reg := newRoomOutcomeServer(t, config.Config{}, true, nil)
	if err := reg.UpsertStatic(roomsrv.StaticRoom{Code: "TuhisRoom"}); err != nil {
		t.Fatal(err)
	}
	src := srv.RoomStatsSource()
	if src == nil {
		t.Fatal("RoomStatsSource is nil with a registry installed")
	}
	if tot := src.TotalStats(); tot.Static != 1 || tot.Dynamic != 0 {
		t.Fatalf("TotalStats = %+v, want one static room", tot)
	}

	release1 := srv.trackProxied("k7xq2m", "dynamic")
	release2 := srv.trackProxied("k7xq2m", "dynamic")
	releaseHome := srv.trackProxied("tuhisroom", "static") // a stale proxy of a room now homed here
	rows := src.Stats()
	proxyKey, homeKey := r.ObfuscateID("k7xq2m"), r.ObfuscateID("tuhisroom")
	if row := rows[proxyKey]; row.Role != "proxy" || row.Participants != 2 || row.Kind != "dynamic" {
		t.Fatalf("proxy row = %+v", row)
	}
	if row := rows[homeKey]; row.Role != "home" {
		t.Fatalf("home row shadowed by a proxy row: %+v", row)
	}
	if _, raw := rows["k7xq2m"]; raw {
		t.Fatal("stats keyed by the raw room code")
	}

	collector := metrics.NewRoomCollector(src)
	if _, err := testutil.CollectAndLint(collector); err != nil {
		t.Errorf("CollectAndLint: %v", err)
	}
	promReg := prometheus.NewPedanticRegistry()
	promReg.MustRegister(collector)
	mfs, err := promReg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, c := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"gawk_rooms_live", map[string]string{"kind": "static"}, 1},
		{"gawk_rooms_live", map[string]string{"kind": "dynamic"}, 0},
		{"gawk_room_proxied_sessions", nil, 2},
		{"gawk_room_participants", map[string]string{"room": homeKey}, 0},
		{"gawk_room_attachments", map[string]string{"room": homeKey}, 0},
	} {
		if got := gaugeValue(mfs, c.name, c.labels); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
	if got := gaugeValue(mfs, "gawk_room_participants", map[string]string{"room": proxyKey}); got != -1 {
		t.Errorf("a proxied room got its own participants gauge: %v (want none; it folds into gawk_room_proxied_sessions)", got)
	}

	release1()
	if row := src.Stats()[proxyKey]; row.Participants != 1 {
		t.Fatalf("proxy row after one release = %+v", row)
	}
	release2()
	releaseHome()
	if rows := src.Stats(); len(rows) != 1 || rows[homeKey].Role != "home" {
		t.Fatalf("rows after every release = %+v, want only the home row", rows)
	}
}
