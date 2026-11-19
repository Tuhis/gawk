package readapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/rollup"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rules"
)

// dvrSession is a stored deep-buffer viewer session as finalize sees it: two
// minutes of samples with the offset at the 3000 ms DVR floor, video bytes
// growing at goodputBps, and the relay's records — the viewer's subscriber
// entry with its path counters, and the origin's broadcast record at either
// end of the span, ingesting streamBps.
func dvrSession(t *testing.T, goodputBps, streamBps, srtt, minRTT float64, lost uint64) (rollup.Row, rollup.Input, [][]byte) {
	t.Helper()
	const sid = "d2d2d2d2d2d2d2d2d2d2d2d2"
	const start, spanMs = int64(1_000_000), 120_000
	var samples []rollup.Sample
	for i := range 61 {
		tMs := float64(i * 2000)
		samples = append(samples, rollup.Sample{TMs: tMs, Stats: map[string]any{
			"receivedFps": 60.0, "decoderFps": 60.0, "renderedFps": 60.0,
			"timeSinceLastFrameMs": 16.0, "deliveryMode": "dvr", "dvrBufferMs": 3000.0,
			"playoutOffsetMs": 3000.0, "dvrStripeWidth": 1.0,
			"videoBytesReceived":      goodputBps / 8 * tMs / 1000,
			"connectionBytesReceived": []any{goodputBps / 8 * tMs / 1000},
		}})
	}
	in := rollup.Input{
		SessionID: sid, BroadcastKey: bkey, Role: "viewer",
		StartedAtMs: start, EndedAtMs: start + spanMs, Samples: samples,
	}
	line := func(v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	bc := func(atMs int64) []byte {
		bytes := streamBps / 8 * float64(atMs-start+60_000) / 1000
		return line(map[string]any{"kind": "broadcast", "atMs": atMs, "pod": "pod-a", "role": "origin",
			"broadcastKey": bkey, "broadcast": map[string]any{"publisherActive": true, "framesRelayed": atMs,
				"ingressDatagramBytes": uint64(bytes * 0.55), "keyframeBytesIn": uint64(bytes * 0.45)}})
	}
	lines := [][]byte{
		bc(start + 1000),
		line(map[string]any{"kind": "subscriber", "atMs": start + 60_000, "pod": "pod-a", "role": "origin",
			"broadcastKey": bkey, "sessionId": sid,
			"subscriber": map[string]any{"dvr": true, "dvrBufferMs": 3000, "dvrResyncs": 0,
				"pathSrttMs": srtt, "pathMinRttMs": minRTT, "pathPacketsSent": 100_000, "pathPacketsLost": lost}}),
		bc(start + spanMs - 1000),
	}
	return rollup.Compute(in), in, lines
}

// docs/91 SG3 on the stored path, end to end through finalize's
// DiagnoseRow: the §2 session fires undersupply on goodput and the ceiling
// on the relay's path; a healthy deep-buffer session fires neither; a full
// link fires undersupply and not the ceiling.
func TestStoredDVRSessionVerdicts(t *testing.T) {
	cases := []struct {
		name                     string
		goodput, stream          float64
		srtt, minRTT             float64
		lost                     uint64
		undersupply, ceiling     bool
		undersupplyBlamesTheLink bool
	}{
		{"§2: loss-limited at the field RTT", 5.05e6, 10.7e6, 36, 33, 1100, true, true, false},
		{"healthy deep buffer", 11.2e6, 11.3e6, 41, 40, 0, false, false, false},
		{"§11.1 full link", 7.6e6, 11.3e6, 180, 40, 1950, true, false, true},
	}
	f := newFixture(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row, in, lines := dvrSession(t, c.goodput, c.stream, c.srtt, c.minRTT, c.lost)
			rep := f.api.DiagnoseRow(row, in, lines)
			under := findOne(rep, "resilient-undersupply")
			if (under != nil) != c.undersupply {
				t.Errorf("resilient-undersupply fired = %v, want %v; findings %+v unavailable %+v",
					under != nil, c.undersupply, rep.Findings, rep.Unavailable)
			}
			if (findOne(rep, "per-connection-ceiling") != nil) != c.ceiling {
				t.Errorf("per-connection-ceiling fired = %v, want %v; findings %+v", !c.ceiling, c.ceiling, rep.Findings)
			}
			if under == nil {
				return
			}
			var cited bool
			for _, e := range under.Evidence {
				if e.Signal == "videoGoodputBps" && e.Value > c.goodput*0.99 && e.Value < c.goodput*1.01 {
					cited = true
				}
			}
			if !cited {
				t.Errorf("undersupply evidence does not cite the measured goodput: %+v", under.Evidence)
			}
			if blames := strings.Contains(under.Verdict, "the link cannot"); blames != c.undersupplyBlamesTheLink {
				t.Errorf("verdict %q: blames the link = %v, want %v", under.Verdict, blames, c.undersupplyBlamesTheLink)
			}
		})
	}
}

func findOne(rep rules.Report, id string) *rules.Finding {
	for i := range rep.Findings {
		if rep.Findings[i].ID == id {
			return &rep.Findings[i]
		}
	}
	return nil
}

// JoinRelay puts the connection's path counters on the permanent row beside
// its other relay counters, so a past session's path is queryable.
func TestJoinRelayKeepsPathCounters(t *testing.T) {
	row, _, lines := dvrSession(t, 5.05e6, 10.7e6, 36, 33, 1100)
	JoinRelay(&row, lines, 0)
	for name, want := range map[string]float64{
		"pathPacketsSent": 100_000, "pathPacketsLost": 1100, "pathSrttMs": 36, "pathMinRttMs": 33,
	} {
		if got, ok := row.Relay[name]; !ok || got != want {
			t.Errorf("row.Relay[%s] = %v (%v), want %v", name, got, ok, want)
		}
	}
}
