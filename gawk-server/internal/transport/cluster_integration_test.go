// R17 W3 (docs/22 Decision 8): lease lifecycle == broadcast lifecycle,
// exercised through the real transport server wired to a real Coordinator on
// a fake clientset. Publish claims the origin Lease; the SIGTERM drain
// releases holdership (the broadcaster's 0 ms reconnect then claims it on a
// ready pod); the cluster-wide MaxBroadcasts binds at Lease-create.
package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/wire"

	"github.com/prometheus/client_golang/prometheus"
)

// clusteredPod is one in-process "relay pod": a real transport.Server wired
// to a real Coordinator over the SHARED fake clientset — the in-process twin
// of the kind/k3s smoke.
type clusteredPod struct {
	name     string
	port     int
	registry *hub.Registry
	coord    *cluster.Coordinator
	srv      *Server
	done     chan error
}

func (p *clusteredPod) addr() string { return fmt.Sprintf("127.0.0.1:%d", p.port) }

// startClusteredPod boots one pod. All pods share cert+pool (standing in for
// the fleet's one public certificate) and the fake clientset (the control
// plane). The internal PSK is fixed: "fleet-psk".
func startClusteredPod(t *testing.T, ctx context.Context, cs *fake.Clientset, podName string, cert tls.Certificate, pool *x509.CertPool, clusterMax int) *clusteredPod {
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
		// Fleet requirement (docs/22 Decision 7): resume tokens must verify
		// on EVERY pod, or re-homing 403s — share the key like the chart's
		// Secret does.
		ResumeTokenKey: bytes.Repeat([]byte{0x42}, 32),
	}
	// The lease callbacks close over the late-bound server, exactly like
	// main.go's wiring (the coordinator and server reference each other).
	var srv *Server
	coord, err := cluster.New(cluster.Options{
		Client:         cs,
		Namespace:      "gawk",
		PodName:        podName,
		AdvertiseAddr:  cfg.Addr,
		BroadcastGrace: cfg.BroadcastGrace,
		MaxBroadcasts:  clusterMax,
		Log:            discardLog,
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

	r := hub.NewRegistry(discardLog, hub.Options{
		MaxSubscribers: cfg.MaxSubscribers,
		BroadcastGrace: cfg.BroadcastGrace,
	})
	srv = New(cfg, r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }, discardLog,
		metrics.NewServerMetrics(prometheus.NewRegistry()))
	srv.drainSleep = func(time.Duration) {} // drain instantly in tests
	srv.SetCluster(coord, podName)
	// The production dialer trusts the system roots; the test fleet's cert is
	// self-signed, so re-point the dialer at the shared pool.
	srv.edges.dial = newEdgeDialer(cfg.InternalServerName, cfg.InternalPSK, pool, discardLog)

	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	return &clusteredPod{name: podName, port: port, registry: r, coord: coord, srv: srv, done: done}
}

