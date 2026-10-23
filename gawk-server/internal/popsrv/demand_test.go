package popsrv

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/popclient"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/popsrc"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/pops"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// A reserve record is answered per ID: ok, ok again for a PoP re-asserting
// its own reservation (a relink), conflict for one a home broadcast or
// another PoP holds, and limit past spec.limits.maxBroadcasts.
func TestReserveRecordsAreAnsweredPerID(t *testing.T) {
	r := hub.NewRegistry(harnessLog(t), hub.Options{})
	homeID, _, err := r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{pops: map[string]*pops.Pop{
		"lan": {Spec: pops.PopSpec{Limits: pops.Limits{MaxBroadcasts: 2}}},
	}}
	src.pops["lan"].Name = "lan"
	m := New(Options{Source: src, Registry: r, Log: harnessLog(t)})
	res := NewMemReservations()
	u := m.StartUplink(UplinkOptions{Reservations: res})
	if _, err := res.Reserve(context.Background(), "OTHER2", "elsewhere", 0); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, want string }{
		{"K7XQ2M", poplink.ReservedOK},
		{"K7XQ2M", poplink.ReservedOK}, // re-asserted on a relink
		{homeID, poplink.ReservedConflict},
		{"OTHER2", poplink.ReservedConflict},
		{"ABC234", poplink.ReservedOK},
		{"DEF567", poplink.ReservedLimit},
	} {
		if got := u.reserveOne("lan", tc.id); got != tc.want {
			t.Errorf("reserve %s = %q, want %q", tc.id, got, tc.want)
		}
	}
	if owner, ok := u.ReservedFor("K7XQ2M"); !ok || owner != "lan" {
		t.Errorf("ReservedFor = %q, %v", owner, ok)
	}
}

// A single-pod home (-pops-file, no Leases, no edge pulls): an outside
// viewer of a PoP broadcast waits in a publisher-less hub, the want reaches
// the PoP, its leg claims that hub, the viewer gets the prime — and the
// linger closes the leg with 4013.
func TestSinglePodHomeServesAPoPBroadcastOnDemand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	log := harnessLog(t)
	cred, _ := pops.NewCredential()
	path := filepath.Join(t.TempDir(), "pops.yaml")
	writePopsFile(t, path, pops.CredentialSHA256(cred))
	cert, err := tlsutil.GenerateDevCert([]string{"localhost", "127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Home.
	port := freeUDPPort(t)
	cfg := config.Config{
		Addr: fmt.Sprintf("127.0.0.1:%d", port), MaxSubscribers: 4, MaxIdleTimeout: 30 * time.Second,
		KeepAlivePeriod: 10 * time.Second, BroadcastGrace: time.Minute, Pops: true, PopsFile: path,
		UplinkCarriers: true, PopUplinkLinger: time.Second, PopDemandTimeout: time.Second,
	}
	r := hub.NewRegistry(log, hub.Options{MaxSubscribers: 4, BroadcastGrace: time.Minute})
	srv := transport.New(cfg, r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }, log,
		metrics.NewServerMetrics(prometheus.NewRegistry()))
	src, err := popsrc.StartFile(ctx, path, log)
	if err != nil {
		t.Fatal(err)
	}
	m := New(Options{Pod: "home", PodAddr: cfg.Addr, Source: src, Status: popsrc.NewMemStatus(src), Registry: r,
		Log: log, LeaseRenew: 100 * time.Millisecond, ByeGrace: 200 * time.Millisecond})
	t.Cleanup(m.Close)
	srv.SetPopHome(m)
	up := m.StartUplink(UplinkOptions{Reservations: NewMemReservations(), DemandTimeout: time.Second})
	srv.SetPopDemand(up)
	done := make(chan struct{})
	go func() { _ = srv.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	// PoP relay.
	pport := freeUDPPort(t)
	pcfg := config.Config{Addr: fmt.Sprintf("127.0.0.1:%d", pport), MaxSubscribers: 4, MaxIdleTimeout: 30 * time.Second,
		KeepAlivePeriod: 10 * time.Second, BroadcastGrace: time.Minute}
	pr := hub.NewRegistry(log, hub.Options{MaxSubscribers: 4, BroadcastGrace: time.Minute})
	psrv := transport.New(pcfg, pr, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }, log,
		metrics.NewServerMetrics(prometheus.NewRegistry()))
	c := popclient.New(popclient.Options{
		StateDir: t.TempDir(), Version: "test", Log: log, Dial: popclient.Dialer(certPool(cert)),
		Registry: pr, Server: psrv, Cfg: pcfg,
		RetryMin: 50 * time.Millisecond, RetryMax: 200 * time.Millisecond, TerminalRetry: 200 * time.Millisecond,
	})
	pdone := make(chan struct{})
	go func() { _ = psrv.Run(ctx); close(pdone) }()
	cdone := make(chan struct{})
	go func() { _ = c.Run(ctx); close(cdone) }()
	t.Cleanup(func() { cancel(); <-pdone; <-cdone })
	if err := c.Pair(ctx, popclient.CredentialPrefix+cfg.Addr+"/"+cred); err != nil {
		t.Fatal(err)
	}
	WaitFor(t, "file link up", func() bool { return c.State().Header == popclient.HeaderLinked })

	clientTLS := &tls.Config{RootCAs: certPool(cert), ServerName: "localhost", NextProtos: []string{http3.NextProtoH3}}
	pub := wtDial(t, clientTLS, "https://"+pcfg.Addr+"/publish")
	id := announcedID(t, pub)
	WaitFor(t, "the reservation at home", func() bool {
		owner, ok := up.ReservedFor(id)
		return ok && owner == "lan-party"
	})
	kf := testKeyframe(t, 1)
	st, err := pub.OpenUniStream()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.Write(kf)
	_ = st.Close()
	WaitFor(t, "the PoP caches the keyframe", func() bool {
		s, ok := pr.Stats().Broadcasts[pr.ObfuscateID(id)]
		return ok && s.CachedKeyframeID == 1
	})

	viewer := wtDial(t, clientTLS, "https://"+cfg.Addr+"/subscribe/"+id)
	if got := firstKeyframe(t, viewer); !bytes.Equal(got, kf) {
		t.Fatal("prime differs")
	}
	if s := c.UplinkStats(); s.Legs != 1 {
		t.Fatalf("legs = %d", s.Legs)
	}
	viewer.CloseWithError(0, "")
	WaitFor(t, "4013 ends the want", func() bool {
		for _, b := range c.UplinkBroadcasts() {
			if b.ID == id {
				return !b.Wanted && !b.Forwarded
			}
		}
		return false
	})
	if owner, ok := up.ReservedFor(id); !ok || owner != "lan-party" {
		t.Fatal("the reservation did not survive the linger")
	}
}

