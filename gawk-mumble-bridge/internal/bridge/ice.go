package bridge

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/murmurice"
	"github.com/Tuhis/gawk/gawk-server/voice"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// iceMirror is Ice mode's roster source (docs/89 D2): one per process, in
// every pod, whether or not it holds an observer link. It keeps one
// ServerCallback registration with murmur (D14, D15), a mirror of the
// server's users and channels that the events keep current, and re-reads
// the state every -mumble-ice-reconcile for what murmur can drop silently.
// Every watcher (the Ice-mode observer, D3) reads the same mirror.
type iceMirror struct {
	s     *Server
	o     IceOptions
	cli   *murmurice.Client
	ep    *murmurice.Endpoint
	proxy murmurice.Proxy

	cancel context.CancelFunc
	done   chan struct{}

	mu        sync.Mutex
	users     map[uint32]murmurice.User
	chans     map[uint32]murmurice.Channel
	available bool
	reason    string
	sid       int
	version   murmurice.Version
	haveVer   bool
	changed   chan struct{}
	// reading buffers events while a read is in flight (D2, D15).
	reading  bool
	buffered []murmurice.Event
	fsm      *callbackFSM
	// registered is gawk_bridge_ice_registered: set by a successful
	// addCallback, cleared by removeCallback and by a client reconnect.
	registered    bool
	lastEvent     time.Time
	lastReconcile time.Time
	left          bool
}

func newIceMirror(s *Server) *iceMirror {
	o := s.opts.Mumble.Ice
	if o.Timeout <= 0 {
		o.Timeout = 2 * time.Second
	}
	if o.CallbackTimeout <= 0 {
		o.CallbackTimeout = 100 * time.Millisecond
	}
	if o.Reconcile <= 0 {
		o.Reconcile = 30 * time.Second
	}
	if o.CallbackProbe <= 0 {
		o.CallbackProbe = 10 * time.Minute
	}
	return &iceMirror{
		s:       s,
		o:       o,
		cli:     murmurice.NewClient(o.Addr, o.Secret, o.Timeout),
		users:   map[uint32]murmurice.User{},
		chans:   map[uint32]murmurice.Channel{},
		reason:  ReasonIceUnavailable,
		sid:     -1,
		changed: make(chan struct{}),
		fsm:     newCallbackFSM(o.CallbackProbe),
		done:    make(chan struct{}),
	}
}

// listen binds the callback endpoint (D14), so a port clash fails start-up.
func (m *iceMirror) listen() error {
	id := murmurice.NewIdentity()
	ep, err := murmurice.Listen(m.o.CallbackListen, id, m.onEvent)
	if err != nil {
		return fmt.Errorf("mumble-ice-callback-listen %s: %w", m.o.CallbackListen, err)
	}
	m.ep = ep
	m.proxy = murmurice.Proxy{Object: id, Host: m.o.CallbackHost, Port: ep.Addr().Port, Timeout: m.o.CallbackTimeout}
	return nil
}

// Changed is closed at the next change of the mirror or its availability.
func (m *iceMirror) Changed() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changed
}

func (m *iceMirror) notifyLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// onEvent is the endpoint's handler: murmur's events, oneway (F12).
func (m *iceMirror) onEvent(ev murmurice.Event, err error) {
	s := m.s
	if err != nil {
		s.recordFailure(ReasonIceProtocol, "")
		s.log.Warn("malformed Ice callback", "reason", ReasonIceProtocol, "err", err)
		return
	}
	s.metrics.iceEvents.WithLabelValues(ev.Op).Inc()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fsm.state == cbUnsafe {
		return // never registered: not murmur's
	}
	was := m.fsm.state
	m.fsm.onEvent()
	if was != cbRegistered {
		s.log.Info("Ice callbacks delivered again", "was", was)
	}
	m.lastEvent = s.now()
	if m.reading {
		m.buffered = append(m.buffered, ev)
		return
	}
	if applyEvent(m.users, m.chans, ev) {
		m.notifyLocked()
	}
}

