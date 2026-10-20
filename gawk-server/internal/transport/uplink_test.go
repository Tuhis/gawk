package transport

// R73 RU1 (docs/75 D2, D10): the publisher's uplink carriers over a real
// quic-go session, the capability bit, the off switch and the receive
// windows.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// uplinkTestConfig is the production shape for the uplink knobs: carriers
// on, windows for 50 Mbps, parity and striping at their defaults.
func uplinkTestConfig(carriers bool) config.Config {
	return config.Config{
		MaxSubscribers:        15,
		MaxIdleTimeout:        30 * time.Second,
		KeepAlivePeriod:       10 * time.Second,
		BroadcastGrace:        5 * time.Minute,
		ParityDefault:         2,
		StripedDelivery:       true,
		UplinkCarriers:        carriers,
		UplinkMaxBitrateBytes: 50_000_000 / 8,
	}
}

// relayCapabilities hunts a session's server-initiated streams for the
// RelayCapabilities message; leftovers are earlier messages already consumed
// by a handshake helper. ok=false when none arrives within the timeout.
func relayCapabilities(t *testing.T, ctx context.Context, sess *webtransport.Session, leftovers [][]byte) (wire.RelayCapabilities, []byte, bool) {
	t.Helper()
	check := func(msg []byte) (wire.RelayCapabilities, bool) {
		if len(msg) < 2 || msg[1] != wire.TypeRelayCapabilities {
			return wire.RelayCapabilities{}, false
		}
		c, err := wire.ParseRelayCapabilities(msg)
		if err != nil {
			t.Fatalf("ParseRelayCapabilities: %v", err)
		}
		return c, true
	}
	for _, m := range leftovers {
		if c, ok := check(m); ok {
			return c, m, true
		}
	}
	streamCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		str, err := sess.AcceptUniStream(streamCtx)
		if err != nil {
			return wire.RelayCapabilities{}, nil, false
		}
		msg, err := io.ReadAll(str)
		if err != nil {
			t.Fatalf("read uni stream: %v", err)
		}
		if c, ok := check(msg); ok {
			return c, msg, true
		}
	}
}

// The bit goes to publishers, and only with -uplink-carriers on; a viewer's
// capabilities never carry it (docs/75 D2).
func TestUplinkCarriersCapabilityGoesToPublishersOnly(t *testing.T) {
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
			if got := caps.Flags&wire.CapUplinkCarriers != 0; got != on {
				t.Fatalf("publisher flags = %#04x, uplink bit %v, want %v", caps.Flags, got, on)
			}
			if !on {
				// G6: the pre-R73 bytes for this config (parity 2, striping).
				if got, want := fmt.Sprintf("%x", raw), "010f000302"; got != want {
					t.Fatalf("publisher capabilities = %s, want the pre-R73 %s", got, want)
				}
			}

			sub := dialSubscriber(t, ctx, port, id, clientTLS)
			defer sub.CloseWithError(0, "")
			caps, _, ok = relayCapabilities(t, ctx, sub, nil)
			if !ok {
				t.Fatal("viewer got no RelayCapabilities")
			}
			if caps.Flags&wire.CapUplinkCarriers != 0 {
				t.Fatalf("viewer flags = %#04x carry the uplink-carriers bit", caps.Flags)
			}
		})
	}
}

// uplinkRecords builds n delta chunks of frameID..frameID+n-1, chunkCount 1,
// each a full MaxDatagramSize datagram.
func uplinkRecords(t *testing.T, firstFrame uint32, n int) [][]byte {
	t.Helper()
	out := make([][]byte, 0, n)
	payload := make([]byte, wire.MaxChunkPayload)
	for i := range n {
		payload[0] = byte(i)
		d, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{
			FrameID: firstFrame + uint32(i), ChunkIndex: 0, ChunkCount: 1,
			TimestampUs: uint64(firstFrame+uint32(i)) * 16_667,
		}, payload)
		if err != nil {
			t.Fatalf("AppendVideoChunk: %v", err)
		}
		out = append(out, d)
	}
	return out
}

func carrierStreamBytes(t *testing.T, records [][]byte) []byte {
	t.Helper()
	out := wire.AppendCarrierPrologue(nil)
	for _, r := range records {
		var err error
		if out, err = wire.AppendCarrierRecord(out, r); err != nil {
			t.Fatalf("AppendCarrierRecord: %v", err)
		}
	}
	return out
}

