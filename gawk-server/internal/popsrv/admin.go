package popsrv

import (
	"slices"

	"github.com/Tuhis/gawk/gawk-server/adminapi"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// AdminPops is GET /internal/admin/pops on this pod: one row per PoP whose
// link THIS pod holds (gawk-admin merges the fleet scrape). It carries the
// PoP's public and LAN addresses and raw broadcast IDs, which the admin
// routes may carry and events never do (docs/76 §8). PP8, PP9 and PP13 add
// their counts to the Stats the PoP reports and to OffersSent.
func (m *Manager) AdminPops() []adminapi.PopState {
	m.mu.Lock()
	locals := make([]*link, 0, len(m.links))
	for _, l := range m.links {
		locals = append(locals, l)
	}
	m.mu.Unlock()
	out := make([]adminapi.PopState, 0, len(locals))
	for _, l := range locals {
		select {
		case <-l.ended:
			continue
		default:
		}
		p, _ := m.pop(l.name)
		row := l.adminState(p)
		if of := m.offers.Load(); of != nil {
			row.OffersSent = of.Sent(l.name)
		}
		out = append(out, row)
	}
	slices.SortFunc(out, func(a, b adminapi.PopState) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	return out
}

func (l *link) adminState(p *pops.Pop) adminapi.PopState {
	l.mu.Lock()
	defer l.mu.Unlock()
	since := l.since
	s := adminapi.PopState{
		Name:            l.name,
		UID:             l.uid,
		Linked:          true,
		Since:           &since,
		Version:         l.hello.Version,
		Proto:           l.hello.Proto,
		PublicAddrs:     slices.Clone(l.publicAddrs),
		LanAddr:         l.hello.LanAddr,
		NamedHost:       l.hello.NamedHost,
		UplinkMbps:      l.stats.UplinkMbps,
		UplinkStreams:   l.stats.UplinkStreams,
		DownlinkStreams: l.stats.DownlinkStreams,
		GuestsConnected: l.stats.GuestsConnected,
		Broadcasts:      slices.Clone(l.stats.Broadcasts),
		Health:          slices.Clone(l.health),
	}
	var up, down pops.Budget
	if p != nil {
		up, down = p.EffectiveUplink(), p.Spec.Downlink
		if s.NamedHost == "" && p.Status.TLS != nil {
			s.NamedHost = p.Status.TLS.Host
		}
	}
	s.Uplink = lowered(up, l.stats.Uplink)
	s.Downlink = lowered(down, l.stats.Downlink)
	for _, b := range l.stats.Broadcasts {
		s.Live++
		if b.Forwarded {
			s.Forwarded++
		}
		s.OutsideViewers += b.OutsideViewers
	}
	return s
}

// lowered is the effective ceiling: the CR's, lowered (never raised) by
// the host's own budget. Zero is unlimited on either side.
func lowered(ceiling, host pops.Budget) adminapi.BudgetUse {
	return adminapi.BudgetUse{MaxMbps: minLimit(ceiling.MaxMbps, host.MaxMbps), MaxStreams: minLimit(ceiling.MaxStreams, host.MaxStreams)}
}

func minLimit(a, b int) int {
	switch {
	case a <= 0:
		return max(b, 0)
	case b <= 0:
		return a
	}
	return min(a, b)
}
