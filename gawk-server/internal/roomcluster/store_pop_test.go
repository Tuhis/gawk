package roomcluster

import (
	"testing"

	"github.com/Tuhis/gawk/gawk-server/rooms"
)

// R74 (docs/76 item 33): a Room CR's spec.pop and spec.maxParticipants
// reach the registry's static definition, or a structural schema keeping
// them would still leave the wall at the fleet's caps.
func TestStaticDefCarriesPopAndMaxParticipants(t *testing.T) {
	r := &rooms.Room{Spec: rooms.RoomSpec{Kind: rooms.KindStatic, Pop: "assembly-2026", MaxBroadcasts: 40, MaxParticipants: 200}}
	r.Name = "assembly-2026"
	def, err := (&Store{}).staticDef(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if def.Pop != "assembly-2026" || def.MaxParticipants != 200 || def.MaxBroadcasts != 40 {
		t.Errorf("staticDef = %+v", def)
	}
}