func wtDial(t *testing.T, tlsCfg *tls.Config, target string) *webtransport.Session {
	t.Helper()
	d := &webtransport.Transport{TLSClientConfig: tlsCfg, QUICConfig: &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true}}
	t.Cleanup(func() { d.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, sess, err := d.Dial(ctx, target, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", target, err)
	}
	t.Cleanup(func() { sess.CloseWithError(0, "") })
	return sess
}

func announcedID(t *testing.T, sess *webtransport.Session) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		st, err := sess.AcceptUniStream(ctx)
		if err != nil {
			t.Fatalf("no announce: %v", err)
		}
		data, _ := io.ReadAll(st)
		if _, typ, err := wire.PeekType(data); err == nil && typ == wire.TypeBroadcastAnnounce {
			id, _ := wire.ParseBroadcastAnnounce(data)
			return id
		}
	}
}

func firstKeyframe(t *testing.T, sess *webtransport.Session) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		st, err := sess.AcceptUniStream(ctx)
		if err != nil {
			var se *webtransport.SessionError
			if errors.As(err, &se) {
				t.Fatalf("viewer closed with %d", se.ErrorCode)
			}
			t.Fatalf("no keyframe: %v", err)
		}
		data, _ := io.ReadAll(st)
		if _, typ, err := wire.PeekType(data); err == nil && typ == wire.TypeStreamFrame {
			return data
		}
	}
}

func testKeyframe(t *testing.T, frameID uint32) []byte {
	t.Helper()
	payload := bytes.Repeat([]byte{byte(frameID)}, 3000)
	msg, err := wire.AppendStreamFrameHeader(nil, wire.StreamFrameHeader{
		Keyframe: true, FrameID: frameID, TimestampUs: 16_667, PayloadLen: uint32(len(payload)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(msg, payload...)
}
