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
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientfeatures "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"

	"github.com/prometheus/client_golang/prometheus"

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
	done  chan error
}

func (p *roomPod) url(path string) string { return fmt.Sprintf("https://127.0.0.1:%d%s", p.port, path) }

type roomFleet struct {
	client    *dynamicfake.FakeDynamicClient
	cert      tls.Certificate
	pool      *x509.CertPool
	clientTLS *tls.Config
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
	return &roomFleet{client: client, cert: cert, pool: pool,
		clientTLS: &tls.Config{RootCAs: pool, ServerName: "localhost", NextProtos: []string{"h3"}}}
}

// startRoomPod boots one pod with rooms + the cluster room store wired the
// way main does it, the store's informer running and synced.
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
		BroadcastGrace:     5 * time.Minute,
		InternalPSK:        "fleet-psk",
		InternalServerName: "localhost",
		ResumeTokenKey:     []byte(strings.Repeat("k", 32)),
		StatsKey:           []byte(strings.Repeat("s", 32)),
		Rooms:              true,
		ClusterMode:        true,
	}
	r := hub.NewRegistry(discardLog, hub.Options{MaxSubscribers: cfg.MaxSubscribers, BroadcastGrace: cfg.BroadcastGrace, StatsKey: cfg.StatsKey})
	srv := New(cfg, r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &f.cert, nil }, discardLog,
		metrics.NewServerMetrics(prometheus.NewRegistry()))
	srv.drainSleep = func(time.Duration) {}

	var store *roomcluster.Store
	reg := roomsrv.NewRegistry(roomsrv.Options{
		Broadcasts: hubBroadcastsAdapter{r}, Obfuscate: r.ObfuscateID, Log: discardLog, EmptyGrace: time.Hour, PodName: podName,
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
	go store.Run(ctx)
	waitFor(t, 15*time.Second, store.HasSynced, podName+" room informer sync")

	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	return &roomPod{name: podName, port: port, srv: srv, hub: r, reg: reg, store: store, done: done}
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
	case <-a.done:
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
