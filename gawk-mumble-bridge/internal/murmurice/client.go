package murmurice

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Client is a twoway Ice client to murmur's Ice endpoint over one TCP
// connection, with requests multiplexed by ID (D7). It dials lazily, redials
// when the connection is gone, and retries an idempotent call once on a new
// connection when the old one broke under it. Every call has a deadline.
//
// Generation counts connections: it changes whenever the client dials a new
// one, which is how the bridge sees a reconnect (D11).
type Client struct {
	addr    string
	secret  string
	timeout time.Duration

	mu     sync.Mutex
	cur    *conn
	gen    uint64
	closed bool
}

// NewClient returns a client for murmur's Ice at addr (host:port, TCP).
// secret is sent as the "secret" request context on every call, unless
// empty. timeout bounds each call and each dial.
func NewClient(addr, secret string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &Client{addr: addr, secret: secret, timeout: timeout}
}

// Generation is the number of connections dialled so far.
func (c *Client) Generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// Connect makes sure a connection is up and returns its generation.
func (c *Client) Connect(ctx context.Context) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cn, err := c.conn(ctx)
	if err != nil {
		return 0, err
	}
	return cn.gen, nil
}

// Lost is closed when the current connection ends. With no connection it is
// already closed.
func (c *Client) Lost() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return c.cur.done
}

// Reset drops the current connection; the next call dials a new one.
func (c *Client) Reset() {
	c.mu.Lock()
	cn := c.cur
	c.cur = nil
	c.mu.Unlock()
	if cn != nil {
		cn.close(ErrConnectionLost)
	}
}

// Close ends the client and its connection.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	cn := c.cur
	c.cur = nil
	c.mu.Unlock()
	if cn != nil {
		cn.wmu.Lock()
		_ = cn.nc.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		_, _ = cn.nc.Write(control(msgClose))
		cn.wmu.Unlock()
		cn.close(ErrClosed)
	}
	return nil
}

func (c *Client) conn(ctx context.Context) (*conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if c.cur != nil && !c.cur.isDone() {
		return c.cur, nil
	}
	c.cur = nil
	cn, err := dialConn(ctx, c.addr)
	if err != nil {
		return nil, err
	}
	c.gen++
	cn.gen = c.gen
	c.cur = cn
	return cn, nil
}

// drop forgets cn if it is still the current connection.
func (c *Client) drop(cn *conn) {
	c.mu.Lock()
	if c.cur == cn {
		c.cur = nil
	}
	c.mu.Unlock()
}

// invoke runs one call. An idempotent call that loses its connection is
// retried once on a fresh one.
func (c *Client) invoke(ctx context.Context, target Identity, op string, mode uint8, params []byte) ([]byte, error) {
	req := request{target: target, operation: op, mode: mode, params: params}
	if c.secret != "" {
		req.reqCtx = map[string]string{"secret": c.secret}
	}
	for attempt := 0; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, c.timeout)
		cn, err := c.conn(actx)
		if err != nil {
			cancel()
			return nil, err
		}
		out, err := cn.call(actx, req)
		cancel()
		if errors.Is(err, ErrConnectionLost) {
			c.drop(cn)
			if mode == modeIdempotent && attempt == 0 && ctx.Err() == nil {
				continue
			}
		}
		return out, err
	}
}

// conn is one TCP connection to murmur's Ice.
type conn struct {
	nc  net.Conn
	gen uint64

	wmu sync.Mutex

	mu      sync.Mutex
	next    int32
	pending map[int32]chan reply
	done    chan struct{}
	err     error
}

type reply struct {
	status  uint8
	payload []byte
	err     error
}

// DialError is a failure to reach murmur's Ice or to complete its
// validate-connection handshake.
type DialError struct{ Err error }

func (e *DialError) Error() string { return "murmurice: dial: " + e.Err.Error() }
func (e *DialError) Unwrap() error { return e.Err }

func dialConn(ctx context.Context, addr string) (*conn, error) {
	var nd net.Dialer
	nc, err := nd.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, &DialError{Err: err}
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = nc.SetReadDeadline(dl)
	}
	br := bufio.NewReader(nc)
	typ, _, err := readMessage(br)
	if err != nil {
		nc.Close()
		var pe *ProtocolError
		if errors.As(err, &pe) {
			return nil, err
		}
		return nil, &DialError{Err: fmt.Errorf("validate connection: %w", err)}
	}
	if typ != msgValidate {
		nc.Close()
		return nil, protoErr(fmt.Sprintf("first message type %d, want validate connection", typ), nil)
	}
	_ = nc.SetReadDeadline(time.Time{})
	cn := &conn{nc: nc, pending: map[int32]chan reply{}, done: make(chan struct{})}
	go cn.readLoop(br)
	return cn, nil
}

