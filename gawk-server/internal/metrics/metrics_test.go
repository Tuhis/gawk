package metrics

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func withKind(base map[string]string, kind string) map[string]string {
	out := map[string]string{"kind": kind}
	for k, v := range base {
		out[k] = v
	}
	return out
}

func withReason(base map[string]string, reason string) map[string]string {
	out := map[string]string{"reason": reason}
	for k, v := range base {
		out[k] = v
	}
	return out
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

func timeAfter() <-chan time.Time { return time.After(5 * time.Second) }

// fakeConn is a minimal hub.Conn whose keyframe stream opens always fail —
// giving the test a deterministic, synchronous reason-labeled drop.
type fakeConn struct{}

func (fakeConn) SendDatagram([]byte) error { return nil }
func (fakeConn) OpenKeyframeStream() (hub.KeyframeStream, error) {
	return nil, errors.New("open failed")
}
func (fakeConn) OpenCarrierStream() (hub.KeyframeStream, error) {
	return nil, errors.New("open failed")
}
func (fakeConn) CloseWithError(uint32, string) error { return nil }

func deltaDgram(t *testing.T, frameID uint32) []byte {
	t.Helper()
	d, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{
		FrameID: frameID, ChunkIndex: 0, ChunkCount: 1, TimestampUs: uint64(frameID),
	}, []byte("payload"))
	if err != nil {
		t.Fatalf("AppendVideoChunk: %v", err)
	}
	return d
}

func keyframeMsg(t *testing.T, frameID uint32) []byte {
	t.Helper()
	payload := []byte("kf-payload")
	msg, err := wire.AppendStreamFrameHeader(nil, wire.StreamFrameHeader{
		Keyframe: true, FrameID: frameID, TimestampUs: uint64(frameID),
		PayloadLen: uint32(len(payload)),
	})
	if err != nil {
		t.Fatalf("AppendStreamFrameHeader: %v", err)
	}
	return append(msg, payload...)
}

// value finds one series by family name + labels; -1 if absent.
func value(mfs []*dto.MetricFamily, name string, labels map[string]string) float64 {
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	metric:
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			for k, v := range labels {
				if k == "broadcast" && v == "*" {
					if got[k] == "" {
						continue metric
					}
					continue
				}
				if got[k] != v {
					continue metric
				}
			}
			if m.GetCounter() != nil {
				return m.GetCounter().GetValue()
			}
			return m.GetGauge().GetValue()
		}
	}
	return -1
}

func TestRegistryCollectorScenario(t *testing.T) {
	r := hub.NewRegistry(discardLog, hub.Options{})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	if _, err := r.Subscribe(id, fakeConn{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Two delta frames + one keyframe; the keyframe fan-out to the fake conn
	// fails at stream-open, which must land under reason="open_failed".
	pub.HandleDatagram(deltaDgram(t, 1))
	pub.HandleDatagram(deltaDgram(t, 2))
	if err := pub.IngestKeyframeStream(bytesReader(keyframeMsg(t, 3))); err != nil {
		t.Fatalf("IngestKeyframeStream: %v", err)
	}

	collector := NewRegistryCollector(r)
	if _, err := testutil.CollectAndLint(collector); err != nil {
		t.Errorf("CollectAndLint: %v", err)
	}

	promReg := prometheus.NewPedanticRegistry()
	promReg.MustRegister(collector)
	mfs, err := promReg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	b := map[string]string{"broadcast": "*"}
	checks := []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"gawk_broadcasts_active", nil, 1},
		{"gawk_subscribers_active", nil, 1},
		{"gawk_broadcast_publisher_active", b, 1},
		{"gawk_broadcast_subscribers", b, 1},
		// R18: the origin's pushed global count — one local viewer here.
		{"gawk_broadcast_viewers_global", b, 1},
		{"gawk_broadcast_frames_relayed_total", withKind(b, "delta"), 2},
		{"gawk_broadcast_frames_relayed_total", withKind(b, "keyframe"), 1},
		{"gawk_broadcast_datagrams_relayed_total", b, 2},
		{"gawk_broadcast_keyframe_streams_dropped_total", withReason(b, "open_failed"), 1},
		{"gawk_broadcast_keyframe_streams_dropped_total", withReason(b, "slow"), 0},
		{"gawk_broadcast_ingress_bytes_total", withKind(b, "keyframe"), float64(len(keyframeMsg(t, 3)))},
		{"gawk_relay_frames_relayed_total", map[string]string{"kind": "delta"}, 2},
		{"gawk_relay_keyframe_streams_dropped_total", map[string]string{"reason": "open_failed"}, 1},
		{"gawk_relay_datagrams_dropped_total", map[string]string{"reason": "queue_full"}, 0},
		{"gawk_relay_datagrams_dropped_total", map[string]string{"reason": "bandwidth"}, 0},
	}
	for _, c := range checks {
		if got := value(mfs, c.name, c.labels); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}

	// Delta ingress bytes: two datagrams' worth.
	if got := value(mfs, "gawk_broadcast_ingress_bytes_total", withKind(b, "delta")); got != float64(2*len(deltaDgram(t, 1))) {
		t.Errorf("ingress delta bytes = %v, want %v", got, 2*len(deltaDgram(t, 1)))
	}
}

