package moderation

import (
	"net/netip"
	"sync"
	"time"
)

// Set is the in-memory ban list a relay pod evaluates on the publish path
// (docs/42 §4.2). ID bans are an O(1) map; CIDR bans live in a
// path-compressed binary radix trie (a patricia trie) keyed on address bits,
// so a lookup costs O(prefix length) pointer hops rather than one comparison
// per ban.
//
// A linear scan was rejected explicitly (docs/42 §7): this is a foundational
// check on the publish hot path, the list grows, and prefixes are genuinely
// mixed — v4 and v6, /8 through /32 and /128, overlapping allowed. Any match
// means banned; the MOST SPECIFIC match is the record reported, because that
// is the one whose reason and expiry the operator meant to apply.
//
// The trie is hand-rolled rather than pulled from a library
// (github.com/gaissmai/bart was the alternative the design allowed): ~150
// lines against an unchanged dependency set for the relay image is the
// better trade here.
//
// The zero value is not usable; call NewSet. Every method is safe for
// concurrent use, and every method is nil-receiver-safe so a relay with
// -moderation-source=off can carry a nil Set and pay nothing.
type Set struct {
	mu    sync.RWMutex
	ids   map[string]Record
	root4 *trieNode
	root6 *trieNode
}

// NewSet returns an empty Set.
func NewSet() *Set {
	return &Set{ids: make(map[string]Record)}
}

// Replace swaps in a whole ban list: the informer's resync and the file
// source's reload. Records that fail normalization are dropped — a caller
// that cares logs them first (see internal/moderationsrc).
func (s *Set) Replace(all []Record) {
	if s == nil {
		return
	}
	ids := make(map[string]Record, len(all))
	var root4, root6 *trieNode
	for _, r := range all {
		norm, err := Normalize(r)
		if err != nil {
			continue
		}
		switch norm.Target.Type {
		case TargetBroadcastID:
			ids[norm.Target.Value] = norm
		case TargetIP:
			p, err := ParsePrefix(norm.Target.Value)
			if err != nil {
				continue
			}
			if p.Addr().Is4() {
				root4 = trieInsert(root4, p, norm)
			} else {
				root6 = trieInsert(root6, p, norm)
			}
		}
	}
	s.mu.Lock()
	s.ids, s.root4, s.root6 = ids, root4, root6
	s.mu.Unlock()
}

// Upsert adds or replaces one record (informer Add/Update). A record whose
// target does not normalize is rejected; the error is returned so the caller
// can log the offending CR rather than silently under-enforcing.
func (s *Set) Upsert(r Record) error {
	norm, err := Normalize(r)
	if err != nil {
		return err
	}
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch norm.Target.Type {
	case TargetBroadcastID:
		s.ids[norm.Target.Value] = norm
	case TargetIP:
		p, perr := ParsePrefix(norm.Target.Value)
		if perr != nil {
			return perr
		}
		if p.Addr().Is4() {
			s.root4 = trieInsert(s.root4, p, norm)
		} else {
			s.root6 = trieInsert(s.root6, p, norm)
		}
	}
	return nil
}

// Remove drops one target (informer Delete). Removing an absent target is a
// no-op, which is what makes replaying a delete safe.
func (s *Set) Remove(t Target) error {
	norm, err := Normalize(Record{Target: t})
	if err != nil {
		return err
	}
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch norm.Target.Type {
	case TargetBroadcastID:
		delete(s.ids, norm.Target.Value)
	case TargetIP:
		p, perr := ParsePrefix(norm.Target.Value)
		if perr != nil {
			return perr
		}
		if p.Addr().Is4() {
			s.root4 = trieDelete(s.root4, p)
		} else {
			s.root6 = trieDelete(s.root6, p)
		}
	}
	return nil
}

// BannedID reports whether an already-normalized broadcast ID is banned at
// now. Expiry is evaluated here, lazily: an expired record never matches,
// regardless of whether any janitor has deleted its CR (docs/42 §6).
func (s *Set) BannedID(normID string, now time.Time) (Record, bool) {
	if s == nil {
		return Record{}, false
	}
	s.mu.RLock()
	rec, ok := s.ids[normID]
	s.mu.RUnlock()
	if !ok || !rec.Active(now) {
		return Record{}, false
	}
	return rec, true
}

// BannedIP reports whether an address falls inside a banned CIDR at now,
// returning the most specific unexpired match. IPv4-mapped IPv6 addresses
// are collapsed to v4 first, so a 203.0.113.7/32 ban catches
// ::ffff:203.0.113.7.
func (s *Set) BannedIP(ip netip.Addr, now time.Time) (Record, bool) {
	if s == nil || !ip.IsValid() {
		return Record{}, false
	}
	ip = CanonicalAddr(ip)
	s.mu.RLock()
	root := s.root6
	if ip.Is4() {
		root = s.root4
	}
	rec, ok := trieLookup(root, ip, now)
	s.mu.RUnlock()
	return rec, ok
}