// applyEvent applies one event to a mirror. Every event carries the full
// state of its user or channel, so applying one twice is harmless.
func applyEvent(users map[uint32]murmurice.User, chans map[uint32]murmurice.Channel, ev murmurice.Event) bool {
	switch ev.Op {
	case murmurice.OpUserConnected, murmurice.OpUserStateChanged:
		if old, ok := users[ev.User.Session]; ok && old == ev.User {
			return false
		}
		users[ev.User.Session] = ev.User
	case murmurice.OpUserDisconnected:
		if _, ok := users[ev.User.Session]; !ok {
			return false
		}
		delete(users, ev.User.Session)
	case murmurice.OpChannelCreated, murmurice.OpChannelStateChanged:
		if old, ok := chans[ev.Channel.ID]; ok && old == ev.Channel {
			return false
		}
		chans[ev.Channel.ID] = ev.Channel
	case murmurice.OpChannelRemoved:
		if _, ok := chans[ev.Channel.ID]; !ok {
			return false
		}
		delete(chans, ev.Channel.ID)
	default: // userTextMessage: ignored
		return false
	}
	return true
}

// errReconnected is a call that the client retried on a new connection: the
// pod treats it as a reconnect (D11).
var errReconnected = errors.New("the Ice client reconnected")

// run keeps the mirror until stop.
func (m *iceMirror) run(ctx context.Context) {
	defer close(m.done)
	s := m.s
	backoff := s.timings.ObserverBackoff
	first := true
	for ctx.Err() == nil {
		start := time.Now()
		err := m.session(ctx, first)
		first = false
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errReconnected) {
			continue // re-read at once; the session marked nothing
		}
		reason := iceReason(err)
		m.setUnavailable(reason)
		s.recordFailure(reason, "")
		s.log.Warn("Ice unavailable", "reason", reason, "err", err)
		m.cli.Reset()
		if time.Since(start) > s.timings.ObserverBackoffMax {
			backoff = s.timings.ObserverBackoff
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(2*backoff, s.timings.ObserverBackoffMax)
	}
}

func (m *iceMirror) setUnavailable(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.available || m.reason != reason {
		m.available, m.reason = false, reason
		m.notifyLocked()
	}
}

// iceReason maps an Ice error to its D11 reason.
func iceReason(err error) string {
	var mf *mumbleFailure
	var pe *murmurice.ProtocolError
	switch {
	case errors.As(err, &mf):
		return mf.reason
	case murmurice.IsUserException(err, murmurice.ExInvalidSecret):
		return ReasonIceSecret
	case errors.As(err, &pe):
		return ReasonIceProtocol
	}
	return ReasonIceUnavailable
}

// session is one Ice connection's life: connect, version, registration,
// the first read, then events and reconciles until it fails.
func (m *iceMirror) session(ctx context.Context, first bool) error {
	s := m.s
	gen, err := m.cli.Connect(ctx)
	if err != nil {
		return err
	}
	if !first || gen > 1 {
		m.mu.Lock()
		m.registered = false // a reconnect: murmur may have restarted
		m.mu.Unlock()
		s.metrics.iceRegistered.Set(0)
	}
	ver, err := m.cli.Version(ctx)
	if err != nil {
		return err
	}
	sid := m.o.ServerID
	if sid < 0 {
		ids, err := m.cli.BootedServers(ctx)
		if err != nil {
			return err
		}
		switch len(ids) {
		case 1:
			sid = ids[0]
		case 0:
			return errors.New("murmur has no booted virtual server")
		default:
			return fail(ReasonIceServerAmbiguous, fmt.Errorf("%d booted virtual servers; set -mumble-ice-server-id", len(ids)))
		}
	}
	m.mu.Lock()
	wasUnsafe := m.fsm.state == cbUnsafe
	m.version, m.haveVer, m.sid = ver, true, sid
	act := m.fsm.onConnect(ver.CallbacksUnsafe(), s.now())
	m.mu.Unlock()
	if ver.CallbacksUnsafe() && !wasUnsafe {
		s.log.Warn("murmur's Ice callbacks are unsafe on this version: the roster follows changes at reconciliation pace only (docs/89 D16)",
			"murmur_version", ver.String(), "reconcile", m.o.Reconcile)
	}
	if err := m.perform(ctx, act); err != nil {
		return err
	}
	if _, err := m.read(ctx, false); err != nil {
		return err
	}
	if m.cli.Generation() != gen {
		return errReconnected
	}
	m.mu.Lock()
	if !m.available {
		m.available, m.reason = true, ""
		m.notifyLocked()
	}
	m.mu.Unlock()
	s.log.Info("Ice roster available", "murmur_version", ver.String(), "server_id", sid, "callbacks", m.callbacks())

	tick := time.NewTicker(m.o.Reconcile)
	defer tick.Stop()
	for {
		var probe <-chan time.Time
		var probeTimer *time.Timer
		m.mu.Lock()
		at, due := m.fsm.probeDue()
		m.mu.Unlock()
		if due {
			probeTimer = time.NewTimer(max(at.Sub(s.now()), 0))
			probe = probeTimer.C
		}
		stopProbe := func() {
			if probeTimer != nil {
				probeTimer.Stop()
			}
		}
		select {
		case <-ctx.Done():
			stopProbe()
			return nil
		case <-m.cli.Lost():
			stopProbe()
			return murmurice.ErrConnectionLost
		case <-probe:
			m.mu.Lock()
			act := m.fsm.onProbe(s.now())
			m.mu.Unlock()
			if act.add {
				s.log.Info("probing Ice callbacks again")
			}
			if err := m.perform(ctx, act); err != nil {
				return err
			}
		case <-tick.C:
			stopProbe()
			drift, err := m.read(ctx, true)
			if err != nil {
				s.metrics.iceReconciles.WithLabelValues("error").Inc()
				return err
			}
			if m.cli.Generation() != gen {
				return errReconnected
			}
			m.mu.Lock()
			m.lastReconcile = s.now()
			var act cbAction
			gaveUp := false
			if drift {
				act, gaveUp = m.fsm.onDrift(s.now())
			}
			m.mu.Unlock()
			if !drift {
				s.metrics.iceReconciles.WithLabelValues("ok").Inc()
				continue
			}
			s.metrics.iceReconciles.WithLabelValues("drift").Inc()
			s.metrics.iceDrift.Inc()
			if gaveUp {
				s.log.Warn("murmur is not delivering Ice callbacks to this pod: giving up on them; the roster follows changes at reconciliation pace (docs/89 D15)",
					"callback_host", m.o.CallbackHost, "callback_port", m.proxy.Port, "next_probe", m.o.CallbackProbe)
			}
			if err := m.perform(ctx, act); err != nil {
				return err
			}
		}
	}
}

