package wire

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

// Golden vectors for the room control protocol (R42, docs/44 §4.6),
// computed by hand from the layout comment in room.go. Restated
// byte-identically in the TS mirror (gawk-app/src/transport/wire.test.ts),
// the Go broadcaster's wirecheck and the Rust crate (docs/44 RM1). Do not
// regenerate them from code; if they change, the wire format changed.
const (
	// RoomHello: protocol 1, clientKind 1 (web-broadcaster), wantCaps 0,
	// nickname "tuhis".
	//
	//   01              version
	//   13              type = RoomHello
	//   01              protocol = 1
	//   01              clientKind = web-broadcaster
	//   00              wantCaps = none
	//   05 74 75 68 69 73   nickLen = 5, "tuhis"
	goldenRoomHelloHex = "0113010100057475686973"

	// One room record framing the golden RoomHello (11 bytes).
	goldenRoomRecordHex = "000b" + goldenRoomHelloHex

	// RoomState, dynamic room right after /room/new: flags dynamic|creator
	// (0x03), caps none, seq 7, yourID 1, code "5UP4XW", no display name,
	// creator token 00..0f, one attachment (ABCDEF, label "tuhis", live,
	// 3 viewers), one participant (id 1, web-broadcaster, streaming,
	// "tuhis", no identity).
	//
	//   01 14                         version, type = RoomState
	//   03                            flags = dynamic | creator
	//   00                            caps = none
	//   00 00 00 07                   seq = 7
	//   00 01                         yourID = 1
	//   06 35 55 50 34 58 57          codeLen = 6, "5UP4XW"
	//   00                            nameLen = 0
	//   10 00 01 .. 0f                tokenLen = 16, creator token
	//   06 1a 2b 3c 4d 5e 6f          keyLen = 6, room key
	//   01                            attachCount = 1
	//     06 41 42 43 44 45 46          idLen = 6, "ABCDEF"
	//     05 74 75 68 69 73             labelLen = 5, "tuhis"
	//     01                            flags = live
	//     00 00 00 03                   viewerCount = 3
	//   00 01                         participantCount = 1
	//     00 01                         id = 1
	//     01                            kind = web-broadcaster
	//     02                            flags = streaming
	//     05 74 75 68 69 73             nickLen = 5, "tuhis"
	//     00                            identityLen = 0
	goldenRoomStateDynamicHex = "0114" + "03" + "00" + "00000007" + "0001" +
		"06355550345857" + "00" +
		"10000102030405060708090a0b0c0d0e0f" +
		"061a2b3c4d5e6f" +
		"01" + "06414243444546" + "057475686973" + "01" + "00000003" +
		"0001" + "0001" + "01" + "02" + "057475686973" + "00"

	// RoomState, static room, empty: flags attachOK (0x04), caps none,
	// seq 0, yourID 2, code "TuhisRoom", display name "Tuhis' room", no
	// token, no attachments, one participant (id 2, web-viewer, no flags,
	// "viewer").
	//
	//   01 14 04 00 00 00 00 00 00 02
	//   09 54 75 68 69 73 52 6f 6f 6d         "TuhisRoom"
	//   0b 54 75 68 69 73 27 20 72 6f 6f 6d   "Tuhis' room"
	//   00                                    tokenLen = 0
	//   00                                    keyLen = 0
	//   00                                    attachCount = 0
	//   00 01                                 participantCount = 1
	//     00 02 00 00 06 76 69 65 77 65 72 00
	goldenRoomStateStaticHex = "0114" + "04" + "00" + "00000000" + "0002" +
		"095475686973526f6f6d" + "0b54756869732720726f6f6d" + "00" + "00" + "00" +
		"0001" + "0002" + "00" + "00" + "06766965776572" + "00"

	// RoomEvent ParticipantJoined, seq 8: id 3, native, streaming, "pc".
	//   01 15 00 00 00 08 01 00 03 02 02 02 70 63 00
	goldenRoomEventJoinedHex = "011500000008" + "01" + "0003" + "02" + "02" + "027063" + "00"

	// RoomEvent ParticipantLeft, seq 9, id 3.
	goldenRoomEventLeftHex = "011500000009" + "02" + "0003"

	// RoomEvent AttachmentUpdated, seq 13: ABCDEF, "tuhis", AWAY, 12 viewers.
	goldenRoomEventAttachmentUpdatedHex = "01150000000d" + "12" + "06414243444546" + "057475686973" + "00" + "0000000c"

	// RoomEvent AttachmentRemoved, seq 10: ABCDEF, reason expired (2).
	goldenRoomEventAttachmentRemovedHex = "01150000000a" + "11" + "06414243444546" + "02"

	// RoomEvent RoomEnding, seq 11, reason creator (2).
	goldenRoomEventEndingHex = "01150000000b" + "20" + "02"

	// RoomEvent CommandRejected, seq 12: command attach (1), reason limit
	// (1), message "room full".
	goldenRoomEventRejectedHex = "01150000000c" + "30" + "01" + "01" + "09726f6f6d2066756c6c"

	// RoomCommand Attach: ABCDEF, resume token a0..af, label "tuhis".
	goldenRoomCommandAttachHex = "0116" + "01" + "06414243444546" +
		"10a0a1a2a3a4a5a6a7a8a9aaabacadaeaf" + "057475686973"

	// RoomCommand Detach ABCDEF.
	goldenRoomCommandDetachHex = "0116" + "02" + "06414243444546"

	// RoomCommand SetNickname "tuhis".
	goldenRoomCommandNickHex = "0116" + "03" + "057475686973"

	// RoomCommand EndRoom / Resync: no payload.
	goldenRoomCommandEndHex    = "011604"
	goldenRoomCommandResyncHex = "011605"
)

