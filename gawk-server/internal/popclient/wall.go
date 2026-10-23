package popclient

// wall.go: the party wall, PoP side (docs/76 §6.10, PP11). Every party
// broadcast joins the wall at publish unless its broadcaster hid it
// (wall=hidden on /publish, or a Hide). While linked the PoP vouches for it
// home with a wall record and sends its guests' room joins home over the
// link; unlinked it serves a local read-only wall of its own broadcasts.
// The broadcaster's 0x18 carries the wall code and its on-wall state, and
// is sent again when that state changes.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// wallSweep is how often ended broadcasts are taken off the wall.
const wallSweep = time.Second

// wall is the PoP's party wall state.
type wall struct {
	c *Client

	mu sync.Mutex
	// entries are the party broadcasts the wall knows, by ID.
	entries map[string]*wallEntry
	seq     int
	// code is the wall's normalized code, display its spelling and pop the
	// PoP's name, from home's last config ("" with no wall); local is the
	// code of the read-only wall in this relay's own registry.
	code, display, pop, local string
	homeRooms                 bool
	watchers                  map[string]map[int]func()
	nextWatch                 int
}

type wallEntry struct {
	label  string
	hidden bool
}

func newWall(c *Client) *wall {
	w := &wall{c: c, entries: map[string]*wallEntry{}, watchers: map[string]map[int]func(){}}
	c.OnRecord(poplink.TConfig, w.onConfig)
	c.Subscribe(func(LinkState) { w.notifyAll() })
	if s := c.opts.Server; s != nil {
		s.AddPublishObserver(w)
		s.SetRoomForwarder(w)
	}
	return w
}

func (w *wall) rooms() *roomsrv.Registry {
	if s := w.c.opts.Server; s != nil {
		return s.RoomRegistry()
	}
	return nil
}

// linkedWall reports whether wall traffic goes home now: linked, home runs
// rooms, and the PoP has a wall.
func (w *wall) linkedWall() bool {
	st := w.c.State()
	w.mu.Lock()
	defer w.mu.Unlock()
	return st.Header == HeaderLinked && w.homeRooms && w.code != ""
}

// PublisherLive is transport's PublishObserver: a party broadcast goes on
// the wall at publish, labelled with the broadcaster's ?name= or "Stream
// n", unless it is hidden (wall=hidden now, or a Hide remembered for this
// ID). It must not block: the sync runs on its own goroutine.
func (w *wall) PublisherLive(r *http.Request, id string) {
	q := r.URL.Query()
	w.mu.Lock()
	e := w.entries[id]
	if e == nil {
		w.seq++
		e = &wallEntry{label: fmt.Sprintf("Stream %d", w.seq)}
		w.entries[id] = e
	}
	if name := strings.TrimSpace(q.Get("name")); name != "" && utf8.ValidString(name) {
		e.label = name
	}
	if q.Get("wall") == "hidden" {
		e.hidden = true
	}
	w.mu.Unlock()
	go w.sync(id)
}

// sync makes the wall show id as its entry says: locally always (so an
// unlinked moment finds the local wall ready), home while linked.
func (w *wall) sync(id string) {
	w.mu.Lock()
	e := w.entries[id]
	code, local, pop := w.code, w.local, w.pop
	var on bool
	var label string
	if e != nil {
		on, label = !e.hidden && code != "", e.label
	}
	w.mu.Unlock()
	if e == nil || code == "" {
		return
	}
	if reg := w.rooms(); reg != nil && local != "" {
		if on {
			_ = reg.AttachVouched(local, pop, id, label)
		} else {
			_ = reg.DetachVouched(local, pop, id)
		}
	}
	if w.linkedWall() {
		op := poplink.WallDetach
		if on {
			op = poplink.WallAttach
		}
		if err := w.c.Send(poplink.Wall{Op: op, ID: id, Label: label}); err != nil {
			w.c.log.Debug("wall record not sent", "err", err)
		}
	}
	w.notify(id)
}

