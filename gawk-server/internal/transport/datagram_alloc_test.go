// Fanning a chunk out to a datagram viewer must not allocate a copy of it.
// Stock quic-go allocates two per viewer and chunk: http3 builds a new
// buffer to put the session's quarter stream ID in front, then
// quic.Conn.SendDatagram copies that into the DATAGRAM frame. At a viewer's
// thousand chunks a second that was most of what the relay allocated, and
// with it most of its GC work. The vendored quic-go in third_party/quic-go
// assembles the prefixed datagram once, in a pooled buffer that goes back
// to the pool once it is in a packet (its GAWK-PATCH.md, patch 3).
package transport

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

const (
	// allocTestChunks is enough full chunks that the pool's first few
	// buffers don't matter to the average.
	allocTestChunks = 2000
	// sendDatagramFunc is the relay's call into its viewer's session, made
	// from a subscriber's drain (subscriberFuncs); the test's publisher
	// calls it too, client side.
	sendDatagramFunc = "github.com/quic-go/webtransport-go.(*Session).SendDatagram"
	subscriberFuncs  = "github.com/Tuhis/gawk/gawk-server/internal/hub.(*Subscriber)."
)

// sendAllocBytes is what the heap profile attributes to allocations made
// under sendDatagramFunc called from a subscriber. The profile covers allocations up to the last
// completed GC, so this collects first.
func sendAllocBytes() int64 {
	runtime.GC()
	runtime.GC()
	var recs []runtime.MemProfileRecord
	n, _ := runtime.MemProfile(nil, true)
	for {
		recs = make([]runtime.MemProfileRecord, n+50)
		var ok bool
		if n, ok = runtime.MemProfile(recs, true); ok {
			recs = recs[:n]
			break
		}
	}
	var total int64
	for _, r := range recs {
		var send, sub bool
		frames := runtime.CallersFrames(r.Stack())
		for {
			f, more := frames.Next()
			send = send || strings.HasPrefix(f.Function, sendDatagramFunc)
			sub = sub || strings.HasPrefix(f.Function, subscriberFuncs)
			if !more {
				break
			}
		}
		if send && sub {
			total += r.AllocBytes
		}
	}
	return total
}

// maxSendAllocPerChunk bounds what one chunk's send to one viewer may
// allocate: the frame's bookkeeping, a small fraction of the chunk. Stock
// quic-go allocates over twice the chunk. Under -race, sync.Pool drops a
// quarter of the buffers put back, and each costs a new one.
func maxSendAllocPerChunk() int64 {
	if raceEnabled {
		return wire.MaxDatagramSize / 2
	}
	return wire.MaxDatagramSize / 4
}

// fanOutChunks publishes n full chunks to a relay with one datagram viewer
// and returns how many reached it, failing on any that arrived corrupt.
// beforeSend runs once the viewer is subscribed.
func fanOutChunks(t *testing.T, n int, beforeSend func()) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	port, clientTLS, r, _, _ := startTestServerCfgLogSrv(t, ctx, config.Config{
		MaxSubscribers:  4,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace:  5 * time.Minute,
	}, discardLog)
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	waitFor(t, 5*time.Second, func() bool { return r.ViewerSubscribers(id) == 1 }, "the viewer's subscription")

	// Each chunk's payload is its frame ID's low byte less one, so a buffer
	// reused before its datagram left shows up as a corrupt chunk.
	var chunks, corrupt atomic.Int64
	go func() {
		for {
			d, err := sub.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			if _, typ, err := wire.PeekType(d); err != nil || typ != wire.TypeVideoChunk {
				continue
			}
			chunks.Add(1)
			hdr, payload, err := wire.ParseVideoChunk(d)
			if err != nil || len(payload) != wire.MaxChunkPayload ||
				!bytes.Equal(payload, bytes.Repeat([]byte{byte(hdr.FrameID - 1)}, len(payload))) {
				corrupt.Add(1)
			}
		}
	}()

	beforeSend()
	for i := range n {
		d, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{
			FrameID:     uint32(i + 1),
			ChunkCount:  1,
			TimestampUs: uint64(i+1) * 16_667,
		}, bytes.Repeat([]byte{byte(i)}, wire.MaxChunkPayload))
		if err != nil {
			t.Fatalf("AppendVideoChunk: %v", err)
		}
		if err := pub.SendDatagram(d); err != nil {
			t.Fatalf("SendDatagram: %v", err)
		}
		// Paced so neither the relay's nor the viewer's queues overflow.
		if i%50 == 49 {
			time.Sleep(2 * time.Millisecond)
		}
	}
	waitFor(t, 10*time.Second, func() bool { return chunks.Load() >= int64(n)*9/10 },
		"the viewer to receive the chunks")
	time.Sleep(100 * time.Millisecond)
	got := chunks.Load()
	if c := corrupt.Load(); c > 0 {
		t.Fatalf("%d of %d chunks reached the viewer corrupt", c, got)
	}
	return got
}

