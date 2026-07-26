package rules

// The docs/13 bottleneck playbook, transcribed row for row (docs/33 D6 +
// §4.7). Each rule keeps the playbook's own verdict wording, so a report and
// the human procedure say the same thing in the same words.
//
// Adding a rule here is how a field finding gets institutionalized: docs/20
// finding 8's concealment-vs-overflow ratio is a rule below, and finding 12's
// `avSkewMs` over-report becomes one the moment its signature is known — the
// 14-day window is what will let it be known.
//
// Thresholds are named constants rather than inline numbers, because the
// dashboard evaluates the SAME rules over a live window (§4.8.3) and a
// threshold that drifted between the two would put two disagreeing truths
// about one stream in front of an operator.

const (
	// A funnel gap this large between adjacent stages localizes a bottleneck.
	// Below it, normal variance.
	funnelGapRatio = 0.75
	// Frame-gap thresholds. One GOP is 500 ms; two is a freeze a viewer feels.
	stallWarnMs = 1000
	stallBadMs  = 3000
	// A subscriber dropping this many times the peer median is an outlier
	// rather than a fleet-wide problem.
	outlierDropRatio = 3
	// Ingress loss above this is leg A, not noise. RTP-style windowed loss
	// (R9 M3) is already smoothed, so a small nonzero value is real.
	ingressLossWarn = 0.005
	ingressLossBad  = 0.02
	// docs/20 finding 8: overflow drops at this share of arrivals with
	// concealment climbing alongside is the latch, not ordinary loss.
	audioOverflowLatchRatio = 0.25
	// A playout offset pinned within this of its clamp is not adapting.
	playoutClampSlackMs = 50
	resilientClampMs    = 2000
)

// Playbook returns the full rule set.
func Playbook() []Rule {
	return []Rule{
		legBSingleViewer(),
		bandwidthCap(),
		relayEgressSaturation(),
		legABroadcasterUplink(),
		encoderOverload(),
		sendPathGap(),
		decoderChoking(),
		stallAttribution(),
		keyframeGapChurn(),
		configOrLimits(),
		resilientUndersupply(),
		carrierQueueOverflow(),
		viewerCountGap(),
		dvrRingOutlived(),
		audioOverflowLatch(),
	}
}

// Row 1: one viewer stutters, others fine.
func legBSingleViewer() Rule {
	return Rule{
		ID:    "leg-b-single-viewer",
		Scope: "viewer",
		Requires: []string{
			"relay.subscriberDropped", "relay.peerMedianDropped", "relay.ingressLossRatio",
		},
		Verdict: "Leg B for this viewer — their downlink or machine",
		Action:  "Nothing to fix relay-side; check that viewer's network or device.",
		Eval: func(f *Facts) *Finding {
			dropped, _ := f.Relay("subscriberDropped")
			peer, _ := f.Relay("peerMedianDropped")
			ingress, _ := f.Relay("ingressLossRatio")
			// The discriminator is the CONTRAST: this subscriber's drops far
			// above its peers' while the relay's own ingress is clean. Either
			// half alone means something else entirely.
			if ingress >= ingressLossWarn || dropped == 0 {
				return nil
			}
			if peer > 0 && dropped < peer*outlierDropRatio {
				return nil
			}
			if peer == 0 && dropped < 10 {
				return nil
			}
			return &Finding{
				Severity:   SeverityWarn,
				Confidence: 0.85,
				Evidence: []Evidence{
					{Signal: "subscriber.dropped", Value: dropped, From: FromRelay, Comparison: "vs peer median"},
					{Signal: "peer.median.dropped", Value: peer, From: FromRelay},
					{Signal: "broadcast.ingressLossRatio", Value: ingress, From: FromRelay},
				},
			}
		},
	}
}

// Row 2: all viewers stutter, egress drops reason="bandwidth".
func bandwidthCap() Rule {
	return Rule{
		ID:       "configured-bandwidth-cap",
		Scope:    "broadcast",
		Requires: []string{"relay.bandwidthDroppedDatagrams", "relay.ingressLossRatio"},
		Verdict:  "Configured bandwidth cap — the relay is shedding by policy, not by failure",
		Action:   "Raise -max-bandwidth or lower the ladder rung.",
		Eval: func(f *Facts) *Finding {
			dropped, _ := f.Relay("bandwidthDroppedDatagrams")
			ingress, _ := f.Relay("ingressLossRatio")
			if dropped == 0 || ingress >= ingressLossWarn {
				return nil
			}
			return &Finding{
				Severity: SeverityBad, Confidence: 0.95,
				Evidence: []Evidence{
					{Signal: "bandwidthDroppedDatagrams", Value: dropped, From: FromRelay},
					{Signal: "ingressLossRatio", Value: ingress, From: FromRelay},
				},
			}
		},
	}
}

