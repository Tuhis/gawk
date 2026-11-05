package wire

import (
	"bytes"
	"errors"
	"testing"
)

// --- R57 SessionClosing (0x17) (docs/59 CN1) ---

// Golden vector, computed by hand from the layout in closing.go. Restated
// byte-identically in both mirrors (wire.ts, crates/wire).
//
//	01           version
//	17           type = SessionClosing
//	00 00 0f a4  code = 4004 (CloseCodePublisherSuperseded), big-endian
const goldenSessionClosingHex = "011700000fa4"

func TestGoldenSessionClosing(t *testing.T) {
	want := mustHex(t, goldenSessionClosingHex)
	msg, err := AppendSessionClosing(nil, CloseCodePublisherSuperseded)
	if err != nil {
		t.Fatalf("AppendSessionClosing: %v", err)
	}
	if !bytes.Equal(msg, want) {
		t.Errorf("AppendSessionClosing produced %x, want %x", msg, want)
	}
	if len(msg) != SessionClosingSize {
		t.Errorf("len = %d, want SessionClosingSize %d", len(msg), SessionClosingSize)
	}
	code, err := ParseSessionClosing(want)
	if err != nil {
		t.Fatalf("ParseSessionClosing: %v", err)
	}
	if code != CloseCodePublisherSuperseded {
		t.Errorf("code = %d, want %d", code, CloseCodePublisherSuperseded)
	}
}

func TestSessionClosingRoundTripsEveryNoticedCode(t *testing.T) {
	for _, c := range []uint32{CloseCodeBroadcastEnded, CloseCodePublisherSuperseded, CloseCodeTerminatedByOperator, CloseCodePopUplinkFull, CloseCodeBroadcasterStopped} {
		msg, err := AppendSessionClosing(nil, c)
		if err != nil {
			t.Fatalf("AppendSessionClosing(%d): %v", c, err)
		}
		got, err := ParseSessionClosing(msg)
		if err != nil || got != c {
			t.Fatalf("round trip %d = (%d, %v)", c, got, err)
		}
	}
}

// R74 (docs/76 item 28): 4012 PopUplinkFull is noticed in-band too.
//
//	01           version
//	17           type = SessionClosing
//	00 00 0f ac  code = 4012 (CloseCodePopUplinkFull), big-endian
const goldenSessionClosingPopUplinkFullHex = "011700000fac"

func TestGoldenSessionClosingPopUplinkFull(t *testing.T) {
	want := mustHex(t, goldenSessionClosingPopUplinkFullHex)
	msg, err := AppendSessionClosing(nil, CloseCodePopUplinkFull)
	if err != nil {
		t.Fatalf("AppendSessionClosing: %v", err)
	}
	if !bytes.Equal(msg, want) {
		t.Errorf("AppendSessionClosing produced %x, want %x", msg, want)
	}
	code, err := ParseSessionClosing(want)
	if err != nil || code != CloseCodePopUplinkFull {
		t.Fatalf("ParseSessionClosing = (%d, %v), want 4012", code, err)
	}
}

// R78 (docs/79 §5): 4014 BroadcasterStopped is noticed in-band to viewers.
//
//	01           version
//	17           type = SessionClosing
//	00 00 0f ae  code = 4014 (CloseCodeBroadcasterStopped), big-endian
const goldenSessionClosingBroadcasterStoppedHex = "011700000fae"

func TestGoldenSessionClosingBroadcasterStopped(t *testing.T) {
	if CloseCodeBroadcasterStopped != 4014 {
		t.Fatalf("CloseCodeBroadcasterStopped = %d; docs/79 D1 pins 4014", CloseCodeBroadcasterStopped)
	}
	want := mustHex(t, goldenSessionClosingBroadcasterStoppedHex)
	msg, err := AppendSessionClosing(nil, CloseCodeBroadcasterStopped)
	if err != nil {
		t.Fatalf("AppendSessionClosing: %v", err)
	}
	if !bytes.Equal(msg, want) {
		t.Errorf("AppendSessionClosing produced %x, want %x", msg, want)
	}
}

// R33 (docs/83 D3): 4017 PublishMinted is noticed in-band before a mint-only
// publish session closes.
//
//	01           version
//	17           type = SessionClosing
//	00 00 0f b1  code = 4017 (CloseCodePublishMinted), big-endian
const goldenSessionClosingPublishMintedHex = "011700000fb1"