func (m *iceMirror) callbacks() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fsm.state
}

// perform runs an FSM action against murmur. A refused or failed
// addCallback was counted when it was decided (D15); the outcome goes to
// gawk_bridge_ice_registrations_total. A read secret is refused before
// murmur registers anything (F4), so that refusal is uncounted and fails
// the session as ice_secret: a configuration error, not an outage.
func (m *iceMirror) perform(ctx context.Context, a cbAction) error {
	s := m.s
	m.mu.Lock()
	sid := m.sid
	m.mu.Unlock()
	if a.remove {
		err := m.cli.RemoveCallback(ctx, sid, m.proxy)
		m.mu.Lock()
		m.registered = false
		m.mu.Unlock()
		s.metrics.iceRegistered.Set(0)
		if err != nil && !murmurice.IsUserException(err, murmurice.ExInvalidCallback) {
			if murmurice.IsUserException(err, murmurice.ExInvalidSecret) {
				return err
			}
			var pe *murmurice.ProtocolError
			if errors.Is(err, murmurice.ErrConnectionLost) || errors.As(err, &pe) {
				return err
			}
		}
	}
	if a.add {
		err := m.cli.AddCallback(ctx, sid, m.proxy)
		switch {
		case err == nil:
			s.metrics.iceRegistrations.WithLabelValues("ok").Inc()
			m.mu.Lock()
			m.registered = true
			m.mu.Unlock()
			s.metrics.iceRegistered.Set(1)
		case murmurice.IsUserException(err, murmurice.ExInvalidSecret):
			s.metrics.iceRegistrations.WithLabelValues("secret").Inc()
			m.mu.Lock()
			m.fsm.unanswered = max(m.fsm.unanswered-1, 0)
			m.mu.Unlock()
			return err
		default:
			s.metrics.iceRegistrations.WithLabelValues("error").Inc()
			var pe *murmurice.ProtocolError
			if errors.Is(err, murmurice.ErrConnectionLost) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &pe) {
				return err
			}
			s.log.Warn("murmur refused the Ice callback registration", "err", err)
		}
	}
	return nil
}

