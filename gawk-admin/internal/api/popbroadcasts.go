package api

// Party broadcasts in the Broadcasts view (R74, docs/76 item 29, §7.2).
// The fleet scan lists what home carries; each linked PoP's report lists
// what is live at the party. The merge badges a broadcast home carries with
// its PoP, and gives a party-only one (home does not carry it: nobody
// outside is watching, or the PoP is past limits.maxBroadcasts) a row of
// its own, so it can be killed before anyone outside watches. A kill is an
// ID ban like any other; the link-home pod forwards it to the PoP.

import (
	"sort"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/relayscan"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
)

// broadcastPopJSON is a party broadcast's PoP badge. PublisherLanAddr is
// the broadcaster's address on the party LAN (admin-only, like
// publisherRemoteIp; docs/76 item 30).
type broadcastPopJSON struct {
	Name             string `json:"name"`
	Forwarded        bool   `json:"forwarded"`
	Label            string `json:"label,omitempty"`
	PublisherLanAddr string `json:"publisherLanAddr,omitempty"`
	LocalViewers     int    `json:"localViewers"`
	OutsideViewers   int    `json:"outsideViewers"`
}

// mergePopBroadcasts badges rows with their PoP and appends a row per
// party-only broadcast, returning the rows sorted by ID. active is nil when
// ban state is unknown.
func mergePopBroadcasts(rows []broadcastJSON, popAggs []relayscan.PopAggregate, cfg linkConfig,
	active []store.Ban, banStateKnown bool, now time.Time) []broadcastJSON {
	if len(popAggs) == 0 {
		return rows
	}
	idx := make(map[string]int, len(rows))
	for i, r := range rows {
		if norm, err := normalizeBroadcastID(r.ID); err == nil {
			idx[norm] = i
		}
	}
	for _, p := range popAggs {
		for _, b := range p.Broadcasts {
			norm, err := normalizeBroadcastID(b.ID)
			if err != nil {
				continue
			}
			badge := &broadcastPopJSON{
				Name: p.Name, Forwarded: b.Forwarded, Label: b.Label, PublisherLanAddr: b.PublisherLanAddr,
				LocalViewers: b.LocalViewers, OutsideViewers: b.OutsideViewers,
			}
			if i, ok := idx[norm]; ok {
				if rows[i].Pop == nil {
					rows[i].Pop = badge
				}
				continue
			}
			var state *banStateJSON
			if banStateKnown {
				state = &banStateJSON{}
				// No publisher address: the PoP's traffic is exempt from
				// IP bans (docs/76 item 30), so only the ID ban covers it.
				if ban := coveringBan(active, now, norm, ""); ban != nil {
					rendered := renderBan(*ban)
					state.Banned = true
					state.Ban = &rendered
				}
			}
			idx[norm] = len(rows)
			rows = append(rows, broadcastJSON{
				ID:              norm,
				PublisherActive: true,
				ViewersGlobal:   b.LocalViewers + b.OutsideViewers,
				Pods:            []podPlacementJSON{},
				Links:           linksFor(cfg.app, "", norm, ""),
				BanState:        state,
				Pop:             badge,
			})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}
