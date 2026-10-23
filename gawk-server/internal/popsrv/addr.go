package popsrv

import (
	"net/netip"
	"slices"
)

// RecordAddr records an address a PoP's dial came from (/pop/addr, which
// reveals the family the link did not, docs/76 item 13). Any pod may serve
// /pop/addr, so it writes status.link.publicAddrs; the link-home pod also
// keeps it in memory for Linked.
func (m *Manager) RecordAddr(pop string, remote netip.Addr) {
	if !remote.IsValid() {
		return
	}
	key := publicAddrKey(remote)
	if l := m.local(pop); l != nil {
		l.mu.Lock()
		if !slices.Contains(l.publicAddrs, key) {
			l.publicAddrs = append(l.publicAddrs, key)
		}
		l.mu.Unlock()
	}
	ctx, cancel := background()
	defer cancel()
	if err := m.opts.Status.AddPublicAddr(ctx, pop, key); err != nil {
		m.log.Warn("pop public address write failed", "pop", pop, "err", err)
	}
}

// publicAddrKey is how a public address is recorded and matched: a v4
// address exactly, a v6 address as its /64 (privacy addresses rotate
// within it, docs/76 §6.12).
func publicAddrKey(a netip.Addr) string {
	a = a.Unmap()
	if a.Is4() {
		return a.String()
	}
	p, err := a.Prefix(64)
	if err != nil {
		return a.String()
	}
	return p.String()
}

// parsePrefixes parses recorded public addresses into prefixes (/32 for a
// v4 address); unparseable entries are skipped.
func parsePrefixes(keys []string) []netip.Prefix {
	var out []netip.Prefix
	for _, k := range keys {
		if p, err := netip.ParsePrefix(k); err == nil {
			out = append(out, p.Masked())
			continue
		}
		if a, err := netip.ParseAddr(k); err == nil {
			out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
		}
	}
	return out
}
