package transport

// R42 RM3 (docs/44 §4.5) over real QUIC: two in-process relay pods sharing
// one fake dynamic client (the control plane). The internal room route's
// status vocabulary (404 no wiring / 401 PSK / 404 not home / 409 stale
// generation), proxy piping (a joiner on pod B reaches a room homed on pod A
// and sees the same RoomState; the home sees one more participant), the
// /statusz proxy row, the non-terminal close when the upstream goes away,
// adoption after the home drained (attachments rebuilt from the CR), a
// static CR joinable from both pods, and the lease-loss fence.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientfeatures "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/roomcluster"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

type roomPod struct {
	name  string
	port  int
	srv   *Server
	hub   *hub.Registry
	reg   *roomsrv.Registry
	store *roomcluster.Store
	coord *cluster.Coordinator
	done  chan error
}

func (p *roomPod) url(path string) string { return fmt.Sprintf("https://127.0.0.1:%d%s", p.port, path) }

type roomFleet struct {
	client    *dynamicfake.FakeDynamicClient
	cs        *fake.Clientset
	cert      tls.Certificate
	pool      *x509.CertPool
	clientTLS *tls.Config
	// leaseWatches receives one token per Lease watch registered with the
	// fake tracker (see cluster_test.go's leaseWatchRegistered: a watch
	// that starts late never learns of earlier objects, so a pod must not
	// be handed out before its lease informer is actually watching).
	leaseWatches chan struct{}
	// broadcastGrace is the hub's (and the lease's) grace; the lifecycle
	// test shortens it before starting its pods.
	broadcastGrace time.Duration
}

func newRoomFleet(t *testing.T, objs ...runtime.Object) *roomFleet {
	t.Helper()
	// Plain LIST+WATCH for the fake tracker (see roomcluster's tests).
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.WatchListClient, false)
	cert, err := tlsutil.GenerateDevCert([]string{"localhost", "127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatalf("GenerateDevCert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{rooms.GroupVersionResource: rooms.ListKind}, objs...)
	f := &roomFleet{client: client, cs: fake.NewClientset(), cert: cert, pool: pool,
		clientTLS:      &tls.Config{RootCAs: pool, ServerName: "localhost", NextProtos: []string{"h3"}},
		leaseWatches:   make(chan struct{}, 16),
		broadcastGrace: 5 * time.Minute}
	gvr := coordv1.SchemeGroupVersion.WithResource("leases")
	f.cs.PrependWatchReactor("leases", func(action k8stesting.Action) (bool, watch.Interface, error) {
		w, err := f.cs.Tracker().Watch(gvr, action.GetNamespace())
		if err != nil {
			return false, nil, err
		}
		f.leaseWatches <- struct{}{}
		return true, w, nil
	})
	return f
}

