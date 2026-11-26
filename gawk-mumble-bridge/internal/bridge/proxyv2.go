package bridge

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"time"
)

// R88 (docs/92 D2, D8): with -mumble-proxy-addr set, each voice user's
// Mumble connection goes through gawk-mmproxy in murmur's pod, which dials
// murmur from the browser's own address. The bridge writes one standard
// PROXY protocol v2 header before TLS: the PROXY command, TCP over IPv4 or
// IPv6, the browser's address and port as the source, the unspecified
// address of the same family and port 0 as the destination, no TLVs.
// gawk-mmproxy has the decoder; testdata/proxyv2-vectors.json pins the bytes
// for both.

var proxyV2Signature = []byte("\r\n\r\n\x00\r\nQUIT\n")

// proxyV2Header encodes src.
func proxyV2Header(src netip.AddrPort) []byte {
	a := src.Addr().Unmap()
	b := append(make([]byte, 0, 52), proxyV2Signature...)
	if a.Is4() {
		b = append(b, 0x21, 0x11, 0, 12)
		v := a.As4()
		b = append(b, v[:]...)
		b = append(b, 0, 0, 0, 0)
	} else {
		b = append(b, 0x21, 0x21, 0, 36)
		v := a.As16()
		b = append(b, v[:]...)
		b = append(b, make([]byte, 16)...)
	}
	b = binary.BigEndian.AppendUint16(b, src.Port())
	return binary.BigEndian.AppendUint16(b, 0)
}

// errProxySource never carries the address (docs/80 D24, docs/92 D10).
var errProxySource = errors.New("the voice session's remote address can't be sent to gawk-mmproxy")

// proxySource is the browser's address as the header carries it,
// canonicalised (zone stripped, v4-mapped unmapped) as the relay's
// moderation.CanonicalAddr does. That package isn't imported: it would pull
// Kubernetes' apimachinery into the bridge for one line. An address the proxy
// would refuse (unspecified, multicast) is refused here too, so the join
// fails with proxy_source rather than as an unreachable murmur.
func proxySource(remote string) (netip.AddrPort, error) {
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		return netip.AddrPort{}, errProxySource
	}
	a := ap.Addr().WithZone("").Unmap()
	if !a.IsValid() || a.IsUnspecified() || a.IsMulticast() || a == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return netip.AddrPort{}, errProxySource
	}
	return netip.AddrPortFrom(a, ap.Port()), nil
}

// proxyDialer dials gawk-mmproxy and writes src's header before the
// Mumble client starts TLS.
func proxyDialer(src netip.AddrPort) func(ctx context.Context, network, addr string) (net.Conn, error) {
	h := proxyV2Header(src)
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if dl, ok := ctx.Deadline(); ok {
			_ = c.SetWriteDeadline(dl)
		}
		if _, err := c.Write(h); err != nil {
			c.Close()
			return nil, err
		}
		_ = c.SetWriteDeadline(time.Time{})
		return c, nil
	}
}
