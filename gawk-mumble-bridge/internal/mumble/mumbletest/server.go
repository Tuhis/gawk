// Package mumbletest is an in-process stand-in for murmur, for the unit
// tests of the Mumble client and the bridge (the integration tests run
// against real murmur, testdata/murmur.sh). It speaks the control framing,
// the messages the bridge uses and the legacy voice packet, and models the
// murmur behaviours the bridge depends on: UsernameInUse, InvalidUsername by
// a username rule, ServerFull, the autoban's silent close, temporary
// channels that vanish when empty, kicks, bans and server mutes, and voice
// relayed to the other users of the speaker's channel. It holds no audio
// beyond relaying it.
package mumbletest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"io"
	"math/big"
	"net"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble"
)

// Message types (Mumble.proto order).
const (
	tVersion          = 0
	tUDPTunnel        = 1
	tAuthenticate     = 2
	tPing             = 3
	tReject           = 4
	tServerSync       = 5
	tChannelRemove    = 6
	tChannelState     = 7
	tUserRemove       = 8
	tUserState        = 9
	tPermissionDenied = 12
	tACL              = 13
)

// Server is a fake murmur.
type Server struct {
	ln  net.Listener
	tls *tls.Config

	// UsernameRule, when set, refuses names it does not fully match with
	// InvalidUsername (murmur's `username` setting).
	UsernameRule *regexp.Regexp
	// Password is the server password; empty accepts any.
	Password string
	// MaxUsers refuses logins beyond it with ServerFull; 0 is unlimited.
	MaxUsers int
	// Refuse closes every new connection before the TLS handshake (the
	// autoban).
	Refuse atomic.Bool
	// TempToken, when set, is the access token a user needs to create a
	// temporary channel (an ACL granting #token MakeTempChannel).
	TempToken string
	// DenyEnter refuses every channel move into these channel IDs.
	DenyEnter map[uint32]bool

	mu       sync.Mutex
	nextSess uint32
	nextChan uint32
	channels map[uint32]*channel
	users    map[uint32]*user
	versions []uint64
	closed   bool
	wg       sync.WaitGroup
}

type channel struct {
	id, parent uint32
	name       string
	temp       bool
}

type user struct {
	session                         uint32
	name                            string
	channel                         uint32
	tokens                          []string
	mute, deaf, suppress, smute, sd bool
	conn                            net.Conn
	wmu                             sync.Mutex
}

// User is a snapshot of one connected user.
type User struct {
	Session  uint32
	Name     string
	Channel  uint32
	Mute     bool
	Deaf     bool
	SelfMute bool
	SelfDeaf bool
}

// Start runs a fake murmur on 127.0.0.1 until the test ends.
func Start(t testing.TB) *Server {
	t.Helper()
	cert := selfSigned(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		ln:       ln,
		tls:      &tls.Config{Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequestClientCert},
		nextSess: 1,
		nextChan: 1,
		channels: map[uint32]*channel{0: {id: 0, name: "Root"}},
		users:    map[uint32]*user{},
	}
	s.wg.Add(1)
	go s.accept()
	t.Cleanup(s.Close)
	return s
}

// Addr is the host:port to dial.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close stops the server and drops every connection.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	var conns []net.Conn
	for _, u := range s.users {
		conns = append(conns, u.conn)
	}
	s.mu.Unlock()
	s.ln.Close()
	for _, c := range conns {
		c.Close()
	}
	s.wg.Wait()
}

// AddChannel creates a permanent channel and returns its ID.
func (s *Server) AddChannel(parent uint32, name string) uint32 {
	s.mu.Lock()
	id := s.nextChan
	s.nextChan++
	ch := &channel{id: id, parent: parent, name: name}
	s.channels[id] = ch
	s.mu.Unlock()
	s.broadcast(tChannelState, channelState(ch))
	return id
}

// Channel looks a channel up by parent and name.
func (s *Server) Channel(parent uint32, name string) (uint32, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ch := range s.channels {
		if id != 0 && ch.parent == parent && ch.name == name {
			return id, true
		}
	}
	return 0, false
}

// Users snapshots the connected users by session.
func (s *Server) Users() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []User
	for _, u := range s.users {
		out = append(out, User{Session: u.session, Name: u.name, Channel: u.channel, Mute: u.mute, Deaf: u.deaf, SelfMute: u.smute, SelfDeaf: u.sd})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Session < out[j].Session })
	return out
}

