package roomsrv

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/voice"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Room voice through the Mumble bridge (R79, docs/80 MV3): the home pod's
// half of D2's "the relay is the authority".
//
// A room is VOICE-LIVE (D12) while voice is configured on this relay and the
// room has a valid Mumble mapping: a static room's CR / rooms-file entry, or
// a dynamic room minted with ?voice=1 (C3). Voice-live is not live: it emits
// no bus event, counts toward no room cap, and a static room homed only for
// voice (the cluster keeper's adoption) stays out of AdminRooms until someone
// joins it.
//
// Per voice-live room the registry keeps what the observer link last
// reported (the channel's users, and whether the observer is in it) and the
// voice sessions the session links registered, one per participant. From
// the two it builds the voice roster (D15, §11.1 C2), an out-of-sequence
// VoiceRosterUpdated that only participants whose RoomHello asked for
// RoomCapVoice receive.
//
// The network halves — dialing the observer link, serving the session link
// — are internal/transport's (voice_observer.go, voice_session.go); this
// file only ever sees a VoiceLink it can close. As everywhere in the
// registry, nothing here does I/O under r.mu: every Close a decision causes
// is collected under the lock and run after it (voiceClose).
//
// With voice unconfigured (Options.Voice.Enabled false) no room is ever
// voice-live, so none of this runs: no capability, no voice event, and
// VoiceJoin is the RoomRejectUnsupported every relay before R79 answered.

// Voice defaults (docs/80 §11.4).
const (
	DefaultVoiceMaxRooms        = 10
	DefaultVoiceMaxUsersPerRoom = 25
)

// VoiceOptions configures room voice (docs/80 §11.4). Every field crosses
// from the config through roomOptions (TestRoomOptionsCarryAllKnobs).
type VoiceOptions struct {
	// Enabled is true only when the bridge is fully configured
	// (config.Config.VoiceConfigured); false is the off-path.
	Enabled bool
	// PublicURL is the bridge's browser-facing https URL, sent in every
	// VoiceTicket (-voice-bridge-public-url).
	PublicURL string
	// TicketSecret is the HMAC key tickets are minted under
	// (-voice-ticket-secret); the bridge holds the same.
	TicketSecret []byte
	// MaxRooms caps voice-live DYNAMIC rooms (-voice-max-rooms): per pod,
	// fleet-wide in cluster mode through Options.VoiceRoomsInUse.
	MaxRooms int
	// MaxUsersPerRoom caps voice sessions per room
	// (-voice-max-users-per-room).
	MaxUsersPerRoom int
}

// Sentinel errors of a session-link registration (RegisterVoice). Each maps
// to exactly one close code at the transport boundary (voice_session.go):
// the retryable ones (an expired ticket, a home move, a full room) to 4016,
// a room session that is gone to 4002, everything else to the terminal 4015.
var (
	ErrVoiceBadTicket     = errors.New("roomsrv: voice ticket invalid")
	ErrVoiceTicketExpired = errors.New("roomsrv: voice ticket expired")
	ErrVoiceWrongRoom     = errors.New("roomsrv: voice ticket is for another room")
	ErrVoiceEpoch         = errors.New("roomsrv: voice ticket from another home epoch")
	ErrVoiceReplay        = errors.New("roomsrv: voice ticket already used")
	ErrVoiceNoParticipant = errors.New("roomsrv: voice ticket's participant is no longer in the room")
	ErrVoiceNotLive       = errors.New("roomsrv: room has no voice")
	ErrVoiceFull          = errors.New("roomsrv: room voice is full")
)

// VoiceLink is the session link to the bridge pod hosting one voice session
// (docs/80 D2). Close tells that pod to end the browser's voice session with
// code (the link's Close message), then ends the link. It must not block for
// long: the registry calls it outside its lock but on the caller's path.
type VoiceLink interface {
	Close(code uint32, reason string)
}

// Close reasons. They travel to the bridge and on to the browser's in-band
// notice, so they name what happened, never a code or an ID.
const (
	voiceReasonReplaced   = "replaced by a newer voice session"
	voiceReasonRoomEnded  = "room ended"
	voiceReasonRoomMoved  = "room moved to another server"
	voiceReasonLeft       = "room session ended"
	voiceReasonVoiceOff   = "voice turned off for the room"
	voiceReasonRemapped   = "voice channel changed"
	voiceReasonBanned     = "banned"
	voiceReasonTerminated = "removed by an operator"
)

