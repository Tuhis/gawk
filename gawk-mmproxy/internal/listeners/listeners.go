// Package listeners answers D7's readiness question without connecting to
// murmur: is there a TCP listener on the port that the proxy's loopback
// dial can reach, for IPv4 and for IPv6 (docs/92 D7)?
//
// It dumps listening sockets over NETLINK_SOCK_DIAG (inet_diag), which needs
// no capability and sees only the caller's network namespace, the one the
// proxy shares with murmur. /proc/net/tcp6 is not used: only inet_diag says
// whether a `::` socket is IPv6-only (INET_DIAG_SKV6ONLY), and a v6only `::`
// takes no IPv4 connection. Both AF_INET and AF_INET6 are dumped, because a
// dual-stack `::` socket appears only in the AF_INET6 dump.
package listeners

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"

	"golang.org/x/sys/unix"
)

// INET_DIAG_SKV6ONLY: the kernel attaches it to every AF_INET6 socket in a
// dump. x/sys/unix has no constant for it.
const inetDiagSKV6Only = 11

const (
	sizeofReqV2 = 56 // struct inet_diag_req_v2
	sizeofMsg   = 72 // struct inet_diag_msg
	tcpListen   = 10 // TCP_LISTEN
)

// Listener is one listening socket.
type Listener struct {
	Addr   netip.AddrPort
	V6Only bool // meaningful for IPv6 sockets only
}

// Reach is what D7 needs for one port.
type Reach struct {
	V4 bool // a listener D3's 127.0.0.1 dial reaches
	V6 bool // a listener D3's [::1] dial reaches
}

// ErrUnavailable wraps a dump the kernel refused (no inet_diag or tcp_diag,
// module loading disabled). D7: never ready, no /proc fallback.
var ErrUnavailable = errors.New("listeners: the kernel refused the inet_diag dump")

// Dump lists every TCP listener in the namespace, both families.
func Dump() ([]Listener, error) {
	var out []Listener
	for _, fam := range []uint8{unix.AF_INET, unix.AF_INET6} {
		ls, err := dump(fam)
		if err != nil {
			return nil, err
		}
		out = append(out, ls...)
	}
	return out, nil
}

// ReachOf applies D7's rule to port over ls.
//
// IPv4 is reachable on 0.0.0.0 or exactly 127.0.0.1 (D3's target), or on a
// `::` socket that is not v6only. IPv6 is reachable on `::` or `::1`.
func ReachOf(ls []Listener, port uint16) Reach {
	var r Reach
	lo4, any4 := netip.MustParseAddr("127.0.0.1"), netip.IPv4Unspecified()
	lo6, any6 := netip.IPv6Loopback(), netip.IPv6Unspecified()
	for _, l := range ls {
		if l.Addr.Port() != port {
			continue
		}
		a := l.Addr.Addr()
		switch {
		case a.Is4():
			if a == lo4 || a == any4 {
				r.V4 = true
			}
		case a == any6:
			r.V6 = true
			if !l.V6Only {
				r.V4 = true
			}
		case a == lo6:
			r.V6 = true
		case a.Is4In6() && (a.Unmap() == lo4 || a.Unmap() == any4):
			r.V4 = true
		}
	}
	return r
}

// Reachable dumps and applies ReachOf.
func Reachable(port uint16) (Reach, error) {
	ls, err := Dump()
	if err != nil {
		return Reach{}, err
	}
	return ReachOf(ls, port), nil
}

func dump(fam uint8) ([]Listener, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return nil, fmt.Errorf("%w: socket: %v", ErrUnavailable, err)
	}
	defer unix.Close(fd)
	req := make([]byte, unix.SizeofNlMsghdr+sizeofReqV2)
	binary.NativeEndian.PutUint32(req[0:4], uint32(len(req)))
	binary.NativeEndian.PutUint16(req[4:6], unix.SOCK_DIAG_BY_FAMILY)
	binary.NativeEndian.PutUint16(req[6:8], unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	binary.NativeEndian.PutUint32(req[8:12], 1)
	b := req[unix.SizeofNlMsghdr:]
	b[0] = fam
	b[1] = unix.IPPROTO_TCP
	b[2] = 0 // idiag_ext: the v6only attribute comes without asking
	binary.NativeEndian.PutUint32(b[4:8], 1<<tcpListen)
	if err := unix.Sendto(fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("%w: send: %v", ErrUnavailable, err)
	}
	var out []Listener
	buf := make([]byte, 1<<16)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return nil, fmt.Errorf("%w: recv: %v", ErrUnavailable, err)
		}
		ls, done, err := parse(buf[:n], fam)
		if err != nil {
			return nil, err
		}
		out = append(out, ls...)
		if done {
			return out, nil
		}
	}
}

// parse decodes one recv's worth of dump messages.
func parse(b []byte, fam uint8) ([]Listener, bool, error) {
	var out []Listener
	for len(b) >= unix.SizeofNlMsghdr {
		l := int(binary.NativeEndian.Uint32(b[0:4]))
		if l < unix.SizeofNlMsghdr || l > len(b) {
			return nil, false, errors.New("listeners: bad netlink message length")
		}
		typ := binary.NativeEndian.Uint16(b[4:6])
		body := b[unix.SizeofNlMsghdr:l]
		switch typ {
		case unix.NLMSG_DONE:
			return out, true, nil
		case unix.NLMSG_ERROR:
			code := int32(0)
			if len(body) >= 4 {
				code = int32(binary.NativeEndian.Uint32(body[0:4]))
			}
			return nil, false, fmt.Errorf("%w: %v", ErrUnavailable, unix.Errno(-code))
		case unix.SOCK_DIAG_BY_FAMILY:
			if l, ok := listener(body, fam); ok {
				out = append(out, l)
			}
		}
		next := (l + unix.NLMSG_ALIGNTO - 1) &^ (unix.NLMSG_ALIGNTO - 1)
		if next >= len(b) {
			break
		}
		b = b[next:]
	}
	return out, false, nil
}

// listener decodes struct inet_diag_msg and its attributes.
//
//	u8 family, state, timer, retrans
//	inet_diag_sockid: be16 sport, be16 dport, be32 src[4], be32 dst[4], u32 if, u32 cookie[2]
//	u32 expires, rqueue, wqueue, uid, inode
func listener(b []byte, fam uint8) (Listener, bool) {
	if len(b) < sizeofMsg || b[0] != fam || b[1] != tcpListen {
		return Listener{}, false
	}
	port := binary.BigEndian.Uint16(b[4:6])
	var addr netip.Addr
	if fam == unix.AF_INET {
		addr = netip.AddrFrom4([4]byte(b[8:12]))
	} else {
		addr = netip.AddrFrom16([16]byte(b[8:24]))
	}
	l := Listener{Addr: netip.AddrPortFrom(addr, port)}
	for a := b[sizeofMsg:]; len(a) >= unix.SizeofRtAttr; {
		n := int(binary.NativeEndian.Uint16(a[0:2]))
		if n < unix.SizeofRtAttr || n > len(a) {
			break
		}
		if binary.NativeEndian.Uint16(a[2:4]) == inetDiagSKV6Only && n > unix.SizeofRtAttr {
			l.V6Only = a[unix.SizeofRtAttr] != 0
		}
		next := (n + unix.RTA_ALIGNTO - 1) &^ (unix.RTA_ALIGNTO - 1)
		if next >= len(a) {
			break
		}
		a = a[next:]
	}
	return l, true
}