// Row 3: all viewers stutter, queue_full on ALL subscribers.
func relayEgressSaturation() Rule {
	return Rule{
		ID:       "relay-egress-saturation",
		Scope:    "broadcast",
		Requires: []string{"relay.subscribersDropping", "relay.subscribers", "relay.ingressLossRatio"},
		Verdict:  "Relay egress / homelab uplink — every subscriber is dropping uniformly",
		Action:   "Check node network metrics and the uplink; this is not one viewer's problem.",
		Eval: func(f *Facts) *Finding {
			dropping, _ := f.Relay("subscribersDropping")
			total, _ := f.Relay("subscribers")
			ingress, _ := f.Relay("ingressLossRatio")
			// "All of them", not "several": the whole point of this row is
			// distinguishing a shared bottleneck from a set of unlucky viewers.
			if total < 2 || dropping < total || ingress >= ingressLossWarn {
				return nil
			}
			return &Finding{
				Severity: SeverityBad, Confidence: 0.8,
				Evidence: []Evidence{
					{Signal: "subscribersDropping", Value: dropping, From: FromRelay, Comparison: "of all subscribers"},
					{Signal: "subscribers", Value: total, From: FromRelay},
				},
			}
		},
	}
}

// Row 4: all viewers stutter, relay ingress loss RISING.
func legABroadcasterUplink() Rule {
	return Rule{
		ID:       "leg-a-broadcaster-uplink",
		Scope:    "broadcast",
		Requires: []string{"relay.ingressLossRatio"},
		Verdict:  "Leg A — the broadcaster's uplink is losing frames before the relay sees them",
		Action:   "Drop a ladder rung or the bitrate on the broadcaster.",
		Eval: func(f *Facts) *Finding {
			loss, _ := f.Relay("ingressLossRatio")
			if loss < ingressLossWarn {
				return nil
			}
			sev := SeverityWarn
			if loss >= ingressLossBad {
				sev = SeverityBad
			}
			return &Finding{
				Severity: sev, Confidence: 0.9,
				Evidence: []Evidence{
					{Signal: "ingressLossRatio", Value: loss, Unit: "ratio", From: FromRelay},
				},
			}
		},
	}
}

// Row 5: broadcaster's source is choppy, no network signals.
func encoderOverload() Rule {
	return Rule{
		ID:       "encoder-overload",
		Scope:    "broadcaster",
		Requires: []string{"client.captureFps", "client.encoderFps"},
		Verdict:  "Encoder overload — frames arrive faster than they encode",
		Action: "R4's auto ladder territory. On hardware encode paths watch the funnel gap: " +
			"encodeQueueSize under-fires there (docs/09 finding).",
		Eval: func(f *Facts) *Finding {
			capture, _ := f.Client("captureFps")
			encode, _ := f.Client("encoderFps")
			if capture <= 0 || encode >= capture*funnelGapRatio {
				return nil
			}
			ev := []Evidence{
				{Signal: "captureFps", Value: capture, Unit: "fps", From: FromClient},
				{Signal: "encoderFps", Value: encode, Unit: "fps", From: FromClient, Comparison: "below capture"},
			}
			// The queue depth corroborates but is not required: it is exactly
			// the signal docs/09 found under-fires on hardware encode.
			if q, ok := f.Client("encoderQueueDepth"); ok {
				ev = append(ev, Evidence{Signal: "encoderQueueDepth", Value: q, From: FromClient})
			}
			return &Finding{Severity: SeverityWarn, Confidence: 0.7, Evidence: ev}
		},
	}
}

