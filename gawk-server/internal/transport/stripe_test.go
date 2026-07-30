package transport

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

func stripedTestConfig(maxSubs int) config.Config {
	return config.Config{
		MaxSubscribers:  maxSubs,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace:  5 * time.Minute,
		ParityDefault:   2,
		StripedDelivery: true,
	}
}

// TestStripeLegDialValidation (docs/35 §5.3): leg dials are validated
// pre-upgrade and strictly — a mis-striped leg is useless to both sides, so
// 400 is the graceful outcome and the viewer stays unstriped.
func TestStripeLegDialValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _ := startTestServerCfg(t, ctx, stripedTestConfig(15))
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")

	base := fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s", port, id)
	for name, params := range map[string]string{
		"width over max":       "?stripe=5&leg=0",
		"width zero":           "?stripe=0&leg=0",
		"member out of range":  "?stripe=2&leg=2",
		"member negative":      "?stripe=2&leg=-1",
		"stripe without leg":   "?stripe=2",
		"leg without stripe":   "?leg=0",
		"garbage width":        "?stripe=x&leg=0",
		"reliable combination": "?stripe=2&leg=0&delivery=reliable",
		"dvr combination":      "?stripe=2&leg=0&delivery=reliable&buffer=2500",
	} {
		if _, sess, err := dialOnce(t, ctx, base+params, clientTLS); err == nil {
			sess.CloseWithError(0, "")
			t.Errorf("%s: dial accepted, want 400 rejection", name)
		}
	}

	// A valid leg dial is accepted.
	leg := dial(t, ctx, base+"?stripe=2&leg=1", clientTLS)
	leg.CloseWithError(0, "")
}

// TestStripeLegRejectedWhenDisabled: -striped-delivery=false refuses leg
// dials outright (the capability bit is never advertised, so a well-behaved
// viewer never gets here — the 400 is the backstop for one that does).
func TestStripeLegRejectedWhenDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := stripedTestConfig(15)
	cfg.StripedDelivery = false
	port, clientTLS, _, _ := startTestServerCfg(t, ctx, cfg)
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")

	url := fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s?stripe=2&leg=0", port, id)
	if _, sess, err := dialOnce(t, ctx, url, clientTLS); err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("leg dial accepted on a striping-disabled relay, want 400")
	}
}

// readDatagramsUntil collects datagrams from a session until the deadline,
// returning them. Datagram loss on loopback is real (see the QUIC test
// gotchas), so callers assert SHAPE (no mismapped ordinal, no forbidden
// type), never exact counts.
func readDatagramsUntil(ctx context.Context, sess *webtransport.Session, until time.Time) [][]byte {
	var got [][]byte
	readCtx, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	for {
		d, err := sess.ReceiveDatagram(readCtx)
		if err != nil {
			return got
		}
		got = append(got, d)
	}
}

