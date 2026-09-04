package main

import (
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// The flag surface is the harness's contract (e2e/rooms-assert.sh builds
// its invocations from it), so the join/mint path and every rejection are
// pinned here.
func TestParseArgsBuildsTheJoinAndMintPaths(t *testing.T) {
	hex32 := strings.Repeat("ab", wire.ResumeTokenSize)
	cases := []struct {
		name     string
		args     []string
		wantPath string
		wantErr  string
	}{
		{name: "join by code", args: []string{"-code", "TuhisRoom", "-nick", "first"},
			wantPath: "/room/TuhisRoom?name=first"},
		{name: "join with creator and attach", args: []string{"-code", "ab2cd3", "-creator", hex32, "-attach", "s3cret"},
			wantPath: "/room/ab2cd3?attach=s3cret&creator=" + hex32 + "&name=roomsim"},
		{name: "mint", args: []string{"-mint", "-broadcast", "AB2CD3", "-resume", hex32, "-label", "pc", "-create-secret", "inv"},
			wantPath: "/room/new?broadcast=AB2CD3&create=inv&label=pc&name=roomsim&resume=" + hex32},
		{name: "neither", args: nil, wantErr: "one of -code or -mint"},
		{name: "both", args: []string{"-mint", "-code", "x"}, wantErr: "mutually exclusive"},
		{name: "mint without proof", args: []string{"-mint", "-broadcast", "AB2CD3"}, wantErr: "-broadcast and -resume"},
		{name: "mint with a short token", args: []string{"-mint", "-broadcast", "AB2CD3", "-resume", "abcd"}, wantErr: "-resume must be"},
		{name: "join with a bad creator token", args: []string{"-code", "x", "-creator", "zz"}, wantErr: "-creator must be"},
		{name: "unknown flag", args: []string{"-code", "x", "-bogus"}, wantErr: "flag provided but not defined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, err := parseArgs(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArgs: %v", err)
			}
			if o.path != tc.wantPath {
				t.Fatalf("path = %q, want %q", o.path, tc.wantPath)
			}
		})
	}
}

func TestParseArgsDefaultsAndURLTrimming(t *testing.T) {
	o, err := parseArgs([]string{"-code", "x", "-url", "https://relay.example:4433/", "-insecure", "-duration", "3s"})
	if err != nil {
		t.Fatal(err)
	}
	if o.url != "https://relay.example:4433" || !o.insecure || o.duration != 3*time.Second || o.nick != "roomsim" {
		t.Fatalf("options = %+v", o)
	}
}

// The JSON shapes a harness greps: hex for the byte fields, empty arrays
// (never null) for the lists, and event bodies only for the kinds that
// carry them.
func TestJSONLines(t *testing.T) {
	st := stateLine(wire.RoomState{Seq: 1, Code: "AB2CD3", Key: []byte{1, 2, 3, 4, 5, 6}, CreatorToken: make([]byte, 16)})
	if st.Key != "010203040506" || st.CreatorToken != strings.Repeat("00", 16) || st.Attachments == nil || st.Participants == nil {
		t.Fatalf("state = %+v", st)
	}
	joined := eventLine(wire.RoomEvent{Kind: wire.RoomEventParticipantJoined, Participant: wire.RoomParticipant{ID: 2, Nickname: "v"}})
	if joined.Participant == nil || joined.Participant.Nickname != "v" || joined.Attachment != nil {
		t.Fatalf("joined = %+v", joined)
	}
	ending := eventLine(wire.RoomEvent{Kind: wire.RoomEventRoomEnding, Reason: wire.RoomEndReasonOperator})
	if ending.Participant != nil || ending.Attachment != nil || ending.Reason != wire.RoomEndReasonOperator {
		t.Fatalf("ending = %+v", ending)
	}
}
