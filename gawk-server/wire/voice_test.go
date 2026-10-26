package wire

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Golden vectors for R79's voice messages (docs/80 D20), computed by hand
// from the layout comments in voice.go and room.go. Restated
// byte-identically in the TS mirror (gawk-app/src/transport/wire.test.ts)
// and the Rust crate (gawk-broadcast-desktop/crates/wire/tests/golden.rs).
// Do not regenerate them from code; if they change, the wire format changed.
const (
	// VoiceHello: protocol 1, flags listen-only, ticket 01..05.
	//   01 1a  version, type
	//   01     protocol
	//   04     flags = listen-only
	//   0005   ticketLen
	//   0102030405
	goldenVoiceHelloHex = "011a" + "01" + "04" + "0005" + "0102030405"

	// VoiceFrame: not the end, target 0, speaker 7, seq 0x12, payload fcfffe.
	goldenVoiceFrameHex = "011b" + "00" + "00" + "00000007" + "00000012" + "fcfffe"

	// VoiceFrame terminator: end of transmission, target 2 (direct
	// whisper), speaker 0x01020304, seq 0xffffffff, no payload.
	goldenVoiceFrameEndHex = "011b" + "01" + "02" + "01020304" + "ffffffff"

	// VoiceState: connected|self-mute, session 42, name "tuhis [web]".
	goldenVoiceStateHex = "011c" + "28" + "0000002a" + "0b" + "7475686973205b7765625d"

	// VoiceControl: self-mute|self-deaf.
	goldenVoiceControlHex = "011d03"

	// RoomCommand VoiceJoin: no payload.
	goldenRoomCommandVoiceJoinHex = "011650"

	// RoomEvent VoiceTicket at seq 7: url "https://v.example/voice" (23
	// bytes), ticket aabb.
	goldenRoomEventVoiceTicketHex = "0115" + "00000007" + "50" +
		"0017" + "68747470733a2f2f762e6578616d706c652f766f696365" +
		"0002" + "aabb"

	// RoomEvent VoiceRosterUpdated at seq 7: available, two entries —
	// session 3, native Mumble, self-muted, "bob"; session 9, gawk
	// participant 2, no flags, "tuhis [web]".
	goldenRoomEventVoiceRosterHex = "0115" + "00000007" + "51" + "01" + "0002" +
		"00000003" + "00" + "0000" + "01" + "03626f62" +
		"00000009" + "01" + "0002" + "00" + "0b7475686973205b7765625d"

	// RoomEvent VoiceRosterUpdated at seq 7: unavailable, empty.
	goldenRoomEventVoiceRosterEmptyHex = "0115" + "00000007" + "51" + "00" + "0000"
)

var goldenVoiceRoster = []VoiceRosterEntry{
	{Session: 3, Kind: RoomVoiceKindMumble, Flags: RoomVoiceUserSelfMute, Name: "bob"},
	{Session: 9, Kind: RoomVoiceKindGawk, ParticipantID: 2, Name: "tuhis [web]"},
}

func TestVoiceAllocations(t *testing.T) {
	// The allocation map (docs/80 D20): after LocalRelayOffer, in order.
	if TypeVoiceHello != TypeLocalRelayOffer+1 || TypeVoiceFrame != 0x1B || TypeVoiceState != 0x1C || TypeVoiceControl != 0x1D {
		t.Fatalf("voice types 0x%02x..0x%02x", TypeVoiceHello, TypeVoiceControl)
	}
	if CloseCodeVoiceRemoved != 4015 || CloseCodeVoiceUnavailable != 4016 {
		t.Fatalf("voice close codes %d %d", CloseCodeVoiceRemoved, CloseCodeVoiceUnavailable)
	}
	if CloseCodeVoiceRemoved != CloseCodeBroadcasterStopped+1 {
		t.Fatal("4015 must follow 4014")
	}
	if RoomCommandVoiceJoin != 0x50 || RoomEventVoiceTicket != 0x50 || RoomEventVoiceRosterUpdated != 0x51 {
		t.Fatal("voice room kinds must sit in the reserved 0x50-0x5F range")
	}
	if MaxVoicePayload != MaxDatagramSize-VoiceFrameHeaderSize {
		t.Fatal("MaxVoicePayload")
	}
	// The largest roster must fit one room record.
	big := make([]VoiceRosterEntry, MaxVoiceRosterEntries)
	for i := range big {
		big[i] = VoiceRosterEntry{Session: uint32(i), Name: strings.Repeat("x", MaxVoiceNameLen)}
	}
	if _, err := AppendRoomEvent(nil, RoomEvent{Kind: RoomEventVoiceRosterUpdated, VoiceRoster: big}); err != nil {
		t.Fatalf("a full roster does not fit a record: %v", err)
	}
}

