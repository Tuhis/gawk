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
	"github.com/Tuhis/gawk/gawk-server/internal/wire"
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