// voiceRoom is a voice-live room's voice state.
type voiceRoom struct {
	// id tells this voice state apart from an earlier one of the same code
	// (a room that ended and came back, a remapped channel): an observer
	// link reports against the id it was dialled for, and a stale one's
	// reports are dropped.
	id        uint64
	channel   string
	temporary bool
	// linkUp is true while an observer link is connected; available and
	// users are what it last reported.
	linkUp    bool
	available bool
	reason    string
	problems  []string
	users     []voice.User
	sessions  map[uint16]*VoiceSession
	// lastRoster is the last pushed roster (encoded at seq 0), so a report
	// that changes nothing visible pushes nothing.
	lastRoster []byte
}

// VoiceSession is one participant's registered voice session (docs/80 D5),
// alive while its session link is.
type VoiceSession struct {
	reg    *Registry
	room   *room
	vr     *voiceRoom
	pid    uint16
	link   VoiceLink
	remote netip.Addr
	// mumble is the bridged user's Mumble session once Joined; 0 before.
	mumble uint32
	name   string
	closed bool
}

// VoiceAccepted is what the home answers a registration with (the session
// link's Accepted message): the channel, so the hosting bridge pod needs
// nothing from the observer's pod.
type VoiceAccepted struct {
	Code        string
	DisplayCode string
	Channel     string
	Temporary   bool
}

// VoiceRoom is one voice-live room as the observer link needs it.
type VoiceRoom struct {
	Code        string
	DisplayCode string
	Channel     string
	Temporary   bool
	// ID is the voice state's identity; pass it back with every report.
	ID uint64
}

// voiceClose is a close decided under r.mu and run after it.
type voiceClose struct {
	link   VoiceLink
	code   uint32
	reason string
}

func runVoiceCloses(cs []voiceClose) {
	for _, c := range cs {
		c.link.Close(c.code, c.reason)
	}
}

// randomEpoch is a single pod's epoch (D7): random per process, so a
// restarted relay refuses every ticket its previous run minted.
func randomEpoch() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any supported platform; a fixed
		// fallback still separates nothing worse than a restart would.
		return uint64(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint64(b[:])
}

// epochFor is the room's current epoch: the lease generation in cluster
// mode (Options.VoiceEpoch), this process's random value otherwise. Called
// WITHOUT r.mu: the cluster seam takes the store's lock.
func (r *Registry) epochFor(code string) (uint64, bool) {
	if r.opts.VoiceEpoch != nil {
		return r.opts.VoiceEpoch(code)
	}
	return r.epoch, true
}

// VoiceEpoch exposes epochFor to the observer link, which carries it on
// Observe for the bridge's logs.
func (r *Registry) VoiceEpoch(code string) (uint64, bool) { return r.epochFor(code) }

// validMumble applies rooms.ValidateMumble at the registry boundary: an
// invalid mapping (a CR applied by hand, a typo in the rooms file) is a room
// without voice and a warning, never a refused room.
func (r *Registry) validMumble(code string, m *rooms.MumbleIntegration) *rooms.MumbleIntegration {
	if m == nil {
		return nil
	}
	if err := rooms.ValidateMumble(m); err != nil {
		r.log.Warn("room voice mapping ignored", "room_key", r.opts.Obfuscate(code), "err", err)
		return nil
	}
	cp := *m
	return &cp
}

func sameMumble(a, b *rooms.MumbleIntegration) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// newVoiceRoomLocked builds a fresh voice state for mapping m.
func (r *Registry) newVoiceRoomLocked(m *rooms.MumbleIntegration) *voiceRoom {
	r.voiceSeq++
	return &voiceRoom{id: r.voiceSeq, channel: m.Channel, temporary: m.Temporary, sessions: make(map[uint16]*VoiceSession)}
}

// applyMumbleLocked makes rm's voice state match mapping m (already
// validated). It reports whether voice-live-ness or the mapping changed, and
// the closes the change causes: voice off ends every session terminally
// (4015), a remapped channel retryably (4016 — rejoin lands in the new
// channel). A participant whose room gained or lost voice gets a fresh
// RoomState: the caps byte is the only place a client learns either.
func (r *Registry) applyMumbleLocked(rm *room, m *rooms.MumbleIntegration) (bool, []voiceClose) {
	want := r.opts.Voice.Enabled && m != nil
	prev := rm.mumble
	rm.mumble = m
	var closes []voiceClose
	switch {
	case rm.voice == nil && !want:
		return false, nil
	case rm.voice == nil && want:
		rm.voice = r.newVoiceRoomLocked(m)
		r.resendStatesLocked(rm)
		return true, nil
	case rm.voice != nil && !want:
		closes = r.closeVoiceSessionsLocked(rm, wire.CloseCodeVoiceRemoved, voiceReasonVoiceOff)
		rm.voice = nil
		r.resendStatesLocked(rm)
		return true, closes
	}
	if sameMumble(prev, m) {
		return false, nil
	}
	closes = r.closeVoiceSessionsLocked(rm, wire.CloseCodeVoiceUnavailable, voiceReasonRemapped)
	rm.voice = r.newVoiceRoomLocked(m)
	r.pushVoiceRosterLocked(rm)
	return true, closes
}

