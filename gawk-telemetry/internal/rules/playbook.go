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
	// D16: where an intermittent collapse stops being "degraded" and becomes
	// "broken from a viewer's point of view". Mirrors of the rollup's
	// DipShareBad/DipCountBad — kept as rule-side constants for the same reason
	// every other threshold here is: the live dashboard evaluates these SAME
	// rules over a rolling window, and a threshold that drifted between the two
	// would put two disagreeing truths about one stream in front of an operator.
	dipShareBad = 0.1
	dipCountBad = 4
	// D17: how far under its configured target a broadcast may sit before the
	// gap is worth naming. Same shape as funnelGapRatio and deliberately the
	// same value — "a quarter down on what was asked for" is the same size of
	// discrepancy whether the comparison is between two funnel stages or
	// between intent and outcome.
	targetShortfallRatio = 0.75
	// docs/34 finding 3: a delta frame is packetized into ~9-10 datagrams plus
	// two parity symbols, so a browser receive queue shallower than this cannot
	// hold one frame's burst and sheds its head every time. Firefox 154
	// defaults to 1, measured.
	minSafeDatagramQueue = 16
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
		parityIneffective(),
		burstThresholdLoss(),
		configOrLimits(),
		resilientUndersupply(),
		carrierQueueOverflow(),
		viewerCountGap(),
		dvrRingOutlived(),
		audioOverflowLatch(),
		intermittentFpsDips(),
		keyframeOnlyDelivery(),
		deliveredBelowTarget(),
	}
}

// The steady-state miss (docs/33 D17), and the exact complement of the dip
// rules above. Those are SELF-RELATIVE: they catch a stream falling below its
// own baseline. They structurally cannot catch a stream whose baseline was
// wrong all along — a broadcaster that asked for 60 fps and delivered 30 for
// the whole session has a flat baseline, zero episodes, and a perfect funnel
// (capture 30 → encode 30 → sent 30). Every other rule passes it.
//
// What makes this answerable at all is that the target is now recorded. What
// makes it HONEST is the capture comparison below: gawk's capture is
// damage-driven, so a motionless screen legitimately produces far fewer frames
// than the target, and a rule that read a shortfall as a fault would accuse
// every quiet stream on the fleet.
func deliveredBelowTarget() Rule {
	return Rule{
		ID:       "delivered-below-target",
		Scope:    "broadcaster",
		Requires: []string{"client.targetFps", "client.sentFps"},
		Verdict:  "Delivered rate is below what this broadcast was configured for",
		Action: "Compare against captureFps in the evidence: capture below target too means the " +
			"SOURCE is not producing frames (a static screen does this legitimately, and R4's auto " +
			"ladder deliberately does not step for it — docs/09). Capture at target with sent below " +
			"it is a pipeline problem, and encoder-overload / send-path-gap localize it.",
		Why: "The steady-state miss. Every other rule is self-relative and therefore blind to a " +
			"stream whose baseline was wrong from the first frame: a broadcaster that asked for " +
			"60 fps and delivered 30 throughout has a flat baseline, no dip episodes and a " +
			"perfect funnel. This compares OUTCOME against INTENT, which is only possible because " +
			"D17 records the target. Capture is read beside it because gawk's capture is " +
			"damage-driven — a motionless screen legitimately produces far fewer frames than the " +
			"target, and reading that as a fault would accuse every quiet stream.",
		Thresholds: []Threshold{
			{Name: "targetShortfallRatio", Value: targetShortfallRatio, Unit: "ratio", Note: "sent below this share of the configured target"},
		},
		Eval: func(f *Facts) *Finding {
			target, _ := f.Client("targetFps")
			sent, _ := f.Client("sentFps")
			if target <= 0 || sent >= target*targetShortfallRatio {
				return nil
			}
			ev := []Evidence{
				{Signal: "targetFps", Value: target, Unit: "fps", From: FromClient, Comparison: "configured target"},
				{Signal: "sentFps", Value: sent, Unit: "fps", From: FromClient, Comparison: "below target"},
			}
			verdict := "Delivered rate is below the configured target, and capture is keeping up — " +
				"the shortfall is in the pipeline, not the source"
			conf := 0.6
			sourceLimited := false
			if capture, ok := f.Client("captureFps"); ok {
				ev = append(ev, Evidence{Signal: "captureFps", Value: capture, Unit: "fps", From: FromClient})
				if capture < target*targetShortfallRatio {
					// Source-limited. Worth SAYING — a broadcaster who picked
					// 60 should know they are getting 30 — but it is not a
					// fault, and must not read as one.
					sourceLimited = true
					verdict = "Capture is not producing the configured target rate — source-limited, " +
						"which a static or low-motion screen does legitimately"
					conf = 0.5
				}
			}
			// The encoder's committed acceleration, read opportunistically and
			// deliberately NOT in Requires: the native engine (R14) reports its
			// encoder by name and no acceleration string, and a required signal
			// would make this whole rule dead for every native broadcaster.
			//
			// It only narrows a PIPELINE shortfall. On a source-limited one the
			// encoder is keeping up with everything it is given, so naming it
			// would point at the wrong stage — which is the failure mode this
			// rule's whole shape exists to avoid.
			if accel, ok := f.Text("acceleration"); ok && !sourceLimited {
				ev = append(ev, Evidence{
					Signal: "acceleration", Text: accel, From: FromClient,
					Comparison: "the acceleration the encoder committed to at configure time",
				})
				if accel == "software" {
					verdict = "Delivered rate is below the configured target while encoding in " +
						"SOFTWARE — the encoder is the first thing to suspect"
					conf = 0.75
				}
			}
			// The relay's own relayed rate turns this from the client's account
			// of itself into a corroborated one (D7).
			if relayed, ok := f.Relay("framesRelayedPerSec"); ok {
				ev = append(ev, Evidence{Signal: "framesRelayedPerSec", Value: relayed, From: FromRelay})
				conf += 0.2
			}
			// Never `bad`: a shortfall against a target is a discrepancy worth
			// surfacing, not a broken stream. The rules that describe an
			// actually-broken one are already in the playbook above.
			return &Finding{Severity: SeverityWarn, Verdict: verdict, Confidence: conf, Evidence: ev}
		},
	}
}

