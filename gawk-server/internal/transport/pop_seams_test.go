package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
)

// R74 W0 (docs/76): the PoP seams are behaviour-neutral until a PoP
// subsystem installs something into them. These tests pin what each one
// does once installed, so the later packages build on a known shape.

type recordingMintGate struct {
	mu        sync.Mutex
	ids       []string
	localOnly bool
	err       error
}

func (g *recordingMintGate) ReserveMint(_ context.Context, id string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ids = append(g.ids, id)
	return g.localOnly, g.err
}

func (g *recordingMintGate) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.ids...)
}

type recordingObserver struct {
	mu  sync.Mutex
	ids []string
}

func (o *recordingObserver) PublisherLive(r *http.Request, id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if r == nil {
		id = "<nil request>"
	}
	o.ids = append(o.ids, id)
}

func (o *recordingObserver) seen() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.ids...)
}

func seamTestConfig() config.Config {
	return config.Config{
		MaxSubscribers:  4,
		MaxBroadcasts:   4,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace:  5 * time.Minute,
	}
}

// An installed gate sees every minted ID before the announce, a reclaim
// does not consult it, and every registered observer is told of both the
// mint and the reclaim.
func TestMintGateAndPublishObserversSeeEveryPublisher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _, srv := startTestServerCfgLogSrv(t, ctx, seamTestConfig(), discardLog)

	gate := &recordingMintGate{localOnly: true}
	srv.SetMintGate(gate)
	obsA, obsB := &recordingObserver{}, &recordingObserver{}
	srv.AddPublishObserver(obsA)
	srv.AddPublishObserver(obsB)

	pub, id, token := dialPublisherHandshake(t, ctx, port, clientTLS)
	if got := gate.seen(); len(got) != 1 || got[0] != id {
		t.Fatalf("gate saw %v, want [%s]", got, id)
	}
	pub.CloseWithError(0, "")

	again := dialPublisherReclaim(t, ctx, port, id, token, clientTLS)
	defer again.CloseWithError(0, "")
	if got := gate.seen(); len(got) != 1 {
		t.Errorf("gate consulted on a reclaim: %v", got)
	}
	for name, o := range map[string]*recordingObserver{"A": obsA, "B": obsB} {
		waitFor(t, 5*time.Second, func() bool { return len(o.seen()) == 2 }, "observer "+name+" told twice")
		if got := o.seen(); got[0] != id || got[1] != id {
			t.Errorf("observer %s saw %v, want [%s %s]", name, got, id, id)
		}
	}
}

// A gate error refuses the mint: no announce, the session closes with 503
// and the minted hub does not stay live.
func TestMintGateErrorRefusesTheMint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, reg, _, srv := startTestServerCfgLogSrv(t, ctx, seamTestConfig(), discardLog)
	gate := &recordingMintGate{err: errors.New("link down")}
	srv.SetMintGate(gate)
	obs := &recordingObserver{}
	srv.AddPublishObserver(obs)

	sess := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
	acceptCtx, acceptCancel := context.WithTimeout(ctx, 5*time.Second)
	defer acceptCancel()
	for {
		st, err := sess.AcceptUniStream(acceptCtx)
		if err != nil {
			var se *webtransport.SessionError
			if !errors.As(err, &se) || se.ErrorCode != http.StatusServiceUnavailable {
				t.Fatalf("session ended with %v, want a 503 session close", err)
			}
			break
		}
		st.CancelRead(0)
	}
	ids := gate.seen()
	if len(ids) != 1 {
		t.Fatalf("gate saw %v, want one ID", ids)
	}
	// The hub may linger in grace, but it has no live publisher.
	waitFor(t, 5*time.Second, func() bool {
		live, _, _ := reg.BroadcastState(ids[0])
		return !live
	}, "refused mint not live")
	if got := obs.seen(); len(got) != 0 {
		t.Errorf("observer told of a refused mint: %v", got)
	}

	// Removing the gate restores the plain mint.
	srv.SetMintGate(nil)
	pub, _, _ := dialPublisherHandshake(t, ctx, port, clientTLS)
	pub.CloseWithError(0, "")
}

// EnsureEdge on a reservation (a gawk/pop lease with no holder) is handed
// to the installed onReserved hook; without the hook it is today's 404.
func TestEnsureEdgeHandsReservationsToOnReserved(t *testing.T) {
	h := newEdgeHarness(t)
	h.resolver.set(cluster.Origin{Pop: "lan-party"}, nil)
	if err := h.manager.EnsureEdge(context.Background(), "AAAAAA"); !errors.Is(err, hub.ErrNotFound) {
		t.Fatalf("no hook: EnsureEdge = %v, want hub.ErrNotFound", err)
	}

	var got cluster.Origin
	var gotID string
	sentinel := errors.New("hook ran")
	h.manager.onReserved = func(_ context.Context, id string, o cluster.Origin) error {
		gotID, got = id, o
		return sentinel
	}
	if err := h.manager.EnsureEdge(context.Background(), "AAAAAA"); !errors.Is(err, sentinel) {
		t.Fatalf("hook: EnsureEdge = %v, want the hook's error", err)
	}
	if gotID != "AAAAAA" || got.Pop != "lan-party" {
		t.Errorf("hook got (%s, %+v), want (AAAAAA, Pop lan-party)", gotID, got)
	}

	// A held lease (pop label or not) and a plain empty-holder lease never
	// reach the hook.
	called := false
	h.manager.onReserved = func(context.Context, string, cluster.Origin) error { called = true; return nil }
	h.resolver.set(cluster.Origin{}, nil)
	if err := h.manager.EnsureEdge(context.Background(), "AAAAAA"); !errors.Is(err, hub.ErrNotFound) || called {
		t.Errorf("plain empty holder: err=%v called=%v, want hub.ErrNotFound and no hook", err, called)
	}
	h.resolver.set(cluster.Origin{Holder: "pod-self", Pop: "lan-party"}, nil)
	if err := h.manager.EnsureEdge(context.Background(), "AAAAAA"); !errors.Is(err, hub.ErrNotFound) || called {
		t.Errorf("self-held pop lease: err=%v called=%v, want hub.ErrNotFound and no hook", err, called)
	}
}