// ActiveCounts returns the number of unexpired records per target type, for
// the gawk_moderation_bans_active gauge. Both keys are always present so the
// series exist at zero.
func (s *Set) ActiveCounts(now time.Time) map[string]int {
	counts := map[string]int{
		string(TargetBroadcastID): 0,
		string(TargetIP):          0,
	}
	if s == nil {
		return counts
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, rec := range s.ids {
		if rec.Active(now) {
			counts[string(TargetBroadcastID)]++
		}
	}
	walk := func(n *trieNode) {
		trieWalk(n, func(rec Record) {
			if rec.Active(now) {
				counts[string(TargetIP)]++
			}
		})
	}
	walk(s.root4)
	walk(s.root6)
	return counts
}

// --- patricia trie ----------------------------------------------------
//
// Invariant: a child's prefix strictly extends its parent's (more bits, and
// contained in it), and a node's index in its parent is the parent-length'th
// bit of its address. Interior "glue" nodes (rec == nil) exist only to hold
// two diverging children. Because children are strictly deeper than parents,
// walking down an address visits candidate matches in increasing specificity
// — the last one that is unexpired is the longest-prefix match.

type trieNode struct {
	prefix netip.Prefix // always masked and canonical (see ParsePrefix)
	rec    *Record      // nil for a glue node
	child  [2]*trieNode
}

// bitAt returns bit i of an address, MSB first.
func bitAt(a netip.Addr, i int) int {
	b := a.As16()
	if a.Is4() {
		v4 := a.As4()
		b = [16]byte{}
		copy(b[:4], v4[:])
	}
	return int(b[i/8]>>(7-uint(i%8))) & 1
}

// commonPrefixLen returns how many leading bits two same-family addresses
// share, capped at their bit length.
func commonPrefixLen(a, b netip.Addr) int {
	n := a.BitLen()
	for i := 0; i < n; i++ {
		if bitAt(a, i) != bitAt(b, i) {
			return i
		}
	}
	return n
}

func trieInsert(n *trieNode, p netip.Prefix, rec Record) *trieNode {
	if n == nil {
		r := rec
		return &trieNode{prefix: p, rec: &r}
	}
	if n.prefix == p {
		r := rec
		n.rec = &r
		return n
	}
	if n.prefix.Bits() < p.Bits() && n.prefix.Contains(p.Addr()) {
		// p belongs below n.
		b := bitAt(p.Addr(), n.prefix.Bits())
		n.child[b] = trieInsert(n.child[b], p, rec)
		return n
	}
	if p.Bits() < n.prefix.Bits() && p.Contains(n.prefix.Addr()) {
		// p becomes n's parent.
		r := rec
		parent := &trieNode{prefix: p, rec: &r}
		parent.child[bitAt(n.prefix.Addr(), p.Bits())] = n
		return parent
	}
	// Disjoint: splice in a glue node at the longest shared prefix. Neither
	// contains the other, so the shared length is strictly shorter than both.
	cl := commonPrefixLen(n.prefix.Addr(), p.Addr())
	if cl > n.prefix.Bits() {
		cl = n.prefix.Bits()
	}
	if cl > p.Bits() {
		cl = p.Bits()
	}
	glue := &trieNode{prefix: netip.PrefixFrom(p.Addr(), cl).Masked()}
	r := rec
	glue.child[bitAt(n.prefix.Addr(), cl)] = n
	glue.child[bitAt(p.Addr(), cl)] = &trieNode{prefix: p, rec: &r}
	return glue
}

func trieDelete(n *trieNode, p netip.Prefix) *trieNode {
	if n == nil {
		return nil
	}
	switch {
	case n.prefix == p:
		n.rec = nil
	case n.prefix.Bits() < p.Bits() && n.prefix.Contains(p.Addr()):
		b := bitAt(p.Addr(), n.prefix.Bits())
		n.child[b] = trieDelete(n.child[b], p)
	default:
		return n // not present
	}
	// Prune: a node with no record of its own is only worth keeping while it
	// still forks. Collapsing it is safe — its single child still strictly
	// extends the grandparent's prefix.
	if n.rec == nil {
		switch {
		case n.child[0] == nil && n.child[1] == nil:
			return nil
		case n.child[0] == nil:
			return n.child[1]
		case n.child[1] == nil:
			return n.child[0]
		}
	}
	return n
}

func trieLookup(n *trieNode, ip netip.Addr, now time.Time) (Record, bool) {
	var best Record
	found := false
	for n != nil {
		if !n.prefix.Contains(ip) {
			break
		}
		// Deeper nodes overwrite shallower ones, so the last unexpired hit is
		// the longest-prefix match. An expired record does not shadow a
		// broader ban that is still in force.
		if n.rec != nil && n.rec.Active(now) {
			best, found = *n.rec, true
		}
		if n.prefix.Bits() >= ip.BitLen() {
			break
		}
		n = n.child[bitAt(ip, n.prefix.Bits())]
	}
	return best, found
}

func trieWalk(n *trieNode, fn func(Record)) {
	if n == nil {
		return
	}
	if n.rec != nil {
		fn(*n.rec)
	}
	trieWalk(n.child[0], fn)
	trieWalk(n.child[1], fn)
}
