// Package mumble is the bridge's own Mumble client (docs/80 §11.1 C1): the
// TLS control channel, the few protobuf messages the bridge needs, and Opus
// voice in the legacy packet format through the TCP tunnel. No UDP, no
// codec: Opus passes through untouched.
//
// It keeps the three properties docs/80 D6 asked of a gumble fork:
//
//   - raw audio: every Opus packet reaches a listener with its sender
//     session, sequence number, target and end-of-transmission bit, never
//     decoded (AudioPacket);
//   - delivery never blocks the read loop: each listener has a bounded
//     queue that drops its OLDEST packet on overflow and counts the drop
//     (AudioQueue);
//   - the reported client version is configurable (Config.Version), which
//     is what makes murmur speak the legacy format to us.
//
// Nothing in this package logs, stores or formats audio bytes (D24).
package mumble

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultVersion is the client version the bridge reports: below 1.5.0, so
// murmur sends the legacy voice packet on the TCP tunnel (docs/80 §11.4).
var DefaultVersion = Version{Major: 1, Minor: 4, Patch: 0}

// LegacyAudioBefore is the first version murmur speaks the protobuf voice
// format to; this client only speaks the legacy one.
var LegacyAudioBefore = Version{Major: 1, Minor: 5, Patch: 0}

// MinOpusVersion is the first Mumble version with Opus.
var MinOpusVersion = Version{Major: 1, Minor: 2, Patch: 4}

// Defaults.
const (
	DefaultDialTimeout  = 10 * time.Second
	DefaultPingInterval = 15 * time.Second
	writeTimeout        = 10 * time.Second
	controlQueueLen     = 64
	audioSendQueueLen   = 64
	maxDenials          = 16
)

// Config configures one connection.
type Config struct {
	// Addr is murmur's host:port.
	Addr string
	// Username is the name to log in with; Password the server password
	// (or a registered user's); Tokens the access tokens (D10).
	Username string
	Password string
	Tokens   []string
	// TLS configures trust in murmur's certificate.
	TLS TLSOptions
	// Certificate is the client certificate. Nil: a fresh one is generated
	// (NewClientCertificate); murmur needs one to create a temporary
	// channel. NoCertificate turns that off.
	Certificate   *tls.Certificate
	NoCertificate bool
	// Version is the reported client version; zero is DefaultVersion.
	Version Version
	// Release and OS are reported in Version; informational.
	Release   string
	OS        string
	OSVersion string
	// SelfMute and SelfDeaf are applied right after login.
	SelfMute bool
	SelfDeaf bool
	// AudioQueue, when > 0, creates the Audio() listener before the read
	// loop starts, so no packet is missed between login and listening.
	AudioQueue int
	// DialTimeout bounds the TCP and TLS handshake and the login; zero is
	// DefaultDialTimeout.
	DialTimeout time.Duration
	// PingInterval is the keepalive (murmur drops a silent client after
	// 30 s); zero is DefaultPingInterval.
	PingInterval time.Duration
	// DialContext replaces the TCP dialer (tests).
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Channel is one channel as murmur reported it.
type Channel struct {
	ID        uint32
	Parent    uint32
	Name      string
	Temporary bool
}

// User is one connected user as murmur reported it.
type User struct {
	Session uint32
	Name    string
	Channel uint32
	// UserID is the registered user ID, -1 for an unregistered user.
	UserID   int64
	Mute     bool // server mute
	Deaf     bool // server deafen
	Suppress bool
	SelfMute bool
	SelfDeaf bool
}

// Stats counts what the connection carried.
type Stats struct {
	AudioReceived uint64 // Opus packets delivered to listeners
	AudioIgnored  uint64 // voice packets that were not legacy Opus
	AudioSent     uint64
	AudioDropped  uint64 // sends dropped because the send queue was full
}

type denial struct {
	seq uint64
	d   PermissionDenied
}

// Client is one logged-in Mumble connection.
type Client struct {
	cfg  Config
	conn net.Conn

	ctrl  chan []byte // control frames, never dropped
	audio chan []byte // voice frames, dropped when full

	mu        sync.Mutex
	self      uint32
	channels  map[uint32]*Channel
	users     map[uint32]*User
	changed   chan struct{} // closed and replaced on every state change
	denials   []denial
	denySeq   uint64
	listeners []*AudioQueue
	err       error

	audioQ *AudioQueue

	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	audioReceived, audioIgnored, audioSent, audioDropped atomic.Uint64
}

// Dial connects to murmur, logs in and returns once murmur has sent
// ServerSync. Errors are typed: *DialError (unreachable or the TLS check
// failed), *RejectError (murmur refused the login), ErrRefused (murmur
// closed the connection before login without a Reject: the autoban).
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Version == (Version{}) {
		cfg.Version = DefaultVersion
	}
	if cfg.Release == "" {
		cfg.Release = "gawk-mumble-bridge"
	}
	if cfg.OS == "" {
		cfg.OS = "gawk"
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = DefaultDialTimeout
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = DefaultPingInterval
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return nil, &DialError{Err: fmt.Errorf("address %q: %w", cfg.Addr, err)}
	}
	tlsCfg, err := cfg.TLS.Config(host)
	if err != nil {
		return nil, &DialError{Err: err}
	}
	if !cfg.NoCertificate {
		cert := cfg.Certificate
		if cert == nil {
			gen, err := NewClientCertificate(cfg.Username)
			if err != nil {
				return nil, &DialError{Err: fmt.Errorf("client certificate: %w", err)}
			}
			cert = &gen
		}
		tlsCfg.Certificates = []tls.Certificate{*cert}
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.DialTimeout)
	defer cancel()
	dial := cfg.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	raw, err := dial(ctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, &DialError{Err: err}
	}
	conn := tls.Client(raw, tlsCfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		// Murmur drops a banned address's TCP connection before the TLS
		// handshake: an EOF here is the autoban, not an unreachable server.
		if isPeerClosed(err) {
			return nil, ErrRefused
		}
		return nil, &DialError{Err: err}
	}
	c := &Client{
		cfg:      cfg,
		conn:     conn,
		ctrl:     make(chan []byte, controlQueueLen),
		audio:    make(chan []byte, audioSendQueueLen),
		channels: make(map[uint32]*Channel),
		users:    make(map[uint32]*User),
		changed:  make(chan struct{}),
		done:     make(chan struct{}),
	}
	if cfg.AudioQueue > 0 {
		c.audioQ = NewAudioQueue(cfg.AudioQueue)
		c.listeners = append(c.listeners, c.audioQ)
	}
	if err := c.login(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if cfg.SelfMute || cfg.SelfDeaf {
		c.sendNow(msgUserState, encodeUserState(userStateUpdate{Session: c.self, SelfMute: &cfg.SelfMute, SelfDeaf: &cfg.SelfDeaf}))
	}
	c.wg.Add(3)
	go c.readLoop()
	go c.writeLoop()
	go c.pingLoop()
	return c, nil
}

// isPeerClosed reports an error that means the server hung up on us.
func isPeerClosed(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && (op.Op == "read" || op.Op == "write") {
		return true // connection reset by peer, broken pipe
	}
	return false
}

// sendNow writes a frame synchronously; only before the loops start.
func (c *Client) sendNow(typ uint16, payload []byte) error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := c.conn.Write(appendFrame(nil, typ, payload))
	return err
}

