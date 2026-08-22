package moderation

import (
	"math/rand"
	"net/netip"
	"testing"
	"time"
)

func rec(typ TargetType, value, reason string) Record {
	return Record{Target: Target{Type: typ, Value: value}, Reason: reason}
}

func mustUpsert(t *testing.T, s *Set, r Record) {
	t.Helper()
	if err := s.Upsert(r); err != nil {
		t.Fatalf("Upsert(%+v): %v", r.Target, err)
	}
}

func TestSetIDExactAfterNormalize(t *testing.T) {
	now := time.Now()
	s := NewSet()
	// The CR may carry any case; the check is against the normalized ID.
	mustUpsert(t, s, rec(TargetBroadcastID, "abc23z", "fraud"))

	got, ok := s.BannedID("ABC23Z", now)
	if !ok {
		t.Fatal("BannedID(ABC23Z) = false, want true")
	}
	if got.Reason != "fraud" {
		t.Errorf("reason = %q, want %q", got.Reason, "fraud")
	}
	if _, ok := s.BannedID("ZZZ23Z", now); ok {
		t.Error("an unrelated ID matched")
	}
	// Not a substring/prefix match, and not a lowercase match: BannedID is
	// documented as taking an ALREADY normalized ID.
	if _, ok := s.BannedID("abc23z", now); ok {
		t.Error("un-normalized ID matched; callers must normalize first")
	}

	if err := s.Remove(Target{TargetBroadcastID, "ABC23Z"}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := s.BannedID("ABC23Z", now); ok {
		t.Error("ID still banned after Remove")
	}
}

func TestSetBareIPCanonicalization(t *testing.T) {
	now := time.Now()
	s := NewSet()
	mustUpsert(t, s, rec(TargetIP, "203.0.113.7", "v4 host"))
	mustUpsert(t, s, rec(TargetIP, "2001:db8::1", "v6 host"))

	if _, ok := s.BannedIP(netip.MustParseAddr("203.0.113.7"), now); !ok {
		t.Error("bare v4 ban did not match its own address")
	}
	if _, ok := s.BannedIP(netip.MustParseAddr("203.0.113.8"), now); ok {
		t.Error("bare v4 ban matched a neighbour — it must be a /32")
	}
	if _, ok := s.BannedIP(netip.MustParseAddr("2001:db8::1"), now); !ok {
		t.Error("bare v6 ban did not match its own address")
	}
	if _, ok := s.BannedIP(netip.MustParseAddr("2001:db8::2"), now); ok {
		t.Error("bare v6 ban matched a neighbour — it must be a /128")
	}
	// A v4-mapped v6 peer address is the same host as its v4 form.
	if _, ok := s.BannedIP(netip.MustParseAddr("::ffff:203.0.113.7"), now); !ok {
		t.Error("v4-mapped form of a banned v4 address did not match")
	}
	// Families do not cross: a v4 ban must not catch a v6 address.
	if _, ok := s.BannedIP(netip.MustParseAddr("2001:db8::cb00:7107"), now); ok {
		t.Error("v4 ban matched an unrelated v6 address")
	}
}

// Overlapping prefixes are allowed, mixed v4/v6, and the MOST SPECIFIC match
// is the record reported (docs/42 §4.2).
func TestSetLongestPrefixMatch(t *testing.T) {
	now := time.Now()
	s := NewSet()
	mustUpsert(t, s, rec(TargetIP, "203.0.0.0/8", "slash8"))
	mustUpsert(t, s, rec(TargetIP, "203.0.113.0/24", "slash24"))
	mustUpsert(t, s, rec(TargetIP, "203.0.113.7/32", "slash32"))
	mustUpsert(t, s, rec(TargetIP, "198.51.100.0/23", "other23"))
	mustUpsert(t, s, rec(TargetIP, "2001:db8::/32", "v6slash32"))
	mustUpsert(t, s, rec(TargetIP, "2001:db8:1::/48", "v6slash48"))
	mustUpsert(t, s, rec(TargetIP, "::/0", "v6default"))
	// The whole-internet kill switch is a legal target (ParsePrefix accepts it
	// and even manufactures it from ::ffff:0.0.0.0/96), and /0 sits at the
	// trie's root — the classic restructuring-defect site.
	mustUpsert(t, s, rec(TargetIP, "0.0.0.0/0", "v4default"))

	tests := []struct {
		addr string
		want string // "" = not banned
	}{
		{"203.0.113.7", "slash32"},
		{"203.0.113.8", "slash24"},
		{"203.0.200.1", "slash8"},
		{"204.0.113.7", "v4default"}, // nothing narrower — the /0 catches it
		{"198.51.100.5", "other23"},
		{"198.51.101.5", "other23"}, // /23 spans two /24s
		{"198.51.102.5", "v4default"},
		{"2001:db8:1::5", "v6slash48"},
		{"2001:db8:2::5", "v6slash32"},
		// The /0s stay family-scoped: a v6 address falls through to ::/0, never
		// to 0.0.0.0/0, and vice versa.
		{"2001:dead::1", "v6default"},
	}
	for _, tt := range tests {
		got, ok := s.BannedIP(netip.MustParseAddr(tt.addr), now)
		if (tt.want != "") != ok {
			t.Errorf("BannedIP(%s) ok = %v, want %v", tt.addr, ok, tt.want != "")
			continue
		}
		if ok && got.Reason != tt.want {
			t.Errorf("BannedIP(%s) matched %q, want the more specific %q", tt.addr, got.Reason, tt.want)
		}
	}

	// Deleting the most specific ban must fall back to the next one, not
	// un-ban the address.
	if err := s.Remove(Target{TargetIP, "203.0.113.7/32"}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got, ok := s.BannedIP(netip.MustParseAddr("203.0.113.7"), now)
	if !ok || got.Reason != "slash24" {
		t.Errorf("after removing the /32: got %q ok=%v, want slash24", got.Reason, ok)
	}
}

// Lazy expiry: an expired record never matches, with no janitor involved —
// and it does not shadow a broader ban that is still in force.
func TestSetLazyExpiry(t *testing.T) {
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	exp := base.Add(10 * time.Minute)

	s := NewSet()
	idRec := rec(TargetBroadcastID, "ABC23Z", "cooldown")
	idRec.ExpiresAt = &exp
	mustUpsert(t, s, idRec)

	if _, ok := s.BannedID("ABC23Z", base); !ok {
		t.Error("ID ban not in force before expiry")
	}
	if _, ok := s.BannedID("ABC23Z", exp.Add(-time.Nanosecond)); !ok {
		t.Error("ID ban lapsed a nanosecond early")
	}
	if _, ok := s.BannedID("ABC23Z", exp); ok {
		t.Error("ID ban still matched exactly at its expiry")
	}
	if _, ok := s.BannedID("ABC23Z", exp.Add(time.Hour)); ok {
		t.Error("expired ID ban still matched — expiry must not need the janitor")
	}

	// Specific ban expires; broader permanent ban must still catch the host.
	narrow := rec(TargetIP, "203.0.113.7/32", "narrow")
	narrow.ExpiresAt = &exp
	mustUpsert(t, s, narrow)
	mustUpsert(t, s, rec(TargetIP, "203.0.113.0/24", "broad"))

	got, ok := s.BannedIP(netip.MustParseAddr("203.0.113.7"), base)
	if !ok || got.Reason != "narrow" {
		t.Errorf("before expiry: %q ok=%v, want narrow", got.Reason, ok)
	}
	got, ok = s.BannedIP(netip.MustParseAddr("203.0.113.7"), exp.Add(time.Hour))
	if !ok || got.Reason != "broad" {
		t.Errorf("after expiry: %q ok=%v, want the still-active broad ban", got.Reason, ok)
	}

	// An expired ban alone means not banned.
	only := NewSet()
	mustUpsert(t, only, narrow)
	if _, ok := only.BannedIP(netip.MustParseAddr("203.0.113.7"), exp.Add(time.Second)); ok {
		t.Error("expired CIDR ban still matched")
	}
	counts := only.ActiveCounts(exp.Add(time.Second))
	if counts[string(TargetIP)] != 0 {
		t.Errorf("ActiveCounts counted an expired ban: %v", counts)
	}
}

func TestSetReplaceSwapsWholeList(t *testing.T) {
	now := time.Now()
	s := NewSet()
	mustUpsert(t, s, rec(TargetBroadcastID, "ABC23Z", "old"))
	mustUpsert(t, s, rec(TargetIP, "203.0.113.0/24", "old"))

	s.Replace([]Record{
		rec(TargetBroadcastID, "ZZZ23Z", "new"),
		rec(TargetIP, "198.51.100.0/24", "new"),
		rec(TargetIP, "garbage", "dropped"), // unparseable entries are dropped
	})

	if _, ok := s.BannedID("ABC23Z", now); ok {
		t.Error("Replace left a stale ID ban behind")
	}
	if _, ok := s.BannedIP(netip.MustParseAddr("203.0.113.7"), now); ok {
		t.Error("Replace left a stale CIDR ban behind")
	}
	if _, ok := s.BannedID("ZZZ23Z", now); !ok {
		t.Error("Replace did not install the new ID ban")
	}
	if _, ok := s.BannedIP(netip.MustParseAddr("198.51.100.7"), now); !ok {
		t.Error("Replace did not install the new CIDR ban")
	}
	if got := s.ActiveCounts(now); got[string(TargetBroadcastID)] != 1 || got[string(TargetIP)] != 1 {
		t.Errorf("ActiveCounts = %v, want one of each", got)
	}
}

// A nil Set is the -moderation-source=off shape: every check is a cheap miss
// and no mutation panics.
func TestNilSetIsAnEmptySet(t *testing.T) {
	var s *Set
	now := time.Now()
	if _, ok := s.BannedID("ABC23Z", now); ok {
		t.Error("nil Set reported an ID ban")
	}
	if _, ok := s.BannedIP(netip.MustParseAddr("203.0.113.7"), now); ok {
		t.Error("nil Set reported an IP ban")
	}
	s.Replace([]Record{rec(TargetIP, "203.0.113.0/24", "x")})
	if err := s.Upsert(rec(TargetIP, "203.0.113.0/24", "x")); err != nil {
		t.Errorf("nil Upsert: %v", err)
	}
	if err := s.Remove(Target{TargetIP, "203.0.113.0/24"}); err != nil {
		t.Errorf("nil Remove: %v", err)
	}
	if got := s.ActiveCounts(now); got[string(TargetIP)] != 0 {
		t.Errorf("nil ActiveCounts = %v", got)
	}
}

func TestSetRejectsMalformedTargets(t *testing.T) {
	s := NewSet()
	if err := s.Upsert(rec(TargetIP, "203.0.113.0/99", "")); err == nil {
		t.Error("Upsert accepted a malformed CIDR")
	}
	if err := s.Upsert(rec(TargetBroadcastID, "nope", "")); err == nil {
		t.Error("Upsert accepted a malformed broadcast ID")
	}
	if err := s.Remove(Target{TargetIP, "nope"}); err == nil {
		t.Error("Remove accepted a malformed CIDR")
	}
}

// The acceptance criterion (docs/42 §9 AP2): the longest-prefix-match trie
// must agree with a naive linear reference matcher over randomized, mixed
// v4/v6, non-/32, overlapping prefixes — including expiry and deletions.
func TestSetLPMMatchesLinearReference(t *testing.T) {
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	for seed := int64(0); seed < 40; seed++ {
		rng := rand.New(rand.NewSource(seed))
		s := NewSet()
		ref := map[netip.Prefix]Record{} // last write wins, same as the trie

		// Bias towards a shared /8 and a shared /32-v6 so prefixes actually
		// overlap and nest instead of being uniformly disjoint. Bit counts run
		// all the way down to /0: the root/near-root path of a path-compressed
		// trie is exactly where insert/delete/glue bugs live, and 0.0.0.0/0 is
		// a legal production target (the whole-internet kill switch).
		gen := func() netip.Prefix {
			if rng.Intn(2) == 0 {
				b := [4]byte{203, byte(rng.Intn(4)), byte(rng.Intn(8)), byte(rng.Intn(256))}
				return netip.PrefixFrom(netip.AddrFrom4(b), rng.Intn(33)).Masked()
			}
			var b [16]byte
			b[0], b[1] = 0x20, 0x01
			b[2], b[3] = 0x0d, 0xb8
			for i := 4; i < 16; i++ {
				if rng.Intn(3) == 0 {
					b[i] = byte(rng.Intn(256))
				}
			}
			return netip.PrefixFrom(netip.AddrFrom16(b), rng.Intn(129)).Masked()
		}

		for op := 0; op < 120; op++ {
			p := gen()
			switch {
			case rng.Intn(4) == 0 && len(ref) > 0:
				// Remove an existing entry more often than a random one.
				for q := range ref {
					p = q
					break
				}
				if err := s.Remove(Target{TargetIP, p.String()}); err != nil {
					t.Fatalf("Remove(%s): %v", p, err)
				}
				delete(ref, p)
			default:
				r := Record{Target: Target{TargetIP, p.String()}, Reason: p.String()}
				switch rng.Intn(3) {
				case 0: // already expired
					e := base.Add(-time.Duration(rng.Intn(60)+1) * time.Minute)
					r.ExpiresAt = &e
				case 1: // expires later
					e := base.Add(time.Duration(rng.Intn(60)+1) * time.Minute)
					r.ExpiresAt = &e
				}
				mustUpsert(t, s, r)
				norm, err := Normalize(r)
				if err != nil {
					t.Fatalf("Normalize(%s): %v", p, err)
				}
				ref[p] = norm
			}
		}

		// Every third round, exercise Replace with the accumulated list too.
		if seed%3 == 0 {
			all := make([]Record, 0, len(ref))
			for _, r := range ref {
				all = append(all, r)
			}
			s.Replace(all)
		}

		for probe := 0; probe < 300; probe++ {
			var addr netip.Addr
			if rng.Intn(2) == 0 {
				addr = netip.AddrFrom4([4]byte{203, byte(rng.Intn(4)), byte(rng.Intn(8)), byte(rng.Intn(256))})
				if rng.Intn(4) == 0 {
					// Probe the v4-mapped form too, so CanonicalAddr's collapse
					// is property-exercised rather than only unit-pinned.
					addr = netip.AddrFrom16(addr.As16())
				}
			} else {
				var b [16]byte
				b[0], b[1], b[2], b[3] = 0x20, 0x01, 0x0d, 0xb8
				for i := 4; i < 16; i++ {
					if rng.Intn(3) == 0 {
						b[i] = byte(rng.Intn(256))
					}
				}
				addr = netip.AddrFrom16(b)
			}
			now := base
			if rng.Intn(4) == 0 {
				now = base.Add(time.Duration(rng.Intn(120)) * time.Minute)
			}

			wantRec, wantOK := linearBanned(ref, addr, now)
			gotRec, gotOK := s.BannedIP(addr, now)
			if gotOK != wantOK {
				t.Fatalf("seed %d: BannedIP(%s, %v) ok = %v, reference %v", seed, addr, now, gotOK, wantOK)
			}
			if gotOK && gotRec.Target.Value != wantRec.Target.Value {
				t.Fatalf("seed %d: BannedIP(%s, %v) matched %s, reference (longest prefix) %s",
					seed, addr, now, gotRec.Target.Value, wantRec.Target.Value)
			}
		}
	}
}

// linearBanned is the naive reference: scan every prefix, keep the longest
// that contains the address and is still in force.
func linearBanned(ref map[netip.Prefix]Record, addr netip.Addr, now time.Time) (Record, bool) {
	addr = CanonicalAddr(addr)
	var best Record
	bestBits := -1
	for p, r := range ref {
		if !p.Contains(addr) || !r.Active(now) {
			continue
		}
		if p.Bits() > bestBits {
			best, bestBits = r, p.Bits()
		}
	}
	return best, bestBits >= 0
}

func TestSetConcurrentReadsAndWrites(t *testing.T) {
	s := NewSet()
	now := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			mustUpsert(t, s, rec(TargetIP, netip.PrefixFrom(
				netip.AddrFrom4([4]byte{203, 0, 113, byte(i % 256)}), 32).String(), "x"))
			_ = s.Remove(Target{TargetIP, "203.0.113.1/32"})
		}
	}()
	for i := 0; i < 500; i++ {
		s.BannedIP(netip.MustParseAddr("203.0.113.5"), now)
		s.BannedID("ABC23Z", now)
		s.ActiveCounts(now)
	}
	<-done
}
