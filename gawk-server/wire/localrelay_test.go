package wire

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// --- R74 LocalRelayInfo (0x18) and LocalRelayOffer (0x19) (docs/76 §9) ---

// Golden vectors, computed by hand from the layouts in localrelay.go.
// Restated byte-identically in both mirrors (wire.ts, crates/wire).
//
// A, a linked PoP whose broadcaster is on the party wall:
//
//	01                         version
//	18                         type = LocalRelayInfo
//	00                         flags (strict, 0)
//	03                         state = LocalRelayLinked | LocalRelayOnWall
//	0d "Assembly 2026"         name
//	14 "https://gawk.ioio.fi"  home app origin
//	0d "assembly-2026"         wall code
const goldenLocalRelayInfoLinkedHex = "01180003" +
	"0d417373656d626c792032303236" +
	"1468747470733a2f2f6761776b2e696f696f2e6669" +
	"0d617373656d626c792d32303236"

// B, a standalone PoP: state 0, no origin, no wall.
//
//	01 18 00 00                version, type, flags, state
//	0d "Assembly 2026"         name
//	00                         origin (empty)
//	00                         wall code (empty)
const goldenLocalRelayInfoStandaloneHex = "011800000d417373656d626c7920323032360000"

// The offer: handle 6f1c2d3e-4a5b-4c6d-8e7f-0123456789ab, name "Assembly
// 2026", validity 3600 s, and two candidates.
//
//	01 19 00                   version, type, flags
//	24 "6f1c2d3e-…-0123456789ab"  handle (36 bytes)
//	0d "Assembly 2026"         name
//	00 00 0e 10                validity = 3600 s
//	02                         two candidates
//	00 26 "https://p-7k2m9x.pop.gawk.ioio.fi:4433"  URL (38 bytes)
//	00                         no hashes
//	00 19 "https://192.168.1.20:4433"               URL (25 bytes)
//	02                         two hashes
//	00 01 … 1f                 SHA-256 #1
//	20 21 … 3f                 SHA-256 #2
const goldenLocalRelayOfferHex = "011900" +
	"2436663163326433652d346135622d346336642d386537662d303132333435363738396162" +
	"0d417373656d626c792032303236" +
	"00000e10" +
	"02" +
	"002668747470733a2f2f702d376b326d39782e706f702e6761776b2e696f696f2e66693a34343333" +
	"00" +
	"001968747470733a2f2f3139322e3136382e312e32303a34343333" +
	"02" +
	"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f" +
	"202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"

func goldenLocalRelayInfoLinked() LocalRelayInfo {
	return LocalRelayInfo{
		Linked:     true,
		OnWall:     true,
		Name:       "Assembly 2026",
		HomeOrigin: "https://gawk.ioio.fi",
		WallCode:   "assembly-2026",
	}
}

func goldenLocalRelayOffer() LocalRelayOffer {
	var h1, h2 [32]byte
	for i := range h1 {
		h1[i] = byte(i)
		h2[i] = byte(0x20 + i)
	}
	return LocalRelayOffer{
		Handle:   "6f1c2d3e-4a5b-4c6d-8e7f-0123456789ab",
		Name:     "Assembly 2026",
		Validity: 3600,
		Candidates: []LocalRelayCandidate{
			{URL: "https://p-7k2m9x.pop.gawk.ioio.fi:4433"},
			{URL: "https://192.168.1.20:4433", Hashes: [][32]byte{h1, h2}},
		},
	}
}