func TestRegistryCollectorTotalsSurviveGC(t *testing.T) {
	// Counters folded from an expired broadcast must remain visible in the
	// gawk_relay_* totals even after the per-broadcast series disappear.
	r := hub.NewRegistry(discardLog, hub.Options{BroadcastGrace: 1}) // ~instant GC
	_, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	pub.HandleDatagram(deltaDgram(t, 1))
	pub.Close()

	collector := NewRegistryCollector(r)
	promReg := prometheus.NewPedanticRegistry()
	promReg.MustRegister(collector)

	deadline := make(chan struct{})
	go func() {
		for {
			mfs, err := promReg.Gather()
			if err == nil &&
				value(mfs, "gawk_broadcasts_active", nil) == 0 &&
				value(mfs, "gawk_relay_frames_relayed_total", map[string]string{"kind": "delta"}) == 1 {
				close(deadline)
				return
			}
		}
	}()
	select {
	case <-deadline:
	case <-timeAfter():
		t.Fatal("totals did not survive broadcast GC")
	}
}

func TestServerMetricsNilSafe(t *testing.T) {
	var m *ServerMetrics
	m.Connection("publish", OutcomeAccepted) // must not panic
	m.RateLimited()
	m.OriginRejected()
	if m.ConnectionCount("publish", OutcomeAccepted) != 0 {
		t.Error("nil ServerMetrics ConnectionCount != 0")
	}
}

func TestServerMetricsCounts(t *testing.T) {
	m := NewServerMetrics(prometheus.NewRegistry())
	m.Connection("publish", OutcomeAccepted)
	m.Connection("publish", OutcomeAccepted)
	m.Connection("subscribe", OutcomeNotFound)
	m.RateLimited()
	if got := m.ConnectionCount("publish", OutcomeAccepted); got != 2 {
		t.Errorf("publish/accepted = %v, want 2", got)
	}
	if got := m.ConnectionCount("subscribe", OutcomeNotFound); got != 1 {
		t.Errorf("subscribe/not_found = %v, want 1", got)
	}
	if got := m.RateLimitedCount(); got != 1 {
		t.Errorf("rate_limited = %v, want 1", got)
	}
}

