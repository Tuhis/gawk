// Package routes installs and checks the loopback policy routes the spoofed
// dial depends on (docs/92 D4):
//
//	from 127.0.0.0/8 iif lo lookup <table>
//	local 0.0.0.0/0 dev lo table <table>
//	from ::1/128 iif lo lookup <table>     (when lo has ::1)
//	local ::/0 dev lo table <table>        (when lo has ::1)
//
// Murmur answers the proxy's spoofed loopback connection from 127.0.0.1 (or
// ::1) to the client's address. Without these routes that reply would leave
// for the internet; with them every packet from loopback, arriving on lo, is
// delivered locally, back to the proxy's transparent socket.
//
// Installing needs CAP_NET_ADMIN over the namespace; checking needs no
// capability. Install checks before it adds (and routes use NLM_F_EXCL with
// EEXIST as done), so a second run never duplicates a rule (F12: `ip rule
// add` does, and so does NLM_F_EXCL for rules).
package routes

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"golang.org/x/sys/unix"
)

// DefaultTable is the policy-routing table (docs/92 §5.1).
const DefaultTable = 64738

// Family is one address family's pair of rule and route.
type Family struct {
	family   uint8
	src      netip.Prefix // the rule's source
	name     string
	loopback netip.Addr // the loopback address whose presence enables it
}

var (
	v4 = Family{family: unix.AF_INET, src: netip.MustParsePrefix("127.0.0.0/8"), name: "IPv4", loopback: netip.MustParseAddr("127.0.0.1")}
	v6 = Family{family: unix.AF_INET6, src: netip.MustParsePrefix("::1/128"), name: "IPv6", loopback: netip.MustParseAddr("::1")}
)

// State is what Check found.
type State struct {
	V4 bool // the IPv4 rule and route are both in place
	V6 bool // the IPv6 rule and route are both in place
}

// Result is what Install did, per family.
type Result struct {
	Family  string
	Skipped string // why the family was skipped (no ::1 on lo), or ""
	Err     error
}

// Install adds the rule and route for IPv4, and for IPv6 when lo has ::1.
// It never removes anything: the routes live and die with the namespace.
func Install(table uint32) []Result {
	lo, err := net.InterfaceByName("lo")
	if err != nil {
		return []Result{{Family: "IPv4", Err: fmt.Errorf("no lo: %w", err)}}
	}
	var out []Result
	for _, f := range []Family{v4, v6} {
		if f.family == unix.AF_INET6 && !hasAddr(lo, f.loopback) {
			out = append(out, Result{Family: f.name, Skipped: "lo has no ::1 (IPv6 is disabled in this namespace); IPv6 sources will be refused"})
			continue
		}
		err := addRule(f, table)
		if err == nil {
			err = addRoute(f, table, uint32(lo.Index))
		}
		out = append(out, Result{Family: f.name, Err: err})
	}
	return out
}

// Check reports which families have both their rule and route in table.
func Check(table uint32) (State, error) {
	lo, err := net.InterfaceByName("lo")
	if err != nil {
		return State{}, fmt.Errorf("no lo: %w", err)
	}
	var st State
	for _, f := range []Family{v4, v6} {
		rule, err := hasRule(f, table)
		if err != nil {
			if f.family == unix.AF_INET6 && errors.Is(err, unix.EAFNOSUPPORT) {
				continue
			}
			return State{}, err
		}
		route, err := hasRoute(f, table, uint32(lo.Index))
		if err != nil {
			if f.family == unix.AF_INET6 && errors.Is(err, unix.EAFNOSUPPORT) {
				continue
			}
			return State{}, err
		}
		ok := rule && route
		if f.family == unix.AF_INET {
			st.V4 = ok
		} else {
			st.V6 = ok
		}
	}
	return st, nil
}

func hasAddr(ifc *net.Interface, want netip.Addr) bool {
	addrs, err := ifc.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap() == want {
				return true
			}
		}
	}
	return false
}

// struct fib_rule_hdr: family, dst_len, src_len, tos, table, res1, res2,
// action (u8 each), flags (u32).
const sizeofFibRuleHdr = 12

func ruleMsg(f Family, table uint32) []byte {
	h := make([]byte, sizeofFibRuleHdr)
	h[0] = f.family
	h[2] = uint8(f.src.Bits())
	if table < 256 {
		h[4] = uint8(table)
	}
	h[7] = unix.FR_ACT_TO_TBL
	b := attr(h, unix.FRA_SRC, f.src.Addr().AsSlice())
	b = attr(b, unix.FRA_IIFNAME, []byte("lo\x00"))
	b = attr(b, unix.FRA_TABLE, u32(table))
	return b
}