// UserByName finds a connected user.
func (s *Server) UserByName(name string) (User, bool) {
	for _, u := range s.Users() {
		if u.Name == name {
			return u, true
		}
	}
	return User{}, false
}

// Versions are the version_v2 values clients reported, in login order.
func (s *Server) Versions() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint64(nil), s.versions...)
}

// Kick removes a user as an admin (session 0 is "the server") would.
func (s *Server) Kick(session uint32, reason string, ban bool) {
	b := pbUint(nil, 1, uint64(session))
	b = pbUint(b, 2, 0) // actor: present
	b = pbString(b, 3, reason)
	if ban {
		b = pbBool(b, 4, true)
	}
	s.broadcast(tUserRemove, b)
	s.mu.Lock()
	u := s.users[session]
	s.mu.Unlock()
	if u != nil {
		time.AfterFunc(20*time.Millisecond, func() { u.conn.Close() })
	}
}

// Ghost removes a user the way murmur does when the same name logs in again
// from the same address: no actor, the fixed reason.
func (s *Server) Ghost(session uint32) {
	b := pbUint(nil, 1, uint64(session))
	b = pbString(b, 3, "You connected to the server from another device")
	s.mu.Lock()
	u := s.users[session]
	s.mu.Unlock()
	if u != nil {
		s.send(u, tUserRemove, b)
		time.AfterFunc(20*time.Millisecond, func() { u.conn.Close() })
	}
}

// SetMute server-mutes a user.
func (s *Server) SetMute(session uint32, mute bool) {
	s.mu.Lock()
	u := s.users[session]
	if u != nil {
		u.mute = mute
	}
	s.mu.Unlock()
	if u != nil {
		b := pbUint(nil, 1, uint64(session))
		b = pbBool(b, 6, mute)
		s.broadcast(tUserState, b)
	}
}

// DropAll closes every connection without a word (murmur restarting).
func (s *Server) DropAll() {
	s.mu.Lock()
	var conns []net.Conn
	for _, u := range s.users {
		conns = append(conns, u.conn)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		raw, err := s.ln.Accept()
		if err != nil {
			return
		}
		if s.Refuse.Load() {
			raw.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(tls.Server(raw, s.tls))
		}()
	}
}

func readFrame(r io.Reader) (uint16, []byte, error) {
	var hdr [6]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[2:])
	if n > 1<<20 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return binary.BigEndian.Uint16(hdr[:2]), b, err
}

func frame(typ uint16, payload []byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, typ)
	b = binary.BigEndian.AppendUint32(b, uint32(len(payload)))
	return append(b, payload...)
}

func (s *Server) send(u *user, typ uint16, payload []byte) {
	u.wmu.Lock()
	defer u.wmu.Unlock()
	_ = u.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = u.conn.Write(frame(typ, payload))
}

func (s *Server) broadcast(typ uint16, payload []byte) {
	s.mu.Lock()
	us := make([]*user, 0, len(s.users))
	for _, u := range s.users {
		us = append(us, u)
	}
	s.mu.Unlock()
	for _, u := range us {
		s.send(u, typ, payload)
	}
}

func reject(conn net.Conn, typ mumble.RejectType, reason string) {
	b := pbUint(nil, 1, uint64(typ))
	b = pbString(b, 2, reason)
	_, _ = conn.Write(frame(tReject, b))
	time.Sleep(10 * time.Millisecond)
	conn.Close()
}

