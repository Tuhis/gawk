package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestRunRefusesABadConfig(t *testing.T) {
	var out syncBuf
	if code := run(nil, func(string) string { return "" }, &out, nil); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "gawk-whip-gateway:") {
		t.Fatalf("output %q", out.String())
	}
	if code := run([]string{"-h"}, func(string) string { return "" }, io.Discard, nil); code != 0 {
		t.Fatalf("-h exit %d", code)
	}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

// WG4: the binary starts from GAWK_WHIP_* envs alone, serves /healthz and
// /metrics (the gawk_whip_ series), logs no secret, and exits 0 on SIGTERM.
func TestRunServesHealthAndMetricsAndStopsOnSIGTERM(t *testing.T) {
	envs := map[string]string{
		"GAWK_WHIP_LISTEN":                    "127.0.0.1:0",
		"GAWK_WHIP_INTERNAL_LISTEN":           "127.0.0.1:0",
		"GAWK_WHIP_OPS_LISTEN":                "127.0.0.1:0",
		"GAWK_WHIP_MEDIA_BIND":                "127.0.0.1",
		"GAWK_WHIP_MEDIA_BASE_PORT":           "1", // + ordinal; replaced below
		"GAWK_WHIP_RELAY_URL":                 "https://relay.example:4433",
		"GAWK_WHIP_RELAY_PUBLISH_SECRET":      "very-secret-publish",
		"GAWK_WHIP_RELAY_PUBLISH_PROXY_TOKEN": "very-secret-proxy",
		"GAWK_WHIP_INTERNAL_TOKEN":            "very-secret-internal",
		"GAWK_WHIP_PEERS":                     "http://127.0.0.1:1",
		"GAWK_WHIP_LOG_FORMAT":                "json",
	}
	// A free media port: bind one, release it, use it.
	envs["GAWK_WHIP_MEDIA_BASE_PORT"] = freePort(t)
	var out syncBuf
	ready := make(chan Listening, 1)
	done := make(chan int, 1)
	go func() { done <- run(nil, func(k string) string { return envs[k] }, &out, ready) }()
	var l Listening
	select {
	case l = <-ready:
	case code := <-done:
		t.Fatalf("exited %d: %s", code, out.String())
	case <-time.After(10 * time.Second):
		t.Fatal("not ready")
	}
	for _, u := range []string{"http://" + l.Ops + "/healthz", "http://" + l.Ops + "/readyz", "http://" + l.Public + "/healthz"} {
		if code, _ := get(t, u); code != http.StatusOK {
			t.Fatalf("%s: %d", u, code)
		}
	}
	code, metrics := get(t, "http://"+l.Ops+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics %d", code)
	}
	for _, m := range []string{"gawk_whip_sessions", "gawk_whip_build_info"} {
		if !strings.Contains(metrics, m) {
			t.Errorf("/metrics lacks %s", m)
		}
	}
	if code, _ := get(t, "http://"+l.Internal+"/internal/v1/keys/00"); code != http.StatusUnauthorized {
		t.Fatalf("internal listener without the token: %d", code)
	}
	if code, _ := get(t, "http://"+l.Public+"/metrics"); code == http.StatusOK {
		t.Fatal("metrics are served on the public listener")
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("did not stop on SIGTERM")
	}
	if strings.Contains(out.String(), "very-secret") {
		t.Fatal("a secret reached the log")
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	for i := 0; i < 50; i++ {
		u, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		p := u.LocalAddr().(*net.UDPAddr).Port
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		u.Close()
		if err == nil {
			l.Close()
			return strconv.Itoa(p)
		}
	}
	t.Fatal("no free port")
	return ""
}
