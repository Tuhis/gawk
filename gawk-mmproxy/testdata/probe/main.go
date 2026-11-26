// Command probe is the kind job's client (docs/92 PX3 criterion 3), TEST
// ONLY: under testdata, so `go build ./...` skips it and no image ships it.
// It dials gawk-mmproxy, sends a PROXY v2 header for -src, then a TLS
// handshake with murmur through it, and prints one word: ok, eof, reset,
// timeout or refused.
package main

import (
	"crypto/tls"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "mumble-mmproxy:64739", "gawk-mmproxy's address")
	src := flag.String("src", "203.0.113.7:40001", "the source address the header carries")
	flag.Parse()
	fmt.Println(probe(*addr, netip.MustParseAddrPort(*src)))
}

func probe(addr string, src netip.AddrPort) string {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return "timeout"
		}
		return "refused"
	}
	defer c.Close()
	h := append([]byte{}, "\r\n\r\n\x00\r\nQUIT\n"...)
	a := src.Addr().As4()
	h = append(h, 0x21, 0x11, 0, 12)
	h = append(append(h, a[:]...), 0, 0, 0, 0)
	h = binary.BigEndian.AppendUint16(h, src.Port())
	h = binary.BigEndian.AppendUint16(h, 0)
	if _, err := c.Write(h); err != nil {
		return "reset"
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	err = tls.Client(c, &tls.Config{InsecureSkipVerify: true}).Handshake()
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	fmt.Fprintln(os.Stderr, err)
	return "error"
}
