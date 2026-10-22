package popsrv

// The in-process PoP harness (contract §5): a home fleet of real transport
// servers, each wired to a real cluster.Coordinator and a real Pop
// informer over SHARED fake clientsets, plus one PoP — a relay with its own
// hub and transport server and a popclient whose dials go through a fake
// load balancer to whichever home pod the test routes to. Wave 3 tests
// reuse it; a package that needs more adds a sibling helper of its own.
//
// It lives in a non-test file so other packages' tests can import it. It
// never runs in production: nothing calls StartHomeAndPop outside tests,
// and the fake clientsets it links add ~100 KB to the binaries.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	coordv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientfeatures "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/quic-go/quic-go/http3"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/popclient"
	"github.com/Tuhis/gawk/gawk-server/internal/popsrc"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// HarnessNamespace is the harness fleet's namespace.
const HarnessNamespace = "gawk"

// HarnessEndpoint is the home endpoint pairing strings name; the fake load
// balancer maps it to a pod.
const HarnessEndpoint = "home.gawk.test:4433"

// HarnessPSK is the fleet's internal PSK.
const HarnessPSK = "fleet-psk"

// HarnessHomeOrigin is the home app origin the fleet tells its PoPs.
const HarnessHomeOrigin = "https://gawk.example"

// HomePod is one in-process home relay pod.
type HomePod struct {
	Name     string
	Port     int
	Registry *hub.Registry
	Coord    *cluster.Coordinator
	Server   *transport.Server
	Manager  *Manager
	Source   popsrc.Source
	Metrics  *metrics.ServerMetrics
	cancel   context.CancelFunc
	done     chan struct{}
	stopped  atomic.Bool
}

// Addr is the pod's UDP address.
func (p *HomePod) Addr() string { return fmt.Sprintf("127.0.0.1:%d", p.Port) }

// Drain stops the pod as SIGTERM does (4002 to every session, holderships
// released) and waits for it to exit.
func (p *HomePod) Drain(t testing.TB) {
	t.Helper()
	p.stopped.Store(true)
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("home pod %s did not drain", p.Name)
	}
}

// HomeFleet is the home side of the harness.
type HomeFleet struct {
	Pods []*HomePod
	// Leases is the coordination fake (broadcast Leases); Pops the dynamic
	// fake holding the Pop CRs.
	Leases *fake.Clientset
	Pops   *dynamicfake.FakeDynamicClient
	Cert   tls.Certificate
	Pool   *x509.CertPool
	// ClientTLS dials any home pod (or the PoP) as a test client.
	ClientTLS *tls.Config

	route atomic.Int64 // the fake load balancer's next pod
	cfg   func(*config.Config)
}

// Route makes the fake load balancer send the next dials to pod i (it
// skips drained pods).
func (f *HomeFleet) Route(i int) { f.route.Store(int64(i)) }

// target is the pod the load balancer picks.
func (f *HomeFleet) target() *HomePod {
	start := int(f.route.Load())
	for k := range f.Pods {
		p := f.Pods[(start+k)%len(f.Pods)]
		if !p.stopped.Load() {
			return p
		}
	}
	return nil
}

// PoP is the party side of the harness.
type PoP struct {
	Port     int
	Registry *hub.Registry
	Server   *transport.Server
	Client   *popclient.Client
	// Rooms is the PoP's own room registry (the relay profile runs -rooms).
	Rooms    *roomsrv.Registry
	StateDir string
	// Dialed records the home pod each link dial went to.
	Dialed func() []string
	cancel context.CancelFunc
	done   chan struct{}
}

// Addr is the PoP's UDP address.
func (p *PoP) Addr() string { return fmt.Sprintf("127.0.0.1:%d", p.Port) }

// Stop ends the PoP.
func (p *PoP) Stop(t testing.TB) {
	t.Helper()
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the PoP did not stop")
	}
}

// HarnessOption adjusts the harness before it starts.
type HarnessOption func(*harnessOpts)

type harnessOpts struct {
	home  func(*config.Config)
	pop   func(*config.Config)
	popO  func(*popclient.Options)
	homeO func(*Options)
	pops  []*pops.Pop
}

// WithHomeConfig adjusts every home pod's config.
func WithHomeConfig(fn func(*config.Config)) HarnessOption {
	return func(o *harnessOpts) { o.home = fn }
}

