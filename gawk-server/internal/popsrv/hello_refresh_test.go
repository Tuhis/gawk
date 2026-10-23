package popsrv

import (
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/lanaddr"
	"github.com/Tuhis/gawk/gawk-server/internal/popclient"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
)

// linkedView is the link-home pod's offer view of the harness PoP.
func linkedView(f *HomeFleet) (LinkedPop, bool) {
	holder := f.LinkHolder(popName)
	if holder == nil {
		return LinkedPop{}, false
	}
	for _, lp := range holder.Manager.Linked() {
		if lp.Name == popName {
			return lp, true
		}
	}
	return LinkedPop{}, false
}

// docs/76 §6.1 items 3 and 4: a PoP that stays linked republishes its
// pinned hashes when the certificate rotates and its LAN address when it
// changes, so home's offers (and the named DNS record, from status) never
// name a retired hash or an old address.
func TestALinkedPoPRepublishesItsHashesAndLANAddress(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	rolling, err := tlsutil.NewRolling(tlsutil.RollingOptions{
		Dir: t.TempDir(), Hosts: func() []string { return []string{"localhost"} }, Now: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	lan := lanaddr.NewWatcher("192.168.1.20", time.Hour)
	f, pop := StartHomeAndPop(t, 2, WithPopClient(func(o *popclient.Options) {
		o.LAN = lan
		o.Rolling = rolling
	}))
	pairLinked(t, f, pop)
	cur, next := rolling.Hashes()
	at := func(ip string) string {
		return netip.AddrPortFrom(netip.MustParseAddr(ip), uint16(pop.Port)).String()
	}
	WaitFor(t, "home has the first hello", func() bool {
		lp, ok := linkedView(f)
		return ok && lp.LanAddr == at("192.168.1.20") && slices.Equal(lp.PinnedHashes, []string{cur, next})
	})

	lan.SetOverride(netip.MustParseAddr("192.168.1.37"))
	WaitFor(t, "home follows the LAN address", func() bool {
		lp, ok := linkedView(f)
		return ok && lp.LanAddr == at("192.168.1.37")
	})
	WaitFor(t, "status.link follows the LAN address", func() bool {
		l := f.GetPop(t, popName).Status.Link
		return l != nil && l.LanAddr == at("192.168.1.37")
	})

	mu.Lock()
	now = now.Add(7 * 24 * time.Hour)
	mu.Unlock()
	rolling.Tick()
	cur2, next2 := rolling.Hashes()
	if cur2 == cur {
		t.Fatal("the certificate did not rotate")
	}
	WaitFor(t, "home follows the rotation", func() bool {
		lp, ok := linkedView(f)
		return ok && slices.Equal(lp.PinnedHashes, []string{cur2, next2})
	})
}

// gawk-relay -addr: a relay on a port other than 4433 (the harness PoP's
// is random) reports it, and the offer's pinned candidate carries it.
func TestTheLinkReportsTheRelaysPort(t *testing.T) {
	f, pop := StartHomeAndPop(t, 1, WithPopClient(func(o *popclient.Options) {
		o.LAN = lanaddr.NewWatcher("192.168.1.20", time.Hour)
	}))
	pairLinked(t, f, pop)
	if pop.Port == 4433 {
		t.Skip("the harness PoP drew the default port")
	}
	want := netip.AddrPortFrom(netip.MustParseAddr("192.168.1.20"), uint16(pop.Port)).String()
	WaitFor(t, "home has the port", func() bool {
		lp, ok := linkedView(f)
		return ok && lp.LanAddr == want
	})
}