// read re-reads users and channels into the mirror. Events arriving while
// the read is in flight are buffered and applied after it (D2). For a
// reconcile it reports drift: the read against the mirror as it stood when
// the read began, leaving out the users and channels a buffered event
// touched, whose state the event already accounts for.
func (m *iceMirror) read(ctx context.Context, reconcile bool) (bool, error) {
	m.mu.Lock()
	sid := m.sid
	m.reading = true
	m.buffered = nil
	beforeU, beforeC := maps.Clone(m.users), maps.Clone(m.chans)
	m.mu.Unlock()
	users, err := m.cli.Users(ctx, sid)
	var chans []murmurice.Channel
	if err == nil {
		chans, err = m.cli.Channels(ctx, sid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	buf := m.buffered
	m.reading, m.buffered = false, nil
	if err != nil {
		changed := false
		for _, ev := range buf {
			changed = applyEvent(m.users, m.chans, ev) || changed
		}
		if changed {
			m.notifyLocked()
		}
		return false, err
	}
	newU := make(map[uint32]murmurice.User, len(users))
	for _, u := range users {
		newU[u.Session] = u
	}
	newC := make(map[uint32]murmurice.Channel, len(chans))
	for _, c := range chans {
		newC[c.ID] = c
	}
	drift := false
	if reconcile {
		touchedU, touchedC := map[uint32]bool{}, map[uint32]bool{}
		for _, ev := range buf {
			if ev.Op == murmurice.OpUserTextMessage {
				continue
			}
			if ev.Op[0] == 'u' {
				touchedU[ev.User.Session] = true
			} else {
				touchedC[ev.Channel.ID] = true
			}
		}
		drift = differs(beforeU, newU, touchedU) || differs(beforeC, newC, touchedC)
	}
	m.users, m.chans = newU, newC
	for _, ev := range buf {
		applyEvent(m.users, m.chans, ev)
	}
	if !maps.Equal(beforeU, m.users) || !maps.Equal(beforeC, m.chans) {
		m.notifyLocked()
	}
	return drift, nil
}

// differs compares two mirrors, leaving out the keys in skip.
func differs[V comparable](a, b map[uint32]V, skip map[uint32]bool) bool {
	for k, v := range a {
		if skip[k] {
			continue
		}
		if w, ok := b[k]; !ok || w != v {
			return true
		}
	}
	for k := range b {
		if skip[k] {
			continue
		}
		if _, ok := a[k]; !ok {
			return true
		}
	}
	return false
}

// stop ends the loop and waits for it.
func (m *iceMirror) stop() {
	if m.cancel != nil {
		m.cancel()
		<-m.done
	}
}

// leave is D17: removeCallback before the listener closes, in every state
// but unsafe, whatever murmur still holds. A refusal or a failure counts as
// done: murmur drops a dead registration at its next event anyway.
func (m *iceMirror) leave() {
	m.stop()
	m.mu.Lock()
	if m.left {
		m.mu.Unlock()
		return
	}
	m.left = true
	state, sid, have := m.fsm.state, m.sid, m.haveVer
	m.mu.Unlock()
	if state == cbUnsafe || !have || sid < 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.o.Timeout)
	defer cancel()
	err := m.cli.RemoveCallback(ctx, sid, m.proxy)
	m.mu.Lock()
	m.registered = false
	m.mu.Unlock()
	m.s.metrics.iceRegistered.Set(0)
	if err != nil && !murmurice.IsUserException(err, murmurice.ExInvalidCallback) {
		m.s.log.Info("removing the Ice callback on exit failed; murmur drops it at its next event", "err", err)
		return
	}
	m.s.log.Info("removed the Ice callback")
}

// close shuts the endpoint and the client (after leave).
func (m *iceMirror) close() {
	if m.ep != nil {
		_ = m.ep.Close()
	}
	_ = m.cli.Close()
}

// tempChannelsUnder counts the temporary channels under parent (D6). The
// mirror keeps its last state through an outage, which is good enough for
// an abuse guard.
func (m *iceMirror) tempChannelsUnder(parent uint32) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, ch := range m.chans {
		if id != 0 && ch.Parent == parent && ch.Temporary {
			n++
		}
	}
	return n
}

// rosterView is what one watcher reports.
type rosterView struct {
	available bool
	reason    string
	users     []voice.User
}

func (v rosterView) equal(o rosterView) bool {
	return v.available == o.available && v.reason == o.reason && slices.Equal(v.users, o.users)
}

