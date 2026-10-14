// R40 CF1 (docs/71 D13, §9.3): the screening notice rides RelayIdentity on
// /echo and — only with -screening-notice on — on its own uni stream on every
// publish session. Subscribe and /internal/subscribe never receive it.
package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

func noticeConfig(notice bool) config.Config {
	return config.Config{
		MaxSubscribers:  2,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		ServerName:      "Screened Lab",
		ReleaseVersion:  "1.2.3",
		ScreeningNotice: notice,
	}
}

// With the notice on: the publish session receives exactly one RelayIdentity,
// carrying the screening bit and the same version and name /echo advertises;
// a subscribe session on the same broadcast receives none.
func TestScreeningNoticeOnPublishSessionOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _ := startTestServerCfg(t, ctx, noticeConfig(true))

	pub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
	id, _, consumed := readPublisherHandshakeMsgs(t, ctx, pub)
	pubMsgs := append(consumed, collectUniMessages(t, pub, 2*time.Second)...)
	ident, count := identityFrom(t, pubMsgs)
	if count != 1 {
		t.Fatalf("publish session got %d RelayIdentity streams, want exactly 1", count)
	}
	if ident.Notices&wire.RelayNoticeScreening == 0 {
		t.Fatalf("publish identity notices = %#x, want the screening bit", ident.Notices)
	}
	if ident.ServerVersion != "1.2.3" || ident.Name != "Screened Lab" {
		t.Fatalf("publish identity = %+v, want 1.2.3 / Screened Lab", ident)
	}

	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	if _, n := identityFrom(t, collectUniMessages(t, sub, 1500*time.Millisecond)); n != 0 {
		t.Fatalf("subscribe session got %d RelayIdentity streams, want none", n)
	}

	// The /echo probe carries the same bit, so the desktop and iOS pre-live
	// probes see it.
	echo := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/echo", port), clientTLS)
	echoIdent, n := identityFrom(t, collectUniMessages(t, echo, 2*time.Second))
	if n != 1 || echoIdent.Notices&wire.RelayNoticeScreening == 0 {
		t.Fatalf("echo identity %+v (%d streams), want one with the screening bit", echoIdent, n)
	}
}

// With the notice off (the default), a publish session receives no
// RelayIdentity at all — byte-identical to a pre-R40 relay — and /echo's
// identity carries no notices byte.
func TestScreeningNoticeOffLeavesPublishSessionsAlone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _ := startTestServerCfg(t, ctx, noticeConfig(false))

	pub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
	id, _, consumed := readPublisherHandshakeMsgs(t, ctx, pub)
	pubMsgs := append(consumed, collectUniMessages(t, pub, 1500*time.Millisecond)...)
	if _, n := identityFrom(t, pubMsgs); n != 0 {
		t.Fatalf("publish session got %d RelayIdentity streams with the notice off, want none", n)
	}
	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	if _, n := identityFrom(t, collectUniMessages(t, sub, 1500*time.Millisecond)); n != 0 {
		t.Fatalf("subscribe session got %d RelayIdentity streams, want none", n)
	}

	echo := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/echo", port), clientTLS)
	for _, m := range collectUniMessages(t, echo, 2*time.Second) {
		if len(m) >= 2 && m[1] == wire.TypeRelayIdentity {
			want, err := wire.AppendRelayIdentity(nil, wire.RelayIdentity{ServerVersion: "1.2.3", Name: "Screened Lab"})
			if err != nil {
				t.Fatal(err)
			}
			if string(m) != string(want) {
				t.Fatalf("echo identity %x, want the R37 shape %x (no notices byte)", m, want)
			}
		}
	}
}

// An edge is plumbing, not a broadcaster: its /internal/subscribe session
// gets no notice even from a pod that advertises one.
func TestScreeningNoticeNotSentToInternalSubscribe(t *testing.T) {
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

	pod := startClusteredPod(t, ctx, cs, "pod-a", cert, pool, 0, func(c *config.Config) { c.ScreeningNotice = true })

	pub := dial(t, ctx, fmt.Sprintf("https://%s/publish", pod.addr()), clientTLS)
	defer pub.CloseWithError(0, "")
	id, _, consumed := readPublisherHandshakeMsgs(t, ctx, pub)
	if _, n := identityFrom(t, append(consumed, collectUniMessages(t, pub, 1500*time.Millisecond)...)); n != 1 {
		t.Fatalf("publisher got %d notices; the fixture is wrong, not the assertion", n)
	}

	edge := dial(t, ctx, fmt.Sprintf("https://%s/internal/subscribe/%s?psk=fleet-psk&gen=1&proto=%d",
		pod.addr(), id, wire.Version), clientTLS)
	defer edge.CloseWithError(0, "")
	noMoreUniStreams(t, ctx, edge)
}
