package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
)

type fakeVoiceRooms struct{}

func (fakeVoiceRooms) Stats() map[string]roomsrv.RoomStats {
	return map[string]roomsrv.RoomStats{
		"aaaaaaaaaaaa": {Role: "home", VoiceLive: true, VoiceSessions: 3},
		"bbbbbbbbbbbb": {Role: "home"},
		"cccccccccccc": {Role: "proxy", VoiceLive: true, VoiceSessions: 9},
	}
}

func (fakeVoiceRooms) TotalStats() roomsrv.Totals {
	return roomsrv.Totals{VoiceLive: 1, VoiceSessions: 3}
}

func TestVoiceCollector(t *testing.T) {
	c := NewVoiceCollector(fakeVoiceRooms{})
	if _, err := testutil.CollectAndLint(c); err != nil {
		t.Fatal(err)
	}
	want := `
# HELP gawk_room_voice_sessions Voice sessions registered in a voice-live room this pod homes (R79).
# TYPE gawk_room_voice_sessions gauge
gawk_room_voice_sessions{room="aaaaaaaaaaaa"} 3
# HELP gawk_rooms_voice_live Voice-live rooms this pod homes (R79).
# TYPE gawk_rooms_voice_live gauge
gawk_rooms_voice_live 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
	if n := testutil.CollectAndCount(NewVoiceCollector(nil)); n != 1 {
		t.Fatalf("a nil source yields %d series", n)
	}
}