func TestGoldenVoiceHello(t *testing.T) {
	want := mustHex(t, goldenVoiceHelloHex)
	h := VoiceHello{Protocol: VoiceProtocolVersion, Flags: VoiceHelloFlagListenOnly, Ticket: []byte{1, 2, 3, 4, 5}}
	got, err := AppendVoiceHello(nil, h)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("AppendVoiceHello = %x, %v; want %x", got, err, want)
	}
	back, err := ParseVoiceHello(want)
	if err != nil || !reflect.DeepEqual(back, h) {
		t.Fatalf("ParseVoiceHello = %+v, %v", back, err)
	}
}

func TestVoiceHelloStrict(t *testing.T) {
	good := mustHex(t, goldenVoiceHelloHex)
	for name, msg := range map[string][]byte{
		"short":         good[:5],
		"bad protocol":  patch(good, 2, 2),
		"reserved flag": patch(good, 3, 0x08),
		"zero ticket":   mustHex(t, "011a01000000"),
		"trailing":      append(append([]byte(nil), good...), 0),
		"overrun":       good[:len(good)-1],
	} {
		if _, err := ParseVoiceHello(msg); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	if _, err := AppendVoiceHello(nil, VoiceHello{Protocol: 1, Ticket: make([]byte, MaxVoiceTicketLen+1)}); !errors.Is(err, ErrBadVoiceHello) {
		t.Errorf("oversize ticket: %v", err)
	}
	if _, err := AppendVoiceHello(nil, VoiceHello{Protocol: 1}); !errors.Is(err, ErrBadVoiceHello) {
		t.Errorf("empty ticket: %v", err)
	}
	if _, err := AppendVoiceHello(nil, VoiceHello{Protocol: 1, Flags: 0x80, Ticket: []byte{1}}); !errors.Is(err, ErrBadVoiceHello) {
		t.Errorf("reserved flag: %v", err)
	}
	if _, err := AppendVoiceHello(nil, VoiceHello{Protocol: 9, Ticket: []byte{1}}); !errors.Is(err, ErrBadVoiceHello) {
		t.Errorf("bad protocol: %v", err)
	}
}

func TestGoldenVoiceFrame(t *testing.T) {
	for _, tc := range []struct {
		name    string
		hex     string
		h       VoiceFrameHeader
		payload []byte
	}{
		{"frame", goldenVoiceFrameHex, VoiceFrameHeader{Speaker: 7, Seq: 0x12}, []byte{0xfc, 0xff, 0xfe}},
		{"end", goldenVoiceFrameEndHex, VoiceFrameHeader{End: true, Target: 2, Speaker: 0x01020304, Seq: 0xffffffff}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := mustHex(t, tc.hex)
			got, err := AppendVoiceFrame(nil, tc.h, tc.payload)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("AppendVoiceFrame = %x, %v; want %x", got, err, want)
			}
			h, p, err := ParseVoiceFrame(want)
			if err != nil || h != tc.h || !bytes.Equal(p, tc.payload) {
				t.Fatalf("ParseVoiceFrame = %+v %x %v", h, p, err)
			}
		})
	}
}

