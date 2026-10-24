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
	"runtime"
	"strings"
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

func TestViewerDatagramSendAllocatesNoChunkCopy(t *testing.T) {
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

	// Sample every allocation from here on.
	prevRate := runtime.MemProfileRate
	runtime.MemProfileRate = 1
	defer func() { runtime.MemProfileRate = prevRate }()
	before := sendAllocBytes()

	for i := range allocTestChunks {
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
	// The viewer's count is a lower bound on the relay's sends, so the
	// average below can only come out high.
	waitFor(t, 5*time.Second, func() bool { return chunks.Load() >= allocTestChunks*9/10 },
		"the viewer to receive the chunks")
	time.Sleep(100 * time.Millisecond)
	got := chunks.Load()
	if c := corrupt.Load(); c > 0 {
		t.Fatalf("%d of %d chunks reached the viewer corrupt", c, got)
	}

	perChunk := (sendAllocBytes() - before) / got
	t.Logf("%d chunks reached the viewer; sending allocated %d bytes per chunk", got, perChunk)
	if perChunk > maxSendAllocPerChunk() {
		t.Fatalf("sending a %d-byte chunk to a viewer allocated %d bytes, want at most %d",
			wire.MaxDatagramSize, perChunk, maxSendAllocPerChunk())
	}
}
