package transport

// R79 MV3 (docs/80 §9) over real QUIC: the observer link to a fake bridge
// (dial, Observe, Roster, redial after a drop), the session link route
// (its gates, Register and every refusal, Joined into the roster, the ends
// of a voice session sent as the home's Close), the kill and ban paths, the
// ?voice=1 mint, and the off-path — with voice unconfigured, none of it is
// reachable.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/moderation"
	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/voice"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

const (
	testVoicePSK       = "link-psk"
	testVoiceSecret    = "0123456789abcdef0123"
	testVoicePublicURL = "https://voice.example/voice"
)

// fakeBridge is gawk-mumble-bridge's internal listener as the relay sees
// it: CONNECT /internal/observe?psk=, one stream, an Observe, then whatever
// Roster messages the test writes.
type fakeBridge struct {
	addr   string
	pool   *x509.CertPool
	links  chan *bridgeObserver
	status atomic.Int32
}

type bridgeObserver struct {
	sess    *webtransport.Session
	stream  *webtransport.Stream
	observe voice.Message
	origin  string
}

func (o *bridgeObserver) roster(t *testing.T, m voice.Message) {
	t.Helper()
	m.Type = voice.TypeRoster
	if err := voice.WriteMessage(o.stream, m); err != nil {
		t.Fatalf("write roster: %v", err)
	}
}

func startFakeBridge(t *testing.T) *fakeBridge {
	t.Helper()
	cert, err := tlsutil.GenerateDevCert([]string{"localhost", "127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBridge{addr: pc.LocalAddr().String(), pool: x509.NewCertPool(), links: make(chan *bridgeObserver, 16)}
	b.pool.AddCert(cert.Leaf)
	mux := http.NewServeMux()
	wt := &webtransport.Server{
		H3: &http3.Server{
			TLSConfig:       http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}}),
			Handler:         mux,
			EnableDatagrams: true,
			QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
		},
		CheckOrigin: func(*http.Request) bool { return true },
	}
	webtransport.ConfigureHTTP3Server(wt.H3)
	mux.HandleFunc("CONNECT "+voice.ObservePath, func(w http.ResponseWriter, r *http.Request) {
		if st := b.status.Load(); st != 0 {
			w.WriteHeader(int(st))
			return
		}
		if r.URL.Query().Get("psk") != testVoicePSK {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sess, err := wt.Upgrade(w, r)
		if err != nil {
			return
		}
		stream, err := sess.AcceptStream(r.Context())
		if err != nil {
			return
		}
		m, err := voice.ReadMessage(stream)
		if err != nil {
			return
		}
		b.links <- &bridgeObserver{sess: sess, stream: stream, observe: m, origin: r.Header.Get("Origin")}
		<-sess.Context().Done()
	})
	go func() { _ = wt.Serve(pc) }()
	t.Cleanup(func() { _ = wt.Close(); _ = pc.Close() })
	return b
}

func (b *fakeBridge) nextLink(t *testing.T) *bridgeObserver {
	t.Helper()
	select {
	case l := <-b.links:
		return l
	case <-time.After(10 * time.Second):
		t.Fatal("the relay opened no observer link")
	}
	return nil
}

func voiceConfig(addr string) config.Config {
	return config.Config{MaxSubscribers: 5, MaxIdleTimeout: 30 * time.Second, KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace: 5 * time.Minute, Rooms: true,
		VoiceBridgeAddr: addr, VoiceBridgePublicURL: testVoicePublicURL, VoiceBridgePSK: testVoicePSK,
		VoiceTicketSecret: testVoiceSecret, VoiceMaxRooms: 5, VoiceMaxUsersPerRoom: 5}
}

func fastVoiceManager(srv *Server) {
	hook := func(m *voiceManager) {
		m.backoffBase, m.backoffMax, m.keeperEvery = 20*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond
	}
	srv.testHookVoiceManager.Store(&hook)
}

