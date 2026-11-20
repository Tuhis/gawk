package live

import (
	"math"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/ingest"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/relayscrape"
)

// R87 (docs/91 SG3) on the live path: goodput over the rolling window, the
// stream bitrate over the origin's recent rounds, the connection's path as
// scraped and its loss over the scrape window — and the two rules reading
// them, on §2's numbers.
func TestLiveJudgesADeepBufferViewerOnGoodputAndPath(t *testing.T) {
	const sid = "f2f2f2f2f2f2f2f2f2f2f2f2"
	cases := []struct {
		name                 string
		goodput, stream      float64
		srtt                 float64
		lostPerRound         uint64
		undersupply, ceiling bool
	}{
		{"§2: loss-limited", 5.05e6, 10.7e6, 36, 110, true, true},
		{"healthy deep buffer", 11.2e6, 11.3e6, 34, 0, false, false},
		{"full link", 7.6e6, 11.3e6, 170, 200, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, clk := newProj()
			sub := relayscrape.Subscriber{SessionID: sid, DVR: true, DVRBufferMs: 3000,
				PathSRTTMs: c.srtt, PathMinRTTMs: 33}
			var ingress uint64 = 1_000_000
			tMs := 0.0
			for r := range 6 {
				// 10 s of client samples per round, 2 s apart.
				var samples []ingest.Sample
				for range 5 {
					tMs += 2000
					bytes := c.goodput / 8 * tMs / 1000
					samples = append(samples, ingest.Sample{TMs: tMs, Stats: map[string]any{
						"receivedFps": 60.0, "decoderFps": 60.0, "deliveryMode": "dvr",
						"dvrBufferMs": 3000.0, "playoutOffsetMs": 3000.0, "dvrStripeWidth": 1.0,
						// The client's own live video rate: the stream less its audio.
						"dvrLiveRateBps":     c.stream - 128_000,
						"dvrGoodputBps":      c.goodput,
						"videoBytesReceived": bytes, "connectionBytesReceived": []any{bytes},
					}})
				}
				b := batch(sid, "viewer", nil)
				b.Samples = samples
				p.ObserveClient(b, "Firefox 161", "Windows", "0.40.0")

				sub.PathPacketsSent += 10_000
				sub.PathPacketsLost += c.lostPerRound
				obs := relayObs(sub)
				obs[0].Broadcast.IngressDatagramBytes = ingress
				p.ObserveRelay(round(obs))
				ingress += uint64(c.stream / 8 * 10)
				if r < 5 {
					clk.add(10 * time.Second)
				}
			}

			stream, ok := relayFact(p, sid, "streamBitrateBps")
			if !ok || math.Abs(stream-c.stream) > c.stream*0.01 {
				t.Errorf("streamBitrateBps = %v (%v), want %v", stream, ok, c.stream)
			}
			if loss, ok := relayFact(p, sid, "pathLossRatio"); !ok || loss != float64(c.lostPerRound)/10_000 {
				t.Errorf("pathLossRatio = %v (%v), want the window's %v", loss, ok, float64(c.lostPerRound)/10_000)
			}
			snap := p.Snapshot()
			v := findSession(snap, sid)
			if v == nil {
				t.Fatal("session not in the snapshot")
			}
			if g := v.Metrics["videoGoodputBps"]; math.Abs(g-c.goodput) > c.goodput*0.01 {
				t.Errorf("videoGoodputBps = %v, want %v", g, c.goodput)
			}
			fired := map[string]bool{}
			for _, f := range v.Findings {
				fired[f.ID] = true
			}
			if fired["resilient-undersupply"] != c.undersupply || fired["per-connection-ceiling"] != c.ceiling {
				t.Errorf("fired %v, want undersupply %v ceiling %v", fired, c.undersupply, c.ceiling)
			}
		})
	}
}

