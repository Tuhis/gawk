package bridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble"
	"github.com/Tuhis/gawk/gawk-server/voice"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// maxNameAttempts is D9's "up to 10 attempts" per name family, for a
// bridged user: one family per nickname.
const maxNameAttempts = 10

// maxObserverNameAttempts is the observers' ladder. Every voice-live room
// has its own observer and all of them share -mumble-observer-name, so the
// family is as wide as the voice rooms a server holds, not ten (PR #595
// review). Names this pod already sees are skipped without a dial, so the
// long ladder costs a dial only where murmur itself refuses a name.
const maxObserverNameAttempts = 1000

// mumbleFailure is a Mumble-side failure with its D18 reason.
type mumbleFailure struct {
	reason string
	err    error
}

func (f *mumbleFailure) Error() string { return f.reason + ": " + f.err.Error() }
func (f *mumbleFailure) Unwrap() error { return f.err }

func fail(reason string, err error) error { return &mumbleFailure{reason: reason, err: err} }

// failureReason maps an error to its D18 reason.
func failureReason(err error) string {
	var mf *mumbleFailure
	if errors.As(err, &mf) {
		return mf.reason
	}
	return classify(err)
}

// classify maps a mumble package error to a reason.
func classify(err error) string {
	var rej *mumble.RejectError
	var dial *mumble.DialError
	var den *mumble.DeniedError
	var rem *mumble.RemovedError
	switch {
	case errors.As(err, &rej):
		switch rej.Type {
		case mumble.RejectServerFull:
			return ReasonServerFull
		case mumble.RejectWrongServerPW:
			return ReasonWrongPassword
		case mumble.RejectInvalidUsername:
			return ReasonInvalidUsername
		case mumble.RejectUsernameInUse, mumble.RejectWrongUserPW:
			return ReasonNameUnavailable
		case mumble.RejectNoNewConnections:
			return ReasonAutoban
		}
		return ReasonRejected
	case errors.Is(err, mumble.ErrRefused):
		return ReasonAutoban
	case errors.As(err, &dial):
		return ReasonUnreachable
	case errors.Is(err, mumble.ErrChannelNotFound):
		return ReasonChannelNotFound
	case errors.As(err, &den):
		return ReasonPermissionDenied
	case errors.As(err, &rem) && rem.Ghost():
		return ReasonReplaced
	case errors.Is(err, context.DeadlineExceeded):
		return ReasonTimeout
	}
	return ReasonConnectionLost
}

// sanitizeNick makes a gawk nickname safe to put in a Mumble name: no
// control characters, no leading/trailing space, and short enough that the
// full name with a " 10" suffix fits wire.MaxVoiceNameLen, so the name the
// browser and the roster show is the name Mumble shows.
func sanitizeNick(nick string, template string) string {
	nick = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, nick)
	nick = strings.TrimSpace(nick)
	if nick == "" {
		nick = "guest"
	}
	budget := wire.MaxVoiceNameLen - (len(template) - len("{nick}")) - len(" 10")
	if budget < 1 {
		budget = 1
	}
	for len(nick) > budget {
		_, size := utf8.DecodeLastRuneInString(nick)
		nick = nick[:len(nick)-size]
	}
	return nick
}

// MumbleName expands -mumble-name-template for a nickname (D9).
func MumbleName(template, nick string) string {
	return strings.ReplaceAll(template, "{nick}", sanitizeNick(nick, template))
}

