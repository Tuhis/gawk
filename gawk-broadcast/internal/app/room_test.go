package app

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/config"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/notify"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// R42 rooms (RM6): the room card's state machine, driven by the engine's
// room callbacks, testable without a window.

func liveApp(t *testing.T, fs *fakeSession) (*App, *config.Config) {
	t.Helper()
	a, cfg := testApp(t, fs, notify.Discard{})
	a.Start(context.Background(), "")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "going live")
	fs.cb.OnBroadcastID("K7M2QP")
	return a, cfg
}

// The card follows the relay: joined → attached → away → detached → ended.
func TestRoomCardFollowsTheRelay(t *testing.T) {
	fs := &fakeSession{}
	a, _ := liveApp(t, fs)
	if r := a.Room(); r.Status != RoomNone {
		t.Fatalf("status before any room = %v", r.Status)
	}

	fs.cb.OnRoomState(wire.RoomState{Code: "AB2CD3", Seq: 3,
		Attachments:  []wire.RoomAttachment{{BroadcastID: "K7M2QP", Label: "Desk", Live: true}, {BroadcastID: "ZZ9ZZ9", Live: true}},
		Participants: []wire.RoomParticipant{{ID: 1}, {ID: 2}, {ID: 3}},
	})
	r := a.Room()
	if r.Status != RoomAttached || r.Code != "AB2CD3" || r.Participants != 3 || r.Broadcasts != 2 {
		t.Errorf("after the snapshot: %+v", r)
	}

	fs.cb.OnRoomEvent(wire.RoomEvent{Kind: wire.RoomEventAttachmentUpdated, Attachment: wire.RoomAttachment{BroadcastID: "K7M2QP", Live: false}})
	if r := a.Room(); r.Status != RoomAway {
		t.Errorf("after live=false: %v", r.Status)
	}
	// Another broadcast's flag flip is not ours.
	fs.cb.OnRoomEvent(wire.RoomEvent{Kind: wire.RoomEventAttachmentUpdated, Attachment: wire.RoomAttachment{BroadcastID: "ZZ9ZZ9", Live: true}})
	if r := a.Room(); r.Status != RoomAway {
		t.Errorf("another attachment's update changed our status: %v", r.Status)
	}
	fs.cb.OnRoomEvent(wire.RoomEvent{Kind: wire.RoomEventAttachmentUpdated, Attachment: wire.RoomAttachment{BroadcastID: "K7M2QP", Live: true}})
	if r := a.Room(); r.Status != RoomAttached {
		t.Errorf("after live=true: %v", r.Status)
	}

	fs.cb.OnRoomEvent(wire.RoomEvent{Kind: wire.RoomEventParticipantLeft, Participant: wire.RoomParticipant{ID: 3}})
	fs.cb.OnRoomEvent(wire.RoomEvent{Kind: wire.RoomEventAttachmentRemoved, Attachment: wire.RoomAttachment{BroadcastID: "K7M2QP"}, Reason: wire.RoomDetachReasonPublisher})
	r = a.Room()
	if r.Status != RoomJoined || r.Participants != 2 || r.Broadcasts != 1 {
		t.Errorf("after detach + one leaving: %+v", r)
	}

	fs.cb.OnRoomEnded(wire.RoomEndReasonCreator)
	if r := a.Room(); r.Status != RoomEnded || r.Code != "AB2CD3" {
		t.Errorf("after the room ended: %+v", r)
	}
	// The broadcast is untouched.
	if s, _ := a.State(); s != StateLive {
		t.Error("the room ending changed the broadcast state")
	}

	// The session ending resets the card.
	fs.cb.OnEnded()
	if r := a.Room(); r.Status != RoomNone || r.Code != "" {
		t.Errorf("after the session ended: %+v", r)
	}
}

// Attach while idle is remembered and reaches the engine config on the next
// start; attach while live joins immediately as well.
func TestAttachRoomIdleThenLive(t *testing.T) {
	fs := &fakeSession{}
	a, cfg := testApp(t, fs, notify.Discard{})

	// A pasted room link is accepted: the code is the path segment.
	a.AttachRoom("https://gawk.example/#/room/TuhisRoom?rt=abc", " k ")
	if cfg.Room != "TuhisRoom" || cfg.RoomAttachSecret != "k" {
		t.Errorf("config after AttachRoom = room %q secret %q", cfg.Room, cfg.RoomAttachSecret)
	}
	reloaded, err := config.Load(cfg.Path())
	if err != nil || reloaded.Room != "TuhisRoom" || reloaded.RoomAttachSecret != "k" {
		t.Errorf("AttachRoom did not persist: %+v (%v)", reloaded, err)
	}
	if r := a.Room(); r.Status != RoomNone || r.Configured != "TuhisRoom" {
		t.Errorf("idle card = %+v, want not-in-a-room with the configured code", r)
	}
	if len(fs.rooms()) != 0 {
		t.Errorf("idle attach called the engine: %v", fs.rooms())
	}

	a.Start(context.Background(), "")
	waitFor(t, func() bool { s, _ := a.State(); return s == StateLive }, "going live")
	if fs.cfg.Room != "TuhisRoom" || fs.cfg.RoomAttachSecret != "k" {
		t.Errorf("engine config = room %q secret %q, want the persisted room", fs.cfg.Room, fs.cfg.RoomAttachSecret)
	}
	if r := a.Room(); r.Status != RoomJoining || r.Code != "TuhisRoom" {
		t.Errorf("card while starting with a room = %+v", r)
	}

	a.AttachRoom("ab2cd3", "")
	waitFor(t, func() bool { return len(fs.rooms()) == 1 }, "the live join")
	if got := fs.rooms(); !reflect.DeepEqual(got, []string{"join:ab2cd3:"}) {
		t.Errorf("engine calls = %v", got)
	}
	if cfg.Room != "ab2cd3" || cfg.RoomAttachSecret != "" {
		t.Errorf("config after the live attach = room %q secret %q", cfg.Room, cfg.RoomAttachSecret)
	}
}

