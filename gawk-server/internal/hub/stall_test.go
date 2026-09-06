package hub

import (
	"errors"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// chunk is one delta video chunk datagram for frameID.
func chunk(t *testing.T, frameID uint32) []byte {
	t.Helper()
	d, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{FrameID: frameID, ChunkIndex: 0, ChunkCount: 1, TimestampUs: uint64(frameID)}, []byte{1, 2, 3})
	if err != nil {
		t.Fatalf("AppendVideoChunk: %v", err)
	}
	return d
}

// The publisher stall state (docs/06 revision 2026-09-06): a connected
// publisher that stops sending media is reported as not live after
// PublisherStallTimeout, and its broadcast is ended after BroadcastGrace of
// silence — the fix for a background broadcaster tab that held its slot and
// a "live" room tile for five hours on zero frames.

func stallRegistry(stall, grace time.Duration) *Registry {
	return NewRegistry(discardLog, Options{PublisherStallTimeout: stall, BroadcastGrace: grace})
}

func TestStalledPublisherIsNotLive(t *testing.T) {
	r := stallRegistry(30*time.Millisecond, time.Hour)
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer p.Close()

	// Fresh publisher: live, the stall clock started at the claim.
	if live, _, known := r.BroadcastState(id); !known || !live {
		t.Fatalf("fresh publisher: live=%v known=%v, want live", live, known)
	}
	// Media keeps it live past the timeout.
	time.Sleep(20 * time.Millisecond)
	p.HandleDatagram(chunk(t, 1))
	time.Sleep(20 * time.Millisecond)
	if live, _, _ := r.BroadcastState(id); !live {
		t.Fatal("publisher that sent a chunk 20ms ago reads as not live")
	}
	// Silence past the timeout: stalled — not live, still active, still known.
	time.Sleep(40 * time.Millisecond)
	live, _, known := r.BroadcastState(id)
	if !known || live {
		t.Fatalf("silent publisher: live=%v known=%v, want known and not live", live, known)
	}
	st := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if !st.PublisherActive || !st.PublisherStalled {
		t.Fatalf("stats: active=%v stalled=%v, want active and stalled", st.PublisherActive, st.PublisherStalled)
	}
	// Media returning un-stalls it.
	p.HandleDatagram(chunk(t, 2))
	if live, _, _ := r.BroadcastState(id); !live {
		t.Fatal("publisher that resumed sending still reads as not live")
	}
	if st := r.Stats().Broadcasts[r.ObfuscateID(id)]; st.PublisherStalled {
		t.Fatal("stats still stalled after media returned")
	}
}

func TestKeyframeAndAudioCountAsMedia(t *testing.T) {
	r := stallRegistry(30*time.Millisecond, time.Hour)
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer p.Close()
	time.Sleep(40 * time.Millisecond)
	if live, _, _ := r.BroadcastState(id); live {
		t.Fatal("precondition: should be stalled")
	}
	ingestKeyframe(t, p, keyframeMsg(t, 1, "avc1.42E02A", "kf"))
	if live, _, _ := r.BroadcastState(id); !live {
		t.Fatal("a keyframe did not clear the stall")
	}
	time.Sleep(40 * time.Millisecond)
	audio, err := wire.AppendAudioFrame(nil, wire.AudioFrameHeader{Seq: 1, TimestampUs: 1}, []byte{1, 2, 3})
	if err != nil {
		t.Fatalf("AppendAudioFrame: %v", err)
	}
	p.HandleDatagram(audio)
	if live, _, _ := r.BroadcastState(id); !live {
		t.Fatal("an audio frame did not clear the stall")
	}
}

func TestStallSweepEndsTheBroadcastAfterGrace(t *testing.T) {
	r := stallRegistry(20*time.Millisecond, 60*time.Millisecond)
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	pc := &fakePublisherConn{}
	if !p.BindConn(pc) {
		t.Fatal("BindConn = false on a fresh publisher")
	}
	f := &fakeSender{}
	if _, err := r.Subscribe(id, f); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Stalled but inside the grace: the sweep only notes it.
	time.Sleep(30 * time.Millisecond)
	r.SweepStalledPublishers(time.Now())
	if _, closed := f.getCloseInfo(); closed {
		t.Fatal("viewer closed before the grace elapsed")
	}
	if err := r.CheckSubscribe(id); err != nil {
		t.Fatalf("broadcast gone before the grace elapsed: %v", err)
	}

	// Silent for the whole grace: ended for everyone with the terminal code.
	time.Sleep(40 * time.Millisecond)
	r.SweepStalledPublishers(time.Now())
	code, closed := f.getCloseInfo()
	if !closed || code != uint32(wire.CloseCodeBroadcastEnded) {
		t.Fatalf("viewer: closed=%v code=%d, want closed with %d", closed, code, wire.CloseCodeBroadcastEnded)
	}
	if pcode, pclosed := pc.getCloseInfo(); !pclosed || pcode != uint32(wire.CloseCodeBroadcastEnded) {
		t.Fatalf("publisher session: closed=%v code=%d, want closed with %d", pclosed, pcode, wire.CloseCodeBroadcastEnded)
	}
	if err := r.CheckSubscribe(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("broadcast still known after the stall ended it: %v", err)
	}
	// The slot is free again.
	if err := r.CheckPublishNew(); err != nil {
		t.Fatalf("slot not released: %v", err)
	}
}

func TestStallDisabledByZeroTimeout(t *testing.T) {
	r := stallRegistry(0, 30*time.Millisecond)
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer p.Close()
	time.Sleep(50 * time.Millisecond)
	r.SweepStalledPublishers(time.Now())
	if live, _, known := r.BroadcastState(id); !known || !live {
		t.Fatalf("with the timeout off a silent publisher must stay live: live=%v known=%v", live, known)
	}
}

func TestEdgeHubNeverStalls(t *testing.T) {
	r := stallRegistry(10*time.Millisecond, time.Hour)
	id, p, err := r.EdgePublish("ABCDEF")
	if err != nil {
		t.Fatalf("EdgePublish: %v", err)
	}
	defer p.Close()
	time.Sleep(30 * time.Millisecond)
	if live, _, known := r.BroadcastState(id); !known || !live {
		t.Fatalf("edge hub: live=%v known=%v, want live (the Lease is its liveness)", live, known)
	}
}