// R17 W6 (docs/22 Decision 14): loss attribution stays clean — an origin
// hub's ingress window feeds the broadcaster-leg family, an edge hub's feeds
// the SEPARATE edge-leg family, and neither ever appears in the other. Role
// and edge-session gauges ride along.
func TestLossAttributionByLeg(t *testing.T) {
	r := hub.NewRegistry(discardLog, hub.Options{})

	// The ingress window declares loss only when an unseen frame AGES OUT of
	// its 1024-frame ring (reordering robustness): create the gap, then
	// advance far enough to evict the missing slots.
	// Origin hub: frames 1, 4 seen; 2 and 3 age out ⇒ 2 lost.
	originID, originPub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer originPub.Close()
	originPub.HandleDatagram(deltaDgram(t, 1))
	originPub.HandleDatagram(deltaDgram(t, 4))
	originPub.HandleDatagram(deltaDgram(t, 4+1024))

	// Edge hub: frames 1, 3 seen; 2 ages out ⇒ 1 lost.
	edgeID, edgePub, err := r.EdgePublish("K7XQ2M")
	if err != nil {
		t.Fatalf("EdgePublish: %v", err)
	}
	defer edgePub.Close()
	edgePub.HandleDatagram(deltaDgram(t, 1))
	edgePub.HandleDatagram(deltaDgram(t, 3))
	edgePub.HandleDatagram(deltaDgram(t, 3+1024))

	collector := NewRegistryCollector(r)
	if _, err := testutil.CollectAndLint(collector); err != nil {
		t.Errorf("CollectAndLint: %v", err)
	}
	promReg := prometheus.NewPedanticRegistry()
	promReg.MustRegister(collector)
	mfs, err := promReg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	obfOrigin := r.ObfuscateID(originID)
	obfEdge := r.ObfuscateID(edgeID)

	get := func(family string, labels map[string]string) float64 {
		for _, mf := range mfs {
			if mf.GetName() != family {
				continue
			}
		metric:
			for _, m := range mf.GetMetric() {
				got := map[string]string{}
				for _, lp := range m.GetLabel() {
					got[lp.GetName()] = lp.GetValue()
				}
				for k, v := range labels {
					if got[k] != v {
						continue metric
					}
				}
				if m.GetCounter() != nil {
					return m.GetCounter().GetValue()
				}
				return m.GetGauge().GetValue()
			}
		}
		return -1
	}

	// Broadcaster-leg family: origin's 2 lost frames, and NO edge series.
	if v := get("gawk_broadcast_ingress_frames_lost_total", map[string]string{"broadcast": obfOrigin}); v != 2 {
		t.Errorf("origin broadcaster-leg loss = %v, want 2", v)
	}
	if v := get("gawk_broadcast_ingress_frames_lost_total", map[string]string{"broadcast": obfEdge}); v != -1 {
		t.Errorf("edge hub leaked into the broadcaster-leg family: %v", v)
	}
	// Edge-leg family: edge's 1 lost frame, and NO origin series.
	if v := get("gawk_broadcast_edge_ingress_frames_lost_total", map[string]string{"broadcast": obfEdge}); v != 1 {
		t.Errorf("edge-leg loss = %v, want 1", v)
	}
	if v := get("gawk_broadcast_edge_ingress_frames_lost_total", map[string]string{"broadcast": obfOrigin}); v != -1 {
		t.Errorf("origin hub leaked into the edge-leg family: %v", v)
	}
	// Relay-lifetime totals keep the split too.
	if v := get("gawk_relay_ingress_frames_lost_total", nil); v != 2 {
		t.Errorf("relay broadcaster-leg total = %v, want 2", v)
	}
	if v := get("gawk_relay_edge_ingress_frames_lost_total", nil); v != 1 {
		t.Errorf("relay edge-leg total = %v, want 1", v)
	}
	// Role labels.
	if v := get("gawk_broadcast_role", map[string]string{"broadcast": obfOrigin, "role": "origin"}); v != 1 {
		t.Errorf("origin role gauge = %v, want 1", v)
	}
	if v := get("gawk_broadcast_role", map[string]string{"broadcast": obfEdge, "role": "edge"}); v != 1 {
		t.Errorf("edge role gauge = %v, want 1", v)
	}
	// R18: viewers_global is an origin-only series — an edge hub doesn't
	// compute G, and a zero would read as "no viewers" instead of "not mine".
	if v := get("gawk_broadcast_viewers_global", map[string]string{"broadcast": obfOrigin}); v != 0 {
		t.Errorf("origin viewers_global = %v, want 0 (no viewers attached)", v)
	}
	if v := get("gawk_broadcast_viewers_global", map[string]string{"broadcast": obfEdge}); v != -1 {
		t.Errorf("edge hub emitted viewers_global: %v", v)
	}
}