// startRoomPod boots one pod with rooms, the R17 origin coordinator and
// the cluster room store wired the way main does it: the hub's lifecycle
// hooks stamp/delete the broadcast lease and move the local registry, the
// room registry's broadcast source is the transport's fleet-wide one, both
// informers are running and synced, and the refresh poll runs (after the
// lease sync, as main orders it).
func (f *roomFleet) startRoomPod(t *testing.T, ctx context.Context, podName string) *roomPod {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()

	cfg := config.Config{
		Addr:               fmt.Sprintf("127.0.0.1:%d", port),
		MaxSubscribers:     4,
		MaxIdleTimeout:     30 * time.Second,
		KeepAlivePeriod:    10 * time.Second,
		BroadcastGrace:     f.broadcastGrace,
		InternalPSK:        "fleet-psk",
		InternalServerName: "localhost",
		ResumeTokenKey:     []byte(strings.Repeat("k", 32)),
		StatsKey:           []byte(strings.Repeat("s", 32)),
		Rooms:              true,
		ClusterMode:        true,
	}
	// The hooks close over the late-bound coordinator and registry, as in
	// main: cluster mode's lease stamp/delete first, then the room hook.
	var (
		coord *cluster.Coordinator
		reg   *roomsrv.Registry
		srv   *Server
	)
	r := hub.NewRegistry(discardLog, hub.Options{
		MaxSubscribers: cfg.MaxSubscribers, BroadcastGrace: cfg.BroadcastGrace, StatsKey: cfg.StatsKey,
		// Errors are dropped as main only logs them (and the hooks can fire
		// during teardown, after the test has ended).
		OnPublisherClosed: func(id string) {
			_ = coord.EnterGrace(context.Background(), id)
			reg.PublisherClosed(id)
		},
		OnBroadcastExpired: func(id string) {
			_ = coord.Delete(context.Background(), id)
			reg.BroadcastExpired(id)
		},
		IDReserved: func(id string) bool { return reg.Has(id) },
	})
	coord, err = cluster.New(cluster.Options{
		Client: f.cs, Namespace: "gawk", PodName: podName, AdvertiseAddr: cfg.Addr,
		BroadcastGrace: cfg.BroadcastGrace, Log: discardLog,
		OnLeaseDeleted: func(id string) { srv.HandleLeaseDeleted(id) },
		OnLeaseLost:    func(id string, o cluster.Origin) { srv.HandleLeaseLost(id, o) },
		RenewInterval:  50 * time.Millisecond, LeaseDuration: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("cluster.New: %v", err)
	}
	srv = New(cfg, r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &f.cert, nil }, discardLog,
		metrics.NewServerMetrics(prometheus.NewRegistry()))
	srv.drainSleep = func(time.Duration) {}
	srv.SetCluster(coord, podName)
	srv.edgeManager().dial = newEdgeDialer(cfg.InternalServerName, cfg.InternalPSK, f.pool, discardLog)

	var store *roomcluster.Store
	reg = roomsrv.NewRegistry(roomsrv.Options{
		Broadcasts: srv.RoomBroadcasts(), Obfuscate: r.ObfuscateID, Log: discardLog, EmptyGrace: time.Hour, PodName: podName,
		RefreshInterval: 100 * time.Millisecond, UnknownIsExpired: true,
		Reserve:              func(ctx context.Context, room *rooms.Room) error { return store.Reserve(ctx, room) },
		OnRoomEnded:          func(code string, reason uint8) { store.RoomEnded(code, reason) },
		OnRoomEmpty:          func(code string, empty bool) { store.RoomEmpty(code, empty) },
		OnAttachmentsChanged: func(code string, list []rooms.Attachment) { store.AttachmentsChanged(code, list) },
	})
	store, err = roomcluster.New(roomcluster.Options{
		Client: f.client, Namespace: "gawk", PodName: podName, AdvertiseAddr: cfg.Addr,
		Registry: reg, Obfuscate: r.ObfuscateID, Log: discardLog,
		OnLeaseLost:   srv.HandleRoomLeaseLost,
		RenewInterval: 50 * time.Millisecond, LeaseDuration: 2 * time.Second, SyncTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("roomcluster.New: %v", err)
	}
	srv.SetRoomCluster(store, podName)
	// The fleet's cert is self-signed: point the proxy dialer at its pool.
	srv.roomClusterWiring().dial = newRoomProxyDialer(cfg.InternalServerName, cfg.InternalPSK, f.pool)
	srv.SetRooms(reg)
	go coord.Run(ctx)
	select {
	case <-f.leaseWatches:
	case <-time.After(15 * time.Second):
		t.Fatalf("%s: lease informer never registered its watch", podName)
	}
	if !coord.WaitLeaseSync(ctx) {
		t.Fatalf("%s: lease informer never synced", podName)
	}
	go store.Run(ctx)
	waitFor(t, 15*time.Second, store.HasSynced, podName+" room informer sync")
	go reg.RunRefresh(ctx)

	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	return &roomPod{name: podName, port: port, srv: srv, hub: r, reg: reg, store: store, coord: coord, done: done}
}

func (f *roomFleet) roomCR(t *testing.T, name string) *rooms.Room {
	t.Helper()
	u, err := f.client.Resource(rooms.GroupVersionResource).Namespace("gawk").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get room CR %s: %v", name, err)
	}
	var r rooms.Room
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func staticRoomObject(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	r := &rooms.Room{TypeMeta: metav1.TypeMeta{APIVersion: rooms.SchemeGroupVersion.String(), Kind: rooms.Kind},
		Spec: rooms.RoomSpec{Kind: rooms.KindStatic, DisplayCode: "TuhisRoom"}}
	r.Name = name
	r.Namespace = "gawk"
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(r)
	if err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: raw}
}