// WithPopConfig adjusts the PoP relay's config.
func WithPopConfig(fn func(*config.Config)) HarnessOption {
	return func(o *harnessOpts) { o.pop = fn }
}

// WithPopClient adjusts the PoP's popclient options.
func WithPopClient(fn func(*popclient.Options)) HarnessOption {
	return func(o *harnessOpts) { o.popO = fn }
}

// WithManager adjusts every home pod's Manager options.
func WithManager(fn func(*Options)) HarnessOption {
	return func(o *harnessOpts) { o.homeO = fn }
}

// WithPops seeds Pop CRs before the informers start.
func WithPops(ps ...*pops.Pop) HarnessOption {
	return func(o *harnessOpts) { o.pops = append(o.pops, ps...) }
}

// StartHomeAndPop boots n home pods (cluster mode, -pops, -rooms, the
// docs/22 finding-11 Lease-name reactor and resourceVersion CAS on the
// Pop CRs) and one PoP relay that is not yet paired. Everything stops at
// test cleanup.
func StartHomeAndPop(t testing.TB, n int, opts ...HarnessOption) (*HomeFleet, *PoP) {
	t.Helper()
	var ho harnessOpts
	for _, o := range opts {
		o(&ho)
	}
	// client-go defaults WatchListClient on; the fake tracker speaks plain
	// LIST+WATCH (popsrc's k8s tests explain).
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.WatchListClient, false)

	cert, err := tlsutil.GenerateDevCert([]string{"localhost", "127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatalf("GenerateDevCert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	f := &HomeFleet{
		Leases:    fake.NewClientset(),
		Cert:      cert,
		Pool:      pool,
		ClientTLS: &tls.Config{RootCAs: pool, ServerName: "localhost", NextProtos: []string{http3.NextProtoH3}},
	}
	f.Leases.PrependReactor("create", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		lease := action.(k8stesting.CreateAction).GetObject().(*coordv1.Lease)
		if errs := validation.IsDNS1123Subdomain(lease.Name); len(errs) > 0 {
			return true, nil, apierrors.NewInvalid(schema.GroupKind{Group: "coordination.k8s.io", Kind: "Lease"}, lease.Name, nil)
		}
		return false, nil, nil
	})
	var seed []runtime.Object
	for _, p := range ho.pops {
		seed = append(seed, PopObject(p))
	}
	f.Pops = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{pops.GroupVersionResource: pops.ListKind}, seed...)
	installPopCAS(f.Pops)
	watchers := make(chan struct{}, n)
	f.Pops.PrependWatchReactor("pops", func(action k8stesting.Action) (bool, watch.Interface, error) {
		w, err := f.Pops.Tracker().Watch(pops.GroupVersionResource, action.GetNamespace())
		if err != nil {
			return false, nil, err
		}
		select {
		case watchers <- struct{}{}:
		default:
		}
		return true, w, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	log := harnessLog(t)
	for i := range n {
		f.Pods = append(f.Pods, f.startPod(t, ctx, fmt.Sprintf("home-%d", i), log, ho))
	}
	for i := range n {
		select {
		case <-watchers:
		case <-time.After(5 * time.Second):
			t.Fatalf("pop informer %d never registered its watch", i)
		}
	}
	for _, p := range f.Pods {
		WaitFor(t, "pop informer sync on "+p.Name, p.Source.Synced)
	}
	pop := f.startPoP(t, ctx, log, ho)
	return f, pop
}

func (f *HomeFleet) startPod(t testing.TB, parent context.Context, name string, log *slog.Logger, ho harnessOpts) *HomePod {
	t.Helper()
	port := freeUDPPort(t)
	cfg := config.Config{
		Addr:               fmt.Sprintf("127.0.0.1:%d", port),
		MaxSubscribers:     8,
		MaxBroadcasts:      50,
		MaxIdleTimeout:     30 * time.Second,
		KeepAlivePeriod:    10 * time.Second,
		BroadcastGrace:     5 * time.Minute,
		ClusterMode:        true,
		InternalPSK:        HarnessPSK,
		InternalServerName: "localhost",
		ResumeTokenKey:     bytes.Repeat([]byte{0x42}, 32),
		Pops:               true,
		Rooms:              true,
		AllowedOrigins:     []string{HarnessHomeOrigin},
	}
	if ho.home != nil {
		ho.home(&cfg)
	}
	ctx, cancel := context.WithCancel(parent)
	var srv *transport.Server
	coord, err := cluster.New(cluster.Options{
		Client:         f.Leases,
		Namespace:      HarnessNamespace,
		PodName:        name,
		AdvertiseAddr:  cfg.Addr,
		BroadcastGrace: cfg.BroadcastGrace,
		MaxBroadcasts:  cfg.MaxBroadcasts,
		Log:            log,
		OnLeaseDeleted: func(id string) {
			if srv != nil {
				srv.HandleLeaseDeleted(id)
			}
		},
		OnLeaseLost: func(id string, o cluster.Origin) {
			if srv != nil {
				srv.HandleLeaseLost(id, o)
			}
		},
	})
	if err != nil {
		t.Fatalf("cluster.New: %v", err)
	}
	// The cluster-mode lease hooks serve.Run installs: a publisher that
	// leaves graces its Lease, an expired hub deletes it.
	leaseOp := func(fn func(ctx context.Context) error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := fn(ctx); err != nil {
			log.Warn("harness lease hook failed", "err", err)
		}
	}
	r := hub.NewRegistry(log, hub.Options{
		MaxSubscribers: cfg.MaxSubscribers,
		MaxBroadcasts:  cfg.MaxBroadcasts,
		BroadcastGrace: cfg.BroadcastGrace,
		OnPublisherClosed: func(id string) {
			leaseOp(func(ctx context.Context) error { return coord.EnterGrace(ctx, id) })
		},
		OnBroadcastExpired: func(id string) {
			leaseOp(func(ctx context.Context) error { return coord.Delete(ctx, id) })
		},
		OnPublisherStalled: func(id string, stalled bool) {
			leaseOp(func(ctx context.Context) error { return coord.SetStalled(ctx, id, stalled) })
		},
	})
	sm := metrics.NewServerMetrics(prometheus.NewRegistry())
	cert := f.Cert
	srv = transport.New(cfg, r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }, log, sm)
	srv.SetCluster(coord, name)
	srv.SetInternalRootCAs(f.Pool)
	go coord.Run(ctx)

	src, err := popsrc.StartK8s(ctx, f.Pops, HarnessNamespace, log)
	if err != nil {
		t.Fatal(err)
	}
	mo := Options{
		Pod:             name,
		PodAddr:         cfg.Addr,
		Source:          src,
		Status:          popsrc.NewK8sStatus(f.Pops, HarnessNamespace),
		Coord:           coord,
		Registry:        r,
		HomeOrigin:      HarnessHomeOrigin,
		HomeRooms:       cfg.Rooms,
		ScreeningNotice: cfg.ScreeningNotice,
		ClusterPSK:      cfg.InternalPSK,
		Log:             log,
		LeaseRenew:      200 * time.Millisecond,
		ByeGrace:        500 * time.Millisecond,
	}
	if ho.homeO != nil {
		ho.homeO(&mo)
	}
	m := New(mo)
	srv.SetPopHome(m)
	done := make(chan struct{})
	go func() { _ = srv.Run(ctx); close(done) }()
	pod := &HomePod{Name: name, Port: port, Registry: r, Coord: coord, Server: srv, Manager: m, Source: src, Metrics: sm, cancel: cancel, done: done}
	t.Cleanup(func() {
		pod.stopped.Store(true)
		cancel()
		<-done
		m.Close()
	})
	return pod
}

