// R74 (docs/76 items 42, 45): LocalRelayInfo (0x18) and LocalRelayOffer
// (0x19) on every client-facing session kind — /echo, /publish, a viewer's
// /subscribe and /room — and never on a stripe leg, /internal/* or /pop/*.
package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// fakeLocalRelay is a PoP's LocalRelayProvider: it records the publish IDs
// it is asked about and reports a broadcast on the wall when it has one.
type fakeLocalRelay struct {
	mu  sync.Mutex
	ids []string
}

func (f *fakeLocalRelay) LocalRelayInfo(publishID string) wire.LocalRelayInfo {
	f.mu.Lock()
	f.ids = append(f.ids, publishID)
	f.mu.Unlock()
	return wire.LocalRelayInfo{
		Linked:     true,
		OnWall:     publishID != "",
		Name:       "Assembly 2026",
		HomeOrigin: "https://gawk.ioio.fi",
		WallCode:   "assembly-2026",
	}
}

func (f *fakeLocalRelay) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ids)
}

// fakeOfferer is a home's Offerer: it offers to everyone (or no one) and
// records what it was asked.
type fakeOfferer struct {
	offer bool
	mu    sync.Mutex
	asked []offerAsk
}

type offerAsk struct {
	remote netip.Addr
	lr     bool
}

func testOffer() wire.LocalRelayOffer {
	return wire.LocalRelayOffer{
		Handle:     "6f1c2d3e-4a5b-4c6d-8e7f-0123456789ab",
		Name:       "Assembly 2026",
		Validity:   3600,
		Candidates: []wire.LocalRelayCandidate{{URL: "https://192.168.1.20:4433", Hashes: [][32]byte{{1}}}},
	}
}

func (f *fakeOfferer) Offer(remote netip.Addr, lr bool) (wire.LocalRelayOffer, bool) {
	f.mu.Lock()
	f.asked = append(f.asked, offerAsk{remote, lr})
	f.mu.Unlock()
	if !f.offer {
		return wire.LocalRelayOffer{}, false
	}
	return testOffer(), true
}

