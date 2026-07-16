// R17 W5 acceptance tests (docs/22 Decision 11): the origin-move / demote
// path — the in-process twin of the split-brain drill. A broadcaster
// re-homing onto pod B force-takes the Lease; pod A kills its stale
// publisher session, 4003-closes downstream edges (which re-resolve to B),
// and self-demotes to an edge for its still-connected local viewers. Depth
// stays ≤ 2 throughout. Plus the W5 units: jittered backoff bounds and the
// trusted-CIDR limiter bypass.
package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/quic-go/webtransport-go"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
)

// The split-brain / origin-move drill, in process: publisher on A with a
// local viewer, a second viewer on C (edge from A); the broadcaster re-homes
// to B with its resume token; every viewer keeps playing from the new origin
// and every role lands where Decision 11 says it must.
func TestOriginMoveDemotesAndReservesViewers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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

	// The informer drives OnLeaseLost in production; wire it here the way
	// main.go does.
	for _, p := range []*clusteredPod{podA, podB, podC} {
		go p.coord.Run(ctx)
	}

	// Broadcast starts on A; viewers land on A (local) and C (edge).
	pubA := dial(t, ctx, fmt.Sprintf("https://%s/publish", podA.addr()), clientTLS)
	id, token := readPublisherHandshake(t, ctx, pubA)
	kf1 := buildStreamKeyframe(t, 0, "avc1.42E02A", 1200)
	sendKeyframeStream(t, pubA, kf1)

	viewerA := dialSubscriber(t, ctx, podA.port, id, clientTLS)
	recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
	if hdr, _ := readNextKeyframeStream(t, recvCtx, viewerA); hdr.FrameID != 0 {
		recvCancel()
		t.Fatalf("viewer A prime frameID = %d, want 0", hdr.FrameID)
	}
	recvCancel()

	viewerC := dialSubscriber(t, ctx, podC.port, id, clientTLS)
	recvCtx, recvCancel = context.WithTimeout(ctx, 10*time.Second)
	if hdr, _ := readNextKeyframeStream(t, recvCtx, viewerC); hdr.FrameID != 0 {
		recvCancel()
		t.Fatalf("viewer C prime frameID = %d, want 0", hdr.FrameID)
	}
	recvCancel()

	// The re-home: the broadcaster's resume claim lands on B and force-takes
	// the Lease (this is what a NAT rebind or a drain reconnect looks like).
	pubB := dial(t, ctx,
		fmt.Sprintf("https://%s/publish/%s?resume=%s", podB.addr(), id, token), clientTLS)
	idB, _ := readPublisherHandshake(t, ctx, pubB)
	if idB != id {
		t.Fatalf("re-home announced %q, want %q", idB, id)
	}

	// A's demote: the stale publisher session is closed by the lease-loss
	// watch (the client already abandoned it — assert it dies).
	acceptCtx, acceptCancel := context.WithTimeout(ctx, 15*time.Second)
	_, err = pubA.AcceptUniStream(acceptCtx)
	acceptCancel()
	if err == nil {
		t.Fatal("stale publisher session on A still alive after lease loss")
	}

	// New-origin keyframe: BOTH viewers receive it byte-identical — viewer A
	// through A's self-demoted edge pull, viewer C through C's re-resolved
	// edge (4003 → re-attach at B).
	kf2 := buildStreamKeyframe(t, 100, "avc1.42E02A", 1500)
	sendKeyframeStream(t, pubB, kf2)
	// Re-send periodically: the re-attaches happen over jittered backoffs and
	// each attach is primed with the newest keyframe.
	stopResend := make(chan struct{})
	go func() {
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopResend:
				return
			case <-tick.C:
				sendKeyframeStream(t, pubB, kf2)
			}
		}
	}()
	defer close(stopResend)

	// Read keyframe streams until the new origin's frameID 100 arrives
	// (stale in-flight primes of kf1 may precede it).
	readUntilKf := func(name string, sess *webtransport.Session) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			recvCtx, recvCancel := context.WithTimeout(ctx, 20*time.Second)
			hdr, data, err := readNextKeyframeStreamNoFatal(recvCtx, sess)
			recvCancel()
			if err != nil {
				t.Fatalf("%s: keyframe read failed post-move: %v", name, err)
			}
			if hdr.FrameID == 100 {
				if !bytes.Equal(data, kf2) {
					t.Fatalf("%s: post-move keyframe not byte-identical", name)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: never received the new origin's keyframe", name)
			}
		}
	}
	readUntilKf("viewer A", viewerA)
	readUntilKf("viewer C", viewerC)

	// Roles (docs/22 Decision 11): B origin, A and C edges; B counts two
	// edge sessions; depth ≤ 2 — A and C both pull from B directly.
	waitFor(t, 10*time.Second, func() bool {
		stB := podB.registry.Stats().Broadcasts[podB.registry.ObfuscateID(id)]
		return stB.Role == "origin" && stB.EdgeSessions == 2
	}, "new origin accounting")
	stA := podA.registry.Stats().Broadcasts[podA.registry.ObfuscateID(id)]
	if stA.Role != "edge" || stA.Subscribers != 1 {
		t.Errorf("pod A post-demote = role %q, %d viewers; want edge/1", stA.Role, stA.Subscribers)
	}
	stC := podC.registry.Stats().Broadcasts[podC.registry.ObfuscateID(id)]
	if stC.Role != "edge" {
		t.Errorf("pod C post-move role = %q, want edge", stC.Role)
	}
}

