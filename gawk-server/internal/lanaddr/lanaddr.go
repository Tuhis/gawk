// Package lanaddr finds the address a LAN relay advertises (R74 PP2,
// docs/76 §6.1 item 4): the IPv4 address of the interface that holds the
// default route, with loopback, link-local, container and virtual bridges,
// VPN tunnels and every non-private address (CGNAT and public ones
// included) excluded. The host can pick another
// candidate in the console; a DHCP change is picked up by the Watcher.
package lanaddr

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
)

// Candidate is one advertisable address.
type Candidate struct {
	Addr    netip.Addr
	Iface   string
	Primary bool
}

// Iface is the slice of a network interface Filter looks at.
type Iface struct {
	Name  string
	Flags net.Flags
	Addrs []netip.Prefix
}

// excludedIfacePrefixes are container, VM-bridge and VPN interfaces: an
// address on them is not one a guest on the venue's network can reach.
// Case-sensitive on purpose: "veth" is Linux's pair device, while Windows'
// "vEthernet (…)" can be the host's real uplink behind a Hyper-V switch.
//
//   - containers and VM bridges: docker, veth, br- (Docker's bridges, not a
//     user's br0), virbr (libvirt), vmnet and vboxnet (VMware and
//     VirtualBox on Linux and macOS), "VMware" and "VirtualBox" (their
//     Windows adapters' names), bridge1 (macOS bridge100 and up: UTM,
//     Parallels, Internet Sharing; bridge0 is the Thunderbolt bridge);
//   - VPN tunnels: utun (macOS), tailscale, wg (WireGuard), tun and tap
//     (OpenVPN and others), zt (ZeroTier).
var excludedIfacePrefixes = []string{
	"docker", "veth", "br-", "virbr", "vmnet", "vboxnet", "VMware", "VirtualBox", "bridge1",
	"utun", "tailscale", "wg", "tun", "tap", "zt",
}

// ula is fc00::/7, the IPv6 private range the certHash rule accepts.
var ula = netip.MustParsePrefix("fc00::/7")

// Filter is the pure half of Detect: the candidates among ifs, primary
// first. Only private addresses are kept (RFC 1918, and unique-local
// IPv6), because the pinned join link is accepted only for private
// literals (docs/76 §6.1 item 5).
// The primary is the first IPv4 address on defaultRouteIface; without one
// (no default route, as at a LAN party with no internet, or a route over a
// VPN), the first IPv4 candidate in interface-name order.
func Filter(ifs []Iface, defaultRouteIface string) []Candidate {
	var out []Candidate
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || excludedIface(ifc.Name) {
			continue
		}
		for _, p := range ifc.Addrs {
			a := p.Addr().Unmap()
			if !usable(a) {
				continue
			}
			out = append(out, Candidate{Addr: a, Iface: ifc.Name})
		}
	}
	if len(out) == 0 {
		return nil
	}
	slices.SortStableFunc(out, func(x, y Candidate) int {
		xr, yr := x.Iface == defaultRouteIface, y.Iface == defaultRouteIface
		switch {
		case x.Addr.Is4() != y.Addr.Is4():
			if x.Addr.Is4() {
				return -1
			}
			return 1
		case xr != yr:
			if xr {
				return -1
			}
			return 1
		case x.Iface != y.Iface:
			return strings.Compare(x.Iface, y.Iface)
		default:
			return x.Addr.Compare(y.Addr)
		}
	})
	if out[0].Addr.Is4() {
		out[0].Primary = true
	}
	// Primary first, then the remaining IPv4, then IPv6.
	return out
}