// SanitizeUsername replaces every character outside murmur's default
// username rule, [-=\w\[\]\{\}\(\)\@\|\.]+, with '_' (docs/80 §11.5). \w is
// taken as ASCII: what passes here passes whether the operator's murmur reads
// \w as ASCII or as Unicode.
func SanitizeUsername(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_',
			strings.ContainsRune("-=[]{}()@|.", r):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// nameCandidate is D9's sequence for one family: base, "base 2" … up to
// the caller's attempt budget (sep " " for the pretty form, "_" for the
// sanitized one).
func nameCandidate(base, sep string, i int) string {
	if i == 0 {
		return base
	}
	return base + sep + strconv.Itoa(i+1)
}

// mumbleConfig is the connection config every bridged connection shares
// (D10: server password plus the access token; unregistered).
func (s *Server) mumbleConfig(name string) mumble.Config {
	var tokens []string
	if s.opts.Mumble.AccessToken != "" {
		tokens = []string{s.opts.Mumble.AccessToken}
	}
	return mumble.Config{
		Addr:         s.opts.Mumble.Addr,
		Username:     name,
		Password:     s.opts.Mumble.Password,
		Tokens:       tokens,
		TLS:          s.opts.Mumble.TLS,
		Version:      s.opts.Mumble.ClientVersion,
		Release:      "gawk-mumble-bridge " + s.opts.Version,
		DialTimeout:  s.timings.MumbleDial,
		PingInterval: s.timings.MumblePing,
	}
}

// connectMumble logs in under base's D9 sequence. On UsernameInUse (or a
// registered name) it tries the next suffix; on InvalidUsername it switches
// once to the sanitized family. Names this pod's own connections hold, or
// any connection of this pod sees on the server, are skipped without a dial:
// murmur does not refuse a second login with the same name from the same
// address, it disconnects the first one as a ghost.
func (s *Server) connectMumble(ctx context.Context, base string, attempts int, mute, deaf bool, audioQueue int) (*mumble.Client, string, func(), error) {
	families := []struct{ base, sep string }{{base, " "}, {SanitizeUsername(base), "_"}}
	var lastErr error
	for fi, fam := range families {
		if fi == 1 && fam.base == base {
			break // the name was already valid: the rule is not the problem
		}
		invalid := false
		for i := 0; i < attempts && !invalid; i++ {
			name := nameCandidate(fam.base, fam.sep, i)
			release, ok := s.names.claim(name)
			if !ok {
				continue
			}
			cfg := s.mumbleConfig(name)
			cfg.SelfMute, cfg.SelfDeaf, cfg.AudioQueue = mute, deaf, audioQueue
			c, err := mumble.Dial(ctx, cfg)
			if err == nil {
				s.names.attach(c)
				return c, name, release, nil
			}
			release()
			lastErr = err
			var rej *mumble.RejectError
			if !errors.As(err, &rej) {
				return nil, "", nil, fail(classify(err), err)
			}
			switch rej.Type {
			case mumble.RejectUsernameInUse, mumble.RejectWrongUserPW:
				// taken or registered: the next suffix
			case mumble.RejectInvalidUsername:
				if fi > 0 {
					return nil, "", nil, fail(ReasonInvalidUsername, err)
				}
				invalid = true
			default:
				return nil, "", nil, fail(classify(err), err)
			}
		}
		if !invalid {
			if lastErr == nil {
				lastErr = errors.New("every candidate name is in use")
			}
			return nil, "", nil, fail(ReasonNameUnavailable, lastErr)
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no valid name")
	}
	return nil, "", nil, fail(ReasonInvalidUsername, lastErr)
}

// enterStatic joins a static room's mapped channel.
func enterStatic(ctx context.Context, c *mumble.Client, channel string) (uint32, error) {
	id, ok := c.FindPath(voice.ChannelPath(channel))
	if !ok {
		return 0, fail(ReasonChannelNotFound, mumble.ErrChannelNotFound)
	}
	if c.Self().Channel == id {
		return id, nil
	}
	if err := c.JoinChannel(ctx, id); err != nil {
		return 0, fail(classify(err), err)
	}
	return id, nil
}

// tempChannel names a dynamic room's channel (D11).
func (s *Server) tempChannel(code, displayCode string) string {
	return voice.TempChannelName(s.opts.Mumble.TempChannelTemplate, s.opts.TicketSecret, code, displayCode)
}

// enterTemp joins the room's temporary channel under the parent, waiting up
// to wait for it to appear (the observer creates it) and creating it itself
// when it does not (D11: a redialled observer, or a user's pod that got
// there first, finds the existing channel by name).
func enterTemp(ctx context.Context, c *mumble.Client, parentPath, name string, wait time.Duration) (uint32, error) {
	parent, ok := c.FindPath(voice.ChannelPath(parentPath))
	if !ok {
		return 0, fail(ReasonChannelNotFound, fmt.Errorf("%w: temporary-channel parent", mumble.ErrChannelNotFound))
	}
	if wait > 0 {
		wctx, cancel := context.WithTimeout(ctx, wait)
		_ = waitState(wctx, c, func() bool { _, ok := c.FindChild(parent, name); return ok })
		cancel()
	}
	for attempt := 0; attempt < 3; attempt++ {
		if id, ok := c.FindChild(parent, name); ok {
			if c.Self().Channel == id {
				return id, nil
			}
			err := c.JoinChannel(ctx, id)
			if err == nil {
				return id, nil
			}
			if _, still := c.FindChild(parent, name); still || !isDenied(err) {
				return 0, fail(classify(err), err)
			}
			continue // it went away under us: create it
		}
		id, err := c.CreateChannel(ctx, parent, name, true)
		if err == nil {
			return id, nil
		}
		var den *mumble.DeniedError
		if errors.As(err, &den) && den.Type == mumble.DenyChannelName {
			continue // someone created it first: join theirs
		}
		return 0, fail(classify(err), err)
	}
	return 0, fail(ReasonPermissionDenied, errors.New("the temporary channel kept changing"))
}

func isDenied(err error) bool {
	var den *mumble.DeniedError
	return errors.As(err, &den)
}

// waitState waits until cond holds on c's state, ctx ends or c closes.
func waitState(ctx context.Context, c *mumble.Client, cond func() bool) error {
	for {
		ch := c.Changed()
		if cond() {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		case <-c.Done():
			return c.Err()
		}
	}
}

// roomFlags is a Mumble user's wire.RoomVoiceUser* bitmap.
func roomFlags(u mumble.User) uint8 {
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

// stateFlags is the browser's wire.VoiceStateFlag* bitmap.
func stateFlags(u mumble.User, connected bool) uint8 {
	var f uint8
	if u.Mute {
		f |= wire.VoiceStateFlagServerMute
	}
	if u.Deaf {
		f |= wire.VoiceStateFlagServerDeaf
	}
	if u.Suppress {
		f |= wire.VoiceStateFlagSuppressed
	}
	if u.SelfMute {
		f |= wire.VoiceStateFlagSelfMute
	}
	if u.SelfDeaf {
		f |= wire.VoiceStateFlagSelfDeaf
	}
	if connected {
		f |= wire.VoiceStateFlagConnected
	}
	return f
}