// A carrier's records reach a datagram viewer as the very datagrams they
// replace, over a real session; a third concurrent carrier is reset and
// counted.
func TestUplinkCarrierOverRealSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, uplinkTestConfig(true))
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")
	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	defer sub.CloseWithError(0, "")
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

	sendKeyframeStream(t, pub, buildStreamKeyframe(t, 10, "avc1.42E02A", 2000))
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.KeyframeStreamsIn == 1 }, "keyframe ingested")

	// Fewer records than the test viewer's quic-go keeps in its own
	// 32-deep datagram queue (docs/75 D7): a burst past it drops on the
	// viewer's side, which is not what this test is about.
	recs := uplinkRecords(t, 11, 16)
	car, err := pub.OpenUniStream()
	if err != nil {
		t.Fatalf("OpenUniStream: %v", err)
	}
	if _, err := car.Write(carrierStreamBytes(t, recs)); err != nil {
		t.Fatalf("write carrier: %v", err)
	}
	// Held open, like a GOP's carrier: records must flow before the FIN.
	got := map[string]bool{}
	recvCtx, recvCancel := context.WithTimeout(ctx, 5*time.Second)
	defer recvCancel()
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
	if st.UplinkCarriers != 1 || st.UplinkRecords != uint64(len(recs)) || st.BadDatagrams != 0 {
		t.Fatalf("carriers %d records %d bad %d, want 1 / %d / 0", st.UplinkCarriers, st.UplinkRecords, st.BadDatagrams, len(recs))
	}

	// A second carrier held open, then a third: refused and reset.
	second, err := pub.OpenUniStream()
	if err != nil {
		t.Fatalf("OpenUniStream: %v", err)
	}
	if _, err := second.Write(wire.AppendCarrierPrologue(nil)); err != nil {
		t.Fatalf("write second prologue: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.UplinkCarriers == 2 }, "second carrier open")
	third, err := pub.OpenUniStream()
	if err != nil {
		t.Fatalf("OpenUniStream: %v", err)
	}
	if _, err := third.Write(wire.AppendCarrierPrologue(nil)); err != nil {
		t.Fatalf("write third prologue: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.UplinkCarriersRejected == 1 }, "third carrier rejected")
	// The relay's STOP_SENDING surfaces as a write error on the third.
	waitFor(t, 5*time.Second, func() bool {
		_, err := third.Write([]byte{0})
		return err != nil
	}, "third carrier reset by the relay")
	if st := r.Stats().Totals; st.UplinkCarriers != 2 {
		t.Fatalf("carriers = %d after the rejection, want 2", st.UplinkCarriers)
	}
	car.Close()
	second.Close()
}

// statuszKeys flattens a /statusz document into its set of key paths, with
// the per-broadcast keys (obfuscated IDs) and array indices collapsed.
func statuszKeys(t *testing.T, body []byte) []string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal /statusz: %v", err)
	}
	var out []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				if prefix == ".broadcasts" {
					k = "*"
				}
				out = append(out, prefix+"."+k)
				walk(prefix+"."+k, child)
			}
		case []any:
			for _, child := range x {
				walk(prefix+"[]", child)
			}
		}
	}
	walk("", doc)
	sort.Strings(out)
	return slices.Compact(out)
}

func metricSeriesNames(t *testing.T, r *hub.Registry) []string {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(metrics.NewRegistryCollector(r))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var out []string
	for _, mf := range mfs {
		out = append(out, mf.GetName())
	}
	sort.Strings(out)
	return out
}

