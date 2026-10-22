package transport

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"
	coordv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
)

// R74 PP8 (docs/76 item 14), home side of the transport: a mint never lands
// on a PoP reservation, and a resume token never forces one.

const testPop = "lan-party"

// newPopTestCoordinator is a real Coordinator over a fake clientset — the
// control plane every pod shares — not Run, so its Lookup cache stays empty
// and every check below goes to the authoritative Claim.
func newPopTestCoordinator(t *testing.T, cs *fake.Clientset, pod string) *cluster.Coordinator {
	t.Helper()
	c, err := cluster.New(cluster.Options{
		Client:         cs,
		Namespace:      "gawk",
		PodName:        pod,
		AdvertiseAddr:  pod + ":4433",
		BroadcastGrace: 5 * time.Minute,
		Log:            discardLog,
	})
	if err != nil {
		t.Fatalf("cluster.New: %v", err)
	}
	return c
}

// reservingCoordinator reserves each of the first n IDs the mint path claims
// for testPop just before the claim — a PoP reserving the very ID the hub's
// draw produced (the hub's IDReserved is left unset, so the draw itself
// cannot avoid it and the claim refusal is what is exercised).
type reservingCoordinator struct {
	*cluster.Coordinator
	mu       sync.Mutex
	n        int
	claims   []string
	reserved []string
}

func (c *reservingCoordinator) Claim(ctx context.Context, id string, force bool) (int64, error) {
	c.mu.Lock()
	c.claims = append(c.claims, id)
	reserve := len(c.reserved) < c.n
	if reserve {
		c.reserved = append(c.reserved, id)
	}
	c.mu.Unlock()
	if reserve {
		if err := c.Reserve(ctx, id, testPop); err != nil {
			return 0, fmt.Errorf("test reserve: %w", err)
		}
	}
	return c.Coordinator.Claim(ctx, id, force)
}

func (c *reservingCoordinator) seen() (claims, reserved []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.claims), slices.Clone(c.reserved)
}

func getLease(t *testing.T, cs *fake.Clientset, id string) *coordv1.Lease {
	t.Helper()
	l, err := cs.CoordinationV1().Leases("gawk").Get(context.Background(), "gawk-bc-"+strings.ToLower(id), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get lease %s: %v", id, err)
	}
	return l
}

func assertDormantReservation(t *testing.T, cs *fake.Clientset, id string) {
	t.Helper()
	l := getLease(t, cs, id)
	if l.Labels[cluster.PopLabelKey] != testPop {
		t.Errorf("lease %s labels = %v, want %s=%s", id, l.Labels, cluster.PopLabelKey, testPop)
	}
	if h := l.Spec.HolderIdentity; h != nil && *h != "" {
		t.Errorf("reservation %s gained holder %q", id, *h)
	}
}

func TestHomeMintRedrawsAroundAReservation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, reg, _, srv := startTestServerCfgLogSrv(t, ctx, seamTestConfig(), discardLog)
	cs := fake.NewClientset()
	coord := &reservingCoordinator{Coordinator: newPopTestCoordinator(t, cs, "pod-self"), n: 2}
	srv.SetCluster(coord, "pod-self")

	pub, id, _ := dialPublisherHandshake(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")

	claims, reserved := coord.seen()
	if len(reserved) != 2 || len(claims) != 3 {
		t.Fatalf("claims %v reserved %v, want 3 claims of which the first 2 drew reservations", claims, reserved)
	}
	if slices.Contains(reserved, id) || claims[2] != id {
		t.Fatalf("announced %s, want the third draw %s (reserved: %v)", id, claims[2], reserved)
	}
	for _, rid := range reserved {
		assertDormantReservation(t, cs, rid)
		// The refused draw's hub is removed outright, not left in grace: a
		// graced hub would satisfy CheckSubscribe and keep viewers of the
		// PoP's broadcast off the edge pull.
		if err := reg.CheckSubscribe(rid); !errors.Is(err, hub.ErrNotFound) {
			t.Errorf("hub for reserved draw %s survived: %v", rid, err)
		}
	}
	if l := getLease(t, cs, id); l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != "pod-self" {
		t.Errorf("minted lease holder = %v, want pod-self", l.Spec.HolderIdentity)
	}
}

func TestHomeMintGivesUpAfterBoundedRedraws(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, reg, _, srv := startTestServerCfgLogSrv(t, ctx, seamTestConfig(), discardLog)
	cs := fake.NewClientset()
	coord := &reservingCoordinator{Coordinator: newPopTestCoordinator(t, cs, "pod-self"), n: 1000}
	srv.SetCluster(coord, "pod-self")

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
	claims, _ := coord.seen()
	if len(claims) != 1+mintRedraws {
		t.Errorf("claims = %d, want %d (the draw plus %d redraws)", len(claims), 1+mintRedraws, mintRedraws)
	}
	for _, rid := range claims {
		assertDormantReservation(t, cs, rid)
		if err := reg.CheckSubscribe(rid); !errors.Is(err, hub.ErrNotFound) {
			t.Errorf("hub for reserved draw %s survived: %v", rid, err)
		}
	}
}