func (f *fakeOfferer) asks() []offerAsk {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

// ofType returns every message of type typ.
func ofType(msgs [][]byte, typ uint8) [][]byte {
	var out [][]byte
	for _, m := range msgs {
		if len(m) >= 2 && m[0] == wire.Version && m[1] == typ {
			out = append(out, m)
		}
	}
	return out
}

func oneLocalRelayInfo(t *testing.T, who string, msgs [][]byte) wire.LocalRelayInfo {
	t.Helper()
	got := ofType(msgs, wire.TypeLocalRelayInfo)
	if len(got) != 1 {
		t.Fatalf("%s: got %d LocalRelayInfo streams, want exactly 1 (of %d)", who, len(got), len(msgs))
	}
	info, err := wire.ParseLocalRelayInfo(got[0])
	if err != nil {
		t.Fatalf("%s: ParseLocalRelayInfo: %v", who, err)
	}
	return info
}

func oneLocalRelayOffer(t *testing.T, who string, msgs [][]byte) wire.LocalRelayOffer {
	t.Helper()
	got := ofType(msgs, wire.TypeLocalRelayOffer)
	if len(got) != 1 {
		t.Fatalf("%s: got %d LocalRelayOffer streams, want exactly 1 (of %d)", who, len(got), len(msgs))
	}
	o, err := wire.ParseLocalRelayOffer(got[0])
	if err != nil {
		t.Fatalf("%s: ParseLocalRelayOffer: %v", who, err)
	}
	return o
}

// A PoP with a provider installed sends one 0x18 on each of the four
// client-facing session kinds; the publish session's is asked with its
// broadcast ID (minted or reclaimed), the others with "".
func TestLocalRelayInfoOnEveryClientSessionKind(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, srv, _ := startRoomServer(t, ctx, nil)
	lr := &fakeLocalRelay{}
	srv.SetLocalRelay(lr)
	base := fmt.Sprintf("https://127.0.0.1:%d", port)

	echo := dial(t, ctx, base+"/echo", clientTLS)
	info := oneLocalRelayInfo(t, "echo", collectUniMessages(t, echo, 1500*time.Millisecond))
	if !info.Linked || info.OnWall || info.Name != "Assembly 2026" || info.HomeOrigin != "https://gawk.ioio.fi" || info.WallCode != "assembly-2026" {
		t.Fatalf("echo info = %+v", info)
	}

	pub := dial(t, ctx, base+"/publish", clientTLS)
	id, token, consumed := readPublisherHandshakeMsgs(t, ctx, pub)
	info = oneLocalRelayInfo(t, "publish", append(consumed, collectUniMessages(t, pub, 1500*time.Millisecond)...))
	if !info.OnWall {
		t.Fatalf("publish info = %+v, want on-wall (asked with the broadcast ID)", info)
	}
	if !slices.Contains(lr.calls(), id) {
		t.Fatalf("provider asked %q, want the minted ID %q among them", lr.calls(), id)
	}

	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	if info := oneLocalRelayInfo(t, "subscribe", collectUniMessages(t, sub, 1500*time.Millisecond)); info.OnWall {
		t.Fatalf("subscribe info = %+v, want no on-wall state", info)
	}

	room := dial(t, ctx, fmt.Sprintf("%s/room/new?broadcast=%s&resume=%s", base, id, token), clientTLS)
	oneLocalRelayInfo(t, "room", collectUniMessages(t, room, 1500*time.Millisecond))

	// A reclaim is a publish session too, asked with the reclaimed ID.
	before := len(lr.calls())
	pub2 := dialPublisherReclaim(t, ctx, port, id, token, clientTLS)
	defer pub2.CloseWithError(0, "")
	waitFor(t, 3*time.Second, func() bool { return len(lr.calls()) > before }, "provider asked for the reclaim")
	if got := lr.calls()[before:]; !slices.Contains(got, id) {
		t.Fatalf("reclaim asked %q, want %q", got, id)
	}
	for _, c := range lr.calls() {
		if c != "" && c != id {
			t.Fatalf("provider asked with %q, want only %q or \"\"", c, id)
		}
	}
}

// Without a provider (every home relay, every relay before R74) nothing new
// is sent.
func TestNoLocalRelayInfoWithoutAProvider(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _ := startTestServer(t, ctx, 5)

	echo := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/echo", port), clientTLS)
	pub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
	id, _, consumed := readPublisherHandshakeMsgs(t, ctx, pub)
	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	for who, msgs := range map[string][][]byte{
		"echo":      collectUniMessages(t, echo, time.Second),
		"publish":   append(consumed, collectUniMessages(t, pub, time.Second)...),
		"subscribe": collectUniMessages(t, sub, time.Second),
	} {
		if n := len(ofType(msgs, wire.TypeLocalRelayInfo)) + len(ofType(msgs, wire.TypeLocalRelayOffer)); n != 0 {
			t.Fatalf("%s: got %d R74 notices with nothing installed", who, n)
		}
	}
}

// Stripe legs carry media only: the primary gets the 0x18, its legs never.
func TestLocalRelayInfoNeverOnStripeLegs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _, srv := startTestServerCfgLogSrv(t, ctx, stripedTestConfig(15), discardLog)
	srv.SetLocalRelay(&fakeLocalRelay{})
	srv.SetOfferer(&fakeOfferer{offer: true})
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")

	base := fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s", port, id)
	const owner = "aabbccdd00112233"
	primary := dial(t, ctx, base+"?owner="+owner, clientTLS)
	defer primary.CloseWithError(0, "")
	leg0 := dial(t, ctx, base+"?stripe=2&leg=0&owner="+owner, clientTLS)
	defer leg0.CloseWithError(0, "")
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 2 }, "primary and leg registered")

	oneLocalRelayInfo(t, "primary", collectUniMessages(t, primary, 1500*time.Millisecond))
	legMsgs := collectUniMessages(t, leg0, 1500*time.Millisecond)
	if n := len(ofType(legMsgs, wire.TypeLocalRelayInfo)) + len(ofType(legMsgs, wire.TypeLocalRelayOffer)); n != 0 {
		t.Fatalf("stripe leg got %d R74 notices, want none", n)
	}
}

