package routes

import (
	"encoding/binary"
	"net/netip"
	"testing"

	"golang.org/x/sys/unix"
)

// The messages the kernel is sent, byte by byte: what the namespaced tests
// exercise for real runs in a child process, whose coverage the parent
// doesn't see.
func TestRuleMessageLayout(t *testing.T) {
	for _, c := range []struct {
		f     Family
		table uint32
		hdrT  uint8
	}{{v4, DefaultTable, 0}, {v6, DefaultTable, 0}, {v4, 100, 100}} {
		b := ruleMsg(c.f, c.table)
		if b[0] != c.f.family || int(b[2]) != c.f.src.Bits() || b[4] != c.hdrT || b[7] != unix.FR_ACT_TO_TBL {
			t.Errorf("%s table %d header %v", c.f.name, c.table, b[:sizeofFibRuleHdr])
		}
		a := attrs(b[sizeofFibRuleHdr:])
		src, _ := netip.AddrFromSlice(a[unix.FRA_SRC])
		if src != c.f.src.Addr() || string(trimNUL(a[unix.FRA_IIFNAME])) != "lo" || binary.NativeEndian.Uint32(a[unix.FRA_TABLE]) != c.table {
			t.Errorf("%s attrs %v", c.f.name, a)
		}
	}
}

func TestRouteMessageLayout(t *testing.T) {
	b := routeMsg(v6, 300, 1)
	if b[0] != unix.AF_INET6 || b[1] != 0 || b[4] != 0 || b[5] != unix.RTPROT_STATIC || b[6] != unix.RT_SCOPE_HOST || b[7] != unix.RTN_LOCAL {
		t.Fatalf("header %v", b[:unix.SizeofRtMsg])
	}
	a := attrs(b[unix.SizeofRtMsg:])
	if binary.NativeEndian.Uint32(a[unix.RTA_TABLE]) != 300 || binary.NativeEndian.Uint32(a[unix.RTA_OIF]) != 1 {
		t.Fatalf("attrs %v", a)
	}
	if anyAddr(v6) != "::" || anyAddr(v4) != "0.0.0.0" {
		t.Error("anyAddr")
	}
}

func nlmsg(typ uint16, seq uint32, body []byte) []byte {
	m := make([]byte, unix.SizeofNlMsghdr, unix.SizeofNlMsghdr+len(body)+3)
	m = append(m, body...)
	binary.NativeEndian.PutUint32(m[0:4], uint32(len(m)))
	binary.NativeEndian.PutUint16(m[4:6], typ)
	binary.NativeEndian.PutUint32(m[8:12], seq)
	for len(m)%unix.NLMSG_ALIGNTO != 0 {
		m = append(m, 0)
	}
	return m
}

func TestSplitAndAttrs(t *testing.T) {
	buf := append(nlmsg(unix.RTM_NEWRULE, 7, []byte{1, 2, 3}), nlmsg(unix.NLMSG_DONE, 7, nil)...)
	msgs, err := split(buf)
	if err != nil || len(msgs) != 2 || msgs[0].typ != unix.RTM_NEWRULE || msgs[0].seq != 7 || len(msgs[0].body) != 3 || msgs[1].typ != unix.NLMSG_DONE {
		t.Fatalf("split %+v %v", msgs, err)
	}
	bad := nlmsg(unix.RTM_NEWRULE, 1, nil)
	binary.NativeEndian.PutUint32(bad[0:4], 4096)
	if _, err := split(bad); err == nil {
		t.Error("a message longer than the buffer was accepted")
	}
	// A truncated attribute stops the walk instead of reading past the end.
	b := attr(nil, 5, []byte{1, 2, 3, 4})
	b = append(b, 0xff, 0x00) // a header that claims more than is left
	if a := attrs(b); len(a) != 1 || len(a[5]) != 4 {
		t.Errorf("attrs %v", a)
	}
	if string(trimNUL([]byte("lo\x00\x00"))) != "lo" {
		t.Error("trimNUL")
	}
}
