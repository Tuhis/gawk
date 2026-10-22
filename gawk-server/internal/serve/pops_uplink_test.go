package serve

import (
	"context"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
)

// R74 PP8: a -pops home's broadcast events name the PoP a broadcast is
// reserved for, through a late-bound reservation set (cluster mode builds
// it after the hub).
func TestPopOfNamesTheReservingPoP(t *testing.T) {
	var res popsrv.Reservations
	popOf := popOfFor(func() popsrv.Reservations { return res })
	if got := popOf("K7XQ2M"); got != "" {
		t.Fatalf("before the set exists PopOf = %q", got)
	}
	res = popsrv.NewMemReservations()
	if _, err := res.Reserve(context.Background(), "K7XQ2M", "lan-party", 0); err != nil {
		t.Fatal(err)
	}
	if got := popOf("K7XQ2M"); got != "lan-party" {
		t.Errorf("PopOf(reserved) = %q, want lan-party", got)
	}
	if got := popOf("ABC234"); got != "" {
		t.Errorf("PopOf(home broadcast) = %q, want none", got)
	}
}