func expectStatus(t *testing.T, ctx context.Context, url string, clientTLS *tls.Config, want int) {
	t.Helper()
	rsp, sess, err := dialOnce(t, ctx, url, clientTLS)
	if err == nil {
		sess.CloseWithError(0, "")
		t.Fatalf("%s: dial succeeded, want %d", url, want)
	}
	if rsp == nil || rsp.StatusCode != want {
		t.Fatalf("%s: got %v (err %v), want %d", url, rsp, err, want)
	}
}

func roomStatszRows(t *testing.T, ctx context.Context, pod *roomPod, clientTLS *tls.Config) map[string]roomsrv.RoomStats {
	t.Helper()
	_, body := h3Get(t, ctx, clientTLS, pod.url("/statusz"))
	var doc struct {
		Rooms map[string]roomsrv.RoomStats `json:"rooms"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Rooms
}

// The internal route's vocabulary, in gate order.
func TestInternalRoomRouteVocabulary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newRoomFleet(t)
	a := f.startRoomPod(t, ctx, "pod-a")

	// A pod with rooms but no cluster wiring: 404, nothing else.
	port, plainTLS, _, _, plain := startRoomServer(t, ctx, nil)
	_ = plain
	expectStatus(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/internal/room/abcdef?psk=fleet-psk&gen=1", port), plainTLS, http.StatusNotFound)

	pub, id, tokenHex := dialPublisherHandshake(t, ctx, a.port, f.clientTLS)
	defer pub.CloseWithError(0, "")
	creator := openControl(t, ctx, a.url("/room/new?broadcast="+id+"&resume="+tokenHex+"&label=pc"), f.clientTLS, "tuhis")
	st := creator.nextState(t)
	code := strings.ToLower(st.Code)
	if gen, held := a.store.Holding(code); !held || gen != 1 {
		t.Fatalf("mint did not make pod-a the home: gen=%d held=%v", gen, held)
	}

	expectStatus(t, ctx, a.url("/internal/room/"+code+"?psk=wrong&gen=1"), f.clientTLS, http.StatusUnauthorized)
	expectStatus(t, ctx, a.url("/internal/room/zzzzzz?psk=fleet-psk&gen=1"), f.clientTLS, http.StatusNotFound)
	expectStatus(t, ctx, a.url("/internal/room/"+code+"?psk=fleet-psk&gen=2"), f.clientTLS, http.StatusConflict)
	expectStatus(t, ctx, a.url("/internal/room/"+code+"?psk=fleet-psk&gen=1&creator=00000000000000000000000000000000"), f.clientTLS, http.StatusForbidden)
	// The right fence joins like any local participant.
	viewer := openControl(t, ctx, a.url("/internal/room/"+code+"?psk=fleet-psk&gen=1"), f.clientTLS, "viewer")
	if vst := viewer.nextState(t); len(vst.Participants) != 2 || vst.Code != st.Code {
		t.Fatalf("internal join state = %+v", vst)
	}
}

// A joiner on pod B reaches a room homed on pod A through the proxy and
// sees the same RoomState; the home sees the participant; B reports a
// proxy row; when A drains the joiner is closed non-terminally, and its
// re-dial lands on B, which adopts with the attachment list whole.
func TestRoomProxyPipesAndAdoptsAfterTheHomeDrains(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newRoomFleet(t)
	aCtx, aCancel := context.WithCancel(ctx)
	defer aCancel()
	a := f.startRoomPod(t, aCtx, "pod-a")
	b := f.startRoomPod(t, ctx, "pod-b")

	pub, id, tokenHex := dialPublisherHandshake(t, ctx, a.port, f.clientTLS)
	defer pub.CloseWithError(0, "")
	creator := openControl(t, ctx, a.url("/room/new?broadcast="+id+"&resume="+tokenHex+"&label=pc"), f.clientTLS, "tuhis")
	st := creator.nextState(t)
	code := strings.ToLower(st.Code)
	// B's cache sees A as the live home.
	waitFor(t, 15*time.Second, func() bool {
		home, ok := b.store.Resolve(code)
		return ok && home.Live && home.Holder == "pod-a"
	}, "pod-b to resolve the home")

	// Wrong attach/creator params are judged by the home and propagated.
	expectStatus(t, ctx, b.url("/room/"+code+"?creator=00000000000000000000000000000000"), f.clientTLS, http.StatusForbidden)
	expectStatus(t, ctx, b.url("/room/nosuch"), f.clientTLS, http.StatusNotFound)

	joiner := openControl(t, ctx, b.url("/room/"+code), f.clientTLS, "viewer")
	jst := joiner.nextState(t)
	if jst.Code != st.Code || len(jst.Participants) != 2 || len(jst.Attachments) != 1 || jst.Attachments[0].BroadcastID != id {
		t.Fatalf("proxied state = %+v, want the home's view", jst)
	}
	if string(jst.Key) != string(st.Key) {
		t.Fatalf("proxied key %x != home key %x", jst.Key, st.Key)
	}
	if e := creator.nextEvent(t, wire.RoomEventParticipantJoined); e.Participant.Nickname != "viewer" {
		t.Fatalf("home saw %+v", e)
	}
	// Commands flow through: a nickname change is echoed to the home.
	joiner.command(t, wire.RoomCommand{Kind: wire.RoomCommandSetNickname, Nickname: "renamed"})
	if e := creator.nextEvent(t, wire.RoomEventParticipantUpdated); e.Participant.Nickname != "renamed" {
		t.Fatalf("home saw %+v", e)
	}
	if info, _ := a.reg.Lookup(code); info.Participants != 2 {
		t.Fatalf("home participants = %d, want 2 (one local, one proxied)", info.Participants)
	}
	if _, held := b.store.Holding(code); held {
		t.Fatal("pod-b claimed a room it should proxy")
	}
	rows := roomStatszRows(t, ctx, b, f.clientTLS)
	key := a.hub.ObfuscateID(code)
	if row, ok := rows[key]; !ok || row.Role != "proxy" || row.Participants != 1 || row.Kind != rooms.KindDynamic {
		t.Fatalf("pod-b statusz rooms = %+v, want a proxy row under %s", rows, key)
	}
	if row := roomStatszRows(t, ctx, a, f.clientTLS)[key]; row.Role != "home" || row.Participants != 2 {
		t.Fatalf("pod-a statusz row = %+v", row)
	}
	cr := f.roomCR(t, code)
	if len(cr.Status.Attachments) != 1 || cr.Status.Attachments[0].BroadcastID != id || cr.Status.Attachments[0].Label != "pc" {
		t.Fatalf("CR attachments = %+v", cr.Status.Attachments)
	}
	if cr.Status.EmptySince != nil {
		t.Fatalf("CR emptySince still set with two participants: %v", cr.Status.EmptySince)
	}

	// The home drains: the proxied joiner is closed NON-terminally (the
	// home's own 4002 rides through the pipe), never 4007.
	aCancel()
	select {
	case err := <-a.done:
		t.Logf("DEBUG pod-a Run returned: %v; holding=%v", err, func() bool { _, h := a.store.Holding(code); return h }())
	case <-time.After(10 * time.Second):
		t.Fatal("pod-a did not drain")
	}
	if c := sessionCloseCode(t, joiner.sess); c == wire.CloseCodeRoomEnded || c < 0 {
		t.Fatalf("proxied session close = %d, want a non-terminal application close", c)
	}
	cr = f.roomCR(t, code)
	if cr.Status.Lease == nil || cr.Status.Lease.Holder != "" || cr.Status.Lease.Generation != 1 {
		t.Fatalf("lease after drain = %+v, want released at gen 1", cr.Status.Lease)
	}
	// The re-dial lands on B, which adopts without any staleness wait and
	// rebuilds the attachment from the CR.
	rejoin := openControl(t, ctx, b.url("/room/"+code), f.clientTLS, "viewer")
	rst := rejoin.nextState(t)
	if rst.Code != st.Code || len(rst.Attachments) != 1 || rst.Attachments[0].BroadcastID != id || rst.Attachments[0].Label != "pc" {
		t.Fatalf("adopted state = %+v, want the attachment list whole", rst)
	}
	if gen, held := b.store.Holding(code); !held || gen != 2 {
		t.Fatalf("pod-b after adoption: gen=%d held=%v, want 2/true", gen, held)
	}
	if row := roomStatszRows(t, ctx, b, f.clientTLS)[key]; row.Role != "home" || row.Participants != 1 {
		t.Fatalf("pod-b statusz row after adoption = %+v", row)
	}
	if !b.reg.Has(code) {
		t.Fatal("the adopted room is not in pod-b's registry")
	}
}

// A static Room CR applied by the operator has no home until its first
// join: the pod that receives it claims, the other proxies — joinable from
// both. Deleting the CR ends it on the home with 4007 (operator).
func TestStaticRoomCRIsJoinableOnBothPods(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newRoomFleet(t, staticRoomObject(t, "tuhisroom"))
	a := f.startRoomPod(t, ctx, "pod-a")
	b := f.startRoomPod(t, ctx, "pod-b")
	if !a.store.Known("TuhisRoom") || !b.store.Known("tuhisroom") {
		t.Fatal("the static CR is not in both informer caches")
	}
	if a.reg.Has("tuhisroom") {
		t.Fatal("a static CR was upserted before any join (a static room has no home until its first participant)")
	}

	first := openControl(t, ctx, a.url("/room/TuhisRoom"), f.clientTLS, "first")
	fst := first.nextState(t)
	if fst.Code != "TuhisRoom" || fst.Flags&wire.RoomStateFlagDynamic != 0 || fst.Flags&wire.RoomStateFlagAttachOK == 0 {
		t.Fatalf("static state = %+v", fst)
	}
	if _, held := a.store.Holding("tuhisroom"); !held {
		t.Fatal("the first join did not claim the static room")
	}
	if cr := f.roomCR(t, "tuhisroom"); cr.Status.Key != a.hub.ObfuscateID("tuhisroom") || cr.Status.Lease.Holder != "pod-a" {
		t.Fatalf("CR status after the claim = %+v", cr.Status)
	}
	waitFor(t, 15*time.Second, func() bool { h, ok := b.store.Resolve("tuhisroom"); return ok && h.Live }, "pod-b to see the home")
	second := openControl(t, ctx, b.url("/room/tuhisroom"), f.clientTLS, "second")
	if sst := second.nextState(t); len(sst.Participants) != 2 || sst.Code != "TuhisRoom" {
		t.Fatalf("proxied static state = %+v", sst)
	}
	if e := first.nextEvent(t, wire.RoomEventParticipantJoined); e.Participant.Nickname != "second" {
		t.Fatalf("home saw %+v", e)
	}

	// The operator deletes the CR: every participant sees RoomEnding then
	// 4007 — the proxied one through the pipe.
	if err := f.client.Resource(rooms.GroupVersionResource).Namespace("gawk").Delete(ctx, "tuhisroom", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if e := first.nextEvent(t, wire.RoomEventRoomEnding); e.Reason != wire.RoomEndReasonOperator {
		t.Fatalf("ending = %+v", e)
	}
	if c := sessionCloseCode(t, first.sess); c != wire.CloseCodeRoomEnded {
		t.Fatalf("home participant close = %d, want 4007", c)
	}
	if c := sessionCloseCode(t, second.sess); c != wire.CloseCodeRoomEnded {
		t.Fatalf("proxied participant close = %d, want 4007 propagated through the proxy", c)
	}
}

// Fencing: a home whose lease is force-taken closes its local control
// sessions non-terminally and forgets the room, so the next join on it
// proxies to the new home instead of serving a stale copy.
func TestRoomLeaseLossClosesLocalSessionsNonTerminally(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newRoomFleet(t)
	a := f.startRoomPod(t, ctx, "pod-a")
	b := f.startRoomPod(t, ctx, "pod-b")

	pub, id, tokenHex := dialPublisherHandshake(t, ctx, a.port, f.clientTLS)
	defer pub.CloseWithError(0, "")
	creator := openControl(t, ctx, a.url("/room/new?broadcast="+id+"&resume="+tokenHex), f.clientTLS, "tuhis")
	st := creator.nextState(t)
	code := strings.ToLower(st.Code)
	waitFor(t, 15*time.Second, func() bool { _, ok := b.store.Resolve(code); return ok }, "pod-b cache")

	// Pod B force-takes (what adoption does once the lease has gone stale).
	if _, err := b.store.Claim(ctx, code, true); err != nil {
		t.Fatalf("force-take: %v", err)
	}
	if c := sessionCloseCode(t, creator.sess); c == wire.CloseCodeRoomEnded || c < 0 {
		t.Fatalf("fenced home closed its session with %d, want non-terminal", c)
	}
	waitFor(t, 15*time.Second, func() bool { return !a.reg.Has(code) }, "pod-a to drop its stale copy")
	if _, held := a.store.Holding(code); held {
		t.Fatal("pod-a still believes it holds the room")
	}
	// A join on A now proxies to B (which has the lease but adopts the room
	// into its registry only through Adopt; Claim alone leaves B's registry
	// empty, so the proxied join is judged by B: 404 from CheckJoin).
	expectStatus(t, ctx, a.url("/room/"+code), f.clientTLS, http.StatusNotFound)
	if _, held := a.store.Holding(code); held {
		t.Fatal("the proxied join made pod-a claim against a live home")
	}
}

// RoomBroadcasts is the registry's only view of a broadcast (docs/44 D1).
// Single-pod shape (no SetCluster): the local hub is the whole answer —
// unknown stays unknown, publishing is live with the viewer count, gone
// within the grace is known but away. Cluster shape: the origin lease
// answers for what no local hub knows — held is live, in grace is away,
// and the viewer count is 0 (G is computed on the origin), while a local
// hub still wins when there is one.
func TestRoomBroadcastsAnswersLocalHubThenOriginLease(t *testing.T) {
	cfg := config.Config{MaxSubscribers: 5, MaxIdleTimeout: 30 * time.Second, KeepAlivePeriod: 10 * time.Second, BroadcastGrace: time.Hour}
	srv, _, r := newOutcomeServer(t, cfg, hub.Options{BroadcastGrace: time.Hour})
	src := srv.RoomBroadcasts()

	if st, known := src.BroadcastState("ZZZZZZ"); known || st.Live || st.Viewers != 0 {
		t.Fatalf("unknown broadcast: %+v, known=%v", st, known)
	}
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	if st, known := src.BroadcastState(id); !known || !st.Live || st.Viewers != 0 {
		t.Fatalf("fresh publisher: %+v, known=%v", st, known)
	}
	if _, err := r.Subscribe(id, nopHubConn{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if st, known := src.BroadcastState(id); !known || !st.Live || st.Viewers != 1 {
		t.Fatalf("with a viewer: %+v, known=%v", st, known)
	}
	p.Close()
	if st, known := src.BroadcastState(id); !known || st.Live || st.Viewers != 1 {
		t.Fatalf("publisher away within the grace: %+v, known=%v", st, known)
	}

	// Cluster shape: leases answer for what the hub does not know.
	srv.SetCluster(&fakeCoordinator{leases: map[string]fakeLease{
		"LIVEAA":  {origin: cluster.Origin{Holder: "pod-b", Addr: "10.0.0.2:4433", Generation: 3}},
		"GRACEA":  {origin: cluster.Origin{Holder: "pod-b"}, inGrace: true},
		"DRAINED": {origin: cluster.Origin{}}, // released holder: the broadcaster is mid-reconnect
	}}, "pod-a")
	if st, known := src.BroadcastState("LIVEAA"); !known || !st.Live || st.Viewers != 0 {
		t.Fatalf("lease held elsewhere: %+v, known=%v, want known+live with 0 viewers", st, known)
	}
	if st, known := src.BroadcastState("GRACEA"); !known || st.Live {
		t.Fatalf("lease in grace: %+v, known=%v, want known+away", st, known)
	}
	if st, known := src.BroadcastState("DRAINED"); !known || st.Live {
		t.Fatalf("lease without a holder: %+v, known=%v, want known+away", st, known)
	}
	if _, known := src.BroadcastState("ZZZZZZ"); known {
		t.Fatal("no lease anywhere must stay unknown")
	}
	// The local hub still answers first — with its own live/away and G.
	if st, known := src.BroadcastState(id); !known || st.Live || st.Viewers != 1 {
		t.Fatalf("local hub in cluster mode: %+v, known=%v", st, known)
	}
}

// nopHubConn is the least a hub subscriber needs: the source only counts
// it, no media ever reaches it here.
type nopHubConn struct{}

func (nopHubConn) SendDatagram([]byte) error                       { return nil }
func (nopHubConn) OpenKeyframeStream() (hub.KeyframeStream, error) { return nil, io.ErrClosedPipe }
func (nopHubConn) OpenCarrierStream() (hub.KeyframeStream, error)  { return nil, io.ErrClosedPipe }
func (nopHubConn) CloseWithError(uint32, string) error             { return nil }

// PR #302 review, finding A: the broadcast source must answer fleet-wide.
// The publisher is on pod A; the mint lands on pod B (no session affinity
// on the Service makes that the common case), so B has no hub for the
// broadcast — and answered 404 before the origin lease was consulted. Now
// the mint succeeds with the attachment live, a joiner on B sees it live,
// and the lifecycle follows the lease rather than the hub hooks (which
// fire on A, the origin, not on B, the home): the publisher leaving A
// stamps the lease's grace and B's refresh flips the tile to away; A's
// grace expiry deletes the lease and B's refresh removes the attachment
// with reason expired and drops it from the CR (docs/44 §6).
func TestRoomMintOnAnotherPodThanThePublisherFollowsTheLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newRoomFleet(t)
	f.broadcastGrace = 2 * time.Second
	a := f.startRoomPod(t, ctx, "pod-a")
	b := f.startRoomPod(t, ctx, "pod-b")

	pub, id, tokenHex := dialPublisherHandshake(t, ctx, a.port, f.clientTLS)
	waitFor(t, 15*time.Second, func() bool { _, _, ok := b.coord.Lookup(id); return ok }, "pod-b to see the origin lease")
	if _, _, known := b.hub.BroadcastState(id); known {
		t.Fatal("pod-b has a hub for a broadcast nobody there watches; the test would not exercise the lease path")
	}

	creator := openControl(t, ctx, b.url("/room/new?broadcast="+id+"&resume="+tokenHex+"&label=pc"), f.clientTLS, "tuhis")
	st := creator.nextState(t)
	if len(st.Attachments) != 1 || st.Attachments[0].BroadcastID != id || !st.Attachments[0].Live || st.Attachments[0].ViewerCount != 0 {
		t.Fatalf("mint on the non-origin pod: attachments = %+v, want the broadcast live with 0 viewers (G is pod-local off-origin)", st.Attachments)
	}
	code := strings.ToLower(st.Code)
	if _, held := b.store.Holding(code); !held {
		t.Fatal("the mint did not make pod-b the home")
	}
	joiner := openControl(t, ctx, b.url("/room/"+code), f.clientTLS, "viewer")
	if jst := joiner.nextState(t); len(jst.Attachments) != 1 || !jst.Attachments[0].Live {
		t.Fatalf("joiner state = %+v, want the attachment live", jst)
	}
	if e := creator.nextEvent(t, wire.RoomEventParticipantJoined); e.Participant.Nickname != "viewer" {
		t.Fatalf("home saw %+v", e)
	}

	// The publisher leaves pod A: its hub hook stamps the lease's grace
	// (the hook lands in A's registry, which does not hold the room), and
	// pod B's refresh reads the stamp within its interval.
	pub.CloseWithError(0, "")
	if e := creator.nextEvent(t, wire.RoomEventAttachmentUpdated); e.Attachment.BroadcastID != id || e.Attachment.Live {
		t.Fatalf("after the publisher left: %+v, want away", e)
	}
	if e := joiner.nextEvent(t, wire.RoomEventAttachmentUpdated); e.Attachment.Live {
		t.Fatalf("joiner after the publisher left: %+v, want away", e)
	}

	// A's grace expiry deletes the lease; B's refresh sees no hub and no
	// lease and expires the attachment.
	if e := creator.nextEvent(t, wire.RoomEventAttachmentRemoved); e.Attachment.BroadcastID != id || e.Reason != wire.RoomDetachReasonExpired {
		t.Fatalf("after the grace: %+v, want removed with reason expired", e)
	}
	if e := joiner.nextEvent(t, wire.RoomEventAttachmentRemoved); e.Reason != wire.RoomDetachReasonExpired {
		t.Fatalf("joiner after the grace: %+v", e)
	}
	if _, _, ok := b.coord.Lookup(id); ok {
		t.Fatal("the origin lease outlived the broadcast grace")
	}
	waitFor(t, 15*time.Second, func() bool { return len(f.roomCR(t, code).Status.Attachments) == 0 }, "the CR to drop the dead broadcast")
	if len(b.reg.Attachments(code)) != 0 {
		t.Fatalf("pod-b registry still lists %+v", b.reg.Attachments(code))
	}
}
