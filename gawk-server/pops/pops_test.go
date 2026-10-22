package pops

import (
	"encoding/base32"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func mt(t time.Time) *metav1.Time { m := metav1.NewTime(t); return &m }

var t0 = time.Date(2026, 10, 21, 18, 0, 0, 0, time.UTC)

func TestConstants(t *testing.T) {
	if GroupName != "gawk.ioio.fi" || Version != "v1alpha1" || Kind != "Pop" || ListKind != "PopList" || Resource != "pops" {
		t.Errorf("GVK constants drifted: %s/%s %s %s %s", GroupName, Version, Kind, ListKind, Resource)
	}
	if LabelKey != "gawk/pop" || MaxNameLen != 63 || PairingPrefix != "gawk-pair:" {
		t.Errorf("LabelKey %q MaxNameLen %d PairingPrefix %q", LabelKey, MaxNameLen, PairingPrefix)
	}
	if DefaultUplinkMbps != 25 || DefaultUplinkStreams != 3 || DefaultMaxBroadcasts != 40 || DefaultMaxDownlinkStreams != 20 {
		t.Error("budget defaults drifted from docs/76 §9")
	}
	if DefaultPairingTTL != 72*time.Hour {
		t.Errorf("DefaultPairingTTL = %v", DefaultPairingTTL)
	}
	if GroupVersionResource.Resource != "pops" || GroupVersionResource.Group != GroupName || GroupVersionResource.Version != Version {
		t.Errorf("GroupVersionResource = %v", GroupVersionResource)
	}
}

func TestSchemeRegistration(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	gvk := schema.GroupVersionKind{Group: GroupName, Version: Version, Kind: Kind}
	obj, err := s.New(gvk)
	if err != nil {
		t.Fatalf("scheme.New(%v): %v", gvk, err)
	}
	if _, ok := obj.(*Pop); !ok {
		t.Fatalf("scheme.New = %T, want *Pop", obj)
	}
	if obj, err := s.New(gvk.GroupVersion().WithKind(ListKind)); err != nil {
		t.Fatalf("scheme.New(%v): %v", ListKind, err)
	} else if _, ok := obj.(*PopList); !ok {
		t.Fatalf("scheme.New(list) = %T", obj)
	}
}

func TestValidateName(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"assembly-2026", true},
		{"a", true},
		{strings.Repeat("a", 63), true},
		{strings.Repeat("a", 64), false},
		{"", false},
		{"Assembly", false},
		{"-abc", false},
		{"abc-", false},
		{"a.b", false},
		{"a_b", false},
		{"a b", false},
	} {
		err := ValidateName(tc.in)
		if tc.ok != (err == nil) {
			t.Errorf("ValidateName(%q) = %v, want ok=%v", tc.in, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrBadName) {
			t.Errorf("ValidateName(%q) = %v, want ErrBadName", tc.in, err)
		}
	}
}

func TestValidateWallCode(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"assembly-2026", true},
		{"Assembly-2026", true},
		{"ab", false},
		{strings.Repeat("a", 33), false}, // a 33..63-char Pop name cannot be its own wall code
		{"5UP4XW", false},                // dynamic-shaped: would be typed into the join box as a broadcast
		{"a_b_c", false},
	} {
		if err := ValidateWallCode(tc.in); tc.ok != (err == nil) {
			t.Errorf("ValidateWallCode(%q) = %v, want ok=%v", tc.in, err, tc.ok)
		}
	}
}

var tokenShape = regexp.MustCompile(`^[A-Z2-7]{5}-[A-Z2-7]{5}-[A-Z2-7]{5}-[A-Z2-7]{5}-[A-Z2-7]{5}-[A-Z2-7]$`)

func TestNewPairingTokenShapeAndEntropy(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		tok, err := NewPairingToken()
		if err != nil {
			t.Fatal(err)
		}
		if !tokenShape.MatchString(tok) {
			t.Fatalf("token %q: want upper base32, no pad, '-' every 5", tok)
		}
		raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ReplaceAll(tok, "-", ""))
		if err != nil || len(raw) != 16 {
			t.Fatalf("token %q decodes to %d bytes, %v; want 128 bits", tok, len(raw), err)
		}
		if seen[tok] {
			t.Fatalf("duplicate token %q", tok)
		}
		seen[tok] = true
	}
}

