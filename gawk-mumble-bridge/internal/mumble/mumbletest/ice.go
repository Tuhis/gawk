package mumbletest

import (
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/murmurice/icetest"
)

// Ice is the fake murmur's Ice interface (docs/89 MI2): an icetest server
// answering getBootedServers, getVersion, getUsers, getChannels,
// addCallback and removeCallback from the fake's own state, and a callback
// caller that delivers that state's changes to every registered proxy the
// way murmur does — lazily dialled, oneway, and a registration dropped,
// silently, after one failed delivery (F13).
type Ice struct {
	s   *Server
	srv *icetest.Server

	// Secret is the write secret; ReadSecret, when set, reads but cannot
	// add a callback (F4).
	Secret     string
	ReadSecret string
	// Major, Minor, Patch are getVersion's answer; ServerIDs the booted
	// servers.
	Major, Minor, Patch int32
	ServerIDs           []int32
	// Down refuses every call with a dropped connection.
	Down atomic.Bool

	mu        sync.Mutex
	regs      map[icetest.Proxy]*icetest.Caller // nil until the first delivery
	adds      int
	removes   int
	delivered int
	failed    int
	syncMu    sync.Mutex
	lastU     map[uint32]icetest.User
	lastC     map[uint32]icetest.Channel
	paused    bool
}

// StartIce turns the fake's Ice on, with the write secret and murmur
// 1.6.1001 on server 0.
func (s *Server) StartIce(t testing.TB, secret string) *Ice {
	t.Helper()
	ic := &Ice{s: s, Secret: secret, Major: 1, Minor: 6, Patch: 1001, ServerIDs: []int32{0},
		regs: map[icetest.Proxy]*icetest.Caller{}}
	ic.lastU, ic.lastC = s.iceState()
	srv, err := icetest.NewServer(ic.handle)
	if err != nil {
		t.Fatal(err)
	}
	ic.srv = srv
	s.mu.Lock()
	s.ice = ic
	s.mu.Unlock()
	t.Cleanup(func() {
		srv.Close()
		ic.mu.Lock()
		for _, c := range ic.regs {
			if c != nil {
				c.Close()
			}
		}
		ic.mu.Unlock()
	})
	return ic
}

// Addr is the Ice endpoint's host:port.
func (ic *Ice) Addr() string { return ic.srv.Addr() }

// CloseConns drops every Ice client connection (a murmur restart, as the
// client sees it).
func (ic *Ice) CloseConns() { ic.srv.CloseConns() }

// Restart is a murmur restart: every registration and connection is lost.
func (ic *Ice) Restart() {
	ic.mu.Lock()
	for p, c := range ic.regs {
		if c != nil {
			c.Close()
		}
		delete(ic.regs, p)
	}
	ic.mu.Unlock()
	ic.srv.CloseConns()
}

// DropRegistrations forgets every registration without telling anyone, as
// murmur does after a refused delivery (F13).
func (ic *Ice) DropRegistrations() {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	for p, c := range ic.regs {
		if c != nil {
			c.Close()
		}
		delete(ic.regs, p)
	}
}

// Registrations is how many proxies are registered now.
func (ic *Ice) Registrations() int {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	return len(ic.regs)
}

// Calls is how many addCallback and removeCallback calls arrived.
func (ic *Ice) Calls() (adds, removes int) {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	return ic.adds, ic.removes
}

// Delivered is how many events reached an endpoint.
func (ic *Ice) Delivered() int {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	return ic.delivered
}

// Pause holds deliveries back (state changes still happen); Resume sends
// what changed meanwhile.
func (ic *Ice) Pause() {
	ic.syncMu.Lock()
	ic.paused = true
	ic.syncMu.Unlock()
}

func (ic *Ice) Resume() {
	ic.syncMu.Lock()
	ic.paused = false
	ic.syncMu.Unlock()
	ic.sync()
}

func (ic *Ice) handle(r icetest.Request) icetest.Reply {
	if ic.Down.Load() {
		go ic.srv.CloseConns()
		return icetest.Reply{Drop: true}
	}
	secret := r.Context["secret"]
	write := secret == ic.Secret
	if !write && (ic.ReadSecret == "" || secret != ic.ReadSecret) {
		return icetest.Reply{Status: 1, Result: icetest.Exception("::MumbleServer::InvalidSecretException")}
	}
	if r.Category == "s" {
		known := false
		for _, id := range ic.ServerIDs {
			if strconv.Itoa(int(id)) == r.Name {
				known = true
			}
		}
		if !known {
			return icetest.Reply{Status: 2}
		}
	}
	var w icetest.W
	switch r.Operation {
	case "getBootedServers":
		w.Size(len(ic.ServerIDs))
		for _, id := range ic.ServerIDs {
			w.Ident("s", strconv.Itoa(int(id)))
			w.Size(0)
			w.U8(0)
			w.Bool(false)
			w.B = append(w.B, 1, 0, 1, 1)
			w.Size(1)
			w.I16(1)
			var ep icetest.W
			ep.Str("127.0.0.1")
			ep.I32(6502)
			ep.I32(-1)
			ep.Bool(false)
			w.Encaps(ep.B)
		}
	case "getVersion":
		w.I32(ic.Major)
		w.I32(ic.Minor)
		w.I32(ic.Patch)
		w.Str("fake")
	case "getUsers":
		u, _ := ic.s.iceState()
		var list []icetest.User
		for _, x := range u {
			list = append(list, x)
		}
		w.B = icetest.UserMap(list)
	case "getChannels":
		_, c := ic.s.iceState()
		var list []icetest.Channel
		for _, x := range c {
			list = append(list, x)
		}
		w.B = icetest.ChannelMap(list)
	case "addCallback", "removeCallback":
		if !write {
			return icetest.Reply{Status: 1, Result: icetest.Exception("::MumbleServer::InvalidSecretException")}
		}
		p, err := icetest.ReadProxy(r.Params)
		if err != nil {
			return icetest.Reply{Status: 1, Result: icetest.Exception("::MumbleServer::InvalidCallbackException")}
		}
		ic.mu.Lock()
		if r.Operation == "addCallback" {
			ic.adds++
			if _, ok := ic.regs[p]; !ok {
				ic.regs[p] = nil
			}
		} else {
			ic.removes++
			if c := ic.regs[p]; c != nil {
				c.Close()
			}
			delete(ic.regs, p)
		}
		ic.mu.Unlock()
	default:
		return icetest.Reply{Status: 4}
	}
	return icetest.Reply{Result: w.B}
}