// Row 6: encodes fine, viewers see low fps — the funnel splits send path from leg A.
func sendPathGap() Rule {
	return Rule{
		ID:       "send-path-gap",
		Scope:    "broadcaster",
		Requires: []string{"client.encoderFps", "client.sentFps"},
		Verdict:  "Send path — frames encode but do not leave the machine",
		Action:   "Compare against the relay's framesRelayed rate to split this from leg A.",
		Eval: func(f *Facts) *Finding {
			encode, _ := f.Client("encoderFps")
			sent, _ := f.Client("sentFps")
			if encode <= 0 || sent >= encode*funnelGapRatio {
				return nil
			}
			ev := []Evidence{
				{Signal: "encoderFps", Value: encode, Unit: "fps", From: FromClient},
				{Signal: "sentFps", Value: sent, Unit: "fps", From: FromClient, Comparison: "below encoded"},
			}
			conf := 0.7
			// The relay's own relayed rate is the anchor that turns this from
			// testimony into a corroborated verdict.
			if relayed, ok := f.Relay("framesRelayedPerSec"); ok {
				ev = append(ev, Evidence{Signal: "framesRelayedPerSec", Value: relayed, From: FromRelay})
				conf = 0.85
			}
			return &Finding{Severity: SeverityWarn, Confidence: conf, Evidence: ev}
		},
	}
}

// Row 7: viewer fps low, network clean → the decoder.
func decoderChoking() Rule {
	return Rule{
		ID:       "decoder-choking",
		Scope:    "viewer",
		Requires: []string{"client.receivedFps", "client.decoderFps"},
		Verdict:  "Decoder choking — likely a software-decode fallback",
		Action:   "Lower the rung or the framerate; check isHardwareAccelerated.",
		Eval: func(f *Facts) *Finding {
			received, _ := f.Client("receivedFps")
			decoded, _ := f.Client("decoderFps")
			if received <= 0 || decoded >= received*funnelGapRatio {
				return nil
			}
			ev := []Evidence{
				{Signal: "receivedFps", Value: received, Unit: "fps", From: FromClient},
				{Signal: "decoderFps", Value: decoded, Unit: "fps", From: FromClient, Comparison: "below received"},
			}
			conf := 0.6
			if hw, ok := f.Client("isHardwareAccelerated"); ok {
				ev = append(ev, Evidence{Signal: "isHardwareAccelerated", Value: hw, From: FromClient})
				if hw == 0 {
					conf = 0.75
				}
			}
			if q, ok := f.Client("decoderQueueDepth"); ok {
				ev = append(ev, Evidence{Signal: "decoderQueueDepth", Value: q, From: FromClient})
			}
			return &Finding{Severity: SeverityWarn, Confidence: conf, Evidence: ev}
		},
	}
}

// Row 8: smooth then freezes — where did it stop?
func stallAttribution() Rule {
	return Rule{
		ID:       "stall-attribution",
		Scope:    "viewer",
		Requires: []string{"client.timeSinceLastFrameMs"},
		Verdict:  "Playback stalled",
		Action:   "Check publisherActive: upstream stopped vs. this viewer's leg going dark.",
		Eval: func(f *Facts) *Finding {
			gap, _ := f.Client("timeSinceLastFrameMs")
			if gap < stallWarnMs {
				return nil
			}
			sev := SeverityWarn
			if gap >= stallBadMs {
				sev = SeverityBad
			}
			ev := []Evidence{
				{Signal: "timeSinceLastFrameMs", Value: gap, Unit: "ms", From: FromClient},
			}
			verdict := "Playback stalled — cause not yet attributed"
			conf := 0.5
			// The relay's publisher state is what splits "the broadcaster
			// stepped away" from "this viewer's leg went dark" — the two look
			// identical from the client alone, which is the whole reason
			// relay numbers anchor.
			if active, ok := f.Relay("publisherActive"); ok {
				ev = append(ev, Evidence{Signal: "publisherActive", Value: active, From: FromRelay})
				conf = 0.9
				if active == 0 {
					verdict = "Playback stalled because the broadcaster stopped sending — not a viewer problem"
					sev = SeverityWarn
				} else {
					verdict = "Playback stalled while the broadcaster was live — leg B for this viewer"
				}
			}
			// A live inbound keepalive while frames are dead is the stream
			// wedge, not an outage (BUGS.md / docs/27).
			if inbound, ok := f.Client("timeSinceLastInboundMs"); ok && inbound < gap/2 {
				ev = append(ev, Evidence{
					Signal: "timeSinceLastInboundMs", Value: inbound, Unit: "ms",
					From: FromClient, Comparison: "far below the frame gap — the session is alive but media is not",
				})
			}
			return &Finding{Severity: sev, Verdict: verdict, Confidence: conf, Evidence: ev}
		},
	}
}

