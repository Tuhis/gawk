package wire

import (
	"bytes"
	"errors"
	"testing"
)

// --- R56 SessionClosing (0x17) (docs/58 CN1) ---

// Golden vector, computed by hand from the layout in closing.go. Restated
// byte-identically in all three mirrors (wire.ts, wirecheck, crates/wire).
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
	for _, c := range []uint32{CloseCodeBroadcastEnded, CloseCodePublisherSuperseded, CloseCodeTerminatedByOperator} {
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
		t.Fatalf("TypeSessionClosing = 0x%02x, size %d; the four mirrors pin 0x17 and 6", TypeSessionClosing, SessionClosingSize)
	}
}
