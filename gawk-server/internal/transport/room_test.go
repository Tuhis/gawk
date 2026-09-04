package transport

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// R42 room control sessions end to end over real QUIC (docs/44 RM2): the
// mint route, the join route and its status vocabulary, the hello/state
// exchange on the bidirectional stream, attach with the resume-token proof,
// 4007 on room end, the /statusz section — and the -rooms-off shape, in
// which none of it is routable.

type hubBroadcastsAdapter struct{ r *hub.Registry }

func (h hubBroadcastsAdapter) BroadcastState(id string) (roomsrv.BroadcastState, bool) {
	live, viewers, known := h.r.BroadcastState(id)
	return roomsrv.BroadcastState{Live: live, Viewers: viewers}, known
}

// startRoomServer starts a relay with -rooms on and returns the room
// registry it was given.
func startRoomServer(t *testing.T, ctx context.Context, mutate func(*roomsrv.Options)) (port int, clientTLS *tls.Config, r *hub.Registry, srv *Server, reg *roomsrv.Registry) {
	t.Helper()
	cfg := config.Config{MaxSubscribers: 5, MaxIdleTimeout: 30 * time.Second, KeepAlivePeriod: 10 * time.Second, BroadcastGrace: 5 * time.Minute, Rooms: true}
	var tlsCfg *tls.Config
	port, tlsCfg, r, _, srv = startTestServerCfgLogSrv(t, ctx, cfg, discardLog)
	opts := roomsrv.Options{Broadcasts: hubBroadcastsAdapter{r}, Obfuscate: r.ObfuscateID, Log: discardLog, EmptyGrace: time.Hour}
	if mutate != nil {
		mutate(&opts)
	}
	reg = roomsrv.NewRegistry(opts)
	srv.SetRooms(reg)
	return port, tlsCfg, r, srv, reg
}

// roomClient is one control session as a test client sees it.
type roomClient struct {
	sess   *webtransport.Session
	stream *webtransport.Stream
}

func (c *roomClient) send(t *testing.T, msg []byte) {
	t.Helper()
	rec, err := wire.AppendRoomRecord(nil, msg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.stream.Write(rec); err != nil {
		t.Fatalf("write control record: %v", err)
	}
}

func (c *roomClient) command(t *testing.T, cmd wire.RoomCommand) {
	t.Helper()
	msg, err := wire.AppendRoomCommand(nil, cmd)
	if err != nil {
		t.Fatal(err)
	}
	c.send(t, msg)
}

// next reads one framed message with a deadline.
func (c *roomClient) next(t *testing.T) []byte {
	t.Helper()
	_ = c.stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hdr [2]byte
	if _, err := io.ReadFull(c.stream, hdr[:]); err != nil {
		t.Fatalf("read record header: %v", err)
	}
	n, err := wire.ParseRoomRecordLength(hdr[:])
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.stream, buf); err != nil {
		t.Fatalf("read record body: %v", err)
	}
	return buf
}

func (c *roomClient) nextState(t *testing.T) wire.RoomState {
	t.Helper()
	st, err := wire.ParseRoomState(c.next(t))
	if err != nil {
		t.Fatalf("expected RoomState: %v", err)
	}
	return st
}

func (c *roomClient) nextEvent(t *testing.T, kind uint8) wire.RoomEvent {
	t.Helper()
	for {
		e, err := wire.ParseRoomEvent(c.next(t))
		if err != nil {
			t.Fatalf("expected RoomEvent: %v", err)
		}
		if e.Kind == kind {
			return e
		}
	}
}

// openControl dials, opens the bidi stream and sends RoomHello.
func openControl(t *testing.T, ctx context.Context, url string, clientTLS *tls.Config, nick string) *roomClient {
	t.Helper()
	sess := dial(t, ctx, url, clientTLS)
	return helloOn(t, ctx, sess, nick)
}

func helloOn(t *testing.T, ctx context.Context, sess *webtransport.Session, nick string) *roomClient {
	t.Helper()
	stream, err := sess.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("OpenStreamSync: %v", err)
	}
	c := &roomClient{sess: sess, stream: stream}
	msg, err := wire.AppendRoomHello(nil, wire.RoomHello{Protocol: wire.RoomProtocolVersion, ClientKind: wire.RoomClientWebViewer, Nickname: nick})
	if err != nil {
		t.Fatal(err)
	}
	c.send(t, msg)
	return c
}

func TestRoomMintJoinAttachAndEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	port, clientTLS, r, _, reg := startRoomServer(t, ctx, nil)

	pub, id, tokenHex := dialPublisherHandshake(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")

	// Mint from the live broadcast: the first RoomState carries the code,
	// the creator token and the attachment.
	mintURL := fmt.Sprintf("https://127.0.0.1:%d/room/new?broadcast=%s&resume=%s&label=pc", port, id, tokenHex)
	creator := openControl(t, ctx, mintURL, clientTLS, "tuhis")
	st := creator.nextState(t)
	if st.Flags&wire.RoomStateFlagDynamic == 0 || st.Flags&wire.RoomStateFlagCreator == 0 || st.Flags&wire.RoomStateFlagAttachOK == 0 {
		t.Fatalf("mint state flags = 0x%02x", st.Flags)
	}
	if len(st.CreatorToken) != wire.RoomCreatorTokenSize || len(st.Code) != 6 {
		t.Fatalf("mint state = %+v", st)
	}
	if len(st.Attachments) != 1 || st.Attachments[0].BroadcastID != id || st.Attachments[0].Label != "pc" || !st.Attachments[0].Live {
		t.Fatalf("mint attachments = %+v", st.Attachments)
	}
	code := st.Code
	creatorToken := hex.EncodeToString(st.CreatorToken)
	if hex.EncodeToString(st.Key) != r.ObfuscateID(strings.ToLower(code)) {
		t.Fatalf("RoomState key %x is not the /statusz key of the room", st.Key)
	}

	// A viewer joins by code and sees the roster; the creator is told.
	viewer := openControl(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/room/%s", port, strings.ToLower(code)), clientTLS, "viewer")
	vst := viewer.nextState(t)
	if vst.Flags&wire.RoomStateFlagCreator != 0 || vst.Flags&wire.RoomStateFlagAttachOK == 0 || len(vst.Participants) != 2 {
		t.Fatalf("viewer state = %+v", vst)
	}
	if e := creator.nextEvent(t, wire.RoomEventParticipantJoined); e.Participant.Nickname != "viewer" {
		t.Fatalf("joined = %+v", e)
	}

	// A second broadcaster attaches with its own resume token; a viewer
	// count arrives once someone watches it.
	pub2, id2, token2 := dialPublisherHandshake(t, ctx, port, clientTLS)
	defer pub2.CloseWithError(0, "")
	raw2, _ := hex.DecodeString(token2)
	bad := append([]byte{}, raw2...)
	bad[0] ^= 0xff
	viewer.command(t, wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: id2, ResumeToken: bad, Label: "laptop"})
	if e := viewer.nextEvent(t, wire.RoomEventCommandRejected); e.Reason != wire.RoomRejectBadProof {
		t.Fatalf("bad proof: %+v", e)
	}
	viewer.command(t, wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: id2, ResumeToken: raw2, Label: "laptop"})
	if e := creator.nextEvent(t, wire.RoomEventAttachmentAdded); e.Attachment.BroadcastID != id2 || e.Attachment.Label != "laptop" {
		t.Fatalf("attached = %+v", e)
	}
	sub := dialSubscriber(t, ctx, port, id2, clientTLS)
	defer sub.CloseWithError(0, "")
	waitFor(t, 5*time.Second, func() bool { return r.ViewerSubscribers(id2) == 1 }, "subscriber counted")
	// The refresh pump is not running in this fixture, so drive it: the
	// viewer count the hub reports reaches the room as a delta.
	reg.Refresh()
	if e := creator.nextEvent(t, wire.RoomEventAttachmentUpdated); e.Attachment.BroadcastID != id2 || e.Attachment.ViewerCount != 1 {
		t.Fatalf("viewer count delta = %+v", e)
	}

	// /statusz carries a rooms section keyed by the HMAC, never the code.
	_, body := h3Get(t, ctx, clientTLS, fmt.Sprintf("https://127.0.0.1:%d/statusz", port))
	var doc struct {
		Rooms map[string]roomsrv.RoomStats `json:"rooms"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Rooms) != 1 {
		t.Fatalf("statusz rooms = %v", doc.Rooms)
	}
	for key, row := range doc.Rooms {
		if strings.EqualFold(key, code) || key != r.ObfuscateID(strings.ToLower(code)) {
			t.Fatalf("statusz keyed by %q", key)
		}
		if row.Participants != 2 || row.Attachments != 2 || row.Kind != "dynamic" {
			t.Fatalf("statusz row = %+v", row)
		}
	}
	if strings.Contains(string(body), code) {
		t.Fatal("statusz leaks the raw room code")
	}

	// The creator's token, presented on a fresh session, grants end-room;
	// every participant sees RoomEnding then 4007, and both broadcasts are
	// still publishable.
	creator2 := openControl(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/room/%s?creator=%s", port, code, creatorToken), clientTLS, "tuhis")
	if st := creator2.nextState(t); st.Flags&wire.RoomStateFlagCreator == 0 {
		t.Fatalf("creator token not honoured: %+v", st)
	}
	creator2.command(t, wire.RoomCommand{Kind: wire.RoomCommandEndRoom})
	if e := viewer.nextEvent(t, wire.RoomEventRoomEnding); e.Reason != wire.RoomEndReasonCreator {
		t.Fatalf("ending = %+v", e)
	}
	if code := sessionCloseCode(t, viewer.sess); code != wire.CloseCodeRoomEnded {
		t.Fatalf("viewer control session close = %d, want 4007", code)
	}
	if code := sessionCloseCode(t, creator.sess); code != wire.CloseCodeRoomEnded {
		t.Fatalf("creator control session close = %d, want 4007", code)
	}
	if err := r.CheckSubscribe(id); err != nil {
		t.Fatalf("broadcast %s not joinable after the room ended: %v", id, err)
	}
	if rsp, sess, err := dialOnce(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/room/%s", port, code), clientTLS); err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("ended room still joinable")
	} else if rsp == nil || rsp.StatusCode != http.StatusNotFound {
		t.Fatalf("ended room: %v (err %v)", rsp, err)
	}
}

// The HMAC'd room key is the ONE handle an operator can carry between the
// relay log, /statusz, RoomState.key and the CR's status.key — so a join by
// an un-normalized code (upper-case, as the join box types it) must log the
// same key those surfaces publish. It hashed the raw path value before,
// which made a room's log lines ungreppable by its /statusz key.
func TestRoomJoinLogsTheNormalizedKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var logs syncBuffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	cfg := config.Config{MaxSubscribers: 5, MaxIdleTimeout: 30 * time.Second, KeepAlivePeriod: 10 * time.Second, BroadcastGrace: 5 * time.Minute, Rooms: true}
	port, clientTLS, r, _, srv := startTestServerCfgLogSrv(t, ctx, cfg, log)
	reg := roomsrv.NewRegistry(roomsrv.Options{Broadcasts: hubBroadcastsAdapter{r}, Obfuscate: r.ObfuscateID, Log: log, EmptyGrace: time.Hour})
	srv.SetRooms(reg)
	if err := reg.UpsertStatic(roomsrv.StaticRoom{Code: "TuhisRoom"}); err != nil {
		t.Fatal(err)
	}
	c := openControl(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/room/TUHISROOM", port), clientTLS, "shouty")
	st := c.nextState(t)
	want := r.ObfuscateID("tuhisroom")
	if hex.EncodeToString(st.Key) != want {
		t.Fatalf("RoomState key = %x, want the normalized key %s", st.Key, want)
	}
	c.sess.CloseWithError(0, "")
	waitFor(t, 5*time.Second, func() bool { return strings.Contains(logs.String(), "room session ended") }, "session end log line")
	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, "room_key=") {
			continue
		}
		if !strings.Contains(line, "room_key="+want) {
			t.Fatalf("log line carries a room_key that is not the normalized key %s: %s", want, line)
		}
	}
	if !strings.Contains(logs.String(), "room_key="+want) {
		t.Fatal("no log line carries the room key at all")
	}
}

// sessionCloseCode blocks (bounded) until the session is closed by the
// peer and returns the application close code, the way the drain and
// cluster tests observe one: through the error a session-level accept
// returns.
func sessionCloseCode(t *testing.T, sess *webtransport.Session) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := sess.AcceptUniStream(ctx)
	if err == nil {
		return -1
	}
	var se *webtransport.SessionError
	if errors.As(err, &se) {
		return int(se.ErrorCode)
	}
	t.Logf("session close error = %v", err)
	return -2
}

func TestRoomJoinStatusVocabulary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	port, clientTLS, _, _, reg := startRoomServer(t, ctx, func(o *roomsrv.Options) { o.MaxParticipants = 1; o.CreateSecret = "invite" })
	if err := reg.UpsertStatic(roomsrv.StaticRoom{Code: "TuhisRoom", AttachSecret: "key"}); err != nil {
		t.Fatal(err)
	}
	expect := func(url string, want int) {
		t.Helper()
		rsp, sess, err := dialOnce(t, ctx, url, clientTLS)
		if err == nil {
			sess.CloseWithError(0, "")
			t.Fatalf("%s: dial succeeded, want %d", url, want)
		}
		if rsp == nil || rsp.StatusCode != want {
			t.Fatalf("%s: got %v (err %v), want %d", url, rsp, err, want)
		}
	}
	base := fmt.Sprintf("https://127.0.0.1:%d", port)
	expect(base+"/room/nosuch", http.StatusNotFound)
	expect(base+"/room/TuhisRoom?attach=wrong", http.StatusForbidden)
	expect(base+"/room/TuhisRoom?creator=00000000000000000000000000000000", http.StatusForbidden)
	// Mint gates: unknown broadcast, wrong create secret.
	pub, id, tokenHex := dialPublisherHandshake(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")
	expect(base+"/room/new?broadcast="+id+"&resume="+tokenHex, http.StatusForbidden)
	expect(base+"/room/new?broadcast=ZZZZZZ&resume="+tokenHex+"&create=invite", http.StatusNotFound)
	expect(base+"/room/new?broadcast="+id+"&resume=00&create=invite", http.StatusForbidden)

	// A static-room viewer needs no secret; the right secret grants attach.
	viewer := openControl(t, ctx, base+"/room/tuhisroom", clientTLS, "v")
	if st := viewer.nextState(t); st.Flags&wire.RoomStateFlagAttachOK != 0 || st.Code != "TuhisRoom" {
		t.Fatalf("static viewer state = %+v", st)
	}
	// Participant limit (1) → 429 pre-upgrade.
	expect(base+"/room/TuhisRoom?attach=key", http.StatusTooManyRequests)
	viewer.sess.CloseWithError(0, "")
	waitFor(t, 5*time.Second, func() bool { info, _ := reg.Lookup("tuhisroom"); return info.Participants == 0 }, "viewer to leave")
	bc := openControl(t, ctx, base+"/room/TuhisRoom?attach=key", clientTLS, "b")
	if st := bc.nextState(t); st.Flags&wire.RoomStateFlagAttachOK == 0 {
		t.Fatalf("attach secret not honoured: %+v", st)
	}
	// A second room for the same broadcast is a 409 (D1).
	bc.sess.CloseWithError(0, "")
	waitFor(t, 5*time.Second, func() bool { info, _ := reg.Lookup("tuhisroom"); return info.Participants == 0 }, "broadcaster to leave")
	first := openControl(t, ctx, base+"/room/new?broadcast="+id+"&resume="+tokenHex+"&create=invite", clientTLS, "a")
	first.nextState(t)
	expect(base+"/room/new?broadcast="+id+"&resume="+tokenHex+"&create=invite", http.StatusConflict)
}

func TestRoomSessionWithoutHelloIsClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	port, clientTLS, _, _, reg := startRoomServer(t, ctx, nil)
	if err := reg.UpsertStatic(roomsrv.StaticRoom{Code: "abc"}); err != nil {
		t.Fatal(err)
	}
	sess := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/room/abc", port), clientTLS)
	stream, err := sess.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A command before the hello breaks the protocol.
	msg, _ := wire.AppendRoomCommand(nil, wire.RoomCommand{Kind: wire.RoomCommandResync})
	rec, _ := wire.AppendRoomRecord(nil, msg)
	if _, err := stream.Write(rec); err != nil {
		t.Fatal(err)
	}
	if code := sessionCloseCode(t, sess); code != roomCloseBadRequest {
		t.Fatalf("close = %d, want %d", code, roomCloseBadRequest)
	}
}

// docs/44 D17: with -rooms off the routes do not exist. The relay answers
// exactly as it would for any unknown path, and no registry is consulted.
func TestRoomsOffLeavesNoRoute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	port, clientTLS, _, _ := startTestServer(t, ctx, 5)
	for _, path := range []string{"/room/new", "/room/abcdef"} {
		rsp, sess, err := dialOnce(t, ctx, fmt.Sprintf("https://127.0.0.1:%d%s", port, path), clientTLS)
		if err == nil {
			sess.CloseWithError(0, "")
			t.Fatalf("%s routable with -rooms off", path)
		}
		if rsp == nil || rsp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: %v (err %v)", path, rsp, err)
		}
	}
	// And /statusz carries no rooms section at all.
	if _, body := h3Get(t, ctx, clientTLS, fmt.Sprintf("https://127.0.0.1:%d/statusz", port)); strings.Contains(string(body), `"rooms"`) {
		t.Fatalf("statusz has a rooms section with -rooms off: %s", body)
	}
}

// The hub half of docs/44 D3: a broadcast is never minted with an ID that
// names a live room.
func TestPublishNeverMintsALiveRoomCode(t *testing.T) {
	reserved := map[string]bool{}
	r := hub.NewRegistry(discardLog, hub.Options{IDReserved: func(id string) bool { return reserved[id] }})
	// Reserve everything: minting must fail rather than hand out a taken ID.
	all := hub.NewRegistry(discardLog, hub.Options{IDReserved: func(string) bool { return true }})
	if _, _, err := all.StartPublish(""); err == nil {
		t.Fatal("minted an ID every one of which names a room")
	}
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	pub.Close()
	if reserved[id] {
		t.Fatal("minted a reserved ID")
	}
}