// resendStatesLocked sends every participant a fresh snapshot (and, to the
// ones that asked, the roster after it). Replace-never-merge makes a second
// RoomState safe; it carries the current seq.
func (r *Registry) resendStatesLocked(rm *room) {
	for _, p := range rm.participants {
		p.enqueueLocked(r.stateRecordLocked(rm, p))
		r.sendVoiceRosterToLocked(rm, p)
	}
}

// closeVoiceSessionsLocked ends every voice session of rm with code.
func (r *Registry) closeVoiceSessionsLocked(rm *room, code uint32, reason string) []voiceClose {
	if rm.voice == nil {
		return nil
	}
	var out []voiceClose
	for _, vs := range rm.voice.sessions {
		out = append(out, vs.endLocked(code, reason))
	}
	return out
}

// endLocked forgets the session and returns its close.
func (vs *VoiceSession) endLocked(code uint32, reason string) voiceClose {
	vs.closed = true
	if cur := vs.vr.sessions[vs.pid]; cur == vs {
		delete(vs.vr.sessions, vs.pid)
	}
	return voiceClose{link: vs.link, code: code, reason: reason}
}

// notifyVoiceChanged tells the observer side the set of voice-live rooms (or
// a mapping) changed. Called outside r.mu.
func (r *Registry) notifyVoiceChanged() {
	if r.opts.OnVoiceChanged != nil {
		r.opts.OnVoiceChanged()
	}
}

func (r *Registry) voiceDynamicCountLocked() int {
	n := 0
	for _, rm := range r.rooms {
		if rm.kind == rooms.KindDynamic && rm.voice != nil {
			n++
		}
	}
	return n
}

// --- the voice roster ---------------------------------------------------

// voiceUserFlagMask is the RoomVoiceUser* bits the wire carries; a newer
// bridge's extra bits are dropped rather than failing the encode.
const voiceUserFlagMask = wire.RoomVoiceUserSelfMute | wire.RoomVoiceUserSelfDeaf | wire.RoomVoiceUserMute |
	wire.RoomVoiceUserDeaf | wire.RoomVoiceUserSuppressed

// voiceRosterEventLocked builds the roster: the observer's users, each
// marked as a gawk participant when its Mumble session is a registered voice
// session's, else as a native Mumble user. Empty while the observer is not
// in the channel. The bridge leaves its own observer user out; nothing else
// is filtered here.
func (r *Registry) voiceRosterEventLocked(rm *room, seq uint32) wire.RoomEvent {
	ev := wire.RoomEvent{Seq: seq, Kind: wire.RoomEventVoiceRosterUpdated}
	vr := rm.voice
	if vr == nil || !vr.available {
		return ev
	}
	ev.VoiceFlags = wire.RoomVoiceRosterFlagAvailable
	gawk := make(map[uint32]uint16, len(vr.sessions))
	for pid, vs := range vr.sessions {
		if vs.mumble != 0 {
			gawk[vs.mumble] = pid
		}
	}
	for _, u := range vr.users {
		if len(ev.VoiceRoster) == wire.MaxVoiceRosterEntries {
			break
		}
		e := wire.VoiceRosterEntry{Session: u.Session, Kind: wire.RoomVoiceKindMumble, Flags: u.Flags & voiceUserFlagMask, Name: wire.TruncateVoiceName(u.Name)}
		if pid, ok := gawk[u.Session]; ok {
			e.Kind, e.ParticipantID = wire.RoomVoiceKindGawk, pid
		}
		ev.VoiceRoster = append(ev.VoiceRoster, e)
	}
	return ev
}

func (r *Registry) voiceRosterRecordLocked(rm *room, seq uint32) []byte {
	rec, err := wire.AppendRoomEvent(nil, r.voiceRosterEventLocked(rm, seq))
	if err != nil {
		r.log.Error("voice roster encode failed", "err", err)
		return nil
	}
	return frame(rec)
}