// W5 unit: the jittered edge re-resolve stays within its bounds and actually
// jitters (herd de-synchronization).
func TestEdgeBackoffJitterBounds(t *testing.T) {
	seen := map[time.Duration]bool{}
	for attempt := 0; attempt < 12; attempt++ {
		for range 20 {
			d := edgeBackoffDuration(attempt)
			if d < edgeRetryBase/2 {
				t.Fatalf("backoff(%d) = %v, below the %v floor", attempt, d, edgeRetryBase/2)
			}
			if d >= edgeRetryCap+edgeRetryBase/2 {
				t.Fatalf("backoff(%d) = %v, above the %v cap", attempt, d, edgeRetryCap+edgeRetryBase/2)
			}
			seen[d] = true
		}
	}
	if len(seen) < 10 {
		t.Errorf("backoff produced only %d distinct delays across 240 draws — not jittered", len(seen))
	}
}

// W5 unit: trusted CIDRs bypass the per-IP limiter; everyone else (and the
// loopback test hook) keeps today's behavior.
func TestRateLimiterTrustedCIDRBypass(t *testing.T) {
	cfg, err := config.ParseFlags([]string{
		"-conn-rate-limit", "1", "-conn-burst-limit", "1",
		"-trusted-cidrs", "10.42.0.0/16,203.0.113.0/24",
		"-dev-cert",
	}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	srv := New(cfg, hub.NewRegistry(discardLog, hub.Options{}), nil, discardLog,
		metrics.NewServerMetrics(prometheus.NewRegistry()))

	req := func(remote string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "https://relay/echo", nil)
		r.RemoteAddr = remote
		return r
	}

	// Trusted: never limited, no matter how fast.
	for i := 0; i < 20; i++ {
		if srv.rateLimited(req("10.42.7.9:1234")) {
			t.Fatalf("trusted node IP rate-limited on attempt %d", i)
		}
		if srv.rateLimited(req("203.0.113.50:999")) {
			t.Fatalf("trusted CIDR IP rate-limited on attempt %d", i)
		}
	}
	// Untrusted: burst 1, then limited.
	if srv.rateLimited(req("198.51.100.1:1000")) {
		t.Fatal("first untrusted attempt limited")
	}
	if !srv.rateLimited(req("198.51.100.1:1000")) {
		t.Fatal("second untrusted attempt not limited")
	}
	// Loopback bypass unchanged.
	for i := 0; i < 5; i++ {
		if srv.rateLimited(req("127.0.0.1:5000")) {
			t.Fatal("loopback rate-limited")
		}
	}
}
