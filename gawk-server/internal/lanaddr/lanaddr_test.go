package lanaddr

import (
	"context"
	"net"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"
)

func pfx(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(s))
	for _, p := range s {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}

const up = net.FlagUp | net.FlagBroadcast | net.FlagMulticast

// docs/76 §6.1 item 4: the primary address is the IPv4 address of the
// default-route interface; loopback, link-local, Docker and virtual bridges
// and CGNAT/VPN ranges are excluded.
func TestFilter(t *testing.T) {
	ifs := []Iface{
		{Name: "lo", Flags: net.FlagUp | net.FlagLoopback, Addrs: pfx("127.0.0.1/8", "::1/128")},
		{Name: "docker0", Flags: up, Addrs: pfx("172.17.0.1/16")},
		{Name: "br-1a2b3c", Flags: up, Addrs: pfx("172.18.0.1/16")},
		{Name: "veth12ab", Flags: up, Addrs: pfx("10.200.0.1/24")},
		{Name: "virbr0", Flags: up, Addrs: pfx("192.168.122.1/24")},
		{Name: "vmnet8", Flags: up, Addrs: pfx("192.168.200.1/24")},
		{Name: "utun3", Flags: up, Addrs: pfx("10.8.0.2/24")},
		{Name: "tailscale0", Flags: up, Addrs: pfx("100.101.102.103/32")},
		{Name: "down0", Flags: net.FlagBroadcast, Addrs: pfx("192.168.9.9/24")},
		{Name: "wlan0", Flags: up, Addrs: pfx("10.0.0.5/24", "169.254.3.4/16", "fe80::1/64")},
		{Name: "eth0", Flags: up, Addrs: pfx("192.168.1.20/24", "100.64.1.1/10", "fd12:3456::20/64", "2001:db8::20/64", "fe80::2/64")},
	}
	got := Filter(ifs, "eth0")
	want := []Candidate{
		{Addr: netip.MustParseAddr("192.168.1.20"), Iface: "eth0", Primary: true},
		{Addr: netip.MustParseAddr("10.0.0.5"), Iface: "wlan0"},
		{Addr: netip.MustParseAddr("fd12:3456::20"), Iface: "eth0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Filter =\n%v\nwant\n%v", got, want)
	}

	// No default route (a LAN party without internet): the first private
	// IPv4 candidate, in interface-name order, is primary, so the relay
	// still has an address.
	got = Filter(ifs, "")
	if len(got) != 3 || !got[0].Primary || got[0].Addr != netip.MustParseAddr("192.168.1.20") || got[1].Primary {
		t.Errorf("Filter with no default route = %v, want 192.168.1.20 alone primary", got)
	}
	// A default route over a VPN tunnel is not a LAN address.
	got = Filter(ifs, "utun3")
	if len(got) == 0 || got[0].Iface == "utun3" {
		t.Errorf("Filter with a VPN default route = %v", got)
	}
	if Filter(nil, "") != nil {
		t.Error("Filter(nil) should be empty")
	}
}

// A public IPv4 address is no candidate: every client drops a certHash
// for a non-private literal, so a join link naming it could not connect.
// The private address on another interface is primary instead.
func TestFilterKeepsOnlyPrivateIPv4(t *testing.T) {
	ifs := []Iface{
		{Name: "eth0", Flags: up, Addrs: pfx("203.0.113.7/24")},
		{Name: "eth1", Flags: up, Addrs: pfx("192.168.1.20/24")},
	}
	got := Filter(ifs, "eth0")
	want := []Candidate{{Addr: netip.MustParseAddr("192.168.1.20"), Iface: "eth1", Primary: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Filter = %v, want %v", got, want)
	}
}

// Common VPN tunnels and VM host-only adapters are excluded too, on every
// OS gawk-relay ships for: a full-tunnel WireGuard or OpenVPN holds the
// default route, and with no default route the name order would pick a
// host-only adapter ahead of the real NIC.
func TestFilterExcludesVPNAndVMAdapters(t *testing.T) {
	ifs := []Iface{
		{Name: "wg0", Flags: up, Addrs: pfx("10.13.13.2/24")},
		{Name: "tun0", Flags: up, Addrs: pfx("10.8.0.6/24")},
		{Name: "tap0", Flags: up, Addrs: pfx("10.9.0.6/24")},
		{Name: "zt5u4abc", Flags: up, Addrs: pfx("10.147.17.5/24")},
		{Name: "vboxnet0", Flags: up, Addrs: pfx("192.168.56.1/24")},
		{Name: "VirtualBox Host-Only Network", Flags: up, Addrs: pfx("192.168.56.1/24")},
		{Name: "VMware Network Adapter VMnet8", Flags: up, Addrs: pfx("192.168.200.1/24")},
		{Name: "bridge100", Flags: up, Addrs: pfx("192.168.64.1/24")},
		{Name: "Wi-Fi", Flags: up, Addrs: pfx("192.168.1.20/24")},
	}
	want := []Candidate{{Addr: netip.MustParseAddr("192.168.1.20"), Iface: "Wi-Fi", Primary: true}}
	for _, route := range []string{"wg0", "tun0", ""} {
		if got := Filter(ifs, route); !reflect.DeepEqual(got, want) {
			t.Errorf("Filter(default route %q) = %v, want %v", route, got, want)
		}
	}
}

type fakeDetector struct {
	mu    sync.Mutex
	cands []Candidate
}

func (f *fakeDetector) set(c ...Candidate) {
	f.mu.Lock()
	f.cands = c
	f.mu.Unlock()
}

func (f *fakeDetector) detect() ([]Candidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Candidate(nil), f.cands...), nil
}