// sendVoiceRosterToLocked sends p the roster, out of sequence, when p asked
// for voice and the room is voice-live: right after every RoomState p gets.
func (r *Registry) sendVoiceRosterToLocked(rm *room, p *Participant) {
	if !p.wantVoice || rm.voice == nil {
		return
	}
	if rec := r.voiceRosterRecordLocked(rm, rm.seq); rec != nil {
		p.enqueueLocked(rec)
	}
}

// pushVoiceRosterLocked sends the roster to every participant that asked
// for voice, unless nothing visible changed since the last push.
func (r *Registry) pushVoiceRosterLocked(rm *room) {
	if rm.voice == nil {
		return
	}
	key := r.voiceRosterRecordLocked(rm, 0)
	if key == nil || bytes.Equal(key, rm.voice.lastRoster) {
		return
	}
	rm.voice.lastRoster = key
	rec := r.voiceRosterRecordLocked(rm, rm.seq)
	for _, p := range rm.participants {
		if p.wantVoice {
			p.enqueueLocked(rec)
		}
	}
}

// --- VoiceJoin (D5) -----------------------------------------------------

// voiceJoin answers a participant's VoiceJoin with a ticket, addressed to it
// alone and out of sequence (the current seq, not advanced), or with a
// CommandRejected: Unsupported without voice, Limit at the per-room cap,
// Unavailable while the observer is not in the channel or this pod cannot
// name its epoch (the lease is being lost).
func (p *Participant) voiceJoin() {
	r := p.reg
	rm := p.room
	const cmd = wire.RoomCommandVoiceJoin
	if !r.opts.Voice.Enabled {
		p.reject(cmd, wire.RoomRejectUnsupported, "unsupported command")
		return
	}
	epoch, haveEpoch := r.epochFor(rm.code)
	nonce, err := voice.NewNonce()
	if err != nil {
		p.reject(cmd, wire.RoomRejectUnavailable, "voice is unavailable right now")
		return
	}
	r.mu.Lock()
	if rm.ended {
		r.mu.Unlock()
		return
	}
	vr := rm.voice
	var (
		reason uint8
		msg    string
	)
	switch {
	case vr == nil:
		reason, msg = wire.RoomRejectUnsupported, "this room has no voice"
	case vr.sessions[p.id] == nil && len(vr.sessions) >= r.opts.Voice.MaxUsersPerRoom:
		reason, msg = wire.RoomRejectLimit, "voice is full"
	case !vr.available || !haveEpoch:
		reason, msg = wire.RoomRejectUnavailable, "voice is unavailable right now"
	}
	if reason != 0 {
		r.mu.Unlock()
		p.reject(cmd, reason, msg)
		return
	}
	ticket, err := voice.MintTicket(r.opts.Voice.TicketSecret, voice.Ticket{
		Code: rm.code, Participant: p.id, Nickname: p.nick, Nonce: nonce, Epoch: epoch,
		Expiry: r.opts.Now().Add(voice.TicketTTL),
	})
	var rec []byte
	if err == nil {
		rec, err = wire.AppendRoomEvent(nil, wire.RoomEvent{Seq: rm.seq, Kind: wire.RoomEventVoiceTicket, VoiceURL: r.opts.Voice.PublicURL, VoiceTicket: ticket})
	}
	if err != nil {
		r.mu.Unlock()
		r.log.Error("voice ticket mint failed", "room_key", r.opts.Obfuscate(rm.code), "err", err)
		p.reject(cmd, wire.RoomRejectUnavailable, "voice is unavailable right now")
		return
	}
	p.enqueueLocked(frame(rec))
	r.mu.Unlock()
}

// --- the session link (D5, §11.5) ---------------------------------------

