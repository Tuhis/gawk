package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
)

type fakeRoomStats struct {
	rows map[string]roomsrv.RoomStats
	tot  roomsrv.Totals
}

func (f fakeRoomStats) Stats() map[string]roomsrv.RoomStats { return f.rows }
func (f fakeRoomStats) TotalStats() roomsrv.Totals          { return f.tot }

// R42 room gauges (docs/44 §4.10): live rooms by kind from the totals,
// per-room participants/attachments for the rooms this pod HOLDS (keyed
// by the HMAC'd key the source hands over, never a raw code), and every
// proxied session folded into one gauge — a proxied room gets no per-room
// series, since the home pod already exports it.
func TestRoomCollectorExportsHomeRowsAndFoldsProxies(t *testing.T) {
	src := fakeRoomStats{
		rows: map[string]roomsrv.RoomStats{
			"key-home1":  {Kind: "dynamic", Participants: 3, Attachments: 2, Role: "home"},
			"key-home2":  {Kind: "static", Participants: 1, Attachments: 0, Role: "home"},
			"key-proxy1": {Kind: "dynamic", Participants: 2, Role: "proxy"},
			"key-proxy2": {Kind: "static", Participants: 4, Role: "proxy"},
		},
		tot: roomsrv.Totals{Static: 1, Dynamic: 1, Participants: 4, Attachments: 2},
	}
	collector := NewRoomCollector(src)
	if _, err := testutil.CollectAndLint(collector); err != nil {
		t.Errorf("CollectAndLint: %v", err)
	}
	promReg := prometheus.NewPedanticRegistry()
	promReg.MustRegister(collector)
	mfs, err := promReg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, c := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"gawk_rooms_live", map[string]string{"kind": "static"}, 1},
		{"gawk_rooms_live", map[string]string{"kind": "dynamic"}, 1},
		{"gawk_room_participants", map[string]string{"room": "key-home1"}, 3},
		{"gawk_room_attachments", map[string]string{"room": "key-home1"}, 2},
		{"gawk_room_participants", map[string]string{"room": "key-home2"}, 1},
		{"gawk_room_attachments", map[string]string{"room": "key-home2"}, 0},
		{"gawk_room_proxied_sessions", nil, 6},
	} {
		if got := value(mfs, c.name, c.labels); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
	for _, mf := range mfs {
		if mf.GetName() != "gawk_room_participants" && mf.GetName() != "gawk_room_attachments" {
			continue
		}
		if n := len(mf.GetMetric()); n != 2 {
			t.Errorf("%s has %d series, want 2 (home rooms only)", mf.GetName(), n)
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "room" && (lp.GetValue() == "key-proxy1" || lp.GetValue() == "key-proxy2") {
					t.Errorf("%s exported a per-room series for proxied room %s", mf.GetName(), lp.GetValue())
				}
			}
		}
	}

	// No rooms at all: the kind gauges are 0 and the proxy gauge is 0.
	empty := NewRoomCollector(fakeRoomStats{})
	emptyReg := prometheus.NewPedanticRegistry()
	emptyReg.MustRegister(empty)
	mfs, err = emptyReg.Gather()
	if err != nil {
		t.Fatalf("Gather(empty): %v", err)
	}
	if got := value(mfs, "gawk_rooms_live", map[string]string{"kind": "dynamic"}); got != 0 {
		t.Errorf("empty gawk_rooms_live{dynamic} = %v", got)
	}
	if got := value(mfs, "gawk_room_proxied_sessions", nil); got != 0 {
		t.Errorf("empty gawk_room_proxied_sessions = %v", got)
	}
}