var (
	goldenRoomHello = RoomHello{Protocol: 1, ClientKind: RoomClientWebBroadcaster, Nickname: "tuhis"}

	goldenCreatorToken = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	goldenRoomKey      = []byte{0x1a, 0x2b, 0x3c, 0x4d, 0x5e, 0x6f}
	goldenResumeToken  = []byte{0xa0, 0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xab, 0xac, 0xad, 0xae, 0xaf}

	goldenRoomStateDynamic = RoomState{
		Flags:        RoomStateFlagDynamic | RoomStateFlagCreator,
		Seq:          7,
		YourID:       1,
		Code:         "5UP4XW",
		CreatorToken: goldenCreatorToken,
		Key:          goldenRoomKey,
		Attachments:  []RoomAttachment{{BroadcastID: "ABCDEF", Label: "tuhis", Live: true, ViewerCount: 3}},
		Participants: []RoomParticipant{{ID: 1, Kind: RoomClientWebBroadcaster, Flags: RoomParticipantFlagStreaming, Nickname: "tuhis"}},
	}
	goldenRoomStateStatic = RoomState{
		Flags:        RoomStateFlagAttachOK,
		YourID:       2,
		Code:         "TuhisRoom",
		DisplayName:  "Tuhis' room",
		Participants: []RoomParticipant{{ID: 2, Kind: RoomClientWebViewer, Nickname: "viewer"}},
	}
)

