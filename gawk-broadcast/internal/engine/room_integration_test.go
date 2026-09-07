package engine_test

// R42 rooms (RM6) against the real relay binary — the same harness as
// relay_integration_test.go (startRelay, newFixtureSource), for the same
// reason: a hand-written fake relay would test the engine against our belief
// about the room protocol, and that belief is the thing worth doubting.

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// roomProbe is a second, plain room participant: a small client speaking the
// control protocol directly, standing in for the web participant whose
// RoomState the RM6 acceptance criterion is stated against.
type roomProbe struct {
	sess   *webtransport.Session
	stream *webtransport.Stream
}

func joinRoomProbe(t *testing.T, ctx context.Context, relayURL, code, query string) *roomProbe {
	t.Helper()
	d := &webtransport.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{http3.NextProtoH3}},
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	rsp, sess, err := d.Dial(ctx, relayURL+"/room/"+code+query, nil)
	if err != nil {
		status := 0
		if rsp != nil {
			status = rsp.StatusCode
		}
		t.Fatalf("room probe dial: %v (status %d)", err, status)
	}
	str, err := sess.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("room probe stream: %v", err)
	}
	hello, err := wire.AppendRoomHello(nil, wire.RoomHello{Protocol: wire.RoomProtocolVersion, ClientKind: wire.RoomClientWebViewer, Nickname: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := wire.AppendRoomRecord(nil, hello)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := str.Write(rec); err != nil {
		t.Fatalf("room probe hello: %v", err)
	}
	t.Cleanup(func() { _ = sess.CloseWithError(0, "") })
	return &roomProbe{sess: sess, stream: str}
}

// read returns the next record within `within`, its length prefix stripped.
func (p *roomProbe) read(t *testing.T, within time.Duration) []byte {
	t.Helper()
	_ = p.stream.SetReadDeadline(time.Now().Add(within))
	var hdr [wire.RoomRecordHeaderSize]byte
	if _, err := io.ReadFull(p.stream, hdr[:]); err != nil {
		t.Fatalf("room probe read: %v", err)
	}
	n, err := wire.ParseRoomRecordLength(hdr[:])
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(p.stream, buf); err != nil {
		t.Fatalf("room probe read: %v", err)
	}
	return buf
}

func (p *roomProbe) state(t *testing.T) wire.RoomState {
	t.Helper()
	rec := p.read(t, 10*time.Second)
	st, err := wire.ParseRoomState(rec)
	if err != nil {
		t.Fatalf("first record is not a RoomState: %v (%x)", err, rec)
	}
	return st
}

// waitEvent reads events until one satisfies want, or the deadline passes.
func (p *roomProbe) waitEvent(t *testing.T, what string, want func(wire.RoomEvent) bool) wire.RoomEvent {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		rec := p.read(t, time.Until(deadline))
		if rec[1] != wire.TypeRoomEvent {
			continue
		}
		ev, err := wire.ParseRoomEvent(rec)
		if err != nil {
			continue
		}
		if want(ev) {
			return ev
		}
	}
	t.Fatalf("room probe never saw %s", what)
	return wire.RoomEvent{}
}

