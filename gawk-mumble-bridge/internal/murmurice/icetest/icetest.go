// Package icetest is an in-process stand-in for both sides of murmur's Ice,
// for unit tests (docs/89 MI1, the shape of mumisija's icetest): a Server
// that answers requests the way murmur's Ice adapter frames them, and a
// Caller that delivers requests to a callback endpoint the way murmur does.
//
// It has its own encoder and decoder, written separately from murmurice's,
// so an encoding bug in one shows up as a mismatch instead of cancelling
// out. The real-murmur integration tests are the final word.
package icetest

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// W is a 1.1-encoding writer.
type W struct{ B []byte }

func (w *W) U8(v uint8) { w.B = append(w.B, v) }
func (w *W) Bool(v bool) {
	if v {
		w.U8(1)
	} else {
		w.U8(0)
	}
}
func (w *W) I16(v int16)   { w.B = binary.LittleEndian.AppendUint16(w.B, uint16(v)) }
func (w *W) I32(v int32)   { w.B = binary.LittleEndian.AppendUint32(w.B, uint32(v)) }
func (w *W) I64(v int64)   { w.B = binary.LittleEndian.AppendUint64(w.B, uint64(v)) }
func (w *W) F32(v float32) { w.B = binary.LittleEndian.AppendUint32(w.B, math.Float32bits(v)) }
func (w *W) Size(n int) {
	if n < 255 {
		w.U8(uint8(n))
		return
	}
	w.U8(255)
	w.I32(int32(n))
}
func (w *W) Str(s string)   { w.Size(len(s)); w.B = append(w.B, s...) }
func (w *W) Bytes(b []byte) { w.Size(len(b)); w.B = append(w.B, b...) }

// Ident writes an Identity (name, then category).
func (w *W) Ident(category, name string) { w.Str(name); w.Str(category) }

// Encaps writes a 1.1 encapsulation around p.
func (w *W) Encaps(p []byte) {
	w.I32(int32(len(p) + 6))
	w.U8(1)
	w.U8(1)
	w.B = append(w.B, p...)
}

// R is a 1.1-encoding reader; Err is sticky.
type R struct {
	B   []byte
	Off int
	Err error
}

func (r *R) take(n int) []byte {
	if r.Err != nil {
		return make([]byte, max(n, 0))
	}
	if n < 0 || r.Off+n > len(r.B) {
		r.Err = io.ErrUnexpectedEOF
		return make([]byte, max(n, 0))
	}
	b := r.B[r.Off : r.Off+n]
	r.Off += n
	return b
}
func (r *R) U8() uint8  { return r.take(1)[0] }
func (r *R) Bool() bool { return r.U8() != 0 }
func (r *R) I16() int16 { return int16(binary.LittleEndian.Uint16(r.take(2))) }
func (r *R) I32() int32 { return int32(binary.LittleEndian.Uint32(r.take(4))) }
func (r *R) Size() int {
	n := int(r.U8())
	if n == 255 {
		n = int(r.I32())
	}
	return n
}
func (r *R) Str() string { return string(r.take(r.Size())) }
func (r *R) Encaps() []byte {
	n := int(r.I32())
	r.take(2)
	return r.take(n - 6)
}

// User is a MumbleServer::User with every field murmur sends, the ones the
// bridge must drop included, so tests can check it drops them.
type User struct {
	Session, UserID                             int32
	Mute, Deaf, Suppress, Priority              bool
	SelfMute, SelfDeaf, Recording               bool
	Channel                                     int32
	Name                                        string
	OnlineSecs, BytesPerSec, Version            int32
	Version2                                    int64
	Release, OS, OSVersion, Ident, Ctx, Comment string
	Addr                                        []byte
	TCPOnly                                     bool
	IdleSecs                                    int32
	UDPPing, TCPPing                            float32
}

// WriteUser writes u in MumbleServer::User's layout.
func (w *W) WriteUser(u User) {
	w.I32(u.Session)
	w.I32(u.UserID)
	for _, b := range []bool{u.Mute, u.Deaf, u.Suppress, u.Priority, u.SelfMute, u.SelfDeaf, u.Recording} {
		w.Bool(b)
	}
	w.I32(u.Channel)
	w.Str(u.Name)
	w.I32(u.OnlineSecs)
	w.I32(u.BytesPerSec)
	w.I32(u.Version)
	w.I64(u.Version2)
	for _, s := range []string{u.Release, u.OS, u.OSVersion, u.Ident, u.Ctx, u.Comment} {
		w.Str(s)
	}
	w.Bytes(u.Addr)
	w.Bool(u.TCPOnly)
	w.I32(u.IdleSecs)
	w.F32(u.UDPPing)
	w.F32(u.TCPPing)
}

