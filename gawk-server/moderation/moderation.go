// Package moderation is the shared vocabulary of R39 enforcement (docs/42):
// the Ban CRD Go types, target normalization, deterministic CR naming, and
// the in-memory Set the relay evaluates on the publish hot path.
//
// It is deliberately PUBLIC (not internal/), exactly like the wire package
// is (R14 Decision 1): gawk-admin — the fourth top-level module — imports it
// through a `replace` directive so the relay and the admin plane agree
// byte-for-byte on normalization, target matching and expiry semantics
// (docs/42 D13). Two implementations of "is this banned?" would eventually
// disagree, and the disagreement would be invisible until it mattered.
//
// Keep the dependency surface here small for the same reason wire's is: it
// is a contract package, not an implementation home. It imports apimachinery
// (unavoidable — it carries CRD types) and internal/broadcastid (see
// Normalize), and nothing else beyond the standard library.
package moderation

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
)

// TargetType enumerates the two ban handles R39 has (docs/42 D4). There is
// no third: with no accounts, a broadcast ID and a publisher IP are the only
// identities the platform can observe.
type TargetType string

const (
	// TargetBroadcastID bans one broadcast ID. Because the resume token is
	// HMAC(key, broadcastID) — stateless — banning the ID is banning the
	// token (internal/transport/resume.go).
	TargetBroadcastID TargetType = "broadcastId"
	// TargetIP bans a publisher source CIDR: the only handle that spans a
	// re-mint loop.
	TargetIP TargetType = "ip"
)

// Target is one ban handle. Exactly one per Ban CR (docs/42 §4.2): clean
// expiry means an expired ban is a deleted object, never a half-patched one.
type Target struct {
	Type  TargetType `json:"type"`
	Value string     `json:"value"`
}

