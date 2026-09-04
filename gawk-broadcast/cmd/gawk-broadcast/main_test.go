package main

import (
	"errors"
	"net/http"
	"testing"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// R42: the room-view link is the grant hand-off docs/44 §4.8 specifies —
// `#/room/<CODE>?rt=<grant>` — and it exists only when an app URL does.
func TestRoomViewLink(t *testing.T) {
	if got := roomViewLink("https://gawk.example/", "AB2CD3", "ff00"); got != "https://gawk.example/#/room/AB2CD3?rt=ff00" {
		t.Errorf("roomViewLink = %q", got)
	}
	if got := roomViewLink("https://gawk.example", "AB2CD3", ""); got != "https://gawk.example/#/room/AB2CD3" {
		t.Errorf("roomViewLink without a grant = %q", got)
	}
	if got := roomViewLink("", "AB2CD3", "ff00"); got != "" {
		t.Errorf("roomViewLink without an app URL = %q, want empty (no default app URL on Linux)", got)
	}
}

func TestRoomMessagesAreSentences(t *testing.T) {
	got := roomMessage(&engine.RoomError{Op: "join", Status: http.StatusForbidden, Err: errors.New("403")})
	if got == "" || got[len(got)-1] != '.' {
		t.Errorf("RoomError message = %q, want a sentence", got)
	}
	got = roomMessage(&engine.RoomRejectError{Command: wire.RoomCommandAttach, Reason: wire.RoomRejectLimit, Detail: "full"})
	if got == "" || got[len(got)-1] != '.' {
		t.Errorf("RoomRejectError message = %q, want a sentence", got)
	}
	if got := roomEndReason(wire.RoomEndReasonCreator); got != "its creator ended it" {
		t.Errorf("roomEndReason(creator) = %q", got)
	}
}