// startVoiceRoomServer is startRoomServer with voice configured against b.
func startVoiceRoomServer(t *testing.T, ctx context.Context, b *fakeBridge) (int, *tls.Config, *hub.Registry, *Server, *roomsrv.Registry) {
	t.Helper()
	port, clientTLS, r, _, srv := startTestServerCfgLogSrv(t, ctx, voiceConfig(b.addr), discardLog)
	cfg := srv.cfg
	reg := roomsrv.NewRegistry(roomsrv.Options{Broadcasts: srv.RoomBroadcasts(), Obfuscate: r.ObfuscateID, Log: discardLog, EmptyGrace: time.Hour,
		Voice: roomsrv.VoiceOptions{Enabled: true, PublicURL: cfg.VoiceBridgePublicURL, TicketSecret: []byte(cfg.VoiceTicketSecret),
			MaxRooms: cfg.VoiceMaxRooms, MaxUsersPerRoom: cfg.VoiceMaxUsersPerRoom},
		OnVoiceChanged: srv.VoiceChanged})
	srv.SetRooms(reg)
	fastVoiceManager(srv)
	srv.StartVoice(ctx, VoiceBridgeOptions{Addr: b.addr, ServerName: "localhost", RootCAs: b.pool})
	return port, clientTLS, r, srv, reg
}

// openVoiceControl is openControl with RoomCapVoice in wantCaps.
func openVoiceControl(t *testing.T, ctx context.Context, url string, clientTLS *tls.Config, nick string) *roomClient {
	t.Helper()
	sess := dial(t, ctx, url, clientTLS)
	stream, err := sess.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := &roomClient{sess: sess, stream: stream}
	msg, err := wire.AppendRoomHello(nil, wire.RoomHello{Protocol: wire.RoomProtocolVersion, ClientKind: wire.RoomClientWebViewer,
		WantCaps: wire.RoomCapVoice, Nickname: nick})
	if err != nil {
		t.Fatal(err)
	}
	c.send(t, msg)
	return c
}

// nextVoice reads past snapshots and other events to the next event of kind.
func (c *roomClient) nextVoice(t *testing.T, kind uint8) wire.RoomEvent {
	t.Helper()
	for {
		rec := c.next(t)
		if rec[1] != wire.TypeRoomEvent {
			continue
		}
		e, err := wire.ParseRoomEvent(rec)
		if err != nil {
			t.Fatalf("ParseRoomEvent: %v", err)
		}
		if e.Kind == kind {
			return e
		}
	}
}

// voiceLinkClient is a bridge pod's session link to the relay.
type voiceLinkClient struct {
	sess   *webtransport.Session
	stream *webtransport.Stream
}

func dialVoiceLink(t *testing.T, ctx context.Context, url string, clientTLS *tls.Config) *voiceLinkClient {
	t.Helper()
	sess := dialWithOrigin(t, ctx, url, clientTLS, voice.LinkOrigin)
	stream, err := sess.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return &voiceLinkClient{sess: sess, stream: stream}
}

func (l *voiceLinkClient) send(t *testing.T, m voice.Message) {
	t.Helper()
	if err := voice.WriteMessage(l.stream, m); err != nil {
		t.Fatalf("link write: %v", err)
	}
}

func (l *voiceLinkClient) read(t *testing.T) voice.Message {
	t.Helper()
	_ = l.stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	m, err := voice.ReadMessage(l.stream)
	if err != nil {
		t.Fatalf("link read: %v", err)
	}
	return m
}

// expectClose reads the home's Close and checks its code.
func (l *voiceLinkClient) expectClose(t *testing.T, code uint32) voice.Message {
	t.Helper()
	m := l.read(t)
	if m.Type != voice.TypeClose || m.CloseCode != code {
		t.Fatalf("link got %+v, want Close %d", m, code)
	}
	return m
}