func TestVoiceFrameStrict(t *testing.T) {
	good := mustHex(t, goldenVoiceFrameHex)
	for name, msg := range map[string][]byte{
		"short":          good[:11],
		"wrong type":     patch(good, 1, TypeAudioFrame),
		"reserved flag":  patch(good, 2, 0x02),
		"target":         patch(good, 3, 32),
		"empty, not end": good[:VoiceFrameHeaderSize],
	} {
		if _, _, err := ParseVoiceFrame(msg); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	if _, err := AppendVoiceFrame(nil, VoiceFrameHeader{}, nil); !errors.Is(err, ErrBadVoiceFrame) {
		t.Errorf("empty, not end: %v", err)
	}
	if _, err := AppendVoiceFrame(nil, VoiceFrameHeader{}, make([]byte, MaxVoicePayload+1)); !errors.Is(err, ErrBadVoiceFrame) {
		t.Errorf("oversize: %v", err)
	}
	if _, err := AppendVoiceFrame(nil, VoiceFrameHeader{Target: 32}, []byte{1}); !errors.Is(err, ErrBadVoiceFrame) {
		t.Errorf("target: %v", err)
	}
	full, err := AppendVoiceFrame(nil, VoiceFrameHeader{}, make([]byte, MaxVoicePayload))
	if err != nil || len(full) != MaxDatagramSize {
		t.Fatalf("a full frame is %d bytes, %v", len(full), err)
	}
}

func TestGoldenVoiceState(t *testing.T) {
	want := mustHex(t, goldenVoiceStateHex)
	s := VoiceState{Flags: VoiceStateFlagConnected | VoiceStateFlagSelfMute, Session: 42, Name: "tuhis [web]"}
	got, err := AppendVoiceState(nil, s)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("AppendVoiceState = %x, %v; want %x", got, err, want)
	}
	back, err := ParseVoiceState(want)
	if err != nil || back != s {
		t.Fatalf("ParseVoiceState = %+v, %v", back, err)
	}
	for name, msg := range map[string][]byte{
		"reserved flag": patch(want, 2, 0x40),
		"trailing":      append(append([]byte(nil), want...), 0),
		"truncated":     want[:len(want)-1],
		"bad utf8":      append(mustHex(t, "011c000000000201"), 0xff),
	} {
		if _, err := ParseVoiceState(msg); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	if _, err := AppendVoiceState(nil, VoiceState{Flags: 0x80}); !errors.Is(err, ErrBadVoiceState) {
		t.Errorf("reserved flag: %v", err)
	}
	if _, err := AppendVoiceState(nil, VoiceState{Name: strings.Repeat("x", MaxVoiceNameLen+1)}); !errors.Is(err, ErrBadVoiceState) {
		t.Errorf("long name: %v", err)
	}
}

func TestGoldenVoiceControl(t *testing.T) {
	want := mustHex(t, goldenVoiceControlHex)
	got, err := AppendVoiceControl(nil, VoiceControlFlagSelfMute|VoiceControlFlagSelfDeaf)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("AppendVoiceControl = %x, %v", got, err)
	}
	f, err := ParseVoiceControl(want)
	if err != nil || f != 3 {
		t.Fatalf("ParseVoiceControl = %d, %v", f, err)
	}
	for name, msg := range map[string][]byte{
		"reserved": mustHex(t, "011d04"),
		"trailing": mustHex(t, "011d0100"),
		"short":    mustHex(t, "011d"),
	} {
		if _, err := ParseVoiceControl(msg); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	if _, err := AppendVoiceControl(nil, 0x04); !errors.Is(err, ErrBadVoiceControl) {
		t.Errorf("reserved: %v", err)
	}
}

func TestGoldenRoomCommandVoiceJoin(t *testing.T) {
	want := mustHex(t, goldenRoomCommandVoiceJoinHex)
	got, err := AppendRoomCommand(nil, RoomCommand{Kind: RoomCommandVoiceJoin})
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("AppendRoomCommand = %x, %v", got, err)
	}
	c, err := ParseRoomCommand(want)
	if err != nil || c.Kind != RoomCommandVoiceJoin {
		t.Fatalf("ParseRoomCommand = %+v, %v", c, err)
	}
	if _, err := ParseRoomCommand(mustHex(t, "01165000")); !errors.Is(err, ErrBadRoomCommand) {
		t.Fatalf("trailing byte: %v", err)
	}
	// The rest of the voice range is still unknown, so a relay answers it
	// Unsupported.
	if _, err := ParseRoomCommand(mustHex(t, "011651")); !errors.Is(err, ErrUnknownRoomKind) {
		t.Fatalf("0x51 command: %v", err)
	}
}

func TestGoldenRoomEventVoiceTicket(t *testing.T) {
	want := mustHex(t, goldenRoomEventVoiceTicketHex)
	ev := RoomEvent{Seq: 7, Kind: RoomEventVoiceTicket, VoiceURL: "https://v.example/voice", VoiceTicket: []byte{0xaa, 0xbb}}
	got, err := AppendRoomEvent(nil, ev)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("AppendRoomEvent = %x, %v; want %x", got, err, want)
	}
	back, err := ParseRoomEvent(want)
	if err != nil || !reflect.DeepEqual(back, ev) {
		t.Fatalf("ParseRoomEvent = %+v, %v", back, err)
	}
	for name, msg := range map[string][]byte{
		"empty url":    mustHex(t, "011500000007500000"+"0002aabb"),
		"empty ticket": mustHex(t, "01150000000750"+"000168"+"0000"),
		"overrun":      want[:len(want)-1],
		"trailing":     append(append([]byte(nil), want...), 0),
	} {
		if _, err := ParseRoomEvent(msg); !errors.Is(err, ErrBadRoomEvent) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := AppendRoomEvent(nil, RoomEvent{Kind: RoomEventVoiceTicket, VoiceTicket: []byte{1}}); !errors.Is(err, ErrBadRoomEvent) {
		t.Errorf("append empty url: %v", err)
	}
	if _, err := AppendRoomEvent(nil, RoomEvent{Kind: RoomEventVoiceTicket, VoiceURL: "https://x"}); !errors.Is(err, ErrBadRoomEvent) {
		t.Errorf("append empty ticket: %v", err)
	}
}

