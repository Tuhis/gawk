package murmurice

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"net"
	"sync"
	"time"
)

// ServerCallbackType is the Slice type ID the endpoint's object implements.
const ServerCallbackType = "::MumbleServer::ServerCallback"

// NewIdentity is a fresh callback identity, gawk-bridge-<128 random bits>
// (D14): new for each process, so a reused pod IP never inherits a dead
// process's events, and hard to guess for anyone who would forge them.
func NewIdentity() Identity {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return Identity{Name: "gawk-bridge-" + hex.EncodeToString(b)}
}

// Handler receives what murmur sends the endpoint: each decoded event, or
// the error a malformed one produced (err set, ev zero).
type Handler func(ev Event, err error)

// Endpoint is the callback endpoint murmur dials (D14): it accepts TCP
// connections, sends validate connection, decodes requests and dispatches
// the seven ServerCallback operations for its one identity to the handler.
// ice_ping and ice_isA are answered; any other operation is "operation does
// not exist" and any other identity "object does not exist". No batch
// requests, compression or TLS.
type Endpoint struct {
	ln      net.Listener
	id      Identity
	handler Handler

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// Listen binds addr and serves id until Close.
func Listen(addr string, id Identity, h Handler) (*Endpoint, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	e := &Endpoint{ln: ln, id: id, handler: h, conns: map[net.Conn]struct{}{}}
	e.wg.Add(1)
	go e.accept()
	return e, nil
}

// Addr is the bound address.
func (e *Endpoint) Addr() *net.TCPAddr { return e.ln.Addr().(*net.TCPAddr) }

// Identity is the identity the endpoint serves.
func (e *Endpoint) Identity() Identity { return e.id }

// Close stops accepting and closes every connection murmur holds.
func (e *Endpoint) Close() error {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	err := e.ln.Close()
	e.CloseConns()
	e.wg.Wait()
	return err
}

// CloseConns closes the connections murmur holds to the endpoint, leaving
// the listener up: murmur's next event dials a new one.
func (e *Endpoint) CloseConns() {
	e.mu.Lock()
	for nc := range e.conns {
		nc.Close()
	}
	e.mu.Unlock()
}

func (e *Endpoint) accept() {
	defer e.wg.Done()
	for {
		nc, err := e.ln.Accept()
		if err != nil {
			return
		}
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			nc.Close()
			return
		}
		e.conns[nc] = struct{}{}
		e.wg.Add(1)
		e.mu.Unlock()
		go func() {
			defer e.wg.Done()
			e.serve(nc)
			e.mu.Lock()
			delete(e.conns, nc)
			e.mu.Unlock()
		}()
	}
}

func (e *Endpoint) serve(nc net.Conn) {
	defer nc.Close()
	var wmu sync.Mutex
	write := func(b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		_ = nc.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, err := nc.Write(b)
		return err
	}
	if write(control(msgValidate)) != nil {
		return
	}
	br := bufio.NewReader(nc)
	for {
		typ, body, err := readMessage(br)
		if err != nil {
			if pe, ok := err.(*ProtocolError); ok {
				e.handler(Event{}, pe)
			}
			return
		}
		switch typ {
		case msgClose:
			return
		case msgValidate:
			continue
		case msgRequest:
		default: // a batch request, or a reply nobody asked for
			e.handler(Event{}, protoErr("unexpected message on the callback endpoint", nil))
			return
		}
		in, err := decodeIncoming(body)
		if err != nil {
			e.handler(Event{}, err)
			return
		}
		status, result := e.dispatch(in)
		if in.id != 0 {
			var out []byte
			if status == statusUnknownLocalException {
				ue := encoder{b: appendHeader(nil, msgReply)}
				ue.i32(in.id)
				ue.u8(status)
				ue.str("cannot unmarshal the parameters")
				out = finish(ue.b)
			} else {
				out = encodeReply(in, status, result)
			}
			if write(out) != nil {
				return
			}
		}
	}
}

func (e *Endpoint) dispatch(in incoming) (uint8, []byte) {
	if in.target != e.id {
		return statusObjectNotExist, nil
	}
	if len(in.facet) > 0 && in.facet[0] != "" {
		return statusFacetNotExist, nil
	}
	switch in.operation {
	case "ice_ping":
		return statusOK, nil
	case "ice_isA":
		d := newDecoder(in.params)
		typeID := d.str()
		if d.end() != nil {
			return statusUnknownLocalException, nil
		}
		var en encoder
		en.boolean(typeID == ServerCallbackType || typeID == "::Ice::Object")
		return statusOK, en.b
	case OpUserConnected, OpUserDisconnected, OpUserStateChanged, OpUserTextMessage,
		OpChannelCreated, OpChannelRemoved, OpChannelStateChanged:
		ev, err := decodeEvent(in.operation, in.params)
		if err != nil {
			e.handler(Event{}, err)
			return statusUnknownLocalException, nil
		}
		e.handler(ev, nil)
		return statusOK, nil
	}
	return statusOperationNotExist, nil
}