func TestGoldenRoomHello(t *testing.T) {
	want := mustHex(t, goldenRoomHelloHex)
	got, err := AppendRoomHello(nil, goldenRoomHello)
	if err != nil {
		t.Fatalf("AppendRoomHello: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("RoomHello bytes\n got %x\nwant %x", got, want)
	}
	h, err := ParseRoomHello(want)
	if err != nil {
		t.Fatalf("ParseRoomHello: %v", err)
	}
	if h != goldenRoomHello {
		t.Fatalf("ParseRoomHello = %+v, want %+v", h, goldenRoomHello)
	}
}

func TestGoldenRoomRecord(t *testing.T) {
	want := mustHex(t, goldenRoomRecordHex)
	got, err := AppendRoomRecord(nil, mustHex(t, goldenRoomHelloHex))
	if err != nil {
		t.Fatalf("AppendRoomRecord: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("room record\n got %x\nwant %x", got, want)
	}
	n, err := ParseRoomRecordLength(want)
	if err != nil || n != 11 {
		t.Fatalf("ParseRoomRecordLength = %d, %v; want 11", n, err)
	}
	if _, err := ParseRoomRecordLength([]byte{0, 0}); !errors.Is(err, ErrBadRoomRecord) {
		t.Fatalf("zero length: %v", err)
	}
	if _, err := ParseRoomRecordLength([]byte{0xff, 0xff}); !errors.Is(err, ErrBadRoomRecord) {
		t.Fatalf("oversize length: %v", err)
	}
	if _, err := AppendRoomRecord(nil, make([]byte, MaxRoomRecordSize+1)); !errors.Is(err, ErrBadRoomRecord) {
		t.Fatalf("oversize append: %v", err)
	}
}

func TestGoldenRoomState(t *testing.T) {
	for _, tc := range []struct {
		name string
		hex  string
		want RoomState
	}{
		{"dynamic", goldenRoomStateDynamicHex, goldenRoomStateDynamic},
		{"static", goldenRoomStateStaticHex, goldenRoomStateStatic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := mustHex(t, tc.hex)
			got, err := AppendRoomState(nil, tc.want)
			if err != nil {
				t.Fatalf("AppendRoomState: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("RoomState bytes\n got %x\nwant %x", got, want)
			}
			s, err := ParseRoomState(want)
			if err != nil {
				t.Fatalf("ParseRoomState: %v", err)
			}
			if !reflect.DeepEqual(s, tc.want) {
				t.Fatalf("ParseRoomState = %+v, want %+v", s, tc.want)
			}
		})
	}
}

func TestGoldenRoomEvents(t *testing.T) {
	for _, tc := range []struct {
		name string
		hex  string
		want RoomEvent
	}{
		{"joined", goldenRoomEventJoinedHex, RoomEvent{Seq: 8, Kind: RoomEventParticipantJoined,
			Participant: RoomParticipant{ID: 3, Kind: RoomClientNative, Flags: RoomParticipantFlagStreaming, Nickname: "pc"}}},
		{"left", goldenRoomEventLeftHex, RoomEvent{Seq: 9, Kind: RoomEventParticipantLeft, Participant: RoomParticipant{ID: 3}}},
		{"attachment updated", goldenRoomEventAttachmentUpdatedHex, RoomEvent{Seq: 13, Kind: RoomEventAttachmentUpdated,
			Attachment: RoomAttachment{BroadcastID: "ABCDEF", Label: "tuhis", ViewerCount: 12}}},
		{"attachment removed", goldenRoomEventAttachmentRemovedHex, RoomEvent{Seq: 10, Kind: RoomEventAttachmentRemoved,
			Attachment: RoomAttachment{BroadcastID: "ABCDEF"}, Reason: RoomDetachReasonExpired}},
		{"ending", goldenRoomEventEndingHex, RoomEvent{Seq: 11, Kind: RoomEventRoomEnding, Reason: RoomEndReasonCreator}},
		{"rejected", goldenRoomEventRejectedHex, RoomEvent{Seq: 12, Kind: RoomEventCommandRejected,
			Command: RoomCommandAttach, Reason: RoomRejectLimit, Message: "room full"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := mustHex(t, tc.hex)
			got, err := AppendRoomEvent(nil, tc.want)
			if err != nil {
				t.Fatalf("AppendRoomEvent: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("RoomEvent bytes\n got %x\nwant %x", got, want)
			}
			e, err := ParseRoomEvent(want)
			if err != nil {
				t.Fatalf("ParseRoomEvent: %v", err)
			}
			if !reflect.DeepEqual(e, tc.want) {
				t.Fatalf("ParseRoomEvent = %+v, want %+v", e, tc.want)
			}
		})
	}
}

func TestGoldenRoomCommands(t *testing.T) {
	for _, tc := range []struct {
		name string
		hex  string
		want RoomCommand
	}{
		{"attach", goldenRoomCommandAttachHex, RoomCommand{Kind: RoomCommandAttach, BroadcastID: "ABCDEF", ResumeToken: goldenResumeToken, Label: "tuhis"}},
		{"detach", goldenRoomCommandDetachHex, RoomCommand{Kind: RoomCommandDetach, BroadcastID: "ABCDEF"}},
		{"nick", goldenRoomCommandNickHex, RoomCommand{Kind: RoomCommandSetNickname, Nickname: "tuhis"}},
		{"end", goldenRoomCommandEndHex, RoomCommand{Kind: RoomCommandEndRoom}},
		{"resync", goldenRoomCommandResyncHex, RoomCommand{Kind: RoomCommandResync}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := mustHex(t, tc.hex)
			got, err := AppendRoomCommand(nil, tc.want)
			if err != nil {
				t.Fatalf("AppendRoomCommand: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("RoomCommand bytes\n got %x\nwant %x", got, want)
			}
			c, err := ParseRoomCommand(want)
			if err != nil {
				t.Fatalf("ParseRoomCommand: %v", err)
			}
			if !reflect.DeepEqual(c, tc.want) {
				t.Fatalf("ParseRoomCommand = %+v, want %+v", c, tc.want)
			}
		})
	}
}

// The docs/44 §4.11 reserved ranges: an unknown kind is reported as such
// with the header fields filled in, so a reader can skip the record and a
// relay can answer RoomRejectUnsupported.
func TestRoomReservedKinds(t *testing.T) {
	e, err := ParseRoomEvent(mustHex(t, "0115000000014041"))
	if !errors.Is(err, ErrUnknownRoomKind) {
		t.Fatalf("event 0x40: %v", err)
	}
	if e.Seq != 1 || e.Kind != 0x40 {
		t.Fatalf("event header = %+v", e)
	}
	c, err := ParseRoomCommand(mustHex(t, "01165000"))
	if !errors.Is(err, ErrUnknownRoomKind) {
		t.Fatalf("command 0x50: %v", err)
	}
	if c.Kind != 0x50 {
		t.Fatalf("command kind = %d", c.Kind)
	}
	if _, err := AppendRoomEvent(nil, RoomEvent{Kind: 0x4f}); !errors.Is(err, ErrUnknownRoomKind) {
		t.Fatalf("append reserved event: %v", err)
	}
	if _, err := AppendRoomCommand(nil, RoomCommand{Kind: 0x5f}); !errors.Is(err, ErrUnknownRoomKind) {
		t.Fatalf("append reserved command: %v", err)
	}
}

func TestRoomParsersReject(t *testing.T) {
	hello := mustHex(t, goldenRoomHelloHex)
	// Trailing bytes are a framing error on strict messages.
	if _, err := ParseRoomHello(append(append([]byte{}, hello...), 0x00)); !errors.Is(err, ErrBadRoomHello) {
		t.Fatalf("hello trailing: %v", err)
	}
	bad := append([]byte{}, hello...)
	bad[2] = 2 // protocol
	if _, err := ParseRoomHello(bad); !errors.Is(err, ErrBadRoomHello) {
		t.Fatalf("hello protocol: %v", err)
	}
	bad = append([]byte{}, hello...)
	bad[3] = 3 // client kind
	if _, err := ParseRoomHello(bad); !errors.Is(err, ErrBadRoomHello) {
		t.Fatalf("hello kind: %v", err)
	}
	bad = append([]byte{}, hello...)
	bad[4] = 0x04 // reserved cap bit
	if _, err := ParseRoomHello(bad); !errors.Is(err, ErrBadRoomHello) {
		t.Fatalf("hello caps: %v", err)
	}
	bad = append([]byte{}, hello...)
	bad[5] = 0x20 // nick overrun
	if _, err := ParseRoomHello(bad); !errors.Is(err, ErrBadRoomHello) {
		t.Fatalf("hello nick overrun: %v", err)
	}
	bad = append([]byte{}, hello...)
	bad[6] = 0xff // invalid UTF-8
	if _, err := ParseRoomHello(bad); !errors.Is(err, ErrBadRoomHello) {
		t.Fatalf("hello utf8: %v", err)
	}
	if _, err := AppendRoomHello(nil, RoomHello{Protocol: 1, Nickname: string(make([]byte, MaxRoomNicknameLen+1))}); !errors.Is(err, ErrBadRoomHello) {
		t.Fatalf("append long nick: %v", err)
	}

	state := mustHex(t, goldenRoomStateDynamicHex)
	bad = append([]byte{}, state...)
	bad[2] = 0x08 // reserved state flag
	if _, err := ParseRoomState(bad); !errors.Is(err, ErrBadRoomState) {
		t.Fatalf("state flags: %v", err)
	}
	bad = append([]byte{}, state...)
	bad[3] = 0x04 // reserved cap
	if _, err := ParseRoomState(bad); !errors.Is(err, ErrBadRoomState) {
		t.Fatalf("state caps: %v", err)
	}
	if _, err := ParseRoomState(state[:len(state)-1]); !errors.Is(err, ErrBadRoomState) {
		t.Fatalf("state truncated: %v", err)
	}
	bad = append([]byte{}, state...)
	bad[17] = 0x05 // token length neither 0 nor 16
	if _, err := ParseRoomState(bad); !errors.Is(err, ErrBadRoomState) {
		t.Fatalf("state token len: %v", err)
	}
	if _, err := AppendRoomState(nil, RoomState{Code: "x", CreatorToken: []byte{1}}); !errors.Is(err, ErrBadRoomState) {
		t.Fatalf("append token len: %v", err)
	}
	if _, err := AppendRoomState(nil, RoomState{Code: "x", Key: []byte{1, 2, 3}}); !errors.Is(err, ErrBadRoomState) {
		t.Fatalf("append key len: %v", err)
	}
	bad = append([]byte{}, state...)
	bad[35] = 0x03 // key length neither 0 nor 6
	if _, err := ParseRoomState(bad); !errors.Is(err, ErrBadRoomState) {
		t.Fatalf("state key len: %v", err)
	}
	if _, err := AppendRoomState(nil, RoomState{}); !errors.Is(err, ErrBadRoomState) {
		t.Fatalf("append empty code: %v", err)
	}
	if _, err := AppendRoomState(nil, RoomState{Code: "x", Attachments: []RoomAttachment{{BroadcastID: "0OIL11"}}}); !errors.Is(err, ErrBadRoomState) {
		t.Fatalf("append bad broadcast id: %v", err)
	}
	if _, err := AppendRoomState(nil, RoomState{Code: "x", Participants: []RoomParticipant{{Flags: 0x80}}}); !errors.Is(err, ErrBadRoomState) {
		t.Fatalf("append reserved participant flag: %v", err)
	}

	ev := mustHex(t, goldenRoomEventJoinedHex)
	if _, err := ParseRoomEvent(append(append([]byte{}, ev...), 0)); !errors.Is(err, ErrBadRoomEvent) {
		t.Fatalf("event trailing: %v", err)
	}
	if _, err := ParseRoomEvent(ev[:6]); !errors.Is(err, ErrShortDatagram) {
		t.Fatalf("event short: %v", err)
	}

	cmd := mustHex(t, goldenRoomCommandAttachHex)
	bad = append([]byte{}, cmd...)
	bad[10] = 0x0f // token length 15
	if _, err := ParseRoomCommand(bad); !errors.Is(err, ErrBadRoomCommand) {
		t.Fatalf("command token len: %v", err)
	}
	if _, err := AppendRoomCommand(nil, RoomCommand{Kind: RoomCommandAttach, BroadcastID: "ABCDEF", ResumeToken: []byte{1}}); !errors.Is(err, ErrBadRoomCommand) {
		t.Fatalf("append short token: %v", err)
	}
	// Broadcast IDs normalize on parse: a lower-case ID on the wire is
	// accepted and returned upper-case, so the relay never compares raw.
	lower := mustHex(t, "0116"+"02"+"06616263646566")
	c, err := ParseRoomCommand(lower)
	if err != nil || c.BroadcastID != "ABCDEF" {
		t.Fatalf("lower-case id: %+v, %v", c, err)
	}
	if _, err := ParseRoomCommand(mustHex(t, "0116"+"02"+"06304f494c3131")); !errors.Is(err, ErrBadRoomCommand) {
		t.Fatal("invalid broadcast id accepted")
	}
	if _, err := ParseRoomCommand(mustHex(t, "011604ff")); !errors.Is(err, ErrBadRoomCommand) {
		t.Fatal("end-room with payload accepted")
	}
}

func TestRoomConstants(t *testing.T) {
	if TypeRoomHello != 0x13 || TypeRoomState != 0x14 || TypeRoomEvent != 0x15 || TypeRoomCommand != 0x16 {
		t.Fatal("room types drifted from docs/44 D15")
	}
	if CloseCodeRoomEnded != 4007 {
		t.Fatal("CloseCodeRoomEnded drifted from docs/44 D15")
	}
}
