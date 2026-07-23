package hub

// R21 DV1 (docs/26): the per-broadcast DVR ring and the cursors into it.
// Everything here is pure — no sessions, no I/O — because the ring's whole job
// is to be a correct data structure under concurrent append and N readers.

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"
)

func gopBytes(n int) []byte { return bytes.Repeat([]byte{byte(n)}, 16) }

func recordBytes(gop, idx int) []byte {
	return []byte(fmt.Sprintf("g%03d-r%03d", gop, idx))
}

// appendGop writes one complete GOP: a keyframe, then n delta records.
func appendGop(r *DVRRing, seq int, records int, at time.Time) {
	r.AppendKeyframe(gopBytes(seq), at)
	for i := range records {
		r.AppendRecord(recordBytes(seq, i), at)
	}
}

func TestDVRRingReplaysGopsVerbatim(t *testing.T) {
	r := NewDVRRing(DVROptions{Window: time.Minute, MaxBytes: 1 << 20})
	t0 := time.Now()
	// Take the cursor while GOP 0 is the newest, then let the broadcast run
	// on: that is exactly the state a subscriber is in after a stall, and it
	// is the only way to hold a cursor into history (a joiner always starts at
	// the live edge).
	appendGop(r, 0, 4, t0)
	c := r.NewCursor()
	for g := 1; g < 3; g++ {
		appendGop(r, g, 4, t0)
	}

	for g := range 3 {
		kf, ok := r.Keyframe(c)
		if !ok {
			t.Fatalf("gop %d: no keyframe at cursor", g)
		}
		if !bytes.Equal(kf, gopBytes(g)) {
			t.Fatalf("gop %d keyframe = %x, want %x", g, kf, gopBytes(g))
		}
		c = c.AtFirstRecord()
		for i := range 4 {
			rec, ok := r.Record(c)
			if !ok {
				t.Fatalf("gop %d record %d missing", g, i)
			}
			if !bytes.Equal(rec, recordBytes(g, i)) {
				t.Fatalf("gop %d record %d = %q, want %q", g, i, rec, recordBytes(g, i))
			}
			c = c.Next()
		}
		if _, ok := r.Record(c); ok {
			t.Fatalf("gop %d yielded a 5th record", g)
		}
		var advanced bool
		c, advanced = r.NextGop(c)
		if g < 2 && !advanced {
			t.Fatalf("gop %d: cursor would not advance to the next gop", g)
		}
	}
}

func TestDVRRingOwnsItsBytes(t *testing.T) {
	// The ring must not alias the caller's buffer. Today's per-subscriber queue
	// already assumes per-datagram ownership; DV1 makes that checked rather
	// than assumed, because a ring holds bytes for seconds rather than
	// milliseconds and would turn a reused buffer into silent corruption.
	r := NewDVRRing(DVROptions{Window: time.Minute, MaxBytes: 1 << 20})
	t0 := time.Now()
	kf := []byte("KEYFRAME")
	rec := []byte("RECORD")
	r.AppendKeyframe(kf, t0)
	r.AppendRecord(rec, t0)

	for i := range kf {
		kf[i] = 'X'
	}
	for i := range rec {
		rec[i] = 'X'
	}

	c := r.NewCursor()
	got, _ := r.Keyframe(c)
	if !bytes.Equal(got, []byte("KEYFRAME")) {
		t.Errorf("keyframe = %q, want KEYFRAME — the ring aliased the caller's buffer", got)
	}
	gotRec, _ := r.Record(c.AtFirstRecord())
	if !bytes.Equal(gotRec, []byte("RECORD")) {
		t.Errorf("record = %q, want RECORD — the ring aliased the caller's buffer", gotRec)
	}
}

