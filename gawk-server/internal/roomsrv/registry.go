// Package roomsrv is the relay's room registry (R42, docs/44 §4.4–4.7,
// RM2): the control-plane state a room is made of — attachments,
// participants, grants, the empty grace, the limits — and the fan-out of
// RoomState / RoomEvent records to every control session.
//
// It knows nothing about QUIC: a control session is a Conn that can write a
// framed record and close with a code, and internal/transport supplies the
// real one. It knows nothing about media either — an attachment is a
// broadcast ID the hub is asked about, never a subscriber. Both keep the
// registry unit-testable with fakes and keep the datagram path untouched
// (docs/44 D1).
//
// Cluster mode (RM3) layers on top through Store / home-pod hooks; with
// none installed the registry is the single-pod truth, which is exactly the
// -cluster-mode-off shape the hub has.
package roomsrv

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Defaults (docs/44 §4.10).
const (
	DefaultEmptyGrace      = 60 * time.Second
	DefaultMaxRooms        = 10
	DefaultMaxBroadcasts   = 4
	DefaultMaxParticipants = 50
	// DefaultRefreshInterval is how often attachment live/viewer state is
	// re-read from the hub and pushed as AttachmentUpdated deltas.
	DefaultRefreshInterval = time.Second
	// outboxDepth bounds the per-participant record queue. Control traffic
	// is tiny; a participant that cannot drain 256 records is not reading.
	outboxDepth = 256
)

// Sentinel errors. Each maps to exactly one HTTP status or reject reason at
// the transport boundary (CODE-REVIEW.md, error mapping).
var (
	// ErrNotFound: unknown room, or (on mint/attach) unknown broadcast.
	ErrNotFound = errors.New("roomsrv: not found")
	// ErrForbidden: wrong attach secret, wrong creator token, wrong
	// create secret, bad resume proof, or a grant the participant lacks.
	ErrForbidden = errors.New("roomsrv: forbidden")
	// ErrFull: -max-room-participants reached.
	ErrFull = errors.New("roomsrv: room full")
	// ErrMaxRooms: -max-rooms reached.
	ErrMaxRooms = errors.New("roomsrv: room limit reached")
	// ErrMaxBroadcasts: the room's attachment limit reached.
	ErrMaxBroadcasts = errors.New("roomsrv: attachment limit reached")
	// ErrAlreadyAttached: the broadcast is attached to another room (D1).
	ErrAlreadyAttached = errors.New("roomsrv: broadcast attached elsewhere")
	// ErrUnavailable: the backing store cannot be reached (docs/44 §6).
	ErrUnavailable = errors.New("roomsrv: store unavailable")
	// ErrDisabled: -rooms is off.
	ErrDisabled = errors.New("roomsrv: rooms disabled")
	// errCollision: no free dynamic code after the mint retry budget.
	errCollision = errors.New("roomsrv: collision limit exceeded minting code")
)

// Conn is one control session as the registry sees it. Write blocks until
// the record is on the stream (the registry serializes writes per
// participant on its own goroutine, so a slow peer stalls only itself);
// Close ends the session with a WebTransport close code.
type Conn interface {
	Write(ctx context.Context, record []byte) error
	Close(code uint32, reason string)
}

// BroadcastState is what the registry asks the hub about an attachment.
type BroadcastState struct {
	// Live is true while the publisher session is up; false means "away"
	// (within the broadcast grace).
	Live bool
	// Viewers is the human viewer count (R18 ViewerSubscribers).
	Viewers int
}

// BroadcastSource answers "is this broadcast known, live, watched" under one
// lock. The hub implements it; tests fake it.
type BroadcastSource interface {
	BroadcastState(id string) (BroadcastState, bool)
}

// Tokens mints and verifies the two stateless credentials rooms use
// (docs/44 D8, D9): the creator token (relay-minted per dynamic room) and
// the broadcast resume token (attach proof). internal/transport implements
// it over the R17 resume-token key.
type Tokens interface {
	MintCreator(code string) []byte
	VerifyCreator(code string, token []byte) bool
	VerifyResume(broadcastID string, token []byte) bool
}

// Options configures the registry. Every -room-* knob crosses here from
// cmd/gawk-server (the R2 rule; roomOptions in main.go is the mapping and
// its carry-all test the guard).
type Options struct {
	EmptyGrace      time.Duration
	MaxRooms        int
	MaxBroadcasts   int
	MaxParticipants int
	// CreateSecret gates dynamic-room minting when set (-room-create-secret).
	CreateSecret string
	// Broadcasts is required.
	Broadcasts BroadcastSource
	// Tokens may be installed later with SetTokens (the transport owns the
	// key); Mint and attach fail closed until then.
	Tokens Tokens
	// Obfuscate keys rooms in /statusz and metrics (docs/44 D16). Nil means
	// the raw code is used — tests only; main always passes the hub's HMAC.
	Obfuscate func(string) string
	// PodName / PodAddr identify this pod in room stats and, in cluster
	// mode, the home lease. Empty in single-pod mode.
	PodName string
	Log     *slog.Logger
	// Now and AfterFunc are test seams; zero values mean wall clock.
	Now       func() time.Time
	AfterFunc func(time.Duration, func()) *time.Timer
	// RefreshInterval paces RunRefresh; zero means DefaultRefreshInterval.
	RefreshInterval time.Duration
	// UnknownIsExpired makes Refresh treat a broadcast its source no longer
	// knows as gone: the attachment is removed with reason expired. Set
	// only in cluster mode, where the source is fleet-wide (local hub, else
	// the origin lease) and the hub's expiry hook fires on the ORIGIN pod,
	// which need not be the room's home (docs/44 §4.5). In single-pod mode
	// the hook is the whole lifecycle and the poll ignores unknowns.
	UnknownIsExpired bool
	// OnRoomEnded / OnRoomEmpty / OnAttachmentsChanged are the cluster
	// seams (RM3): a store reacts to them to delete the CR, stamp
	// emptySince, or rewrite status.attachments. All optional, all called
	// OUTSIDE the registry lock.
	OnRoomEnded          func(code string, reason uint8)
	OnRoomEmpty          func(code string, empty bool)
	OnAttachmentsChanged func(code string, attachments []rooms.Attachment)
	// Reserve is consulted at mint before a dynamic code is used, on top
	// of the local hub/room checks: cluster mode reserves the code by
	// creating the Room CR (docs/44 §4.2), single-pod mode leaves it nil.
	// ErrUnavailable and ErrMaxRooms pass through; any other error means
	// "taken", and minting retries with a fresh code.
	Reserve func(ctx context.Context, room *rooms.Room) error
	// Unreserve undoes a Reserve whose code turned out to be taken locally
	// between the pre-check and the insert (an adoption racing the mint):
	// the store deletes the CR and stops renewing its lease. Nil in
	// single-pod mode.
	Unreserve func(ctx context.Context, code string)
	// AttachSecret resolves a static room's attach secret at join time
	// (review finding 3: the portal rotates the Secret in place, so a
	// cached copy on a homed room would keep admitting the old one).
	// found=false means "this room has no Secret reference; use the
	// definition's inline secret"; an error means the reference exists but
	// could not be read — the join fails closed with ErrUnavailable. Nil
	// means inline secrets only (file source, tests).
	AttachSecret func(code string) (secret string, found bool, err error)
}

