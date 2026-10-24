//go:build linux

// A viewer's video leaves the relay as one QUIC DATAGRAM packet per chunk,
// and on Linux quic-go can hand the kernel a run of equal-size packets in
// one sendmsg (UDP GSO). Stock quic-go only batches packets that are exactly
// its maximum size, which a datagram packet never is: a full 1200-byte chunk
// makes a packet of about 1235 bytes against a 1280–1452 maximum, so every
// chunk to every viewer cost its own system call. The vendored quic-go in
// third_party/quic-go batches runs of equal-size packets instead (its
// GAWK-PATCH.md).
package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"

	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

const (
	// gsoTestALPN is this test's own protocol: the connection is plain QUIC.
	gsoTestALPN = "gawk-gso-test"
	// gsoTestDatagrams is enough datagrams for the congestion window to fill
	// and refill several times, so the run sees ACK-paced sends as well as
	// the first burst.
	gsoTestDatagrams = 400
	// minBatchSegment is QUIC's minimum datagram size (RFC 9000 §14). A batch
	// whose segments are any shorter would cap the packets after a short
	// first one (an ACK, the tail of a stream) at that size.
	minBatchSegment = 1200
)

// gsoWrite is one sendmsg: the bytes it carried and its UDP_SEGMENT size
// (0 when it carried a single packet without GSO).
type gsoWrite struct {
	bytes, segment int
}

func (w gsoWrite) packets() int {
	if w.segment == 0 || w.bytes <= w.segment {
		return 1
	}
	return (w.bytes + w.segment - 1) / w.segment
}

// gsoRecorder is the server's UDP socket, recording every write quic-go
// makes. It keeps everything that makes quic-go treat it as a *net.UDPConn
// (OOB, GSO detection through SyscallConn, batched reads).
type gsoRecorder struct {
	*net.UDPConn
	batch *ipv4.PacketConn

	mu     sync.Mutex
	on     bool
	writes []gsoWrite
}

func newGSORecorder(t *testing.T) *gsoRecorder {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return &gsoRecorder{UDPConn: c, batch: ipv4.NewPacketConn(c)}
}

// ReadBatch keeps quic-go's batched receive path (its batchConn).
func (c *gsoRecorder) ReadBatch(ms []ipv4.Message, flags int) (int, error) {
	return c.batch.ReadBatch(ms, flags)
}

func (c *gsoRecorder) WriteMsgUDP(b, oob []byte, addr *net.UDPAddr) (int, int, error) {
	c.mu.Lock()
	if c.on {
		c.writes = append(c.writes, gsoWrite{bytes: len(b), segment: udpSegment(oob)})
	}
	c.mu.Unlock()
	return c.UDPConn.WriteMsgUDP(b, oob, addr)
}

func (c *gsoRecorder) record() {
	c.mu.Lock()
	c.on = true
	c.mu.Unlock()
}

func (c *gsoRecorder) recorded() []gsoWrite {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]gsoWrite(nil), c.writes...)
}

// udpSegment returns the UDP_SEGMENT size a sendmsg's control data carries.
func udpSegment(oob []byte) int {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return 0
	}
	for _, m := range msgs {
		if m.Header.Level == unix.IPPROTO_UDP && m.Header.Type == unix.UDP_SEGMENT && len(m.Data) >= 2 {
			return int(binary.NativeEndian.Uint16(m.Data))
		}
	}
	return 0
}

// requireGSO skips where quic-go would not use GSO at all, so nothing here
// could be batched with or without the patch.
func requireGSO(t *testing.T, c *net.UDPConn) {
	t.Helper()
	if off, err := strconv.ParseBool(os.Getenv("QUIC_GO_DISABLE_GSO")); err == nil && off {
		t.Skip("QUIC_GO_DISABLE_GSO is set")
	}
	rc, err := c.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		_, serr = unix.GetsockoptInt(int(fd), unix.IPPROTO_UDP, unix.UDP_SEGMENT)
	}); err != nil {
		t.Fatalf("Control: %v", err)
	}
	if serr != nil {
		t.Skipf("the kernel has no UDP GSO: %v", serr)
	}
}

// gsoPair is a server connection on a recorded socket and its client.
type gsoPair struct {
	rec    *gsoRecorder
	server *quic.Conn
	client *quic.Conn
}

