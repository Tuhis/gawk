package routes

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// A minimal rtnetlink client: one request, then either its ACK or the
// messages of its dump. Nothing in gawk-mmproxy needs more.

var seq atomic.Uint32

const nlmsgHdrLen = unix.SizeofNlMsghdr

type message struct {
	typ  uint16
	body []byte
}

// request sends one netlink message on a fresh socket of protocol proto and
// returns the dump's messages (flags with NLM_F_DUMP) or nil after an ACK.
// A netlink error comes back as its unix.Errno.
func request(proto int, typ uint16, flags uint16, body []byte) ([]message, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, proto)
	if err != nil {
		return nil, fmt.Errorf("netlink socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("netlink bind: %w", err)
	}
	s := seq.Add(1)
	msg := make([]byte, nlmsgHdrLen, nlmsgHdrLen+len(body))
	msg = append(msg, body...)
	binary.NativeEndian.PutUint32(msg[0:4], uint32(len(msg)))
	binary.NativeEndian.PutUint16(msg[4:6], typ)
	binary.NativeEndian.PutUint16(msg[6:8], flags|unix.NLM_F_REQUEST)
	binary.NativeEndian.PutUint32(msg[8:12], s)
	if err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("netlink send: %w", err)
	}
	var out []message
	buf := make([]byte, 1<<16)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return nil, fmt.Errorf("netlink recv: %w", err)
		}
		msgs, err := split(buf[:n])
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			if m.seq != s {
				continue
			}
			switch m.typ {
			case unix.NLMSG_DONE:
				return out, nil
			case unix.NLMSG_ERROR:
				if len(m.body) < 4 {
					return nil, errors.New("netlink: short error message")
				}
				if code := int32(binary.NativeEndian.Uint32(m.body[0:4])); code != 0 {
					return nil, unix.Errno(-code)
				}
				return out, nil // the ACK
			default:
				// The next Recvfrom reuses buf: keep a copy.
				out = append(out, message{typ: m.typ, body: append([]byte(nil), m.body...)})
			}
		}
	}
}

type rawMessage struct {
	typ  uint16
	seq  uint32
	body []byte
}

func split(b []byte) ([]rawMessage, error) {
	var out []rawMessage
	for len(b) >= nlmsgHdrLen {
		l := int(binary.NativeEndian.Uint32(b[0:4]))
		if l < nlmsgHdrLen || l > len(b) {
			return nil, errors.New("netlink: bad message length")
		}
		out = append(out, rawMessage{
			typ:  binary.NativeEndian.Uint16(b[4:6]),
			seq:  binary.NativeEndian.Uint32(b[8:12]),
			body: b[nlmsgHdrLen:l],
		})
		if align(l) >= len(b) {
			break
		}
		b = b[align(l):]
	}
	return out, nil
}

func align(n int) int { return (n + unix.NLMSG_ALIGNTO - 1) &^ (unix.NLMSG_ALIGNTO - 1) }

func rtaAlign(n int) int { return (n + unix.RTA_ALIGNTO - 1) &^ (unix.RTA_ALIGNTO - 1) }

// attr appends one route attribute.
func attr(b []byte, typ uint16, data []byte) []byte {
	l := unix.SizeofRtAttr + len(data)
	h := make([]byte, unix.SizeofRtAttr)
	binary.NativeEndian.PutUint16(h[0:2], uint16(l))
	binary.NativeEndian.PutUint16(h[2:4], typ)
	b = append(b, h...)
	b = append(b, data...)
	for pad := rtaAlign(l) - l; pad > 0; pad-- {
		b = append(b, 0)
	}
	return b
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.NativeEndian.PutUint32(b, v)
	return b
}

// attrs parses route attributes into a map of the last value per type.
func attrs(b []byte) map[uint16][]byte {
	m := map[uint16][]byte{}
	for len(b) >= unix.SizeofRtAttr {
		l := int(binary.NativeEndian.Uint16(b[0:2]))
		if l < unix.SizeofRtAttr || l > len(b) {
			break
		}
		m[binary.NativeEndian.Uint16(b[2:4])&^unix.NLA_F_NESTED] = b[unix.SizeofRtAttr:l]
		if rtaAlign(l) > len(b) {
			break
		}
		b = b[rtaAlign(l):]
	}
	return m
}