// Record is one evaluated ban. ExpiresAt nil means permanent.
//
// The JSON tags are the on-disk format of the file ban source
// (-moderation-source=file:<path>, docs/42 §4.14): a plain JSON array of
// these. They mirror the CRD's spec field names so the two representations
// read the same.
type Record struct {
	Target    Target     `json:"target"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	Reason    string     `json:"reason,omitempty"`
	CreatedBy string     `json:"createdBy,omitempty"`
}

// ErrInvalidTarget is returned for any target that cannot be normalized.
// Check with errors.Is.
var ErrInvalidTarget = errors.New("moderation: invalid ban target")

// Active reports whether the record is in force at now. Expiry is evaluated
// lazily, here, against the caller's clock — never by asking whether some
// janitor has deleted the CR yet (docs/42 §6: "relays evaluate expiresAt
// themselves at check time, so enforcement ends on schedule even if the
// janitor is down"). An expiry exactly at now has passed.
func (r Record) Active(now time.Time) bool {
	return r.ExpiresAt == nil || now.Before(*r.ExpiresAt)
}

// Normalize canonicalizes a record's target, returning a copy. It is the
// single definition of "the same target" for the relay and gawk-admin alike:
//
//   - broadcastId: uppercased and validated by internal/broadcastid.Normalize.
//     That package is internal to gawk-server, but the internal rule is
//     enforced at the import site, and this package lives inside
//     gawk-server — so importing it here is legal and it stays legal for
//     gawk-admin, which reaches it only transitively. Chosen over exporting a
//     second copy of the 31-symbol alphabet: CODE-REVIEW.md's "shared
//     constants have exactly one definition per language" is the whole point.
//   - ip: parsed with net/netip. A bare address becomes a /32 or /128; a
//     non-canonical prefix (203.0.113.7/24) is masked to its network
//     (203.0.113.0/24); a v4-mapped v6 form collapses to plain v4 so that
//     ::ffff:203.0.113.7 and 203.0.113.7 are one target, not two.
//
// Everything else — an unknown type, an empty value, a malformed CIDR — is
// ErrInvalidTarget. Rejecting is the safe direction: an unparseable ban that
// silently became a wildcard would take the fleet down.
func Normalize(r Record) (Record, error) {
	switch r.Target.Type {
	case TargetBroadcastID:
		id, err := broadcastid.Normalize(strings.TrimSpace(r.Target.Value))
		if err != nil {
			return Record{}, fmt.Errorf("%w: broadcast ID %q: %w", ErrInvalidTarget, r.Target.Value, err)
		}
		r.Target.Value = id
	case TargetIP:
		p, err := ParsePrefix(r.Target.Value)
		if err != nil {
			return Record{}, err
		}
		r.Target.Value = p.String()
	default:
		return Record{}, fmt.Errorf("%w: unknown target type %q", ErrInvalidTarget, r.Target.Type)
	}
	return r, nil
}

// ParsePrefix canonicalizes an IP ban value into a netip.Prefix: a bare
// address widens to a host prefix (/32 or /128), a prefix is masked to its
// network, and a v4-mapped v6 prefix collapses to plain v4. Exported because
// gawk-admin resolves a publisher IP into a ban target with the same rules.
func ParsePrefix(value string) (netip.Prefix, error) {
	s := strings.TrimSpace(value)
	if s == "" {
		return netip.Prefix{}, fmt.Errorf("%w: empty IP target", ErrInvalidTarget)
	}
	if !strings.Contains(s, "/") {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%w: address %q: %w", ErrInvalidTarget, value, err)
		}
		addr = CanonicalAddr(addr)
		return netip.PrefixFrom(addr, addr.BitLen()), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%w: CIDR %q: %w", ErrInvalidTarget, value, err)
	}
	// Zones are meaningless in a ban list (they name a local interface, not a
	// peer), and netip refuses to mask a zoned prefix sanely — drop it.
	p = netip.PrefixFrom(p.Addr().WithZone(""), p.Bits()).Masked()
	if p.Addr().Is4In6() {
		// After masking, an Is4In6 address implies Bits() >= 96 (a shorter
		// mask would have zeroed the ::ffff: marker), so this subtraction is
		// always in range: ::ffff:0.0.0.0/96 becomes 0.0.0.0/0.
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
	}
	return p, nil
}

// CanonicalAddr is how an observed peer address becomes a lookup key:
// zone stripped, v4-mapped-v6 collapsed to v4. Callers on the publish path
// use it so ::ffff:203.0.113.7 is matched by a 203.0.113.7/32 ban.
func CanonicalAddr(a netip.Addr) netip.Addr {
	return a.WithZone("").Unmap()
}

// CRName returns the deterministic Ban CR name for a target, so a re-ban
// updates the existing object instead of creating a duplicate (docs/42 §4.2).
// The result is always a valid DNS-1123 subdomain:
//
//   - ban-id-<lowercased broadcast ID>. The ID alphabet is alphanumeric and
//     DNS-safe by construction — the Lease naming scheme gawk-bc-<id>
//     (internal/cluster/cluster.go) already relies on exactly this.
//   - ban-ip-<first 12 hex of SHA-256 of the canonical CIDR>. CIDRs contain
//     ':', '/' and '.', so they are hashed rather than munged; 48 bits is
//     ample against accidental collision in an operator-curated list, and a
//     collision would only ever coalesce two bans, never un-ban anything.
//
// The target is normalized first, so CRName("203.0.113.7") and
// CRName("203.0.113.7/32") name the same object.
func CRName(t Target) (string, error) {
	norm, err := Normalize(Record{Target: t})
	if err != nil {
		return "", err
	}
	switch norm.Target.Type {
	case TargetBroadcastID:
		return "ban-id-" + strings.ToLower(norm.Target.Value), nil
	case TargetIP:
		sum := sha256.Sum256([]byte(norm.Target.Value))
		return "ban-ip-" + hex.EncodeToString(sum[:])[:12], nil
	default:
		// Unreachable: Normalize rejects every other type.
		return "", fmt.Errorf("%w: unknown target type %q", ErrInvalidTarget, norm.Target.Type)
	}
}