func TestGoldenSessionClosingPublishMinted(t *testing.T) {
	if CloseCodePublishMinted != 4017 {
		t.Fatalf("CloseCodePublishMinted = %d; docs/83 D3 pins 4017", CloseCodePublishMinted)
	}
	if CloseCodePublishMinted != CloseCodeVoiceUnavailable+1 {
		t.Fatal("4017 must follow R79's 4016")
	}
	want := mustHex(t, goldenSessionClosingPublishMintedHex)
	msg, err := AppendSessionClosing(nil, CloseCodePublishMinted)
	if err != nil {
		t.Fatalf("AppendSessionClosing: %v", err)
	}
	if !bytes.Equal(msg, want) {
		t.Errorf("AppendSessionClosing produced %x, want %x", msg, want)
	}
	code, err := ParseSessionClosing(want)
	if err != nil || code != CloseCodePublishMinted {
		t.Fatalf("ParseSessionClosing = (%d, %v), want 4017", code, err)
	}
}

// 4018 BroadcastFull and 4019 RelayFull are noticed in-band before the
// relay closes a viewer it refused for capacity (BUGS.md fix 2026-11-06:
// a refused CONNECT's 429 never reached the browser).
//
//	01           version
//	17           type = SessionClosing
//	00 00 0f b2  code = 4018 (CloseCodeBroadcastFull), big-endian
//	00 00 0f b3  code = 4019 (CloseCodeRelayFull), big-endian
const (
	goldenSessionClosingBroadcastFullHex = "011700000fb2"
	goldenSessionClosingRelayFullHex     = "011700000fb3"
)

func TestGoldenSessionClosingCapacityRefusals(t *testing.T) {
	if CloseCodeBroadcastFull != 4018 || CloseCodeRelayFull != 4019 {
		t.Fatalf("CloseCodeBroadcastFull, CloseCodeRelayFull = %d, %d; want 4018, 4019", CloseCodeBroadcastFull, CloseCodeRelayFull)
	}
	if CloseCodeBroadcastFull != CloseCodePublishMinted+1 {
		t.Fatal("4018 must follow R33's 4017")
	}
	for _, tc := range []struct {
		code uint32
		hex  string
	}{
		{CloseCodeBroadcastFull, goldenSessionClosingBroadcastFullHex},
		{CloseCodeRelayFull, goldenSessionClosingRelayFullHex},
	} {
		want := mustHex(t, tc.hex)
		msg, err := AppendSessionClosing(nil, tc.code)
		if err != nil {
			t.Fatalf("AppendSessionClosing(%d): %v", tc.code, err)
		}
		if !bytes.Equal(msg, want) {
			t.Errorf("AppendSessionClosing(%d) produced %x, want %x", tc.code, msg, want)
		}
		code, err := ParseSessionClosing(want)
		if err != nil || code != tc.code {
			t.Fatalf("ParseSessionClosing(%s) = (%d, %v), want %d", tc.hex, code, err, tc.code)
		}
	}
}

func TestSessionClosingRejects(t *testing.T) {
	if _, err := AppendSessionClosing(nil, 0); !errors.Is(err, ErrBadSessionClosing) {
		t.Errorf("AppendSessionClosing(0) err = %v, want ErrBadSessionClosing", err)
	}
	if _, err := AppendSessionClosing(nil, 3999); !errors.Is(err, ErrBadSessionClosing) {
		t.Errorf("AppendSessionClosing(3999) err = %v, want ErrBadSessionClosing", err)
	}
	cases := []struct {
		name string
		msg  []byte
		want error
	}{
		{"empty", nil, ErrShortDatagram},
		{"5 bytes", []byte{0x01, 0x17, 0x00, 0x00, 0x0f}, ErrShortDatagram},
		{"7 bytes", []byte{0x01, 0x17, 0x00, 0x00, 0x0f, 0xa4, 0x00}, ErrBadSessionClosing},
		{"bad version", []byte{0x02, 0x17, 0x00, 0x00, 0x0f, 0xa4}, ErrBadVersion},
		{"bad type", []byte{0x01, 0x09, 0x00, 0x00, 0x0f, 0xa4}, ErrBadType},
		{"code below the gawk range", []byte{0x01, 0x17, 0x00, 0x00, 0x0f, 0x9f}, ErrBadSessionClosing},
		{"code above the gawk range", []byte{0x01, 0x17, 0x00, 0x00, 0x13, 0x88}, ErrBadSessionClosing},
	}
	for _, tc := range cases {
		if _, err := ParseSessionClosing(tc.msg); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestSessionClosingTypeIsPinned(t *testing.T) {
	if TypeSessionClosing != 0x17 || SessionClosingSize != 6 {
		t.Fatalf("TypeSessionClosing = 0x%02x, size %d; the mirrors pin 0x17 and 6", TypeSessionClosing, SessionClosingSize)
	}
}