func (c *Client) login(ctx context.Context) error {
	if dl, ok := ctx.Deadline(); ok {
		_ = c.conn.SetDeadline(dl)
	}
	defer c.conn.SetDeadline(time.Time{})
	if err := c.sendNow(msgVersion, encodeVersion(c.cfg.Version, c.cfg.Release, c.cfg.OS, c.cfg.OSVersion)); err != nil {
		return c.loginIOError(err)
	}
	if err := c.sendNow(msgAuthenticate, encodeAuthenticate(c.cfg.Username, c.cfg.Password, c.cfg.Tokens)); err != nil {
		return c.loginIOError(err)
	}
	for {
		typ, body, err := readFrame(c.conn)
		if err != nil {
			return c.loginIOError(err)
		}
		switch typ {
		case msgReject:
			r, err := decodeReject(body)
			if err != nil {
				return &DialError{Err: fmt.Errorf("bad Reject: %w", err)}
			}
			return r
		case msgServerSync:
			s, err := decodeServerSync(body)
			if err != nil || !s.hasSession {
				return &DialError{Err: errors.New("ServerSync without a session")}
			}
			c.mu.Lock()
			c.self = s.session
			c.mu.Unlock()
			return nil
		default:
			if err := c.handle(typ, body); err != nil {
				return &DialError{Err: err}
			}
		}
	}
}

func (c *Client) loginIOError(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &DialError{Err: fmt.Errorf("login timed out: %w", err)}
	}
	if isPeerClosed(err) {
		return ErrRefused
	}
	return &DialError{Err: err}
}

// --- loops -------------------------------------------------------------------

func (c *Client) readLoop() {
	defer c.wg.Done()
	for {
		typ, body, err := readFrame(c.conn)
		if err != nil {
			c.fail(fmt.Errorf("mumble: connection lost: %w", err))
			return
		}
		if err := c.handle(typ, body); err != nil {
			c.fail(err)
			return
		}
	}
}