// G6 (docs/75): with -uplink-carriers=false the relay is the pre-R73 relay.
// A publisher's 0x0A stream is rejected as bad input exactly as before, and
// for the same datagram broadcast /statusz has the same shape as a relay
// with the flag on, while /metrics differs only by the uplink series, which
// only the flag-on relay has.
func TestUplinkCarriersOffIsByteIdentical(t *testing.T) {
	type observed struct {
		statusz []string
		series  []string
	}
	run := func(on bool) observed {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		port, clientTLS, r, _ := startTestServerCfg(t, ctx, uplinkTestConfig(on))
		pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
		defer pub.CloseWithError(0, "")
		sub := dialSubscriber(t, ctx, port, id, clientTLS)
		defer sub.CloseWithError(0, "")
		waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

		sendKeyframeStream(t, pub, buildStreamKeyframe(t, 0, "avc1.42E02A", 2000))
		for _, d := range encodeFrame(t, 1, false, 3) {
			if err := pub.SendDatagram(d); err != nil {
				t.Fatalf("SendDatagram: %v", err)
			}
		}
		waitFor(t, 5*time.Second, func() bool {
			st := r.Stats().Broadcasts[r.ObfuscateID(id)]
			return st.KeyframeStreamsIn == 1 && st.DatagramsRelayed >= 3
		}, "datagram broadcast relayed")

		if !on {
			// The pre-R73 relay's answer to a 0x0A stream: bad input.
			str, err := pub.OpenUniStream()
			if err != nil {
				t.Fatalf("OpenUniStream: %v", err)
			}
			if _, err := str.Write(carrierStreamBytes(t, uplinkRecords(t, 2, 3))); err != nil {
				t.Fatalf("write carrier: %v", err)
			}
			str.Close()
			waitFor(t, 5*time.Second, func() bool {
				return r.Stats().Broadcasts[r.ObfuscateID(id)].BadDatagrams == 1
			}, "0x0A stream counted as bad input")
			st := r.Stats().Broadcasts[r.ObfuscateID(id)]
			if st.UplinkRecords != 0 || st.FramesRelayed != 2 {
				t.Fatalf("flag off ingested the carrier: %+v", st)
			}
		}

		_, body := h3Get(t, ctx, clientTLS, fmt.Sprintf("https://127.0.0.1:%d/statusz", port))
		if !on && strings.Contains(strings.ToLower(string(body)), "uplink") {
			t.Fatalf("flag-off /statusz mentions uplink: %s", body)
		}
		return observed{statusz: statuszKeys(t, body), series: metricSeriesNames(t, r)}
	}

	off, on := run(false), run(true)
	if !slices.Equal(off.statusz, on.statusz) {
		t.Fatalf("/statusz shape differs with the flag off:\n off %v\n on  %v", off.statusz, on.statusz)
	}
	var onWithoutUplink []string
	for _, n := range on.series {
		if !strings.Contains(n, "uplink") {
			onWithoutUplink = append(onWithoutUplink, n)
		}
	}
	if !slices.Equal(off.series, onWithoutUplink) {
		t.Fatalf("/metrics differs beyond the uplink series:\n off %v\n on  %v", off.series, onWithoutUplink)
	}
	if len(on.series) != len(off.series)+8 {
		t.Fatalf("flag on adds %d series, want the 8 uplink ones", len(on.series)-len(off.series))
	}
}

// docs/75 D10: the windows follow -uplink-max-bitrate, and the production
// constructor puts them on the relay's QUIC config.
func TestUplinkReceiveWindows(t *testing.T) {
	for _, tc := range []struct {
		name         string
		bytesPerSec  int64
		stream, conn uint64
	}{
		{"default 50 Mbps", 50_000_000 / 8, 2_000_000, 3_000_000},
		{"off", 0, 0, 0},
		{"1 Mbps floors at quic-go's defaults", 1_000_000 / 8, 512 << 10, 768 << 10},
		{"1 Gbps caps at quic-go's maxima", 1_000_000_000 / 8, 6 << 20, 9 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c := uplinkReceiveWindows(tc.bytesPerSec)
			if s != tc.stream || c != tc.conn {
				t.Fatalf("windows = %d / %d, want %d / %d", s, c, tc.stream, tc.conn)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, _, _, srv := startTestServerCfgLogSrv(t, ctx, uplinkTestConfig(true), discardLog)
	qc := srv.wt.H3.QUICConfig
	if qc.InitialStreamReceiveWindow != 2_000_000 || qc.InitialConnectionReceiveWindow != 3_000_000 {
		t.Fatalf("relay QUIC config windows = %d / %d, want 2000000 / 3000000",
			qc.InitialStreamReceiveWindow, qc.InitialConnectionReceiveWindow)
	}
	if qc.MaxStreamReceiveWindow != 0 || qc.MaxConnectionReceiveWindow != 0 {
		t.Fatalf("maxima = %d / %d, want quic-go's (0)", qc.MaxStreamReceiveWindow, qc.MaxConnectionReceiveWindow)
	}
}