func TestPairingFormatParseVectors(t *testing.T) {
	// A fixed 128-bit vector: 16 bytes 0x00..0x0f.
	raw := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	if enc != "AAAQEAYEAUDAOCAJBIFQYDIOB4" {
		t.Fatalf("base32 vector = %q", enc)
	}
	const canonical = "AAAQE-AYEAU-DAOCA-JBIFQ-YDIOB-4"
	s := FormatPairing("api.gawk.ioio.fi:4433", canonical)
	if s != "gawk-pair:api.gawk.ioio.fi:4433/AAAQE-AYEAU-DAOCA-JBIFQ-YDIOB-4" {
		t.Fatalf("FormatPairing = %q", s)
	}
	ep, got, err := ParsePairing(s)
	if err != nil || ep != "api.gawk.ioio.fi:4433" || got != canonical {
		t.Fatalf("ParsePairing = %q %q %v", ep, got, err)
	}
	// Tolerant of what a human paste does: whitespace, lower case, no dashes.
	ep, got, err = ParsePairing("  gawk-pair:api.gawk.ioio.fi:4433/aaaqeayeaudaocajbifqydiob4\n")
	if err != nil || ep != "api.gawk.ioio.fi:4433" || got != canonical {
		t.Fatalf("tolerant ParsePairing = %q %q %v", ep, got, err)
	}
	// IPv6 endpoint.
	ep, _, err = ParsePairing("gawk-pair:[2001:db8::1]:4433/" + canonical)
	if err != nil || ep != "[2001:db8::1]:4433" {
		t.Fatalf("v6 ParsePairing = %q %v", ep, err)
	}
	// The hash ignores the presentation.
	want := TokenSHA256(canonical)
	if TokenSHA256("aaaqeayeaudaocajbifqydiob4") != want || TokenSHA256(enc) != want {
		t.Error("TokenSHA256 depends on dashes or case")
	}
	// Pinned: sha256("AAAQEAYEAUDAOCAJBIFQYDIOB4"), so gawk-admin and the
	// relays can never disagree on the stored form.
	if want != "b3f0010fec117d12f0a1d428855f4e1b64bcc4ab28c8cfc4b168d4b677a37578" {
		t.Errorf("TokenSHA256 vector = %q", want)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(want) {
		t.Errorf("TokenSHA256 = %q, want lower 64-hex", want)
	}
	if TokenSHA256(canonical) == TokenSHA256("AAAQE-AYEAU-DAOCA-JBIFQ-YDIOB-5") {
		t.Error("distinct tokens hash equal")
	}

	for _, bad := range []string{
		"",
		"api.gawk.ioio.fi:4433/" + canonical, // no prefix
		"gawk-pair:/" + canonical,            // no endpoint
		"gawk-pair:api.gawk.ioio.fi/" + canonical,   // no port
		"gawk-pair:api.gawk.ioio.fi:0/" + canonical, // port 0
		"gawk-pair:api.gawk.ioio.fi:99999/" + canonical,
		"gawk-pair:api.gawk.ioio.fi:4433",             // no token
		"gawk-pair:api.gawk.ioio.fi:4433/",            // empty token
		"gawk-pair:api.gawk.ioio.fi:4433/AAAQE-AYEAU", // short token
		"gawk-pair:api.gawk.ioio.fi:4433/" + canonical + "AA",
		"gawk-pair:api.gawk.ioio.fi:4433/AAAQE-AYEAU-DAOCA-JBIFQ-YDIOB-1", // '1' not base32
		"gawk-pair:https://api.gawk.ioio.fi:4433/" + canonical,
		"gawk-pair:user@api.gawk.ioio.fi:4433/" + canonical,
	} {
		if _, _, err := ParsePairing(bad); !errors.Is(err, ErrBadPairing) {
			t.Errorf("ParsePairing(%q) = %v, want ErrBadPairing", bad, err)
		}
	}
}

func TestCredentials(t *testing.T) {
	a, err := NewCredential()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewCredential()
	if a == b {
		t.Fatal("credentials repeat")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(a) {
		t.Fatalf("credential %q: want 256 bits base64url raw", a)
	}
	h := CredentialSHA256(a)
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(h) || h == CredentialSHA256(b) {
		t.Fatalf("CredentialSHA256 = %q", h)
	}
	// sha256("abc")
	if CredentialSHA256("abc") != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("CredentialSHA256 vector = %q", CredentialSHA256("abc"))
	}
}

// paired builds a Pop whose credential was paired with tokenSHA.
func paired(cred, tokenSHA string) *Pop {
	return &Pop{
		ObjectMeta: metav1.ObjectMeta{Name: "assembly-2026"},
		Spec: PopSpec{
			DisplayName: "Assembly 2026",
			Pairing:     Pairing{TokenSHA256: tokenSHA, IssuedAt: mt(t0), ExpiresAt: mt(t0.Add(72 * time.Hour))},
		},
		Status: PopStatus{Credential: &Credential{SHA256: CredentialSHA256(cred), PairingTokenSHA256: tokenSHA, PairedAt: mt(t0.Add(time.Minute))}},
	}
}

func TestCheckCredentialTruthTable(t *testing.T) {
	const cred = "the-credential"
	tokA := TokenSHA256("AAAAA-AAAAA-AAAAA-AAAAA-AAAAA-A")
	now := t0.Add(time.Hour)

	for _, tc := range []struct {
		name string
		pop  func() *Pop
		cred string
		now  time.Time
		want error
	}{
		{"current", func() *Pop { return paired(cred, tokA) }, cred, now, nil},
		{"wrong credential", func() *Pop { return paired(cred, tokA) }, "other", now, ErrCredentialNotCurrent},
		{"empty credential", func() *Pop { return paired(cred, tokA) }, "", now, ErrCredentialNotCurrent},
		{"never paired", func() *Pop { p := paired(cred, tokA); p.Status.Credential = nil; return p }, cred, now, ErrCredentialNotCurrent},
		{"re-paired: old credential refused at once", func() *Pop {
			p := paired(cred, tokA)
			p.Spec.Pairing.TokenSHA256 = TokenSHA256("BBBBB-BBBBB-BBBBB-BBBBB-BBBBB-B")
			return p
		}, cred, now, ErrCredentialNotCurrent},
		{"outlives pairing expiresAt", func() *Pop { return paired(cred, tokA) }, cred, t0.Add(30 * 24 * time.Hour), nil},
		{"revoked", func() *Pop { p := paired(cred, tokA); p.Spec.Revoked = true; return p }, cred, now, ErrRevoked},
		{"revoked, wrong credential reveals nothing", func() *Pop { p := paired(cred, tokA); p.Spec.Revoked = true; return p }, "other", now, ErrCredentialNotCurrent},
		{"past notAfter", func() *Pop { p := paired(cred, tokA); p.Spec.NotAfter = mt(now.Add(-time.Second)); return p }, cred, now, ErrExpired},
		{"before notAfter", func() *Pop { p := paired(cred, tokA); p.Spec.NotAfter = mt(now.Add(time.Second)); return p }, cred, now, nil},
		{"file pop: no pairing at all", func() *Pop {
			fp := FilePop{Name: "lan", UID: "6f1c2d3e-4a5b-4c6d-8e7f-0123456789ab", CredentialSHA256: CredentialSHA256(cred)}
			p := fp.ToPop()
			return &p
		}, cred, now, nil},
	} {
		err := tc.pop().CheckCredential(tc.cred, tc.now)
		if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Errorf("%s: CheckCredential = %v, want %v", tc.name, err, tc.want)
		}
	}
	var nilPop *Pop
	if err := nilPop.CheckCredential(cred, now); !errors.Is(err, ErrCredentialNotCurrent) {
		t.Errorf("nil pop: %v", err)
	}
}

