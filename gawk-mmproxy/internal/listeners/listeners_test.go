package listeners

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func lst(a string, v6only bool) Listener {
	return Listener{Addr: netip.MustParseAddrPort(a), V6Only: v6only}
}

// D7's rule, per family (PX1 criterion 2's fixtures).
func TestReachOf(t *testing.T) {
	for _, c := range []struct {
		name string
		ls   []Listener
		want Reach
	}{
		{"dual-stack :: (murmur, host= blank)", []Listener{lst("[::]:64738", false)}, Reach{V4: true, V6: true}},
		{"0.0.0.0", []Listener{lst("0.0.0.0:64738", false)}, Reach{V4: true}},
		{"127.0.0.1", []Listener{lst("127.0.0.1:64738", false)}, Reach{V4: true}},
		{"0.0.0.0 and a v6only ::", []Listener{lst("0.0.0.0:64738", false), lst("[::]:64738", true)}, Reach{V4: true, V6: true}},
		{"only the pod's address", []Listener{lst("10.11.0.7:64738", false)}, Reach{}},
		{"only ::1", []Listener{lst("[::1]:64738", false)}, Reach{V6: true}},
		{"only 127.0.0.2", []Listener{lst("127.0.0.2:64738", false)}, Reach{}},
		{"a v6only :: alone", []Listener{lst("[::]:64738", true)}, Reach{V6: true}},
		{"another port", []Listener{lst("[::]:64739", false), lst("0.0.0.0:1", false)}, Reach{}},
		{"nothing", nil, Reach{}},
	} {
		if got := ReachOf(c.ls, 64738); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// A rejected dump is ErrUnavailable, never an empty list (D7: never ready).
func TestRejectedDumpIsUnavailable(t *testing.T) {
	msg := make([]byte, unix.SizeofNlMsghdr+4)
	binary.NativeEndian.PutUint32(msg[0:4], uint32(len(msg)))
	binary.NativeEndian.PutUint16(msg[4:6], unix.NLMSG_ERROR)
	enoent := int32(-2) // -ENOENT
	binary.NativeEndian.PutUint32(msg[unix.SizeofNlMsghdr:], uint32(enoent))
	if _, _, err := parse(msg, unix.AF_INET); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err %v, want ErrUnavailable", err)
	}
}

func listen(t *testing.T, network, addr string, v6only bool) uint16 {
	t.Helper()
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if network == "tcp6" {
			_ = c.Control(func(fd uintptr) {
				v := 0
				if v6only {
					v = 1
				}
				serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, v)
			})
		}
		return serr
	}}
	ln, err := lc.Listen(context.Background(), network, addr)
	if err != nil {
		t.Skipf("can't listen on %s %s: %v", network, addr, err)
	}
	t.Cleanup(func() { ln.Close() })
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}

// The real kernel, in this process's own namespace: no capability needed.
func TestDumpSeesRealListenersAndV6Only(t *testing.T) {
	for _, c := range []struct {
		name, network, addr string
		v6only              bool
		want                Reach
	}{
		{"dual-stack ::", "tcp6", "[::]:0", false, Reach{V4: true, V6: true}},
		{"v6only ::", "tcp6", "[::]:0", true, Reach{V6: true}},
		{"::1", "tcp6", "[::1]:0", true, Reach{V6: true}},
		{"127.0.0.1", "tcp4", "127.0.0.1:0", false, Reach{V4: true}},
		{"0.0.0.0", "tcp4", "0.0.0.0:0", false, Reach{V4: true}},
		{"127.0.0.2", "tcp4", "127.0.0.2:0", false, Reach{}},
	} {
		port := listen(t, c.network, c.addr, c.v6only)
		got, err := Reachable(port)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s on port %d: %+v, want %+v", c.name, port, got, c.want)
		}
	}
}