// onConfig follows home's wall: a new or changed wall code rebuilds the
// local wall, and every config (the first after a relink included) resyncs
// the attachments home, so a relink repairs whatever the outage missed.
func (w *wall) onConfig(rec any) {
	cfg, ok := rec.(*poplink.Config)
	if !ok {
		return
	}
	code := ""
	if cfg.HomeRooms && cfg.WallCode != "" {
		if norm, err := rooms.NormalizeCode(cfg.WallCode); err == nil {
			code = norm
		}
	}
	w.mu.Lock()
	oldLocal := w.local
	w.code, w.display, w.pop, w.homeRooms = code, cfg.WallCode, cfg.PopName, cfg.HomeRooms
	if code == "" {
		w.display = ""
	}
	ids := make([]string, 0, len(w.entries))
	for id := range w.entries {
		ids = append(ids, id)
	}
	w.mu.Unlock()

	if reg := w.rooms(); reg != nil {
		if oldLocal != "" && oldLocal != code {
			reg.EndRoom(oldLocal, wire.RoomEndReasonOperator)
		}
		newLocal := ""
		if code != "" {
			// The local wall: read-only (a spec.pop room grants no attach),
			// its tiles this relay's own broadcasts. Linked, the forwarder
			// sends its joins home instead.
			// It shows every party broadcast, so its cap is the relay's
			// own -max-broadcasts when that is the larger.
			if err := reg.UpsertStatic(roomsrv.StaticRoom{Code: code, DisplayCode: cfg.WallCode, DisplayName: cfg.DisplayName,
				Pop: cfg.PopName, MaxBroadcasts: max(cfg.MaxBroadcasts, w.c.opts.Cfg.MaxBroadcasts, 1)}); err != nil {
				w.c.log.Warn("local party wall not created", "err", err)
			} else {
				newLocal = code
				reg.OnWallLeave(w.left)
			}
		}
		w.mu.Lock()
		w.local = newLocal
		w.mu.Unlock()
	}
	// Off the link's reader: each sync may write a record.
	go func() {
		for _, id := range ids {
			w.sync(id)
		}
		w.notifyAll()
	}()
}

// run takes ended broadcasts off the wall until ctx ends.
func (w *wall) run(ctx context.Context) {
	t := time.NewTicker(wallSweep)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.sweep()
		}
	}
}

func (w *wall) sweep() {
	reg := w.c.opts.Registry
	if reg == nil {
		return
	}
	w.mu.Lock()
	var gone []string
	for id := range w.entries {
		if _, _, known := reg.BroadcastState(id); !known {
			gone = append(gone, id)
		}
	}
	for _, id := range gone {
		delete(w.entries, id)
	}
	local, pop := w.local, w.pop
	w.mu.Unlock()
	for _, id := range gone {
		if rr := w.rooms(); rr != nil && local != "" {
			_ = rr.DetachVouched(local, pop, id)
		}
		if w.linkedWall() {
			_ = w.c.Send(poplink.Wall{Op: poplink.WallDetach, ID: id})
		}
	}
}

// ForwardRoom is transport's RoomForwarder: while linked to a home that
// runs rooms, the wall goes home, and so does any code this relay does not
// hold (docs/76 item 16: codes typed on the PoP resolve upwards). Rooms
// minted here stay here.
func (w *wall) ForwardRoom(code string, local bool) bool {
	st := w.c.State()
	w.mu.Lock()
	defer w.mu.Unlock()
	if st.Header != HeaderLinked || !w.homeRooms {
		return false
	}
	return (w.code != "" && code == w.code) || !local
}

