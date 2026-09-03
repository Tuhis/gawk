package roomsrc

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/rooms"
)

type recordingSink struct {
	mu    sync.Mutex
	calls [][]rooms.FileRoom
}

func (r *recordingSink) ReplaceStatic(defs []rooms.FileRoom) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]rooms.FileRoom(nil), defs...))
}

func (r *recordingSink) last() ([]rooms.FileRoom, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return nil, 0
	}
	return r.calls[len(r.calls)-1], len(r.calls)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func waitCalls(t *testing.T, s *recordingSink, n int) []rooms.FileRoom {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if last, c := s.last(); c >= n {
			return last
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("sink saw fewer than %d loads", n)
	return nil
}

func TestFileSourceLoadsReloadsAndKeepsTheLastGoodSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rooms.json")
	write(t, path, `[{"code":"TuhisRoom","displayName":"Tuhis' room","attachSecret":"k"},{"code":"x"}]`)
	sink := &recordingSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := StartFile(ctx, path, Options{Registry: sink, Log: log, PollInterval: 10 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	defs, _ := sink.last()
	if len(defs) != 1 || defs[0].Code != "TuhisRoom" || defs[0].AttachSecret != "k" {
		t.Fatalf("startup load = %+v (the invalid 'x' entry must be skipped)", defs)
	}
	// A change is picked up.
	time.Sleep(15 * time.Millisecond) // a distinct mtime on coarse filesystems
	write(t, path, `[{"code":"other"}]`)
	defs = waitCalls(t, sink, 2)
	if len(defs) != 1 || defs[0].Code != "other" {
		t.Fatalf("reload = %+v", defs)
	}
	// Malformed JSON keeps the previous set (no new ReplaceStatic call).
	_, before := sink.last()
	time.Sleep(15 * time.Millisecond)
	write(t, path, `{not json`)
	time.Sleep(60 * time.Millisecond)
	if _, after := sink.last(); after != before {
		t.Fatal("malformed file replaced the static rooms")
	}
	// A removed file clears them.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	defs = waitCalls(t, sink, before+1)
	if len(defs) != 0 {
		t.Fatalf("absent file left rooms: %+v", defs)
	}
}

func TestFileSourceRequiresARegistry(t *testing.T) {
	if err := StartFile(context.Background(), "x", Options{}); err == nil {
		t.Fatal("no registry accepted")
	}
}
