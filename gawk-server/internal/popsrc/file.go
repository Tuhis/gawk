package popsrc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/Tuhis/gawk/gawk-server/pops"
)

const defaultPoll = time.Second

// LoadFile reads a -pops-file: a YAML (or JSON) list of pops.FilePop. It is
// all or nothing, because it is an authentication source: a name that is
// not a DNS-1123 label, a missing or malformed UID (a UUID: it is the PoP's
// offer handle), a duplicate name or UID, a credentialSHA256 that is not 64
// hex characters, an invalid wall code or an unknown field (a typo'd key
// would otherwise drop a setting silently) fails the whole file. Errors
// name the entry by index and Pop name, never its credential hash.
func LoadFile(path string) ([]pops.Pop, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var defs []pops.FilePop
	if err := yaml.UnmarshalStrict(data, &defs); err != nil {
		return nil, fmt.Errorf("popsrc: %s is not a list of Pops: %w", path, err)
	}
	names := make(map[string]bool, len(defs))
	uids := make(map[string]bool, len(defs))
	out := make([]pops.Pop, 0, len(defs))
	for i, d := range defs {
		where := fmt.Sprintf("popsrc: entry %d (%q)", i, d.Name)
		if err := pops.ValidateName(d.Name); err != nil {
			return nil, fmt.Errorf("%s: name: %w", where, err)
		}
		if !isUUID(d.UID) {
			return nil, fmt.Errorf("%s: uid must be a UUID", where)
		}
		d.UID = strings.ToLower(d.UID)
		if names[d.Name] {
			return nil, fmt.Errorf("%s: duplicate name", where)
		}
		if uids[d.UID] {
			return nil, fmt.Errorf("%s: duplicate uid", where)
		}
		names[d.Name], uids[d.UID] = true, true
		if b, err := hex.DecodeString(d.CredentialSHA256); err != nil || len(b) != 32 {
			return nil, fmt.Errorf("%s: credentialSHA256 must be 64 hex characters", where)
		}
		d.CredentialSHA256 = strings.ToLower(d.CredentialSHA256)
		if d.Wall != nil {
			if err := pops.ValidateWallCode(d.Wall.RoomCode); err != nil {
				return nil, fmt.Errorf("%s: wall: %w", where, err)
			}
		}
		out = append(out, d.ToPop())
	}
	return out, nil
}

// isUUID checks the canonical 8-4-4-4-12 hex form.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

type fileSource struct {
	*store
	path string
	log  *slog.Logger

	lastMod    time.Time
	lastSize   int64
	missing    bool
	unreadable bool
	malformed  bool
}

// StartFile loads path synchronously, then keeps the set in sync until ctx
// ends: re-read on mtime/size change (one-second poll) and on SIGHUP, the
// roomsrc posture. A missing file means no PoPs (and ends any it defined).
// A malformed file fails the start; on a later reload it keeps the previous
// set, so a typo never unlinks a party mid-event.
func StartFile(ctx context.Context, path string, log *slog.Logger) (Source, error) {
	return startFile(ctx, path, log, defaultPoll)
}

func startFile(ctx context.Context, path string, log *slog.Logger, poll time.Duration) (Source, error) {
	if log == nil {
		log = slog.Default()
	}
	f := &fileSource{store: newStore(), path: path, log: log}
	if err := f.reload("startup"); err != nil {
		return nil, err
	}
	f.synced.Store(true)

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
				_ = f.reload("sighup")
			case <-ticker.C:
				if f.changed() {
					_ = f.reload("file changed")
				}
			}
		}
	}()
	log.Info("pops file source started", "path", path, "poll_interval", poll)
	return f, nil
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

// reload returns an error only for a malformed or unreadable file; the
// caller decides whether that is fatal (startup) or kept-previous (later).
func (f *fileSource) reload(why string) error {
	list, err := LoadFile(f.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !f.missing {
			f.log.Warn("pops file absent: no PoPs", "path", f.path, "reason", why)
		}
		f.notify(f.replace(nil))
		f.missing, f.unreadable, f.malformed = true, false, false
		f.lastMod, f.lastSize = time.Time{}, -1
		return nil
	case err != nil:
		var pe *fs.PathError
		if errors.As(err, &pe) {
			if !f.unreadable {
				f.log.Warn("pops file unreadable: keeping the previous PoPs", "path", f.path, "reason", why, "err", err)
			}
			f.unreadable = true
			return err
		}
		if !f.malformed {
			f.log.Warn("pops file rejected: keeping the previous PoPs", "path", f.path, "reason", why, "err", err)
		}
		f.markLoaded()
		f.malformed = true
		return err
	}
	f.notify(f.replace(list))
	f.markLoaded()
	f.malformed = false
	f.log.Info("pops file loaded", "path", f.path, "reason", why, "pops", len(list))
	return nil
}

func (f *fileSource) markLoaded() {
	f.missing, f.unreadable = false, false
	if fi, err := os.Stat(f.path); err == nil {
		f.lastMod, f.lastSize = fi.ModTime(), fi.Size()
	}
}