// OpenRoom opens a room stream on the link and writes its RoomOpen.
func (w *wall) OpenRoom(ctx context.Context, path, rawQuery string) (io.ReadWriteCloser, error) {
	w.c.mu.Lock()
	sess := w.c.ctrl
	w.c.mu.Unlock()
	if sess == nil {
		return nil, ErrNotLinked
	}
	s, err := sess.OpenStream(ctx)
	if err != nil {
		return nil, err
	}
	if err := poplink.Write(s, poplink.RoomOpen{Path: path, RawQuery: rawQuery}); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// WallCommand is transport's Hide/Show hook: an Attach (Show) or Detach
// (Hide) of a broadcast its sender proved it publishes here, on the wall.
func (w *wall) WallCommand(code string, cmd wire.RoomCommand) bool {
	id, err := broadcastid.Normalize(cmd.BroadcastID)
	if err != nil {
		return false
	}
	w.mu.Lock()
	if w.code == "" || code != w.code {
		w.mu.Unlock()
		return false
	}
	e := w.entries[id]
	if e == nil {
		w.seq++
		e = &wallEntry{label: fmt.Sprintf("Stream %d", w.seq)}
		w.entries[id] = e
	}
	switch cmd.Kind {
	case wire.RoomCommandAttach:
		e.hidden = false
		if l := strings.TrimSpace(cmd.Label); l != "" {
			e.label = l
		}
	case wire.RoomCommandDetach:
		e.hidden = true
	default:
		w.mu.Unlock()
		return false
	}
	w.mu.Unlock()
	go w.sync(id)
	return true
}

// left is the local registry's OnWallLeave: a broadcast left the local
// wall by its broadcaster's choice (a room minted here). That is a Hide:
// home stops showing it, and a resync does not put it back.
func (w *wall) left(code, id string) {
	w.leave(code, id)
}

// LeftWall is transport's wallLeaver: a broadcaster at this PoP attached
// its broadcast to a room at home through the link, which home confirmed.
// Like a mint here, that moves it off the wall (docs/76 item 34).
func (w *wall) LeftWall(id string) {
	w.mu.Lock()
	code := w.code
	w.mu.Unlock()
	w.leave(code, id)
}

// leave marks id hidden on the wall named code and syncs it (home's tile
// goes, and the 0x18 follows).
func (w *wall) leave(code, id string) {
	w.mu.Lock()
	e := w.entries[id]
	if w.code == "" || code != w.code || e == nil || e.hidden {
		w.mu.Unlock()
		return
	}
	e.hidden = true
	w.mu.Unlock()
	go w.sync(id)
}

// fill adds the wall fields to a session's 0x18: the wall code whenever
// the PoP has a wall, and on a publish session whether that broadcast is
// on it.
func (w *wall) fill(info *wire.LocalRelayInfo, publishID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.code == "" {
		return
	}
	info.WallCode = w.display
	if len(info.WallCode) > wire.MaxRoomCodeLen {
		info.WallCode = w.code
	}
	if publishID != "" {
		if e := w.entries[publishID]; e != nil && !e.hidden {
			info.OnWall = true
		}
	}
}

// WallRow is the console's Live-table wall column for a broadcast
// (relayconsole.LiveRow Wall and Label): wall is "on" or "hidden" for a
// party broadcast while the PoP has a wall and "" otherwise, and label is
// its tile label ("" for an ID the wall never saw).
func (c *Client) WallRow(id string) (wall, label string) {
	w := c.wall
	w.mu.Lock()
	defer w.mu.Unlock()
	e := w.entries[id]
	if e == nil {
		return "", ""
	}
	switch {
	case w.code == "":
	case e.hidden:
		wall = "hidden"
	default:
		wall = "on"
	}
	return wall, e.label
}

// WatchLocalRelayInfo is transport's LocalRelayWatcher: changed runs when a
// publish session's 0x18 may have changed (Hide, Show, the wall or the link
// changing), and the transport re-sends it if it did.
func (c *Client) WatchLocalRelayInfo(publishID string, changed func()) (cancel func()) {
	w := c.wall
	w.mu.Lock()
	id := w.nextWatch
	w.nextWatch++
	if w.watchers[publishID] == nil {
		w.watchers[publishID] = map[int]func(){}
	}
	w.watchers[publishID][id] = changed
	w.mu.Unlock()
	return func() {
		w.mu.Lock()
		delete(w.watchers[publishID], id)
		if len(w.watchers[publishID]) == 0 {
			delete(w.watchers, publishID)
		}
		w.mu.Unlock()
	}
}

func (w *wall) notify(id string) {
	w.mu.Lock()
	fns := make([]func(), 0, len(w.watchers[id]))
	for _, fn := range w.watchers[id] {
		fns = append(fns, fn)
	}
	w.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

func (w *wall) notifyAll() {
	w.mu.Lock()
	var fns []func()
	for _, m := range w.watchers {
		for _, fn := range m {
			fns = append(fns, fn)
		}
	}
	w.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

var (
	_ transport.PublishObserver = (*wall)(nil)
	_ transport.RoomForwarder   = (*wall)(nil)
)
