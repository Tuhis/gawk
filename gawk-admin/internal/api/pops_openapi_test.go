package api

// popFixtures are the R74 schemas' fully populated Go values, merged into
// schemaFixtures so the drift test holds every PoP schema to its struct.
func popFixtures() map[string]any {
	ts := "2026-10-21T18:04:11Z"
	budget := popBudgetJSON{MaxMbps: 25, MaxStreams: 3}
	limits := popLimitsJSON{MaxBroadcasts: 40, MaxDownlinkStreams: 20}
	pairing := popPairingJSON{IssuedAt: ts, ExpiresAt: ts, Paired: true, PairedAt: ts}
	link := popLinkJSON{Connected: true, Since: ts, Version: "0.44.0", Proto: 1,
		PublicAddrs: []string{"203.0.113.7"}, LanAddr: "192.168.1.20", Pod: "gawk-server-0"}
	live := popLiveJSON{Pod: "gawk-server-0", Live: 7, Forwarded: 2, OutsideViewers: 11, UplinkMbps: 14.5, UplinkStreams: 2,
		Uplink: budget, Downlink: budget, DownlinkStreams: 1, GuestsConnected: 23, OffersSent: 19}
	bcast := popBroadcastJSON{ID: "ABC234", Label: "main pc", PublisherLanAddr: "192.168.1.41", LocalViewers: 5,
		OutsideViewers: 4, Forwarded: true, Wall: "on", Links: &attachmentLinksJSON{Watch: "https://gawk.example/#/view/ABC234"}}
	health := popHealthJSON{Key: "firewall", OK: false, Detail: "UDP 4433 is blocked"}
	bs, hs := []popBroadcastJSON{bcast}, []popHealthJSON{health}
	pop := popJSON{
		Name: "assembly-2026", DisplayName: "Assembly 2026", State: PopStateLinked, StateSince: ts, NotAfter: ts, CreatedAt: ts,
		Managed: true, Revoked: true, Pairing: pairing, Uplink: budget, Downlink: budget, Limits: limits,
		Guests: popGuestsJSON{AutoSwitch: true}, Wall: &popWallJSON{RoomCode: "assembly-2026"},
		NamedAddress: popNamedJSON{Enabled: true, Host: "p-7k2m9x.pop.gawk.ioio.fi", CertificateNotAfter: ts},
	}
	pop.Link, pop.Live, pop.Broadcasts, pop.Health = &link, &live, &bs, &hs
	on := true
	budgetReq := &popBudgetRequest{MaxMbps: 25, MaxStreams: 3}
	limitsReq := &popLimitsRequest{MaxBroadcasts: 40, MaxDownlinkStreams: 20}
	guestsReq := &popGuestsRequest{AutoSwitch: &on}
	name := "Assembly 2026"
	return map[string]any{
		"Pop":              pop,
		"PopPairing":       pairing,
		"PopBudget":        budget,
		"PopLimits":        limits,
		"PopLink":          link,
		"PopLive":          live,
		"PopBroadcast":     bcast,
		"PopHealthLine":    health,
		"PopsPage":         popsPageJSON{Pops: []popJSON{pop}},
		"PopWithPairing":   popWithPairingJSON{Pop: pop, PairingString: "gawk-pair:api.gawk.ioio.fi:4433/K7M2Q-9XDA4", ExpiresAt: ts},
		"PopBudgetRequest": *budgetReq,
		"PopLimitsRequest": *limitsReq,
		"PopGuestsRequest": *guestsReq,
		"CreatePopRequest": createPopRequest{Name: "assembly-2026", DisplayName: name, NotAfter: ts, Uplink: budgetReq,
			Downlink: budgetReq, Limits: limitsReq, Guests: guestsReq,
			Wall: &popWallRequest{RoomCode: "assembly-2026", MaxParticipants: 200}, NamedAddress: true},
		"PopPatchRequest": popPatchRequest{DisplayName: &name, NotAfter: &ts, Uplink: budgetReq, Downlink: budgetReq,
			Limits: limitsReq, Guests: guestsReq, NamedAddress: &on},
	}
}
