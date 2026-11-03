package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/publisher"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/status"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/streamkey"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/testmedia"
)

// End to end against the real relay (gawk-server/cmd/gawk-server, built
// from this checkout like gawk-server/publisher's relay_integration_test):
// a pion WHIP client pushes H.264 RTP through the gateway into the relay,
// the relay's /statusz shows the broadcast, and the WHIP DELETE ends it at
// once (4014: the broadcast is gone, not in grace).

var relayBuild struct {
	once      sync.Once
	dir       string
	bin, cert string
	err       error
	help      string
}

func buildRelay(t *testing.T) (bin, devcert string) {
	t.Helper()
	if testing.Short() {
		t.Skip("the relay e2e test builds gawk-server; skipped in -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	serverDir, err := filepath.Abs("../../../gawk-server")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(serverDir, "go.mod")); err != nil {
		t.Skipf("gawk-server source not present: %v", err)
	}
	relayBuild.once.Do(func() {
		dir, err := os.MkdirTemp("", "gawk-whip-e2e-")
		if err != nil {
			relayBuild.err = err
			return
		}
		relayBuild.dir = dir
		relayBuild.bin = filepath.Join(dir, "gawk-server")
		relayBuild.cert = filepath.Join(dir, "gawk-devcert")
		for _, b := range []struct{ out, pkg string }{{relayBuild.bin, "./cmd/gawk-server"}, {relayBuild.cert, "./cmd/gawk-devcert"}} {
			cmd := exec.Command("go", "build", "-o", b.out, b.pkg)
			cmd.Dir = serverDir
			if out, err := cmd.CombinedOutput(); err != nil {
				relayBuild.err = fmt.Errorf("building %s: %v\n%s", b.pkg, err, out)
				return
			}
		}
		out, _ := exec.Command(relayBuild.bin, "-h").CombinedOutput()
		relayBuild.help = string(out)
	})
	if relayBuild.err != nil {
		t.Fatal(relayBuild.err)
	}
	return relayBuild.bin, relayBuild.cert
}

func TestMain(m *testing.M) {
	code := m.Run()
	if relayBuild.dir != "" {
		_ = os.RemoveAll(relayBuild.dir)
	}
	os.Exit(code)
}

func freeLocalPort(t *testing.T, network string) int {
	t.Helper()
	if network == "udp" {
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).Port
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startRealRelay(t *testing.T, extra ...string) (relayURL, ops string) {
	t.Helper()
	bin, dc := buildRelay(t)
	certDir := filepath.Join(t.TempDir(), "cert")
	if out, err := exec.Command(dc, "-out", certDir).CombinedOutput(); err != nil {
		t.Fatalf("gawk-devcert: %v\n%s", err, out)
	}
	port, opsPort := freeLocalPort(t, "udp"), freeLocalPort(t, "tcp")
	args := append([]string{
		"-addr", fmt.Sprintf("127.0.0.1:%d", port),
		"-metrics-addr", fmt.Sprintf("127.0.0.1:%d", opsPort),
		"-cert-file", filepath.Join(certDir, "cert.pem"),
		"-key-file", filepath.Join(certDir, "key.pem"),
	}, extra...)
	cmd := exec.Command(bin, args...)
	if os.Getenv("GAWK_WHIP_TEST_LOG") != "" {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ops = fmt.Sprintf("http://127.0.0.1:%d", opsPort)
	eventually(t, "relay healthy", 15*time.Second, func() bool {
		r, err := http.Get(ops + "/healthz")
		if err != nil {
			return false
		}
		r.Body.Close()
		return r.StatusCode == http.StatusOK
	})
	return fmt.Sprintf("https://127.0.0.1:%d", port), ops
}

type relayStatusz struct {
	Totals struct {
		Broadcasts int `json:"broadcasts"`
	} `json:"totals"`
	Broadcasts map[string]struct {
		PublisherActive   bool   `json:"publisherActive"`
		FramesRelayed     uint64 `json:"framesRelayed"`
		KeyframeStreamsIn uint64 `json:"keyframeStreamsIn"`
		HasConfig         bool   `json:"hasConfig"`
	} `json:"broadcasts"`
}

func fetchRelayStatusz(t *testing.T, ops string) relayStatusz {
	t.Helper()
	r, err := http.Get(ops + "/statusz")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var s relayStatusz
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// mintKey gets a stream key from the relay the way any publisher does —
// publish, read the ID and the resume token — then ends that broadcast, so
// the gateway's reclaim creates it again (§3.1). Only public API: the
// publisher engine.
func mintKey(t *testing.T, relayURL, secret string) streamkey.Key {
	t.Helper()
	var mu sync.Mutex
	var id, token string
	src := newSource(false)
	eng := publisher.New(publisher.Config{RelayURL: relayURL, Insecure: true, PublishSecret: secret},
		publisher.Callbacks{
			OnBroadcastID: func(s string) { mu.Lock(); id = s; mu.Unlock() },
			OnResumeToken: func(s string) { mu.Lock(); token = s; mu.Unlock() },
		},
		publisher.Options{MediaFactory: src.factory})
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("mint: %v", err)
	}
	eventually(t, "ID and token", 5*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return id != "" && token != "" })
	_ = eng.StopBroadcast()
	k, err := streamkey.Parse(streamkey.Prefix + id + "_" + token)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func runE2E(t *testing.T, relayArgs []string, proxyToken string) {
	relayURL, ops := startRealRelay(t, relayArgs...)
	key := mintKey(t, relayURL, "e2e-secret")
	eventually(t, "minted broadcast ended", 5*time.Second, func() bool { return fetchRelayStatusz(t, ops).Totals.Broadcasts == 0 })

	g := startGW(t, nil, func(o *Options) {
		o.Dial = nil // the engine's real WebTransport dial
		o.Relay = RelayOptions{URL: relayURL, PublishSecret: "e2e-secret", PublishProxyToken: proxyToken, Insecure: true}
	})
	c := connect(t, g.public.URL, key)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%30 == 0 {
				c.sendAU(testmedia.IDR(c.sps, 6000))
			} else {
				c.sendAU(testmedia.P(1500, byte(i)))
			}
			time.Sleep(time.Second / 60)
		}
	}()
	eventually(t, "the relay sees the broadcast", 10*time.Second, func() bool {
		s := fetchRelayStatusz(t, ops)
		for _, b := range s.Broadcasts {
			if b.PublisherActive && b.KeyframeStreamsIn > 0 && b.FramesRelayed > 10 && b.HasConfig {
				return true
			}
		}
		return false
	})
	eventually(t, "gateway status live", 5*time.Second, func() bool { return g.status(key).State == status.StateLive })
	close(stop)
	<-done
	if r := c.delete(); r.code != http.StatusOK {
		t.Fatalf("DELETE: %d", r.code)
	}
	// 4014: the broadcast ends now, it does not wait out the grace.
	eventually(t, "the relay ended the broadcast", 5*time.Second, func() bool { return fetchRelayStatusz(t, ops).Totals.Broadcasts == 0 })
	if st := g.status(key); st.State != status.StateEnded || st.LastError != nil {
		t.Fatalf("after DELETE: %+v", st)
	}
}

func TestE2EThroughTheRealRelay(t *testing.T) {
	runE2E(t, []string{"-publish-secret", "e2e-secret"}, "")
}

// The D4 variant: the relay trusts the gateway as a publish proxy
// (-publish-proxy-token, which D12 ties to an explicit -resume-token-key).
func TestE2EThroughTheRealRelayAsPublishProxy(t *testing.T) {
	buildRelay(t)
	if !strings.Contains(relayBuild.help, "publish-proxy-token") {
		t.Skip("this gawk-server build has no -publish-proxy-token yet (docs/83 WG3); the proxy variant can't run")
	}
	runE2E(t, []string{"-publish-secret", "e2e-secret", "-publish-proxy-token", "e2e-proxy-token",
		"-resume-token-key", strings.Repeat("ab", 32)}, "e2e-proxy-token")
}