// Channel is a MumbleServer::Channel.
type Channel struct {
	ID, Parent  int32
	Name        string
	Links       []int32
	Description string
	Temporary   bool
	Position    int32
}

// WriteChannel writes c in MumbleServer::Channel's layout.
func (w *W) WriteChannel(c Channel) {
	w.I32(c.ID)
	w.Str(c.Name)
	w.I32(c.Parent)
	w.Size(len(c.Links))
	for _, l := range c.Links {
		w.I32(l)
	}
	w.Str(c.Description)
	w.Bool(c.Temporary)
	w.I32(c.Position)
}

// UserMap encodes getUsers' dictionary<int, User>.
func UserMap(users []User) []byte {
	var w W
	w.Size(len(users))
	for _, u := range users {
		w.I32(u.Session)
		w.WriteUser(u)
	}
	return w.B
}

// ChannelMap encodes getChannels' dictionary<int, Channel>.
func ChannelMap(chans []Channel) []byte {
	var w W
	w.Size(len(chans))
	for _, c := range chans {
		w.I32(c.ID)
		w.WriteChannel(c)
	}
	return w.B
}

// Exception encodes a user exception's 1.1 slices for typeID (no data
// members, so one slice: flags, type ID, and the last-slice flag set).
func Exception(typeID string) []byte {
	var w W
	w.U8(0x01 | 0x20) // type ID as a string, last slice
	w.Str(typeID)
	return w.B
}

// Proxy is a decoded ServerCallback proxy.
type Proxy struct {
	Category, Name string
	Mode           uint8
	Host           string
	Port           int32
	TimeoutMs      int32
}

// Addr is the proxy's host:port.
func (p Proxy) Addr() string { return net.JoinHostPort(p.Host, fmt.Sprint(p.Port)) }

// ReadProxy decodes a single-TCP-endpoint proxy.
func ReadProxy(params []byte) (Proxy, error) {
	r := &R{B: params}
	var p Proxy
	p.Name, p.Category = r.Str(), r.Str()
	if n := r.Size(); n != 0 {
		return p, fmt.Errorf("%d facets", n)
	}
	p.Mode = r.U8()
	r.Bool()
	r.take(4)
	if n := r.Size(); n != 1 {
		return p, fmt.Errorf("%d endpoints", n)
	}
	if t := r.I16(); t != 1 {
		return p, fmt.Errorf("endpoint type %d", t)
	}
	ep := &R{B: r.Encaps()}
	p.Host, p.Port, p.TimeoutMs = ep.Str(), ep.I32(), ep.I32()
	ep.Bool()
	if ep.Err != nil || ep.Off != len(ep.B) || r.Err != nil || r.Off != len(r.B) {
		return p, errors.New("malformed proxy")
	}
	return p, nil
}

// Request is one request the fake server received.
type Request struct {
	ID        int32
	Category  string
	Name      string
	Operation string
	Mode      uint8
	Context   map[string]string
	Params    []byte
}

// Reply is what a Handler answers: Status 0 with Result, 1 with Exception
// data, 2-4 for object/facet/operation not exist.
type Reply struct {
	Status uint8
	Result []byte
	// Raw, when set, is written instead of a well-formed reply payload
	// (after the request ID and status), for malformed-reply tests.
	Raw []byte
	// Delay holds the reply back (deadline tests); Drop never sends it.
	Delay time.Duration
	Drop  bool
}

// Handler answers one request.
type Handler func(Request) Reply

// Server is a fake Ice adapter.
type Server struct {
	ln      net.Listener
	handler Handler
	// NoValidate skips the validate-connection message (handshake tests).
	NoValidate atomic.Bool

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

// NewServer listens on 127.0.0.1 and answers with h until Close.
func NewServer(h Handler) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, handler: h, conns: map[net.Conn]struct{}{}}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// Addr is the server's host:port.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// CloseConns drops every client connection, leaving the listener up.
func (s *Server) CloseConns() {
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
}

// Close stops the server.
func (s *Server) Close() {
	s.ln.Close()
	s.CloseConns()
	s.wg.Wait()
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { s.mu.Lock(); delete(s.conns, c); s.mu.Unlock(); c.Close() }()
			s.serve(c)
		}()
	}
}

