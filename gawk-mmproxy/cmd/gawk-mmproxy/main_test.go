package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-mmproxy/internal/config"
)

func TestRun(t *testing.T) {
	for _, c := range []struct {
		args []string
		code int
		out  string
	}{
		{nil, 2, "usage"},
		{[]string{"nope"}, 2, "usage"},
		{[]string{"version"}, 0, "gawk-mmproxy dev"},
		{[]string{"serve", "-max-conns", "0"}, 2, "invalid -max-conns"},
		{[]string{"routes", "-route-table", "255"}, 2, "invalid -route-table"},
		{[]string{"serve", "-h"}, 0, "-require-routes"},
	} {
		var b bytes.Buffer
		if code := run(c.args, func(string) string { return "" }, &b); code != c.code || !strings.Contains(b.String(), c.out) {
			t.Errorf("%v: %d %q, want %d %q", c.args, code, b.String(), c.code, c.out)
		}
	}
}

// `routes` always exits 0 (docs/92 D4, D15), even when it can't install.
func TestRoutesAlwaysExitsZero(t *testing.T) {
	var b bytes.Buffer
	if code := run([]string{"routes", "-route-table", "4244"}, func(string) string { return "" }, &b); code != 0 {
		t.Fatalf("exit %d: %s", code, b.String())
	}
}

// serve end to end in-process: it starts without the routes (D15: it
// doesn't exit), says why, serves /healthz and /metrics, refuses with a FIN
// while the routes are missing, and stops when its context ends.
func TestServeRunsAndStops(t *testing.T) {
	c, err := config.ParseServe([]string{"-listen", "127.0.0.1:0", "-ops-listen", "127.0.0.1:0", "-route-table", "4245"}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan [2]string, 1)
	done := make(chan int, 1)
	go func() {
		done <- runServe(ctx, c, logger(&logs, slog.LevelDebug, "text"), func(p, o string) { addrs <- [2]string{p, o} })
	}()
	a := <-addrs
	for _, path := range []string{"/healthz", "/metrics", "/readyz"} {
		r, err := http.Get("http://" + a[1] + path)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if want := map[string]int{"/healthz": 200, "/metrics": 200, "/readyz": 503}[path]; r.StatusCode != want {
			t.Errorf("%s: %d, want %d", path, r.StatusCode, want)
		}
	}
	conn, err := net.Dial("tcp", a[0])
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("\r\n\r\n\x00\r\nQUIT\n\x21\x11\x00\x0c\xcb\x00\x71\x07\x00\x00\x00\x00\x9c\x41\x00\x00hello"))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.Copy(io.Discard, conn); err != nil {
		t.Errorf("refusal without routes: %v, want EOF", err)
	}
	conn.Close()
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exit %d", code)
	}
	if !strings.Contains(logs.String(), "routes are missing") {
		t.Errorf("no log line about the missing routes:\n%s", logs.String())
	}
}