// A DHCP change is picked up and republished (docs/76 §6.1 item 4); the
// host's override wins over detection.
func TestWatcher(t *testing.T) {
	a := netip.MustParseAddr("192.168.1.20")
	b := netip.MustParseAddr("192.168.1.42")
	det := &fakeDetector{}
	det.set(Candidate{Addr: a, Iface: "eth0", Primary: true})
	w := NewWatcher("auto", time.Hour)
	w.detect = det.detect
	w.Refresh()
	if w.Current() != a {
		t.Fatalf("Current = %v, want %v", w.Current(), a)
	}

	var mu sync.Mutex
	var seen []netip.Addr
	cancel := w.Subscribe(func(x netip.Addr) { mu.Lock(); seen = append(seen, x); mu.Unlock() })

	w.Refresh() // no change, no notification
	det.set(Candidate{Addr: b, Iface: "eth0", Primary: true})
	w.Refresh()
	if w.Current() != b {
		t.Fatalf("after DHCP change Current = %v, want %v", w.Current(), b)
	}

	o := netip.MustParseAddr("10.0.0.5")
	w.SetOverride(o)
	if w.Current() != o {
		t.Fatalf("override: Current = %v", w.Current())
	}
	det.set(Candidate{Addr: a, Iface: "eth0", Primary: true})
	w.Refresh()
	if w.Current() != o {
		t.Fatal("detection overrode the host's pick")
	}
	w.SetOverride(netip.Addr{})
	if w.Current() != a {
		t.Fatalf("back to auto: Current = %v", w.Current())
	}
	cancel()
	det.set(Candidate{Addr: b, Iface: "eth0", Primary: true})
	w.Refresh()

	mu.Lock()
	defer mu.Unlock()
	if want := []netip.Addr{b, o, a}; !reflect.DeepEqual(seen, want) {
		t.Errorf("notifications = %v, want %v", seen, want)
	}
	if got := w.Candidates(); len(got) != 1 || got[0].Addr != b {
		t.Errorf("Candidates = %v", got)
	}
}

func TestWatcherFixedOverrideAndRun(t *testing.T) {
	w := NewWatcher("192.168.5.5", 10*time.Millisecond)
	det := &fakeDetector{}
	det.set(Candidate{Addr: netip.MustParseAddr("10.1.1.1"), Primary: true})
	w.detect = det.detect
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for len(w.Candidates()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if w.Current() != netip.MustParseAddr("192.168.5.5") {
		t.Errorf("-lan-address literal: Current = %v", w.Current())
	}
	if len(w.Candidates()) != 1 {
		t.Error("Run never refreshed the candidates")
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v, want nil on cancel", err)
	}
}

// Detect runs against the real host; it may find nothing in a sandbox, but
// it must never return an excluded address.
func TestDetectNeverReturnsExcluded(t *testing.T) {
	cands, err := Detect()
	if err != nil {
		t.Skipf("Detect: %v", err)
	}
	for _, c := range cands {
		if c.Addr.IsLoopback() || c.Addr.IsLinkLocalUnicast() || !c.Addr.IsPrivate() || excludedIface(c.Iface) {
			t.Errorf("Detect returned an excluded address %v", c)
		}
	}
}
