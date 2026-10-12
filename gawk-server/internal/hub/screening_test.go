package hub

import (
	"bytes"
	"regexp"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/mediaprobe"
	"github.com/Tuhis/gawk/gawk-server/screening"
)

var epochShape = regexp.MustCompile(`^[0-9a-f]{16}$`)

func adminRow(t *testing.T, r *Registry, id string) AdminBroadcast {
	t.Helper()
	for _, row := range r.AdminStats() {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("no admin row for %s", id)
	return AdminBroadcast{}
}

// R40 CF1 (docs/71 D1): the accessor hands out the cached StreamFrame
// verbatim — the sampler decodes the bytes the publisher sent, not a re-encoding
// — with the frame ID, the session's epoch and the probed codec.
func TestScreeningKeyframeIsVerbatim(t *testing.T) {
	r := NewRegistry(discardLog, Options{BroadcastGrace: 5 * time.Minute})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer p.Close()

	if _, ok := r.ScreeningKeyframe(id); ok {
		t.Fatal("a broadcast with no keyframe yet returned one")
	}
	msg := keyframeMsg(t, 42, "avc1.42E02A", "idr-payload")
	ingestKeyframe(t, p, msg)

	f, ok := r.ScreeningKeyframe(id)
	if !ok {
		t.Fatal("ScreeningKeyframe found nothing after a keyframe")
	}
	if !bytes.Equal(f.Msg, msg) {
		t.Fatalf("frame is not the StreamFrame verbatim:\n got %x\nwant %x", f.Msg, msg)
	}
	if f.FrameID != 42 || f.Codec != mediaprobe.CodecH264 {
		t.Fatalf("frame ID %d codec %q, want 42 / h264", f.FrameID, f.Codec)
	}
	if !epochShape.MatchString(f.PublisherEpoch) {
		t.Fatalf("epoch %q is not 16 lower-case hex characters", f.PublisherEpoch)
	}
	// The broadcast ID is matched as the relay normalizes it everywhere else.
	if _, ok := r.ScreeningKeyframe(bytesLower(id)); !ok {
		t.Fatal("a lower-cased ID did not resolve")
	}
	for _, bad := range []string{"", "NOPE", "ZZZZZZ"} {
		if _, ok := r.ScreeningKeyframe(bad); ok {
			t.Fatalf("ScreeningKeyframe(%q) found a frame", bad)
		}
	}
}

func bytesLower(s string) string { return string(bytes.ToLower([]byte(s))) }

// A new publisher session changes the ETag even when its frame IDs restart at
// the same numbers (docs/71 D1): the epoch is part of the tag, and every claim
// — reclaim and takeover included — mints a new one.
func TestScreeningNewSessionChangesETag(t *testing.T) {
	r := NewRegistry(discardLog, Options{BroadcastGrace: 5 * time.Minute})
	id, p1, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	ingestKeyframe(t, p1, keyframeMsg(t, 1, "avc1.42E02A", "a"))
	f1, _ := r.ScreeningKeyframe(id)
	tag1 := screening.KeyframeETag(f1.PublisherEpoch, f1.FrameID)

	// Reclaim: the same ID, a new session, the same frame ID.
	p1.Close()
	_, p2, err := r.ResumePublish(id)
	if err != nil {
		t.Fatalf("ResumePublish: %v", err)
	}
	if _, ok := r.ScreeningKeyframe(id); ok {
		t.Fatal("the previous session's keyframe survived the reclaim")
	}
	ingestKeyframe(t, p2, keyframeMsg(t, 1, "avc1.42E02A", "a"))
	f2, _ := r.ScreeningKeyframe(id)
	tag2 := screening.KeyframeETag(f2.PublisherEpoch, f2.FrameID)
	if f2.PublisherEpoch == f1.PublisherEpoch || tag2 == tag1 {
		t.Fatalf("reclaim kept the epoch/ETag: %s then %s", tag1, tag2)
	}

	// Takeover (newest publisher wins) is a new session too.
	_, p3, err := r.TakeOverPublish(id)
	if err != nil {
		t.Fatalf("TakeOverPublish: %v", err)
	}
	defer p3.Close()
	ingestKeyframe(t, p3, keyframeMsg(t, 1, "avc1.42E02A", "a"))
	f3, _ := r.ScreeningKeyframe(id)
	if f3.PublisherEpoch == f2.PublisherEpoch {
		t.Fatalf("takeover kept the epoch %s", f3.PublisherEpoch)
	}
}

// The epoch does not depend on telemetry (docs/71 §9.1): with no telemetry
// session handle at all the admin row still names the session, and it keeps
// naming it after the publisher leaves — while the screening route, which
// must not score a frozen frame, answers nothing for the away broadcast.
func TestScreeningEpochWithoutTelemetryAndAfterLeave(t *testing.T) {
	r := NewRegistry(discardLog, Options{BroadcastGrace: 5 * time.Minute})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	ingestKeyframe(t, p, keyframeMsg(t, 3, "vp09.00.10.08", "v"))

	row := adminRow(t, r, id)
	if row.PublisherSessionID != "" {
		t.Fatalf("telemetry handle %q set with telemetry off; the fixture is wrong", row.PublisherSessionID)
	}
	if !epochShape.MatchString(row.PublisherEpoch) {
		t.Fatalf("admin row epoch %q, want 16 lower-case hex", row.PublisherEpoch)
	}
	f, _ := r.ScreeningKeyframe(id)
	if f.PublisherEpoch != row.PublisherEpoch || f.Codec != mediaprobe.CodecVP9 {
		t.Fatalf("frame epoch %q codec %q, want %q / vp9", f.PublisherEpoch, f.Codec, row.PublisherEpoch)
	}

	p.Close()
	if _, ok := r.ScreeningKeyframe(id); ok {
		t.Fatal("an away broadcast's frozen keyframe was served for screening")
	}
	if st := r.Stats().Broadcasts[r.ObfuscateID(id)]; st.CachedKeyframeBytes == 0 {
		t.Fatal("the cache was dropped on leave; the not-found above proves nothing")
	}
	if after := adminRow(t, r, id); after.PublisherEpoch != row.PublisherEpoch || after.PublisherActive {
		t.Fatalf("after leave: epoch %q active %v, want the last session's epoch, inactive", after.PublisherEpoch, after.PublisherActive)
	}
}