// Review round 1: a scrape that no longer supports a path or DVR fact clears
// it. A loss reading left standing would keep lossLimitedPath() true on a
// connection that has since gone clean, or whose relay stopped reporting.
func TestLivePathFactsClearWhenTheScrapeNoLongerSupportsThem(t *testing.T) {
	const sid = "f3f3f3f3f3f3f3f3f3f3f3f3"
	p, clk := newProj()
	sub := relayscrape.Subscriber{SessionID: sid, DVR: true, DVRBufferMs: 3000,
		PathSRTTMs: 36, PathMinRTTMs: 33, PathPacketsSent: 10_000, PathPacketsLost: 10}
	p.ObserveRelay(relayRound(sub))
	clk.add(5 * time.Second)
	sub.PathPacketsSent, sub.PathPacketsLost = 20_000, 120
	p.ObserveRelay(relayRound(sub))
	for _, name := range []string{"pathSrttMs", "pathMinRttMs", "pathLossRatio", "dvrBufferMs"} {
		if _, ok := relayFact(p, sid, name); !ok {
			t.Fatalf("%s absent after a supporting scrape", name)
		}
	}

	// Nothing sent since: the loss window reaches back to what was, but no
	// further than pathLossMaxWindow; after that there is no measurement.
	clk.add(5 * time.Second)
	p.ObserveRelay(relayRound(sub))
	if _, ok := relayFact(p, sid, "pathLossRatio"); !ok {
		t.Error("pathLossRatio absent within the loss window")
	}
	clk.add(pathLossMaxWindow)
	p.ObserveRelay(relayRound(sub))
	if v, ok := relayFact(p, sid, "pathLossRatio"); ok {
		t.Errorf("pathLossRatio = %v with nothing sent for the whole loss window", v)
	}

	// The relay stops reporting the path, and the viewer left deep mode.
	clk.add(5 * time.Second)
	p.ObserveRelay(relayRound(relayscrape.Subscriber{SessionID: sid}))
	for _, name := range []string{"pathSrttMs", "pathMinRttMs", "pathLossRatio", "dvrBufferMs"} {
		if v, ok := relayFact(p, sid, name); ok {
			t.Errorf("%s = %v left standing after a scrape without it", name, v)
		}
	}
}

// Review round 2 on the live path: the broadcaster pauses, the client's
// goodput reads 0 and its live rate holds — not undersupply.
func TestLivePausedBroadcasterIsNotUndersupply(t *testing.T) {
	const sid = "f4f4f4f4f4f4f4f4f4f4f4f4"
	p, clk := newProj()
	bytes, tMs := 0.0, 0.0
	for r := range 6 {
		var samples []ingest.Sample
		for range 5 {
			tMs += 2000
			goodput := 10e6
			if r >= 1 {
				goodput = 0 // paused from the second batch on
			} else {
				bytes += 10e6 / 8 * 2
			}
			samples = append(samples, ingest.Sample{TMs: tMs, Stats: map[string]any{
				"receivedFps": 60.0, "deliveryMode": "dvr", "dvrBufferMs": 3000.0,
				"playoutOffsetMs": 3000.0, "videoBytesReceived": bytes,
				"dvrGoodputBps": goodput, "dvrLiveRateBps": 10.2e6,
			}})
		}
		b := batch(sid, "viewer", nil)
		b.Samples = samples
		p.ObserveClient(b, "Firefox 161", "Windows", "0.40.0")
		obs := relayObs(relayscrape.Subscriber{SessionID: sid, DVR: true, DVRBufferMs: 3000})
		obs[0].Broadcast.IngressDatagramBytes = uint64(1_000_000 + r*13_000_000)
		obs[0].Broadcast.FramesRelayed = uint64(10_000 + r*600)
		p.ObserveRelay(round(obs))
		clk.add(10 * time.Second)
	}
	v := findSession(p.Snapshot(), sid)
	if v == nil {
		t.Fatal("session not in the snapshot")
	}
	for _, f := range v.Findings {
		if f.ID == "resilient-undersupply" {
			t.Errorf("a paused broadcaster read as undersupply: %+v", f)
		}
	}
}