func register(t *testing.T, ctx context.Context, url string, clientTLS *tls.Config, ticket []byte, remote string) (*voiceLinkClient, voice.Message) {
	t.Helper()
	l := dialVoiceLink(t, ctx, url, clientTLS)
	l.send(t, voice.Message{Type: voice.TypeRegister, Ticket: ticket, Remote: remote})
	return l, l.read(t)
}

func voiceTicket(t *testing.T, c *roomClient) []byte {
	t.Helper()
	c.command(t, wire.RoomCommand{Kind: wire.RoomCommandVoiceJoin})
	e := c.nextVoice(t, wire.RoomEventVoiceTicket)
	if e.VoiceURL != testVoicePublicURL {
		t.Fatalf("ticket url %q", e.VoiceURL)
	}
	return append([]byte(nil), e.VoiceTicket...)
}

// voiceRoom upserts the static voice room "friends" and waits for its
// observer link, answering it with one native user.
func voiceRoom(t *testing.T, b *fakeBridge, reg *roomsrv.Registry) *bridgeObserver {
	t.Helper()
	if err := reg.UpsertStatic(roomsrv.StaticRoom{Code: "friends", DisplayCode: "Friends", Mumble: &rooms.MumbleIntegration{Channel: "Friends/Gaming"}}); err != nil {
		t.Fatal(err)
	}
	obs := b.nextLink(t)
	obs.roster(t, voice.Message{Available: true, Users: []voice.User{{Session: 3, Name: "native"}, {Session: 7, Name: "alice [web]"}}})
	waitFor(t, 5*time.Second, func() bool { return voiceStats(reg).VoiceObserver == "available" }, "observer available")
	return obs
}

func TestVoiceLinkOriginIsTheInternalEdgeOrigin(t *testing.T) {
	if voice.LinkOrigin != internalEdgeOrigin {
		t.Fatalf("voice.LinkOrigin %q != internalEdgeOrigin %q", voice.LinkOrigin, internalEdgeOrigin)
	}
}