// iceState converts the fake's state to Ice's types.
func (s *Server) iceState() (map[uint32]icetest.User, map[uint32]icetest.Channel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	users := make(map[uint32]icetest.User, len(s.users))
	for _, u := range s.users {
		users[u.session] = icetest.User{Session: int32(u.session), UserID: -1, Name: u.name, Channel: int32(u.channel),
			Mute: u.mute, Deaf: u.deaf, Suppress: u.suppress, SelfMute: u.smute, SelfDeaf: u.sd,
			Release: "fake", Comment: "a comment", Ident: "an identity", Ctx: "a context", Addr: []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 192, 0, 2, 33}}
	}
	chans := make(map[uint32]icetest.Channel, len(s.channels))
	for _, c := range s.channels {
		parent := int32(c.parent)
		if c.id == 0 {
			parent = -1
		}
		chans[c.id] = icetest.Channel{ID: int32(c.id), Parent: parent, Name: c.name, Temporary: c.temp}
	}
	return users, chans
}

// sync delivers what changed since the last delivery: new channels, then
// users, then removed channels, the order murmur's events take.
func (ic *Ice) sync() {
	ic.syncMu.Lock()
	defer ic.syncMu.Unlock()
	if ic.paused {
		return
	}
	users, chans := ic.s.iceState()
	type ev struct {
		op     string
		params []byte
	}
	var evs []ev
	chanEv := func(op string, c icetest.Channel) {
		var w icetest.W
		w.WriteChannel(c)
		evs = append(evs, ev{op, w.B})
	}
	userEv := func(op string, u icetest.User) {
		var w icetest.W
		w.WriteUser(u)
		evs = append(evs, ev{op, w.B})
	}
	for id, c := range chans {
		if old, ok := ic.lastC[id]; !ok {
			chanEv("channelCreated", c)
		} else if old.Name != c.Name || old.Parent != c.Parent || old.Temporary != c.Temporary {
			chanEv("channelStateChanged", c)
		}
	}
	for id, u := range users {
		if old, ok := ic.lastU[id]; !ok {
			userEv("userConnected", u)
		} else if old.Name != u.Name || old.Channel != u.Channel || old.Mute != u.Mute || old.Deaf != u.Deaf ||
			old.Suppress != u.Suppress || old.SelfMute != u.SelfMute || old.SelfDeaf != u.SelfDeaf {
			userEv("userStateChanged", u)
		}
	}
	for id, u := range ic.lastU {
		if _, ok := users[id]; !ok {
			userEv("userDisconnected", u)
		}
	}
	for id, c := range ic.lastC {
		if _, ok := chans[id]; !ok {
			chanEv("channelRemoved", c)
		}
	}
	ic.lastU, ic.lastC = users, chans
	for _, e := range evs {
		ic.deliver(e.op, e.params)
	}
}

// deliver sends one event to every registration, dropping a registration
// whose delivery fails (F13).
func (ic *Ice) deliver(op string, params []byte) {
	ic.mu.Lock()
	regs := make(map[icetest.Proxy]*icetest.Caller, len(ic.regs))
	for p, c := range ic.regs {
		regs[p] = c
	}
	ic.mu.Unlock()
	for p, c := range regs {
		var err error
		if c != nil && !c.Alive() {
			err = errors.New("the endpoint closed the connection")
		}
		if c == nil {
			timeout := time.Duration(p.TimeoutMs) * time.Millisecond
			if timeout <= 0 {
				timeout = time.Second
			}
			c, err = icetest.Dial(p.Addr(), timeout)
		}
		if err == nil {
			err = c.Oneway(p.Category, p.Name, op, params)
		}
		if err != nil {
			// What murmur's log says, for the tests reading it.
			ic.mu.Lock()
			ic.failed++
			ic.mu.Unlock()
		}
		ic.mu.Lock()
		if _, still := ic.regs[p]; still {
			if err != nil {
				if c != nil {
					c.Close()
				}
				delete(ic.regs, p)
			} else {
				ic.regs[p] = c
				ic.delivered++
			}
		} else if c != nil {
			c.Close()
		}
		ic.mu.Unlock()
	}
}