func (c *Client) writeLoop() {
	defer c.wg.Done()
	for {
		var frame []byte
		// Control first: a join or a mute never waits behind voice.
		select {
		case frame = <-c.ctrl:
		default:
			select {
			case frame = <-c.ctrl:
			case frame = <-c.audio:
			case <-c.done:
				return
			}
		}
		_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := c.conn.Write(frame); err != nil {
			c.fail(fmt.Errorf("mumble: write: %w", err))
			return
		}
	}
}

func (c *Client) pingLoop() {
	defer c.wg.Done()
	t := time.NewTicker(c.cfg.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_ = c.send(msgPing, encodePing(uint64(time.Now().UnixMilli())))
		case <-c.done:
			return
		}
	}
}

// fail ends the client with err (the first cause wins).
func (c *Client) fail(err error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.err = err
		ls := c.listeners
		c.notifyLocked()
		c.mu.Unlock()
		close(c.done)
		_ = c.conn.Close()
		for _, l := range ls {
			l.Close()
		}
	})
}

// Close disconnects. Safe to call more than once and concurrently.
func (c *Client) Close() error {
	c.fail(ErrClosed)
	c.wg.Wait()
	return nil
}

// Done is closed when the connection has ended; Err says why.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is why the connection ended: *RemovedError for a kick or ban,
// ErrClosed after Close, otherwise the I/O error. Nil while connected.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// send queues a control frame. It blocks only while the control queue is
// full, never past the connection's end.
func (c *Client) send(typ uint16, payload []byte) error {
	frame := appendFrame(nil, typ, payload)
	select {
	case <-c.done:
		return ErrClosed
	default:
	}
	select {
	case c.ctrl <- frame:
		return nil
	case <-c.done:
		return ErrClosed
	}
}

// --- message handling ----------------------------------------------------------

func (c *Client) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *Client) handle(typ uint16, body []byte) error {
	switch typ {
	case msgUDPTunnel:
		c.handleAudio(body)
	case msgChannelState:
		cs, err := decodeChannelState(body)
		if err != nil {
			return fmt.Errorf("mumble: ChannelState: %w", err)
		}
		if !cs.hasID {
			return nil
		}
		c.mu.Lock()
		ch := c.channels[cs.id]
		if ch == nil {
			ch = &Channel{ID: cs.id}
			c.channels[cs.id] = ch
		}
		if cs.parent != nil {
			ch.Parent = *cs.parent
		}
		if cs.name != nil {
			ch.Name = *cs.name
		}
		if cs.temporary != nil {
			ch.Temporary = *cs.temporary
		}
		c.notifyLocked()
		c.mu.Unlock()
	case msgChannelRemove:
		id, err := decodeChannelRemove(body)
		if err != nil {
			return err
		}
		c.mu.Lock()
		delete(c.channels, id)
		c.notifyLocked()
		c.mu.Unlock()
	case msgUserState:
		us, err := decodeUserState(body)
		if err != nil {
			return fmt.Errorf("mumble: UserState: %w", err)
		}
		if !us.hasSession {
			return nil
		}
		c.mu.Lock()
		u := c.users[us.session]
		if u == nil {
			u = &User{Session: us.session, UserID: -1}
			c.users[us.session] = u
		}
		if us.name != nil {
			u.Name = *us.name
		}
		if us.userID != nil {
			u.UserID = int64(*us.userID)
		}
		if us.channel != nil {
			u.Channel = *us.channel
		}
		setBool(&u.Mute, us.mute)
		setBool(&u.Deaf, us.deaf)
		setBool(&u.Suppress, us.suppress)
		setBool(&u.SelfMute, us.selfMute)
		setBool(&u.SelfDeaf, us.selfDeaf)
		c.notifyLocked()
		c.mu.Unlock()
	case msgUserRemove:
		ur, err := decodeUserRemove(body)
		if err != nil {
			return fmt.Errorf("mumble: UserRemove: %w", err)
		}
		c.mu.Lock()
		delete(c.users, ur.session)
		self := c.self
		c.notifyLocked()
		c.mu.Unlock()
		if ur.session == self && self != 0 {
			return &RemovedError{Ban: ur.ban, Actor: ur.actor, HasActor: ur.hasActor, Reason: ur.reason}
		}
	case msgPermissionDenied:
		d, err := decodePermissionDenied(body)
		if err != nil {
			return fmt.Errorf("mumble: PermissionDenied: %w", err)
		}
		c.mu.Lock()
		c.denySeq++
		c.denials = append(c.denials, denial{seq: c.denySeq, d: d})
		if len(c.denials) > maxDenials {
			c.denials = c.denials[len(c.denials)-maxDenials:]
		}
		c.notifyLocked()
		c.mu.Unlock()
	case msgReject:
		r, err := decodeReject(body)
		if err != nil {
			return err
		}
		return r
	default:
		// Ping replies, CryptSetup (no UDP), CodecVersion, ServerConfig,
		// SuggestConfig, PermissionQuery, TextMessage, ACL, …: nothing the
		// bridge acts on.
	}
	return nil
}

