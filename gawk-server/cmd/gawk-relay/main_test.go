package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/relaystate"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

// pinnedHashes reads the hashes a client would be given (the console, an
// offer, a link): the DER hash of every slot in the state directory.
func pinnedHashes(t *testing.T, stateDir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	paths, _ := filepath.Glob(filepath.Join(stateDir, certsSubdir, "cert-*.pem"))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if blk, _ := pem.Decode(b); blk != nil {
			sum := sha256.Sum256(blk.Bytes)
			out[hex.EncodeToString(sum[:])] = true
		}
	}
	return out
}

// pinnedTLS trusts exactly the pinned hashes, as a browser given
// serverCertificateHashes does: no CA, no name check.
func pinnedTLS(t *testing.T, stateDir string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{http3.NextProtoH3},
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			sum := sha256.Sum256(raw[0])
			if !pinnedHashes(t, stateDir)[hex.EncodeToString(sum[:])] {
				return errors.New("certificate hash not pinned")
			}
			return nil
		},
	}
}

func dialer(t *testing.T, clientTLS *tls.Config) *webtransport.Transport {
	d := &webtransport.Transport{
		TLSClientConfig: clientTLS,
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true, MaxIdleTimeout: 30 * time.Second},
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// dialRetry dials until the relay is up.
func dialRetry(t *testing.T, ctx context.Context, d *webtransport.Transport, url string) *webtransport.Session {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		rsp, sess, err := d.Dial(ctx, url, nil)
		if err == nil {
			return sess
		}
		if rsp != nil && rsp.StatusCode != http.StatusOK {
			t.Fatalf("Dial %s: status %d", url, rsp.StatusCode)
		}
		if time.Now().After(deadline) {
			t.Fatalf("Dial %s: %v", url, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// startRelay runs the real gawk-relay startup in-process with only a state
// directory and a loopback address; everything else is the profile default.
func startRelay(t *testing.T, ctx context.Context, stateDir string, port int) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-addr", fmt.Sprintf("127.0.0.1:%d", port),
			"-metrics-addr", "off",
			"-lan-address", "127.0.0.1",
			"-console-addr", "127.0.0.1:0", // never the host's real 4480
			"-log-level", "error",
		}, envOf(map[string]string{"GAWK_STATE_DIR": stateDir}))
	}()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("relay run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("relay did not stop")
		}
	}
}

