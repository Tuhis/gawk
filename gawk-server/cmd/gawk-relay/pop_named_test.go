package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"

	"github.com/Tuhis/gawk/gawk-server/adminapi"
	"github.com/Tuhis/gawk/gawk-server/internal/popclient"
	"github.com/Tuhis/gawk/gawk-server/internal/relayconsole"
	"github.com/Tuhis/gawk/gawk-server/internal/relaystate"
)

const testNamedHost = "p-7k2m9x.pop.gawk.test"

// seedNamed writes a named key and chain into a state directory, as a PoP
// that was issued before a restart has them, and returns the issuer's pool.
func seedNamed(t *testing.T, stateDir string) *x509.CertPool {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ACME"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(30 * 24 * time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{testNamedHost}, NotBefore: time.Now().Add(-time.Minute),
		NotAfter: time.Now().Add(6 * 24 * time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	if err := relaystate.Ensure(stateDir); err != nil {
		t.Fatal(err)
	}
	if err := relaystate.SaveSecret(stateDir, "named-key.pem", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})); err != nil {
		t.Fatal(err)
	}
	if err := relaystate.SaveSecret(stateDir, "named-cert.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return pool
}

type staticResolver map[string]netip.Addr

func (s staticResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := s[host]; ok {
		return []netip.Addr{a}, nil
	}
	return nil, nil
}

// The console's QR follows the named tier (docs/76 §7.1, item 40): the
// named page once the venue resolves the name, the pinned link while it
// filters it, with the notice saying why.
func TestConsoleJoinQRFollowsTheNamedTier(t *testing.T) {
	prev := linkForConsole.Load()
	t.Cleanup(func() { linkForConsole.Store(prev) })
	c := testConsole(t)
	seedNamed(t, c.r.StateDir)
	lan := netip.MustParseAddr("192.168.1.20")
	link := popclient.New(popclient.Options{StateDir: c.r.StateDir, Log: slog.New(slog.DiscardHandler)})
	for _, tc := range []struct {
		name           string
		system, public staticResolver
		tier           string
		notice         string
	}{
		{"resolves", staticResolver{testNamedHost: lan}, staticResolver{testNamedHost: lan}, relayconsole.TierNamed, ""},
		{"filtered", staticResolver{}, staticResolver{testNamedHost: lan}, relayconsole.TierStandalone, popclient.DNSFilteredNotice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := popclient.NewNamed(link, popclient.NamedOptions{StateDir: c.r.StateDir, LAN: func() netip.Addr { return lan },
				System: tc.system, Public: tc.public, Log: slog.New(slog.DiscardHandler)})
			n.CheckDNS(context.Background())
			var cl consoleLink = popConsoleLink{c: link, n: n}
			linkForConsole.Store(&cl)
			s := c.State()
			if s.Header != relayconsole.HeaderWaiting {
				t.Fatalf("header = %q: an unpaired link must leave it to the console", s.Header)
			}
			url, tier := relayconsole.JoinQR(s)
			if tier != tc.tier {
				t.Fatalf("JoinQR = %s %s, want tier %s", url, tier, tc.tier)
			}
			if tier == relayconsole.TierNamed && url != "https://"+testNamedHost+":4433" {
				t.Fatalf("named QR = %s", url)
			}
			if s.Notice != tc.notice {
				t.Fatalf("notice = %q, want %q", s.Notice, tc.notice)
			}
			var dnsLine *adminapi.HealthLine
			for i, h := range s.Health {
				if h.Key == "dns" {
					dnsLine = &s.Health[i]
				}
			}
			if dnsLine == nil || dnsLine.OK != (tc.notice == "") {
				t.Fatalf("console health has no right DNS self-check line: %+v", s.Health)
			}
		})
	}
}

// docs/76 §6.11 item 39 through the real startup: a relay holding a named
// certificate serves the web app and its config.js over HTTPS on -ui-addr,
// and its QUIC listener answers the name with the same certificate — while
// a pinned client by IP literal still gets the rolling one.
func TestRelayServesItsNamedAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	stateDir := filepath.Join(t.TempDir(), "state")
	pool := seedNamed(t, stateDir)
	port := freeUDPPort(t)
	uiAddr := freeTCPAddr(t)
	ctx2, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx2, []string{
			"-addr", fmt.Sprintf("127.0.0.1:%d", port),
			"-metrics-addr", "off",
			"-lan-address", "127.0.0.1",
			"-console-addr", "127.0.0.1:0",
			"-ui-addr", uiAddr,
			"-log-level", "error",
		}, envOf(map[string]string{"GAWK_STATE_DIR": stateDir}))
	}()
	defer func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("relay run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("relay did not stop")
		}
	}()

	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: testNamedHost},
	}}
	get := func(path string) string {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			rsp, err := client.Get("https://" + uiAddr + path)
			if err == nil {
				b, _ := io.ReadAll(rsp.Body)
				rsp.Body.Close()
				if rsp.StatusCode != http.StatusOK {
					t.Fatalf("GET %s: %d", path, rsp.StatusCode)
				}
				return string(b)
			}
			if time.Now().After(deadline) {
				t.Fatalf("GET https://%s%s: %v", uiAddr, path, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if page := get("/"); !strings.Contains(page, "<html") {
		t.Fatalf("named page = %.80q", page)
	}
	if js := get("/config.js"); !strings.Contains(js, fmt.Sprintf(`"relayUrl":"https://%s:%d"`, testNamedHost, port)) {
		t.Fatalf("named config.js = %s", js)
	}

	// The relay by name: a publicly trusted handshake, no pinned hashes.
	named := &tls.Config{RootCAs: pool, ServerName: testNamedHost, NextProtos: []string{http3.NextProtoH3}}
	sess := dialRetry(t, ctx, dialer(t, named), fmt.Sprintf("https://127.0.0.1:%d/echo", port))
	sess.CloseWithError(0, "")
	// By IP literal with the pins, as before.
	sess = dialRetry(t, ctx, dialer(t, pinnedTLS(t, stateDir)), fmt.Sprintf("https://127.0.0.1:%d/echo", port))
	sess.CloseWithError(0, "")
}
