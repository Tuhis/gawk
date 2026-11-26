package mumble

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// A server that ends every connection before the TLS handshake, with a FIN
// or a reset, after reading the ClientHello.
func closingServer(t *testing.T, reset bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 5)
			_, _ = io.ReadFull(c, buf)
			tc := c.(*net.TCPConn)
			if reset {
				tc.SetLinger(0)
				tc.Close()
				continue
			}
			tc.CloseWrite()
			tc.SetReadDeadline(time.Now().Add(time.Second))
			io.Copy(io.Discard, tc)
			tc.Close()
		}
	}()
	return ln.Addr().String()
}

// docs/92 D6: through gawk-mmproxy, a pre-TLS reset is murmur's refusal
// (ErrRefused, reason autoban) and a pre-TLS EOF is murmur unreachable (a
// DialError); without a proxy both stay ErrRefused, as before R88.
func TestPreTLSCloseClassification(t *testing.T) {
	for _, c := range []struct {
		name            string
		proxied, reset  bool
		refused, dialEr bool
	}{
		{"direct, EOF", false, false, true, false},
		{"direct, reset", false, true, true, false},
		{"proxied, EOF", true, false, false, true},
		{"proxied, reset", true, true, true, false},
	} {
		addr := closingServer(t, c.reset)
		_, err := Dial(context.Background(), Config{Addr: addr, Username: "x", Proxied: c.proxied,
			TLS: TLSOptions{InsecureSkipVerify: true}, DialTimeout: 3 * time.Second})
		var de *DialError
		if got := errors.Is(err, ErrRefused); got != c.refused {
			t.Errorf("%s: ErrRefused=%v (%v), want %v", c.name, got, err, c.refused)
		}
		if got := errors.As(err, &de); got != c.dialEr {
			t.Errorf("%s: DialError=%v (%v), want %v", c.name, got, err, c.dialEr)
		}
	}
}
