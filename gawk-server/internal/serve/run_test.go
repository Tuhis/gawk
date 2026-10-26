package serve

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// syncBuffer is a log sink the relay's goroutines can write while the test
// reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// noKube makes rest.InClusterConfig fail the way it does off-cluster, even
// when the test itself runs in a pod.
func noKube(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func devCertFunc(t *testing.T) CertFunc {
	t.Helper()
	cert, err := tlsutil.GenerateDevCert([]string{"localhost", "127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }
}

func parseCfg(t *testing.T, args ...string) config.Config {
	t.Helper()
	cfg, err := config.ParseFlags(args, func(string) string { return "" })
	if err != nil {
		t.Fatalf("ParseFlags(%v): %v", args, err)
	}
	return cfg
}

// waitGet polls url until it answers (the ops listener starts beside the
// Extra goroutines, not before them).
func waitGet(ctx context.Context, url, bearer string) (int, string, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rsp, err := http.DefaultClient.Do(req)
		if err == nil {
			b, _ := io.ReadAll(rsp.Body)
			rsp.Body.Close()
			return rsp.StatusCode, string(b), nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return 0, "", err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func writePopsFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pops.yaml")
	body := "- name: lan-party\n  uid: 0a1b2c3d-4e5f-4a6b-8c7d-9e8f7a6b5c4d\n  credentialSHA256: " + pops.CredentialSHA256("c") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The whole single-pod relay, as both binaries run it: rooms from a file,
// PoPs from a file, the admin API on the ops listener. PreRun sees every
// subsystem wired before any Extra runs; the configured caps reach
// /metrics as gawk_limit series (docs/61); the static room and the PoP
// admin route are served (adminOpts.Rooms / adminOpts.Pops are wired); and
// the first Extra error tears the relay down and is what Run returns.
func TestRunWiresASinglePodRelayAndReturnsTheFirstExtraError(t *testing.T) {
	noKube(t)
	t.Setenv("POD_NAME", "relay-test-0")
	roomsPath := filepath.Join(t.TempDir(), "rooms.json")
	if err := os.WriteFile(roomsPath, []byte(`[{"code":"TuhisRoom","displayName":"Tuhis' room"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	opsAddr := freeTCPAddr(t)
	cfg := parseCfg(t,
		"-addr", "127.0.0.1:0",
		"-metrics-addr", opsAddr,
		"-rooms",
		"-rooms-file", roomsPath,
		"-max-rooms", "7",
		"-max-broadcasts", "13",
		"-pops",
		"-pops-file", writePopsFile(t),
		"-admin-api-token", "adm",
		"-allowed-origins", "http://localhost:5173,https://gawk.example",
	)
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))

	var mu sync.Mutex
	var order []string
	note := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, s)
	}
	sentinel := errors.New("extra failed on purpose")
	var metricsBody, roomsBody, popsBody string
	var roomsCode, popsCode int
	var wired Wired
	quietCanceled := make(chan struct{})

	err := Run(t.Context(), cfg, devCertFunc(t), log, Options{
		Version: "v-test",
		PreRun: []func(w Wired) error{func(w Wired) error {
			note("pre")
			wired = w
			return nil
		}},
		Extra: []func(ctx context.Context, w Wired) error{
			func(ctx context.Context, w Wired) error {
				note("extra")
				base := "http://" + opsAddr
				code, body, err := waitGet(ctx, base+"/metrics", "")
				if err != nil || code != http.StatusOK {
					return fmt.Errorf("/metrics: %d %v", code, err)
				}
				metricsBody = body
				roomsCode, roomsBody, _ = waitGet(ctx, base+"/internal/admin/rooms", "adm")
				popsCode, popsBody, _ = waitGet(ctx, base+"/internal/admin/pops", "adm")
				return sentinel
			},
			// A well-behaved Extra: returns nil on cancellation, which must
			// not mask the sentinel.
			func(ctx context.Context, _ Wired) error {
				<-ctx.Done()
				close(quietCanceled)
				return nil
			},
		},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run = %v, want the Extra's error", err)
	}
	select {
	case <-quietCanceled:
	default:
		t.Error("the first error did not cancel the other goroutines")
	}
	if len(order) < 2 || order[0] != "pre" || order[1] != "extra" {
		t.Errorf("hook order = %v, want PreRun before Extra", order)
	}
	if wired.Registry == nil || wired.Server == nil || wired.Rooms == nil || wired.Prom == nil || wired.Bans == nil {
		t.Errorf("PreRun saw an unwired subsystem: %+v", wired)
	}
	if wired.Coord != nil || wired.Bus != nil {
		t.Errorf("single pod, no bus: Coord=%v Bus=%v, want both nil", wired.Coord, wired.Bus)
	}
	if wired.Reservations == nil {
		t.Error("-pops on a single pod must run an in-memory reservation set")
	}
	if !wired.Rooms.Has("tuhisroom") && !wired.Rooms.Has("TuhisRoom") {
		t.Error("the -rooms-file room never reached the registry")
	}
	for _, series := range []string{
		`gawk_limit{name="max_broadcasts"} 13`,
		`gawk_limit{name="max_rooms"} 7`,
		`gawk_limit{name="conn_rate_limit"} 3`,
		`gawk_limit{name="conn_burst_limit"} 10`,
	} {
		if !strings.Contains(metricsBody, series) {
			t.Errorf("/metrics lacks %s", series)
		}
	}
	if roomsCode != http.StatusOK || !strings.Contains(strings.ToLower(roomsBody), "tuhisroom") {
		t.Errorf("/internal/admin/rooms = %d %q, want the static room", roomsCode, roomsBody)
	}
	if popsCode != http.StatusOK {
		t.Errorf("/internal/admin/pops = %d %q, want 200 (the PoP manager's route)", popsCode, popsBody)
	}
	if !strings.Contains(logs.String(), "pop relays enabled") {
		t.Error("the PoP home was never wired")
	}
	if strings.Contains(logs.String(), "shutting down") {
		t.Error(`a failed relay logged the clean "shutting down"`)
	}
}

// The hub's lifecycle hooks Run installs reach their late-bound targets: a
// mint consults the rooms + PoP IDReserved chain, a publisher close and the
// broadcast's end run the rooms hooks, and the origin's end lands in the
// usage histograms (sm is assigned after the hub exists).
func TestRunWiresTheHubLifecycleHooks(t *testing.T) {
	noKube(t)
	cfg := parseCfg(t, "-addr", "127.0.0.1:0", "-metrics-addr", "off",
		"-rooms", "-pops", "-pops-file", writePopsFile(t))
	done := errors.New("done")
	var failure string
	err := Run(t.Context(), cfg, devCertFunc(t), slog.New(slog.DiscardHandler), Options{
		Extra: []func(context.Context, Wired) error{func(_ context.Context, w Wired) error {
			id, pub, err := w.Registry.StartPublish("")
			if err != nil {
				failure = fmt.Sprintf("StartPublish: %v", err)
				return done
			}
			if _, owned := w.Reservations.Owner(id); owned || w.Rooms.Has(id) {
				failure = "the mint drew a reserved ID " + id
			}
			pub.Close()
			w.Registry.EndBroadcast(id)
			if _, _, known := w.Registry.BroadcastState(id); known {
				failure = "the broadcast outlived EndBroadcast"
			}
			mfs, err := w.Prom.Gather()
			if err != nil {
				failure = err.Error()
				return done
			}
			var count uint64
			for _, mf := range mfs {
				if mf.GetName() == "gawk_broadcast_duration_seconds" {
					count = mf.GetMetric()[0].GetHistogram().GetSampleCount()
				}
			}
			if count != 1 {
				failure = fmt.Sprintf("gawk_broadcast_duration_seconds count = %d, want 1 (OnOriginEnded unwired)", count)
			}
			return done
		}},
	})
	if !errors.Is(err, done) {
		t.Fatalf("Run = %v", err)
	}
	if failure != "" {
		t.Fatal(failure)
	}
}

// A cancelled context is a clean shutdown: Run returns nil and says so.
func TestRunReturnsNilOnCancellation(t *testing.T) {
	noKube(t)
	cfg := parseCfg(t, "-addr", "127.0.0.1:0", "-metrics-addr", freeTCPAddr(t))
	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(t.Context())
	err := Run(ctx, cfg, devCertFunc(t), slog.New(slog.NewTextHandler(logs, nil)), Options{
		PreRun: []func(Wired) error{func(Wired) error { return nil }},
		Extra: []func(context.Context, Wired) error{func(ctx context.Context, _ Wired) error {
			cancel()
			<-ctx.Done()
			return nil
		}},
	})
	if err != nil {
		t.Fatalf("Run after cancel = %v, want nil", err)
	}
	if !strings.Contains(logs.String(), "shutting down") {
		t.Error(`a clean shutdown did not log "shutting down"`)
	}
}

// With a bus configured the publisher is built (and handed to the hooks)
// even when the bus is unreachable: the relay must never fail to start
// because its telemetry sink is down (docs/51).
func TestRunStartsWithAnUnreachableEventBus(t *testing.T) {
	noKube(t)
	cfg := parseCfg(t, "-addr", "127.0.0.1:0", "-metrics-addr", "off", "-eventbus-url", "nats://127.0.0.1:1")
	stop := errors.New("stop")
	var sawBus bool
	err := Run(t.Context(), cfg, devCertFunc(t), slog.New(slog.DiscardHandler), Options{
		Extra: []func(context.Context, Wired) error{func(_ context.Context, w Wired) error {
			sawBus = w.Bus != nil
			return stop
		}},
	})
	if !errors.Is(err, stop) {
		t.Fatalf("Run = %v, want the Extra's error", err)
	}
	if !sawBus {
		t.Error("-eventbus-url set but Wired.Bus is nil")
	}
}

// A PreRun failure aborts before the relay accepts a session: no Extra
// runs and the error is Run's.
func TestRunPreRunErrorAbortsBeforeServing(t *testing.T) {
	noKube(t)
	cfg := parseCfg(t, "-addr", "127.0.0.1:0", "-metrics-addr", "off")
	boom := errors.New("pre-run boom")
	err := Run(t.Context(), cfg, devCertFunc(t), slog.New(slog.DiscardHandler), Options{
		PreRun: []func(Wired) error{func(Wired) error { return boom }},
		Extra: []func(context.Context, Wired) error{func(context.Context, Wired) error {
			t.Error("an Extra ran after a PreRun failed")
			return nil
		}},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Run = %v, want the PreRun error", err)
	}
}

// Every way the wiring can refuse surfaces as Run's error rather than a
// half-started relay.
func TestRunFailsOnUnwireableConfig(t *testing.T) {
	noKube(t)
	cluster := []string{"-cluster-mode", "-internal-psk", "psk", "-internal-server-name", "relay.example"}
	for _, tc := range []struct {
		name string
		args []string
		// mutate bypasses ParseFlags' own validation, for a Config a caller
		// could still hand Run directly.
		mutate func(*config.Config)
		want   string
	}{
		{"cluster mode off-cluster", cluster, nil, "cluster-mode requires in-cluster kubernetes config"},
		{"cluster mode with rooms and pops off-cluster", append([]string{"-rooms", "-pops"}, cluster...), nil, "cluster-mode requires in-cluster kubernetes config"},
		{"pops with no source", nil, func(c *config.Config) { c.Pops = true }, "-pops requires -cluster-mode or -pops-file"},
		{"ops listener cannot bind", []string{"-metrics-addr", "256.0.0.1:1"}, nil, "ops listen"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := parseCfg(t, append([]string{"-addr", "127.0.0.1:0", "-metrics-addr", "off"}, tc.args...)...)
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- Run(ctx, cfg, devCertFunc(t), slog.New(slog.DiscardHandler), Options{}) }()
			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				cancel()
				<-done
				t.Fatalf("Run did not fail, want %q", tc.want)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// Off-cluster, both k8s builders refuse with an error naming the missing
// in-cluster config, never a nil coordinator/store.
func TestClusterBuildersRefuseOffCluster(t *testing.T) {
	noKube(t)
	log := slog.New(slog.DiscardHandler)
	c, pod, err := buildCoordinator(config.Config{Addr: ":4433"}, coordinatorHooks{}, log)
	if err == nil || c != nil || pod != "" || !strings.Contains(err.Error(), "cluster-mode requires in-cluster") {
		t.Errorf("buildCoordinator off-cluster = %v %q %v", c, pod, err)
	}
	st, pod, err := buildRoomStore(config.Config{Addr: ":4433"}, nil, nil, nil, nil, log)
	if err == nil || st != nil || pod != "" || !strings.Contains(err.Error(), "rooms in cluster-mode require in-cluster") {
		t.Errorf("buildRoomStore off-cluster = %v %q %v", st, pod, err)
	}
	src, status, err := popSource(t.Context(), config.Config{Pops: true, ClusterMode: true}, log)
	if err == nil || src != nil || status != nil || !strings.Contains(err.Error(), "pops in cluster-mode require in-cluster") {
		t.Errorf("popSource off-cluster = %v %v %v", src, status, err)
	}
}

// An explicit -pops-file wins in cluster mode too, with a warning that the
// lease is then per pod.
func TestPopSourceFileWinsInClusterModeAndWarns(t *testing.T) {
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	cfg := config.Config{Pops: true, ClusterMode: true, PopsFile: writePopsFile(t)}
	src, status, err := popSource(t.Context(), cfg, log)
	if err != nil || src == nil || status == nil {
		t.Fatalf("popSource = %v %v %v", src, status, err)
	}
	if _, ok := src.Get("lan-party"); !ok {
		t.Error("the file's Pop is not in the source")
	}
	if !strings.Contains(logs.String(), "-pops-file set in cluster mode") {
		t.Error("no warning for a per-pod PoP source on a fleet")
	}
	if got := popSourceName(cfg); got != "file" {
		t.Errorf("popSourceName = %q, want file", got)
	}
	if got := popSourceName(config.Config{Pops: true, ClusterMode: true}); got != "kubernetes" {
		t.Errorf("popSourceName without a file = %q, want kubernetes", got)
	}
}

// status.lease names the pod by the downward API when it is there: POD_IP
// with the relay's port; -addr as is when it has no port to keep.
func TestPopPodIdentity(t *testing.T) {
	host, _ := os.Hostname()
	for _, tc := range []struct {
		name, podName, podIP, addr string
		wantPod, wantAddr          string
	}{
		{"downward API", "relay-2", "10.0.0.5", ":4433", "relay-2", "10.0.0.5:4433"},
		{"no POD_IP", "relay-2", "", ":4433", "relay-2", ":4433"},
		{"no POD_NAME", "", "", "127.0.0.1:4433", host, "127.0.0.1:4433"},
		{"unsplittable addr", "relay-2", "10.0.0.5", "no-port", "relay-2", "no-port"},
		{"ipv6 pod ip", "relay-2", "fd00::5", "0.0.0.0:4433", "relay-2", "[fd00::5]:4433"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("POD_NAME", tc.podName)
			t.Setenv("POD_IP", tc.podIP)
			pod, addr := popPodIdentity(config.Config{Addr: tc.addr})
			if pod != tc.wantPod || addr != tc.wantAddr {
				t.Errorf("popPodIdentity = %q %q, want %q %q", pod, addr, tc.wantPod, tc.wantAddr)
			}
		})
	}
}

// Events are attributed to POD_NAME, else the hostname.
func TestPodIdentity(t *testing.T) {
	t.Setenv("POD_NAME", "relay-7")
	if got := podIdentity("gawk-relay"); got != "relay-7" {
		t.Errorf("podIdentity with POD_NAME = %q", got)
	}
	t.Setenv("POD_NAME", "")
	want := "gawk-relay"
	if h, err := os.Hostname(); err == nil && h != "" {
		want = h
	}
	if got := podIdentity("gawk-relay"); got != want {
		t.Errorf("podIdentity without POD_NAME = %q, want %q", got, want)
	}
}

// -log-format json builds a JSON handler, anything else text, and
// -log-level filters both.
func TestNewLogger(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(config.Config{LogFormat: "json", LogLevel: slog.LevelWarn}, &buf)
	log.Info("dropped")
	log.Warn("kept", "k", "v")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("json logger wrote %q: %v", buf.String(), err)
	}
	if rec["msg"] != "kept" || rec["k"] != "v" {
		t.Errorf("json record = %v", rec)
	}
	buf.Reset()
	NewLogger(config.Config{LogFormat: "text"}, &buf).Info("hello", "k", "v")
	if got := buf.String(); !strings.Contains(got, "msg=hello") || !strings.Contains(got, "k=v") {
		t.Errorf("text logger wrote %q", got)
	}
}

// LogCertIdentity always logs the hashes; it warns inside the 72 h window,
// errors once expired (never a negative "remaining"), and is silent on a
// nil leaf.
func TestLogCertIdentity(t *testing.T) {
	leafAt := func(issued time.Time, validity time.Duration) *tls.Certificate {
		c, err := tlsutil.GenerateDevCertAt([]string{"localhost"}, issued, validity)
		if err != nil {
			t.Fatal(err)
		}
		return &c
	}
	run := func(c *tls.Certificate) []map[string]any {
		var buf bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&buf, nil))
		if c == nil {
			LogCertIdentity(log, nil, "loaded certificate", "renew it")
		} else {
			LogCertIdentity(log, c.Leaf, "loaded certificate", "renew it", "cert_file", "/x.pem")
		}
		var out []map[string]any
		for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal(line, &m); err != nil {
				t.Fatal(err)
			}
			out = append(out, m)
		}
		return out
	}

	if recs := run(nil); len(recs) != 0 {
		t.Errorf("nil leaf logged %v", recs)
	}

	fresh := leafAt(time.Now(), 10*24*time.Hour)
	recs := run(fresh)
	if len(recs) != 1 || recs[0]["msg"] != "loaded certificate" || recs[0]["cert_file"] != "/x.pem" {
		t.Fatalf("fresh cert = %v, want only the identity line", recs)
	}
	if recs[0]["cert_hash_hex"] != tlsutil.CertHashHex(fresh.Leaf) || recs[0]["spki_fingerprint"] != tlsutil.SPKIFingerprint(fresh.Leaf) {
		t.Errorf("identity line = %v, want the leaf's hashes", recs[0])
	}

	recs = run(leafAt(time.Now(), 2*time.Hour))
	if len(recs) != 2 || recs[1]["level"] != "WARN" || recs[1]["msg"] != "certificate expires soon" || recs[1]["remedy"] != "renew it" {
		t.Fatalf("soon-expiring cert = %v, want a WARN with the remedy", recs)
	}

	recs = run(leafAt(time.Now().Add(-30*24*time.Hour), time.Hour))
	if len(recs) != 2 || recs[1]["level"] != "ERROR" || !strings.Contains(recs[1]["msg"].(string), "EXPIRED") {
		t.Fatalf("expired cert = %v, want an ERROR", recs)
	}
	if _, ok := recs[1]["remaining"]; ok {
		t.Error("an expired cert reports a (negative) remaining")
	}
	if ago, _ := recs[1]["expired_ago"].(float64); ago <= 0 {
		t.Errorf("expired_ago = %v, want a positive duration", recs[1]["expired_ago"])
	}
}
