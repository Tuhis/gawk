package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

func parityDgram(t *testing.T, frameID uint32, index uint8) []byte {
	t.Helper()
	d, err := wire.AppendParityChunk(nil, wire.ParityChunkHeader{
		FrameID: frameID, ParityIndex: index, ChunkCount: 4, FrameBytes: 16,
	}, []byte{1, 2, 3, 4})
	if err != nil {
		t.Fatalf("AppendParityChunk: %v", err)
	}
	return d
}

// R29 (docs/34 §7.2): the fleet cost of defaulting to k=2 has to be
// SCRAPEABLE, not merely present in /statusz. Without this an operator can see
// the counters on one pod by hand and has no way to watch them over time or
// across a fleet — which is the whole reason the default is a chart value.
func TestParityMetricsExposeForwardedSuppressedAndBytes(t *testing.T) {
	r := hub.NewRegistry(discardLog, hub.Options{ParityDefault: 2})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	// One subscriber served both symbols, one served none: the suppressed
	// counter only means something with a subscriber declining them.
	if _, err := r.SubscribeParity(id, fakeConn{}, 2); err != nil {
		t.Fatalf("SubscribeParity(2): %v", err)
	}
	if _, err := r.SubscribeParity(id, fakeConn{}, 0); err != nil {
		t.Fatalf("SubscribeParity(0): %v", err)
	}

	pub.HandleDatagram(deltaDgram(t, 1))
	pub.HandleDatagram(parityDgram(t, 1, 0))
	pub.HandleDatagram(parityDgram(t, 1, 1))

	promReg := prometheus.NewPedanticRegistry()
	promReg.MustRegister(NewRegistryCollector(r))
	mfs, err := promReg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	b := map[string]string{"broadcast": "*"}
	// The k=2 subscriber got both symbols, the k=0 subscriber neither: two
	// forwarded and two suppressed, from ONE fan-out of two symbols. That
	// ratio is the per-subscriber filter, visible in a scrape.
	for _, c := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"gawk_broadcast_parity_datagrams_total", b, 2},
		{"gawk_broadcast_parity_suppressed_total", b, 2},
		{"gawk_relay_parity_datagrams_total", nil, 2},
		{"gawk_relay_parity_suppressed_total", nil, 2},
	} {
		if got := value(mfs, c.name, c.labels); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
	if got := value(mfs, "gawk_broadcast_egress_parity_bytes_total", b); got <= 0 {
		t.Errorf("gawk_broadcast_egress_parity_bytes_total = %v, want > 0", got)
	}
}

// The bytes metric is a SLICE of egress_bytes_total{kind="delta"}, never a
// sibling — parity rides the datagram path, so summing them double-counts.
// This pins the relationship so a later refactor cannot quietly make them
// disjoint and turn every egress dashboard into an overcount.
func TestParityBytesAreASliceOfDatagramEgress(t *testing.T) {
	r := hub.NewRegistry(discardLog, hub.Options{ParityDefault: 2})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	sub, err := r.SubscribeParity(id, fakeConn{}, 2)
	if err != nil {
		t.Fatalf("SubscribeParity: %v", err)
	}
	pub.HandleDatagram(deltaDgram(t, 1))
	pub.HandleDatagram(parityDgram(t, 1, 0))
	sub.Close()

	st := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if st.EgressParityBytes == 0 {
		t.Fatal("no parity bytes recorded")
	}
	if st.EgressParityBytes >= st.EgressDatagramBytes {
		t.Errorf("EgressParityBytes (%d) >= EgressDatagramBytes (%d): parity must be a SLICE of the datagram total, so the delta payload has to be in there too",
			st.EgressParityBytes, st.EgressDatagramBytes)
	}
	if len(parityDgram(t, 1, 0)) != int(st.EgressParityBytes) {
		t.Errorf("EgressParityBytes = %d, want exactly the one parity datagram's %d bytes",
			st.EgressParityBytes, len(parityDgram(t, 1, 0)))
	}
}

// A fleet with parity off must scrape byte-identically to pre-R29: the
// counters are omitempty in /statusz, and the metrics must simply be zero
// rather than absent (a Prometheus series that appears only under load is
// worse than one that reads 0).
func TestParityMetricsZeroWhenFleetOff(t *testing.T) {
	r := hub.NewRegistry(discardLog, hub.Options{ParityDefault: 0})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	if _, err := r.Subscribe(id, fakeConn{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	pub.HandleDatagram(deltaDgram(t, 1))

	promReg := prometheus.NewPedanticRegistry()
	promReg.MustRegister(NewRegistryCollector(r))
	mfs, err := promReg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, name := range []string{
		"gawk_relay_parity_datagrams_total",
		"gawk_relay_parity_suppressed_total",
		"gawk_relay_egress_parity_bytes_total",
	} {
		if got := value(mfs, name, nil); got != 0 {
			t.Errorf("%s = %v on a parity-off fleet, want 0", name, got)
		}
	}
}
