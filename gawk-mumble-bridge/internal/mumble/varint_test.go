package mumble

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math"
	"testing"
)

// Golden bytes from the Mumble protocol documentation's varint table and
// the reference PacketDataStream encoder.
var varintGolden = []struct {
	v   int64
	hex string
}{
	{0, "00"},
	{1, "01"},
	{0x7F, "7f"},
	{0x80, "8080"},
	{0x3FFF, "bfff"},
	{0x4000, "c04000"},
	{0x1FFFFF, "dfffff"},
	{0x200000, "e0200000"},
	{0xFFFFFFF, "efffffff"},
	{0x10000000, "f010000000"},
	{0xFFFFFFFF, "f0ffffffff"},
	{0x100000000, "f40000000100000000"},
	{math.MaxInt64, "f47fffffffffffffff"},
	{-1, "fc"},
	{-2, "fd"},
	{-4, "ff"},
	{-5, "f804"},
	{-200, "f880c7"},
	{-0x100000000, "f8f0ffffffff"},
	// A negative whose complement does not fit 32 bits goes out as the
	// 64-bit two's complement.
	{-0x100000001, "f4fffffffeffffffff"},
	{math.MinInt64, "f48000000000000000"},
}

func TestVarintGolden(t *testing.T) {
	for _, g := range varintGolden {
		got := AppendVarint(nil, g.v)
		if hex.EncodeToString(got) != g.hex {
			t.Errorf("AppendVarint(%d) = %x, want %s", g.v, got, g.hex)
		}
		want, _ := hex.DecodeString(g.hex)
		v, n, err := ReadVarint(append(want, 0xAA)) // trailing byte must be left
		if err != nil || v != g.v || n != len(want) {
			t.Errorf("ReadVarint(%s) = %d, %d, %v; want %d, %d", g.hex, v, n, err, g.v, len(want))
		}
	}
}

// The decoder accepts every prefix form, including ones the encoder never
// emits: a non-minimal positive, a 32-bit form for a small value, the
// recursive negative of a long form.
func TestVarintDecodesEveryForm(t *testing.T) {
	cases := []struct {
		hex string
		v   int64
	}{
		{"8001", 1},       // 14-bit form of 1
		{"c00001", 1},     // 21-bit
		{"e0000001", 1},   // 28-bit
		{"f000000001", 1}, // 32-bit
		{"f40000000000000001", 1},
		{"f8f000000004", -5},       // negative of a 32-bit 4
		{"f8fc", 0},                // ~(-1) = 0
		{"fe", -3},                 // inline -3
		{"f7ffffffffffffffff", -1}, // 0xF4 with low bits set: still 64-bit
	}
	for _, c := range cases {
		b, _ := hex.DecodeString(c.hex)
		v, n, err := ReadVarint(b)
		if err != nil || v != c.v || n != len(b) {
			t.Errorf("ReadVarint(%s) = %d, %d, %v; want %d", c.hex, v, n, err, c.v)
		}
	}
}

func TestVarintRoundTrip(t *testing.T) {
	vals := []int64{0, 3, 127, 128, 255, 256, 16383, 16384, 1 << 20, 1<<21 - 1, 1 << 21, 1<<28 - 1, 1 << 28,
		1<<32 - 1, 1 << 32, 1 << 40, math.MaxInt64, -1, -3, -4, -5, -127, -128, -1 << 20, -1<<32 + 1, -1 << 32, -1<<32 - 1, math.MinInt64}
	for _, v := range vals {
		b := AppendVarint(nil, v)
		got, n, err := ReadVarint(b)
		if err != nil || got != v || n != len(b) {
			t.Errorf("round trip %d: %x → %d, %d, %v", v, b, got, n, err)
		}
	}
}

func TestVarintTruncated(t *testing.T) {
	for _, h := range []string{"", "80", "c000", "e00000", "f0000000", "f400000000000000", "f8", "f880"} {
		b, _ := hex.DecodeString(h)
		if _, _, err := ReadVarint(b); !errors.Is(err, ErrVarint) {
			t.Errorf("ReadVarint(%q) err = %v, want ErrVarint", h, err)
		}
	}
}

func TestAudioPacketGolden(t *testing.T) {
	got, err := AppendClientAudio(nil, 0, 5, []byte{0xAA, 0xBB}, true)
	if err != nil {
		t.Fatal(err)
	}
	// header Opus<<5|0, seq 5, opus header 2|0x2000 (14-bit form), payload.
	if want := "8005a002aabb"; hex.EncodeToString(got) != want {
		t.Fatalf("client packet %x, want %s", got, want)
	}
	p, err := ParseClientAudio(got)
	if err != nil || p.Seq != 5 || !p.End || !bytes.Equal(p.Opus, []byte{0xAA, 0xBB}) || p.Target != 0 {
		t.Fatalf("parse client %+v %v", p, err)
	}

	srv, err := AppendServerAudio(nil, AudioPacket{Session: 3, Seq: 300, Target: 1, Opus: []byte{0x01}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "8103812c0101"; hex.EncodeToString(srv) != want {
		t.Fatalf("server packet %x, want %s", srv, want)
	}
	// A positional-audio tail after the payload is ignored.
	p, err = ParseServerAudio(append(srv, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12))
	if err != nil || p.Session != 3 || p.Seq != 300 || p.Target != 1 || p.End || !bytes.Equal(p.Opus, []byte{1}) {
		t.Fatalf("parse server %+v %v", p, err)
	}
	// An end-of-transmission terminator may be empty.
	term, _ := AppendServerAudio(nil, AudioPacket{Session: 9, Seq: 7, End: true})
	if p, err := ParseServerAudio(term); err != nil || !p.End || len(p.Opus) != 0 {
		t.Fatalf("terminator %+v %v", p, err)
	}
}

func TestAudioPacketErrors(t *testing.T) {
	if _, err := AppendClientAudio(nil, 32, 0, []byte{1}, false); !errors.Is(err, ErrAudioPacket) {
		t.Errorf("target 32: %v", err)
	}
	if _, err := AppendClientAudio(nil, 0, 0, make([]byte, MaxOpusLen+1), false); !errors.Is(err, ErrAudioPacket) {
		t.Errorf("oversize: %v", err)
	}
	if _, err := AppendServerAudio(nil, AudioPacket{Target: 40}); !errors.Is(err, ErrAudioPacket) {
		t.Errorf("server target: %v", err)
	}
	if _, err := AppendServerAudio(nil, AudioPacket{Opus: make([]byte, MaxOpusLen+1)}); !errors.Is(err, ErrAudioPacket) {
		t.Errorf("server oversize: %v", err)
	}
	bad := map[string]string{
		"empty":                "",
		"truncated session":    "80",
		"truncated sequence":   "8003",
		"truncated header":     "800301",
		"payload past the end": "8003010502",
		"empty without end":    "80030100",
		"negative header":      "800301fc",
		"negative session":     "80fc0101aa",
	}
	for name, h := range bad {
		b, _ := hex.DecodeString(h)
		if _, err := ParseServerAudio(b); !errors.Is(err, ErrAudioPacket) {
			t.Errorf("%s: %v, want ErrAudioPacket", name, err)
		}
	}
	// Other codecs and the legacy ping are well-formed but not ours.
	for _, typ := range []byte{audioTypeCELTAlpha, audioTypePing, audioTypeSpeex, audioTypeCELTBeta, 5} {
		if _, err := ParseServerAudio([]byte{typ << 5, 1, 1, 1, 0}); !errors.Is(err, ErrNotOpus) {
			t.Errorf("type %d: %v, want ErrNotOpus", typ, err)
		}
	}
}