func (f *HomeFleet) startPoP(t testing.TB, parent context.Context, log *slog.Logger, ho harnessOpts) *PoP {
	t.Helper()
	port := freeUDPPort(t)
	cfg := config.Config{
		Addr:            fmt.Sprintf("127.0.0.1:%d", port),
		MaxSubscribers:  8,
		MaxBroadcasts:   50,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace:  5 * time.Minute,
		Rooms:           true,
		MaxRooms:        8,
		ServerName:      "Party relay",
	}
	if ho.pop != nil {
		ho.pop(&cfg)
	}
	ctx, cancel := context.WithCancel(parent)
	r := hub.NewRegistry(log, hub.Options{MaxSubscribers: cfg.MaxSubscribers, MaxBroadcasts: cfg.MaxBroadcasts, BroadcastGrace: cfg.BroadcastGrace})
	cert := f.Cert
	srv := transport.New(cfg, r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }, log,
		metrics.NewServerMetrics(prometheus.NewRegistry()))
	var rooms *roomsrv.Registry
	if cfg.Rooms {
		rooms = roomsrv.NewRegistry(roomsrv.Options{
			Broadcasts: srv.RoomBroadcasts(), Obfuscate: r.ObfuscateID, Log: log,
			MaxRooms: cfg.MaxRooms, EmptyGrace: time.Hour,
		})
		srv.SetRooms(rooms)
	}
	var dmu sync.Mutex
	var dialed []string
	base := popclient.Dialer(f.Pool)
	stateDir := t.TempDir()
	po := popclient.Options{
		StateDir: stateDir,
		Version:  "test",
		Registry: r,
		Server:   srv,
		Cfg:      cfg,
		Log:      log,
		Dial: func(ctx context.Context, network, target string, h http.Header) (popclient.Session, *http.Response, error) {
			pod := f.target()
			if pod == nil {
				return nil, nil, errors.New("harness: no home pod is up")
			}
			if strings.Contains(target, "/pop/link") {
				dmu.Lock()
				dialed = append(dialed, pod.Name)
				dmu.Unlock()
			}
			return base(ctx, network, strings.Replace(target, HarnessEndpoint, pod.Addr(), 1), h)
		},
		RetryMin:      50 * time.Millisecond,
		RetryMax:      500 * time.Millisecond,
		TerminalRetry: 500 * time.Millisecond,
		StatsEvery:    200 * time.Millisecond,
	}
	if ho.popO != nil {
		ho.popO(&po)
	}
	c := popclient.New(po)
	srv.SetLocalRelay(c)
	done := make(chan struct{})
	clientDone := make(chan struct{})
	go func() { _ = srv.Run(ctx); <-clientDone; close(done) }()
	go func() { _ = c.Run(ctx); close(clientDone) }()
	p := &PoP{Port: port, Registry: r, Server: srv, Client: c, Rooms: rooms, StateDir: stateDir, cancel: cancel, done: done,
		Dialed: func() []string {
			dmu.Lock()
			defer dmu.Unlock()
			return append([]string(nil), dialed...)
		}}
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return p
}