// Two Re-pairs in the same second must still differ: the token is random,
// so its hash is, even though metav1.Time has whole-second resolution.
func TestTwoRepairsInTheSameSecondDiffer(t *testing.T) {
	const cred = "c"
	tok1, _ := NewPairingToken()
	tok2, _ := NewPairingToken()
	p := paired(cred, TokenSHA256(tok1))
	// Re-pair #1 and #2 both at t0 (same second).
	p.Spec.Pairing = Pairing{TokenSHA256: TokenSHA256(tok1), IssuedAt: mt(t0), ExpiresAt: mt(t0.Add(DefaultPairingTTL))}
	if err := p.CheckCredential(cred, t0); err != nil {
		t.Fatalf("credential under its own pairing: %v", err)
	}
	p.Spec.Pairing = Pairing{TokenSHA256: TokenSHA256(tok2), IssuedAt: mt(t0), ExpiresAt: mt(t0.Add(DefaultPairingTTL))}
	if err := p.CheckCredential(cred, t0); !errors.Is(err, ErrCredentialNotCurrent) {
		t.Fatalf("second same-second re-pair left the old credential current: %v", err)
	}
	if err := p.Redeemable(TokenSHA256(tok1), t0); !errors.Is(err, ErrPairingUnknown) {
		t.Errorf("superseded string redeemable: %v", err)
	}
	if err := p.Redeemable(TokenSHA256(tok2), t0); err != nil {
		t.Errorf("current string not redeemable: %v", err)
	}
}