func TestGoldenLocalRelayInfo(t *testing.T) {
	cases := []struct {
		name string
		hex  string
		msg  LocalRelayInfo
	}{
		{"linked on wall", goldenLocalRelayInfoLinkedHex, goldenLocalRelayInfoLinked()},
		{"standalone", goldenLocalRelayInfoStandaloneHex, LocalRelayInfo{Name: "Assembly 2026"}},
	}
	for _, tc := range cases {
		want := mustHex(t, tc.hex)
		got, err := AppendLocalRelayInfo(nil, tc.msg)
		if err != nil {
			t.Fatalf("%s: AppendLocalRelayInfo: %v", tc.name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: AppendLocalRelayInfo = %x, want %x", tc.name, got, want)
		}
		parsed, err := ParseLocalRelayInfo(want)
		if err != nil {
			t.Fatalf("%s: ParseLocalRelayInfo: %v", tc.name, err)
		}
		if parsed != tc.msg {
			t.Errorf("%s: ParseLocalRelayInfo = %+v, want %+v", tc.name, parsed, tc.msg)
		}
	}
}

func TestLocalRelayInfoToleratesTrailingBytesAndUnknownState(t *testing.T) {
	msg := mustHex(t, goldenLocalRelayInfoLinkedHex+"a1b2c3")
	got, err := ParseLocalRelayInfo(msg)
	if err != nil {
		t.Fatalf("trailing bytes rejected: %v", err)
	}
	if got != goldenLocalRelayInfoLinked() {
		t.Fatalf("got %+v", got)
	}
	// Unknown state bits are ignored, known ones still read.
	msg = mustHex(t, goldenLocalRelayInfoStandaloneHex)
	msg[3] = 0xfd // every bit but OnWall
	got, err = ParseLocalRelayInfo(msg)
	if err != nil {
		t.Fatalf("unknown state bits rejected: %v", err)
	}
	if !got.Linked || got.OnWall {
		t.Fatalf("state 0xfd = %+v, want linked only", got)
	}
}

func TestLocalRelayInfoOriginRule(t *testing.T) {
	ok := []string{
		"",
		"https://gawk.ioio.fi",
		"https://gawk.ioio.fi:8443",
		"https://192.168.1.20:4433",
		"https://[fd00::1]:4433",
		"https://a",
		"https://a:65535",
		"http://localhost",
		"http://localhost:5173",
		"http://127.0.0.1",
		"http://127.0.0.1:4480",
	}
	for _, o := range ok {
		msg, err := AppendLocalRelayInfo(nil, LocalRelayInfo{Name: "x", Linked: o != "", HomeOrigin: o})
		if err != nil {
			t.Errorf("origin %q refused on append: %v", o, err)
			continue
		}
		got, err := ParseLocalRelayInfo(msg)
		if err != nil || got.HomeOrigin != o {
			t.Errorf("origin %q round trip = (%q, %v)", o, got.HomeOrigin, err)
		}
	}
	bad := []string{
		"https://gawk.ioio.fi/",
		"https://gawk.ioio.fi/app",
		"https://gawk.ioio.fi?x=1",
		"https://gawk.ioio.fi#f",
		"https://user@gawk.ioio.fi",
		"https://",
		"http://gawk.ioio.fi",
		"http://localhost.evil.example",
		"http://192.168.1.20",
		"ws://gawk.ioio.fi",
		"gawk.ioio.fi",
		"https://gawk.ioio.fi:notaport",
		"https://gawk ioio.fi",
		"https://gawk.ioio.fi\x7f",
		"https://a:0",
		"https://a:65536",
		"https://a:",
		"https://a:123456",
		"https://a::1",
		"https://[]",
		"https://[::1",
		"https://[::1]x",
		"https://[[::1]",
		"https://a]",
		"HTTPS://gawk.ioio.fi",
		"https://" + strings.Repeat("a", MaxLocalRelayOriginLen),
	}
	for _, o := range bad {
		if _, err := AppendLocalRelayInfo(nil, LocalRelayInfo{Name: "x", HomeOrigin: o}); !errors.Is(err, ErrBadLocalRelayInfo) {
			t.Errorf("origin %q append err = %v, want ErrBadLocalRelayInfo", o, err)
		}
		if len(o) > 255 {
			continue
		}
		msg := []byte{Version, TypeLocalRelayInfo, 0, 0, 0, uint8(len(o))}
		msg = append(msg, o...)
		msg = append(msg, 0)
		if _, err := ParseLocalRelayInfo(msg); !errors.Is(err, ErrBadLocalRelayInfo) {
			t.Errorf("origin %q parse err = %v, want ErrBadLocalRelayInfo", o, err)
		}
	}
}

// The candidate-URL rule, restated in both mirrors: the copies hand-check
// rather than lean on three different URL parsers.
func TestLocalRelayURLRule(t *testing.T) {
	ok := []string{
		"https://a",
		"https://p-7k2m9x.pop.gawk.ioio.fi:4433",
		"https://192.168.1.20:4433",
		"https://[fd00::1]:4433",
		"https://a.example/some/path",
		"https://a.example/",
	}
	bad := []string{
		"",
		"https://",
		"https:///path",
		"http://a.example",
		"HTTPS://a.example",
		"//a.example",
		"https://a.example?q",
		"https://a.example/p?q=1",
		"https://a.example/#f",
		"https://u@a.example",
		"https://a.example:0",
		"https://a.example:65536",
		"https://a.example:/p",
		"https://a.example:44x3",
		"https://[fd00::1",
		"https://[fd00::1]:",
		"https://a b",
		"https://a\x00",
		"https://\u00e9.example",
	}
	for _, u := range ok {
		if err := validateLocalRelayURL(u); err != nil {
			t.Errorf("%q refused: %v", u, err)
		}
	}
	for _, u := range bad {
		if err := validateLocalRelayURL(u); !errors.Is(err, ErrBadLocalRelayOffer) {
			t.Errorf("%q err = %v, want ErrBadLocalRelayOffer", u, err)
		}
	}
}

func TestLocalRelayInfoRejects(t *testing.T) {
	appendCases := []struct {
		name string
		msg  LocalRelayInfo
	}{
		{"oversize name", LocalRelayInfo{Name: strings.Repeat("a", MaxRelayIdentityNameLen+1)}},
		{"bad UTF-8 name", LocalRelayInfo{Name: "\xff"}},
		{"oversize wall", LocalRelayInfo{WallCode: strings.Repeat("a", MaxRoomCodeLen+1)}},
		{"bad UTF-8 wall", LocalRelayInfo{WallCode: "\xc3"}},
	}
	for _, tc := range appendCases {
		if _, err := AppendLocalRelayInfo(nil, tc.msg); !errors.Is(err, ErrBadLocalRelayInfo) {
			t.Errorf("append %s: err = %v, want ErrBadLocalRelayInfo", tc.name, err)
		}
	}

	golden := mustHex(t, goldenLocalRelayInfoStandaloneHex)
	with := func(f func(b []byte) []byte) []byte {
		b := append([]byte(nil), golden...)
		return f(b)
	}
	parseCases := []struct {
		name string
		msg  []byte
		want error
	}{
		{"empty", nil, ErrShortDatagram},
		{"6 bytes", golden[:6], ErrShortDatagram},
		{"bad version", with(func(b []byte) []byte { b[0] = 2; return b }), ErrBadVersion},
		{"bad type", with(func(b []byte) []byte { b[1] = TypeRelayIdentity; return b }), ErrBadType},
		{"flags set", with(func(b []byte) []byte { b[2] = 0x01; return b }), ErrBadLocalRelayInfo},
		{"name overruns", with(func(b []byte) []byte { b[4] = 0x30; return b }), ErrBadLocalRelayInfo},
		{"name oversize", append([]byte{1, 0x18, 0, 0, 65}, append(bytes.Repeat([]byte("a"), 65), 0, 0)...), ErrBadLocalRelayInfo},
		{"name bad UTF-8", []byte{1, 0x18, 0, 0, 1, 0xff, 0, 0}, ErrBadLocalRelayInfo},
		{"origin length missing", golden[:len(golden)-2], ErrBadLocalRelayInfo},
		{"wall length missing", golden[:len(golden)-1], ErrBadLocalRelayInfo},
		{"wall overruns", with(func(b []byte) []byte { b[len(b)-1] = 5; return b }), ErrBadLocalRelayInfo},
		{"wall oversize", append(golden[:len(golden)-1:len(golden)-1], append([]byte{33}, bytes.Repeat([]byte("a"), 33)...)...), ErrBadLocalRelayInfo},
		{"wall bad UTF-8", append(golden[:len(golden)-1:len(golden)-1], 1, 0xc3), ErrBadLocalRelayInfo},
	}
	for _, tc := range parseCases {
		if _, err := ParseLocalRelayInfo(tc.msg); !errors.Is(err, tc.want) {
			t.Errorf("parse %s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestGoldenLocalRelayOffer(t *testing.T) {
	want := mustHex(t, goldenLocalRelayOfferHex)
	got, err := AppendLocalRelayOffer(nil, goldenLocalRelayOffer())
	if err != nil {
		t.Fatalf("AppendLocalRelayOffer: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("AppendLocalRelayOffer = %x, want %x", got, want)
	}
	parsed, err := ParseLocalRelayOffer(want)
	if err != nil {
		t.Fatalf("ParseLocalRelayOffer: %v", err)
	}
	if !reflect.DeepEqual(parsed, goldenLocalRelayOffer()) {
		t.Errorf("ParseLocalRelayOffer = %+v, want %+v", parsed, goldenLocalRelayOffer())
	}
	// Trailing bytes are the extension space.
	parsed, err = ParseLocalRelayOffer(mustHex(t, goldenLocalRelayOfferHex+"a1b2c3"))
	if err != nil {
		t.Fatalf("trailing bytes rejected: %v", err)
	}
	if !reflect.DeepEqual(parsed, goldenLocalRelayOffer()) {
		t.Errorf("with trailing bytes = %+v", parsed)
	}
}

func TestLocalRelayOfferRejectsOnAppend(t *testing.T) {
	mod := func(f func(o *LocalRelayOffer)) LocalRelayOffer {
		o := goldenLocalRelayOffer()
		// Deep-copy candidates so the closure never aliases the golden value.
		o.Candidates = append([]LocalRelayCandidate(nil), o.Candidates...)
		f(&o)
		return o
	}
	h := [32]byte{}
	cases := []struct {
		name string
		msg  LocalRelayOffer
	}{
		{"empty handle", mod(func(o *LocalRelayOffer) { o.Handle = "" })},
		{"oversize handle", mod(func(o *LocalRelayOffer) { o.Handle = strings.Repeat("a", MaxLocalRelayHandleLen+1) })},
		{"non-printable handle", mod(func(o *LocalRelayOffer) { o.Handle = "a\x01b" })},
		{"oversize name", mod(func(o *LocalRelayOffer) { o.Name = strings.Repeat("a", MaxRelayIdentityNameLen+1) })},
		{"bad UTF-8 name", mod(func(o *LocalRelayOffer) { o.Name = "\xff" })},
		{"validity 0", mod(func(o *LocalRelayOffer) { o.Validity = 0 })},
		{"no candidates", mod(func(o *LocalRelayOffer) { o.Candidates = nil })},
		{"five candidates", mod(func(o *LocalRelayOffer) {
			o.Candidates = make([]LocalRelayCandidate, 5)
			for i := range o.Candidates {
				o.Candidates[i] = LocalRelayCandidate{URL: "https://a.example"}
			}
		})},
		{"three hashes", mod(func(o *LocalRelayOffer) { o.Candidates[1].Hashes = [][32]byte{h, h, h} })},
		{"http URL", mod(func(o *LocalRelayOffer) { o.Candidates[0].URL = "http://192.168.1.20:4433" })},
		{"relative URL", mod(func(o *LocalRelayOffer) { o.Candidates[0].URL = "/pop" })},
		{"empty URL", mod(func(o *LocalRelayOffer) { o.Candidates[0].URL = "" })},
		{"userinfo", mod(func(o *LocalRelayOffer) { o.Candidates[0].URL = "https://u:p@192.168.1.20:4433" })},
		{"query", mod(func(o *LocalRelayOffer) { o.Candidates[0].URL = "https://192.168.1.20:4433/?a=1" })},
		{"fragment", mod(func(o *LocalRelayOffer) { o.Candidates[0].URL = "https://192.168.1.20:4433#x" })},
		{"space", mod(func(o *LocalRelayOffer) { o.Candidates[0].URL = "https://a b.example" })},
		{"oversize URL", mod(func(o *LocalRelayOffer) {
			o.Candidates[0].URL = "https://a.example/" + strings.Repeat("a", MaxLocalRelayURLLen)
		})},
	}
	for _, tc := range cases {
		if _, err := AppendLocalRelayOffer(nil, tc.msg); !errors.Is(err, ErrBadLocalRelayOffer) {
			t.Errorf("%s: err = %v, want ErrBadLocalRelayOffer", tc.name, err)
		}
	}
}

func TestLocalRelayOfferRejectsOnParse(t *testing.T) {
	golden := mustHex(t, goldenLocalRelayOfferHex)
	// Offsets into the golden vector.
	const (
		handleLenAt = 3
		nameLenAt   = handleLenAt + 1 + 36
		validityAt  = nameLenAt + 1 + 13
		candCountAt = validityAt + 4
		url1LenAt   = candCountAt + 1
		hash1At     = url1LenAt + 2 + 38
		url2LenAt   = hash1At + 1
		hash2At     = url2LenAt + 2 + 25
	)
	with := func(f func(b []byte) []byte) []byte {
		return f(append([]byte(nil), golden...))
	}
	// replaceURL1 swaps candidate 1's URL for u.
	replaceURL1 := func(u string) []byte {
		b := append([]byte(nil), golden[:url1LenAt]...)
		b = append(b, byte(len(u)>>8), byte(len(u)))
		b = append(b, u...)
		return append(b, golden[hash1At:]...)
	}
	cases := []struct {
		name string
		msg  []byte
		want error
	}{
		{"empty", nil, ErrShortDatagram},
		{"short", golden[:3], ErrShortDatagram},
		{"bad version", with(func(b []byte) []byte { b[0] = 2; return b }), ErrBadVersion},
		{"bad type", with(func(b []byte) []byte { b[1] = TypeLocalRelayInfo; return b }), ErrBadType},
		{"flags set", with(func(b []byte) []byte { b[2] = 0x80; return b }), ErrBadLocalRelayOffer},
		{"empty handle", append([]byte{1, 0x19, 0, 0}, golden[nameLenAt:]...), ErrBadLocalRelayOffer},
		{"oversize handle", append(append([]byte{1, 0x19, 0, 65}, bytes.Repeat([]byte("a"), 65)...), golden[nameLenAt:]...), ErrBadLocalRelayOffer},
		{"non-printable handle", with(func(b []byte) []byte { b[handleLenAt+1] = 0x01; return b }), ErrBadLocalRelayOffer},
		{"bad UTF-8 name", with(func(b []byte) []byte { b[nameLenAt+1] = 0xff; return b }), ErrBadLocalRelayOffer},
		{"validity 0", with(func(b []byte) []byte { copy(b[validityAt:], []byte{0, 0, 0, 0}); return b }), ErrBadLocalRelayOffer},
		{"candCount 0", with(func(b []byte) []byte { b[candCountAt] = 0; return b[:candCountAt+1] }), ErrBadLocalRelayOffer},
		{"candCount 5", with(func(b []byte) []byte { b[candCountAt] = 5; return b }), ErrBadLocalRelayOffer},
		{"candCount overruns", with(func(b []byte) []byte { b[candCountAt] = 3; return b }), ErrBadLocalRelayOffer},
		{"URL length 0", with(func(b []byte) []byte { b[url1LenAt], b[url1LenAt+1] = 0, 0; return b }), ErrBadLocalRelayOffer},
		{"URL length oversize", with(func(b []byte) []byte { b[url1LenAt], b[url1LenAt+1] = 0x01, 0x01; return b }), ErrBadLocalRelayOffer},
		{"URL overruns", with(func(b []byte) []byte { return b[:url1LenAt+10] }), ErrBadLocalRelayOffer},
		{"bad URL http", replaceURL1("http://p-7k2m9x.pop.gawk.ioio.fi:4433"), ErrBadLocalRelayOffer},
		{"bad URL query", replaceURL1("https://p-7k2m9x.pop.gawk.ioio.fi:4433?q"), ErrBadLocalRelayOffer},
		{"bad URL userinfo", replaceURL1("https://u@p-7k2m9x.pop.gawk.ioio.fi:4433"), ErrBadLocalRelayOffer},
		{"hashCount 3", with(func(b []byte) []byte { b[hash2At] = 3; return append(b, make([]byte, 32)...) }), ErrBadLocalRelayOffer},
		{"hash overruns", with(func(b []byte) []byte { return b[:len(b)-1] }), ErrBadLocalRelayOffer},
		{"hashCount missing", with(func(b []byte) []byte { return b[:hash2At] }), ErrBadLocalRelayOffer},
		{"validity truncated", with(func(b []byte) []byte { return b[:validityAt+2] }), ErrBadLocalRelayOffer},
	}
	for _, tc := range cases {
		if _, err := ParseLocalRelayOffer(tc.msg); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// The offer can name four candidates, each with two hashes; the largest
// message still round-trips (it rides a uni stream, so MaxDatagramSize does
// not bound it).
func TestLocalRelayOfferMaximal(t *testing.T) {
	var h [32]byte
	o := LocalRelayOffer{
		Handle:   strings.Repeat("h", MaxLocalRelayHandleLen),
		Name:     strings.Repeat("n", MaxRelayIdentityNameLen),
		Validity: 0xffffffff,
	}
	for i := 0; i < MaxLocalRelayCandidates; i++ {
		o.Candidates = append(o.Candidates, LocalRelayCandidate{
			URL:    "https://a.example/" + strings.Repeat("p", MaxLocalRelayURLLen-len("https://a.example/")),
			Hashes: [][32]byte{h, h},
		})
	}
	msg, err := AppendLocalRelayOffer(nil, o)
	if err != nil {
		t.Fatalf("AppendLocalRelayOffer: %v", err)
	}
	got, err := ParseLocalRelayOffer(msg)
	if err != nil {
		t.Fatalf("ParseLocalRelayOffer: %v", err)
	}
	if !reflect.DeepEqual(got, o) {
		t.Fatal("maximal offer did not round-trip")
	}
}

func TestLocalRelayFuzzNeverPanics(t *testing.T) {
	seeds := [][]byte{
		mustHex(t, goldenLocalRelayInfoLinkedHex),
		mustHex(t, goldenLocalRelayInfoStandaloneHex),
		mustHex(t, goldenLocalRelayOfferHex),
	}
	for _, seed := range seeds {
		for n := 0; n <= len(seed); n++ {
			_, _ = ParseLocalRelayInfo(seed[:n])
			_, _ = ParseLocalRelayOffer(seed[:n])
		}
		for i := range seed {
			for _, v := range []byte{0x00, 0x01, 0x7f, 0x80, 0xff} {
				b := append([]byte(nil), seed...)
				b[i] = v
				_, _ = ParseLocalRelayInfo(b)
				_, _ = ParseLocalRelayOffer(b)
			}
		}
	}
}

func TestLocalRelayConstantsArePinned(t *testing.T) {
	if TypeLocalRelayInfo != 0x18 || TypeLocalRelayOffer != 0x19 {
		t.Fatalf("types = 0x%02x/0x%02x; docs/76 §9 and the mirrors pin 0x18/0x19", TypeLocalRelayInfo, TypeLocalRelayOffer)
	}
	if LocalRelayLinked != 0x01 || LocalRelayOnWall != 0x02 {
		t.Fatal("LocalRelayInfo state bits drifted")
	}
	if MaxLocalRelayOriginLen != 128 || MaxLocalRelayHandleLen != 64 || MaxLocalRelayURLLen != 256 ||
		MaxLocalRelayCandidates != 4 || MaxLocalRelayHashes != 2 || LocalRelayInfoMinSize != 7 {
		t.Fatal("LocalRelay size constants drifted")
	}
	if CloseCodePopUplinkFull != 4012 || CloseCodePopNoDemand != 4013 {
		t.Fatalf("close codes = %d/%d; docs/76 §9 pins 4012/4013", CloseCodePopUplinkFull, CloseCodePopNoDemand)
	}
}
