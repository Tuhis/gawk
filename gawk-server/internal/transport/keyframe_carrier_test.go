package transport

// R84 BR1 (docs/88 D2, §5.2, K3): a publisher's keyframe carrier over a real
// quic-go session, the capability bit beside RU1's, and the off switch.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// keyframeCarrierHead is a keyframe carrier's first bytes: the prologue and
// the keyframe's StreamFrame message.
func keyframeCarrierHead(keyframe []byte) []byte {
	return append(wire.AppendKeyframeCarrierPrologue(nil), keyframe...)
}

// The keyframe-carrier bit goes to publishers beside RU1's, and only with
// -uplink-carriers on; a viewer's capabilities carry neither (docs/88 BR1).
func TestKeyframeCarriersCapabilityBesideUplinkCarriers(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("uplink-carriers=%v", on), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			port, clientTLS, _, _ := startTestServerCfg(t, ctx, uplinkTestConfig(on))
			pub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
			defer pub.CloseWithError(0, "")
			id, _, leftovers := readPublisherHandshakeMsgs(t, ctx, pub)
			caps, raw, ok := relayCapabilities(t, ctx, pub, leftovers)
			if !ok {
				t.Fatal("publisher got no RelayCapabilities")
			}
			kf, up := caps.Flags&wire.CapKeyframeCarriers != 0, caps.Flags&wire.CapUplinkCarriers != 0
			if kf != on || up != on {
				t.Fatalf("publisher flags = %#04x: keyframe carriers %v, uplink carriers %v, want both %v", caps.Flags, kf, up, on)
			}
			want := "010f000302" // G6: the pre-R73 bytes for this config
			if on {
				want = "010f000f02" // parity 2, striping, both carrier bits
			}
			if got := fmt.Sprintf("%x", raw); got != want {
				t.Fatalf("publisher capabilities = %s, want %s", got, want)
			}

			sub := dialSubscriber(t, ctx, port, id, clientTLS)
			defer sub.CloseWithError(0, "")
			caps, _, ok = relayCapabilities(t, ctx, sub, nil)
			if !ok {
				t.Fatal("viewer got no RelayCapabilities")
			}
			if caps.Flags&(wire.CapKeyframeCarriers|wire.CapUplinkCarriers) != 0 {
				t.Fatalf("viewer flags = %#04x carry a carrier bit", caps.Flags)
			}
		})
	}
}

// nextKeyframe accepts the subscriber's server-initiated uni streams until one
// is a StreamFrame, skipping the session's control messages.
func nextKeyframe(t *testing.T, ctx context.Context, sub *webtransport.Session) []byte {
	t.Helper()
	for {
		str, err := sub.AcceptUniStream(ctx)
		if err != nil {
			t.Fatalf("AcceptUniStream: %v", err)
		}
		data, err := io.ReadAll(str)
		if err != nil {
			t.Fatalf("read uni stream: %v", err)
		}
		if len(data) >= 2 && data[1] == wire.TypeStreamFrame {
			return data
		}
	}
}

// Over a real session the keyframe at the head of a GOP stream reaches a
// viewer as a keyframe stream while the publisher holds the GOP stream open,
// and the records after it reach the viewer as the datagrams they replace.
func TestKeyframeCarrierOverRealSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, uplinkTestConfig(true))
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")
	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	defer sub.CloseWithError(0, "")
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

	kf := buildStreamKeyframe(t, 10, "avc1.42E02A", 20_000)
	car, err := pub.OpenUniStream()
	if err != nil {
		t.Fatalf("OpenUniStream: %v", err)
	}
	if _, err := car.Write(keyframeCarrierHead(kf)); err != nil {
		t.Fatalf("write head: %v", err)
	}
	recvCtx, recvCancel := context.WithTimeout(ctx, 5*time.Second)
	defer recvCancel()
	if got := nextKeyframe(t, recvCtx, sub); !bytes.Equal(got, kf) {
		t.Fatalf("viewer's keyframe differs: %d bytes, want %d", len(got), len(kf))
	}

	recs := uplinkRecords(t, 11, 16)
	if _, err := car.Write(carrierStreamBytes(t, recs)[wire.CarrierPrologueSize:]); err != nil {
		t.Fatalf("write records: %v", err)
	}
	got := map[string]bool{}
	for len(got) < len(recs) {
		d, err := sub.ReceiveDatagram(recvCtx)
		if err != nil {
			t.Fatalf("viewer received %d of %d records: %v", len(got), len(recs), err)
		}
		if len(d) >= 2 && d[1] == wire.TypeVideoChunk {
			got[string(d)] = true
		}
	}
	for i, rec := range recs {
		if !got[string(rec)] {
			t.Fatalf("record %d never reached the viewer verbatim", i)
		}
	}
	st := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if st.UplinkCarriers != 1 || st.UplinkRecords != uint64(len(recs)) || st.KeyframeStreamsIn != 1 || st.BadDatagrams != 0 {
		t.Fatalf("carriers %d records %d keyframes %d bad %d, want 1 / %d / 1 / 0",
			st.UplinkCarriers, st.UplinkRecords, st.KeyframeStreamsIn, st.BadDatagrams, len(recs))
	}
	car.Close()
}