// Row 9: frequent "Awaiting keyframe" / gap resyncs on one viewer.
func keyframeGapChurn() Rule {
	return Rule{
		ID:       "keyframe-gap-churn",
		Scope:    "viewer",
		Requires: []string{"client.reorderGapResyncs", "client.keyframeStreamsReceived"},
		Verdict:  "Delta loss on leg B is eating GOPs",
		Action: "Keyframe cadence + reliable streams bound recovery; if lastKeyframeAgeMs is far " +
			"above the 500 ms GOP, something is wrong at the relay instead.",
		Eval: func(f *Facts) *Finding {
			resyncs, _ := f.Client("reorderGapResyncs")
			keyframes, _ := f.Client("keyframeStreamsReceived")
			// One resync per keyframe is the signature: every GOP is being
			// broken. Fewer is ordinary loss.
			if keyframes < 5 || resyncs < keyframes*0.5 {
				return nil
			}
			ev := []Evidence{
				{Signal: "reorderGapResyncs", Value: resyncs, From: FromClient, Comparison: "vs keyframes received"},
				{Signal: "keyframeStreamsReceived", Value: keyframes, From: FromClient},
			}
			conf := 0.6
			if slow, ok := f.Relay("keyframesDropped"); ok {
				ev = append(ev, Evidence{Signal: "subscriber.keyframesDropped", Value: slow, From: FromRelay})
				conf = 0.8
			}
			return &Finding{Severity: SeverityWarn, Confidence: conf, Evidence: ev}
		},
	}
}

// Row 10: nothing plays for anyone → config/limits, not media.
func configOrLimits() Rule {
	return Rule{
		ID:       "config-or-limits",
		Scope:    "broadcast",
		Requires: []string{"relay.publisherActive", "relay.framesRelayedPerSec"},
		Verdict:  "Config or limits, not media — a publisher is attached but nothing is being relayed",
		Action:   "Check connection outcomes: 401 (secret), 404 (bad ID), 429 (limits), origin_rejected (CORS).",
		Eval: func(f *Facts) *Finding {
			active, _ := f.Relay("publisherActive")
			rate, _ := f.Relay("framesRelayedPerSec")
			if active == 0 || rate > 0 {
				return nil
			}
			return &Finding{
				Severity: SeverityBad, Confidence: 0.8,
				Evidence: []Evidence{
					{Signal: "publisherActive", Value: active, From: FromRelay},
					{Signal: "framesRelayedPerSec", Value: rate, Unit: "fps", From: FromRelay},
				},
			}
		},
	}
}

// Row 11: resilient-mode viewer (R19) stutters anyway.
func resilientUndersupply() Rule {
	return Rule{
		ID:       "resilient-undersupply",
		Scope:    "viewer",
		Requires: []string{"client.playoutOffsetMs", "text.deliveryMode"},
		Verdict:  "Sustained undersupply, not loss — the link cannot carry the stream bitrate",
		Action: "Reliable delivery cannot create bandwidth (docs/24). Lower the rung or bitrate. " +
			"If the mode reads 'reliable requested / datagrams served', the relay predates R19 X2.",
		Eval: func(f *Facts) *Finding {
			mode, _ := f.Text("deliveryMode")
			if mode != "reliable" && mode != "dvr" {
				return nil
			}
			offset, _ := f.Client("playoutOffsetMs")
			// The signature is the buffer pinned at its clamp while cadence
			// stays bad: the mode has spent everything it has.
			if offset < resilientClampMs-playoutClampSlackMs {
				return nil
			}
			ev := []Evidence{
				{Signal: "playoutOffsetMs", Value: offset, Unit: "ms", From: FromClient, Comparison: "pinned at the resilient clamp"},
			}
			conf := 0.6
			if dropped, ok := f.Relay("carrierRecordsDropped"); ok && dropped > 0 {
				ev = append(ev, Evidence{Signal: "carrierRecordsDropped", Value: dropped, From: FromRelay})
				conf = 0.85
			}
			return &Finding{Severity: SeverityWarn, Confidence: conf, Evidence: ev}
		},
	}
}

// Row 12: resilient viewer freezes to keyframes repeatedly (docs/24 finding 11).
func carrierQueueOverflow() Rule {
	return Rule{
		ID:       "carrier-queue-overflow",
		Scope:    "viewer",
		Requires: []string{"relay.carrierQueueOverflow"},
		Verdict:  "The carrier drain cannot keep up for this viewer — deltas are dropped BEFORE reaching the reliable stream",
		Action: "Distinct from carrierRecordsDropped (the carrier itself stalled) and from plain " +
			"queue_full (a normal slow viewer). The viewer sees holes in a stream it trusts as in-order.",
		Eval: func(f *Facts) *Finding {
			overflow, _ := f.Relay("carrierQueueOverflow")
			if overflow == 0 {
				return nil
			}
			return &Finding{
				Severity: SeverityBad, Confidence: 0.9,
				Evidence: []Evidence{
					{Signal: "carrierQueueOverflow", Value: overflow, From: FromRelay},
				},
			}
		},
	}
}

