package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/lanaddr"
	"github.com/Tuhis/gawk/gawk-server/internal/relayconsole"
)

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// getRetry GETs url until the console is up.
func getRetry(t *testing.T, url string) []byte {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		rsp, err := http.Get(url)
		if err == nil {
			b, _ := io.ReadAll(rsp.Body)
			rsp.Body.Close()
			if rsp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s: %d %s", url, rsp.StatusCode, b)
			}
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s: %v", url, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The real startup brings the console up with the relay: /healthz, the
// state with the pinned hashes and a broadcast that is live, and the
// host's /app/config.js naming the relay with both hashes (docs/76 §6.1
// item 6, §7.1).
func TestRelayServesTheConsole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	stateDir := t.TempDir()
	port := freeUDPPort(t)
	consoleAddr := freeTCPAddr(t)
	ctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-addr", fmt.Sprintf("127.0.0.1:%d", port),
			"-metrics-addr", "off",
			"-lan-address", "127.0.0.1",
			"-log-level", "error",
			"-console-addr", consoleAddr,
		}, envOf(map[string]string{"GAWK_STATE_DIR": stateDir}))
	}()
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}()
	base := "http://" + consoleAddr

	if b := getRetry(t, base+"/healthz"); strings.TrimSpace(string(b)) != "ok" {
		t.Fatalf("/healthz = %q", b)
	}

	pins := pinnedHashes(t, stateDir)
	var cfgJS struct {
		RelayURL             string   `json:"relayUrl"`
		RelayCertHashes      []string `json:"relayCertHashes"`
		RequirePublishSecret *bool    `json:"requirePublishSecret"`
	}
	js := string(getRetry(t, base+"/app/config.js"))
	i := strings.Index(js, "{")
	if i < 0 || json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimSpace(js[i:]), ";")), &cfgJS) != nil {
		t.Fatalf("config.js does not parse:\n%s", js)
	}
	if cfgJS.RelayURL != fmt.Sprintf("https://127.0.0.1:%d", port) || cfgJS.RequirePublishSecret == nil || *cfgJS.RequirePublishSecret {
		t.Errorf("config.js = %+v", cfgJS)
	}
	if len(cfgJS.RelayCertHashes) != 2 || !pins[cfgJS.RelayCertHashes[0]] || !pins[cfgJS.RelayCertHashes[1]] {
		t.Errorf("config.js hashes %v, want the two pinned %v", cfgJS.RelayCertHashes, pins)
	}
	if b := string(getRetry(t, base+"/app/")); !strings.Contains(b, "<") {
		t.Errorf("/app/ served %q", b)
	}

	// A broadcast goes live; the console lists its raw code and its tile
	// label (R74 PP11: the Live table's Label column; Wall stays empty
	// with no wall).
	pub := dialRetry(t, ctx, dialer(t, pinnedTLS(t, stateDir)), fmt.Sprintf("https://127.0.0.1:%d/publish?name=tuhis", port))
	defer pub.CloseWithError(0, "")
	id, _ := readHandshake(t, ctx, pub)

	var v struct {
		relayconsole.State
		Join struct{ URL, Tier string }
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := json.Unmarshal(getRetry(t, base+"/api/state"), &v); err != nil {
			t.Fatal(err)
		}
		if len(v.Live) == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(v.Live) != 1 || v.Live[0].Code != id || v.Live[0].Label != "tuhis" || v.Live[0].Wall != "" {
		t.Errorf("live = %+v, want %s labelled tuhis, off any wall", v.Live, id)
	}
	if v.Header != relayconsole.HeaderWaiting || v.Join.Tier != relayconsole.TierStandalone || v.RelayPort != port {
		t.Errorf("state: header %q tier %q port %d", v.Header, v.Join.Tier, v.RelayPort)
	}
	if !pins[v.Hashes[0]] || !pins[v.Hashes[1]] || v.CertRotatesIn <= 0 {
		t.Errorf("hashes %v rotates in %v", v.Hashes, v.CertRotatesIn)
	}
	keys := map[string]bool{}
	for _, h := range v.Health {
		keys[h.Key] = true
	}
	for _, k := range []string{"certificate", "lan", "version"} {
		if !keys[k] {
			t.Errorf("health lacks %q: %+v", k, v.Health)
		}
	}
}