// The RM6 acceptance criterion against the real relay: a native broadcaster
// mints a room from its broadcast, and its attachment — with its label — is
// what a second participant's RoomState lists. Then the engine's own detach
// and re-join, and finally the broadcaster stopping: the attachment stays,
// flagged away (live=false), for the grace — never removed by a stop.
func TestRoomMintAttachDetachAgainstRealRelay(t *testing.T) {
	relayURL, _ := startRelay(t, "-rooms")

	src := newFixtureSource(t, engine.NewClock())
	created := make(chan [2]string, 1)
	roomErrs := make(chan error, 8)
	sess := engine.New(
		engine.Config{RelayURL: relayURL, Insecure: true, Media: engine.DefaultMediaConfig(),
			RoomNew: true, RoomLabel: "Desk", Nickname: "native"},
		engine.Callbacks{
			OnRoomCreated: func(code, tok string) { created <- [2]string{code, tok} },
			OnRoomError:   func(err error) { roomErrs <- err },
		},
		engine.Options{MediaFactory: src.factory()},
	)
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stopped := false
	defer func() {
		if !stopped {
			sess.Stop()
		}
	}()

	var code, creator string
	select {
	case got := <-created:
		code, creator = got[0], got[1]
	case err := <-roomErrs:
		t.Fatalf("room error instead of a mint: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("the engine never minted a room")
	}
	if len(code) != 6 || len(creator) != 2*wire.RoomCreatorTokenSize {
		t.Fatalf("minted code %q / token %q have the wrong shape", code, creator)
	}
	id := sess.BroadcastID()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	probe := joinRoomProbe(t, ctx, relayURL, code, "")
	st := probe.state(t)
	if st.Flags&wire.RoomStateFlagDynamic == 0 {
		t.Errorf("room is not flagged dynamic: flags=%#x", st.Flags)
	}
	if len(st.Attachments) != 1 || st.Attachments[0].BroadcastID != id || st.Attachments[0].Label != "Desk" || !st.Attachments[0].Live {
		t.Fatalf("second participant sees attachments %+v, want the native broadcast %s labelled Desk, live", st.Attachments, id)
	}
	// The native participant is in the roster and flagged streaming: the
	// engine's ownership Attach after the mint may land before or after the
	// probe's snapshot, so accept it from either.
	streaming := func(p wire.RoomParticipant) bool {
		return p.Kind == wire.RoomClientNative && p.Nickname == "native" && p.Flags&wire.RoomParticipantFlagStreaming != 0
	}
	native := false
	for _, p := range st.Participants {
		if streaming(p) {
			native = true
		}
	}
	if !native {
		probe.waitEvent(t, "the native participant flagged streaming", func(ev wire.RoomEvent) bool {
			return ev.Kind == wire.RoomEventParticipantUpdated && streaming(ev.Participant)
		})
	}

	// The engine detaches: the attachment goes, reason publisher.
	sess.LeaveRoom()
	ev := probe.waitEvent(t, "AttachmentRemoved", func(ev wire.RoomEvent) bool {
		return ev.Kind == wire.RoomEventAttachmentRemoved && ev.Attachment.BroadcastID == id
	})
	if ev.Reason != wire.RoomDetachReasonPublisher {
		t.Errorf("detach reason = %d, want publisher (%d)", ev.Reason, wire.RoomDetachReasonPublisher)
	}

	// …and joins again by code through the live API: attached anew.
	if err := sess.JoinRoom(code, ""); err != nil {
		t.Fatal(err)
	}
	probe.waitEvent(t, "AttachmentAdded after re-join", func(ev wire.RoomEvent) bool {
		return ev.Kind == wire.RoomEventAttachmentAdded && ev.Attachment.BroadcastID == id && ev.Attachment.Live
	})

	// The broadcaster stops: away, not gone (docs/44 §4.4 — the attachment
	// outlives the participant until the broadcast grace expires).
	sess.Stop()
	stopped = true
	probe.waitEvent(t, "AttachmentUpdated live=false after the publisher stopped", func(ev wire.RoomEvent) bool {
		return ev.Kind == wire.RoomEventAttachmentUpdated && ev.Attachment.BroadcastID == id && !ev.Attachment.Live
	})

	select {
	case err := <-roomErrs:
		t.Errorf("room error during a healthy session: %v", err)
	default:
	}
}

// A static room with an attach secret (the -rooms-file source): the wrong
// secret is refused pre-upgrade as a typed 403, the right one attaches.
func TestRoomStaticAttachSecretAgainstRealRelay(t *testing.T) {
	roomsFile := filepath.Join(t.TempDir(), "rooms.json")
	if err := os.WriteFile(roomsFile, []byte(`[{"code":"TuhisRoom","attachSecret":"k"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	relayURL, _ := startRelay(t, "-rooms", "-rooms-file", roomsFile)

	src := newFixtureSource(t, engine.NewClock())
	roomErrs := make(chan error, 8)
	sess := engine.New(
		engine.Config{RelayURL: relayURL, Insecure: true, Media: engine.DefaultMediaConfig(),
			Room: "TuhisRoom", RoomAttachSecret: "wrong", RoomLabel: "Desk"},
		engine.Callbacks{OnRoomError: func(err error) { roomErrs <- err }},
		engine.Options{MediaFactory: src.factory()},
	)
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Stop()

	select {
	case err := <-roomErrs:
		re, ok := engine.AsRoomError(err)
		if !ok {
			t.Fatalf("room error %T (%v), want *RoomError", err, err)
		}
		if re.Status != http.StatusForbidden {
			t.Errorf("status = %d, want 403 for a wrong attach secret", re.Status)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a wrong attach secret produced no OnRoomError")
	}

	// The right secret, through the live API: the probe sees the attachment.
	if err := sess.JoinRoom("TuhisRoom", "k"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	probe := joinRoomProbe(t, ctx, relayURL, "tuhisroom", "")
	st := probe.state(t)
	id := sess.BroadcastID()
	for _, a := range st.Attachments {
		if a.BroadcastID == id && a.Label == "Desk" {
			return
		}
	}
	probe.waitEvent(t, "AttachmentAdded with the right secret", func(ev wire.RoomEvent) bool {
		return ev.Kind == wire.RoomEventAttachmentAdded && ev.Attachment.BroadcastID == id && ev.Attachment.Label == "Desk"
	})
}