// Registry is the single-pod room registry.
type Registry struct {
	opts Options
	log  *slog.Logger

	mu    sync.Mutex
	rooms map[string]*room // by normalized code
	// attached maps a broadcast ID to the room it is attached to (D1).
	attached map[string]string
	tokens   Tokens
	enabled  bool
}

type room struct {
	code          string // normalized (CR name)
	display       string // as shown
	name          string // display name
	kind          string // rooms.KindStatic / KindDynamic
	attachSecret  string
	maxBroadcasts int
	creatorFP     string
	createdAt     time.Time

	seq          uint32
	nextPID      uint16
	participants map[uint16]*Participant
	attachments  []*attachment // in attach order
	emptyTimer   *time.Timer
	emptySince   time.Time
	// emptyGen counts timer arms: a callback whose generation is stale
	// (a Join stopped it too late, a Leave re-armed) must not fire.
	emptyGen uint64
	ended    bool
}

type attachment struct {
	id         string
	label      string
	live       bool
	viewers    int
	attachedAt time.Time
	// ownerPID is the participant that attached (or last re-attached) the
	// broadcast; it is what flags that participant as streaming and lets it
	// detach without the creator token.
	ownerPID uint16
}

// NewRegistry builds a registry. opts.Broadcasts must be set.
func NewRegistry(opts Options) *Registry {
	if opts.EmptyGrace <= 0 {
		opts.EmptyGrace = DefaultEmptyGrace
	}
	if opts.MaxRooms <= 0 {
		opts.MaxRooms = DefaultMaxRooms
	}
	if opts.MaxBroadcasts <= 0 {
		opts.MaxBroadcasts = DefaultMaxBroadcasts
	}
	if opts.MaxParticipants <= 0 {
		opts.MaxParticipants = DefaultMaxParticipants
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.AfterFunc == nil {
		opts.AfterFunc = time.AfterFunc
	}
	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = DefaultRefreshInterval
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Obfuscate == nil {
		opts.Obfuscate = func(s string) string { return s }
	}
	return &Registry{
		opts:     opts,
		log:      opts.Log,
		rooms:    make(map[string]*room),
		attached: make(map[string]string),
		tokens:   opts.Tokens,
		enabled:  true,
	}
}

// SetTokens installs the credential implementation (the transport owns the
// resume-token key, so it is wired after construction).
func (r *Registry) SetTokens(t Tokens) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens = t
}

// Has reports whether a normalized code names a live room. The hub calls it
// before minting a broadcast ID so /publish never mints an ID that names a
// live room (docs/44 §4.2 mirror check).
func (r *Registry) Has(code string) bool {
	norm, err := rooms.NormalizeCode(code)
	if err != nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.rooms[norm]
	return ok
}

// Info is a read-only view of a room for the transport and /statusz.
type Info struct {
	Code         string
	Kind         string
	HasSecret    bool
	Participants int
	Attachments  int
}