func TestDVRRingEvictsByWindow(t *testing.T) {
	r := NewDVRRing(DVROptions{Window: 2 * time.Second, MaxBytes: 1 << 20})
	t0 := time.Now()
	// One GOP per 500 ms, six of them (t0 … t0+2500 ms). The window is measured
	// against the newest GOP and the edge is inclusive, so GOP 1 at t0+500 is
	// exactly 2000 ms old and stays; only GOP 0 is dropped.
	for g := range 6 {
		appendGop(r, g, 2, t0.Add(time.Duration(g)*500*time.Millisecond))
	}
	if got := r.OldestGopSeq(); got != 1 {
		t.Errorf("OldestGopSeq = %d, want 1 (2 s window, inclusive edge)", got)
	}
	// One more GOP pushes the frontier along by exactly one.
	appendGop(r, 6, 2, t0.Add(3*time.Second))
	if got := r.OldestGopSeq(); got != 2 {
		t.Errorf("OldestGopSeq = %d, want 2 after the window slid", got)
	}
}

func TestDVRRingEvictsByBytesWhicheverBindsFirst(t *testing.T) {
	// A byte cap well below what the window would allow: it must bind instead.
	// This is the bound that protects the pod from a high-bitrate broadcaster.
	r := NewDVRRing(DVROptions{Window: time.Hour, MaxBytes: 200})
	t0 := time.Now()
	for g := range 20 {
		appendGop(r, g, 4, t0)
	}
	if r.Bytes() > 200 {
		t.Errorf("ring holds %d bytes, want <= 200 — the byte cap did not bind", r.Bytes())
	}
	if r.OldestGopSeq() == 0 {
		t.Error("nothing was evicted despite the byte cap")
	}
}

func TestDVRRingCursorFallsOffTheTail(t *testing.T) {
	r := NewDVRRing(DVROptions{Window: time.Hour, MaxBytes: 1 << 20})
	t0 := time.Now()
	appendGop(r, 0, 2, t0)
	c := r.NewCursor()

	// Evict everything the cursor pointed at.
	for g := 1; g < 40; g++ {
		appendGop(r, g, 2, t0)
	}
	r.EvictTo(30)

	if _, ok := r.Keyframe(c); ok {
		t.Fatal("a cursor into an evicted GOP still resolved — that is a read of freed history")
	}
	if !r.FellOffTail(c) {
		t.Fatal("FellOffTail did not report the stale cursor")
	}
	// Resync lands on the newest complete GOP, which is the only decodable
	// entry point (docs/26 Decision 4).
	c = r.ResyncCursor()
	kf, ok := r.Keyframe(c)
	if !ok {
		t.Fatal("resynced cursor does not resolve")
	}
	if !bytes.Equal(kf, gopBytes(39)) {
		t.Errorf("resync landed on %x, want the newest GOP %x", kf, gopBytes(39))
	}
}

func TestDVRRingLagTracksTheLiveEdge(t *testing.T) {
	r := NewDVRRing(DVROptions{Window: time.Hour, MaxBytes: 1 << 20})
	t0 := time.Now()
	appendGop(r, 0, 2, t0)
	c := r.NewCursor()
	if lag := r.LagMs(c, t0); lag != 0 {
		t.Errorf("fresh cursor lag = %d ms, want 0", lag)
	}
	appendGop(r, 1, 2, t0.Add(2*time.Second))
	if lag := r.LagMs(c, t0.Add(2*time.Second)); lag != 2000 {
		t.Errorf("lag = %d ms, want 2000 — a cursor two seconds behind the newest GOP", lag)
	}
}

func TestDVRRingConcurrentAppendAndReaders(t *testing.T) {
	// The shape the drain actually runs in: one appender under the registry
	// lock, N drains reading their own cursors. Run under -race.
	r := NewDVRRing(DVROptions{Window: time.Hour, MaxBytes: 1 << 22})
	t0 := time.Now()
	const gops = 60
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for g := range gops {
			appendGop(r, g, 8, t0)
		}
		close(stop)
	}()

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := r.NewCursor()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, ok := r.Record(c); ok {
					c = c.Next()
					continue
				}
				if next, ok := r.NextGop(c); ok {
					c = next.AtFirstRecord()
				}
			}
		}()
	}
	wg.Wait()
}