func (cn *conn) isDone() bool {
	select {
	case <-cn.done:
		return true
	default:
		return false
	}
}

// close ends the connection and fails every pending call with err.
func (cn *conn) close(err error) {
	cn.mu.Lock()
	if cn.err != nil {
		cn.mu.Unlock()
		return
	}
	cn.err = err
	pending := cn.pending
	cn.pending = nil
	close(cn.done)
	cn.mu.Unlock()
	cn.nc.Close()
	for _, ch := range pending {
		ch <- reply{err: ErrConnectionLost}
	}
}

func (cn *conn) readLoop(br *bufio.Reader) {
	for {
		typ, body, err := readMessage(br)
		if err != nil {
			cn.close(ErrConnectionLost)
			return
		}
		switch typ {
		case msgReply:
			if len(body) < 5 {
				cn.close(ErrConnectionLost)
				return
			}
			d := newDecoder(body)
			id := d.i32()
			status := d.u8()
			cn.mu.Lock()
			ch := cn.pending[id]
			delete(cn.pending, id)
			cn.mu.Unlock()
			if ch != nil {
				ch <- reply{status: status, payload: body[5:]}
			}
		case msgClose:
			cn.close(ErrConnectionLost)
			return
		case msgValidate:
			// harmless repeat
		default:
			// murmur never calls a client's objects over this connection,
			// and a client has none: refuse it as Ice would.
			if typ == msgRequest {
				if in, err := decodeIncoming(body); err == nil && in.id != 0 {
					cn.write(encodeReply(in, statusObjectNotExist, nil))
				}
			}
		}
	}
}

func (cn *conn) write(b []byte) error {
	cn.wmu.Lock()
	defer cn.wmu.Unlock()
	_ = cn.nc.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := cn.nc.Write(b)
	return err
}

func (cn *conn) call(ctx context.Context, req request) ([]byte, error) {
	ch := make(chan reply, 1)
	cn.mu.Lock()
	if cn.pending == nil {
		cn.mu.Unlock()
		return nil, ErrConnectionLost
	}
	cn.next++
	if cn.next <= 0 { // 0 is oneway; wrap past it
		cn.next = 1
	}
	id := cn.next
	cn.pending[id] = ch
	cn.mu.Unlock()
	if err := cn.write(encodeRequest(id, req)); err != nil {
		cn.close(ErrConnectionLost)
		return nil, ErrConnectionLost
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		return decodeReply(r.status, r.payload)
	case <-ctx.Done():
		cn.mu.Lock()
		if cn.pending != nil {
			delete(cn.pending, id)
		}
		cn.mu.Unlock()
		return nil, ctx.Err()
	}
}

// metaID is murmur's Meta object.
var metaID = Identity{Name: "Meta"}

// serverID is the virtual server sid's object, "s/<sid>".
func serverID(sid int) Identity { return Identity{Category: "s", Name: strconv.Itoa(sid)} }

// BootedServers is Meta.getBootedServers: the IDs of the running virtual
// servers, sorted. 1.6 numbers its default server 0, 1.5 numbers it 1 (F8).
func (c *Client) BootedServers(ctx context.Context) ([]int, error) {
	out, err := c.invoke(ctx, metaID, "getBootedServers", modeIdempotent, nil)
	if err != nil {
		return nil, err
	}
	d := newDecoder(out)
	var ids []int
	for n := d.size(); n > 0 && d.err() == nil; n-- {
		id := d.identity()
		// The proxy's facet, mode, secure flag, versions and endpoints.
		d.proxyRest()
		if d.err() != nil {
			break
		}
		if id.Category != "s" {
			d.fail(fmt.Errorf("server proxy %s", id))
			break
		}
		sid, err := strconv.Atoi(id.Name)
		if err != nil {
			d.fail(fmt.Errorf("server proxy %s", id))
			break
		}
		ids = append(ids, sid)
	}
	if err := d.end(); err != nil {
		return nil, protoErr("getBootedServers", err)
	}
	sort.Ints(ids)
	return ids, nil
}