func TestViewerDatagramSendAllocatesNoChunkCopy(t *testing.T) {
	var before int64
	prevRate := runtime.MemProfileRate
	defer func() { runtime.MemProfileRate = prevRate }()
	got := fanOutChunks(t, allocTestChunks, func() {
		// Sample every allocation from here on.
		runtime.MemProfileRate = 1
		before = sendAllocBytes()
	})

	// The viewer's count is a lower bound on the relay's sends, so the
	// average below can only come out high.
	perChunk := (sendAllocBytes() - before) / got
	t.Logf("%d chunks reached the viewer; sending allocated %d bytes per chunk", got, perChunk)
	if perChunk > maxSendAllocPerChunk() {
		t.Fatalf("sending a %d-byte chunk to a viewer allocated %d bytes, want at most %d",
			wire.MaxDatagramSize, perChunk, maxSendAllocPerChunk())
	}
}

// debugLogChildEnv marks the child process TestDebugLogReadsNoPooledDatagram
// starts.
const debugLogChildEnv = "GAWK_TEST_QUIC_DEBUG_LOG_CHILD"

// quic-go's debug log prints every frame it sends, after the packer has
// returned a datagram's pooled buffer; it must not read the bytes, which
// another connection may already be writing. quic-go reads its log level
// once, at start, so this runs the fan-out in a child test binary with
// QUIC_GO_LOG_LEVEL=DEBUG, where -race reports any such read.
func TestDebugLogReadsNoPooledDatagram(t *testing.T) {
	if os.Getenv(debugLogChildEnv) != "" {
		fanOutChunks(t, 200, func() {})
		return
	}
	if !raceEnabled {
		t.Skip("only a -race build can see the read")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDebugLogReadsNoPooledDatagram$", "-test.count=1")
	cmd.Env = append(os.Environ(), debugLogChildEnv+"=1", "QUIC_GO_LOG_LEVEL=DEBUG")
	var out lastLines
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if bytes.Contains(out.race, []byte("WARNING: DATA RACE")) {
		t.Fatalf("quic-go's debug log races with the datagram buffer pool:\n%s", out.race)
	}
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out.tail())
	}
}

// lastLines keeps a child's first data race report and its last output,
// not the megabytes of debug log in between.
type lastLines struct {
	mu   sync.Mutex
	race []byte
	buf  []byte
}

func (l *lastLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.race == nil {
		if i := bytes.Index(p, []byte("WARNING: DATA RACE")); i >= 0 {
			l.race = append([]byte(nil), p[i:min(len(p), i+16384)]...)
		}
	}
	l.buf = append(l.buf, p...)
	if len(l.buf) > 64<<10 {
		l.buf = append(l.buf[:0], l.buf[len(l.buf)-32<<10:]...)
	}
	return len(p), nil
}

func (l *lastLines) tail() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf
}