// TestStripeLegEndToEnd is the transport-level partition + suppression
// proof: two real leg sessions receive only their ordinal share, the primary
// suppresses deltas on StripeState and resumes on release, and legs receive
// none of the viewer-facing extras (ack datagrams, capability streams,
// keyframes).
func TestStripeLegEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, stripedTestConfig(15))
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")

	base := fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s", port, id)
	primary := dial(t, ctx, base, clientTLS)
	defer primary.CloseWithError(0, "")
	leg0 := dial(t, ctx, base+"?stripe=2&leg=0", clientTLS)
	defer leg0.CloseWithError(0, "")
	leg1 := dial(t, ctx, base+"?stripe=2&leg=1", clientTLS)
	defer leg1.CloseWithError(0, "")
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 3 }, "all three sessions registered")

	// Engage: the suppression datagram is unreliable, so re-send until the
	// relay's stats show it landed (the viewer's 1 Hz refresh, compressed).
	striped, err := wire.AppendStripeState(nil, wire.StripeState{Striped: true, StripeN: 2})
	if err != nil {
		t.Fatalf("AppendStripeState: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_ = primary.SendDatagram(striped)
		for _, b := range r.Stats().Broadcasts {
			if b.StripedPrimaries == 1 {
				return true
			}
		}
		return false
	}, "primary suppression armed")

	// One striped GOP: an 8-chunk delta frame plus both parity symbols.
	frame := encodeFrame(t, 5, false, 8)
	for _, d := range frame {
		if err := pub.SendDatagram(d); err != nil {
			t.Fatalf("SendDatagram: %v", err)
		}
	}
	for idx := uint8(0); idx < 2; idx++ {
		p, err := wire.AppendParityChunk(nil, wire.ParityChunkHeader{
			FrameID: 5, ParityIndex: idx, ChunkCount: 8, FrameBytes: 64,
		}, []byte{1, 2, 3, 4})
		if err != nil {
			t.Fatalf("AppendParityChunk: %v", err)
		}
		if err := pub.SendDatagram(p); err != nil {
			t.Fatalf("SendDatagram(parity): %v", err)
		}
	}

	until := time.Now().Add(2 * time.Second)
	legDgrams := [2][][]byte{
		readDatagramsUntil(ctx, leg0, until),
		readDatagramsUntil(ctx, leg1, until),
	}
	primaryDgrams := readDatagramsUntil(ctx, primary, time.Now().Add(200*time.Millisecond))

	sawShare := false
	for j, dgrams := range legDgrams {
		for _, d := range dgrams {
			if len(d) < 2 {
				continue
			}
			switch d[1] {
			case wire.TypeVideoChunk:
				h, _, err := wire.ParseVideoChunk(d)
				if err != nil {
					t.Fatalf("leg %d: ParseVideoChunk: %v", j, err)
				}
				if int(h.ChunkIndex)%2 != j {
					t.Errorf("leg %d received chunk index %d (mismapped)", j, h.ChunkIndex)
				}
				sawShare = true
			case wire.TypeParityChunk:
				h, _, err := wire.ParseParityChunk(d)
				if err != nil {
					t.Fatalf("leg %d: ParseParityChunk: %v", j, err)
				}
				if int(wire.StripeOrdinal(0, h.ChunkCount, int(h.ParityIndex)))%2 != j {
					t.Errorf("leg %d received parity index %d (mismapped)", j, h.ParityIndex)
				}
				sawShare = true
			case wire.TypeDeliveryAck, wire.TypeViewerCount, wire.TypeDecoderConfig,
				wire.TypeAudioFrame, wire.TypeAudioConfig, wire.TypeClockMapping:
				t.Errorf("leg %d received a 0x%02x datagram — legs carry deltas only", j, d[1])
			}
		}
	}
	if !sawShare {
		t.Fatal("neither leg received any delta datagram — the share filter is not routing")
	}
	// The suppressed primary must not have received the frame's deltas. Its
	// pre-suppression traffic (DeliveryAck re-announces, viewer count) is
	// fine; a video or parity datagram is not.
	for _, d := range primaryDgrams {
		if len(d) >= 2 && (d[1] == wire.TypeVideoChunk || d[1] == wire.TypeParityChunk) {
			t.Error("suppressed primary received a delta datagram")
		}
	}

	// Legs never receive server uni streams: no capabilities, no telemetry
	// hello, and — after a keyframe — no keyframe stream either.
	sendKeyframeStream(t, pub, buildStreamKeyframe(t, 6, "avc1.42E02A", 256))
	for j, leg := range []*webtransport.Session{leg0, leg1} {
		streamCtx, streamCancel := context.WithTimeout(ctx, 700*time.Millisecond)
		if str, err := leg.AcceptUniStream(streamCtx); err == nil {
			prologue := make([]byte, 2)
			_, _ = io.ReadFull(str, prologue)
			t.Errorf("leg %d received a uni stream (type 0x%02x), want none", j, prologue[1])
		}
		streamCancel()
	}

	// Release: deltas resume on the primary.
	unstriped, err := wire.AppendStripeState(nil, wire.StripeState{})
	if err != nil {
		t.Fatalf("AppendStripeState: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_ = primary.SendDatagram(unstriped)
		for _, b := range r.Stats().Broadcasts {
			return b.StripedPrimaries == 0
		}
		return false
	}, "primary suppression released")
	resumed := false
	deadline := time.Now().Add(5 * time.Second)
	for fid := uint32(20); time.Now().Before(deadline) && !resumed; fid++ {
		for _, d := range encodeFrame(t, fid, false, 2) {
			_ = pub.SendDatagram(d)
		}
		for _, d := range readDatagramsUntil(ctx, primary, time.Now().Add(300*time.Millisecond)) {
			if len(d) >= 2 && d[1] == wire.TypeVideoChunk {
				resumed = true
			}
		}
	}
	if !resumed {
		t.Fatal("primary never resumed receiving deltas after the unstripe")
	}

	stats := r.Stats()
	if stats.Totals.StripeSuppressedDatagrams == 0 {
		t.Error("StripeSuppressedDatagrams = 0, want > 0 while the primary was striped")
	}
	if stats.Totals.StripeLegs != 2 {
		t.Errorf("StripeLegs = %d, want 2", stats.Totals.StripeLegs)
	}
	if stats.Totals.Subscribers != 3 {
		t.Errorf("Subscribers = %d, want 3 (legs count against caps)", stats.Totals.Subscribers)
	}
	for _, b := range stats.Broadcasts {
		if b.ViewersGlobal != 1 {
			t.Errorf("ViewersGlobal = %d, want 1 — legs are not viewers", b.ViewersGlobal)
		}
	}
}

