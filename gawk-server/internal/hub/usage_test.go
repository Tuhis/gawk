package hub

import (
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/clientinfo"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// endRecorder collects OnOriginEnded reports.
type endRecorder struct {
	mu   sync.Mutex
	ends []BroadcastEnd
}

func (e *endRecorder) record(b BroadcastEnd) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ends = append(e.ends, b)
}

func (e *endRecorder) all() []BroadcastEnd {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]BroadcastEnd(nil), e.ends...)
}

// R59 (docs/61 D5): an origin broadcast reports its life, creation to its
// publisher's last disconnect, when its hub is removed, and the peak global
// audience it reached. Stripe legs are one viewer's extra connections.
func TestBroadcastEndObservesDurationAndPeak(t *testing.T) {
	var rec endRecorder
	r := NewRegistry(discardLog, Options{OnOriginEnded: rec.record})
	t0 := time.Now()
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := r.Subscribe(id, &fakeSender{})
	b, _ := r.Subscribe(id, &fakeSender{})
	leg, err := r.SubscribeStripeLeg(id, &fakeSender{}, StripeLeg{N: 2, Member: 0, Owner: "aabbccdd00112233"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	r.PumpViewerCounts(time.Now())
	a.Close()
	b.Close()
	leg.Close()
	r.PumpViewerCounts(time.Now())

	time.Sleep(5 * time.Millisecond)
	pub.Close()
	leftAt := time.Now()
	// Grace: the broadcast's life ended at the disconnect, not at the GC.
	time.Sleep(20 * time.Millisecond)
	r.EndBroadcast(id)

	ends := rec.all()
	if len(ends) != 1 {
		t.Fatalf("OnOriginEnded fired %d times, want 1", len(ends))
	}
	if ends[0].PeakViewers != 2 {
		t.Errorf("PeakViewers = %d, want 2 (the leg is not a viewer, the drop to 0 is not the peak)", ends[0].PeakViewers)
	}
	if d := ends[0].Duration; d < 5*time.Millisecond || d > leftAt.Sub(t0) {
		t.Errorf("Duration = %v, want between 5ms and %v: the life up to the disconnect, not the grace after it", d, leftAt.Sub(t0))
	}
}

// A forced removal of a live publisher (operator kill) ends the life now; an
// edge hub is derived state and reports nothing.
func TestBroadcastEndForcedAndEdge(t *testing.T) {
	var rec endRecorder
	r := NewRegistry(discardLog, Options{OnOriginEnded: rec.record})
	id, _, err := r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	if !r.TerminateBroadcast(id, uint32(wire.CloseCodeTerminatedByOperator), "banned") {
		t.Fatal("TerminateBroadcast = false")
	}
	if n := len(rec.all()); n != 1 {
		t.Fatalf("forced removal reported %d ends, want 1", n)
	}

	edgeID, edgePub, err := r.EdgePublish("K7XQ2M")
	if err != nil {
		t.Fatal(err)
	}
	edgePub.Close()
	r.EndBroadcast(edgeID)
	if n := len(rec.all()); n != 1 {
		t.Errorf("an edge hub's removal reported an end (%d total)", n)
	}
}

// R59 (docs/61 D2, D6): the codec comes from the DecoderConfig and the coded
// height from the media — the AVCC extradata for H.264, the keyframe header
// for VP8 — and a new publisher session re-probes both.
func TestHubProbesCodecAndHeightFromMedia(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	pub.SetClient(clientinfo.Info{App: "desktop", OS: "windows", Browser: clientinfo.Unknown})
	stats := func() Stats { return r.Stats().Broadcasts[r.ObfuscateID(id)] }

	// H.264 in avc format: the 1080p SPS rides in the extradata.
	sps, _ := hex.DecodeString("67640028acd940780227e5c044000003000400000300f03c60c658")
	avcc := append([]byte{1, sps[1], sps[2], sps[3], 0xff, 0xe1, 0, byte(len(sps))}, sps...)
	cfg, err := wire.AppendDecoderConfig(nil, wire.DecoderConfig{Codec: "avc1.640028", Extradata: append(avcc, 0)})
	if err != nil {
		t.Fatal(err)
	}
	pub.HandleDatagram(cfg)
	if s := stats(); s.Codec != "h264" || s.CodedHeight != 1080 {
		t.Fatalf("after the H.264 config: codec=%q height=%d, want h264/1080", s.Codec, s.CodedHeight)
	}
	// An AVCC keyframe has no SPS; its bytes must not overwrite the height.
	ingestKeyframe(t, pub, keyframeMsg(t, 1, "", "\x00\x00\x01\x67\x42\xc0\x1f\xd9\x00\x50\x05\xbb"))
	if s := stats(); s.CodedHeight != 1080 {
		t.Errorf("an AVCC keyframe changed the height to %d", s.CodedHeight)
	}
	if s := stats(); s.Client.App != "desktop" || s.Client.OS != "windows" {
		t.Errorf("client = %+v, want desktop/windows", s.Client)
	}

	// A new publisher session switches to VP8: the config in the keyframe
	// names the codec, the VP8 header carries 1280x720.
	pub.Close()
	id, pub, err = r.ResumePublish(id)
	if err != nil {
		t.Fatal(err)
	}
	if s := stats(); s.Codec != "" || s.CodedHeight != 0 {
		t.Fatalf("a new session kept the old media labels: %q/%d", s.Codec, s.CodedHeight)
	}
	vp8, _ := hex.DecodeString("3077019d012a0005d0020007088585888584880202224c71")
	ingestKeyframe(t, pub, keyframeMsg(t, 1, "vp8", string(vp8)))
	if s := stats(); s.Codec != "vp8" || s.CodedHeight != 720 {
		t.Errorf("after the VP8 keyframe: codec=%q height=%d, want vp8/720", s.Codec, s.CodedHeight)
	}
}