// startClusteredTestServer keeps the original single-pod shape for the W3
// lifecycle tests.
func startClusteredTestServer(t *testing.T, ctx context.Context, cs *fake.Clientset, clusterMax int) (port int, clientTLS *tls.Config, done chan error) {
	t.Helper()
	cert, err := tlsutil.GenerateDevCert([]string{"localhost", "127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatalf("GenerateDevCert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	pod := startClusteredPod(t, ctx, cs, "pod-test", cert, pool, clusterMax)
	clientTLS = &tls.Config{RootCAs: pool, ServerName: "localhost", NextProtos: []string{"h3"}}
	return pod.port, clientTLS, pod.done
}

func TestPublishClaimsLeaseAndDrainReleasesIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srvCtx, srvCancel := context.WithCancel(ctx)
	cs := fake.NewClientset()
	port, clientTLS, done := startClusteredTestServer(t, srvCtx, cs, 0)

	pub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
	id, _ := readPublisherHandshake(t, ctx, pub)

	// Publish created the origin Lease with this pod as holder.
	lease, err := cs.CoordinationV1().Leases("gawk").Get(ctx, "gawk-bc-"+strings.ToLower(id), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("lease for %s not created: %v", id, err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "pod-test" {
		t.Fatalf("lease holder = %v, want pod-test", lease.Spec.HolderIdentity)
	}

	// Drain (ctx cancel = SIGTERM): 4002 to the session, holdership released
	// — the lease survives with an empty holder for the instant re-claim.
	srvCancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not drain")
	}
	lease, err = cs.CoordinationV1().Leases("gawk").Get(context.Background(), "gawk-bc-"+strings.ToLower(id), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("lease gone after drain: %v", err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "" {
		t.Errorf("lease holder after drain = %v, want empty (released)", lease.Spec.HolderIdentity)
	}
}

// The cluster-wide MaxBroadcasts binds at Lease-create: a mint that passes
// the per-pod registry limit still gets 429-closed when the cluster is full.
func TestMintRejectedAtClusterBroadcastLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cs := fake.NewClientset()
	port, clientTLS, _ := startClusteredTestServer(t, ctx, cs, 1)

	pubA := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
	defer pubA.CloseWithError(0, "")
	readPublisherHandshake(t, ctx, pubA)

	// Second mint: the local registry (default MaxBroadcasts 5) allows it,
	// the cluster (1) does not — the upgraded session is closed with 429.
	pubB := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
	acceptCtx, acceptCancel := context.WithTimeout(ctx, 5*time.Second)
	_, err := pubB.AcceptUniStream(acceptCtx)
	acceptCancel()
	if err == nil {
		t.Fatal("second mint got a handshake despite the cluster broadcast limit")
	}
	var se *webtransport.SessionError
	if !errors.As(err, &se) || se.ErrorCode != 429 {
		t.Fatalf("second mint close = %v, want SessionError 429", err)
	}

	// Exactly one lease exists.
	list, err := cs.CoordinationV1().Leases("gawk").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Errorf("leases after rejected mint = %d, want 1", len(list.Items))
	}
}

// The W4 headline (in-process twin of the kind/k3s two-pod smoke, plus the
// depth bound): a publisher on pod A, viewers on pods B and C. Both pods
// edge-pull from A (never from each other), the viewer is join-primed with a
// byte-identical keyframe, live deltas flow through, and A's /statusz counts
// edge sessions — not viewers.
func TestMultiPodEdgePullE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs := fake.NewClientset()

	cert, err := tlsutil.GenerateDevCert([]string{"localhost", "127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatalf("GenerateDevCert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	clientTLS := &tls.Config{RootCAs: pool, ServerName: "localhost", NextProtos: []string{"h3"}}

	podA := startClusteredPod(t, ctx, cs, "pod-a", cert, pool, 0)
	podB := startClusteredPod(t, ctx, cs, "pod-b", cert, pool, 0)
	podC := startClusteredPod(t, ctx, cs, "pod-c", cert, pool, 0)

	// Publisher lands on A; the lease names A as origin.
	pub := dial(t, ctx, fmt.Sprintf("https://%s/publish", podA.addr()), clientTLS)
	id, _ := readPublisherHandshake(t, ctx, pub)

	kf := buildStreamKeyframe(t, 0, "avc1.42E02A", 3000)
	sendKeyframeStream(t, pub, kf)
	waitFor(t, 5*time.Second, func() bool {
		return podA.registry.Stats().Broadcasts[podA.registry.ObfuscateID(id)].CachedKeyframeBytes == len(kf)
	}, "keyframe cached on origin")

	// Viewer lands on B: demand-creates the edge pull and gets join-primed
	// with the byte-identical keyframe.
	viewerB := dialSubscriber(t, ctx, podB.port, id, clientTLS)
	recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
	hdrB, dataB := readNextKeyframeStream(t, recvCtx, viewerB)
	recvCancel()
	if hdrB.FrameID != 0 || !bytes.Equal(dataB, kf) {
		t.Fatalf("edge viewer prime: frameID %d, byte-identical=%v", hdrB.FrameID, bytes.Equal(dataB, kf))
	}

	// Viewer on C too — C pulls from A directly (the lease names A).
	viewerC := dialSubscriber(t, ctx, podC.port, id, clientTLS)
	recvCtx, recvCancel = context.WithTimeout(ctx, 10*time.Second)
	hdrC, dataC := readNextKeyframeStream(t, recvCtx, viewerC)
	recvCancel()
	if hdrC.FrameID != 0 || !bytes.Equal(dataC, kf) {
		t.Fatalf("pod-c viewer prime: frameID %d, byte-identical=%v", hdrC.FrameID, bytes.Equal(dataC, kf))
	}

	// A live delta flows publisher → A → B-edge → viewer, byte-identical.
	delta := encodeFrame(t, 1, false, 1)[0]
	gotDelta := make(chan []byte, 1)
	go func() {
		for {
			d, err := viewerB.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			if len(d) >= 2 && d[1] == wire.TypeVideoChunk {
				gotDelta <- d
				return
			}
		}
	}()
	// Datagrams are lossy even on loopback queues — resend until seen.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := pub.SendDatagram(delta); err != nil {
			t.Fatalf("SendDatagram: %v", err)
		}
		select {
		case d := <-gotDelta:
			if !bytes.Equal(d, delta) {
				t.Fatal("delta not byte-identical across the edge hop")
			}
			goto deltaDone
		case <-time.After(200 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("delta never reached the edge viewer")
			}
		}
	}
deltaDone:

	// Accounting (docs/22 Decisions 10/14): the origin counts TWO edge
	// sessions and ZERO viewers; B's hub is role=edge with ONE viewer.
	waitFor(t, 5*time.Second, func() bool {
		st := podA.registry.Stats().Broadcasts[podA.registry.ObfuscateID(id)]
		return st.EdgeSessions == 2 && st.Subscribers == 0 && st.Role == "origin"
	}, "origin accounting (2 edges, 0 viewers)")
	stB := podB.registry.Stats().Broadcasts[podB.registry.ObfuscateID(id)]
	if stB.Role != "edge" || stB.Subscribers != 1 {
		t.Errorf("pod-b hub = role %q, %d subscribers; want edge/1", stB.Role, stB.Subscribers)
	}

	// Depth bound (guard 2): B is an edge, not the origin — another pod
	// asking B for the broadcast gets 404 regardless of credentials, so an
	// edge can never feed another edge.
	rsp, sess, err := dialOnce(t, ctx,
		fmt.Sprintf("https://%s/internal/subscribe/%s?psk=fleet-psk&gen=1&proto=%d", podB.addr(), id, wire.Version), clientTLS)
	if err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("edge pod served an internal subscribe (depth > 2 possible)")
	}
	if rsp != nil && rsp.StatusCode != http.StatusNotFound {
		t.Errorf("edge internal-subscribe status = %d, want 404", rsp.StatusCode)
	}

	// And the origin itself serves it only at the CURRENT generation.
	rsp, sess, err = dialOnce(t, ctx,
		fmt.Sprintf("https://%s/internal/subscribe/%s?psk=fleet-psk&gen=99&proto=%d", podA.addr(), id, wire.Version), clientTLS)
	if err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("origin served a stale-generation internal subscribe")
	}
	if rsp != nil && rsp.StatusCode != http.StatusConflict {
		t.Errorf("stale-gen internal-subscribe status = %d, want 409", rsp.StatusCode)
	}
}
