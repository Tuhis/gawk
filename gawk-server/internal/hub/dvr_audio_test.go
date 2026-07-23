package hub

// R21 DV5 (docs/26 Decision 8): audio gets its OWN ring, indexed by time, with
// its cursor slaved to the video cursor. Audio and video do not map 1:1 and
// must not be made to — a GOP exists because a delta frame is undecodable
// without its keyframe, and audio has no such structure.

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

func TestDVRAudioRingReplaysInOrder(t *testing.T) {
	r := NewDVRAudioRing(DVROptions{Window: time.Minute, MaxBytes: 1 << 20})
	t0 := time.Now()
	for i := range 5 {
		r.Append([]byte(fmt.Sprintf("a%02d", i)), t0.Add(time.Duration(i)*20*time.Millisecond))
	}
	c := r.Oldest()
	for i := range 5 {
		pkt, at, ok := r.At(c)
		if !ok {
			t.Fatalf("packet %d missing", i)
		}
		if want := fmt.Sprintf("a%02d", i); !bytes.Equal(pkt, []byte(want)) {
			t.Fatalf("packet %d = %q, want %q", i, pkt, want)
		}
		_ = at
		c = c.Next()
	}
	if _, _, ok := r.At(c); ok {
		t.Error("ring yielded a 6th packet")
	}
}

func TestDVRAudioRingOwnsItsBytesAndEvicts(t *testing.T) {
	r := NewDVRAudioRing(DVROptions{Window: 100 * time.Millisecond, MaxBytes: 1 << 20})
	t0 := time.Now()
	pkt := []byte("LIVE")
	r.Append(pkt, t0)
	for i := range pkt {
		pkt[i] = 'X'
	}
	got, _, ok := r.At(r.Oldest())
	if !ok || !bytes.Equal(got, []byte("LIVE")) {
		t.Errorf("packet = %q, want LIVE — the ring aliased the caller's buffer", got)
	}
	// Past the window the old packets go.
	for i := range 20 {
		r.Append([]byte("x"), t0.Add(time.Duration(i+1)*50*time.Millisecond))
	}
	if _, _, ok := r.At(DVRAudioCursor{}); ok {
		t.Error("a cursor into evicted audio still resolved")
	}
}

// The load-bearing coupling (docs/26 Decision 8b). Audio is ~4% of the
// bitrate, so after a stall it catches up almost instantly while video is
// still draining. The viewer holds audio against the video schedule and its
// jitter buffer overflow-drops anything arriving far ahead — so an unthrottled
// audio cursor would have the relay rescue the audio and the viewer bin it.
func TestDVRAudioCursorIsHeldToTheVideoCursor(t *testing.T) {
	r := NewDVRAudioRing(DVROptions{Window: time.Minute, MaxBytes: 1 << 20})
	t0 := time.Now()
	for i := range 100 {
		r.Append([]byte("pkt"), t0.Add(time.Duration(i)*20*time.Millisecond))
	}
	// The video cursor is a second behind the newest audio.
	videoAt := t0
	c := r.Oldest()
	sent := 0
	for {
		_, at, ok := r.At(c)
		if !ok || !r.DueFor(at, videoAt, AudioSkewBudget) {
			break
		}
		sent++
		c = c.Next()
	}
	if sent == 0 {
		t.Fatal("nothing was released; audio must track the video cursor, not stop dead")
	}
	// Everything sent must be within the skew budget of the video cursor.
	maxAhead := time.Duration(sent) * 20 * time.Millisecond
	if maxAhead > AudioSkewBudget+40*time.Millisecond {
		t.Errorf("audio ran %v ahead of video, want <= %v", maxAhead, AudioSkewBudget)
	}
}

// End to end: a DVR subscriber with audio enabled keeps its audio across a
// stall, on its own stream, and never behind the video carrier.
func TestDVRAudioSurvivesAStall(t *testing.T) {
	r := NewRegistry(discardLog, Options{
		DVR:      DVROptions{Window: 30 * time.Second, MaxBytes: 1 << 20},
		DVRAudio: true,
	})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	block := make(chan struct{})
	f.setCarBlock(block)
	if _, err := r.SubscribeDVR(id, f, 3000); err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
	}

	const packets = 30
	for g := range 3 {
		ingestKeyframe(t, p, keyframeMsg(t, uint32(g*100), "vp8", "KEY"))
		p.HandleDatagram(chunkDgram(t, false, uint32(g*100+1), 0, 1, "d"))
		for i := range packets / 3 {
			p.HandleDatagram(audioDgram(t, uint32(g*10+i), fmt.Sprintf("op%d-%d", g, i)))
		}
	}
	close(block)

	waitFor(t, 10*time.Second, func() bool {
		n := 0
		for _, rec := range f.carrierRecords(t) {
			if len(rec) >= 2 && rec[1] == wire.TypeAudioFrame {
				n++
			}
		}
		return n == packets
	}, "every audio packet to survive the stall")
}
