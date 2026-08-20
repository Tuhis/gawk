package moderationsrc

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// fileSource reloads a JSON array of moderation.Records (docs/42 §4.14).
//
// DEVIATION from docs/42, deliberate: the doc says "fsnotify + SIGHUP".
// gawk-server has no fsnotify dependency and the repository's existing
// watch-a-mounted-file mechanism — internal/tlsutil/reload.go, which picks up
// cert-manager renewals — polls mtime instead. Following that keeps the relay
// image's dependency set unchanged for a dev-lane feature, and a ban file is
// edited by hand, not by a hot loop. SIGHUP is implemented as specified and
// is the instant path when a second of latency is a second too many.
type fileSource struct {
	path string
	sink Sink
	log  *slog.Logger

	lastMod  time.Time
	lastSize int64
	// missing records that the last load found no file, so a still-absent
	// file is not re-reported (and re-warned) on every poll tick.
	missing bool
}

func startFile(ctx context.Context, path string, opts Options) error {
	poll := opts.PollInterval
	if poll <= 0 {
		poll = defaultPoll
	}
	f := &fileSource{path: path, sink: Sink{Set: opts.Set, OnAdded: opts.OnBanAdded}, log: opts.Log}

	// Load once synchronously so a relay is enforcing before it accepts its
	// first publish. A missing file is not fatal — it is simply "no bans yet"
	// and the poller adopts it the moment it appears — but it is logged,
	// because an operator who pointed at the wrong path deserves to see it.
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

	opts.Log.Info("moderation file source started", "path", path, "poll_interval", poll)
	return nil
}

// changed reports whether the file's mtime or size moved since the last
// load — the same cheap stat check tlsutil.Reloader uses.
func (f *fileSource) changed() bool {
	fi, err := os.Stat(f.path)
	if err != nil {
		// A file that vanished counts as a change exactly once, so the ban
		// set is cleared rather than left frozen at its last contents — and
		// a file that was never there stays quiet.
		return !f.missing
	}
	return f.missing || !fi.ModTime().Equal(f.lastMod) || fi.Size() != f.lastSize
}

func (f *fileSource) reload(why string) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			f.log.Warn("moderation ban file absent: enforcing an empty ban set",
				"path", f.path, "reason", why)
		} else {
			f.log.Warn("moderation ban file unreadable: enforcing an empty ban set",
				"path", f.path, "reason", why, "err", err)
		}
		f.sink.Set.Replace(nil)
		f.missing = true
		f.lastMod, f.lastSize = time.Time{}, -1
		return
	}

	var records []moderation.Record
	if err := json.Unmarshal(data, &records); err != nil {
		// Keep enforcing the last good list: a fat-fingered edit must not
		// silently un-ban everyone.
		f.log.Warn("moderation ban file is not a JSON array of records: keeping the previous ban set",
			"path", f.path, "reason", why, "err", err)
		f.markLoaded()
		return
	}

	valid := make([]moderation.Record, 0, len(records))
	for _, r := range records {
		norm, err := moderation.Normalize(r)
		if err != nil {
			f.log.Warn("moderation ban file entry skipped",
				"path", f.path, "target_type", string(r.Target.Type), "err", err)
			continue
		}
		valid = append(valid, norm)
	}
	f.sink.replace(valid)
	f.markLoaded()
	f.log.Info("moderation ban file loaded",
		"path", f.path, "reason", why, "records", len(valid), "skipped", len(records)-len(valid))
}

func (f *fileSource) markLoaded() {
	f.missing = false
	if fi, err := os.Stat(f.path); err == nil {
		f.lastMod, f.lastSize = fi.ModTime(), fi.Size()
	}
}