// metricValue finds one counter/gauge value in gathered families; -1 when
// no metric matches all the given labels.
func metricValue(mfs []*dto.MetricFamily, family string, labels map[string]string) float64 {
	for _, mf := range mfs {
		if mf.GetName() != family {
			continue
		}
	metric:
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			for k, v := range labels {
				if got[k] != v {
					continue metric
				}
			}
			if m.GetCounter() != nil {
				return m.GetCounter().GetValue()
			}
			return m.GetGauge().GetValue()
		}
	}
	return -1
}

// R17 post-review fix (PR #47): the per-hub ingress-loss counters are
// attributed to the hub's CURRENT role at scrape time, so a role flip
// (demote to edge, or come-home to origin) must fold the accumulated counts
// into the OLD leg's lifetime totals first — losses counted on one leg must
// never resurface under the other family (docs/22 Decision 14: never mixed).
func TestLossAttributionSurvivesRoleFlip(t *testing.T) {
	r := hub.NewRegistry(discardLog, hub.Options{})

	// Origin life: frames 1, 4 seen; 2 and 3 age out ⇒ 2 lost on the
	// broadcaster leg.
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	pub.HandleDatagram(deltaDgram(t, 1))
	pub.HandleDatagram(deltaDgram(t, 4))
	pub.HandleDatagram(deltaDgram(t, 4+1024))
	pub.Close()

	// The pod demotes: the SAME hub becomes an edge; frames 1, 3 seen ⇒ 1
	// lost on the edge leg (the claim resets the ingress window).
	edgeID, edgePub, err := r.EdgePublish(id)
	if err != nil {
		t.Fatalf("EdgePublish: %v", err)
	}
	if edgeID != id {
		t.Fatalf("EdgePublish id = %q, want %q", edgeID, id)
	}
	defer edgePub.Close()
	edgePub.HandleDatagram(deltaDgram(t, 1))
	edgePub.HandleDatagram(deltaDgram(t, 3))
	edgePub.HandleDatagram(deltaDgram(t, 3+1024))

	promReg := prometheus.NewPedanticRegistry()
	promReg.MustRegister(NewRegistryCollector(r))
	mfs, err := promReg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	obf := r.ObfuscateID(id)

	// The edge-leg family shows ONLY the edge-leg loss…
	if v := metricValue(mfs, "gawk_broadcast_edge_ingress_frames_lost_total", map[string]string{"broadcast": obf}); v != 1 {
		t.Errorf("edge-leg loss after flip = %v, want 1 (origin-leg counts leaked across the role flip)", v)
	}
	// …and the origin-leg counts survive the flip on the broadcaster leg
	// (folded into the relay lifetime totals when the role changed).
	if v := metricValue(mfs, "gawk_relay_ingress_frames_lost_total", nil); v != 2 {
		t.Errorf("relay broadcaster-leg total after flip = %v, want 2", v)
	}
	if v := metricValue(mfs, "gawk_relay_edge_ingress_frames_lost_total", nil); v != 1 {
		t.Errorf("relay edge-leg total after flip = %v, want 1", v)
	}
}

// R17 W6: a shared StatsKey gives one broadcast ONE obfuscated identity on
// every pod (metrics aggregate across the fleet); without it, per-process
// keys keep today's single-pod behavior.
func TestSharedStatsKeyFleetIdentity(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i * 3)
	}
	a := hub.NewRegistry(discardLog, hub.Options{StatsKey: key})
	b := hub.NewRegistry(discardLog, hub.Options{StatsKey: key})
	if a.ObfuscateID("K7XQ2M") != b.ObfuscateID("K7XQ2M") {
		t.Error("shared stats key produced different obfuscated IDs across registries")
	}

	c := hub.NewRegistry(discardLog, hub.Options{})
	d := hub.NewRegistry(discardLog, hub.Options{})
	if c.ObfuscateID("K7XQ2M") == d.ObfuscateID("K7XQ2M") {
		t.Error("per-process stats keys unexpectedly collide")
	}
}