// A resume token is HMAC(key, id) and proves ownership of a home broadcast,
// never of a PoP's reservation: the reclaim gets 409, the Lease keeps no
// holder at generation 0, and no hub is left behind.
func TestResumeOnAReservationIs409(t *testing.T) {
	srv, sm, reg := newOutcomeServer(t, config.Config{}, hub.Options{})
	cs := fake.NewClientset()
	coord := newPopTestCoordinator(t, cs, "pod-self")
	srv.SetCluster(coord, "pod-self")
	const id = "K7XQ2M"
	if err := coord.Reserve(context.Background(), id, testPop); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	token := hex.EncodeToString(srv.resume.mint(id))
	w := httptest.NewRecorder()
	srv.handlePublish(w, connectReq("https://relay/publish/"+id+"?resume="+token, map[string]string{"id": id}))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if got := sm.ConnectionCount("publish", metrics.OutcomeConflict); got != 1 {
		t.Errorf("publish/conflict = %v, want 1", got)
	}
	assertDormantReservation(t, cs, id)
	if g := getLease(t, cs, id).Annotations["gawk/origin-generation"]; g != "0" {
		t.Errorf("reservation generation = %q, want 0 (never claimed)", g)
	}
	if err := reg.CheckSubscribe(id); !errors.Is(err, hub.ErrNotFound) {
		t.Errorf("refused resume left a hub: %v", err)
	}
}

// countingCoordinator answers Lookup from a fixed map and counts claims.
type countingCoordinator struct {
	fakeCoordinator
	mu     sync.Mutex
	claims int
}

func (c *countingCoordinator) Claim(context.Context, string, bool) (int64, error) {
	c.mu.Lock()
	c.claims++
	c.mu.Unlock()
	return 1, nil
}

// With the lease cache synced the refusal is decided before anything is
// touched: no claim, and no StopEdge of a pull serving the PoP's viewers.
func TestResumeOnACachedReservationNeverClaims(t *testing.T) {
	srv, sm, reg := newOutcomeServer(t, config.Config{}, hub.Options{})
	const id = "K7XQ2M"
	coord := &countingCoordinator{fakeCoordinator: fakeCoordinator{leases: map[string]fakeLease{
		id: {origin: cluster.Origin{Holder: "pod-b", Addr: "10.0.0.2:4433", Generation: 1, Pop: testPop}},
	}}}
	srv.SetCluster(coord, "pod-self")
	// A live edge hub for the PoP broadcast, as a pull would hold it.
	_, edgePub, err := reg.EdgePublish(id)
	if err != nil {
		t.Fatalf("EdgePublish: %v", err)
	}
	defer edgePub.Close()

	token := hex.EncodeToString(srv.resume.mint(id))
	w := httptest.NewRecorder()
	srv.handlePublish(w, connectReq("https://relay/publish/"+id+"?resume="+token, map[string]string{"id": id}))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if got := sm.ConnectionCount("publish", metrics.OutcomeConflict); got != 1 {
		t.Errorf("publish/conflict = %v, want 1", got)
	}
	if coord.claims != 0 {
		t.Errorf("claims = %d, want 0", coord.claims)
	}
	if st := reg.Stats().Broadcasts[reg.ObfuscateID(id)]; !st.PublisherActive {
		t.Error("the edge hub's publisher slot was taken by a refused resume")
	}
}

// The takeover path (an active slot on this pod, so the claim waits for the
// upgrade) refuses the same way: the session is closed 409 and the Lease
// stays a dormant reservation.
func TestTakeoverOnAReservationIs409(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, reg, _, srv := startTestServerCfgLogSrv(t, ctx, seamTestConfig(), discardLog)
	cs := fake.NewClientset()
	coord := newPopTestCoordinator(t, cs, "pod-self")
	srv.SetCluster(coord, "pod-self")
	const id = "K7XQ2M"
	if err := coord.Reserve(ctx, id, testPop); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// An incumbent in the slot (the PoP's leg would be one), so ResumePublish
	// reports it active and the claim is deferred past the upgrade.
	_, incumbent, err := reg.EdgePublish(id)
	if err != nil {
		t.Fatalf("EdgePublish: %v", err)
	}
	defer incumbent.Close()

	token := hex.EncodeToString(srv.resume.mint(id))
	sess := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish/%s?resume=%s", port, id, token), clientTLS)
	acceptCtx, acceptCancel := context.WithTimeout(ctx, 5*time.Second)
	defer acceptCancel()
	for {
		st, err := sess.AcceptUniStream(acceptCtx)
		if err != nil {
			var se *webtransport.SessionError
			if !errors.As(err, &se) || se.ErrorCode != http.StatusConflict {
				t.Fatalf("session ended with %v, want a 409 session close", err)
			}
			break
		}
		st.CancelRead(0)
	}
	assertDormantReservation(t, cs, id)
}