// RegisterVoice is a session link's Register at the home: the ticket's MAC
// and expiry, the room code against the link's path, the epoch, the nonce
// (single use), the participant still in the room under that ID, the room
// still voice-live, the per-room cap — in that order, each its own sentinel.
// Then newest wins: an older voice session of the same participant is
// closed with 4015. The ban check on the voice client's address is the
// transport's (it holds the ban set) and runs first.
func (r *Registry) RegisterVoice(code string, raw []byte, remote netip.Addr, link VoiceLink) (*VoiceSession, VoiceAccepted, error) {
	if !r.opts.Voice.Enabled {
		return nil, VoiceAccepted{}, ErrVoiceNotLive
	}
	norm, err := rooms.NormalizeCode(code)
	if err != nil {
		return nil, VoiceAccepted{}, ErrVoiceWrongRoom
	}
	now := r.opts.Now()
	t, err := voice.VerifyTicket(r.opts.Voice.TicketSecret, raw, now)
	switch {
	case errors.Is(err, voice.ErrTicketExpired):
		return nil, VoiceAccepted{}, ErrVoiceTicketExpired
	case err != nil:
		return nil, VoiceAccepted{}, fmt.Errorf("%w: %v", ErrVoiceBadTicket, err)
	}
	if t.Code != norm {
		return nil, VoiceAccepted{}, ErrVoiceWrongRoom
	}
	if epoch, ok := r.epochFor(norm); !ok || epoch != t.Epoch {
		return nil, VoiceAccepted{}, ErrVoiceEpoch
	}
	r.mu.Lock()
	r.pruneNoncesLocked(now)
	if _, used := r.voiceNonces[t.Nonce]; used {
		r.mu.Unlock()
		return nil, VoiceAccepted{}, ErrVoiceReplay
	}
	rm, ok := r.rooms[norm]
	if !ok || rm.ended {
		r.mu.Unlock()
		return nil, VoiceAccepted{}, ErrVoiceNotLive
	}
	if rm.participants[t.Participant] == nil {
		r.mu.Unlock()
		return nil, VoiceAccepted{}, ErrVoiceNoParticipant
	}
	vr := rm.voice
	if vr == nil {
		r.mu.Unlock()
		return nil, VoiceAccepted{}, ErrVoiceNotLive
	}
	older := vr.sessions[t.Participant]
	if older == nil && len(vr.sessions) >= r.opts.Voice.MaxUsersPerRoom {
		r.mu.Unlock()
		return nil, VoiceAccepted{}, ErrVoiceFull
	}
	// Accepted from here: the nonce is spent until its ticket would have
	// expired anyway, which is all a replay could use it for.
	r.voiceNonces[t.Nonce] = t.Expiry
	var closes []voiceClose
	if older != nil {
		closes = append(closes, older.endLocked(wire.CloseCodeVoiceRemoved, voiceReasonReplaced))
	}
	vs := &VoiceSession{reg: r, room: rm, vr: vr, pid: t.Participant, link: link, remote: remote}
	vr.sessions[t.Participant] = vs
	acc := VoiceAccepted{Code: rm.code, DisplayCode: rm.display, Channel: vr.channel, Temporary: vr.temporary}
	r.pushVoiceRosterLocked(rm)
	r.mu.Unlock()
	runVoiceCloses(closes)
	r.log.Info("voice session registered", "room_key", r.opts.Obfuscate(norm), "participant", t.Participant, "replaced", older != nil)
	return vs, acc, nil
}

// pruneNoncesLocked forgets nonces whose tickets have expired.
func (r *Registry) pruneNoncesLocked(now time.Time) {
	for n, exp := range r.voiceNonces {
		if !now.Before(exp) {
			delete(r.voiceNonces, n)
		}
	}
}

// Participant is the voice session's participant ID.
func (vs *VoiceSession) Participant() uint16 { return vs.pid }

// Joined records the bridged user's Mumble session (the link's Joined): the
// observer's entry with that session is now this participant's.
func (vs *VoiceSession) Joined(session uint32, name string) {
	r := vs.reg
	r.mu.Lock()
	defer r.mu.Unlock()
	if vs.closed || vs.room.ended {
		return
	}
	vs.mumble, vs.name = session, name
	r.pushVoiceRosterLocked(vs.room)
}

// State applies the link's State to the roster: the observer's roster is
// the main source, and this keeps the bridged user's flags fresh between
// observer reports.
func (vs *VoiceSession) State(session uint32, flags uint8) {
	r := vs.reg
	r.mu.Lock()
	defer r.mu.Unlock()
	if vs.closed || vs.room.ended || vs.room.voice != vs.vr {
		return
	}
	if session == 0 {
		session = vs.mumble
	}
	for i := range vs.vr.users {
		if vs.vr.users[i].Session == session {
			vs.vr.users[i].Flags = flags
		}
	}
	r.pushVoiceRosterLocked(vs.room)
}

// Left forgets the session: the Mumble side ended it (the link's Left) or
// the link closed. Idempotent, and a no-op for a session already ended.
func (vs *VoiceSession) Left() {
	r := vs.reg
	r.mu.Lock()
	defer r.mu.Unlock()
	if vs.closed {
		return
	}
	vs.closed = true
	if vs.vr.sessions[vs.pid] == vs {
		delete(vs.vr.sessions, vs.pid)
	}
	if !vs.room.ended {
		r.pushVoiceRosterLocked(vs.room)
	}
}