// roster resolves a room's channel in the mirror and lists its users (D4).
// A dynamic room whose channel does not exist yet is available and empty
// (D5): its first voice user creates it.
func (m *iceMirror) roster(room voice.Message, tempName string) rosterView {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.available {
		return rosterView{reason: m.reason}
	}
	var channel uint32
	if room.Temporary {
		parent, ok := m.findPathLocked(voice.ChannelPath(m.s.opts.Mumble.TempParent))
		if !ok {
			return rosterView{reason: ReasonChannelNotFound}
		}
		if channel, ok = m.findChildLocked(parent, tempName); !ok {
			return rosterView{available: true}
		}
	} else {
		var ok bool
		if channel, ok = m.findPathLocked(voice.ChannelPath(room.Channel)); !ok {
			return rosterView{reason: ReasonChannelNotFound}
		}
	}
	var in []murmurice.User
	for _, u := range m.users {
		if u.Channel == channel {
			in = append(in, u)
		}
	}
	sort.Slice(in, func(i, j int) bool { return in[i].Session < in[j].Session })
	v := rosterView{available: true}
	for _, u := range in {
		v.users = append(v.users, voice.User{Session: u.Session, Name: wire.TruncateVoiceName(u.Name), Flags: iceRoomFlags(u)})
		if len(v.users) == wire.MaxVoiceRosterEntries {
			break
		}
	}
	return v
}

// findChildLocked and findPathLocked are mumble.Client's FindChild and
// FindPath rules, on the mirror: the lowest ID wins a name clash.
func (m *iceMirror) findChildLocked(parent uint32, name string) (uint32, bool) {
	best, found := uint32(0), false
	for id, ch := range m.chans {
		if id != 0 && ch.Parent == parent && ch.Name == name && (!found || id < best) {
			best, found = id, true
		}
	}
	return best, found
}

func (m *iceMirror) findPathLocked(path []string) (uint32, bool) {
	id := uint32(0)
	if _, ok := m.chans[0]; !ok {
		return 0, false
	}
	for _, name := range path {
		next, ok := m.findChildLocked(id, name)
		if !ok {
			return 0, false
		}
		id = next
	}
	return id, true
}

func iceRoomFlags(u murmurice.User) uint8 {
	var f uint8
	if u.SelfMute {
		f |= wire.RoomVoiceUserSelfMute
	}
	if u.SelfDeaf {
		f |= wire.RoomVoiceUserSelfDeaf
	}
	if u.Mute {
		f |= wire.RoomVoiceUserMute
	}
	if u.Deaf {
		f |= wire.RoomVoiceUserDeaf
	}
	if u.Suppress {
		f |= wire.RoomVoiceUserSuppressed
	}
	return f
}

// IceStatus is /statusz's ice object (D12).
type IceStatus struct {
	Available          bool   `json:"available"`
	Reason             string `json:"reason,omitempty"`
	ServerID           *int   `json:"serverId,omitempty"`
	MurmurVersion      string `json:"murmurVersion,omitempty"`
	Callbacks          string `json:"callbacks"`
	Unanswered         int    `json:"unanswered"`
	NextProbeInMs      *int64 `json:"nextProbeInMs,omitempty"`
	LastEventAgeMs     *int64 `json:"lastEventAgeMs,omitempty"`
	LastReconcileAgeMs *int64 `json:"lastReconcileAgeMs,omitempty"`
}

func (m *iceMirror) status() *IceStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.s.now()
	st := &IceStatus{Available: m.available, Callbacks: m.fsm.state, Unanswered: m.fsm.unanswered}
	if !m.available {
		st.Reason = m.reason
	}
	if m.sid >= 0 {
		sid := m.sid
		st.ServerID = &sid
	}
	if m.haveVer {
		st.MurmurVersion = m.version.String()
	}
	ms := func(t time.Time) *int64 { v := now.Sub(t).Milliseconds(); return &v }
	if at, due := m.fsm.probeDue(); due {
		v := max(at.Sub(now).Milliseconds(), 0)
		st.NextProbeInMs = &v
	}
	if !m.lastEvent.IsZero() {
		st.LastEventAgeMs = ms(m.lastEvent)
	}
	if !m.lastReconcile.IsZero() {
		st.LastReconcileAgeMs = ms(m.lastReconcile)
	}
	return st
}

// callbackAddr is the endpoint's bound address (tests).
func (m *iceMirror) callbackAddr() *net.TCPAddr { return m.ep.Addr() }