// Beyond the playbook: the dips a median hides (docs/33 D16). These two exist
// because the audit that produced D16 found a stream collapsing to 2 fps every
// twenty seconds diagnosing as `healthy` — every funnel rule read the session
// median (30 fps, fine), `stall-attribution` never fired (2 fps is a 500 ms
// gap, under the 1000 ms threshold) and `keyframe-gap-churn`'s ratio was
// diluted from ~1.0 inside the dips to ~0.02 across the session.
//
// The split between them is the point: the first says THAT it happened, the
// second says WHY. A verdict that only says "your framerate dipped" tells an
// operator what they already knew.

// Dips: the collapse itself.
func intermittentFpsDips() Rule {
	return Rule{
		ID:       "intermittent-fps-dips",
		Scope:    "viewer",
		Requires: []string{"client.fpsDipEpisodes"},
		Verdict:  "Intermittent frame-rate collapse — episodes a median hides",
		Action: "Look at keyframe-only-delivery beside this: if that fired, the cause is delta loss " +
			"eating GOPs. If it did not, compare the dips against the broadcaster's sentFps — a " +
			"source-side stutter reaches every viewer.",
		Why: "The collapse a median hides. A session holding 30 fps that drops to the 500 ms GOP " +
			"cadence for six seconds a minute has a median of 30, and every funnel rule reading " +
			"that median passes. The dip detector measures episodes instead — distinct collapses " +
			"below half the session's own baseline — so 'it stutters every now and then' becomes " +
			"a number. Says THAT it happened; keyframe-only-delivery says why.",
		Thresholds: []Threshold{
			{Name: "dipShareBad", Value: dipShareBad, Unit: "ratio", Note: "share of the window spent collapsed that makes it bad rather than degraded"},
			{Name: "dipCountBad", Value: dipCountBad, Unit: "episodes", Note: "separate collapses that make it bad"},
		},
		Eval: func(f *Facts) *Finding {
			count, _ := f.Client("fpsDipEpisodes")
			if count == 0 {
				return nil
			}
			share, _ := f.Client("fpsDipShare")
			// Degraded is one thing; a tenth of the window spent collapsed, or
			// four separate collapses, is broken from a viewer's seat.
			sev := SeverityWarn
			if share >= dipShareBad || count >= dipCountBad {
				sev = SeverityBad
			}
			ev := []Evidence{
				{Signal: "fpsDipEpisodes", Value: count, From: FromClient, Comparison: "distinct collapses in the window"},
			}
			if worst, ok := f.Client("fpsDipWorstFps"); ok {
				ev = append(ev, Evidence{Signal: "fpsDipWorstFps", Value: worst, Unit: "fps", From: FromClient})
			}
			if share > 0 {
				ev = append(ev, Evidence{Signal: "fpsDipShare", Value: share, Unit: "ratio", From: FromClient,
					Comparison: "of the window spent below half the baseline"})
			}
			if longest, ok := f.Client("fpsDipLongestMs"); ok {
				ev = append(ev, Evidence{Signal: "fpsDipLongestMs", Value: longest, Unit: "ms", From: FromClient})
			}
			conf := 0.6
			// A dip the relay ALSO saw is anchored testimony rather than a
			// client's account of itself (D7) — and it localizes the leg.
			if dropped, ok := f.Relay("subscriberDropped"); ok && dropped > 0 {
				ev = append(ev, Evidence{Signal: "subscriberDropped", Value: dropped, From: FromRelay,
					Comparison: "the relay dropped for this subscriber too"})
				conf = 0.85
			}
			return &Finding{Severity: sev, Confidence: conf, Evidence: ev}
		},
	}
}