// Row 13: broadcaster shows fewer viewers than expected (R18).
func viewerCountGap() Rule {
	return Rule{
		ID:       "viewer-count-gap",
		Scope:    "broadcast",
		Requires: []string{"relay.viewersGlobal", "relay.subscribersFleetTotal"},
		Verdict:  "Edge report / aggregation gap — the origin's global count is below the fleet's actual subscribers",
		Action:   "Check the origin's edgeSessions against the edges' subscribers, and edge logs for re-attach churn.",
		Eval: func(f *Facts) *Finding {
			global, _ := f.Relay("viewersGlobal")
			fleet, _ := f.Relay("subscribersFleetTotal")
			if fleet == 0 || global >= fleet {
				return nil
			}
			return &Finding{
				Severity: SeverityWarn, Confidence: 0.85,
				Evidence: []Evidence{
					{Signal: "viewersGlobal", Value: global, From: FromRelay, Comparison: "below the fleet total"},
					{Signal: "subscribersFleetTotal", Value: fleet, From: FromRelay},
				},
			}
		},
	}
}

// Row 14: DVR viewer (R21) freezes despite ring-backed delivery.
func dvrRingOutlived() Rule {
	return Rule{
		ID:       "dvr-ring-outlived",
		Scope:    "viewer",
		Requires: []string{"relay.dvrResyncs"},
		Verdict:  "Stalls are outliving the ring — the cursor fell off the tail",
		Action: "Raise -dvr-window, or accept it: covering a stall S with buffer B needs B/(B-S)x " +
			"burst bandwidth, so the buffer must strictly EXCEED the stall.",
		Eval: func(f *Facts) *Finding {
			resyncs, _ := f.Relay("dvrResyncs")
			if resyncs == 0 {
				return nil
			}
			ev := []Evidence{{Signal: "dvrResyncs", Value: resyncs, From: FromRelay}}
			// The playbook's own caveat, encoded: lag with resyncs FLAT is the
			// mode working exactly as designed, not a fault.
			if lag, ok := f.Relay("dvrLagMs"); ok {
				ev = append(ev, Evidence{
					Signal: "dvrLagMs", Value: lag, Unit: "ms", From: FromRelay,
					Comparison: "lag alone is the feature; resyncs are the mode's only frame loss",
				})
			}
			return &Finding{Severity: SeverityWarn, Confidence: 0.85, Evidence: ev}
		},
	}
}

// Beyond the 14 rows: docs/20 finding 8's concealment-vs-overflow ratio, which
// took a human noticing two counters together. Institutionalizing it is
// exactly what D6 says new rules are for.
func audioOverflowLatch() Rule {
	return Rule{
		ID:       "audio-overflow-latch",
		Scope:    "viewer",
		Requires: []string{"client.audioOverflowDrops", "client.audioPacketsReceived"},
		Verdict:  "Audio overflow latch — the buffer is shedding real audio and replacing it with silence",
		Action: "docs/20 finding 8: overflow drops that cannot lower the depth, concealed by silence " +
			"that re-adds it. Check gapsConcealed against overflowDrops and the context sample rate.",
		Eval: func(f *Facts) *Finding {
			drops, _ := f.Client("audioOverflowDrops")
			received, _ := f.Client("audioPacketsReceived")
			if received < 50 || drops < received*audioOverflowLatchRatio {
				return nil
			}
			ev := []Evidence{
				{Signal: "audioBuffer.overflowDrops", Value: drops, From: FromClient, Comparison: "share of arrivals"},
				{Signal: "audioPacketsReceived", Value: received, From: FromClient},
			}
			// The RATIO of concealment to overflow is what identified the
			// original bug; carrying both is what makes the evidence legible.
			if concealed, ok := f.Client("audioGapsConcealed"); ok {
				ev = append(ev, Evidence{Signal: "audioBuffer.gapsConcealed", Value: concealed, From: FromClient})
			}
			return &Finding{Severity: SeverityWarn, Confidence: 0.6, Evidence: ev}
		},
	}
}