func excludedIface(name string) bool {
	for _, p := range excludedIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func usable(a netip.Addr) bool {
	switch {
	case !a.IsValid(), a.IsLoopback(), a.IsUnspecified(), a.IsMulticast(),
		a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast():
		return false
	case a.Is4():
		// RFC 1918 only, the IPv4 half of the certHash rule: every client
		// drops the hashes for any other literal (public or CGNAT), so a
		// join link naming it could not connect.
		return a.IsPrivate()
	default:
		return ula.Contains(a)
	}
}

// Detect enumerates the host's interfaces and returns Filter's candidates.
func Detect() ([]Candidate, error) {
	nifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	ifs := make([]Iface, 0, len(nifs))
	for _, ni := range nifs {
		addrs, err := ni.Addrs()
		if err != nil {
			continue
		}
		ifc := Iface{Name: ni.Name, Flags: ni.Flags}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if p, err := netip.ParsePrefix(ipn.String()); err == nil {
					ifc.Addrs = append(ifc.Addrs, p)
				}
			}
		}
		ifs = append(ifs, ifc)
	}
	return Filter(ifs, defaultRouteIface(ifs)), nil
}

// defaultRouteIface names the interface the kernel would route an internet
// packet out of. A connected UDP socket sends nothing; the local address
// it is bound to is the route lookup's answer, on every OS. "" when there is
// no default route.
func defaultRouteIface(ifs []Iface) string {
	c, err := net.Dial("udp4", "192.0.2.1:9") // TEST-NET-1, never answered
	if err != nil {
		return ""
	}
	defer c.Close()
	la, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	local, ok := netip.AddrFromSlice(la.IP)
	if !ok {
		return ""
	}
	local = local.Unmap()
	for _, ifc := range ifs {
		for _, p := range ifc.Addrs {
			if p.Addr().Unmap() == local {
				return ifc.Name
			}
		}
	}
	return ""
}

// Watcher tracks the advertised address: the host's override when set,
// otherwise the primary candidate, re-detected every period.
type Watcher struct {
	every  time.Duration
	detect func() ([]Candidate, error)

	mu       sync.Mutex
	override netip.Addr
	cands    []Candidate
	current  netip.Addr
	subs     map[int]func(netip.Addr)
	subID    int
}

// NewWatcher returns a watcher; override is -lan-address ("auto", "" or an
// IP literal). It detects nothing until Refresh or Run.
func NewWatcher(override string, every time.Duration) *Watcher {
	w := &Watcher{every: every, detect: Detect, subs: map[int]func(netip.Addr){}}
	if a, err := netip.ParseAddr(strings.TrimSpace(override)); err == nil {
		w.override = a.Unmap()
		w.current = w.override
	}
	return w
}

// Current is the address to advertise; the zero Addr when there is none.
func (w *Watcher) Current() netip.Addr {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.current
}

// Candidates is the last detection, primary first.
func (w *Watcher) Candidates() []Candidate {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Candidate(nil), w.cands...)
}

// Subscribe runs fn with the new address whenever Current changes.
func (w *Watcher) Subscribe(fn func(netip.Addr)) (cancel func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	id := w.subID
	w.subID++
	w.subs[id] = fn
	return func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.subs, id)
	}
}

// SetOverride pins the advertised address (the console's pick); the zero
// Addr goes back to detection.
func (w *Watcher) SetOverride(a netip.Addr) {
	w.mu.Lock()
	w.override = a.Unmap()
	w.publishLocked()
}

// Refresh re-detects now. A detection error keeps the previous candidates.
func (w *Watcher) Refresh() {
	cands, err := w.detect()
	w.mu.Lock()
	if err == nil {
		w.cands = cands
	}
	w.publishLocked()
}

// publishLocked recomputes Current, then unlocks and notifies on a change.
func (w *Watcher) publishLocked() {
	next := w.override
	if !next.IsValid() {
		for _, c := range w.cands {
			if c.Primary {
				next = c.Addr
				break
			}
		}
	}
	changed := next != w.current
	w.current = next
	var subs []func(netip.Addr)
	if changed {
		for _, fn := range w.subs {
			subs = append(subs, fn)
		}
	}
	w.mu.Unlock()
	for _, fn := range subs {
		fn(next)
	}
}

// Run refreshes immediately and then every period until ctx ends. It
// returns nil on cancellation, so it can ride serve.Options.Extra.
func (w *Watcher) Run(ctx context.Context) error {
	w.Refresh()
	if w.every <= 0 {
		<-ctx.Done()
		return nil
	}
	t := time.NewTicker(w.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.Refresh()
		}
	}
}
