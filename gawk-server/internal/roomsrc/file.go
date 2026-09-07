// Package roomsrc feeds static room definitions into the room registry
// (R42, docs/44 §4.3 "non-cluster mode"): a JSON file of rooms.FileRoom
// entries, reloaded on change and on SIGHUP. It is the -rooms-file lane;
// the Kubernetes lane (Room CRs, RM3) lives in internal/roomcluster.
//
// Same posture as internal/moderationsrc's file source, for the same
// reasons: mtime polling rather than fsnotify (no new dependency), a
// missing file means "no static rooms" (and ends any it previously
// defined), an unreadable or malformed file keeps the previous set.
package roomsrc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Tuhis/gawk/gawk-server/rooms"
)

const defaultPoll = time.Second

// Options configures StartFile.
type Options struct {
	Registry Sink
	Log      *slog.Logger
	// PollInterval is a test seam; zero means one second.
	PollInterval time.Duration
}

// Sink is the registry surface the file source drives.
type Sink interface {
	ReplaceStatic(defs []rooms.FileRoom)
}

type fileSource struct {
	path string
	sink Sink
	log  *slog.Logger

	lastMod    time.Time
	lastSize   int64
	missing    bool
	unreadable bool
}

// StartFile loads path synchronously, then keeps it in sync until ctx ends.
// A missing file is not fatal.
func StartFile(ctx context.Context, path string, opts Options) error {
	if opts.Registry == nil {
		return errors.New("roomsrc: Registry is required")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	poll := opts.PollInterval
	if poll <= 0 {
		poll = defaultPoll
	}
	f := &fileSource{path: path, sink: opts.Registry, log: opts.Log}
	f.reload("startup")

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		defer signal.Stop(hup)
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				f.reload("sighup")
			case <-ticker.C:
				if f.changed() {
					f.reload("file changed")
				}
			}
		}
	}()
	opts.Log.Info("rooms file source started", "path", path, "poll_interval", poll)
	return nil
}

func (f *fileSource) changed() bool {
	fi, err := os.Stat(f.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return !f.missing
		}
		return !f.unreadable
	}
	return f.missing || f.unreadable || !fi.ModTime().Equal(f.lastMod) || fi.Size() != f.lastSize
}

func (f *fileSource) reload(why string) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			if !f.unreadable {
				f.log.Warn("rooms file unreadable: keeping the previous static rooms", "path", f.path, "reason", why, "err", err)
			}
			f.unreadable = true
			return
		}
		f.log.Warn("rooms file absent: no static rooms", "path", f.path, "reason", why)
		f.sink.ReplaceStatic(nil)
		f.missing, f.unreadable = true, false
		f.lastMod, f.lastSize = time.Time{}, -1
		return
	}
	var defs []rooms.FileRoom
	if err := json.Unmarshal(data, &defs); err != nil {
		f.log.Warn("rooms file is not a JSON array of rooms: keeping the previous static rooms", "path", f.path, "reason", why, "err", err)
		f.markLoaded()
		return
	}
	valid := defs[:0]
	for _, d := range defs {
		if _, err := rooms.NormalizeCode(d.Code); err != nil {
			f.log.Warn("rooms file entry skipped", "path", f.path, "err", fmt.Errorf("code: %w", err))
			continue
		}
		valid = append(valid, d)
	}
	f.sink.ReplaceStatic(valid)
	f.markLoaded()
	f.log.Info("rooms file loaded", "path", f.path, "reason", why, "rooms", len(valid), "skipped", len(defs)-len(valid))
}

func (f *fileSource) markLoaded() {
	f.missing, f.unreadable = false, false
	if fi, err := os.Stat(f.path); err == nil {
		f.lastMod, f.lastSize = fi.ModTime(), fi.Size()
	}
}
