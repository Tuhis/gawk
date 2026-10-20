// R73 RU8 (docs/75 D11, F14): reliable and DVR delivery repair the leg from
// the relay to the viewer, not the broadcaster's uplink. A chunk lost before
// the relay is missing for every viewer, carrier retransmission cannot bring
// it back, and only parity can — so the relay serves those viewers the same
// parity prefix a live-edge viewer gets.
//
// The loss here is on INGEST, and it is deterministic: the publisher withholds
// one data chunk of every frame and sends the frame's parity, as a lossy uplink
// would leave it. The subscribers are wired straight to the relay over real
// QUIC, so the carrier itself loses nothing and every frame that comes out
// whole came out whole because of the parity records it carried.

package transport

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

func TestCarrierViewersRecoverChunksLostOnIngest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, config.Config{
		MaxSubscribers:  8,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		ParityDefault:   2,
	})
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")

	// The two carrier modes: Smoother's plain carrier and Most stable's ring.
	// Neither asks for parity, as the shipping viewer does not: the fleet
	// default is what serves them.
	modes := []struct {
		name  string
		query string
		sink  *carrierSink
	}{
		{"reliable", "delivery=reliable", &carrierSink{}},
		{"dvr", "delivery=reliable&buffer=2500", &carrierSink{}},
	}
	deadline := time.Now().Add(30 * time.Second)
	readCtx, readCancel := context.WithDeadline(ctx, deadline)
	defer readCancel()
	for _, m := range modes {
		sub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s?%s", port, id, m.query), clientTLS)
		defer sub.CloseWithError(0, "")
		go readCarriers(readCtx, sub, deadline, m.sink)
	}
	waitFor(t, 10*time.Second, func() bool { return r.Stats().Totals.Subscribers == len(modes) }, "both subscribers registered")

	// One GOP: the keyframe opens the carriers, then the deltas.
	sendKeyframeStream(t, pub, buildStreamKeyframe(t, 0, "avc1.42E02A", 512))

	const (
		frames     = 12
		chunks     = 4
		payloadLen = 300
	)
	frameBytes := chunks * payloadLen
	want := make(map[uint32][][]byte, frames)
	var ingest [][]byte // what the relay receives: every frame one chunk short
	for f := range frames {
		frameID := uint32(f + 1)
		payloads := make([][]byte, chunks)
		withheld := f % chunks // a different position each frame
		for i := range chunks {
			p := make([]byte, payloadLen)
			for b := range p {
				p[b] = byte(int(frameID)*7 + i*31 + b)
			}
			payloads[i] = p
			if i == withheld {
				continue
			}
			d, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{
				FrameID: frameID, ChunkIndex: uint16(i), ChunkCount: chunks,
				TimestampUs: uint64(frameID) * 16_667,
			}, p)
			if err != nil {
				t.Fatalf("AppendVideoChunk: %v", err)
			}
			ingest = append(ingest, d)
		}
		symbols, err := wire.ComputeParity(payloads, 2)
		if err != nil {
			t.Fatalf("ComputeParity: %v", err)
		}
		for i, sym := range symbols {
			d, err := wire.AppendParityChunk(nil, wire.ParityChunkHeader{
				FrameID: frameID, ParityIndex: uint8(i), ChunkCount: chunks, FrameBytes: uint32(frameBytes),
			}, sym)
			if err != nil {
				t.Fatalf("AppendParityChunk: %v", err)
			}
			ingest = append(ingest, d)
		}
		want[frameID] = payloads
	}

	// Datagrams can drop even on loopback (runners cap UDP buffers below what
	// quic-go asks for), and a drop must cost a resend, not the test: the relay
	// forwards a duplicate verbatim and the sink keys by frame and index.
	settled := func() bool {
		for _, m := range modes {
			if sinkOf(m.sink).assess(t, want, chunks, frameBytes).stillMissing > 0 {
				return false
			}
		}
		return true
	}
	for attempt := 0; attempt < 20 && !settled(); attempt++ {
		for _, d := range ingest {
			if err := pub.SendDatagram(d); err != nil {
				t.Fatalf("SendDatagram: %v", err)
			}
		}
		waitUntil(500*time.Millisecond, settled)
	}

	for _, m := range modes {
		fs := sinkOf(m.sink)
		v := fs.assess(t, want, chunks, frameBytes)
		t.Logf("%s: %d parity records, complete=%d recovered=%d missing=%d corrupt=%d",
			m.name, fs.parityCount(), v.completeOnArrival, v.recoveredByParity, v.stillMissing, v.corrupt)
		if fs.parityCount() == 0 {
			t.Errorf("%s: no ParityChunk record on the carrier — the relay served this viewer no parity", m.name)
		}
		if v.completeOnArrival != 0 {
			t.Errorf("%s: %d frames arrived whole, but every frame was sent one chunk short", m.name, v.completeOnArrival)
		}
		if v.stillMissing != 0 || v.recoveredByParity != frames {
			t.Errorf("%s: recovered %d of %d frames from the carrier's parity, %d still missing",
				m.name, v.recoveredByParity, frames, v.stillMissing)
		}
		if v.corrupt != 0 {
			t.Errorf("%s: %d frames recovered to the WRONG bytes", m.name, v.corrupt)
		}
	}

	bst := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if bst.EgressParityBytes == 0 {
		t.Error("EgressParityBytes = 0: the parity written to the carriers was not counted")
	}
}

// sinkOf replays a carrier sink's records through the frame sink the
// datagram parity test uses: a carrier record IS the datagram, so the viewer
// handles both the same way.
func sinkOf(c *carrierSink) *frameSink {
	fs := newFrameSink()
	for _, run := range c.runs() {
		for _, rec := range run {
			fs.observe(rec)
		}
	}
	return fs
}