// proxyRest reads what follows a non-null proxy's identity in the 1.1
// encoding: facet, mode, secure, protocol and encoding versions, then the
// endpoints (each a type and an encapsulation) or an adapter ID.
func (d *decoder) proxyRest() {
	for n := d.size(); n > 0 && d.err() == nil; n-- {
		d.skipStr() // facet
	}
	d.u8()        // mode
	d.boolean()   // secure
	d.next(4)     // protocol and encoding versions
	n := d.size() // endpoints
	if n == 0 {
		d.skipStr() // adapter ID
		return
	}
	for ; n > 0 && d.err() == nil; n-- {
		d.i16()
		d.encaps()
	}
}

// Version is Meta.getVersion (F17).
func (c *Client) Version(ctx context.Context) (Version, error) {
	out, err := c.invoke(ctx, metaID, "getVersion", modeIdempotent, nil)
	if err != nil {
		return Version{}, err
	}
	d := newDecoder(out)
	v := Version{Major: int(d.i32()), Minor: int(d.i32()), Patch: int(d.i32())}
	d.skipStr() // text
	if err := d.end(); err != nil {
		return Version{}, protoErr("getVersion", err)
	}
	return v, nil
}

// Users is Server.getUsers, by session.
func (c *Client) Users(ctx context.Context, sid int) ([]User, error) {
	out, err := c.invoke(ctx, serverID(sid), "getUsers", modeIdempotent, nil)
	if err != nil {
		return nil, err
	}
	d := newDecoder(out)
	var users []User
	for n := d.size(); n > 0 && d.err() == nil; n-- {
		key := d.i32()
		u := d.user()
		if d.err() == nil && uint32(key) != u.Session {
			d.fail(fmt.Errorf("user key %d for session %d", key, u.Session))
		}
		users = append(users, u)
	}
	if err := d.end(); err != nil {
		return nil, protoErr("getUsers", err)
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Session < users[j].Session })
	return users, nil
}

// Channels is Server.getChannels, by ID.
func (c *Client) Channels(ctx context.Context, sid int) ([]Channel, error) {
	out, err := c.invoke(ctx, serverID(sid), "getChannels", modeIdempotent, nil)
	if err != nil {
		return nil, err
	}
	d := newDecoder(out)
	var chans []Channel
	for n := d.size(); n > 0 && d.err() == nil; n-- {
		key := d.i32()
		ch := d.channel()
		if d.err() == nil && uint32(key) != ch.ID {
			d.fail(fmt.Errorf("channel key %d for ID %d", key, ch.ID))
		}
		chans = append(chans, ch)
	}
	if err := d.end(); err != nil {
		return nil, protoErr("getChannels", err)
	}
	sort.Slice(chans, func(i, j int) bool { return chans[i].ID < chans[j].ID })
	return chans, nil
}

// AddCallback is Server.addCallback. It needs the write secret (F4).
func (c *Client) AddCallback(ctx context.Context, sid int, p Proxy) error {
	_, err := c.invoke(ctx, serverID(sid), "addCallback", modeNormal, p.encode())
	return err
}

// RemoveCallback is Server.removeCallback. InvalidCallbackException means
// murmur no longer has it.
func (c *Client) RemoveCallback(ctx context.Context, sid int, p Proxy) error {
	_, err := c.invoke(ctx, serverID(sid), "removeCallback", modeNormal, p.encode())
	return err
}

// Proxy is a ServerCallback proxy: an identity at one TCP endpoint.
type Proxy struct {
	Object Identity
	Host   string
	Port   int
	// Timeout is the endpoint timeout murmur uses to reach it, which bounds
	// how long a blackholed endpoint freezes murmur (F15, D14).
	Timeout time.Duration
}

// encode writes the proxy in the 1.1 encoding:
// <identity> -o:tcp -h <host> -p <port> -t <ms>.
func (p Proxy) encode() []byte {
	var e encoder
	e.identity(p.Object)
	e.size(0)                     // facet
	e.u8(1)                       // oneway: murmur makes it oneway anyway (F12)
	e.boolean(false)              // secure
	e.b = append(e.b, 1, 0, 1, 1) // protocol 1.0, encoding 1.1
	e.size(1)
	e.i16(1) // TCPEndpointType
	var ep encoder
	ep.str(p.Host)
	ep.i32(int32(p.Port))
	ep.i32(int32(p.Timeout / time.Millisecond))
	ep.boolean(false) // compress
	e.encaps(ep.b)
	return e.b
}