// closeParticipantVoiceLocked ends p's voice session, if any (C4: its room
// control session ended).
func (r *Registry) closeParticipantVoiceLocked(rm *room, pid uint16, code uint32, reason string) []voiceClose {
	if rm.voice == nil {
		return nil
	}
	vs := rm.voice.sessions[pid]
	if vs == nil {
		return nil
	}
	c := vs.endLocked(code, reason)
	r.pushVoiceRosterLocked(rm)
	return []voiceClose{c}
}

// CloseVoiceIn ends, with 4015, every voice session on this pod whose voice
// client's address banned reports (an IP ban, docs/80 D17). Every pod gets
// the ban event; only the home holds sessions, so each closes once.
func (r *Registry) CloseVoiceIn(banned func(netip.Addr) bool) int {
	r.mu.Lock()
	var closes []voiceClose
	for _, rm := range r.rooms {
		if rm.voice == nil {
			continue
		}
		for _, vs := range rm.voice.sessions {
			if vs.remote.IsValid() && banned(vs.remote) {
				closes = append(closes, vs.endLocked(wire.CloseCodeVoiceRemoved, voiceReasonBanned))
			}
		}
		if len(closes) > 0 {
			r.pushVoiceRosterLocked(rm)
		}
	}
	r.mu.Unlock()
	runVoiceCloses(closes)
	return len(closes)
}

// CloseVoiceOfBroadcast ends, with 4015, the voice session of whoever
// attached broadcast id to a room this pod homes: a kill of that broadcast
// drops its broadcaster's voice too (docs/80 D17).
func (r *Registry) CloseVoiceOfBroadcast(id string) int {
	id, err := broadcastid.Normalize(id)
	if err != nil {
		return 0
	}
	r.mu.Lock()
	var closes []voiceClose
	for _, rm := range r.roomsOfLocked(id) {
		a := rm.find(id)
		if a == nil || a.ownerPID == 0 {
			continue
		}
		closes = append(closes, r.closeParticipantVoiceLocked(rm, a.ownerPID, wire.CloseCodeVoiceRemoved, voiceReasonTerminated)...)
	}
	r.mu.Unlock()
	runVoiceCloses(closes)
	return len(closes)
}

// --- the observer link (D4) ---------------------------------------------

// VoiceRooms lists the voice-live rooms this pod homes, sorted by code: the
// set the transport keeps one observer link open for.
func (r *Registry) VoiceRooms() []VoiceRoom {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]VoiceRoom, 0)
	for code, rm := range r.rooms {
		if rm.ended || rm.voice == nil {
			continue
		}
		out = append(out, VoiceRoom{Code: code, DisplayCode: rm.display, Channel: rm.voice.channel, Temporary: rm.voice.temporary, ID: rm.voice.id})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// voiceRoomLocked returns the voice state id names, or nil when it is gone.
func (r *Registry) voiceRoomLocked(code string, id uint64) *room {
	rm, ok := r.rooms[code]
	if !ok || rm.ended || rm.voice == nil || rm.voice.id != id {
		return nil
	}
	return rm
}

// ApplyVoiceRoster applies one observer Roster message to voice state id.
func (r *Registry) ApplyVoiceRoster(code string, id uint64, m voice.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rm := r.voiceRoomLocked(code, id)
	if rm == nil {
		return
	}
	vr := rm.voice
	vr.linkUp = true
	vr.available = m.Available
	vr.reason = m.Reason
	vr.problems = append([]string(nil), m.Problems...)
	if m.Available {
		vr.users = append([]voice.User(nil), m.Users...)
	} else {
		vr.users = nil
	}
	r.pushVoiceRosterLocked(rm)
}

// VoiceObserverDown marks voice state id unavailable: its observer link
// dropped (or never came up). The roster empties and says so.
func (r *Registry) VoiceObserverDown(code string, id uint64, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rm := r.voiceRoomLocked(code, id)
	if rm == nil {
		return
	}
	vr := rm.voice
	vr.linkUp, vr.available, vr.users, vr.reason = false, false, nil, reason
	r.pushVoiceRosterLocked(rm)
}

// voiceObserverState names the observer's state for /statusz.
func voiceObserverState(vr *voiceRoom) string {
	switch {
	case vr.available:
		return "available"
	case vr.linkUp:
		return "unavailable"
	default:
		return "down"
	}
}
