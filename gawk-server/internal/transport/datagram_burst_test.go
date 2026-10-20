// R73 RU2 (docs/75 D7, F7): a burst of datagrams that arrives while the
// relay's datagram loop is not reading must wait in quic-go's per-session
// queue, not be dropped from it. Stock quic-go keeps 32 per WebTransport
// session (http3 streamDatagramQueueLen); a top-rung delta is 52–84
// datagrams, so any scheduling gap in the loop lost a burst's tail on a
// clean path. The relay builds against the vendored quic-go in
// third_party/quic-go, patched to 512.
package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

const (
	// burstDatagrams is RU2's burst: more than twice a top-rung delta, and
	// well past stock quic-go's 32, but inside D7's 512.
	burstDatagrams = 200
	// burstLoopBlock is how long the relay's loop stays blocked once the
	// whole burst has been handed to the publisher's QUIC stack.
	burstLoopBlock = 20 * time.Millisecond
)

// burstFrames is burstDatagrams one-chunk delta frames (frame IDs from 1),
// each about a full chunk, so FramesRelayed counts exactly the burst's
// datagrams that reached a hub.
func burstFrames(t *testing.T) [][]byte {
	t.Helper()
	out := make([][]byte, 0, burstDatagrams)
	for i := range burstDatagrams {
		id := uint32(i + 1)
		d, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{
			FrameID:     id,
			ChunkIndex:  0,
			ChunkCount:  1,
			TimestampUs: uint64(id) * 16_667,
		}, bytes.Repeat([]byte{byte(id)}, 1050))
		if err != nil {
			t.Fatalf("AppendVideoChunk: %v", err)
		}
		out = append(out, d)
	}
	return out
}

// loopBlocker is a datagram-loop test hook that, once armed, blocks the loop
// at its next iteration until released.
type loopBlocker struct {
	once     sync.Once
	blocked  chan struct{}
	released chan struct{}
}

func newLoopBlocker() *loopBlocker {
	return &loopBlocker{blocked: make(chan struct{}), released: make(chan struct{})}
}

func (b *loopBlocker) hook() {
	first := false
	b.once.Do(func() { first = true; close(b.blocked) })
	if first {
		<-b.released
	}
}

// sendBurstWhileBlocked arms the blocker through install, sends the burst at
// loopback speed and keeps the loop blocked for burstLoopBlock once the
// burst is out (the loop blocks as soon as it reads its next datagram,
// which is at the latest the burst's first). inFlight, when set, first waits
// for the burst to be on its way to the blocked loop (the edge: the origin
// has relayed it).
func sendBurstWhileBlocked(t *testing.T, pub *webtransport.Session, install func(*func()), uninstall func(), inFlight func()) {
	t.Helper()
	b := newLoopBlocker()
	hook := b.hook
	install(&hook)
	defer uninstall()
	defer close(b.released)
	for _, d := range burstFrames(t) {
		if err := pub.SendDatagram(d); err != nil {
			t.Fatalf("SendDatagram: %v", err)
		}
	}
	select {
	case <-b.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay's datagram loop never read the burst")
	}
	if inFlight != nil {
		inFlight()
	}
	time.Sleep(burstLoopBlock)
}

func framesRelayed(r *hub.Registry, id string) uint64 {
	return r.Stats().Broadcasts[r.ObfuscateID(id)].FramesRelayed
}

// waitForFrames waits for want frames at r and fails with what arrived.
// Settling is bounded: the burst is on loopback and the loop is unblocked.
func waitForFrames(t *testing.T, r *hub.Registry, id string, want uint64, where string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := framesRelayed(r, id)
		if got == want {
			return
		}
		if got > want || time.Now().After(deadline) {
			t.Fatalf("%s: %d of the burst's %d datagrams reached the hub, want all of them "+
				"(the rest were dropped from quic-go's per-session datagram queue; docs/75 D7)",
				where, got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPublisherBurstSurvivesBlockedDatagramLoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	port, clientTLS, r, _, srv := startTestServerCfgLogSrv(t, ctx, config.Config{
		MaxSubscribers:  4,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace:  5 * time.Minute,
	}, discardLog)
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	waitFor(t, 5*time.Second, func() bool {
		_, ok := r.Stats().Broadcasts[r.ObfuscateID(id)]
		return ok
	}, "the broadcast's hub")

	sendBurstWhileBlocked(t, pub,
		func(h *func()) { srv.testHookPublishDatagramLoop.Store(h) },
		func() { srv.testHookPublishDatagramLoop.Store(nil) }, nil)

	waitForFrames(t, r, id, burstDatagrams, "publisher session")
}

// The edge's pull session reads origin → edge datagrams through the same
// quic-go queue, at pod-to-pod speed (docs/75 D7 "Where it matters").
func TestEdgePullBurstSurvivesBlockedDatagramLoop(t *testing.T) {
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

	origin := startClusteredPod(t, ctx, cs, "pod-a", cert, pool, 0)
	edge := startClusteredPod(t, ctx, cs, "pod-b", cert, pool, 0)

	pub := dial(t, ctx, fmt.Sprintf("https://%s/publish", origin.addr()), clientTLS)
	id, _ := readPublisherHandshake(t, ctx, pub)
	// A viewer on the edge pod demand-creates the pull from the origin.
	_ = dialSubscriber(t, ctx, edge.port, id, clientTLS)
	waitForCascade(t, func() (bool, string) {
		st := edge.registry.Stats().Broadcasts[edge.registry.ObfuscateID(id)]
		return st.Role == "edge", fmt.Sprintf("edge role=%q", st.Role)
	}, "the edge pull")

	em := edge.srv.edgeManager()
	sendBurstWhileBlocked(t, pub,
		func(h *func()) { em.testHookDatagramLoop.Store(h) },
		func() { em.testHookDatagramLoop.Store(nil) },
		// The origin's loop is never blocked: it must pass the whole burst
		// on, or the edge's count says nothing about the edge's queue.
		func() { waitForFrames(t, origin.registry, id, burstDatagrams, "origin publisher session") })

	waitForFrames(t, edge.registry, id, burstDatagrams, "edge pull session")
}
