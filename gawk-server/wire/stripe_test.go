package wire

import (
	"encoding/hex"
	"math/rand"
	"testing"
)

func TestStripeStateRoundTrip(t *testing.T) {
	for _, s := range []StripeState{
		{Striped: true, StripeN: 1},
		{Striped: true, StripeN: 3},
		{Striped: true, StripeN: MaxStripeLegs},
		{Striped: false, StripeN: 0},
	} {
		dgram, err := AppendStripeState(nil, s)
		if err != nil {
			t.Fatalf("AppendStripeState(%+v): %v", s, err)
		}
		if len(dgram) != StripeStateSize {
			t.Fatalf("len = %d, want %d", len(dgram), StripeStateSize)
		}
		got, err := ParseStripeState(dgram)
		if err != nil {
			t.Fatalf("ParseStripeState(%+v): %v", s, err)
		}
		if got != s {
			t.Fatalf("got %+v, want %+v", got, s)
		}
	}
}

// TestStripeStateGoldenVectors pins the exact bytes. The TS mirror and
// gawk-broadcast's wirecheck assert the same hex.
func TestStripeStateGoldenVectors(t *testing.T) {
	for name, tc := range map[string]struct {
		s    StripeState
		want string
	}{
		"striped n=3": {StripeState{Striped: true, StripeN: 3}, "0110010300"},
		"unstriped":   {StripeState{Striped: false, StripeN: 0}, "0110000000"},
	} {
		dgram, err := AppendStripeState(nil, tc.s)
		if err != nil {
			t.Fatalf("%s: AppendStripeState: %v", name, err)
		}
		if got := hex.EncodeToString(dgram); got != tc.want {
			t.Fatalf("%s: golden vector mismatch:\n got %s\nwant %s", name, got, tc.want)
		}
	}
}

func TestStripeStateRejectsMalformed(t *testing.T) {
	good, err := AppendStripeState(nil, StripeState{Striped: true, StripeN: 2})
	if err != nil {
		t.Fatalf("AppendStripeState: %v", err)
	}
	for name, bad := range map[string][]byte{
		"short":               good[:StripeStateSize-1],
		"long":                append(clone(good), 0),
		"bad version":         {0x02, TypeStripeState, 0x01, 2, 0},
		"bad type":            {Version, TypeVideoChunk, 0x01, 2, 0},
		"unknown flag bit":    {Version, TypeStripeState, 0x03, 2, 0},
		"striped zero n":      {Version, TypeStripeState, 0x01, 0, 0},
		"striped n over max":  {Version, TypeStripeState, 0x01, MaxStripeLegs + 1, 0},
		"unstriped nonzero n": {Version, TypeStripeState, 0x00, 1, 0},
	} {
		if _, err := ParseStripeState(bad); err == nil {
			t.Fatalf("%s: accepted, want error", name)
		}
	}
}

func TestAppendStripeStateRejectsBadShapes(t *testing.T) {
	for name, s := range map[string]StripeState{
		"striped zero n":      {Striped: true, StripeN: 0},
		"striped n over max":  {Striped: true, StripeN: MaxStripeLegs + 1},
		"unstriped nonzero n": {Striped: false, StripeN: 2},
	} {
		if _, err := AppendStripeState(nil, s); err == nil {
			t.Fatalf("%s: accepted, want error", name)
		}
	}
}

// TestRelayCapabilitiesAcceptsStripedFlag is the version-skew pin (docs/35
// ST2): the capabilities message with the R30 bit set must parse in the
// old 5-byte shape, because the R29 broadcaster parser rejects any other
// size. If this test ever needs the message to grow, the design's "new bits,
// never new bytes" rule (docs/35 §5.3) is being violated.
func TestRelayCapabilitiesAcceptsStripedFlag(t *testing.T) {
	dgram, err := AppendRelayCapabilities(nil, RelayCapabilities{
		Flags:       CapParityChunks | CapStripedDelivery,
		ParityLevel: 2,
	})
	if err != nil {
		t.Fatalf("AppendRelayCapabilities: %v", err)
	}
	if len(dgram) != RelayCapabilitiesSize {
		t.Fatalf("len = %d, want %d — the message must not grow", len(dgram), RelayCapabilitiesSize)
	}
	const want = "010f000302"
	if got := hex.EncodeToString(dgram); got != want {
		t.Fatalf("golden vector mismatch:\n got %s\nwant %s", got, want)
	}
	c, err := ParseRelayCapabilities(dgram)
	if err != nil {
		t.Fatalf("ParseRelayCapabilities: %v", err)
	}
	if c.Flags&CapStripedDelivery == 0 || c.Flags&CapParityChunks == 0 {
		t.Fatalf("flags = %#04x, want both bits set", c.Flags)
	}
}

func TestStripeOrdinal(t *testing.T) {
	// Data chunks keep their index; parity follows the data, so on every leg
	// of any stripe the parity ordinals are the highest the frame emits —
	// the tail-of-burst position finding 4 measured as never lost.
	if got := StripeOrdinal(7, 20, -1); got != 7 {
		t.Fatalf("data ordinal = %d, want 7", got)
	}
	if got := StripeOrdinal(0, 20, 0); got != 20 {
		t.Fatalf("P ordinal = %d, want 20", got)
	}
	if got := StripeOrdinal(0, 20, 1); got != 21 {
		t.Fatalf("Q ordinal = %d, want 21", got)
	}
}

func TestStripeStateFuzzNeverPanics(t *testing.T) {
	rnd := rand.New(rand.NewSource(11))
	buf := make([]byte, StripeStateSize)
	for i := 0; i < 10_000; i++ {
		rnd.Read(buf)
		_, _ = ParseStripeState(buf)
		_, _ = ParseStripeState(buf[:rnd.Intn(len(buf)+1)])
	}
}
