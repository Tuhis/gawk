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

// TestStripeStateGoldenVectors pins the exact bytes. The TS and Rust mirrors
// assert the same hex.
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

// TestRelayCapabilitiesUplinkCarriersGoldenVector pins R73's bit (docs/75
// G7): the same 5-byte shape with bit 2 set. The TS and Rust mirrors assert
// the identical hex.
func TestRelayCapabilitiesUplinkCarriersGoldenVector(t *testing.T) {
	if CapUplinkCarriers != 1<<2 {
		t.Fatalf("CapUplinkCarriers = %#x, want 1<<2", CapUplinkCarriers)
	}
	dgram, err := AppendRelayCapabilities(nil, RelayCapabilities{
		Flags:       CapParityChunks | CapStripedDelivery | CapUplinkCarriers,
		ParityLevel: 2,
	})
	if err != nil {
		t.Fatalf("AppendRelayCapabilities: %v", err)
	}
	const want = "010f000702"
	if got := hex.EncodeToString(dgram); got != want {
		t.Fatalf("golden vector mismatch:\n got %s\nwant %s", got, want)
	}
	c, err := ParseRelayCapabilities(dgram)
	if err != nil {
		t.Fatalf("ParseRelayCapabilities: %v", err)
	}
	if c.Flags&CapUplinkCarriers == 0 {
		t.Fatalf("flags = %#04x, want the uplink-carriers bit", c.Flags)
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

// TestStripeGOPResidueGoldenVectors pins R87's GOP assignment (docs/91 D2).
// Pods of different versions must agree on it, because a leg on one pod and
// its primary on another split one viewer's GOPs between them by this
// function alone. The fmix32 column is MurmurHash3's finalizer as published
// (fmix32(1) = 0x514e28b7), computed independently of this code.
func TestStripeGOPResidueGoldenVectors(t *testing.T) {
	for _, tc := range []struct {
		frameID    uint32
		fmix       uint32
		n2, n3, n4 int
	}{
		{0x00000000, 0x00000000, 0, 0, 0},
		{0x00000001, 0x514e28b7, 1, 1, 3},
		{0x00000002, 0x30f4c306, 0, 1, 2},
		{0x0000001e, 0xe93e3fcd, 1, 2, 1},
		{0x0000003c, 0x23887f93, 1, 1, 3},
		{0x01020304, 0x924bf7ad, 1, 2, 1},
		{0x075bcd15, 0xba60d89a, 0, 1, 2},
		{0xffffffff, 0x81f16f39, 1, 1, 1},
	} {
		if got := fmix32(tc.frameID); got != tc.fmix {
			t.Errorf("fmix32(%#08x) = %#08x, want %#08x", tc.frameID, got, tc.fmix)
		}
		for n, want := range map[int]int{2: tc.n2, 3: tc.n3, 4: tc.n4} {
			if got := StripeGOPResidue(tc.frameID, n); got != want {
				t.Errorf("StripeGOPResidue(%#08x, %d) = %d, want %d", tc.frameID, n, got, want)
			}
		}
		for _, n := range []int{-1, 0, 1} {
			if got := StripeGOPResidue(tc.frameID, n); got != 0 {
				t.Errorf("StripeGOPResidue(%#08x, %d) = %d, want 0 (unstriped)", tc.frameID, n, got)
			}
		}
	}
}

// TestStripeGOPResidueSpreadsFixedLengthGOPs is why the residue is hashed
// (docs/91 D2): with a fixed 30-frame GOP, frameID mod 4 only ever lands on
// 0 and 2. The hashed residue must use every connection, close to evenly.
func TestStripeGOPResidueSpreadsFixedLengthGOPs(t *testing.T) {
	const gops = 6000
	for n := 2; n <= MaxStripeLegs; n++ {
		counts := make([]int, n)
		for k := range gops {
			counts[StripeGOPResidue(uint32(1+30*k), n)]++
		}
		for j, c := range counts {
			if want := gops / n; c < want*9/10 || c > want*11/10 {
				t.Errorf("n=%d: residue %d got %d of %d GOPs, want %d ±10%%", n, j, c, gops, want)
			}
		}
	}
}

// TestRelayCapabilitiesStripedDVRGoldenVector pins R87's bit (docs/91 D8):
// the same 5-byte shape with bit 4 set. The TS and Rust mirrors assert the
// identical hex.
func TestRelayCapabilitiesStripedDVRGoldenVector(t *testing.T) {
	if CapStripedDVR != 1<<4 {
		t.Fatalf("CapStripedDVR = %#x, the mirrors pin 1<<4", CapStripedDVR)
	}
	dgram, err := AppendRelayCapabilities(nil, RelayCapabilities{
		Flags:       CapParityChunks | CapStripedDelivery | CapStripedDVR,
		ParityLevel: 2,
	})
	if err != nil {
		t.Fatalf("AppendRelayCapabilities: %v", err)
	}
	const want = "010f001302"
	if got := hex.EncodeToString(dgram); got != want {
		t.Fatalf("golden vector mismatch:\n got %s\nwant %s", got, want)
	}
	c, err := ParseRelayCapabilities(dgram)
	if err != nil {
		t.Fatalf("ParseRelayCapabilities: %v", err)
	}
	if c.Flags&CapStripedDVR == 0 || c.Flags&CapStripedDelivery == 0 {
		t.Fatalf("flags = %#04x, want the striped and striped-DVR bits", c.Flags)
	}
}

// TestCloseCodeStripeLegDrainedIsPinned (R87, docs/91 D11): the drained-leg
// close follows 4019 and is pinned in the TS and Rust mirrors.
func TestCloseCodeStripeLegDrainedIsPinned(t *testing.T) {
	if CloseCodeStripeLegDrained != 4020 {
		t.Fatalf("CloseCodeStripeLegDrained = %d, the mirrors pin 4020", CloseCodeStripeLegDrained)
	}
	if CloseCodeStripeLegDrained != CloseCodeRelayFull+1 {
		t.Fatal("4020 must follow 4019")
	}
}
