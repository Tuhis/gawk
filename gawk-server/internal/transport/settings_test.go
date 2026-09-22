package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// The WebTransport SETTINGS WebKit requires (docs/gotchas.md, the
// webtransport-go `Server.Config` entry; quic-go/webtransport-go#355). draft-ietf-webtrans-http3
// makes the three WT_INITIAL_MAX_* settings mandatory whenever
// WT_MAX_SESSIONS > 1, and webtransport-go always advertises
// WT_MAX_SESSIONS = 2^62-1. Since v0.12.0 the library sends the trio only when
// Server.Config is set — with a nil Config the relay advertises 3 entries and
// Safari refuses the session before the extended CONNECT, so nothing on the
// relay logs it. This is the deterministic gate that would have caught it.
//
// The codepoints are restated rather than imported: webtransport-go keeps them
// unexported, and a test that reads the wire should not borrow the constants
// it is checking. WT_MAX_SESSIONS is the draft-07+ codepoint, which is what
// webtransport-go advertises — not the draft-02 0xc671706a that the vendored
// wtransport-proto (gawk-broadcast-desktop) still defines for its own
// backwards compatibility.
const (
	settingWTInitialMaxData        = 0x2b61
	settingWTInitialMaxStreamsUni  = 0x2b64
	settingWTInitialMaxStreamsBidi = 0x2b65
	settingWTMaxSessions           = 0x14e9cd29
)

// readRelaySettings dials the relay with a bare HTTP/3 client and returns the
// SETTINGS it advertises — upstream of any CONNECT, exactly where WebKit
// decides.
func readRelaySettings(t *testing.T, ctx context.Context, port int, clientTLS *tls.Config) *http3.Settings {
	t.Helper()
	qconf := &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true}
	var conn *quic.Conn
	deadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		conn, err = quic.DialAddr(ctx, fmt.Sprintf("127.0.0.1:%d", port), clientTLS, qconf)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("quic.DialAddr: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Cleanup(func() { conn.CloseWithError(0, "") })

	tr := &http3.Transport{EnableDatagrams: true}
	t.Cleanup(func() { tr.Close() })
	cc := tr.NewClientConn(conn)
	select {
	case <-cc.ReceivedSettings():
	case <-time.After(5 * time.Second):
		t.Fatal("relay sent no SETTINGS within 5s")
	}
	return cc.Settings()
}

func TestRelayAdvertisesWebTransportFlowControlSettings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _ := startTestServer(t, ctx, 4)

	s := readRelaySettings(t, ctx, port, clientTLS)
	if !s.EnableDatagrams || !s.EnableExtendedConnect {
		t.Fatalf("relay SETTINGS lack H3 datagrams/extended CONNECT: %+v", s)
	}
	if got := s.Other[settingWTMaxSessions]; got < 2 {
		t.Fatalf("WT_MAX_SESSIONS = %d, want > 1 (the value that makes the trio mandatory)", got)
	}
	// The exact advertisement webtransport-go v0.11.1 (the last release WebKit
	// joined) put on the wire, and what Server.Config restores.
	want := map[uint64]uint64{
		settingWTInitialMaxStreamsUni:  1 << 60,
		settingWTInitialMaxStreamsBidi: 1 << 60,
		settingWTInitialMaxData:        1 << 60,
	}
	for id, v := range want {
		got, ok := s.Other[id]
		if !ok {
			t.Errorf("SETTINGS 0x%x missing (advertised: %v)", id, s.Other)
			continue
		}
		if got != v {
			t.Errorf("SETTINGS 0x%x = %d, want %d", id, got, v)
		}
	}
}
