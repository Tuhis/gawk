package media

import (
	"slices"
	"testing"
	"time"
)

// Review finding 2: a sender (or attacker) advancing the sequence number by
// just under MaxJump per packet must not grow the hole table or the work
// per packet without bound.
func TestD6AdversarialSequenceJumpsStayBounded(t *testing.T) {
	cfg := DefaultReorderConfig()
	r := NewReorder(cfg)
	seq := uint16(0)
	start := time.Now()
	for i := 0; i < 2000; i++ {
		r.Push(Packet{Seq: seq, Timestamp: uint32(i), Payload: []byte{0x41, 0x80}}, t0.Add(time.Duration(i)*time.Millisecond))
		seq += uint16(cfg.MaxJump - 1)
		if len(r.holes) > cfg.MaxNACK {
			t.Fatalf("packet %d: %d holes, bound %d", i, len(r.holes), cfg.MaxNACK)
		}
		if span := r.highest - r.next; span > int64(cfg.MaxPackets) {
			t.Fatalf("packet %d: span %d, bound %d", i, span, cfg.MaxPackets)
		}
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("2000 adversarial packets took %v", took)
	}
}

// Review finding 4 (D6: the RTT sample is NACK → retransmit): a packet that
// is merely reordered by a millisecond is never NACKed, so it can't become
// an RTT sample that pins the window at its floor.
func TestD6ReorderedPacketIsNeitherNACKedNorSampled(t *testing.T) {
	r := NewReorder(DefaultReorderConfig())
	fs := &frameStream{}
	pk := fs.packets(bigP(1))
	res := pushAll(r, append([]Packet{pk[0]}, pk[2:]...), t0)
	if len(res.NACK) != 0 {
		t.Fatalf("NACKed on the hole's first appearance: %v", res.NACK)
	}
	got := r.Push(pk[1], t0.Add(time.Millisecond))
	if len(got.Frames) != 1 {
		t.Fatalf("reordered frame not released: %+v", got)
	}
	if n := len(r.Tick(t0.Add(50 * time.Millisecond)).NACK); n != 0 || r.NACKsSent() != 0 || r.RTT().Samples() != 0 {
		t.Fatalf("a reordered packet was NACKed (%d) or sampled (%d)", r.NACKsSent(), r.RTT().Samples())
	}
}

// Review finding 4: a lost packet is NACKed once the reorder tolerance
// (3 ms) has passed, and its retransmit is timed from that NACK.
func TestD6LostPacketIsNACKedAfterTheReorderTolerance(t *testing.T) {
	r := NewReorder(DefaultReorderConfig())
	fs := &frameStream{}
	pk := fs.packets(bigP(1))
	pushAll(r, append([]Packet{pk[0]}, pk[2:]...), t0)
	dl, ok := r.Deadline()
	if !ok || !dl.Equal(t0.Add(3*time.Millisecond)) {
		t.Fatalf("deadline %v %v, want the NACK due at +3ms", dl, ok)
	}
	if res := r.Tick(t0.Add(3 * time.Millisecond)); !slices.Equal(res.NACK, []uint16{pk[1].Seq}) {
		t.Fatalf("NACK %v at +3ms, want [%d]", res.NACK, pk[1].Seq)
	}
	r.Push(pk[1], t0.Add(43*time.Millisecond))
	if r.RTT().RTT() != 40*time.Millisecond {
		t.Fatalf("RTT %v, want 40ms (NACK → retransmit)", r.RTT().RTT())
	}
}