// An edge reads every uni stream on /internal/subscribe as a keyframe, so
// neither notice ever goes there.
func TestLocalRelayNoticesNeverOnInternalSubscribe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cs := fake.NewClientset()
	cert, err := tlsutil.GenerateDevCert([]string{"localhost", "127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatalf("GenerateDevCert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	clientTLS := &tls.Config{RootCAs: pool, ServerName: "localhost", NextProtos: []string{"h3"}}

	pod := startClusteredPod(t, ctx, cs, "pod-a", cert, pool, 0)
	pod.srv.SetLocalRelay(&fakeLocalRelay{})
	pod.srv.SetOfferer(&fakeOfferer{offer: true})

	pub := dial(t, ctx, fmt.Sprintf("https://%s/publish", pod.addr()), clientTLS)
	defer pub.CloseWithError(0, "")
	id, _, consumed := readPublisherHandshakeMsgs(t, ctx, pub)
	oneLocalRelayInfo(t, "publisher (fixture check)", append(consumed, collectUniMessages(t, pub, time.Second)...))

	edge := dial(t, ctx, fmt.Sprintf("https://%s/internal/subscribe/%s?psk=fleet-psk&gen=1&proto=%d",
		pod.addr(), id, wire.Version), clientTLS)
	defer edge.CloseWithError(0, "")
	noMoreUniStreams(t, ctx, edge)
}

// The path guard holds even if a later route calls the hook by mistake:
// nothing under /internal/ or /pop/ ever reaches a provider or an offerer.
func TestSessionNoticesGuardInternalAndPopPaths(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, _, _, srv := startTestServerCfgLogSrv(t, ctx, stripedTestConfig(5), discardLog)
	lr := &fakeLocalRelay{}
	of := &fakeOfferer{offer: true}
	srv.SetLocalRelay(lr)
	srv.SetOfferer(of)
	for _, path := range []string{"/internal/subscribe/NSTMWB", "/internal/pop/x", "/pop/publish/NSTMWB", "/pop/subscribe/NSTMWB", "/pop/link"} {
		// GET, because httptest reads a CONNECT target in authority form
		// (no path); the extended CONNECT a relay serves carries the path.
		r := httptest.NewRequest(http.MethodGet, "https://relay.example"+path, nil)
		// A nil session would panic on any send: the guard must return first.
		srv.sendSessionNotices((*webtransport.Session)(nil), r, kindSubscribe)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(lr.calls()) + len(of.asks()); n != 0 {
		t.Fatalf("guarded paths reached the provider/offerer %d times", n)
	}
}

// A home offers on all four client-facing kinds, asking the Offerer with the
// client's address and its lr=1 flag; a refusal sends nothing.
func TestLocalRelayOfferOnEveryClientSessionKind(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, srv, _ := startRoomServer(t, ctx, nil)
	of := &fakeOfferer{offer: true}
	srv.SetOfferer(of)
	base := fmt.Sprintf("https://127.0.0.1:%d", port)

	echo := dial(t, ctx, base+"/echo?lr=1", clientTLS)
	o := oneLocalRelayOffer(t, "echo", collectUniMessages(t, echo, 1500*time.Millisecond))
	if o.Handle != testOffer().Handle || len(o.Candidates) != 1 {
		t.Fatalf("echo offer = %+v", o)
	}
	asks := of.asks()
	if len(asks) != 1 || !asks[0].lr || asks[0].remote != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("offerer asked %+v, want one ask from 127.0.0.1 with lr", asks)
	}

	pub := dial(t, ctx, base+"/publish", clientTLS)
	id, token, consumed := readPublisherHandshakeMsgs(t, ctx, pub)
	oneLocalRelayOffer(t, "publish", append(consumed, collectUniMessages(t, pub, 1500*time.Millisecond)...))
	if asks := of.asks(); asks[len(asks)-1].lr {
		t.Fatalf("publish without lr=1 asked with lr set: %+v", asks)
	}

	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	oneLocalRelayOffer(t, "subscribe", collectUniMessages(t, sub, 1500*time.Millisecond))

	room := dial(t, ctx, fmt.Sprintf("%s/room/new?broadcast=%s&resume=%s", base, id, token), clientTLS)
	oneLocalRelayOffer(t, "room", collectUniMessages(t, room, 1500*time.Millisecond))

	// No match, no offer.
	of2 := &fakeOfferer{offer: false}
	srv.SetOfferer(of2)
	echo2 := dial(t, ctx, base+"/echo", clientTLS)
	if n := len(ofType(collectUniMessages(t, echo2, time.Second), wire.TypeLocalRelayOffer)); n != 0 {
		t.Fatalf("refused offer still sent %d streams", n)
	}
	if len(of2.asks()) != 1 {
		t.Fatalf("offerer asked %d times, want 1", len(of2.asks()))
	}
}

// A PoP's own dials to home (its console probing /echo) carry popOrigin and
// are never offered the PoP itself.
func TestLocalRelayOfferNeverToAPoPsOwnOrigin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _, srv := startTestServerCfgLogSrv(t, ctx, stripedTestConfig(5), discardLog)
	of := &fakeOfferer{offer: true}
	srv.SetOfferer(of)

	echo := dialWithOrigin(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/echo?lr=1", port), clientTLS, popOrigin)
	if n := len(ofType(collectUniMessages(t, echo, time.Second), wire.TypeLocalRelayOffer)); n != 0 {
		t.Fatalf("PoP origin got %d offers, want none", n)
	}
	if len(of.asks()) != 0 {
		t.Fatalf("offerer consulted for a PoP's own session: %+v", of.asks())
	}
}

// Setting nil removes either hook.
func TestSetLocalRelayAndOffererNilRemoves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _, srv := startTestServerCfgLogSrv(t, ctx, stripedTestConfig(5), discardLog)
	lr := &fakeLocalRelay{}
	of := &fakeOfferer{offer: true}
	srv.SetLocalRelay(lr)
	srv.SetOfferer(of)
	srv.SetLocalRelay(nil)
	srv.SetOfferer(nil)
	echo := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/echo", port), clientTLS)
	msgs := collectUniMessages(t, echo, time.Second)
	if n := len(ofType(msgs, wire.TypeLocalRelayInfo)) + len(ofType(msgs, wire.TypeLocalRelayOffer)); n != 0 {
		t.Fatalf("removed hooks still sent %d notices", n)
	}
	if len(lr.calls())+len(of.asks()) != 0 {
		t.Fatal("removed hooks were consulted")
	}
}
