package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble/mumbletest"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func lastReport(t *testing.T, out string) Report {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var r Report
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &r); err != nil {
		t.Fatalf("%q: %v", out, err)
	}
	return r
}

// One sim plays the fixture, another echoes it back: both directions show
// in the JSON lines, with the end-of-transmission bit.
func TestPlayAndEcho(t *testing.T) {
	m := mumbletest.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	var echoOut, playOut lockedBuf
	done := make(chan int, 2)
	go func() {
		done <- run(ctx, []string{"-addr", m.Addr(), "-insecure", "-name", "echo", "-channel", "Room", "-echo", "-duration", "4s"}, nil, &echoOut, io.Discard)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if u, ok := m.UserByName("echo"); ok && u.Channel != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("echo sim never joined Room")
		}
		time.Sleep(10 * time.Millisecond)
	}
	go func() {
		done <- run(ctx, []string{"-addr", m.Addr(), "-insecure", "-name", "player", "-channel", "Room", "-play", "../../testdata/tone-440hz-10ms.opus", "-duration", "3500ms"}, nil, &playOut, io.Discard)
	}()
	for i := 0; i < 2; i++ {
		if code := <-done; code != 0 {
			t.Fatalf("exit %d: %s %s", code, echoOut.String(), playOut.String())
		}
	}
	e, p := lastReport(t, echoOut.String()), lastReport(t, playOut.String())
	if e.Received["player"] < 200 || e.Ends["player"] != 1 || e.Sent < 200 {
		t.Fatalf("echo %+v", e)
	}
	if p.Received["echo"] < 200 || p.Ends["echo"] != 1 || p.Sent < 250 {
		t.Fatalf("player %+v", p)
	}
}

func TestAdminActionsAndErrors(t *testing.T) {
	m := mumbletest.Start(t)
	ctx := context.Background()
	var out lockedBuf
	go run(ctx, []string{"-addr", m.Addr(), "-insecure", "-name", "victim", "-duration", "3s"}, nil, io.Discard, io.Discard)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok := m.UserByName("victim"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("victim never joined")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code := run(ctx, []string{"-addr", m.Addr(), "-insecure", "-name", "admin", "-mute", "victim", "-duration", "300ms"}, nil, &out, io.Discard); code != 0 {
		t.Fatalf("mute exit %d", code)
	}
	if u, _ := m.UserByName("victim"); !u.Mute {
		t.Fatal("not muted")
	}
	if code := run(ctx, []string{"-addr", m.Addr(), "-insecure", "-name", "admin", "-kick", "victim", "-duration", "300ms"}, nil, &out, io.Discard); code != 0 {
		t.Fatalf("kick exit %d", code)
	}
	if code := run(ctx, []string{"-addr", m.Addr(), "-insecure", "-name", "admin2", "-kick", "nobody", "-duration", "300ms"}, nil, &out, io.Discard); code != 1 {
		t.Fatalf("unknown user exit %d", code)
	}
	if code := run(ctx, []string{"-temp-code", "x"}, nil, &out, io.Discard); code != 2 {
		t.Fatal("temp-code without secret")
	}
	if code := run(ctx, []string{"-nope"}, nil, &out, io.Discard); code != 2 {
		t.Fatal("bad flag")
	}
	if code := run(ctx, []string{"fixture"}, strings.NewReader("junk"), &out, io.Discard); code != 1 {
		t.Fatal("bad ogg")
	}
	if code := run(ctx, []string{"-addr", m.Addr(), "-insecure", "-play", "/nonexistent"}, nil, &out, io.Discard); code != 1 {
		t.Fatal("missing fixture")
	}
}