func setBool(dst *bool, v *bool) {
	if v != nil {
		*dst = *v
	}
}

func (c *Client) handleAudio(body []byte) {
	p, err := ParseServerAudio(body)
	if err != nil {
		c.audioIgnored.Add(1)
		return
	}
	c.audioReceived.Add(1)
	c.mu.Lock()
	ls := c.listeners
	c.mu.Unlock()
	for _, l := range ls {
		l.Push(p)
	}
}

// --- audio -----------------------------------------------------------------------

// Audio is the listener Config.AudioQueue created, or nil.
func (c *Client) Audio() *AudioQueue { return c.audioQ }

// AddAudioListener registers a new bounded listener queue.
func (c *Client) AddAudioListener(size int) *AudioQueue {
	q := NewAudioQueue(size)
	c.mu.Lock()
	if c.err != nil {
		q.Close()
	}
	c.listeners = append(c.listeners, q)
	c.mu.Unlock()
	return q
}

// WriteAudio sends one Opus packet as this user's voice (target 0) through
// the TCP tunnel. It never blocks: when the send queue is full the packet is
// dropped and counted, and WriteAudio reports false.
func (c *Client) WriteAudio(seq int64, opus []byte, end bool) (bool, error) {
	return c.WriteAudioTarget(0, seq, opus, end)
}

// WriteAudioTarget is WriteAudio to a voice target.
func (c *Client) WriteAudioTarget(target uint8, seq int64, opus []byte, end bool) (bool, error) {
	pkt, err := AppendClientAudio(nil, target, seq, opus, end)
	if err != nil {
		return false, err
	}
	frame := appendFrame(nil, msgUDPTunnel, pkt)
	select {
	case <-c.done:
		return false, ErrClosed
	default:
	}
	select {
	case c.audio <- frame:
		c.audioSent.Add(1)
		return true, nil
	default:
		c.audioDropped.Add(1)
		return false, nil
	}
}

// Stats returns the connection's counters.
func (c *Client) Stats() Stats {
	return Stats{
		AudioReceived: c.audioReceived.Load(),
		AudioIgnored:  c.audioIgnored.Load(),
		AudioSent:     c.audioSent.Load(),
		AudioDropped:  c.audioDropped.Load(),
	}
}

// --- state -------------------------------------------------------------------------

// Changed returns a channel closed at the next state change (a channel or
// user update, a removal, a PermissionDenied, the connection ending).
func (c *Client) Changed() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changed
}

// Session is our own session ID.
func (c *Client) Session() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.self
}

// Self is our own user as murmur reports it.
func (c *Client) Self() User {
	c.mu.Lock()
	defer c.mu.Unlock()
	if u := c.users[c.self]; u != nil {
		return *u
	}
	return User{Session: c.self, UserID: -1}
}

// User returns one user by session.
func (c *Client) User(session uint32) (User, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if u := c.users[session]; u != nil {
		return *u, true
	}
	return User{}, false
}

// Users returns every connected user, by session.
func (c *Client) Users() []User {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]User, 0, len(c.users))
	for _, u := range c.users {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Session < out[j].Session })
	return out
}

// UsersIn returns the users in one channel, by session.
func (c *Client) UsersIn(channel uint32) []User {
	all := c.Users()
	out := all[:0]
	for _, u := range all {
		if u.Channel == channel {
			out = append(out, u)
		}
	}
	return out
}

// Channel returns one channel by ID.
func (c *Client) Channel(id uint32) (Channel, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch := c.channels[id]; ch != nil {
		return *ch, true
	}
	return Channel{}, false
}