func TestObserverLinkRedialsAfterDrop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b := startFakeBridge(t)
	port, clientTLS, r, srv, reg := startVoiceRoomServer(t, ctx, b)
	if err := reg.UpsertStatic(roomsrv.StaticRoom{Code: "friends", DisplayCode: "Friends", Mumble: &rooms.MumbleIntegration{Channel: "Friends/Gaming"}}); err != nil {
		t.Fatal(err)
	}
	obs := b.nextLink(t)
	epoch, _ := reg.VoiceEpoch("friends")
	if m := obs.observe; m.Type != voice.TypeObserve || m.Code != "friends" || m.DisplayCode != "Friends" || m.Channel != "Friends/Gaming" || m.Temporary || m.Epoch != epoch {
		t.Fatalf("observe = %+v (epoch %d)", m, epoch)
	}
	if obs.origin != voice.LinkOrigin {
		t.Fatalf("observer origin %q", obs.origin)
	}

	c := openVoiceControl(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/room/friends", port), clientTLS, "alice")
	if st := c.nextState(t); st.Caps != wire.RoomCapVoice {
		t.Fatalf("caps = 0x%02x", st.Caps)
	}
	if e := c.nextVoice(t, wire.RoomEventVoiceRosterUpdated); e.VoiceFlags != 0 {
		t.Fatalf("roster before any report = %+v", e)
	}
	obs.roster(t, voice.Message{Available: true, Users: []voice.User{{Session: 3, Name: "native", Flags: wire.RoomVoiceUserSelfDeaf}}})
	if e := c.nextVoice(t, wire.RoomEventVoiceRosterUpdated); e.VoiceFlags != wire.RoomVoiceRosterFlagAvailable || len(e.VoiceRoster) != 1 ||
		e.VoiceRoster[0].Name != "native" || e.VoiceRoster[0].Flags != wire.RoomVoiceUserSelfDeaf {
		t.Fatalf("roster = %+v", e)
	}
	rows := func() roomsrv.RoomStats {
		_, body := h3Get(t, ctx, clientTLS, fmt.Sprintf("https://127.0.0.1:%d/statusz", port))
		if strings.Contains(string(body), `"friends"`) {
			t.Fatal("statusz carries the raw room code")
		}
		return roomStatszFrom(t, body)[r.ObfuscateID("friends")]
	}
	if row := rows(); !row.VoiceLive || row.VoiceObserver != "available" {
		t.Fatalf("statusz row = %+v", row)
	}

	// The bridge pod drops the link: the roster goes unavailable at once and
	// the relay redials.
	_ = obs.sess.CloseWithError(0, "bridge pod draining")
	if e := c.nextVoice(t, wire.RoomEventVoiceRosterUpdated); e.VoiceFlags != 0 || len(e.VoiceRoster) != 0 {
		t.Fatalf("roster after the drop = %+v", e)
	}
	again := b.nextLink(t)
	if again.observe.Code != "friends" {
		t.Fatalf("redial observe = %+v", again.observe)
	}
	again.roster(t, voice.Message{Available: true, Users: []voice.User{{Session: 4, Name: "back"}}})
	if e := c.nextVoice(t, wire.RoomEventVoiceRosterUpdated); e.VoiceFlags != wire.RoomVoiceRosterFlagAvailable || e.VoiceRoster[0].Name != "back" {
		t.Fatalf("roster after the redial = %+v", e)
	}
	// A refusing bridge is retried too (the backoff loop, not one shot).
	b.status.Store(http.StatusServiceUnavailable)
	_ = again.sess.CloseWithError(0, "")
	c.nextVoice(t, wire.RoomEventVoiceRosterUpdated)
	time.Sleep(200 * time.Millisecond)
	b.status.Store(0)
	b.nextLink(t)

	// The room ending stops the link for good.
	reg.EndRoom("friends", wire.RoomEndReasonOperator)
	waitFor(t, 5*time.Second, func() bool { return len(srv.voice.Load().m.running()) == 0 }, "observer link stopped")
}