func TestGoldenRoomEventVoiceRoster(t *testing.T) {
	for _, tc := range []struct {
		name string
		hex  string
		ev   RoomEvent
	}{
		{"two", goldenRoomEventVoiceRosterHex, RoomEvent{Seq: 7, Kind: RoomEventVoiceRosterUpdated, VoiceFlags: RoomVoiceRosterFlagAvailable, VoiceRoster: goldenVoiceRoster}},
		{"empty", goldenRoomEventVoiceRosterEmptyHex, RoomEvent{Seq: 7, Kind: RoomEventVoiceRosterUpdated}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := mustHex(t, tc.hex)
			got, err := AppendRoomEvent(nil, tc.ev)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("AppendRoomEvent = %x, %v; want %x", got, err, want)
			}
			back, err := ParseRoomEvent(want)
			if err != nil || !reflect.DeepEqual(back, tc.ev) {
				t.Fatalf("ParseRoomEvent = %+v, %v", back, err)
			}
		})
	}
	good := mustHex(t, goldenRoomEventVoiceRosterHex)
	for name, msg := range map[string][]byte{
		"reserved roster flag": patch(good, 7, 0x02),
		"bad kind":             patch(good, 14, 2),
		"reserved user flag":   patch(good, 17, 0x20),
		"overrun":              good[:len(good)-1],
		"trailing":             append(append([]byte(nil), good...), 0),
		"too many":             mustHex(t, "0115000000075101ffff"),
	} {
		if _, err := ParseRoomEvent(msg); !errors.Is(err, ErrBadRoomEvent) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, ev := range map[string]RoomEvent{
		"reserved roster flag": {Kind: RoomEventVoiceRosterUpdated, VoiceFlags: 0x02},
		"bad kind":             {Kind: RoomEventVoiceRosterUpdated, VoiceRoster: []VoiceRosterEntry{{Kind: 2}}},
		"reserved user flag":   {Kind: RoomEventVoiceRosterUpdated, VoiceRoster: []VoiceRosterEntry{{Flags: 0x20}}},
		"long name":            {Kind: RoomEventVoiceRosterUpdated, VoiceRoster: []VoiceRosterEntry{{Name: strings.Repeat("x", MaxVoiceNameLen+1)}}},
		"too many":             {Kind: RoomEventVoiceRosterUpdated, VoiceRoster: make([]VoiceRosterEntry, MaxVoiceRosterEntries+1)},
	} {
		if _, err := AppendRoomEvent(nil, ev); !errors.Is(err, ErrBadRoomEvent) {
			t.Errorf("append %s: %v", name, err)
		}
	}
	// The rest of the voice range stays unknown and skippable.
	e, err := ParseRoomEvent(mustHex(t, "011500000009"+"5f"))
	if !errors.Is(err, ErrUnknownRoomKind) || e.Seq != 9 {
		t.Fatalf("0x5f event: %+v %v", e, err)
	}
}

func TestTruncateVoiceName(t *testing.T) {
	long := strings.Repeat("ä", 40) // 80 bytes
	got := TruncateVoiceName(long)
	if len(got) != 64 || got != strings.Repeat("ä", 32) {
		t.Fatalf("TruncateVoiceName = %q (%d bytes)", got, len(got))
	}
	if got := TruncateVoiceName("a\xffb"); got != "a�b" {
		t.Fatalf("invalid UTF-8 = %q", got)
	}
	if got := TruncateVoiceName("bob"); got != "bob" {
		t.Fatalf("short = %q", got)
	}
}

// patch returns a copy of b with b[i] = v.
func patch(b []byte, i int, v byte) []byte {
	out := append([]byte(nil), b...)
	out[i] = v
	return out
}