// A console address beyond loopback stops the relay with a clear error
// rather than exposing the console.
func TestRelayRefusesANonLoopbackConsole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := run(ctx, []string{
		"-addr", fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t)),
		"-metrics-addr", "off", "-lan-address", "127.0.0.1", "-log-level", "error",
		"-console-addr", "0.0.0.0:0",
	}, envOf(map[string]string{"GAWK_STATE_DIR": t.TempDir()}))
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("run = %v, want the loopback refusal", err)
	}
}

func testConsole(t *testing.T) *console {
	t.Helper()
	r := &Relay{StateDir: t.TempDir(), LAN: lanaddr.NewWatcher("192.168.1.20", 0)}
	r.Cfg.Addr = ":4433"
	r.Relay = config.RelayConfig{UIAddr: ":4433", UplinkBudget: config.Budget{Mbps: 20}}
	return newConsole(r)
}

// Run standalone leaves "Waiting to pair" and survives a restart.
func TestConsoleRunStandalonePersists(t *testing.T) {
	c := testConsole(t)
	if h := c.State().Header; h != relayconsole.HeaderWaiting {
		t.Fatalf("first start: %q, want waiting", h)
	}
	if err := c.RunStandalone(); err != nil {
		t.Fatal(err)
	}
	if h := c.State().Header; h != relayconsole.HeaderStandalone {
		t.Fatalf("after Run standalone: %q", h)
	}
	again := newConsole(c.r)
	if h := again.State().Header; h != relayconsole.HeaderStandalone {
		t.Fatalf("after a restart: %q, want standalone", h)
	}
	if _, err := os.Stat(filepath.Join(c.r.StateDir, standaloneMarker)); err != nil {
		t.Fatal(err)
	}
}

// docs/76 §6.8: the console only lowers a budget. The -uplink-budget flag
// is the starting point; zero goes back to it.
func TestConsoleBudgets(t *testing.T) {
	c := testConsole(t)
	if s := c.State(); s.Uplink != (config.Budget{Mbps: 20}) {
		t.Fatalf("uplink starts at %+v, want the flag's 20 Mbps", s.Uplink)
	}
	if err := c.LowerUplink(config.Budget{Mbps: 10, Streams: 2}); err != nil {
		t.Fatal(err)
	}
	if s := c.State(); s.Uplink != (config.Budget{Mbps: 10, Streams: 2}) {
		t.Fatalf("lowered uplink %+v", s.Uplink)
	}
	if err := c.LowerUplink(config.Budget{Mbps: 30}); err == nil {
		t.Error("raising the uplink past the configured 20 Mbps was accepted")
	}
	if err := c.LowerUplink(config.Budget{}); err != nil || c.State().Uplink != (config.Budget{Mbps: 20}) {
		t.Errorf("reset: %v %+v", err, c.State().Uplink)
	}
	if err := c.LowerDownlink(config.Budget{Mbps: 40}); err != nil || c.State().Downlink != (config.Budget{Mbps: 40}) {
		t.Errorf("downlink: %v %+v", err, c.State().Downlink)
	}
}

func TestConsoleLANOverride(t *testing.T) {
	c := testConsole(t)
	if err := c.SetLANOverride(netip.MustParseAddr("10.1.2.3")); err != nil {
		t.Fatal(err)
	}
	if got := c.State().LanAddr; got != netip.MustParseAddr("10.1.2.3") {
		t.Fatalf("LanAddr = %v", got)
	}
	for _, bad := range []string{"0.0.0.0", "224.0.0.1", "::"} {
		if err := c.SetLANOverride(netip.MustParseAddr(bad)); err == nil {
			t.Errorf("%s accepted as the LAN address", bad)
		}
	}
	// The invalid Addr goes back to detection (none in this test).
	if err := c.SetLANOverride(netip.Addr{}); err != nil {
		t.Fatal(err)
	}
}

// With no PoP link in this build, pairing says so instead of failing
// silently.
func TestConsolePairWithoutALink(t *testing.T) {
	c := testConsole(t)
	if err := c.Pair(context.Background(), "gawk-pair:api.gawk.ioio.fi:4433/ABC"); err == nil {
		t.Fatal("Pair succeeded with no link")
	}
}