func TestRedeemableTruthTable(t *testing.T) {
	tokA := TokenSHA256("AAAAA-AAAAA-AAAAA-AAAAA-AAAAA-A")
	tokB := TokenSHA256("BBBBB-BBBBB-BBBBB-BBBBB-BBBBB-B")
	fresh := func() *Pop {
		return &Pop{Spec: PopSpec{Pairing: Pairing{TokenSHA256: tokA, IssuedAt: mt(t0), ExpiresAt: mt(t0.Add(72 * time.Hour))}}}
	}
	for _, tc := range []struct {
		name string
		pop  func() *Pop
		tok  string
		now  time.Time
		want error
	}{
		{"fresh", fresh, tokA, t0.Add(time.Hour), nil},
		{"wrong token", fresh, tokB, t0.Add(time.Hour), ErrPairingUnknown},
		{"empty token", fresh, "", t0.Add(time.Hour), ErrPairingUnknown},
		{"no pairing issued", func() *Pop { return &Pop{} }, "", t0, ErrPairingUnknown},
		{"expired", fresh, tokA, t0.Add(72*time.Hour + time.Second), ErrPairingExpired},
		{"used", func() *Pop { return paired("c", tokA) }, tokA, t0.Add(time.Hour), ErrPairingUsed},
		{"used beats expired", func() *Pop { return paired("c", tokA) }, tokA, t0.Add(100 * time.Hour), ErrPairingUsed},
		{"re-paired, old credential still in status", func() *Pop {
			p := paired("c", tokB)
			p.Spec.Pairing.TokenSHA256 = tokA
			return p
		}, tokA, t0.Add(time.Hour), nil},
		{"revoked", func() *Pop { p := fresh(); p.Spec.Revoked = true; return p }, tokA, t0.Add(time.Hour), ErrRevoked},
		{"past notAfter", func() *Pop { p := fresh(); p.Spec.NotAfter = mt(t0); return p }, tokA, t0.Add(time.Hour), ErrExpired},
	} {
		err := tc.pop().Redeemable(tc.tok, tc.now)
		if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Errorf("%s: Redeemable = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestEffectiveDefaults(t *testing.T) {
	var p Pop
	if got := p.EffectiveUplink(); got != (Budget{MaxMbps: 25, MaxStreams: 3}) {
		t.Errorf("EffectiveUplink default = %+v", got)
	}
	if got := p.EffectiveLimits(); got != (Limits{MaxBroadcasts: 40, MaxDownlinkStreams: 20}) {
		t.Errorf("EffectiveLimits default = %+v", got)
	}
	if !p.AutoSwitch() {
		t.Error("AutoSwitch default is true")
	}
	f := false
	p.Spec = PopSpec{Uplink: Budget{MaxMbps: 10, MaxStreams: 1}, Limits: Limits{MaxBroadcasts: 5, MaxDownlinkStreams: 2}, Guests: Guests{AutoSwitch: &f}}
	if got := p.EffectiveUplink(); got != (Budget{MaxMbps: 10, MaxStreams: 1}) {
		t.Errorf("EffectiveUplink = %+v", got)
	}
	if got := p.EffectiveLimits(); got != (Limits{MaxBroadcasts: 5, MaxDownlinkStreams: 2}) {
		t.Errorf("EffectiveLimits = %+v", got)
	}
	if p.AutoSwitch() {
		t.Error("autoSwitch:false ignored")
	}
	p.Spec.Uplink = Budget{MaxMbps: 10}
	if got := p.EffectiveUplink(); got != (Budget{MaxMbps: 10, MaxStreams: 3}) {
		t.Errorf("partial uplink = %+v", got)
	}
}

func TestFilePopToPop(t *testing.T) {
	na := t0.Add(48 * time.Hour)
	f := false
	fp := FilePop{
		Name: "assembly-2026", UID: "6f1c2d3e-4a5b-4c6d-8e7f-0123456789ab", DisplayName: "Assembly 2026",
		CredentialSHA256: strings.Repeat("ab", 32), NotAfter: &na, Revoked: true,
		Uplink: Budget{MaxMbps: 30}, Downlink: Budget{MaxStreams: 4},
		Limits: Limits{MaxBroadcasts: 8}, Guests: Guests{AutoSwitch: &f}, Wall: &Wall{RoomCode: "assembly-2026"},
	}
	p := fp.ToPop()
	if p.Name != fp.Name || p.UID != types.UID(fp.UID) || p.Spec.DisplayName != fp.DisplayName {
		t.Errorf("meta = %+v", p.ObjectMeta)
	}
	if p.Kind != Kind || p.APIVersion != SchemeGroupVersion.String() {
		t.Errorf("TypeMeta = %+v", p.TypeMeta)
	}
	if p.Spec.NotAfter == nil || !p.Spec.NotAfter.Time.Equal(na) || !p.Spec.Revoked {
		t.Errorf("notAfter/revoked = %v %v", p.Spec.NotAfter, p.Spec.Revoked)
	}
	if p.Spec.Uplink.MaxMbps != 30 || p.Spec.Downlink.MaxStreams != 4 || p.Spec.Limits.MaxBroadcasts != 8 || p.AutoSwitch() {
		t.Errorf("spec = %+v", p.Spec)
	}
	if p.Spec.Wall == nil || p.Spec.Wall.RoomCode != "assembly-2026" {
		t.Errorf("wall = %+v", p.Spec.Wall)
	}
	if p.Status.Credential == nil || p.Status.Credential.SHA256 != fp.CredentialSHA256 ||
		p.Status.Credential.PairingTokenSHA256 != "" || p.Spec.Pairing.TokenSHA256 != "" {
		t.Errorf("credential = %+v pairing %+v", p.Status.Credential, p.Spec.Pairing)
	}
	if !p.CredentialCurrent() {
		t.Error("a file pop's credential is current")
	}
	// No aliasing back into the file entry.
	p.Spec.Wall.RoomCode = "x"
	*p.Spec.Guests.AutoSwitch = true
	p.Spec.NotAfter.Time = time.Time{}
	if fp.Wall.RoomCode != "assembly-2026" || *fp.Guests.AutoSwitch || !fp.NotAfter.Equal(na) {
		t.Error("ToPop aliases the FilePop")
	}
	// The JSON shape of a file entry.
	var back FilePop
	if err := json.Unmarshal([]byte(`{"name":"lan","uid":"u","displayName":"LAN","credentialSHA256":"h","notAfter":"2026-11-09T00:00:00Z","uplink":{"maxMbps":5},"guests":{"autoSwitch":false},"wall":{"roomCode":"lan-wall"}}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.Name != "lan" || back.UID != "u" || back.CredentialSHA256 != "h" || back.NotAfter == nil || back.Uplink.MaxMbps != 5 || back.Guests.AutoSwitch == nil || back.Wall == nil {
		t.Errorf("FilePop JSON = %+v", back)
	}
}

// The YAML in docs/76 §9, as JSON, must round-trip into Pop.
func TestPopUnmarshalsTheDocumentedShape(t *testing.T) {
	const doc = `{
	  "apiVersion": "gawk.ioio.fi/v1alpha1", "kind": "Pop",
	  "metadata": {"name": "assembly-2026"},
	  "spec": {
	    "displayName": "Assembly 2026",
	    "notAfter": "2026-11-09T00:00:00Z",
	    "revoked": false,
	    "pairing": {"tokenSHA256": "aa", "issuedAt": "2026-10-21T18:00:00Z", "expiresAt": "2026-10-24T18:00:00Z"},
	    "uplink": {"maxMbps": 25, "maxStreams": 3},
	    "downlink": {"maxMbps": 0, "maxStreams": 0},
	    "limits": {"maxBroadcasts": 40, "maxDownlinkStreams": 20},
	    "guests": {"autoSwitch": true},
	    "wall": {"roomCode": "assembly-2026"},
	    "namedAddress": {"enabled": true}
	  },
	  "status": {
	    "credential": {"sha256": "cc", "pairedAt": "2026-10-21T18:04:11Z", "pairingTokenSHA256": "aa"},
	    "lease": {"holder": "gawk-server-7c9f", "addr": "10.42.0.17:4433", "generation": 4, "renewedAt": "2026-10-21T18:05:00Z"},
	    "link": {"connected": true, "since": "2026-10-21T18:04:12Z", "version": "0.44.0", "proto": 1,
	             "publicAddrs": ["203.0.113.7", "2001:db8:1:2::/64"], "lanAddr": "192.168.1.20", "pinnedHashes": ["h1", "h2"]},
	    "tls": {"host": "p-7k2m9x.pop.gawk.ioio.fi", "csr": "CSR", "certificate": "CERT", "issuedForCSR": "dd", "notAfter": "2026-10-28T00:00:00Z"}
	  }
	}`
	var p Pop
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatal(err)
	}
	s := p.Spec
	if s.DisplayName != "Assembly 2026" || s.NotAfter == nil || s.Pairing.TokenSHA256 != "aa" || s.Pairing.ExpiresAt == nil ||
		s.Uplink.MaxMbps != 25 || s.Uplink.MaxStreams != 3 || s.Limits.MaxBroadcasts != 40 || s.Limits.MaxDownlinkStreams != 20 ||
		s.Guests.AutoSwitch == nil || !*s.Guests.AutoSwitch || s.Wall == nil || s.Wall.RoomCode != "assembly-2026" || !s.NamedAddress.Enabled {
		t.Errorf("spec = %+v", s)
	}
	st := p.Status
	if st.Credential == nil || st.Credential.SHA256 != "cc" || st.Credential.PairingTokenSHA256 != "aa" || st.Credential.PairedAt == nil {
		t.Errorf("credential = %+v", st.Credential)
	}
	if st.Lease == nil || st.Lease.Holder != "gawk-server-7c9f" || st.Lease.Generation != 4 || st.Lease.Addr != "10.42.0.17:4433" {
		t.Errorf("lease = %+v", st.Lease)
	}
	if st.Link == nil || !st.Link.Connected || st.Link.Proto != 1 || len(st.Link.PublicAddrs) != 2 || st.Link.LanAddr != "192.168.1.20" || len(st.Link.PinnedHashes) != 2 || st.Link.Version != "0.44.0" {
		t.Errorf("link = %+v", st.Link)
	}
	if st.TLS == nil || st.TLS.Host != "p-7k2m9x.pop.gawk.ioio.fi" || st.TLS.CSR != "CSR" || st.TLS.Certificate != "CERT" || st.TLS.IssuedForCSR != "dd" || st.TLS.NotAfter == nil {
		t.Errorf("tls = %+v", st.TLS)
	}
	if !p.CredentialCurrent() {
		t.Error("credential paired with the current string is current")
	}
	// Empty status and spec sub-blocks are omitted, so a status merge patch
	// written by one owner never carries the other's paths as nulls.
	out, _ := json.Marshal(PopStatus{Credential: &Credential{SHA256: "x"}})
	if strings.Contains(string(out), "lease") || strings.Contains(string(out), "tls") || strings.Contains(string(out), "link") {
		t.Errorf("status marshals empties: %s", out)
	}
	out, _ = json.Marshal(PopSpec{DisplayName: "x"})
	if strings.Contains(string(out), "wall") || strings.Contains(string(out), "notAfter") {
		t.Errorf("spec marshals empties: %s", out)
	}
}

// Hand-written deepcopy: mutating the copy must not touch the original
// through any pointer, slice or map field.
func TestDeepCopyDoesNotAlias(t *testing.T) {
	tr := true
	orig := &Pop{
		TypeMeta:   metav1.TypeMeta{APIVersion: "gawk.ioio.fi/v1alpha1", Kind: "Pop"},
		ObjectMeta: metav1.ObjectMeta{Name: "assembly-2026", Labels: map[string]string{"a": "b"}},
		Spec: PopSpec{
			NotAfter: mt(t0),
			Pairing:  Pairing{TokenSHA256: "t", IssuedAt: mt(t0), ExpiresAt: mt(t0)},
			Guests:   Guests{AutoSwitch: &tr},
			Wall:     &Wall{RoomCode: "w"},
		},
		Status: PopStatus{
			Credential: &Credential{SHA256: "s", PairedAt: mt(t0)},
			Lease:      &Lease{Holder: "h", RenewedAt: mt(t0)},
			Link:       &Link{Since: mt(t0), PublicAddrs: []string{"203.0.113.7"}, PinnedHashes: []string{"p"}},
			TLS:        &TLS{Host: "x", NotAfter: mt(t0)},
		},
	}
	cp, ok := orig.DeepCopyObject().(*Pop)
	if !ok {
		t.Fatal("DeepCopyObject did not return a *Pop")
	}
	later := t0.Add(time.Hour)
	cp.Labels["a"] = "mutated"
	cp.Spec.NotAfter.Time = later
	cp.Spec.Pairing.IssuedAt.Time = later
	cp.Spec.Pairing.ExpiresAt.Time = later
	*cp.Spec.Guests.AutoSwitch = false
	cp.Spec.Wall.RoomCode = "mutated"
	cp.Status.Credential.SHA256 = "mutated"
	cp.Status.Credential.PairedAt.Time = later
	cp.Status.Lease.Holder = "mutated"
	cp.Status.Lease.RenewedAt.Time = later
	cp.Status.Link.Since.Time = later
	cp.Status.Link.PublicAddrs[0] = "mutated"
	cp.Status.Link.PinnedHashes[0] = "mutated"
	cp.Status.TLS.Host = "mutated"
	cp.Status.TLS.NotAfter.Time = later

	switch {
	case orig.Labels["a"] != "b":
		t.Error("labels aliased")
	case !orig.Spec.NotAfter.Time.Equal(t0):
		t.Error("notAfter aliased")
	case !orig.Spec.Pairing.IssuedAt.Time.Equal(t0), !orig.Spec.Pairing.ExpiresAt.Time.Equal(t0):
		t.Error("pairing aliased")
	case !*orig.Spec.Guests.AutoSwitch:
		t.Error("autoSwitch aliased")
	case orig.Spec.Wall.RoomCode != "w":
		t.Error("wall aliased")
	case orig.Status.Credential.SHA256 != "s", !orig.Status.Credential.PairedAt.Time.Equal(t0):
		t.Error("credential aliased")
	case orig.Status.Lease.Holder != "h", !orig.Status.Lease.RenewedAt.Time.Equal(t0):
		t.Error("lease aliased")
	case !orig.Status.Link.Since.Time.Equal(t0), orig.Status.Link.PublicAddrs[0] != "203.0.113.7", orig.Status.Link.PinnedHashes[0] != "p":
		t.Error("link aliased")
	case orig.Status.TLS.Host != "x", !orig.Status.TLS.NotAfter.Time.Equal(t0):
		t.Error("tls aliased")
	}

	list := &PopList{Items: []Pop{*orig.DeepCopy()}}
	lcp, ok := list.DeepCopyObject().(*PopList)
	if !ok {
		t.Fatal("PopList.DeepCopyObject did not return a *PopList")
	}
	lcp.Items[0].Spec.DisplayName = "mutated"
	lcp.Items[0].Spec.Wall.RoomCode = "mutated"
	if list.Items[0].Spec.DisplayName != "" || list.Items[0].Spec.Wall.RoomCode != "w" {
		t.Error("PopList items aliased")
	}
	var nilPop *Pop
	if nilPop.DeepCopy() != nil || nilPop.DeepCopyObject() != nil {
		t.Error("nil Pop DeepCopy is not nil")
	}
	var nilList *PopList
	if nilList.DeepCopy() != nil || nilList.DeepCopyObject() != nil {
		t.Error("nil PopList DeepCopy is not nil")
	}
}