func newGSOPair(t *testing.T, ctx context.Context) gsoPair {
	t.Helper()
	rec := newGSORecorder(t)
	requireGSO(t, rec.UDPConn)

	cert, err := tlsutil.GenerateDevCert([]string{"localhost", "127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatalf("GenerateDevCert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	conf := &quic.Config{EnableDatagrams: true}

	tr := &quic.Transport{Conn: rec}
	t.Cleanup(func() { tr.Close() })
	ln, err := tr.Listen(&tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{gsoTestALPN}}, conf)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	client, err := quic.DialAddr(ctx, rec.LocalAddr().String(),
		&tls.Config{RootCAs: pool, ServerName: "localhost", NextProtos: []string{gsoTestALPN}}, conf)
	if err != nil {
		t.Fatalf("DialAddr: %v", err)
	}
	t.Cleanup(func() { client.CloseWithError(0, "") })
	server, err := ln.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	t.Cleanup(func() { server.CloseWithError(0, "") })
	select {
	case <-server.HandshakeComplete():
	case <-ctx.Done():
		t.Fatal("handshake did not complete")
	}
	return gsoPair{rec: rec, server: server, client: client}
}

// sendAndReceive sends datagrams of the given sizes from the server and
// waits for the client to receive every one. Each carries its index, so
// any datagram the server dropped shows up as missing.
func (p gsoPair) sendAndReceive(t *testing.T, ctx context.Context, sizes []int) {
	t.Helper()
	got := make(chan []int, 1)
	go func() {
		seen := make(map[int]int, len(sizes))
		for len(seen) < len(sizes) {
			d, err := p.client.ReceiveDatagram(ctx)
			if err != nil {
				break
			}
			if len(d) >= 4 {
				seen[int(binary.BigEndian.Uint32(d))] = len(d)
			}
		}
		var missing []int
		for i, n := range sizes {
			if seen[i] != n {
				missing = append(missing, i)
			}
		}
		got <- missing
	}()

	p.rec.record()
	for i, n := range sizes {
		d := make([]byte, n)
		binary.BigEndian.PutUint32(d, uint32(i))
		if err := p.server.SendDatagram(d); err != nil {
			var tooLarge *quic.DatagramTooLargeError
			if errors.As(err, &tooLarge) {
				t.Fatalf("datagram %d: %d bytes is over this path's %d", i, n, tooLarge.MaxDatagramPayloadSize)
			}
			t.Fatalf("SendDatagram %d: %v", i, err)
		}
	}
	if missing := <-got; len(missing) > 0 {
		t.Fatalf("%d of %d datagrams never arrived (first: #%d)", len(missing), len(sizes), missing[0])
	}
}

// checkSegments fails on a batch cut into segments shorter than QUIC's
// minimum packet size, which would cap every packet after the first.
func checkSegments(t *testing.T, writes []gsoWrite) {
	t.Helper()
	for _, w := range writes {
		if w.packets() > 1 && w.segment < minBatchSegment {
			t.Fatalf("a %d-byte write was cut into %d-byte segments, under %d", w.bytes, w.segment, minBatchSegment)
		}
	}
}

// A run of full chunks makes a run of equal-size packets, which must reach
// the kernel in batches instead of a sendmsg each.
func TestViewerDatagramsLeaveInGSOBatches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p := newGSOPair(t, ctx)

	// A full chunk as a viewer's session sends it: http3 puts the session's
	// quarter stream ID (one byte here) in front of the wire datagram.
	sizes := make([]int, gsoTestDatagrams)
	for i := range sizes {
		sizes[i] = wire.MaxDatagramSize + 1
	}
	p.sendAndReceive(t, ctx, sizes)

	writes := p.rec.recorded()
	checkSegments(t, writes)
	packets, batched := 0, 0
	for _, w := range writes {
		packets += w.packets()
		if w.packets() > 1 {
			batched += w.packets()
		}
	}
	t.Logf("%d packets in %d writes, %d of them batched", packets, len(writes), batched)
	if batched < gsoTestDatagrams/4 {
		t.Fatalf("only %d of %d datagram packets left in a batch (%d writes for %d packets); want at least %d",
			batched, gsoTestDatagrams, len(writes), packets, gsoTestDatagrams/4)
	}
}

// A batch caps each later packet at its first packet's size. A datagram that
// doesn't fit under that cap must wait for the next packet, never be
// discarded the way one that can't fit any packet is.
func TestGSOBatchKeepsDatagramsLargerThanItsSegment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p := newGSOPair(t, ctx)

	// Both sizes make packets over minBatchSegment, so either can open a
	// batch, and a larger one keeps following a smaller one's batch.
	rng := rand.New(rand.NewPCG(7, 9))
	sizes := make([]int, gsoTestDatagrams)
	for i := range sizes {
		sizes[i] = 1180
		if rng.IntN(3) == 0 {
			sizes[i] = 1220
		}
	}
	p.sendAndReceive(t, ctx, sizes)
	checkSegments(t, p.rec.recorded())
}