// TestStripeCapabilityBit: the version-skew gate. Striping on advertises the
// bit; striping off with parity on keeps R29's exact bytes; both off sends
// no capabilities at all (pre-R29 wire).
func TestStripeCapabilityBit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		striped   bool
		parity    int
		wantFlags uint16
		wantSent  bool
	}{
		{"both on", true, 2, wire.CapParityChunks | wire.CapStripedDelivery, true},
		{"striping only", true, 0, wire.CapStripedDelivery, true},
		{"parity only (R29 bytes)", false, 2, wire.CapParityChunks, true},
		{"both off (pre-R29 wire)", false, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg := stripedTestConfig(15)
			cfg.StripedDelivery = tc.striped
			cfg.ParityDefault = tc.parity
			port, clientTLS, _, _ := startTestServerCfg(t, ctx, cfg)
			pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
			defer pub.CloseWithError(0, "")
			sub := dialSubscriber(t, ctx, port, id, clientTLS)
			defer sub.CloseWithError(0, "")

			streamCtx, streamCancel := context.WithTimeout(ctx, 2*time.Second)
			defer streamCancel()
			for {
				str, err := sub.AcceptUniStream(streamCtx)
				if err != nil {
					if tc.wantSent {
						t.Fatal("no capabilities stream arrived")
					}
					return // both off: silence is the assertion
				}
				payload, err := io.ReadAll(str)
				if err != nil {
					t.Fatalf("read uni stream: %v", err)
				}
				if len(payload) >= 2 && payload[1] == wire.TypeRelayCapabilities {
					if !tc.wantSent {
						t.Fatal("capabilities sent on a both-off relay")
					}
					caps, err := wire.ParseRelayCapabilities(payload)
					if err != nil {
						t.Fatalf("ParseRelayCapabilities: %v", err)
					}
					if caps.Flags != tc.wantFlags {
						t.Fatalf("flags = %#04x, want %#04x", caps.Flags, tc.wantFlags)
					}
					return
				}
			}
		})
	}
}

// TestStripeStateIgnoredWhenDisabled: with -striped-delivery=false a 0x10
// datagram is discarded like any other unknown datagram — no suppression, no
// transition count.
func TestStripeStateIgnoredWhenDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := stripedTestConfig(15)
	cfg.StripedDelivery = false
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, cfg)
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")
	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	defer sub.CloseWithError(0, "")
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

	striped, err := wire.AppendStripeState(nil, wire.StripeState{Striped: true, StripeN: 2})
	if err != nil {
		t.Fatalf("AppendStripeState: %v", err)
	}
	for i := 0; i < 10; i++ {
		_ = sub.SendDatagram(striped)
		time.Sleep(20 * time.Millisecond)
	}
	stats := r.Stats()
	if stats.Totals.StripedPrimaries != 0 || stats.Totals.StripeTransitions != 0 {
		t.Fatalf("striping state changed on a disabled relay: primaries=%d transitions=%d",
			stats.Totals.StripedPrimaries, stats.Totals.StripeTransitions)
	}
}
