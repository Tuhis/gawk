package moderation

import (
	"errors"
	"math/rand"
	"net/netip"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestNormalizeBroadcastID(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"ABC23Z", "ABC23Z", false},
		{"abc23z", "ABC23Z", false}, // same rules as broadcastid.Normalize
		{" abc23z ", "ABC23Z", false},
		{"ABC23", "", true},   // too short
		{"ABC23ZZ", "", true}, // too long
		{"ABC230", "", true},  // 0 is not in the alphabet
		{"ABC23O", "", true},  // O is not in the alphabet
		{"", "", true},
	}
	for _, tt := range tests {
		got, err := Normalize(Record{Target: Target{Type: TargetBroadcastID, Value: tt.in}})
		if (err != nil) != tt.wantErr {
			t.Errorf("Normalize(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if err != nil {
			if !errors.Is(err, ErrInvalidTarget) {
				t.Errorf("Normalize(%q) err = %v, want ErrInvalidTarget", tt.in, err)
			}
			continue
		}
		if got.Target.Value != tt.want {
			t.Errorf("Normalize(%q) = %q, want %q", tt.in, got.Target.Value, tt.want)
		}
	}
}

func TestNormalizeIPCanonicalization(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"203.0.113.7", "203.0.113.7/32", false},            // bare v4 widens to /32
		{"2001:db8::1", "2001:db8::1/128", false},           // bare v6 widens to /128
		{"203.0.113.7/24", "203.0.113.0/24", false},         // non-canonical masked
		{"2001:db8::1/64", "2001:db8::/64", false},          // non-canonical masked
		{"::ffff:203.0.113.7", "203.0.113.7/32", false},     // v4-mapped collapses
		{"::ffff:203.0.113.7/120", "203.0.113.0/24", false}, // ...prefix too
		{"::ffff:0.0.0.0/96", "0.0.0.0/0", false},
		{"0.0.0.0/0", "0.0.0.0/0", false},
		{"::/0", "::/0", false},
		{"", "", true},
		{"not-an-ip", "", true},
		{"203.0.113.0/33", "", true},
		{"203.0.113.0/-1", "", true},
	}
	for _, tt := range tests {
		got, err := Normalize(Record{Target: Target{Type: TargetIP, Value: tt.in}})
		if (err != nil) != tt.wantErr {
			t.Errorf("Normalize(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if err != nil {
			if !errors.Is(err, ErrInvalidTarget) {
				t.Errorf("Normalize(%q) err = %v, want ErrInvalidTarget", tt.in, err)
			}
			continue
		}
		if got.Target.Value != tt.want {
			t.Errorf("Normalize(%q) = %q, want %q", tt.in, got.Target.Value, tt.want)
		}
	}
}

func TestNormalizeRejectsUnknownTargetType(t *testing.T) {
	if _, err := Normalize(Record{Target: Target{Type: "publisher", Value: "x"}}); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("err = %v, want ErrInvalidTarget", err)
	}
	if _, err := Normalize(Record{}); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("zero target err = %v, want ErrInvalidTarget", err)
	}
}

// A ban must not lose its metadata to normalization.
func TestNormalizePreservesRecordFields(t *testing.T) {
	exp := time.Date(2026, 8, 20, 18, 0, 0, 0, time.UTC)
	in := Record{
		Target:    Target{Type: TargetIP, Value: "203.0.113.7"},
		ExpiresAt: &exp,
		Reason:    "spam",
		CreatedBy: "juho@example.com",
	}
	got, err := Normalize(in)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got.Reason != in.Reason || got.CreatedBy != in.CreatedBy || got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("Normalize dropped fields: %+v", got)
	}
}

func TestCRNameDeterministicAndDNS1123(t *testing.T) {
	cases := []struct {
		target Target
		want   string
	}{
		{Target{TargetBroadcastID, "ABC23Z"}, "ban-id-abc23z"},
		{Target{TargetBroadcastID, "abc23z"}, "ban-id-abc23z"}, // normalized first
		{Target{TargetIP, "203.0.113.7"}, ""},
		{Target{TargetIP, "203.0.113.7/32"}, ""}, // same object as the bare form
		{Target{TargetIP, "2001:db8::/64"}, ""},
	}
	names := make([]string, len(cases))
	for i, c := range cases {
		got, err := CRName(c.target)
		if err != nil {
			t.Fatalf("CRName(%+v): %v", c.target, err)
		}
		names[i] = got
		if c.want != "" && got != c.want {
			t.Errorf("CRName(%+v) = %q, want %q", c.target, got, c.want)
		}
		if errs := validation.IsDNS1123Subdomain(got); len(errs) > 0 {
			t.Errorf("CRName(%+v) = %q is not a DNS-1123 subdomain: %v", c.target, got, errs)
		}
		// Determinism: the same input always names the same object.
		again, err := CRName(c.target)
		if err != nil || again != got {
			t.Errorf("CRName(%+v) not deterministic: %q then %q (err %v)", c.target, got, again, err)
		}
	}
	if names[0] != names[1] {
		t.Errorf("ID case-variants named differently: %q vs %q", names[0], names[1])
	}
	if names[2] != names[3] {
		t.Errorf("bare IP and /32 named differently: %q vs %q", names[2], names[3])
	}
	if !strings.HasPrefix(names[2], "ban-ip-") || len(names[2]) != len("ban-ip-")+12 {
		t.Errorf("IP CR name %q is not ban-ip- + 12 hex", names[2])
	}
	if _, err := CRName(Target{TargetIP, "nope"}); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("CRName on a malformed target: err = %v, want ErrInvalidTarget", err)
	}
}

// Every ID in the alphabet and a spread of CIDRs must yield DNS-1123 names —
// the CR name is what a Ban is created as, so an invalid one is an
// un-enforceable ban.
func TestCRNameAlwaysDNS1123(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	const alphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
	for i := 0; i < 300; i++ {
		var sb strings.Builder
		for j := 0; j < 6; j++ {
			sb.WriteByte(alphabet[rng.Intn(len(alphabet))])
		}
		name, err := CRName(Target{TargetBroadcastID, sb.String()})
		if err != nil {
			t.Fatalf("CRName(%q): %v", sb.String(), err)
		}
		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			t.Fatalf("CRName(%q) = %q invalid: %v", sb.String(), name, errs)
		}
	}
	for i := 0; i < 300; i++ {
		p := randomPrefix(rng)
		name, err := CRName(Target{TargetIP, p.String()})
		if err != nil {
			t.Fatalf("CRName(%q): %v", p, err)
		}
		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			t.Fatalf("CRName(%q) = %q invalid: %v", p, name, errs)
		}
	}
}

func randomPrefix(rng *rand.Rand) netip.Prefix {
	if rng.Intn(2) == 0 {
		var b [4]byte
		rng.Read(b[:])
		return netip.PrefixFrom(netip.AddrFrom4(b), rng.Intn(33)).Masked()
	}
	var b [16]byte
	rng.Read(b[:])
	return netip.PrefixFrom(netip.AddrFrom16(b), rng.Intn(129)).Masked()
}