// Dips: the cause, for the commonest one. Playbook row 9's physics, measured
// inside the dip window instead of across a session that dilutes it.
//
// This does NOT replace keyframe-gap-churn: a stream broken CONTINUOUSLY should
// still fire that one, and both firing together is a coherent statement, not a
// duplicate.
func keyframeOnlyDelivery() Rule {
	return Rule{
		ID:       "keyframe-only-delivery",
		Scope:    "viewer",
		Requires: []string{"client.fpsDipResyncs", "client.fpsDipKeyframes"},
		Verdict: "Delta loss is eating whole GOPs — only keyframes are surviving, so the viewer sees " +
			"the keyframe cadence as its framerate",
		Action: "This is leg B for this viewer at the datagram level. R19 resilient mode (reliable " +
			"carriers) is the designed answer; a lower rung or bitrate reduces the exposure.",
		Why: "Playbook row 9's physics, measured INSIDE the dip window rather than across a " +
			"session that dilutes it. One gap resync per keyframe received means every GOP is " +
			"being broken by delta loss, so the viewer sees the keyframe cadence as their " +
			"framerate. Two keyframes is the least that can establish the ratio at all — below " +
			"that one unlucky GOP would read as a pattern.",
		Thresholds: []Threshold{
			{Name: "keyframeFloor", Value: 2, Unit: "keyframes", Note: "the fewest keyframes that can establish the ratio"},
			{Name: "resyncRatio", Value: 0.5, Unit: "ratio", Note: "resyncs per keyframe above which every GOP is being broken"},
		},
		Eval: func(f *Facts) *Finding {
			resyncs, _ := f.Client("fpsDipResyncs")
			keyframes, _ := f.Client("fpsDipKeyframes")
			// Two keyframes is the least that can establish a ratio at all;
			// below it a single unlucky GOP would read as a pattern.
			if keyframes < 2 || resyncs < keyframes*0.5 {
				return nil
			}
			ev := []Evidence{
				{Signal: "fpsDipResyncs", Value: resyncs, From: FromClient,
					Comparison: "gap resyncs during the dips, vs keyframes received"},
				{Signal: "fpsDipKeyframes", Value: keyframes, From: FromClient},
			}
			conf := 0.6
			if dropped, ok := f.Relay("keyframesDropped"); ok {
				ev = append(ev, Evidence{Signal: "subscriber.keyframesDropped", Value: dropped, From: FromRelay})
				conf = 0.8
			}
			return &Finding{Severity: SeverityBad, Confidence: conf, Evidence: ev}
		},
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
		Why: "One viewer stutters and the others do not. The discriminator is the CONTRAST: this " +
			"subscriber's drops far above its peers' while the relay's own ingress is clean. " +
			"Either half alone means something else entirely — clean ingress with uniform drops " +
			"is relay egress, and dirty ingress is leg A whatever the subscribers look like.",
		Thresholds: []Threshold{
			{Name: "outlierDropRatio", Value: outlierDropRatio, Unit: "x peer median", Note: "how far above its peers a subscriber must drop to be an outlier"},
			{Name: "ingressLossWarn", Value: ingressLossWarn, Unit: "ratio", Note: "ingress loss above which this is not a single-viewer problem"},
		},
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
		Why: "The relay shedding BY POLICY rather than by failure. Bandwidth-attributed egress " +
			"drops with clean ingress is a configured cap doing exactly what it was told to; " +
			"reading it as a fault sends an operator hunting a network problem that does not " +
			"exist.",
		Thresholds: []Threshold{
			{Name: "ingressLossWarn", Value: ingressLossWarn, Unit: "ratio", Note: "ingress loss above which this is leg A instead"},
		},
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
		Why: "ALL subscribers dropping uniformly with clean ingress — a shared bottleneck at the " +
			"relay or on its uplink. 'All of them', not 'several', is the whole point of the row: " +
			"a set of unlucky viewers is a different problem with a different fix, and the " +
			"threshold that separates them is the word 'every'.",
		Thresholds: []Threshold{
			{Name: "minSubscribers", Value: 2, Unit: "subscribers", Note: "below this there is no 'all of them' to observe"},
			{Name: "ingressLossWarn", Value: ingressLossWarn, Unit: "ratio", Note: "ingress loss above which this is leg A instead"},
		},
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
		Why: "The relay is losing frames BEFORE it ever sees a complete one — the broadcaster's " +
			"uplink, not anything downstream. R9 M3's ingress loss is already windowed and " +
			"smoothed, so a small nonzero value here is real rather than noise.",
		Thresholds: []Threshold{
			{Name: "ingressLossWarn", Value: ingressLossWarn, Unit: "ratio", Note: "above this, ingress loss is real rather than noise"},
			{Name: "ingressLossBad", Value: ingressLossBad, Unit: "ratio", Note: "above this it is broken rather than degraded"},
		},
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
		Why: "Frames arrive from capture faster than the encoder can consume them. The funnel gap " +
			"is the signal, not the queue depth: docs/09 found encodeQueueSize under-fires on " +
			"hardware encode paths, so a rule requiring it would be silent exactly where hardware " +
			"encode is the happy path.",
		Thresholds: []Threshold{
			{Name: "funnelGapRatio", Value: funnelGapRatio, Unit: "ratio", Note: "encoded below this share of captured is a bottleneck rather than variance"},
		},
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
		Why: "Frames encode but do not leave the machine. Splitting this from leg A needs the " +
			"relay's own relayed rate — the client alone cannot tell 'I did not send' from 'the " +
			"network ate it', which is why the relay number lifts the confidence when it is " +
			"there.",
		Thresholds: []Threshold{
			{Name: "funnelGapRatio", Value: funnelGapRatio, Unit: "ratio", Note: "sent below this share of encoded is a bottleneck rather than variance"},
		},
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
		Why: "Frames arrive and do not get decoded — typically a software-decode fallback. Both " +
			"sides of the ratio are medians, deliberately: an earlier version compared a median " +
			"against a p05 and fired on a clean 30 fps loopback stream, which is how a " +
			"confidently wrong verdict gets manufactured.",
		Thresholds: []Threshold{
			{Name: "funnelGapRatio", Value: funnelGapRatio, Unit: "ratio", Note: "decoded below this share of received is a bottleneck rather than variance"},
		},
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
		Why: "Media stopped. Attribution is the whole value: 'the broadcaster stepped away' and " +
			"'this viewer's leg went dark' are indistinguishable from the client alone, and the " +
			"relay's publisher state is what separates them. A live inbound keepalive while " +
			"frames are dead is a third thing again — the stream wedge, not an outage.",
		Thresholds: []Threshold{
			{Name: "stallWarnMs", Value: stallWarnMs, Unit: "ms", Note: "two GOPs at the 500 ms cadence — a freeze a viewer feels"},
			{Name: "stallBadMs", Value: stallBadMs, Unit: "ms", Note: "above this it is broken rather than degraded"},
		},
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
		Why: "Delta loss on leg B eating whole GOPs, measured across the session rather than " +
			"inside a dip. One resync per keyframe is the signature; fewer is ordinary loss. Five " +
			"keyframes is the floor for the ratio to mean anything.",
		Thresholds: []Threshold{
			{Name: "keyframeFloor", Value: 5, Unit: "keyframes", Note: "the fewest keyframes that make the ratio meaningful"},
			{Name: "resyncRatio", Value: 0.5, Unit: "ratio", Note: "resyncs per keyframe above which every GOP is being broken"},
		},
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

// R29 (docs/34 §7.3): parity is being served and the viewer is STILL losing
// frames.
//
// The whole value of this rule is in its action text, because the two causes
// call for opposite responses:
//
//   - loss above what k covers → raise the fleet parity level;
//   - BURSTY loss → raising k is useless. A burst inside one frame consumes
//     several of its chunks at once, which is precisely the failure mode a
//     per-frame code cannot cover at any k (docs/34 §9 kill criterion 1 puts
//     the burst model at 37 % of GOPs still damaged even at k=4). The answer
//     there is Resilient mode, whose QUIC retransmission does not care how
//     the loss is distributed.
//
// The discriminator is how much parity RECOVERED relative to how much
// arrived. An under-provisioned code still repairs plenty and simply cannot
// keep up; a per-frame code facing bursts repairs almost nothing, because the
// erasures cluster into the frames whose symbols they also took out.
func parityIneffective() Rule {
	return Rule{
		ID:       "parity-ineffective",
		Scope:    "viewer",
		Requires: []string{"client.parityChunksReceived", "client.framesDroppedIncomplete"},
		Verdict:  "Forward parity is being served and frames are still being lost",
		Action: "Compare framesRecoveredByParity against parityChunksReceived in the evidence: a " +
			"code that is merely under-provisioned still recovers plenty, and raising the fleet " +
			"parity level helps. One recovering almost nothing is facing BURSTY loss, which no " +
			"per-frame code covers at any level — route that viewer to Resilient mode instead. " +
			"If burst-threshold-loss fired for the same session, read that first: R30 striping is " +
			"the designed answer to the per-connection threshold shape.",
		Why: "R29's forward parity is being served and frames are STILL being lost. The " +
			"discriminator is how much parity recovered relative to how much arrived: an " +
			"under-provisioned code still repairs plenty and simply cannot keep up, so raising " +
			"the fleet level helps; a per-frame code facing BURSTY loss repairs almost nothing, " +
			"because the erasures cluster into the very frames whose symbols they also took out — " +
			"and no k fixes that.",
		Thresholds: []Threshold{
			{Name: "symbolFloor", Value: 50, Unit: "chunks", Note: "parity has to actually be in play"},
			{Name: "incompleteFloor", Value: 10, Unit: "frames", Note: "enough loss for the ratio to mean anything"},
			{Name: "recoveryRatio", Value: 0.25, Unit: "ratio", Note: "losses below this share of recoveries are the structural residue every k has"},
			{Name: "minSafeDatagramQueue", Value: minSafeDatagramQueue, Unit: "datagrams", Note: "a receive queue shallower than one frame's burst sheds its head every time (docs/34 finding 3)"},
		},
		Eval: func(f *Facts) *Finding {
			symbols, _ := f.Client("parityChunksReceived")
			incomplete, _ := f.Client("framesDroppedIncomplete")
			// Parity has to actually be in play, and there has to be enough
			// traffic for the ratio below to mean anything.
			if symbols < 50 || incomplete < 10 {
				return nil
			}
			recovered, _ := f.Client("framesRecoveredByParity")
			// Still losing a material share of what parity was asked to
			// protect. Below this the code is working and the residue is the
			// structural one every k has (docs/34 §11).
			if incomplete < recovered*0.25 {
				return nil
			}
			ev := []Evidence{
				{Signal: "parityChunksReceived", Value: symbols, From: FromClient},
				{Signal: "framesRecoveredByParity", Value: recovered, From: FromClient,
					Comparison: "frames parity actually repaired"},
				{Signal: "framesDroppedIncomplete", Value: incomplete, From: FromClient,
					Comparison: "frames lost despite parity"},
			}
			// R29 finding 3 (docs/34): a THIRD cause, checked first because it
			// mimics the under-provisioned signature exactly and calls for the
			// opposite response. A shallow browser receive queue drops from
			// the HEAD of each frame's burst, so the parity symbols — written
			// last — survive while the data chunks ahead of them die in runs.
			// Recovery then looks respectable and "raise the fleet level"
			// spends uplink on every viewer to fix a defect in one client.
			//
			// Gated on the browser's OWN reported depth, so this is a fact
			// rather than an inference from the ratio it shares with the
			// under-provisioned case.
			depth, haveDepth := f.Client("datagramBufferDefault")
			governs, _ := f.Client("datagramBufferGovernsDrops")
			if haveDepth && depth < minSafeDatagramQueue {
				ev = append(ev,
					Evidence{Signal: "datagramBufferDefault", Value: depth, From: FromClient,
						Comparison: "datagrams the browser buffers, against a frame burst of ~10"},
					Evidence{Signal: "datagramBufferGovernsDrops", Value: governs, From: FromClient,
						Comparison: "whether this browser exposes the attribute that moves the drop threshold"})
				return &Finding{
					ID:         "parity-ineffective",
					Severity:   SeverityWarn,
					Confidence: 0.7,
					Verdict: "This browser's datagram receive queue is shallower than one frame — " +
						"it is dropping the head of each burst, which parity cannot repair",
					Evidence: ev,
					Action: "Not a parity level problem: the symbols arrive intact (they are sent last) " +
						"while the chunks ahead of them die in runs, so raising the fleet level buys " +
						"almost nothing. Route this viewer to Resilient or Deep-buffer mode — those " +
						"carry video on reliable streams and never touch the datagram queue.",
				}
			}
			// The verdict splits on the discriminator, so the operator reads
			// the cause rather than deriving it.
			verdict := "Forward parity is under-provisioned — loss exceeds what this level repairs"
			if recovered < incomplete*0.5 {
				verdict = "Forward parity is recovering almost nothing — the loss is bursty, and no per-frame code covers that"
				ev = append(ev, Evidence{
					Signal: "framesRecoveredByParity", Value: recovered, From: FromDerived,
					Comparison: "far below the frames lost — the signature of clustered rather than independent loss",
				})
			}
			return &Finding{Severity: SeverityWarn, Verdict: verdict, Confidence: 0.6, Evidence: ev}
		},
	}
}

// R30 (docs/35 §7): the finding-4 signature — chunks of LARGE frames dying
// while small frames stay clean. That shape is a per-connection burst
// buffer overflowing (docs/34 finding 4: a threshold at ~8 packets,
// head-of-burst, parity spared), not a lossy network: uniform loss takes
// small frames too and never matches. The two action branches depend on
// whether striping is already active, because they call for opposite
// responses — an idle stripe should engage (check the capability, the
// subscriber caps, the viewer's mode), while an ACTIVE stripe that still
// shows the signature means the per-connection composition does not hold on
// that path and the answer is Resilient mode, never more legs (docs/35 §10
// kill criterion 1's field echo).
func burstThresholdLoss() Rule {
	return Rule{
		ID:       "burst-threshold-loss",
		Scope:    "viewer",
		Requires: []string{"client.stripeLargeLossPct", "client.stripeLargeChunks"},
		Verdict:  "Large frames are losing chunks while small frames arrive clean — the burst-threshold shape",
		Action: "This is a per-connection receive-buffer overflow, not a lossy network. If striping is " +
			"not active, find out why (relay capability, subscriber caps, the viewer's Striping menu " +
			"setting); if it IS active and the loss persists, the split is not buying headroom on this " +
			"path — route the viewer to Resilient mode.",
		Why: "R30's shape: large frames losing chunks while small frames arrive clean. That is a " +
			"per-connection receive-buffer overflow, not a lossy network — a lossy network does " +
			"not distinguish frames by size. An INACTIVE stripe with this signature means " +
			"striping should be engaging and is not; an ACTIVE stripe that still shows it means " +
			"the split is not buying headroom on that path and the answer is Resilient mode, " +
			"never more legs.",
		Thresholds: []Threshold{
			{Name: "largeChunkFloor", Value: 500, Unit: "chunks", Note: "enough large-frame traffic for the percentages to mean anything"},
			{Name: "largeLossPct", Value: 1.0, Unit: "%", Note: "large-frame chunk loss above which the shape is real"},
			{Name: "smallLossPct", Value: 0.1, Unit: "%", Note: "small-frame loss must be below this, or the loss is uniform and this rule has nothing true to say"},
		},
		Eval: func(f *Facts) *Finding {
			largePct, haveLarge := f.Client("stripeLargeLossPct")
			largeChunks, _ := f.Client("stripeLargeChunks")
			if !haveLarge || largeChunks < 500 || largePct < 1.0 {
				return nil
			}
			smallPct, haveSmall := f.Client("stripeSmallLossPct")
			if !haveSmall || smallPct > 0.1 {
				// Shape unproven (no small-frame evidence) or uniform loss —
				// either way this rule has nothing true to say; the parity
				// and delivery rules own those cases.
				return nil
			}
			active, _ := f.Client("stripeActive")
			ev := []Evidence{
				{Signal: "stripeLargeLossPct", Value: largePct, From: FromClient,
					Comparison: "chunk loss on frames past the ~8-datagram threshold"},
				{Signal: "stripeSmallLossPct", Value: smallPct, From: FromClient,
					Comparison: "loss on frames under it — clean is the threshold signature"},
				{Signal: "stripeLargeChunks", Value: largeChunks, From: FromClient},
				{Signal: "stripeActive", Value: active, From: FromClient,
					Comparison: "stripe legs carrying deltas while this was measured"},
			}
			if active > 0 {
				return &Finding{
					Severity:   SeverityWarn,
					Confidence: 0.6,
					Verdict: "Striping is active and the burst-threshold loss persists — the " +
						"per-connection split is not buying headroom on this path",
					Evidence: ev,
					Action: "More legs will not help (docs/35 §10 criterion 1): route this viewer to " +
						"Resilient mode, whose reliable carriers retransmit regardless of burst shape.",
				}
			}
			return &Finding{
				Severity:   SeverityWarn,
				Confidence: 0.6,
				Verdict: "The path shows per-connection burst-threshold loss and striping is not " +
					"engaged",
				Evidence: ev,
				Action: "Striping should be engaging here. Check, in order: the relay advertises " +
					"CAP_STRIPED_DELIVERY (striped-delivery flag on), the subscriber caps have room " +
					"for legs, and the viewer's Striping menu setting is not 'off'.",
			}
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
		Why: "A publisher is attached and nothing is being relayed. Nothing plays for anyone, and " +
			"no media signal explains it — which is what makes it a configuration or limits " +
			"problem rather than a delivery one.",
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
		Why: "An R19 resilient viewer stuttering anyway. The signature is the playout buffer " +
			"PINNED at its clamp while cadence stays bad: the mode has spent everything it has. " +
			"Reliable delivery cannot create bandwidth (docs/24), so this is a bitrate problem " +
			"wearing a delivery problem's clothes.",
		Thresholds: []Threshold{
			{Name: "resilientClampMs", Value: resilientClampMs, Unit: "ms", Note: "the resilient mode's playout clamp"},
			{Name: "playoutClampSlackMs", Value: playoutClampSlackMs, Unit: "ms", Note: "how close to the clamp counts as pinned"},
		},
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
		Why: "docs/24 finding 11: the carrier drain cannot keep up, so deltas are dropped BEFORE " +
			"they reach the reliable stream. Distinct from carrierRecordsDropped (the carrier " +
			"itself stalled) and from plain queue_full (an ordinary slow viewer) — this viewer " +
			"sees holes in a stream it is entitled to trust as in-order, which is the worst of " +
			"the three.",
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
		Why: "R18's count, disagreeing with itself across the fleet: the origin's global figure is " +
			"below what the edges actually hold. An aggregation or edge-report gap, and at " +
			"replicas 1 it cannot fire at all.",
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
		Why: "An R21 DVR viewer freezing despite ring-backed delivery: the cursor fell off the " +
			"tail. The playbook's own caveat is encoded here — LAG alone is the feature working " +
			"exactly as designed, and resyncs are the mode's only real frame loss.",
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
		Why: "docs/20 finding 8, institutionalized. Overflow drops that cannot lower the buffer " +
			"depth, concealed by silence that re-adds it — the buffer sheds real audio and " +
			"replaces it with nothing. It took a human noticing two counters together; the ratio " +
			"of concealment to overflow is what made it legible, so both ride the evidence.",
		Thresholds: []Threshold{
			{Name: "audioOverflowLatchRatio", Value: audioOverflowLatchRatio, Unit: "ratio", Note: "overflow drops as a share of arrivals that indicates the latch"},
			{Name: "packetFloor", Value: 50, Unit: "packets", Note: "below this there is not enough audio to judge"},
		},
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
