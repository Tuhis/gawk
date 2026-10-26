package bridge

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble"
	"github.com/Tuhis/gawk/gawk-server/voice"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// observer is one room's observer (D4): the Mumble connection named
// -mumble-observer-name that sits in the room's channel — creating a dynamic
// room's temporary channel and staying in it, so murmur keeps it while the
// room is voice-live (D11) — and reports the channel's users to the room's
// home over the observer link, which the home dialled.
type observer struct {
	s    *Server
	sess *webtransport.Session
	l    *link
	room voice.Message // the Observe
	key  string
	ctx  context.Context
	stop context.CancelFunc
	done chan struct{}

	mu        sync.Mutex
	available bool
	reason    string
	users     int
	holdsTemp bool
}

func (s *Server) handleObserve(w http.ResponseWriter, r *http.Request) {
	if !s.pskOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.draining.Load() {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}
	sess, err := s.internalWT.Upgrade(w, r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	actx, cancel := context.WithTimeout(sess.Context(), s.timings.HelloTimeout)
	str, err := sess.AcceptStream(actx)
	cancel()
	if err != nil {
		_ = sess.CloseWithError(0, "no observe stream")
		return
	}
	l := &link{sess: sess, str: str}
	_ = str.SetReadDeadline(time.Now().Add(s.timings.HelloTimeout))
	m, err := l.read()
	_ = str.SetReadDeadline(time.Time{})
	if err != nil || m.Type != voice.TypeObserve || m.Code == "" || len(m.Code) > wire.MaxRoomCodeLen {
		_ = sess.CloseWithError(0, "bad observe")
		return
	}
	ctx, stop := context.WithCancel(sess.Context())
	ob := &observer{s: s, sess: sess, l: l, room: m, key: s.roomKeyOf(m.Code), ctx: ctx, stop: stop, done: make(chan struct{})}
	if !s.trackObserver(ob) {
		stop()
		_ = sess.CloseWithError(webtransport.SessionErrorCode(codeDraining), "bridge draining")
		return
	}
	defer s.untrackObserver(ob)
	s.log.Info("observing room", "room", ob.key, "temporary", m.Temporary, "epoch", m.Epoch)
	ob.run()
}

func (s *Server) trackObserver(ob *observer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining.Load() {
		return false
	}
	s.observers[ob] = struct{}{}
	s.metrics.observers.Set(float64(len(s.observers)))
	return true
}

func (s *Server) untrackObserver(ob *observer) {
	s.mu.Lock()
	delete(s.observers, ob)
	s.metrics.observers.Set(float64(len(s.observers)))
	ob.mu.Lock()
	if ob.holdsTemp {
		ob.holdsTemp = false
		s.tempChannels--
	}
	ob.mu.Unlock()
	s.metrics.tempChannels.Set(float64(s.tempChannels))
	s.mu.Unlock()
}

// acquireTemp takes one of -max-temp-channels for this observer's life.
func (ob *observer) acquireTemp() bool {
	s := ob.s
	s.mu.Lock()
	defer s.mu.Unlock()
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if ob.holdsTemp {
		return true
	}
	if s.tempChannels >= s.opts.MaxTempChannels {
		return false
	}
	s.tempChannels++
	ob.holdsTemp = true
	s.metrics.tempChannels.Set(float64(s.tempChannels))
	return true
}

func (ob *observer) status() ObserverStatus {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	return ObserverStatus{Temporary: ob.room.Temporary, Available: ob.available, Reason: ob.reason, Users: ob.users}
}

// close ends the observer (the drain), closing the link with code so the
// home redials, and waits for its Mumble connection to be gone.
func (ob *observer) close(code uint32, reason string) {
	ob.stop()
	_ = ob.sess.CloseWithError(webtransport.SessionErrorCode(code), reason)
	<-ob.done
}

func (ob *observer) run() {
	defer close(ob.done)
	defer ob.stop()
	defer func() { _ = ob.sess.CloseWithError(0, "observer ended") }()
	// The link's other direction: the home sends nothing after Observe, so
	// a read ending is the link ending.
	go func() {
		for {
			if _, err := ob.l.read(); err != nil {
				ob.stop()
				return
			}
		}
	}()
	s := ob.s
	backoff := s.timings.ObserverBackoff
	for ob.ctx.Err() == nil {
		start := time.Now()
		err := ob.observe()
		if ob.ctx.Err() != nil {
			return
		}
		reason := failureReason(err)
		s.recordFailure(reason, ob.key)
		s.log.Warn("observer unavailable", "room", ob.key, "reason", reason, "err", err)
		ob.report(voice.Message{Type: voice.TypeRoster, Available: false, Reason: failureText[reason]}, false, reason, 0)
		if time.Since(start) > s.timings.ObserverBackoffMax {
			backoff = s.timings.ObserverBackoff // it was up for a while: start over
		}
		select {
		case <-time.After(backoff):
		case <-ob.ctx.Done():
			return
		}
		backoff = min(2*backoff, s.timings.ObserverBackoffMax)
	}
}

// report sends one Roster and records it for /statusz.
func (ob *observer) report(m voice.Message, available bool, reason string, users int) error {
	ob.mu.Lock()
	ob.available, ob.reason, ob.users = available, reason, users
	ob.mu.Unlock()
	return ob.l.send(m)
}

// enter puts the observer into the room's channel.
func (ob *observer) enter(c *mumble.Client) (uint32, error) {
	s := ob.s
	ctx, cancel := context.WithTimeout(ob.ctx, s.timings.ChannelEnter)
	defer cancel()
	if ob.room.Temporary {
		return enterTemp(ctx, c, s.opts.Mumble.TempParent, s.tempChannel(ob.room.Code, ob.room.DisplayCode), 0)
	}
	return enterStatic(ctx, c, ob.room.Channel)
}

// observe runs one observer connection until it fails or the link ends.
func (ob *observer) observe() error {
	s := ob.s
	if ob.room.Temporary && !ob.acquireTemp() {
		return fail(ReasonTempChannelCap, errors.New("-max-temp-channels reached"))
	}
	c, _, release, err := s.connectMumble(ob.ctx, s.opts.Mumble.ObserverName, true, true, 0)
	if err != nil {
		return err
	}
	defer release()
	defer c.Close()
	channel, err := ob.enter(c)
	if err != nil {
		return err
	}
	var last []voice.User
	sent := false
	for {
		ch := c.Changed()
		self := c.Self()
		if self.Channel != channel {
			// Moved out by an admin, or the channel went away: go back.
			if channel, err = ob.enter(c); err != nil {
				return err
			}
			continue
		}
		users := rosterUsers(c, channel, self.Session)
		if !sent || !slices.Equal(users, last) {
			m := voice.Message{Type: voice.TypeRoster, Available: true, Users: users}
			if err := ob.report(m, true, "", len(users)); err != nil {
				ob.stop()
				return nil
			}
			last, sent = users, true
		}
		select {
		case <-ch:
		case <-c.Done():
			return c.Err()
		case <-ob.ctx.Done():
			return nil
		}
		// Coalesce a burst (a join is a UserState per field, a channel
		// move several messages) into one Roster.
		select {
		case <-time.After(s.timings.RosterCoalesce):
		case <-ob.ctx.Done():
			return nil
		}
	}
}

// rosterUsers is every user in channel except the observer itself.
func rosterUsers(c *mumble.Client, channel, self uint32) []voice.User {
	var out []voice.User
	for _, u := range c.UsersIn(channel) {
		if u.Session == self {
			continue
		}
		out = append(out, voice.User{Session: u.Session, Name: wire.TruncateVoiceName(u.Name), Flags: roomFlags(u)})
		if len(out) == wire.MaxVoiceRosterEntries {
			break
		}
	}
	return out
}