func roomStatszFrom(t *testing.T, body []byte) map[string]roomsrv.RoomStats {
	t.Helper()
	var doc struct {
		Rooms map[string]roomsrv.RoomStats `json:"rooms"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Rooms
}

func TestVoiceSessionLinkRegistersAndEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b := startFakeBridge(t)
	port, clientTLS, _, srv, reg := startVoiceRoomServer(t, ctx, b)
	voiceRoom(t, b, reg)
	linkURL := fmt.Sprintf("https://127.0.0.1:%d/internal/voice/friends?psk=%s", port, testVoicePSK)

	// The route's gates.
	expectStatus(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/internal/voice/friends?psk=wrong", port), clientTLS, http.StatusUnauthorized)
	expectStatus(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/internal/voice/bad_code?psk=%s", port, testVoicePSK), clientTLS, http.StatusNotFound)

	alice := openVoiceControl(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/room/friends", port), clientTLS, "alice")
	st := alice.nextState(t)
	raw := voiceTicket(t, alice)
	link, acc := register(t, ctx, linkURL, clientTLS, raw, "198.51.100.7:50000")
	if acc.Type != voice.TypeAccepted || acc.Channel != "Friends/Gaming" || acc.Code != "friends" || acc.DisplayCode != "Friends" || acc.Temporary {
		t.Fatalf("accepted = %+v", acc)
	}
	link.send(t, voice.Message{Type: voice.TypeJoined, Session: 7, Name: "alice [web]"})
	for {
		e := alice.nextVoice(t, wire.RoomEventVoiceRosterUpdated)
		if e.Seq != st.Seq {
			t.Fatalf("roster seq %d advanced past %d", e.Seq, st.Seq)
		}
		if len(e.VoiceRoster) == 2 && e.VoiceRoster[1].Kind == wire.RoomVoiceKindGawk && e.VoiceRoster[1].ParticipantID == st.YourID {
			break
		}
	}
	link.send(t, voice.Message{Type: voice.TypeState, Session: 7, Flags: wire.RoomVoiceUserMute})
	if e := alice.nextVoice(t, wire.RoomEventVoiceRosterUpdated); e.VoiceRoster[1].Flags != wire.RoomVoiceUserMute {
		t.Fatalf("state = %+v", e.VoiceRoster)
	}

	// The same ticket again: a replay, refused terminally — and the
	// legitimate session is untouched.
	replay, m := register(t, ctx, linkURL, clientTLS, raw, "198.51.100.7:50001")
	if m.Type != voice.TypeClose || m.CloseCode != wire.CloseCodeVoiceRemoved {
		t.Fatalf("replay = %+v", m)
	}
	if code := sessionCloseCode(t, replay.sess); code != wire.CloseCodeVoiceRemoved {
		t.Fatalf("replay session close = %d", code)
	}
	// A forged ticket.
	if _, m := register(t, ctx, linkURL, clientTLS, []byte("not a ticket"), ""); m.CloseCode != wire.CloseCodeVoiceRemoved {
		t.Fatalf("forged = %+v", m)
	}
	// Not a Register at all: a protocol close.
	bad := dialVoiceLink(t, ctx, linkURL, clientTLS)
	bad.send(t, voice.Message{Type: voice.TypeJoined})
	if code := sessionCloseCode(t, bad.sess); code != roomCloseBadRequest {
		t.Fatalf("non-register close = %d", code)
	}

	// A newer registration of alice replaces the first (newest wins).
	newer, acc2 := register(t, ctx, linkURL, clientTLS, voiceTicket(t, alice), "198.51.100.7:50002")
	if acc2.Type != voice.TypeAccepted {
		t.Fatalf("newer = %+v", acc2)
	}
	link.expectClose(t, wire.CloseCodeVoiceRemoved)

	// Bob registers, then the bridge reports him Left: he is gone from the
	// session count.
	bob := openVoiceControl(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/room/friends", port), clientTLS, "bob")
	bob.nextState(t)
	bl, _ := register(t, ctx, linkURL, clientTLS, voiceTicket(t, bob), "198.51.100.8:1")
	waitFor(t, 5*time.Second, func() bool { return voiceStats(reg).VoiceSessions == 2 }, "two voice sessions")
	bl.send(t, voice.Message{Type: voice.TypeLeft, CloseCode: wire.CloseCodeVoiceRemoved, Reason: "kicked"})
	waitFor(t, 5*time.Second, func() bool { return voiceStats(reg).VoiceSessions == 1 }, "bob's Left")

	// Alice's room session ends: her voice goes with 4002 (C4).
	_ = alice.sess.CloseWithError(0, "")
	newer.expectClose(t, wire.CloseCodeServerDraining)
	if code := sessionCloseCode(t, newer.sess); code != wire.CloseCodeServerDraining {
		t.Fatalf("link session close = %d", code)
	}
	_ = srv
}

func TestKillClosesVoiceOverTheSessionLink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b := startFakeBridge(t)
	port, clientTLS, _, srv, reg := startVoiceRoomServer(t, ctx, b)
	bans := moderation.NewSet()
	srv.SetModeration(bans)
	voiceRoom(t, b, reg)
	linkURL := fmt.Sprintf("https://127.0.0.1:%d/internal/voice/friends?psk=%s", port, testVoicePSK)

	pub, id, tokenHex := dialPublisherHandshake(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")
	streamer := openVoiceControl(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/room/friends", port), clientTLS, "streamer")
	streamer.nextState(t)
	token, _ := hex.DecodeString(tokenHex)
	streamer.command(t, wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: id, ResumeToken: token, Label: "pc"})
	streamer.nextVoice(t, wire.RoomEventAttachmentAdded)
	sl, acc := register(t, ctx, linkURL, clientTLS, voiceTicket(t, streamer), "203.0.113.50:4000")
	if acc.Type != voice.TypeAccepted {
		t.Fatalf("accepted = %+v", acc)
	}
	viewer := openVoiceControl(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/room/friends", port), clientTLS, "viewer")
	viewer.nextState(t)
	vl, _ := register(t, ctx, linkURL, clientTLS, voiceTicket(t, viewer), "198.51.100.9:4000")

	// The broadcast is killed: its broadcaster's voice goes, terminally;
	// the viewer's stays.
	srv.HandleBanAdded(idBan(id, "abuse"))
	sl.expectClose(t, wire.CloseCodeVoiceRemoved)
	if code := sessionCloseCode(t, sl.sess); code != wire.CloseCodeVoiceRemoved {
		t.Fatalf("killed link close = %d", code)
	}
	if n := voiceStats(reg).VoiceSessions; n != 1 {
		t.Fatalf("voice sessions after the kill = %d, want the viewer's", n)
	}

	// An IP ban covering the viewer's voice client closes it, and refuses
	// its next registration.
	if err := bans.Upsert(ipBan("198.51.100.0/24", "abuse")); err != nil {
		t.Fatal(err)
	}
	srv.HandleBanAdded(ipBan("198.51.100.0/24", "abuse"))
	vl.expectClose(t, wire.CloseCodeVoiceRemoved)
	if _, m := register(t, ctx, linkURL, clientTLS, voiceTicket(t, viewer), "198.51.100.9:4001"); m.Type != voice.TypeClose || m.CloseCode != wire.CloseCodeVoiceRemoved {
		t.Fatalf("banned register = %+v", m)
	}
}

func TestVoiceRoomEndClosesTheLinkTerminally(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b := startFakeBridge(t)
	port, clientTLS, _, _, reg := startVoiceRoomServer(t, ctx, b)
	voiceRoom(t, b, reg)
	c := openVoiceControl(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/room/friends", port), clientTLS, "a")
	c.nextState(t)
	l, _ := register(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/internal/voice/friends?psk=%s", port, testVoicePSK), clientTLS, voiceTicket(t, c), "")
	reg.EndRoom("friends", wire.RoomEndReasonOperator)
	l.expectClose(t, wire.CloseCodeVoiceRemoved)
}

func TestMintWithVoiceOverTheWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b := startFakeBridge(t)
	port, clientTLS, _, _, _ := startVoiceRoomServer(t, ctx, b)
	pub, id, tokenHex := dialPublisherHandshake(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")
	c := openVoiceControl(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/room/new?broadcast=%s&resume=%s&voice=1", port, id, tokenHex), clientTLS, "a")
	if st := c.nextState(t); st.Caps != wire.RoomCapVoice {
		t.Fatalf("caps = 0x%02x", st.Caps)
	}
	if obs := b.nextLink(t); !obs.observe.Temporary || obs.observe.Channel != "" {
		t.Fatalf("observe = %+v", obs.observe)
	}
	pub2, id2, token2 := dialPublisherHandshake(t, ctx, port, clientTLS)
	defer pub2.CloseWithError(0, "")
	plain := openVoiceControl(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/room/new?broadcast=%s&resume=%s", port, id2, token2), clientTLS, "b")
	if st := plain.nextState(t); st.Caps != 0 {
		t.Fatalf("plain mint caps = 0x%02x", st.Caps)
	}
}

// The off-path (docs/80 §9 MV3, the docs/44 RM2 shape): with voice not
// configured there is no /internal/voice route, a mapped room advertises no
// capability, VoiceJoin is Unsupported, ?voice=1 mints a plain room, and a
// client that asks for voice receives no voice event.
func TestVoiceOffPathOverTheWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	port, clientTLS, _, srv, reg := startRoomServer(t, ctx, nil)
	if srv.voice.Load() != nil {
		t.Fatal("a voice manager without voice")
	}
	expectStatus(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/internal/voice/friends?psk=x", port), clientTLS, http.StatusNotFound)
	if err := reg.UpsertStatic(roomsrv.StaticRoom{Code: "friends", Mumble: &rooms.MumbleIntegration{Channel: "Friends"}}); err != nil {
		t.Fatal(err)
	}
	c := openVoiceControl(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/room/friends", port), clientTLS, "a")
	if st := c.nextState(t); st.Caps != 0 {
		t.Fatalf("caps = 0x%02x", st.Caps)
	}
	c.command(t, wire.RoomCommand{Kind: wire.RoomCommandVoiceJoin})
	e, err := wire.ParseRoomEvent(c.next(t))
	if err != nil || e.Kind != wire.RoomEventCommandRejected || e.Reason != wire.RoomRejectUnsupported || e.Command != wire.RoomCommandVoiceJoin {
		t.Fatalf("VoiceJoin = %+v, %v; want only an Unsupported reject", e, err)
	}
	pub, id, tokenHex := dialPublisherHandshake(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")
	m := openVoiceControl(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/room/new?broadcast=%s&resume=%s&voice=1", port, id, tokenHex), clientTLS, "b")
	if st := m.nextState(t); st.Caps != 0 {
		t.Fatalf("?voice=1 caps = 0x%02x", st.Caps)
	}
	// A Resync is answered with exactly the snapshot.
	c.command(t, wire.RoomCommand{Kind: wire.RoomCommandResync})
	c.nextState(t)
	_ = c.stream.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	var hdr [2]byte
	for {
		if _, err := io.ReadFull(c.stream, hdr[:]); err != nil {
			break // nothing more: no voice event followed
		}
		n, _ := wire.ParseRoomRecordLength(hdr[:])
		buf := make([]byte, n)
		_, _ = io.ReadFull(c.stream, buf)
		if e, err := wire.ParseRoomEvent(buf); err == nil && e.Kind >= wire.RoomEventVoiceTicket {
			t.Fatalf("a voice event without voice: %+v", e)
		}
	}
	srv.VoiceChanged()           // a no-op before StartVoice
	srv.StaticVoiceRoomSeen("x") // likewise
}

// voiceStats is the one voice-live room's stats row (rows are keyed by the
// HMAC'd code).
func voiceStats(reg *roomsrv.Registry) roomsrv.RoomStats {
	for _, row := range reg.Stats() {
		if row.VoiceLive {
			return row
		}
	}
	return roomsrv.RoomStats{}
}

// Every registration sentinel has its own deliberate close code (CODE-REVIEW
// "error mapping at boundaries", docs/80 §11.5).
func TestVoiceRefusalMapsEverySentinel(t *testing.T) {
	for err, want := range map[error]uint32{
		roomsrv.ErrVoiceTicketExpired: wire.CloseCodeVoiceUnavailable,
		roomsrv.ErrVoiceEpoch:         wire.CloseCodeVoiceUnavailable,
		roomsrv.ErrVoiceFull:          wire.CloseCodeVoiceUnavailable,
		roomsrv.ErrVoiceNoParticipant: wire.CloseCodeServerDraining,
		roomsrv.ErrVoiceReplay:        wire.CloseCodeVoiceRemoved,
		roomsrv.ErrVoiceWrongRoom:     wire.CloseCodeVoiceRemoved,
		roomsrv.ErrVoiceNotLive:       wire.CloseCodeVoiceRemoved,
		roomsrv.ErrVoiceBadTicket:     wire.CloseCodeVoiceRemoved,
	} {
		if code, reason := voiceRefusal(fmt.Errorf("wrapped: %w", err)); code != want || reason == "" {
			t.Errorf("%v → %d %q, want %d", err, code, reason, want)
		}
	}
}