// New room mints from the live broadcast, the creator token becomes the
// room-view grant, and it is never persisted. Detach forgets the room.
func TestNewRoomAndDetach(t *testing.T) {
	fs := &fakeSession{}
	a, cfg := liveApp(t, fs)
	cfg.RoomLabel = "Desk"

	a.NewRoom()
	waitFor(t, func() bool { return len(fs.rooms()) == 1 }, "the mint")
	if got := fs.rooms(); !reflect.DeepEqual(got, []string{"new:Desk"}) {
		t.Errorf("engine calls = %v", got)
	}
	fs.cb.OnRoomCreated("AB2CD3", strings.Repeat("ab", 16))
	fs.cb.OnRoomState(wire.RoomState{Code: "AB2CD3", Attachments: []wire.RoomAttachment{{BroadcastID: "K7M2QP", Live: true}}})
	if r := a.Room(); r.Status != RoomAttached || r.CreatorToken != strings.Repeat("ab", 16) {
		t.Errorf("after the mint: %+v", r)
	}
	if got, want := a.RoomViewLink(), "https://gawk.example/#/room/AB2CD3?rt="+strings.Repeat("ab", 16); got != want {
		t.Errorf("RoomViewLink = %q, want %q", got, want)
	}
	if strings.Contains(a.Diagnostics(), strings.Repeat("ab", 16)) {
		t.Error("the creator token leaked into the diagnostics dump")
	}

	a.DetachRoom()
	waitFor(t, func() bool { return len(fs.rooms()) == 2 }, "the leave")
	if got := fs.rooms()[1]; got != "leave" {
		t.Errorf("second engine call = %q", got)
	}
	if r := a.Room(); r.Status != RoomNone || r.CreatorToken != "" || cfg.Room != "" {
		t.Errorf("after detach: %+v, cfg.Room=%q", r, cfg.Room)
	}
	if a.RoomViewLink() != "" {
		t.Error("RoomViewLink outside a room")
	}
}

// Room failures are sentences on the card, and only a dead session (a refused
// dial) flips the status — a rejected command leaves the membership intact.
func TestRoomErrorsAreSentencesOnTheCard(t *testing.T) {
	fs := &fakeSession{}
	a, _ := liveApp(t, fs)

	fs.cb.OnRoomState(wire.RoomState{Code: "TuhisRoom"})
	fs.cb.OnRoomError(&engine.RoomRejectError{Command: wire.RoomCommandAttach, Reason: wire.RoomRejectForbidden, Detail: "attach key required"})
	r := a.Room()
	if r.Status != RoomJoined || !strings.Contains(r.Error, "attach secret") {
		t.Errorf("after a rejected attach: %+v", r)
	}

	fs.cb.OnRoomError(&engine.RoomError{Op: "join", Status: http.StatusNotFound, Err: errors.New("404")})
	r = a.Room()
	if r.Status != RoomError || !strings.HasSuffix(r.Error, ".") {
		t.Errorf("after a refused dial: %+v", r)
	}
	if a.LastError() != "" {
		t.Error("a room failure landed in the broadcast's error box")
	}
}

// The room-view grant hand-off: a static room's attach secret is the grant
// when there is no creator token, and no app URL means no link.
func TestRoomViewLinkGrants(t *testing.T) {
	fs := &fakeSession{}
	a, cfg := liveApp(t, fs)
	cfg.RoomAttachSecret = "k"
	fs.cb.OnRoomState(wire.RoomState{Code: "TuhisRoom"})
	if got := a.RoomViewLink(); got != "https://gawk.example/#/room/TuhisRoom?rt=k" {
		t.Errorf("RoomViewLink with an attach secret = %q", got)
	}
	cfg.AppURL = ""
	if got := a.RoomViewLink(); got != "" {
		t.Errorf("RoomViewLink without an app URL = %q", got)
	}
	if got := roomCodeFromInput("  https://gawk.example/#/room/AB2CD3  "); got != "AB2CD3" {
		t.Errorf("roomCodeFromInput(link) = %q", got)
	}
	if got := roomCodeFromInput("TuhisRoom"); got != "TuhisRoom" {
		t.Errorf("roomCodeFromInput(slug) = %q", got)
	}
}