// Channels returns every channel, by ID.
func (c *Client) Channels() []Channel {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Channel, 0, len(c.channels))
	for _, ch := range c.channels {
		out = append(out, *ch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// FindChild returns the child of parent named name.
func (c *Client) FindChild(parent uint32, name string) (uint32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.findChildLocked(parent, name)
}

func (c *Client) findChildLocked(parent uint32, name string) (uint32, bool) {
	best, found := uint32(0), false
	for id, ch := range c.channels {
		if id != 0 && ch.Parent == parent && ch.Name == name && (!found || id < best) {
			best, found = id, true
		}
	}
	return best, found
}

// FindPath resolves a channel path below the root by names; an empty path is
// the root (channel 0).
func (c *Client) FindPath(path []string) (uint32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := uint32(0)
	for _, name := range path {
		next, ok := c.findChildLocked(id, name)
		if !ok {
			return 0, false
		}
		id = next
	}
	return id, true
}

// waitFor blocks until cond holds, a PermissionDenied arrives after the call
// started, the connection ends, or ctx is done.
func (c *Client) waitFor(ctx context.Context, startSeq uint64, cond func() bool) error {
	for {
		c.mu.Lock()
		ok := cond()
		var denied *PermissionDenied
		for i := range c.denials {
			if c.denials[i].seq > startSeq {
				denied = &c.denials[i].d
				break
			}
		}
		ch, err := c.changed, c.err
		c.mu.Unlock()
		switch {
		case ok:
			return nil
		case denied != nil:
			return &DeniedError{PermissionDenied: *denied}
		case err != nil:
			return err
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *Client) denyMark() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.denySeq
}

// JoinChannel moves us into channel id and waits until murmur confirms it.
// A refusal is a *DeniedError.
func (c *Client) JoinChannel(ctx context.Context, id uint32) error {
	mark := c.denyMark()
	self := c.Session()
	if err := c.send(msgUserState, encodeUserState(userStateUpdate{Session: self, Channel: &id})); err != nil {
		return err
	}
	return c.waitFor(ctx, mark, func() bool {
		u := c.users[self]
		return u != nil && u.Channel == id
	})
}

// CreateChannel asks murmur for a channel named name under parent and waits
// for it to appear. A temporary channel moves its creator into it (murmur's
// rule), so CreateChannel with temporary also waits for that move. A refusal
// is a *DeniedError.
//
// A channel that already exists under parent with that name is returned as
// is (and, for a temporary one, joined): murmur would refuse the duplicate.
func (c *Client) CreateChannel(ctx context.Context, parent uint32, name string, temporary bool) (uint32, error) {
	if id, ok := c.FindChild(parent, name); ok {
		if temporary {
			return id, c.JoinChannel(ctx, id)
		}
		return id, nil
	}
	mark := c.denyMark()
	self := c.Session()
	if err := c.send(msgChannelState, encodeChannelCreate(parent, name, temporary)); err != nil {
		return 0, err
	}
	var id uint32
	err := c.waitFor(ctx, mark, func() bool {
		var ok bool
		id, ok = c.findChildLocked(parent, name)
		if !ok {
			return false
		}
		if !temporary {
			return true
		}
		u := c.users[self]
		return u != nil && u.Channel == id
	})
	return id, err
}

// SetSelf sets our own self-mute and self-deafen.
func (c *Client) SetSelf(mute, deaf bool) error {
	return c.send(msgUserState, encodeUserState(userStateUpdate{Session: c.Session(), SelfMute: &mute, SelfDeaf: &deaf}))
}

// SetServerMute server-mutes (or unmutes) another user. Needs MuteDeafen.
func (c *Client) SetServerMute(session uint32, mute bool) error {
	return c.send(msgUserState, encodeUserState(userStateUpdate{Session: session, Mute: &mute}))
}

// SetServerDeaf server-deafens (or undeafens) another user.
func (c *Client) SetServerDeaf(session uint32, deaf bool) error {
	return c.send(msgUserState, encodeUserState(userStateUpdate{Session: session, Deaf: &deaf}))
}

// Kick removes another user (ban also bans them). Needs Kick / Ban.
func (c *Client) Kick(session uint32, reason string, ban bool) error {
	return c.send(msgUserRemove, encodeUserRemove(session, reason, ban))
}

// SetACL replaces a channel's own ACL entries (inheriting the parent's).
// Needs Write on the channel: an operator action.
func (c *Client) SetACL(channel uint32, entries []ACLEntry) error {
	return c.send(msgACL, encodeACL(channel, entries))
}

// RemoveChannel removes a channel (and its subchannels). Needs Write: an
// operator action.
func (c *Client) RemoveChannel(id uint32) error {
	return c.send(msgChannelRemove, pbUint(nil, 1, uint64(id)))
}

// Move moves another user into a channel. Needs Move.
func (c *Client) Move(session, channel uint32) error {
	return c.send(msgUserState, encodeUserState(userStateUpdate{Session: session, Channel: &channel}))
}