// Keyframe carriers hold uplink-carrier slots, never one of
// acceptKeyframeStreams' four: with MaxUplinkCarriers of them held open, four
// keyframe streams held open at once are all still ingested, and a fourth
// carrier of either kind is refused.
func TestKeyframeCarriersTakeNoKeyframeStreamSlot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, uplinkTestConfig(true))
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")
	stats := func() hub.Stats { return r.Stats().Broadcasts[r.ObfuscateID(id)] }

	var held []*webtransport.SendStream
	for i := range hub.MaxUplinkCarriers {
		car, err := pub.OpenUniStream()
		if err != nil {
			t.Fatalf("OpenUniStream: %v", err)
		}
		if _, err := car.Write(keyframeCarrierHead(buildStreamKeyframe(t, uint32(100*i), "", 500))); err != nil {
			t.Fatalf("write carrier %d: %v", i, err)
		}
		held = append(held, car)
	}
	waitFor(t, 5*time.Second, func() bool {
		st := stats()
		return st.UplinkCarriers == hub.MaxUplinkCarriers && st.KeyframeStreamsIn == hub.MaxUplinkCarriers
	}, "keyframe carriers open")

	// Four keyframe streams, each held open after half its message, then
	// finished together: all four are read only if no slot was taken.
	var streams []*webtransport.SendStream
	var rest [][]byte
	for i := range maxConcurrentKeyframeStreams {
		msg := buildStreamKeyframe(t, uint32(1000+i), "", 500)
		str, err := pub.OpenUniStream()
		if err != nil {
			t.Fatalf("OpenUniStream: %v", err)
		}
		if _, err := str.Write(msg[:len(msg)/2]); err != nil {
			t.Fatalf("write keyframe stream %d: %v", i, err)
		}
		streams, rest = append(streams, str), append(rest, msg[len(msg)/2:])
	}
	time.Sleep(200 * time.Millisecond) // let the relay accept all four before any finishes
	for i, str := range streams {
		if _, err := str.Write(rest[i]); err != nil {
			t.Fatalf("finish keyframe stream %d: %v", i, err)
		}
		str.Close()
	}
	waitFor(t, 5*time.Second, func() bool {
		return stats().KeyframeStreamsIn == hub.MaxUplinkCarriers+maxConcurrentKeyframeStreams
	}, "all four keyframe streams ingested beside the keyframe carriers")

	fourth, err := pub.OpenUniStream()
	if err != nil {
		t.Fatalf("OpenUniStream: %v", err)
	}
	if _, err := fourth.Write(keyframeCarrierHead(buildStreamKeyframe(t, 5000, "", 500))); err != nil {
		t.Fatalf("write fourth: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return stats().UplinkCarriersRejected == 1 }, "fourth carrier rejected")
	if st := stats(); st.KeyframeStreamsIn != hub.MaxUplinkCarriers+maxConcurrentKeyframeStreams || st.BadDatagrams != 0 {
		t.Fatalf("keyframes %d bad %d after the refusal", st.KeyframeStreamsIn, st.BadDatagrams)
	}
	for _, h := range held {
		h.Close()
	}
}

// G6: with -uplink-carriers=false a 0x1F stream is what any unknown stream is
// to the pre-R73 relay: bad input, reset, nothing cached or forwarded.
func TestKeyframeCarrierRejectedWithCarriersOff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, uplinkTestConfig(false))
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")
	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	defer sub.CloseWithError(0, "")
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

	str, err := pub.OpenUniStream()
	if err != nil {
		t.Fatalf("OpenUniStream: %v", err)
	}
	stream := keyframeCarrierHead(buildStreamKeyframe(t, 1, "avc1.42E02A", 2000))
	stream = append(stream, carrierStreamBytes(t, uplinkRecords(t, 2, 3))[wire.CarrierPrologueSize:]...)
	if _, err := str.Write(stream); err != nil {
		t.Fatalf("write: %v", err)
	}
	str.Close()
	waitFor(t, 5*time.Second, func() bool {
		return r.Stats().Broadcasts[r.ObfuscateID(id)].BadDatagrams == 1
	}, "0x1F stream counted as bad input")
	st := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if st.KeyframeStreamsIn != 0 || st.FramesRelayed != 0 || st.UplinkRecords != 0 || st.DatagramsRelayed != 0 {
		t.Fatalf("flag off ingested the keyframe carrier: %+v", st)
	}
}