// Lookup returns a room's Info by any spelling of its code.
func (r *Registry) Lookup(code string) (Info, bool) {
	norm, err := rooms.NormalizeCode(code)
	if err != nil {
		return Info{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rm, ok := r.rooms[norm]
	if !ok {
		return Info{}, false
	}
	return Info{Code: rm.code, Kind: rm.kind, HasSecret: rm.attachSecret != "", Participants: len(rm.participants), Attachments: len(rm.attachments)}, true
}

// StaticRoom is a static room definition as the file source or the CR
// informer hands it over.
type StaticRoom struct {
	Code          string
	DisplayCode   string
	DisplayName   string
	AttachSecret  string
	MaxBroadcasts int
	// Attachments seeds a static room being ADOPTED from its CR
	// (status.attachments, docs/44 §4.3 "rebuilt by the home pod on
	// adoption"); ignored on an update of a room this pod already holds.
	Attachments []rooms.Attachment
}

// UpsertStatic creates or updates a static room. Participants of an updated
// room see nothing (the attach secret and limits apply to the next action);
// a kind change is impossible — a dynamic room of the same code refuses.
func (r *Registry) UpsertStatic(def StaticRoom) error {
	norm, err := rooms.NormalizeCode(def.Code)
	if err != nil {
		return err
	}
	display := def.DisplayCode
	if display == "" {
		display = strings.TrimSpace(def.Code)
	}
	states := r.prefetchStates(def.Attachments)
	r.mu.Lock()
	defer r.mu.Unlock()
	rm, ok := r.rooms[norm]
	if ok {
		if rm.kind != rooms.KindStatic {
			// No code in the text: callers log this error (D16).
			return fmt.Errorf("%w: the code names a live dynamic room", ErrAlreadyAttached)
		}
		rm.display = display
		rm.name = def.DisplayName
		rm.attachSecret = def.AttachSecret
		rm.maxBroadcasts = def.MaxBroadcasts
		return nil
	}
	rm = &room{
		code: norm, display: display, name: def.DisplayName, kind: rooms.KindStatic,
		attachSecret: def.AttachSecret, maxBroadcasts: def.MaxBroadcasts,
		createdAt: r.opts.Now(), nextPID: 1, participants: make(map[uint16]*Participant),
	}
	r.rooms[norm] = rm
	for i := range states {
		if _, taken := r.attached[states[i].id]; taken {
			continue
		}
		r.attachLocked(rm, states[i].id, states[i].label, states[i].state, 0)
	}
	return nil
}

// prefetchStates asks the hub about each CR attachment BEFORE the registry
// lock is taken: the hub's mint path calls back into Has under its own
// lock, so querying it under r.mu is the AB/BA deadlock the review found.
type prefetched struct {
	id, label string
	state     BroadcastState
}

func (r *Registry) prefetchStates(list []rooms.Attachment) []prefetched {
	out := make([]prefetched, 0, len(list))
	for _, at := range list {
		id, err := broadcastid.Normalize(at.BroadcastID)
		if err != nil {
			continue
		}
		state, _ := r.opts.Broadcasts.BroadcastState(id)
		out = append(out, prefetched{id: id, label: at.Label, state: state})
	}
	return out
}

// ReplaceStatic makes the set of static rooms exactly defs: new ones are
// created, changed ones updated, and static rooms no longer listed end
// with 4007 (reason operator). Dynamic rooms are untouched. Malformed
// entries are skipped with a warning, never fatal — the file source's
// posture (docs/42 §4.14).
func (r *Registry) ReplaceStatic(defs []rooms.FileRoom) {
	keep := make(map[string]bool, len(defs))
	for _, d := range defs {
		def := StaticRoom{Code: d.Code, DisplayName: d.DisplayName, AttachSecret: d.AttachSecret, MaxBroadcasts: d.MaxBroadcasts}
		if err := r.UpsertStatic(def); err != nil {
			r.log.Warn("static room skipped", "code_len", len(d.Code), "err", err)
			continue
		}
		norm, _ := rooms.NormalizeCode(d.Code)
		keep[norm] = true
	}
	r.mu.Lock()
	var gone []string
	for code, rm := range r.rooms {
		if rm.kind == rooms.KindStatic && !keep[code] {
			gone = append(gone, code)
		}
	}
	r.mu.Unlock()
	for _, code := range gone {
		r.EndRoom(code, wire.RoomEndReasonOperator)
	}
}

// Grants is what a joining participant is allowed to do (RoomState flags).
type Grants struct {
	Creator  bool
	AttachOK bool
}

// CheckJoin is the pre-upgrade gate for CONNECT /room/{code}: it resolves
// the code, verifies the optional creator token and attach secret, and
// checks the participant limit. It returns the grants the session will
// carry. ErrNotFound → 404, ErrForbidden → 403, ErrFull → 429.
//
// A wrong attach secret is refused even though a viewer needs none: a
// broadcaster that typed the key wrong must learn it now, not after its
// first attach command (docs/44 §4.2, §4.9 "wrong attach secret" state).
func (r *Registry) CheckJoin(code, creatorToken string, attachSecret string, haveAttachSecret bool) (Grants, error) {
	norm, err := rooms.NormalizeCode(code)
	if err != nil {
		return Grants{}, ErrNotFound
	}
	// The attach secret is resolved per join (review finding 3), and
	// outside the lock: the resolver may read a Kubernetes Secret.
	var (
		resolved string
		found    bool
	)
	if haveAttachSecret && r.opts.AttachSecret != nil {
		var err error
		resolved, found, err = r.opts.AttachSecret(norm)
		if err != nil {
			return Grants{}, fmt.Errorf("%w: attach secret: %v", ErrUnavailable, err)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rm, ok := r.rooms[norm]
	if !ok || rm.ended {
		return Grants{}, ErrNotFound
	}
	expected := rm.attachSecret
	if found {
		expected = resolved
	}
	var g Grants
	if creatorToken != "" {
		tok, err := decodeHex(creatorToken)
		if err != nil || r.tokens == nil || !r.tokens.VerifyCreator(norm, tok) {
			return Grants{}, fmt.Errorf("%w: creator token", ErrForbidden)
		}
		g.Creator = true
	}
	switch {
	case rm.kind == rooms.KindDynamic:
		g.AttachOK = true
	case expected == "" && !found:
		g.AttachOK = true
	case haveAttachSecret:
		if expected == "" || subtle.ConstantTimeCompare([]byte(attachSecret), []byte(expected)) != 1 {
			return Grants{}, fmt.Errorf("%w: attach secret", ErrForbidden)
		}
		g.AttachOK = true
	}
	if len(rm.participants) >= r.opts.MaxParticipants {
		return Grants{}, ErrFull
	}
	return g, nil
}

// MintRequest is a /room/new request after the transport's own gates.
type MintRequest struct {
	// BroadcastID and ResumeToken are the attach proof (docs/44 D9).
	BroadcastID string
	ResumeToken []byte
	Label       string
	// CreateSecret is the presented -room-create-secret.
	CreateSecret string
}

// MintResult is what /room/new returns in its first RoomState.
type MintResult struct {
	Code         string // normalized
	Display      string
	CreatorToken []byte
}

// Mint creates a dynamic room with the requesting broadcast attached
// (docs/44 §4.4 step 1). Gate order: create secret (ErrForbidden),
// broadcast known (ErrNotFound — /subscribe already reveals liveness, so
// answering it before the proof leaks nothing), resume proof
// (ErrForbidden), not attached elsewhere (ErrAlreadyAttached), -max-rooms
// (ErrMaxRooms). The room starts
// its empty grace immediately so a mint whose session never arrives is
// reclaimed; the join that follows the upgrade clears it.
func (r *Registry) Mint(ctx context.Context, req MintRequest) (MintResult, error) {
	if r.opts.CreateSecret != "" && subtle.ConstantTimeCompare([]byte(req.CreateSecret), []byte(r.opts.CreateSecret)) != 1 {
		return MintResult{}, fmt.Errorf("%w: create secret", ErrForbidden)
	}
	id, err := broadcastid.Normalize(req.BroadcastID)
	if err != nil {
		return MintResult{}, ErrNotFound
	}
	state, known := r.opts.Broadcasts.BroadcastState(id)
	if !known {
		return MintResult{}, ErrNotFound
	}
	r.mu.Lock()
	tokens := r.tokens
	r.mu.Unlock()
	if tokens == nil || !tokens.VerifyResume(id, req.ResumeToken) {
		return MintResult{}, fmt.Errorf("%w: resume token", ErrForbidden)
	}
	if !utf8.ValidString(req.Label) || len(req.Label) > wire.MaxRoomLabelLen {
		req.Label = ""
	}

	for range 10 {
		raw, err := broadcastid.Mint()
		if err != nil {
			return MintResult{}, err
		}
		norm := strings.ToLower(raw)
		// A dynamic code must not name a live broadcast either (docs/44
		// §4.2): the join box would otherwise resolve it to the room and
		// hide the broadcast, or vice versa.
		if _, taken := r.opts.Broadcasts.BroadcastState(raw); taken {
			continue
		}
		r.mu.Lock()
		if _, taken := r.rooms[norm]; taken {
			r.mu.Unlock()
			continue
		}
		if other, taken := r.attached[id]; taken {
			r.mu.Unlock()
			_ = other // the other room's code stays out of the error text (D16)
			return MintResult{}, fmt.Errorf("%w: broadcast is in another room", ErrAlreadyAttached)
		}
		if r.dynamicCountLocked() >= r.opts.MaxRooms {
			r.mu.Unlock()
			return MintResult{}, ErrMaxRooms
		}
		now := r.opts.Now()
		token := tokens.MintCreator(norm)
		rm := &room{
			code: norm, display: raw, kind: rooms.KindDynamic,
			creatorFP: rooms.Fingerprint(token), createdAt: now,
			nextPID: 1, participants: make(map[uint16]*Participant),
		}
		if r.opts.Reserve != nil {
			// Cluster mode: the CR create is the atomic reservation. Hold
			// no lock across it; re-check for the local race afterwards.
			r.mu.Unlock()
			cr := &rooms.Room{Spec: rooms.RoomSpec{Kind: rooms.KindDynamic}}
			cr.Name = norm
			cr.Status = rooms.RoomStatus{CreatorTokenFingerprint: rm.creatorFP, CreatedAt: metaTime(now)}
			if err := r.opts.Reserve(ctx, cr); err != nil {
				if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrMaxRooms) {
					return MintResult{}, err
				}
				r.log.Debug("dynamic code reservation lost, re-minting", "err", err)
				continue
			}
			r.mu.Lock()
			if _, taken := r.rooms[norm]; taken {
				r.mu.Unlock()
				// The CR was created and its lease is being renewed; give
				// both back or the slot counts against -max-rooms forever.
				if r.opts.Unreserve != nil {
					r.opts.Unreserve(ctx, norm)
				}
				continue
			}
		}
		r.rooms[norm] = rm
		r.attachLocked(rm, id, req.Label, state, 0)
		r.startEmptyGraceLocked(rm)
		r.mu.Unlock()
		r.log.Info("room minted", "room_key", r.opts.Obfuscate(norm), "broadcast_key", r.opts.Obfuscate(id))
		r.notifyAttachments(rm)
		return MintResult{Code: norm, Display: raw, CreatorToken: token}, nil
	}
	return MintResult{}, errCollision
}

func (r *Registry) dynamicCountLocked() int {
	n := 0
	for _, rm := range r.rooms {
		if rm.kind == rooms.KindDynamic {
			n++
		}
	}
	return n
}

// Participant is one control session inside a room.
type Participant struct {
	reg  *Registry
	room *room
	conn Conn

	id       uint16
	kind     uint8
	nick     string
	identity string
	grants   Grants
	// firstState carries the creator token into the very first RoomState
	// after a mint (docs/44 §4.4); nil otherwise.
	firstState []byte

	outbox    chan []byte
	closed    chan struct{}
	leaveOnce sync.Once
	// closeCode / closeReason are what the writer closes with when it
	// reaches the nil sentinel closeAfterDrain queued; set before the
	// sentinel is sent, read after it is received (the channel orders them).
	closeCode   uint32
	closeReason string
}

// ID is the participant's per-room ID.
func (p *Participant) ID() uint16 { return p.id }

// Nickname is the participant's current (possibly suffixed) nickname.
func (p *Participant) Nickname() string {
	p.reg.mu.Lock()
	defer p.reg.mu.Unlock()
	return p.nick
}

// Join adds a control session to a room after the transport has upgraded
// it and read its RoomHello. grants come from CheckJoin (or from Mint for
// the minting session, with creatorToken set so the first RoomState
// carries it). It re-checks the participant limit under lock — the
// pre-upgrade check and this one bracket a race window exactly as
// CheckSubscribe / Subscribe do — and sends the initial RoomState before
// returning. The participant's writer goroutine runs until Leave.
func (r *Registry) Join(code string, hello wire.RoomHello, grants Grants, creatorToken []byte, conn Conn) (*Participant, error) {
	norm, err := rooms.NormalizeCode(code)
	if err != nil {
		return nil, ErrNotFound
	}
	r.mu.Lock()
	rm, ok := r.rooms[norm]
	if !ok || rm.ended {
		r.mu.Unlock()
		return nil, ErrNotFound
	}
	if len(rm.participants) >= r.opts.MaxParticipants {
		r.mu.Unlock()
		return nil, ErrFull
	}
	// IDs are per-room and wrap after 65535 joins (a long-lived static
	// room gets there); skip any ID a live participant still holds.
	pid := rm.nextPID
	for {
		if _, live := rm.participants[pid]; pid != 0 && !live {
			break
		}
		pid++
	}
	p := &Participant{
		reg: r, room: rm, conn: conn,
		id: pid, kind: hello.ClientKind,
		grants: grants, firstState: creatorToken,
		outbox: make(chan []byte, outboxDepth), closed: make(chan struct{}),
	}
	rm.nextPID = pid + 1
	if rm.nextPID == 0 {
		rm.nextPID = 1
	}
	p.nick = rm.uniqueNickLocked(hello.Nickname, p.id)
	rm.participants[p.id] = p
	wasEmpty := r.clearEmptyGraceLocked(rm)
	go p.writeLoop()
	// The joined event is sequenced first so the joiner's snapshot (which
	// already includes it) reports the seq the others are at; both under
	// the lock so no delta can slip between them.
	r.broadcastLocked(rm, wire.RoomEvent{Kind: wire.RoomEventParticipantJoined, Participant: p.recordLocked()}, p.id)
	p.enqueueLocked(r.stateRecordLocked(rm, p))
	p.firstState = nil
	r.mu.Unlock()
	if wasEmpty && r.opts.OnRoomEmpty != nil {
		r.opts.OnRoomEmpty(norm, false)
	}
	return p, nil
}

// Leave removes the participant. Idempotent; the transport calls it when
// the session ends for any reason. Attachments the participant made stay
// (a reload must not detach the broadcast — docs/44 §4.4); only the
// streaming flag on the roster goes with the session.
func (p *Participant) Leave() {
	p.leaveOnce.Do(func() {
		r := p.reg
		rm := p.room
		r.mu.Lock()
		if cur, ok := rm.participants[p.id]; ok && cur == p {
			delete(rm.participants, p.id)
			r.broadcastLocked(rm, wire.RoomEvent{Kind: wire.RoomEventParticipantLeft, Participant: wire.RoomParticipant{ID: p.id}}, 0)
		}
		nowEmpty := !rm.ended && len(rm.participants) == 0
		if nowEmpty {
			r.startEmptyGraceLocked(rm)
		}
		close(p.closed)
		r.mu.Unlock()
		if nowEmpty && rm.kind == rooms.KindDynamic && r.opts.OnRoomEmpty != nil {
			r.opts.OnRoomEmpty(rm.code, true)
		}
	})
}

// writeLoop drains the outbox onto the conn. It ends when the participant
// leaves; a write error ends the session (the transport's read loop then
// calls Leave).
// closeSettle is how long the writer holds a room-ending close after the
// RoomEnding record went out, so the record reaches the client's reader
// before the session close discards it (see writeLoop). Long enough to put
// them in different packets and scheduling slots on any path; short enough
// that "room ended" still lands within one UI frame of the event.
const closeSettle = 250 * time.Millisecond

func (p *Participant) writeLoop() {
	ctx := context.Background()
	for {
		select {
		case <-p.closed:
			return
		case rec := <-p.outbox:
			if rec == nil {
				// closeAfterDrain's sentinel: everything queued before it
				// has been written. Hold closeSettle before the close so
				// the client READS it too — a session close resets every
				// stream (webtransport-go: CancelRead on session close,
				// which discards unread data), so a record and a close
				// that share a packet lose the record. The drain's window
				// before 4002 exists for the same reason (docs/22).
				select {
				case <-time.After(closeSettle):
				case <-p.closed:
				}
				p.conn.Close(p.closeCode, p.closeReason)
				p.Leave()
				return
			}
			if err := p.conn.Write(ctx, rec); err != nil {
				p.conn.Close(uint32(500), "control write failed")
				return
			}
		}
	}
}

// enqueueLocked queues one framed record; on overflow the participant is
// evicted with 4001 (non-terminal: a fresh session restores the credit),
// the R10 rule applied to control records.
func (p *Participant) enqueueLocked(rec []byte) {
	select {
	case p.outbox <- rec:
	default:
		p.reg.log.Warn("room participant unresponsive, evicting", "room_key", p.reg.opts.Obfuscate(p.room.code), "participant", p.id)
		go p.conn.Close(wire.CloseCodeSubscriberUnresponsive, "control queue overflow")
	}
}

// HandleCommand applies one RoomCommand from the participant's stream.
// Every failure goes back as a CommandRejected event; nothing here closes
// the session.
func (p *Participant) HandleCommand(cmd wire.RoomCommand) {
	r := p.reg
	rm := p.room
	// The wire parser normalizes broadcast IDs; a caller that builds the
	// command by hand (tests, the cluster proxy) may not have.
	if cmd.Kind == wire.RoomCommandAttach || cmd.Kind == wire.RoomCommandDetach {
		id, err := broadcastid.Normalize(cmd.BroadcastID)
		if err != nil {
			p.reject(cmd.Kind, wire.RoomRejectNotFound, "invalid broadcast id")
			return
		}
		cmd.BroadcastID = id
	}
	switch cmd.Kind {
	case wire.RoomCommandAttach:
		p.attach(cmd)
	case wire.RoomCommandDetach:
		p.detach(cmd)
	case wire.RoomCommandSetNickname:
		r.mu.Lock()
		if rm.ended {
			r.mu.Unlock()
			return
		}
		p.nick = rm.uniqueNickLocked(cmd.Nickname, p.id)
		r.broadcastLocked(rm, wire.RoomEvent{Kind: wire.RoomEventParticipantUpdated, Participant: p.recordLocked()}, 0)
		r.mu.Unlock()
	case wire.RoomCommandEndRoom:
		r.mu.Lock()
		creator := p.grants.Creator
		dynamic := rm.kind == rooms.KindDynamic
		r.mu.Unlock()
		if !creator || !dynamic {
			p.reject(cmd.Kind, wire.RoomRejectForbidden, "only the creator can end a dynamic room")
			return
		}
		r.EndRoom(rm.code, wire.RoomEndReasonCreator)
	case wire.RoomCommandResync:
		r.mu.Lock()
		if !rm.ended {
			p.enqueueLocked(r.stateRecordLocked(rm, p))
		}
		r.mu.Unlock()
	default:
		p.reject(cmd.Kind, wire.RoomRejectUnsupported, "unsupported command")
	}
}

func (p *Participant) attach(cmd wire.RoomCommand) {
	r := p.reg
	rm := p.room
	r.mu.Lock()
	tokens := r.tokens
	attachOK := p.grants.AttachOK
	r.mu.Unlock()
	if !attachOK {
		p.reject(cmd.Kind, wire.RoomRejectForbidden, "attach key required")
		return
	}
	if tokens == nil || !tokens.VerifyResume(cmd.BroadcastID, cmd.ResumeToken) {
		p.reject(cmd.Kind, wire.RoomRejectBadProof, "resume token does not match")
		return
	}
	state, known := r.opts.Broadcasts.BroadcastState(cmd.BroadcastID)
	if !known {
		p.reject(cmd.Kind, wire.RoomRejectNotFound, "broadcast not found")
		return
	}
	r.mu.Lock()
	if rm.ended {
		r.mu.Unlock()
		return
	}
	if other, taken := r.attached[cmd.BroadcastID]; taken && other != rm.code {
		r.mu.Unlock()
		p.reject(cmd.Kind, wire.RoomRejectAlreadyAttached, "broadcast is in another room")
		return
	}
	if existing := rm.find(cmd.BroadcastID); existing != nil {
		// Idempotent re-attach (a reconnected broadcaster): adopt ownership,
		// refresh the label, and re-flag the participant as streaming.
		existing.ownerPID = p.id
		if cmd.Label != "" {
			existing.label = cmd.Label
		}
		existing.live, existing.viewers = state.Live, state.Viewers
		r.broadcastLocked(rm, wire.RoomEvent{Kind: wire.RoomEventAttachmentUpdated, Attachment: existing.record()}, 0)
		r.broadcastLocked(rm, wire.RoomEvent{Kind: wire.RoomEventParticipantUpdated, Participant: p.recordLocked()}, 0)
		r.mu.Unlock()
		r.notifyAttachments(rm)
		return
	}
	limit := rm.maxBroadcasts
	if limit <= 0 {
		limit = r.opts.MaxBroadcasts
	}
	if len(rm.attachments) >= limit {
		r.mu.Unlock()
		p.reject(cmd.Kind, wire.RoomRejectLimit, "room has no free broadcast slot")
		return
	}
	a := r.attachLocked(rm, cmd.BroadcastID, cmd.Label, state, p.id)
	r.broadcastLocked(rm, wire.RoomEvent{Kind: wire.RoomEventAttachmentAdded, Attachment: a.record()}, 0)
	r.broadcastLocked(rm, wire.RoomEvent{Kind: wire.RoomEventParticipantUpdated, Participant: p.recordLocked()}, 0)
	r.mu.Unlock()
	r.log.Info("broadcast attached", "room_key", r.opts.Obfuscate(rm.code), "broadcast_key", r.opts.Obfuscate(cmd.BroadcastID))
	r.notifyAttachments(rm)
}

func (p *Participant) detach(cmd wire.RoomCommand) {
	r := p.reg
	rm := p.room
	r.mu.Lock()
	if rm.ended {
		r.mu.Unlock()
		return
	}
	a := rm.find(cmd.BroadcastID)
	if a == nil {
		r.mu.Unlock()
		p.reject(cmd.Kind, wire.RoomRejectNotFound, "broadcast is not attached")
		return
	}
	reason := uint8(wire.RoomDetachReasonPublisher)
	if a.ownerPID != p.id {
		if !p.grants.Creator {
			r.mu.Unlock()
			p.reject(cmd.Kind, wire.RoomRejectForbidden, "only the attacher or the creator can detach")
			return
		}
		reason = wire.RoomDetachReasonCreator
	}
	owner := rm.participants[a.ownerPID]
	r.removeAttachmentLocked(rm, a, reason)
	if owner != nil {
		r.broadcastLocked(rm, wire.RoomEvent{Kind: wire.RoomEventParticipantUpdated, Participant: owner.recordLocked()}, 0)
	}
	r.mu.Unlock()
	r.notifyAttachments(rm)
}

func (p *Participant) reject(cmdKind, reason uint8, msg string) {
	r := p.reg
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.room.ended {
		return
	}
	// Addressed to one participant, so it does not advance the room
	// sequence (the others would see a gap); it carries the current one.
	rec, err := wire.AppendRoomEvent(nil, wire.RoomEvent{Seq: p.room.seq, Kind: wire.RoomEventCommandRejected, Command: cmdKind, Reason: reason, Message: msg})
	if err != nil {
		return
	}
	p.enqueueLocked(frame(rec))
}

// --- room helpers (all *Locked) ---------------------------------------

func (rm *room) find(id string) *attachment {
	for _, a := range rm.attachments {
		if a.id == id {
			return a
		}
	}
	return nil
}

func (a *attachment) record() wire.RoomAttachment {
	return wire.RoomAttachment{BroadcastID: a.id, Label: a.label, Live: a.live, ViewerCount: uint32(a.viewers)}
}

func (p *Participant) recordLocked() wire.RoomParticipant {
	var flags uint8
	for _, a := range p.room.attachments {
		if a.ownerPID == p.id {
			flags |= wire.RoomParticipantFlagStreaming
			break
		}
	}
	return wire.RoomParticipant{ID: p.id, Kind: p.kind, Flags: flags, Nickname: p.nick, Identity: p.identity}
}

// uniqueNickLocked returns nick, or nick suffixed " (n)" until it is unique
// in the room (docs/44 D10). An empty or invalid nickname becomes
// "guest-<id>". The result always fits MaxRoomNicknameLen.
func (rm *room) uniqueNickLocked(nick string, self uint16) string {
	nick = strings.TrimSpace(nick)
	if nick == "" || !utf8.ValidString(nick) {
		nick = fmt.Sprintf("guest-%d", self)
	}
	nick = truncate(nick, wire.MaxRoomNicknameLen)
	taken := func(n string) bool {
		for id, p := range rm.participants {
			if id != self && p.nick == n {
				return true
			}
		}
		return false
	}
	if !taken(nick) {
		return nick
	}
	for n := 2; ; n++ {
		suffix := fmt.Sprintf(" (%d)", n)
		cand := truncate(nick, wire.MaxRoomNicknameLen-len(suffix)) + suffix
		if !taken(cand) {
			return cand
		}
	}
}

func truncate(s string, max int) string {
	for len(s) > max {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

func (r *Registry) attachLocked(rm *room, id, label string, state BroadcastState, owner uint16) *attachment {
	a := &attachment{id: id, label: label, live: state.Live, viewers: state.Viewers, attachedAt: r.opts.Now(), ownerPID: owner}
	rm.attachments = append(rm.attachments, a)
	r.attached[id] = rm.code
	return a
}

func (r *Registry) removeAttachmentLocked(rm *room, a *attachment, reason uint8) {
	for i, x := range rm.attachments {
		if x == a {
			rm.attachments = append(rm.attachments[:i], rm.attachments[i+1:]...)
			break
		}
	}
	delete(r.attached, a.id)
	r.broadcastLocked(rm, wire.RoomEvent{Kind: wire.RoomEventAttachmentRemoved, Attachment: wire.RoomAttachment{BroadcastID: a.id}, Reason: reason}, 0)
}

// broadcastLocked assigns the next seq and enqueues one event to every
// participant except skip (0 = nobody).
func (r *Registry) broadcastLocked(rm *room, ev wire.RoomEvent, skip uint16) {
	rm.seq++
	ev.Seq = rm.seq
	rec, err := wire.AppendRoomEvent(nil, ev)
	if err != nil {
		r.log.Error("room event encode failed", "err", err)
		return
	}
	framed := frame(rec)
	for id, p := range rm.participants {
		if id == skip {
			continue
		}
		p.enqueueLocked(framed)
	}
}

func (r *Registry) stateRecordLocked(rm *room, p *Participant) []byte {
	var flags uint8
	if rm.kind == rooms.KindDynamic {
		flags |= wire.RoomStateFlagDynamic
	}
	if p.grants.Creator {
		flags |= wire.RoomStateFlagCreator
	}
	if p.grants.AttachOK {
		flags |= wire.RoomStateFlagAttachOK
	}
	st := wire.RoomState{Flags: flags, Seq: rm.seq, YourID: p.id, Code: rm.display, DisplayName: rm.name, CreatorToken: p.firstState, Key: r.roomKey(rm.code)}
	for _, a := range rm.attachments {
		st.Attachments = append(st.Attachments, a.record())
	}
	ids := make([]int, 0, len(rm.participants))
	for id := range rm.participants {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	for _, id := range ids {
		st.Participants = append(st.Participants, rm.participants[uint16(id)].recordLocked())
	}
	rec, err := wire.AppendRoomState(nil, st)
	if err != nil {
		r.log.Error("room state encode failed", "err", err)
		return nil
	}
	return frame(rec)
}

// roomKey is the room's HMAC'd handle as raw bytes for RoomState (docs/44
// D16): the obfuscator yields the same 12-hex digest /statusz is keyed by;
// decoded here so the client can report it to telemetry. Nil when the
// obfuscator is not the fleet HMAC (tests with the identity obfuscator).
func (r *Registry) roomKey(code string) []byte {
	key, err := hex.DecodeString(r.opts.Obfuscate(code))
	if err != nil || len(key) != wire.RoomKeySize {
		return nil
	}
	return key
}

func frame(msg []byte) []byte {
	rec, err := wire.AppendRoomRecord(nil, msg)
	if err != nil {
		return nil
	}
	return rec
}

// startEmptyGraceLocked arms the empty timer for a dynamic room; static
// rooms never end on their own (docs/44 D7).
func (r *Registry) startEmptyGraceLocked(rm *room) {
	if rm.kind != rooms.KindDynamic || rm.ended || rm.emptyTimer != nil {
		return
	}
	rm.emptySince = r.opts.Now()
	code := rm.code
	rm.emptyGen++
	gen := rm.emptyGen
	rm.emptyTimer = r.opts.AfterFunc(r.opts.EmptyGrace, func() {
		// Only THIS arm may end the room: a callback that lost the lock
		// race to a Join+Leave pair sees a newer generation and yields to
		// the fresh grace (the reload-at-expiry case, docs/44 D7).
		r.mu.Lock()
		cur, ok := r.rooms[code]
		expired := ok && cur == rm && len(rm.participants) == 0 && rm.emptyTimer != nil && rm.emptyGen == gen
		r.mu.Unlock()
		if expired {
			r.EndRoom(code, wire.RoomEndReasonEmpty)
		}
	})
}

// clearEmptyGraceLocked disarms the empty timer; reports whether one was
// armed (so the caller can tell the store the room is populated again).
func (r *Registry) clearEmptyGraceLocked(rm *room) bool {
	if rm.emptyTimer == nil {
		return false
	}
	rm.emptyTimer.Stop()
	rm.emptyTimer = nil
	rm.emptySince = time.Time{}
	return true
}

// EndRoom ends a room (docs/44 §4.4 step 4): every participant gets a
// RoomEnding event and then 4007, attachments are released (the broadcasts
// themselves are untouched, D1), and the room is forgotten. Idempotent.
func (r *Registry) EndRoom(code string, reason uint8) {
	norm, err := rooms.NormalizeCode(code)
	if err != nil {
		return
	}
	r.mu.Lock()
	rm, ok := r.rooms[norm]
	if !ok || rm.ended {
		r.mu.Unlock()
		return
	}
	rm.ended = true
	r.clearEmptyGraceLocked(rm)
	delete(r.rooms, norm)
	for _, a := range rm.attachments {
		delete(r.attached, a.id)
	}
	r.broadcastLocked(rm, wire.RoomEvent{Kind: wire.RoomEventRoomEnding, Reason: reason}, 0)
	parts := make([]*Participant, 0, len(rm.participants))
	for _, p := range rm.participants {
		parts = append(parts, p)
	}
	r.mu.Unlock()
	r.log.Info("room ended", "room_key", r.opts.Obfuscate(norm), "kind", rm.kind, "reason", reason)
	for _, p := range parts {
		// Let the writer flush the RoomEnding record, then close: the
		// close is what the client acts on (4007 is terminal), the event
		// is what it shows.
		p.closeAfterDrain(wire.CloseCodeRoomEnded, "room ended")
	}
	if r.opts.OnRoomEnded != nil {
		r.opts.OnRoomEnded(norm, reason)
	}
}

// closeAfterDrain closes the conn once queued records are written (or after
// a short bound), then marks the participant gone.
func (p *Participant) closeAfterDrain(code uint32, reason string) {
	// In-order, through the writer: a nil record is the sentinel the
	// writeLoop turns into the close once every record queued ahead of it
	// is on the wire. Polling "outbox empty" was not the same thing — the
	// outbox empties the instant the writer DEQUEUES the last record,
	// before its Write returns, and a client whose session closed mid-write
	// never sees the RoomEnding the close was supposed to follow.
	p.closeCode, p.closeReason = code, reason
	select {
	case p.outbox <- nil:
	default:
		// The outbox is full: this participant is not draining, and the
		// eviction rule (enqueueLocked) already applies. Close now rather
		// than wait on a writer that may be stuck behind it.
		go func() {
			p.conn.Close(code, reason)
			p.Leave()
		}()
	}
}

// PublisherClosed is the hub's OnPublisherClosed hook: the broadcast's
// publisher went away (grace started). Its attachment flips to away.
func (r *Registry) PublisherClosed(id string) {
	r.setLive(id, false)
}

// BroadcastExpired is the hub's OnBroadcastExpired hook: grace GC deleted
// the broadcast, so its attachment is removed (docs/44 §4.4 "Attachment").
func (r *Registry) BroadcastExpired(id string) {
	norm, err := broadcastid.Normalize(id)
	if err != nil {
		return
	}
	r.mu.Lock()
	code, ok := r.attached[norm]
	if !ok {
		r.mu.Unlock()
		return
	}
	rm := r.rooms[code]
	a := rm.find(norm)
	var owner *Participant
	if a != nil {
		owner = rm.participants[a.ownerPID]
		r.removeAttachmentLocked(rm, a, wire.RoomDetachReasonExpired)
		if owner != nil {
			r.broadcastLocked(rm, wire.RoomEvent{Kind: wire.RoomEventParticipantUpdated, Participant: owner.recordLocked()}, 0)
		}
	}
	r.mu.Unlock()
	r.notifyAttachments(rm)
}

func (r *Registry) setLive(id string, live bool) {
	norm, err := broadcastid.Normalize(id)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	code, ok := r.attached[norm]
	if !ok {
		return
	}
	rm := r.rooms[code]
	if a := rm.find(norm); a != nil && a.live != live {
		a.live = live
		r.broadcastLocked(rm, wire.RoomEvent{Kind: wire.RoomEventAttachmentUpdated, Attachment: a.record()}, 0)
	}
}

// Refresh re-reads every attachment's state from the hub and pushes an
// AttachmentUpdated for each change (viewer counts, and the live flag in
// case a hook was missed). RunRefresh calls it on a ticker. With
// UnknownIsExpired (cluster mode) it is also the expiry path for an
// attachment homed away from its origin: no local hub and no lease means
// the broadcast is gone fleet-wide, and the attachment goes the way
// BroadcastExpired takes it.
//
// The hub is queried with r.mu RELEASED: hub.StartPublish holds the hub
// lock while it asks IDReserved → Has, which takes r.mu, so asking the hub
// under r.mu is an AB/BA deadlock (review finding 1). Snapshot, query,
// re-lock, re-find — an attachment may have gone in between.
func (r *Registry) Refresh() {
	type probe struct {
		rm *room
		id string
	}
	r.mu.Lock()
	var probes []probe
	for _, rm := range r.rooms {
		for _, a := range rm.attachments {
			probes = append(probes, probe{rm: rm, id: a.id})
		}
	}
	r.mu.Unlock()
	for _, p := range probes {
		state, known := r.opts.Broadcasts.BroadcastState(p.id)
		if !known {
			if r.opts.UnknownIsExpired {
				// Cluster mode: the origin's expiry hook landed on the
				// origin pod's registry, not here. The fleet-wide source
				// saying "unknown" is the same fact, so take the same
				// path (participants told, the CR's list rewritten).
				r.BroadcastExpired(p.id)
				continue
			}
			// Single-pod mode: the expiry hook handles removal; a missed
			// one is caught by the next hook delivery, never by a poll.
			continue
		}
		r.mu.Lock()
		if !p.rm.ended {
			if a := p.rm.find(p.id); a != nil && (state.Live != a.live || state.Viewers != a.viewers) {
				a.live, a.viewers = state.Live, state.Viewers
				r.broadcastLocked(p.rm, wire.RoomEvent{Kind: wire.RoomEventAttachmentUpdated, Attachment: a.record()}, 0)
			}
		}
		r.mu.Unlock()
	}
}

// RunRefresh runs Refresh on the configured interval until ctx ends.
func (r *Registry) RunRefresh(ctx context.Context) {
	t := time.NewTicker(r.opts.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Refresh()
		}
	}
}

// notifyAttachments hands the cluster seam the current attachment list.
func (r *Registry) notifyAttachments(rm *room) {
	if r.opts.OnAttachmentsChanged == nil {
		return
	}
	r.mu.Lock()
	if rm.ended {
		r.mu.Unlock()
		return
	}
	list := make([]rooms.Attachment, 0, len(rm.attachments))
	for _, a := range rm.attachments {
		list = append(list, rooms.Attachment{BroadcastID: a.id, Label: a.label, AttachedAt: metaTime(a.attachedAt)})
	}
	r.mu.Unlock()
	r.opts.OnAttachmentsChanged(rm.code, list)
}

// Attachments returns the current attachments of a room (cluster adoption
// and tests).
func (r *Registry) Attachments(code string) []rooms.Attachment {
	norm, err := rooms.NormalizeCode(code)
	if err != nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rm, ok := r.rooms[norm]
	if !ok {
		return nil
	}
	list := make([]rooms.Attachment, 0, len(rm.attachments))
	for _, a := range rm.attachments {
		list = append(list, rooms.Attachment{BroadcastID: a.id, Label: a.label, AttachedAt: metaTime(a.attachedAt)})
	}
	return list
}

// AdoptDynamic recreates a dynamic room from its CR on this pod (cluster
// adoption, docs/44 §4.5): attachments are rebuilt from the CR, the roster
// starts empty, and the empty grace is armed so an adopted room nobody
// rejoins still ends. Returns false when the code is already live here.
func (r *Registry) AdoptDynamic(cr *rooms.Room) bool {
	norm, err := rooms.NormalizeCode(cr.Name)
	if err != nil {
		return false
	}
	states := r.prefetchStates(cr.Status.Attachments)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.rooms[norm]; exists {
		return false
	}
	rm := &room{
		code: norm, display: rooms.DisplayCode(cr), kind: rooms.KindDynamic,
		creatorFP: cr.Status.CreatorTokenFingerprint, createdAt: r.opts.Now(),
		nextPID: 1, participants: make(map[uint16]*Participant),
	}
	if cr.Status.CreatedAt != nil {
		rm.createdAt = cr.Status.CreatedAt.Time
	}
	for i := range states {
		if _, taken := r.attached[states[i].id]; taken {
			continue
		}
		r.attachLocked(rm, states[i].id, states[i].label, states[i].state, 0)
	}
	r.rooms[norm] = rm
	r.startEmptyGraceLocked(rm)
	return true
}

// RoomStats is one row of the /statusz rooms section, keyed by the HMAC'd
// code (docs/44 §4.10).
type RoomStats struct {
	Kind         string `json:"kind"`
	Participants int    `json:"participants"`
	Attachments  int    `json:"attachments"`
	// Role is "home" on the pod holding the room; "proxy" rows are added
	// by the transport for sessions it forwards elsewhere (RM3).
	Role       string `json:"role"`
	EmptySince string `json:"emptySince,omitempty"`
}

// Stats snapshots every room this pod holds.
func (r *Registry) Stats() map[string]RoomStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]RoomStats, len(r.rooms))
	for code, rm := range r.rooms {
		row := RoomStats{Kind: rm.kind, Participants: len(rm.participants), Attachments: len(rm.attachments), Role: "home"}
		if !rm.emptySince.IsZero() {
			row.EmptySince = rm.emptySince.UTC().Format(time.RFC3339)
		}
		out[r.opts.Obfuscate(code)] = row
	}
	return out
}

// Totals are the fleet-facing gauges (metrics).
type Totals struct {
	Static, Dynamic, Participants, Attachments int
}

// TotalStats sums across rooms.
func (r *Registry) TotalStats() Totals {
	r.mu.Lock()
	defer r.mu.Unlock()
	var t Totals
	for _, rm := range r.rooms {
		if rm.kind == rooms.KindDynamic {
			t.Dynamic++
		} else {
			t.Static++
		}
		t.Participants += len(rm.participants)
		t.Attachments += len(rm.attachments)
	}
	return t
}

func decodeHex(s string) ([]byte, error) {
	if len(s) != wire.RoomCreatorTokenSize*2 {
		return nil, fmt.Errorf("token length %d", len(s))
	}
	out := make([]byte, wire.RoomCreatorTokenSize)
	for i := 0; i < len(out); i++ {
		hi, lo := unhex(s[2*i]), unhex(s[2*i+1])
		if hi < 0 || lo < 0 {
			return nil, errors.New("non-hex token")
		}
		out[i] = byte(hi<<4 | lo)
	}
	return out, nil
}

func unhex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}