// CreatePop creates a Pop CR with a fresh pairing string (expiring in ttl)
// and returns that string, as gawk-admin's wizard shows it.
func (f *HomeFleet) CreatePop(t testing.TB, name, displayName string, ttl time.Duration) string {
	t.Helper()
	token, err := pops.NewPairingToken()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	issued, exp := metav1.NewTime(now), metav1.NewTime(now.Add(ttl))
	p := &pops.Pop{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(fmt.Sprintf("6f1c2d3e-4a5b-4c6d-8e7f-%012x", uidSeq.Add(1)))},
		Spec: pops.PopSpec{
			DisplayName: displayName,
			Pairing:     pops.Pairing{TokenSHA256: pops.TokenSHA256(token), IssuedAt: &issued, ExpiresAt: &exp},
		},
	}
	if _, err := f.Pops.Resource(pops.GroupVersionResource).Namespace(HarnessNamespace).Create(
		context.Background(), PopObject(p), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pop %s: %v", name, err)
	}
	f.waitAllSee(t, name, func(p *pops.Pop) bool { return p.Spec.Pairing.TokenSHA256 == pops.TokenSHA256(token) })
	return pops.FormatPairing(HarnessEndpoint, token)
}

// GetPop reads a Pop CR from the fake API.
func (f *HomeFleet) GetPop(t testing.TB, name string) *pops.Pop {
	t.Helper()
	u, err := f.Pops.Resource(pops.GroupVersionResource).Namespace(HarnessNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pop %s: %v", name, err)
	}
	var p pops.Pop
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

// UpdateSpec applies fn to the Pop's spec as gawk-admin would (a
// read-modify-write of the main resource; status is left as read) and
// waits until every pod's informer has it.
func (f *HomeFleet) UpdateSpec(t testing.TB, name string, fn func(*pops.PopSpec)) {
	t.Helper()
	p := f.GetPop(t, name)
	fn(&p.Spec)
	var want pops.PopSpec
	p.Spec.DeepCopyInto(&want)
	if _, err := f.Pops.Resource(pops.GroupVersionResource).Namespace(HarnessNamespace).Update(
		context.Background(), PopObject(p), metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pop %s: %v", name, err)
	}
	f.waitAllSee(t, name, func(p *pops.Pop) bool {
		return p.Spec.Revoked == want.Revoked && p.Spec.Pairing.TokenSHA256 == want.Pairing.TokenSHA256 &&
			p.Spec.DisplayName == want.DisplayName
	})
}

// RePair writes a new pairing string (gawk-admin's Re-pair) and returns it.
func (f *HomeFleet) RePair(t testing.TB, name string, ttl time.Duration) string {
	t.Helper()
	token, err := pops.NewPairingToken()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	issued, exp := metav1.NewTime(now), metav1.NewTime(now.Add(ttl))
	f.UpdateSpec(t, name, func(s *pops.PopSpec) {
		s.Pairing = pops.Pairing{TokenSHA256: pops.TokenSHA256(token), IssuedAt: &issued, ExpiresAt: &exp}
	})
	return pops.FormatPairing(HarnessEndpoint, token)
}

func (f *HomeFleet) waitAllSee(t testing.TB, name string, ok func(*pops.Pop) bool) {
	t.Helper()
	for _, pod := range f.Pods {
		WaitFor(t, "pod "+pod.Name+" sees pop "+name, func() bool {
			p, found := pod.Source.Get(name)
			return found && ok(p)
		})
	}
}

// LinkHolder is the pod that serves the PoP's link now (nil: none).
func (f *HomeFleet) LinkHolder(name string) *HomePod {
	for _, p := range f.Pods {
		if !p.stopped.Load() && p.Manager.local(name) != nil {
			return p
		}
	}
	return nil
}

var uidSeq atomic.Uint64

// PopObject converts a Pop to the unstructured shape the dynamic fake holds.
func PopObject(p *pops.Pop) *unstructured.Unstructured {
	p = p.DeepCopy()
	p.TypeMeta = metav1.TypeMeta{APIVersion: pops.SchemeGroupVersion.String(), Kind: pops.Kind}
	if p.Namespace == "" {
		p.Namespace = HarnessNamespace
	}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(p)
	if err != nil {
		panic(err)
	}
	return &unstructured.Unstructured{Object: raw}
}

// installPopCAS gives the fake tracker resourceVersion semantics for Pops:
// an update must carry the stored version or it is a Conflict, and a
// successful write bumps it.
func installPopCAS(client *dynamicfake.FakeDynamicClient) {
	var mu sync.Mutex
	client.PrependReactor("update", "pops", func(action k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		obj := action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured)
		cur, err := client.Tracker().Get(pops.GroupVersionResource, action.GetNamespace(), obj.GetName())
		if err != nil {
			return true, nil, err
		}
		rv := cur.(*unstructured.Unstructured).GetResourceVersion()
		if obj.GetResourceVersion() != rv {
			return true, nil, apierrors.NewConflict(pops.GroupVersionResource.GroupResource(), obj.GetName(), errors.New("resourceVersion mismatch"))
		}
		n, _ := strconv.Atoi(rv)
		obj.SetResourceVersion(strconv.Itoa(n + 1))
		if action.GetSubresource() == "status" {
			// A status write leaves the spec as stored; the main resource
			// leaves the status (the subresource contract).
			obj.Object["spec"] = cur.(*unstructured.Unstructured).Object["spec"]
		} else {
			obj.Object["status"] = cur.(*unstructured.Unstructured).Object["status"]
		}
		return false, nil, nil
	})
}

// WaitFor polls cond for up to 10 s.
func WaitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func freeUDPPort(t testing.TB) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

// harnessLog logs to the test with -v, and drops what straggling
// goroutines log once the test is over (t.Log would panic then).
func harnessLog(t testing.TB) *slog.Logger {
	if !testing.Verbose() {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	w := &testWriter{t: t}
	t.Cleanup(func() { w.done.Store(true) })
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct {
	t    testing.TB
	done atomic.Bool
}

func (w *testWriter) Write(b []byte) (int, error) {
	if !w.done.Load() {
		w.t.Log(strings.TrimRight(string(b), "\n"))
	}
	return len(b), nil
}