func addRule(f Family, table uint32) error {
	// Check first: NLM_F_EXCL does not catch a duplicate rule, because the
	// kernel gives each rule without FRA_PRIORITY a new priority, and two
	// rules that differ only in priority are not "the same" (measured; F12).
	if ok, err := hasRule(f, table); err != nil {
		return err
	} else if ok {
		return nil
	}
	_, err := request(unix.NETLINK_ROUTE, unix.RTM_NEWRULE, unix.NLM_F_ACK|unix.NLM_F_CREATE|unix.NLM_F_EXCL, ruleMsg(f, table))
	if errors.Is(err, unix.EEXIST) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("add rule from %s iif lo lookup %d: %w", f.src, table, err)
	}
	return nil
}

func hasRule(f Family, table uint32) (bool, error) {
	h := make([]byte, sizeofFibRuleHdr)
	h[0] = f.family
	msgs, err := request(unix.NETLINK_ROUTE, unix.RTM_GETRULE, unix.NLM_F_DUMP, h)
	if err != nil {
		return false, fmt.Errorf("list %s rules: %w", f.name, err)
	}
	for _, m := range msgs {
		if m.typ != unix.RTM_NEWRULE || len(m.body) < sizeofFibRuleHdr {
			continue
		}
		b := m.body
		if b[0] != f.family || int(b[2]) != f.src.Bits() || b[7] != unix.FR_ACT_TO_TBL {
			continue
		}
		a := attrs(b[sizeofFibRuleHdr:])
		t := uint32(b[4])
		if v, ok := a[unix.FRA_TABLE]; ok && len(v) == 4 {
			t = binary.NativeEndian.Uint32(v)
		}
		src, _ := netip.AddrFromSlice(a[unix.FRA_SRC])
		if t == table && src == f.src.Addr() && string(trimNUL(a[unix.FRA_IIFNAME])) == "lo" {
			return true, nil
		}
	}
	return false, nil
}

// struct rtmsg: family, dst_len, src_len, tos, table, protocol, scope,
// type (u8 each), flags (u32).
func routeMsg(f Family, table, lo uint32) []byte {
	h := make([]byte, unix.SizeofRtMsg)
	h[0] = f.family
	if table < 256 {
		h[4] = uint8(table)
	}
	h[5] = unix.RTPROT_STATIC
	h[6] = unix.RT_SCOPE_HOST
	h[7] = unix.RTN_LOCAL
	b := attr(h, unix.RTA_TABLE, u32(table))
	b = attr(b, unix.RTA_OIF, u32(lo))
	return b
}

func addRoute(f Family, table, lo uint32) error {
	_, err := request(unix.NETLINK_ROUTE, unix.RTM_NEWROUTE, unix.NLM_F_ACK|unix.NLM_F_CREATE|unix.NLM_F_EXCL, routeMsg(f, table, lo))
	if errors.Is(err, unix.EEXIST) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("add route local %s/0 dev lo table %d: %w", anyAddr(f), table, err)
	}
	return nil
}

func hasRoute(f Family, table, lo uint32) (bool, error) {
	h := make([]byte, unix.SizeofRtMsg)
	h[0] = f.family
	msgs, err := request(unix.NETLINK_ROUTE, unix.RTM_GETROUTE, unix.NLM_F_DUMP, h)
	if err != nil {
		return false, fmt.Errorf("list %s routes: %w", f.name, err)
	}
	for _, m := range msgs {
		if m.typ != unix.RTM_NEWROUTE || len(m.body) < unix.SizeofRtMsg {
			continue
		}
		b := m.body
		if b[0] != f.family || b[1] != 0 || b[7] != unix.RTN_LOCAL {
			continue
		}
		a := attrs(b[unix.SizeofRtMsg:])
		t := uint32(b[4])
		if v, ok := a[unix.RTA_TABLE]; ok && len(v) == 4 {
			t = binary.NativeEndian.Uint32(v)
		}
		oif := uint32(0)
		if v, ok := a[unix.RTA_OIF]; ok && len(v) == 4 {
			oif = binary.NativeEndian.Uint32(v)
		}
		if t == table && oif == lo {
			return true, nil
		}
	}
	return false, nil
}

func anyAddr(f Family) string {
	if f.family == unix.AF_INET6 {
		return "::"
	}
	return "0.0.0.0"
}

func trimNUL(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}