// docs/76 §6.1: no certificate flags, no keys: the relay creates its state
// directory, keys and pinned certificate and serves a pinned client.
func TestRelayStartsWithNoConfiguration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stateDir := filepath.Join(t.TempDir(), "state")
	port := freeUDPPort(t)
	stop := startRelay(t, ctx, stateDir, port)
	defer stop()

	sess := dialRetry(t, ctx, dialer(t, pinnedTLS(t, stateDir)), fmt.Sprintf("https://127.0.0.1:%d/echo", port))
	defer sess.CloseWithError(0, "")
	if err := sess.SendDatagram([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
	defer rcancel()
	if b, err := sess.ReceiveDatagram(rctx); err != nil || string(b) != "ping" {
		t.Fatalf("echo = %q, %v", b, err)
	}
	for _, f := range []string{relaystate.StatelessResetKeyFile, relaystate.ResumeTokenKeyFile, relaystate.StatsKeyFile} {
		if _, err := os.Stat(filepath.Join(stateDir, f)); err != nil {
			t.Errorf("%s not persisted: %v", f, err)
		}
	}
	if n := len(pinnedHashes(t, stateDir)); n != 2 {
		t.Errorf("%d pinned certificates in the state directory, want 2 (current + next)", n)
	}
}

// readHandshake reads the publisher's announce and resume token, skipping
// every other session-start message (accept order is unspecified).
func readHandshake(t *testing.T, ctx context.Context, sess *webtransport.Session) (id, token string) {
	t.Helper()
	for id == "" || token == "" {
		actx, cancel := context.WithTimeout(ctx, 5*time.Second)
		s, err := sess.AcceptUniStream(actx)
		cancel()
		if err != nil {
			t.Fatalf("AcceptUniStream: %v", err)
		}
		data, err := io.ReadAll(s)
		if err != nil {
			t.Fatal(err)
		}
		_, typ, err := wire.PeekType(data)
		if err != nil {
			continue
		}
		switch typ {
		case wire.TypeBroadcastAnnounce:
			if id, err = wire.ParseBroadcastAnnounce(data); err != nil {
				t.Fatal(err)
			}
		case wire.TypeResumeToken:
			tok, err := wire.ParseResumeToken(data)
			if err != nil {
				t.Fatal(err)
			}
			token = hex.EncodeToString(tok)
		}
	}
	return id, token
}

// docs/76 PP2: a relay restart resumes a broadcast with its resume token,
// with no publish secret, because the resume-token key is persisted. A relay
// with a fresh state directory (another key) refuses the same token, which
// is what makes the first half mean something.
func TestRelayRestartResumesABroadcast(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stateDir := t.TempDir()
	port := freeUDPPort(t)

	stop := startRelay(t, ctx, stateDir, port)
	d := dialer(t, pinnedTLS(t, stateDir))
	pub := dialRetry(t, ctx, d, fmt.Sprintf("https://127.0.0.1:%d/publish", port))
	id, token := readHandshake(t, ctx, pub)
	stop()

	stop = startRelay(t, ctx, stateDir, port)
	d2 := dialer(t, pinnedTLS(t, stateDir))
	re := dialRetry(t, ctx, d2, fmt.Sprintf("https://127.0.0.1:%d/publish/%s?resume=%s", port, id, token))
	gotID, _ := readHandshake(t, ctx, re)
	if gotID != id {
		t.Fatalf("reclaim after restart got %q, want %q", gotID, id)
	}
	re.CloseWithError(0, "")
	stop()

	other := t.TempDir()
	stop = startRelay(t, ctx, other, port)
	defer stop()
	d3 := dialer(t, pinnedTLS(t, other))
	url := fmt.Sprintf("https://127.0.0.1:%d/publish/%s?resume=%s", port, id, token)
	deadline := time.Now().Add(15 * time.Second)
	for {
		rsp, sess, err := d3.Dial(ctx, url, nil)
		if err == nil {
			sess.CloseWithError(0, "")
			t.Fatal("a relay with another resume-token key accepted the token")
		}
		if rsp != nil {
			if rsp.StatusCode == http.StatusOK {
				t.Fatal("reclaim with a foreign token succeeded")
			}
			break // refused at the HTTP layer: the token did not verify
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay with a fresh state dir never answered: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// An explicit -cert-file/-key-file pair overrides the rolling certificate,
// and a broken explicit pair fails rather than falling back to self-signing.
func TestRelayExplicitCertificateOverridesRolling(t *testing.T) {
	stateDir := t.TempDir()
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	pair, err := tlsutil.GenerateDevCert([]string{"relay.example"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := tlsutil.WriteCertPair(certPath, keyPath, pair); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	r := &Relay{StateDir: stateDir, Log: log}
	r.Cfg.CertFile, r.Cfg.KeyFile = certPath, keyPath
	get, err := certificate(r)
	if err != nil {
		t.Fatal(err)
	}
	served, err := get(nil)
	if err != nil || tlsutil.CertHashHex(served.Leaf) != tlsutil.CertHashHex(pair.Leaf) {
		t.Fatalf("served %v, %v; want the explicit pair", served, err)
	}
	if r.Rolling != nil {
		t.Fatal("rolling certificate built despite the explicit pair")
	}
	if _, err := os.Stat(filepath.Join(stateDir, certsSubdir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("rolling slots written despite the explicit pair (%v)", err)
	}

	r = &Relay{StateDir: stateDir, Log: log}
	r.Cfg.CertFile, r.Cfg.KeyFile = filepath.Join(dir, "missing.crt"), filepath.Join(dir, "missing.key")
	if _, err := certificate(r); err == nil || r.Rolling != nil {
		t.Fatal("a missing explicit pair fell back to the rolling certificate")
	}
}

func TestRelayRejectsBadFlags(t *testing.T) {
	err := run(context.Background(), []string{"-pop-leg-delivery", "bogus"}, envOf(map[string]string{"GAWK_STATE_DIR": t.TempDir()}))
	if err == nil || !strings.Contains(err.Error(), "pop-leg-delivery") {
		t.Fatalf("run = %v, want the flag error", err)
	}
}
