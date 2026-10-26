package main

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/bridge"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestRunRefusesABadConfig(t *testing.T) {
	var out syncBuf
	if code := run([]string{"-psk", ""}, func(string) string { return "" }, &out, nil); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "gawk-mumble-bridge:") {
		t.Fatalf("output %q", out.String())
	}
	if code := run([]string{"-h"}, func(string) string { return "" }, &out, nil); code != 0 {
		t.Fatalf("-h exit %d", code)
	}
	// A certificate that does not load fails start-up.
	args := []string{"-cert-file", "/nonexistent.crt", "-key-file", "/nonexistent.key", "-psk", "p",
		"-ticket-secret", "0123456789abcdef", "-relay-url", "https://relay:4433", "-mumble-addr", "m:1"}
	if code := run(args, func(string) string { return "" }, &out, nil); code != 1 {
		t.Fatalf("missing cert exit %d", code)
	}
}

// The binary starts from envs alone, serves the ops endpoints, logs no
// secret, and drains and exits 0 on SIGTERM.
func TestRunServesAndDrainsOnSIGTERM(t *testing.T) {
	envs := map[string]string{
		"GAWK_BRIDGE_DEV_CERT":        "true",
		"GAWK_BRIDGE_LISTEN":          "127.0.0.1:0",
		"GAWK_BRIDGE_INTERNAL_LISTEN": "127.0.0.1:0",
		"GAWK_BRIDGE_OPS_LISTEN":      "127.0.0.1:0",
		"GAWK_BRIDGE_PSK":             "very-secret-psk",
		"GAWK_BRIDGE_TICKET_SECRET":   "very-secret-ticket-key",
		"GAWK_BRIDGE_RELAY_URL":       "https://relay.example:4433",
		"GAWK_BRIDGE_MUMBLE_ADDR":     "127.0.0.1:1",
		"GAWK_BRIDGE_DRAIN_TIMEOUT":   "2s",
		"GAWK_BRIDGE_LOG_FORMAT":      "json",
	}
	var out syncBuf
	ready := make(chan *bridge.Server, 1)
	done := make(chan int, 1)
	go func() { done <- run(nil, func(k string) string { return envs[k] }, &out, ready) }()
	var srv *bridge.Server
	select {
	case srv = <-ready:
	case code := <-done:
		t.Fatalf("exited %d: %s", code, out.String())
	case <-time.After(5 * time.Second):
		t.Fatal("not started")
	}
	_, _, ops := srv.Addrs()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rsp, err := http.Get("http://" + ops + "/readyz")
		if err == nil {
			rsp.Body.Close()
			if rsp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("/readyz never ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, p := range []string{"/healthz", "/metrics", "/statusz"} {
		rsp, err := http.Get("http://" + ops + p)
		if err != nil || rsp.StatusCode != 200 {
			t.Fatalf("%s: %v", p, err)
		}
		rsp.Body.Close()
	}
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not exit on SIGTERM")
	}
	log := out.String()
	for _, secret := range []string{"very-secret-psk", "very-secret-ticket-key"} {
		if strings.Contains(log, secret) {
			t.Fatalf("log carries a secret: %s", log)
		}
	}
	if !strings.Contains(log, "drained, exiting") {
		t.Fatalf("log %s", log)
	}
}
