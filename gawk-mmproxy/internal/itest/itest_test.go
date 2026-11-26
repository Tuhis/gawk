// Package itest runs gawk-mmproxy against real murmur, in the pod layout of
// testdata/topology.sh (docs/92 PX1 criterion 4). It skips unless
// GAWK_TEST_MMPROXY is set:
//
//	eval "$(testdata/topology.sh up 1.6)"
//	go test ./internal/itest/
//
// The Mumble-level cases that need a Mumble client (a kickban, audio both
// ways between a native client and a proxied user) are in
// gawk-mumble-bridge's integration tests, which have one (PX2 criterion 3).
// These are the TCP and TLS-level ones: murmur logs and refuses the spoofed
// address, the reset and FIN signalling, readiness against the real
// listener, a murmur restart.
package itest

import (
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

type env struct {
	murmur, proxy, ops, native, prefix, host string
}

func load(t *testing.T) env {
	t.Helper()
	if os.Getenv("GAWK_TEST_MMPROXY") == "" {
		t.Skip("GAWK_TEST_MMPROXY unset; see testdata/topology.sh")
	}
	return env{
		murmur: os.Getenv("GAWK_TEST_MMPROXY_MURMUR"), proxy: os.Getenv("GAWK_TEST_MMPROXY_PROXY"),
		ops: os.Getenv("GAWK_TEST_MMPROXY_OPS"), native: os.Getenv("GAWK_TEST_MMPROXY_NATIVE_ADDR"),
		prefix: os.Getenv("GAWK_TEST_MMPROXY_PREFIX"), host: os.Getenv("GAWK_TEST_MMPROXY_MURMUR_HOST"),
	}
}

var signature = []byte("\r\n\r\n\x00\r\nQUIT\n")

func header(src netip.AddrPort) []byte {
	b := append([]byte{}, signature...)
	if src.Addr().Is4() {
		b = append(b, 0x21, 0x11, 0, 12)
		a := src.Addr().As4()
		b = append(append(b, a[:]...), 0, 0, 0, 0)
	} else {
		b = append(b, 0x21, 0x21, 0, 36)
		a := src.Addr().As16()
		b = append(append(b, a[:]...), make([]byte, 16)...)
	}
	b = binary.BigEndian.AppendUint16(b, src.Port())
	return binary.BigEndian.AppendUint16(b, 0)
}

// handshake opens a TLS connection to murmur through the proxy as src.
func handshake(t *testing.T, e env, src string) error {
	t.Helper()
	c, err := net.DialTimeout("tcp", e.proxy, 3*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Write(header(netip.MustParseAddrPort(src))); err != nil {
		return err
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	return tls.Client(c, &tls.Config{InsecureSkipVerify: true}).Handshake()
}

func direct(t *testing.T, e env) error {
	t.Helper()
	c, err := net.DialTimeout("tcp", e.murmur, 3*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	return tls.Client(c, &tls.Config{InsecureSkipVerify: true}).Handshake()
}

func murmurLog(t *testing.T, e env) string {
	t.Helper()
	out, _ := exec.Command("docker", "logs", e.prefix+"-murmur").CombinedOutput()
	return string(out)
}

func sawConnection(t *testing.T, e env, addr string) bool {
	l := murmurLog(t, e)
	return strings.Contains(l, "New connection: "+addr+":") || strings.Contains(l, "New connection: ["+addr+"]:")
}

func topo(t *testing.T, verb string) {
	t.Helper()
	cmd := exec.Command("../../testdata/topology.sh", verb, os.Getenv("GAWK_TEST_MMPROXY_VERSION"))
	cmd.Env = append(os.Environ(), "GAWK_MMPROXY_PREFIX="+os.Getenv("GAWK_TEST_MMPROXY_PREFIX"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("topology.sh %s: %v\n%s", verb, err, out)
	}
}

func readyz(t *testing.T, e env) (bool, string) {
	t.Helper()
	r, err := http.Get("http://" + e.ops + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode == 200, strings.TrimSpace(string(b))
}

func metric(t *testing.T, e env, line string) bool {
	t.Helper()
	r, err := http.Get("http://" + e.ops + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return strings.Contains(string(b), line)
}

func isReset(err error) bool { return errors.Is(err, syscall.ECONNRESET) }

func TestAgainstRealMurmur(t *testing.T) {
	e := load(t)
	if e.host != "" {
		t.Skip("murmur has host= set: TestHostSetting covers this run")
	}

	t.Run("spoofed addresses reach murmur (F1, F6)", func(t *testing.T) {
		for _, c := range []struct{ src, addr string }{{"203.0.113.7:40001", "203.0.113.7"}, {"[2001:db8::7]:40020", "2001:db8::7"}} {
			if err := handshake(t, e, c.src); err != nil {
				t.Fatalf("%s: %v", c.src, err)
			}
			if !sawConnection(t, e, c.addr) {
				t.Errorf("murmur never logged %s", c.addr)
			}
		}
		if !metric(t, e, `gawk_mmproxy_connections_total{result="proxied"}`) {
			t.Error("no proxied metric")
		}
	})

	t.Run("a native client and a proxied one on the same address (F2)", func(t *testing.T) {
		if err := direct(t, e); err != nil {
			t.Fatalf("native: %v", err)
		}
		native := e.native
		if !sawConnection(t, e, native) {
			t.Skipf("murmur saw the native client at an address other than %s; can't pair them", native)
		}
		if err := handshake(t, e, native+":40003"); err != nil {
			t.Fatalf("proxied as the native client's address: %v", err)
		}
		if err := direct(t, e); err != nil {
			t.Fatalf("native after: %v", err)
		}
	})

	t.Run("native UDP is unaffected (F3)", func(t *testing.T) {
		c, err := net.Dial("udp", e.murmur)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		ping := make([]byte, 12)
		binary.BigEndian.PutUint64(ping[4:], 0x1234)
		c.SetDeadline(time.Now().Add(3 * time.Second))
		c.Write(ping)
		buf := make([]byte, 64)
		if n, err := c.Read(buf); err != nil || n != 24 {
			t.Fatalf("UDP ping: %d bytes, %v", n, err)
		}
	})

	t.Run("autoban is per address, and its reset reaches the client (F5, F8)", func(t *testing.T) {
		var last error
		n := 0
		for ; n < 12; n++ {
			if last = handshake(t, e, fmt.Sprintf("192.0.2.50:%d", 41000+n)); last != nil {
				break
			}
		}
		if last == nil {
			t.Fatal("never autobanned")
		}
		if !isReset(last) {
			t.Errorf("the autobanned handshake ended with %v, want a reset passed through", last)
		}
		if !metric(t, e, "gawk_mmproxy_resets_total") {
			t.Error("no resets metric")
		}
		if err := handshake(t, e, "192.0.2.51:41100"); err != nil {
			t.Errorf("another address right after: %v", err)
		}
	})

	t.Run("murmur down is a FIN, not a reset (D6, F9)", func(t *testing.T) {
		topo(t, "stop-murmur")
		defer topo(t, "start-murmur")
		time.Sleep(300 * time.Millisecond)
		err := handshake(t, e, "203.0.113.40:40040")
		if err == nil || isReset(err) {
			t.Fatalf("handshake with murmur down: %v, want EOF", err)
		}
		if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "EOF") {
			t.Errorf("handshake with murmur down: %v, want EOF", err)
		}
		if ok, why := readyz(t, e); ok || !strings.Contains(why, "no listener") {
			t.Errorf("/readyz with murmur down: %v %q", ok, why)
		}
	})

	t.Run("a murmur restart needs nothing from the proxy (F7)", func(t *testing.T) {
		topo(t, "restart-murmur")
		if err := handshake(t, e, "203.0.113.30:40030"); err != nil {
			t.Fatal(err)
		}
		if !sawConnection(t, e, "203.0.113.30") {
			t.Error("murmur never logged 203.0.113.30 after its restart")
		}
		if ok, why := readyz(t, e); !ok {
			t.Errorf("/readyz after the restart: %q", why)
		}
	})
}

// D7 against murmur's real sockets for this run's host= (blank or ::): the
// listing is recorded, and /readyz must agree with what D3's IPv4 dial
// actually does (docs/92 PX1 criterion 4).
func TestHostSetting(t *testing.T) {
	e := load(t)
	out, _ := exec.Command("docker", "exec", e.prefix+"-pause", "netstat", "-ltn").CombinedOutput()
	t.Logf("murmur %s, host=%q, listeners:\n%s", os.Getenv("GAWK_TEST_MMPROXY_VERSION"), e.host, out)
	ready, why := readyz(t, e)
	err := handshake(t, e, "203.0.113.60:40060")
	t.Logf("/readyz: %v %q; IPv4 dial: %v", ready, why, err)
	if ready != (err == nil) {
		t.Fatalf("/readyz says ready=%v but the IPv4 spoofed dial got %v", ready, err)
	}
	if e.host == "" && !ready {
		t.Fatalf("blank host= must be ready: %q", why)
	}
}