// Review round 2: a client fact this branch added is cleared when a sample
// omits it — the client omits dvrLiveRateBps after a broadcaster restart
// until it is re-measured, and every deep-mode field when it leaves deep mode.
func TestLiveDeepModeClientFactsClearWhenOmitted(t *testing.T) {
	const sid = "f5f5f5f5f5f5f5f5f5f5f5f5"
	p, _ := newProj()
	deep := map[string]any{"receivedFps": 60.0, "deliveryMode": "dvr",
		"dvrBufferMs": 3000.0, "dvrStripeWidth": 2.0, "dvrLiveRateBps": 10e6, "dvrGoodputBps": 9e6}
	b := batch(sid, "viewer", nil)
	for i := range 6 {
		b.Samples = append(b.Samples, ingest.Sample{TMs: float64(i * 2000), Stats: deep})
	}
	p.ObserveClient(b, "Firefox 161", "Windows", "0.40.0")
	v := findSession(p.Snapshot(), sid)
	for _, name := range []string{"dvrBufferMs", "dvrStripeWidth", "dvrLiveRateBps", "dvrGoodputBps", "dvrSupplyRatio"} {
		if _, ok := v.Metrics[name]; !ok {
			t.Fatalf("%s absent while in deep mode: %v", name, v.Metrics)
		}
	}

	// Out of deep mode, long enough that the window holds no deep sample.
	plain := map[string]any{"receivedFps": 60.0, "deliveryMode": "reliable"}
	b = batch(sid, "viewer", nil)
	for i := range DipWindowSamples {
		b.Samples = append(b.Samples, ingest.Sample{TMs: float64(20_000 + i*2000), Stats: plain})
	}
	p.ObserveClient(b, "Firefox 161", "Windows", "0.40.0")
	v = findSession(p.Snapshot(), sid)
	for _, name := range []string{"dvrBufferMs", "dvrStripeWidth", "dvrLiveRateBps", "dvrGoodputBps", "dvrSupplyRatio"} {
		if got, ok := v.Metrics[name]; ok {
			t.Errorf("%s = %v left standing after samples that omit it", name, got)
		}
	}
}

// Review round 3: dvrIdle is a deep-mode fact like the others — read off the
// latest sample, cleared when a sample omits it.
func TestLiveDvrIdleClearsWhenOmitted(t *testing.T) {
	const sid = "f6f6f6f6f6f6f6f6f6f6f6f6"
	p, _ := newProj()
	b := batch(sid, "viewer", map[string]any{"receivedFps": 60.0, "deliveryMode": "dvr", "dvrIdle": true})
	p.ObserveClient(b, "Firefox 161", "Windows", "0.40.0")
	if v := findSession(p.Snapshot(), sid).Metrics["dvrIdle"]; v != 1 {
		t.Fatalf("dvrIdle = %v, want 1", v)
	}
	p.ObserveClient(batch(sid, "viewer", map[string]any{"receivedFps": 60.0, "deliveryMode": "reliable"}),
		"Firefox 161", "Windows", "0.40.0")
	if v, ok := findSession(p.Snapshot(), sid).Metrics["dvrIdle"]; ok {
		t.Errorf("dvrIdle = %v left standing after a sample without it", v)
	}
}

// PR review round 1: a near-idle connection sends a few keepalives per
// scrape, and one lost packet among them is not a loss rate. The window
// grows until it holds relayscrape.PathLossMinPackets.
func TestLivePathLossNeedsEnoughPackets(t *testing.T) {
	const sid = "f5f5f5f5f5f5f5f5f5f5f5f5"
	p, clk := newProj()
	sub := relayscrape.Subscriber{SessionID: sid, DVR: true, DVRBufferMs: 3000,
		PathSRTTMs: 34, PathMinRTTMs: 33, PathPacketsSent: 50_000, PathPacketsLost: 0}
	p.ObserveRelay(relayRound(sub))
	// 40 packets, one lost: 2.5 % on its face.
	clk.add(5 * time.Second)
	sub.PathPacketsSent, sub.PathPacketsLost = 50_040, 1
	p.ObserveRelay(relayRound(sub))
	if v, ok := relayFact(p, sid, "pathLossRatio"); ok {
		t.Errorf("pathLossRatio = %v over 40 packets", v)
	}
	// A slow stream: 1000 packets a scrape, so two scrapes make a window.
	for i := range 2 {
		clk.add(5 * time.Second)
		sub.PathPacketsSent += 1000
		sub.PathPacketsLost += uint64(3 * i)
		p.ObserveRelay(relayRound(sub))
		if v, ok := relayFact(p, sid, "pathLossRatio"); ok != (i == 1) || (ok && v != 3.0/2000) {
			t.Errorf("scrape %d: pathLossRatio = %v (%v), want 3/2000 once two scrapes hold enough packets", i, v, ok)
		}
	}
}