func (s *Server) serve(conn net.Conn) {
	defer conn.Close()
	var name, password string
	var tokens []string
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	for name == "" {
		typ, body, err := readFrame(conn)
		if err != nil {
			return
		}
		switch typ {
		case tVersion:
			for _, f := range fields(body) {
				if f.num == 5 {
					s.mu.Lock()
					s.versions = append(s.versions, f.u)
					s.mu.Unlock()
				}
			}
		case tAuthenticate:
			for _, f := range fields(body) {
				switch f.num {
				case 1:
					name = string(f.b)
				case 2:
					password = string(f.b)
				case 3:
					tokens = append(tokens, string(f.b))
				}
			}
			if name == "" {
				reject(conn, mumble.RejectInvalidUsername, "empty name")
				return
			}
		}
	}
	_ = conn.SetDeadline(time.Time{})
	if s.Password != "" && password != s.Password {
		reject(conn, mumble.RejectWrongServerPW, "Wrong server password")
		return
	}
	if s.UsernameRule != nil && !s.UsernameRule.MatchString(name) {
		reject(conn, mumble.RejectInvalidUsername, "Invalid username")
		return
	}
	s.mu.Lock()
	for _, u := range s.users {
		if strings.EqualFold(u.name, name) {
			s.mu.Unlock()
			reject(conn, mumble.RejectUsernameInUse, "Username already in use")
			return
		}
	}
	if s.MaxUsers > 0 && len(s.users) >= s.MaxUsers {
		s.mu.Unlock()
		reject(conn, mumble.RejectServerFull, "Server is full")
		return
	}
	u := &user{session: s.nextSess, name: name, tokens: tokens, conn: conn}
	s.nextSess++
	s.users[u.session] = u
	chans := make([]*channel, 0, len(s.channels))
	for _, ch := range s.channels {
		chans = append(chans, ch)
	}
	others := make([]*user, 0, len(s.users))
	for _, o := range s.users {
		others = append(others, o)
	}
	s.mu.Unlock()
	sort.Slice(chans, func(i, j int) bool { return chans[i].id < chans[j].id })
	for _, ch := range chans {
		s.send(u, tChannelState, channelState(ch))
	}
	for _, o := range others {
		if o != u {
			s.send(u, tUserState, s.fullUserState(o))
		}
	}
	s.broadcast(tUserState, s.fullUserState(u))
	s.send(u, tServerSync, pbUint(nil, 1, uint64(u.session)))

	defer s.leave(u)
	for {
		typ, body, err := readFrame(conn)
		if err != nil {
			return
		}
		switch typ {
		case tPing:
			s.send(u, tPing, body)
		case tUserState:
			s.userState(u, body)
		case tChannelState:
			s.channelCreate(u, body)
		case tUserRemove:
			var target uint32
			var ban bool
			var reason string
			for _, f := range fields(body) {
				switch f.num {
				case 1:
					target = uint32(f.u)
				case 3:
					reason = string(f.b)
				case 4:
					ban = f.u != 0
				}
			}
			b := pbUint(nil, 1, uint64(target))
			b = pbUint(b, 2, uint64(u.session))
			b = pbString(b, 3, reason)
			if ban {
				b = pbBool(b, 4, true)
			}
			s.broadcast(tUserRemove, b)
			s.mu.Lock()
			t := s.users[target]
			s.mu.Unlock()
			if t != nil {
				time.AfterFunc(20*time.Millisecond, func() { t.conn.Close() })
			}
		case tUDPTunnel:
			s.voice(u, body)
		}
	}
}

func (s *Server) fullUserState(u *user) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := pbUint(nil, 1, uint64(u.session))
	b = pbString(b, 3, u.name)
	b = pbUint(b, 5, uint64(u.channel))
	b = pbBool(b, 6, u.mute)
	b = pbBool(b, 7, u.deaf)
	b = pbBool(b, 9, u.smute)
	b = pbBool(b, 10, u.sd)
	return b
}

func channelState(ch *channel) []byte {
	b := pbUint(nil, 1, uint64(ch.id))
	if ch.id != 0 {
		b = pbUint(b, 2, uint64(ch.parent))
	}
	b = pbString(b, 3, ch.name)
	if ch.temp {
		b = pbBool(b, 8, true)
	}
	return b
}

func (s *Server) deny(u *user, typ uint64, reason string) {
	b := pbUint(nil, 5, typ)
	b = pbString(b, 4, reason)
	s.send(u, tPermissionDenied, b)
}

func (s *Server) userState(u *user, body []byte) {
	target := u.session
	var move *uint32
	var smute, sdeaf, mute *bool
	for _, f := range fields(body) {
		v := f.u != 0
		switch f.num {
		case 1:
			target = uint32(f.u)
		case 5:
			c := uint32(f.u)
			move = &c
		case 6:
			mute = &v
		case 9:
			smute = &v
		case 10:
			sdeaf = &v
		}
	}
	s.mu.Lock()
	t := s.users[target]
	if t == nil {
		s.mu.Unlock()
		return
	}
	if move != nil {
		if _, ok := s.channels[*move]; !ok || s.DenyEnter[*move] {
			s.mu.Unlock()
			s.deny(u, uint64(mumble.DenyPermission), "")
			return
		}
	}
	out := pbUint(nil, 1, uint64(target))
	if move != nil {
		old := t.channel
		t.channel = *move
		out = pbUint(out, 5, uint64(*move))
		defer s.reapTemp(old)
	}
	if mute != nil {
		t.mute = *mute
		out = pbBool(out, 6, *mute)
	}
	if smute != nil {
		t.smute = *smute
		out = pbBool(out, 9, *smute)
	}
	if sdeaf != nil {
		t.sd = *sdeaf
		if *sdeaf {
			t.smute = true
			out = pbBool(out, 9, true)
		}
		out = pbBool(out, 10, *sdeaf)
	}
	s.mu.Unlock()
	s.broadcast(tUserState, out)
}

