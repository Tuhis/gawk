package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Tuhis/gawk/gawk-server/internal/clientinfo"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// R59 (docs/61 D6): an origin broadcast exports one info series with exactly
// these labels. The key is the HMAC'd one; an unprobed codec and height read
// "unknown", never blank.
func TestBroadcastInfoGaugeLabels(t *testing.T) {
	r := hub.NewRegistry(discardLog, hub.Options{})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	pub.SetClient(clientinfo.Info{App: "web", OS: "macos", Browser: "safari"})
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewRegistryCollector(r))
	key := r.ObfuscateID(id)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"broadcast": key, "codec": "unknown", "resolution": "unknown", "app": "web", "os": "macos", "browser": "safari"}
	if got := value(mfs, "gawk_broadcast_info", want); got != 1 {
		t.Fatalf("gawk_broadcast_info%v = %v, want 1", want, got)
	}

	cfg, err := wire.AppendDecoderConfig(nil, wire.DecoderConfig{Codec: "vp8"})
	if err != nil {
		t.Fatal(err)
	}
	pub.HandleDatagram(cfg)
	mfs, _ = reg.Gather()
	want["codec"] = "vp8"
	if got := value(mfs, "gawk_broadcast_info", want); got != 1 {
		t.Errorf("after a VP8 config: gawk_broadcast_info%v = %v, want 1", want, got)
	}
	for _, mf := range mfs {
		if mf.GetName() != "gawk_broadcast_info" {
			continue
		}
		for _, m := range mf.GetMetric() {
			if len(m.GetLabel()) != 6 {
				t.Errorf("info series has %d labels, want exactly 6: %v", len(m.GetLabel()), m.GetLabel())
			}
			for _, l := range m.GetLabel() {
				if strings.Contains(l.GetValue(), id) {
					t.Errorf("label %s carries the raw broadcast ID", l.GetName())
				}
			}
		}
	}
}

// An edge hub never exports the info series: the origin is the broadcast's
// one source of truth, and a second series would double every count.
func TestBroadcastInfoGaugeOriginOnly(t *testing.T) {
	r := hub.NewRegistry(discardLog, hub.Options{})
	if _, _, err := r.EdgePublish("K7XQ2M"); err != nil {
		t.Fatal(err)
	}
	if n := testutil.CollectAndCount(NewRegistryCollector(r), "gawk_broadcast_info"); n != 0 {
		t.Errorf("an edge hub exported %d info series", n)
	}
}

// R59 (docs/61 D7): every configured cap is a series.
func TestLimitGaugesExportConfiguredCaps(t *testing.T) {
	c := NewLimitsCollector(Limits{"max_broadcasts": 30, "max_total_subscribers": 400, "max_bandwidth_bytes": 0})
	want := `
# HELP gawk_limit A configured cap (R59, docs/61 D7); 0 = unlimited. Per pod unless the name says otherwise.
# TYPE gawk_limit gauge
gawk_limit{name="max_bandwidth_bytes"} 0
gawk_limit{name="max_broadcasts"} 30
gawk_limit{name="max_total_subscribers"} 400
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "gawk_limit"); err != nil {
		t.Error(err)
	}
}

// The usage recorders label a zero Info "unknown" and are nil-safe.
func TestUsageRecordersLabelsAndNilSafety(t *testing.T) {
	var nilM *ServerMetrics
	nilM.BroadcastStarted(BroadcastNew, clientinfo.Info{})
	nilM.ViewerJoined(JoinFirst, "datagrams", clientinfo.Info{})
	nilM.ViewerLeft("datagrams", time.Second)
	nilM.BroadcastEnded(hub.BroadcastEnd{})
	nilM.RoomMinted()

	reg := prometheus.NewRegistry()
	m := NewServerMetrics(reg)
	m.BroadcastStarted(BroadcastNew, clientinfo.Info{})
	if got := m.BroadcastsStartedCount(BroadcastNew, "unknown", "unknown", "unknown"); got != 1 {
		t.Errorf("zero Info counted under %v, want unknown labels", got)
	}
	m.BroadcastEnded(hub.BroadcastEnd{Duration: 90 * time.Second, PeakViewers: 7})
	if n := testutil.CollectAndCount(reg, "gawk_broadcast_duration_seconds", "gawk_broadcast_peak_viewers"); n != 2 {
		t.Errorf("lifetime histograms: %d series, want 2", n)
	}
}