// Header builds a message header for typ and total size.
func Header(typ uint8, size int) []byte {
	b := []byte{'I', 'c', 'e', 'P', 1, 0, 1, 0, typ, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(b[10:], uint32(size))
	return b
}

// ReadMessage reads one message.
func ReadMessage(r io.Reader) (uint8, []byte, error) {
	var h [14]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	if string(h[:4]) != "IceP" {
		return 0, nil, errors.New("bad magic")
	}
	n := int(binary.LittleEndian.Uint32(h[10:]))
	body := make([]byte, n-14)
	_, err := io.ReadFull(r, body)
	return h[8], body, err
}

func (s *Server) serve(c net.Conn) {
	var wmu sync.Mutex
	write := func(b []byte) {
		wmu.Lock()
		defer wmu.Unlock()
		_, _ = c.Write(b)
	}
	if !s.NoValidate.Load() {
		write(Header(3, 14))
	}
	br := bufio.NewReader(c)
	for {
		typ, body, err := ReadMessage(br)
		if err != nil || typ == 4 {
			return
		}
		if typ != 0 {
			continue
		}
		r := &R{B: body}
		req := Request{ID: r.I32()}
		req.Name, req.Category = r.Str(), r.Str()
		for n := r.Size(); n > 0; n-- {
			r.Str()
		}
		req.Operation = r.Str()
		req.Mode = r.U8()
		req.Context = map[string]string{}
		for n := r.Size(); n > 0; n-- {
			k := r.Str()
			req.Context[k] = r.Str()
		}
		req.Params = r.Encaps()
		if r.Err != nil {
			return
		}
		rep := s.handler(req)
		if req.ID == 0 || rep.Drop {
			continue
		}
		send := func() {
			var w W
			w.I32(req.ID)
			w.U8(rep.Status)
			switch {
			case rep.Raw != nil:
				w.B = append(w.B, rep.Raw...)
			case rep.Status <= 1:
				w.Encaps(rep.Result)
			case rep.Status <= 4:
				w.Ident(req.Category, req.Name)
				w.Size(0)
				w.Str(req.Operation)
			default:
				w.Str("fake failure")
			}
			write(append(Header(2, 14+len(w.B)), w.B...))
		}
		if rep.Delay > 0 {
			go func() { time.Sleep(rep.Delay); send() }()
		} else {
			send()
		}
	}
}

// Caller delivers requests to a callback endpoint over one connection, as
// murmur does.
type Caller struct {
	c    net.Conn
	br   *bufio.Reader
	next int32
	mu   sync.Mutex
}

// Dial connects to a callback endpoint and reads its validate connection.
func Dial(addr string, timeout time.Duration) (*Caller, error) {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(timeout + time.Second))
	typ, _, err := ReadMessage(br)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil || typ != 3 {
		c.Close()
		return nil, fmt.Errorf("no validate connection: %v", err)
	}
	return &Caller{c: c, br: br}, nil
}

// Close closes the connection.
func (k *Caller) Close() error { return k.c.Close() }

// Alive reports whether the endpoint still holds the connection: a closed
// one reads EOF at once. (TCP would accept one more write after the peer
// closed; murmur's next delivery would then fail, which this models.)
func (k *Caller) Alive() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	_ = k.c.SetReadDeadline(time.Now().Add(time.Millisecond))
	defer k.c.SetReadDeadline(time.Time{})
	_, err := k.br.Peek(1)
	var ne net.Error
	return err == nil || (errors.As(err, &ne) && ne.Timeout())
}

func (k *Caller) request(id int32, category, name, op string, params []byte) error {
	var w W
	w.I32(id)
	w.Ident(category, name)
	w.Size(0)
	w.Str(op)
	w.U8(2)
	w.Size(0)
	w.Encaps(params)
	_ = k.c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := k.c.Write(append(Header(0, 14+len(w.B)), w.B...))
	return err
}

// Oneway sends op to category/name without waiting.
func (k *Caller) Oneway(category, name, op string, params []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.request(0, category, name, op, params)
}

// Twoway sends op and returns the reply's status and payload.
func (k *Caller) Twoway(category, name, op string, params []byte) (uint8, []byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.next++
	if err := k.request(k.next, category, name, op, params); err != nil {
		return 0, nil, err
	}
	_ = k.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer k.c.SetReadDeadline(time.Time{})
	for {
		typ, body, err := ReadMessage(k.br)
		if err != nil {
			return 0, nil, err
		}
		if typ != 2 {
			continue
		}
		r := &R{B: body}
		if id := r.I32(); id != k.next {
			continue
		}
		return r.U8(), body[5:], nil
	}
}

// Raw writes bytes as they are (malformed-message tests).
func (k *Caller) Raw(b []byte) error { _, err := k.c.Write(b); return err }