func (s *Server) channelCreate(u *user, body []byte) {
	var parent uint32
	var name string
	var temp bool
	for _, f := range fields(body) {
		switch f.num {
		case 2:
			parent = uint32(f.u)
		case 3:
			name = string(f.b)
		case 8:
			temp = f.u != 0
		}
	}
	if temp && s.TempToken != "" {
		ok := false
		for _, t := range u.tokens {
			ok = ok || t == s.TempToken
		}
		if !ok {
			s.deny(u, uint64(mumble.DenyPermission), "")
			return
		}
	}
	s.mu.Lock()
	if _, ok := s.channels[parent]; !ok {
		s.mu.Unlock()
		s.deny(u, uint64(mumble.DenyPermission), "")
		return
	}
	for id, ch := range s.channels {
		if id != 0 && ch.parent == parent && ch.name == name {
			s.mu.Unlock()
			s.deny(u, uint64(mumble.DenyChannelName), "A channel with that name already exists")
			return
		}
	}
	ch := &channel{id: s.nextChan, parent: parent, name: name, temp: temp}
	s.nextChan++
	s.channels[ch.id] = ch
	old := u.channel
	if temp {
		u.channel = ch.id
	}
	s.mu.Unlock()
	s.broadcast(tChannelState, channelState(ch))
	if temp {
		s.broadcast(tUserState, pbUint(pbUint(nil, 1, uint64(u.session)), 5, uint64(ch.id)))
		s.reapTemp(old)
	}
}

// reapTemp removes an empty temporary channel (murmur's rule).
func (s *Server) reapTemp(id uint32) {
	s.mu.Lock()
	ch := s.channels[id]
	if ch == nil || !ch.temp {
		s.mu.Unlock()
		return
	}
	for _, u := range s.users {
		if u.channel == id {
			s.mu.Unlock()
			return
		}
	}
	delete(s.channels, id)
	s.mu.Unlock()
	s.broadcast(tChannelRemove, pbUint(nil, 1, uint64(id)))
}

func (s *Server) leave(u *user) {
	s.mu.Lock()
	_, present := s.users[u.session]
	delete(s.users, u.session)
	s.mu.Unlock()
	if present {
		s.broadcast(tUserRemove, pbUint(nil, 1, uint64(u.session)))
	}
	s.reapTemp(u.channel)
}

func (s *Server) voice(u *user, body []byte) {
	p, err := mumble.ParseClientAudio(body)
	if err != nil {
		return
	}
	s.mu.Lock()
	if u.mute || u.smute || u.suppress {
		s.mu.Unlock()
		return
	}
	p.Session = u.session
	var to []*user
	for _, o := range s.users {
		if o != u && o.channel == u.channel && !o.deaf && !o.sd {
			to = append(to, o)
		}
	}
	s.mu.Unlock()
	pkt, err := mumble.AppendServerAudio(nil, p)
	if err != nil {
		return
	}
	for _, o := range to {
		s.send(o, tUDPTunnel, pkt)
	}
}

// --- protobuf helpers ----------------------------------------------------

type field struct {
	num protowire.Number
	u   uint64
	b   []byte
}

func fields(msg []byte) []field {
	var out []field
	for len(msg) > 0 {
		num, typ, n := protowire.ConsumeTag(msg)
		if n < 0 {
			return out
		}
		msg = msg[n:]
		f := field{num: num}
		switch typ {
		case protowire.VarintType:
			f.u, n = protowire.ConsumeVarint(msg)
		case protowire.BytesType:
			f.b, n = protowire.ConsumeBytes(msg)
		default:
			n = protowire.ConsumeFieldValue(num, typ, msg)
		}
		if n < 0 {
			return out
		}
		msg = msg[n:]
		out = append(out, f)
	}
	return out
}

func pbUint(b []byte, num protowire.Number, v uint64) []byte {
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

func pbBool(b []byte, num protowire.Number, v bool) []byte {
	return pbUint(b, num, protowire.EncodeBool(v))
}

func pbString(b []byte, num protowire.Number, s string) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendString(b, s)
}

func selfSigned(t testing.TB) tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake murmur"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// CertDER is the server certificate's DER bytes (for a pin test).
func (s *Server) CertDER() []byte { return s.tls.Certificates[0].Certificate[0] }
