package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// record frames one encoded room message the way the relay does.
func record(t *testing.T, msg []byte, err error) []byte {
	t.Helper()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	rec, err := wire.AppendRoomRecord(nil, msg)
	if err != nil {
		t.Fatalf("frame: %v", err)
	}
	return rec
}

func stateRec(t *testing.T, st wire.RoomState) []byte {
	t.Helper()
	msg, err := wire.AppendRoomState(nil, st)
	return record(t, msg, err)
}

func eventRec(t *testing.T, e wire.RoomEvent) []byte {
	t.Helper()
	msg, err := wire.AppendRoomEvent(nil, e)
	return record(t, msg, err)
}

func commandRec(t *testing.T, c wire.RoomCommand) []byte {
	t.Helper()
	msg, err := wire.AppendRoomCommand(nil, c)
	return record(t, msg, err)
}

// decodeLines splits pump's stdout into one generic map per line.
func decodeLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		lines = append(lines, m)
	}
	return lines
}

// The stream contract, end to end from the bytes a relay writes: a state
// with attachments and participants, every event kind the relay emits, a
// record type the sim does not print (skipped, noted on stderr), and a
// clean EOF once the stream ends.
func TestPumpPrintsEveryRelayRecordAsOneJSONLine(t *testing.T) {
	var stream bytes.Buffer
	key := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01}
	stream.Write(stateRec(t, wire.RoomState{
		Flags: wire.RoomStateFlagDynamic | wire.RoomStateFlagAttachOK, Seq: 3, YourID: 2, Code: "AB2CD3", Key: key,
		Attachments:  []wire.RoomAttachment{{BroadcastID: "GHJKMN", Label: "pc", Live: true, ViewerCount: 7}},
		Participants: []wire.RoomParticipant{{ID: 1, Kind: wire.RoomClientWebBroadcaster, Flags: wire.RoomParticipantFlagStreaming, Nickname: "tuhis"}, {ID: 2, Nickname: "sim"}},
	}))
	events := []wire.RoomEvent{
		{Seq: 4, Kind: wire.RoomEventParticipantJoined, Participant: wire.RoomParticipant{ID: 3, Nickname: "v"}},
		{Seq: 5, Kind: wire.RoomEventParticipantUpdated, Participant: wire.RoomParticipant{ID: 3, Nickname: "w", Flags: wire.RoomParticipantFlagStreaming}},
		{Seq: 6, Kind: wire.RoomEventParticipantLeft, Participant: wire.RoomParticipant{ID: 3}},
		{Seq: 7, Kind: wire.RoomEventAttachmentAdded, Attachment: wire.RoomAttachment{BroadcastID: "PQRSTU", Label: "laptop", Live: true, ViewerCount: 1}},
		{Seq: 8, Kind: wire.RoomEventAttachmentUpdated, Attachment: wire.RoomAttachment{BroadcastID: "PQRSTU", Label: "laptop", Live: false, ViewerCount: 0}},
		{Seq: 9, Kind: wire.RoomEventAttachmentRemoved, Attachment: wire.RoomAttachment{BroadcastID: "PQRSTU"}, Reason: wire.RoomDetachReasonExpired},
		{Seq: 9, Kind: wire.RoomEventCommandRejected, Command: wire.RoomCommandAttach, Reason: wire.RoomRejectLimit, Message: "no slot"},
		{Seq: 10, Kind: wire.RoomEventRoomEnding, Reason: wire.RoomEndReasonCreator},
	}
	for _, e := range events {
		stream.Write(eventRec(t, e))
	}
	// A RoomCommand is a client→relay record; a relay never sends one, and
	// a sim that met one must skip it rather than die.
	stream.Write(commandRec(t, wire.RoomCommand{Kind: wire.RoomCommandResync}))

	var out, errs bytes.Buffer
	if err := pump(&stream, json.NewEncoder(&out), &errs); err != io.EOF {
		t.Fatalf("pump at a clean end: %v, want io.EOF", err)
	}
	if !strings.Contains(errs.String(), "unexpected record type 0x16 ignored") {
		t.Errorf("stderr = %q, want the skipped-record note", errs.String())
	}
	lines := decodeLines(t, out.String())
	if len(lines) != 1+len(events) {
		t.Fatalf("%d lines, want %d:\n%s", len(lines), 1+len(events), out.String())
	}

	st := lines[0]
	if st["type"] != "state" || st["seq"] != 3.0 || st["yourID"] != 2.0 || st["code"] != "AB2CD3" || st["key"] != "deadbeef0001" {
		t.Errorf("state line = %v", st)
	}
	if _, has := st["creatorToken"]; has {
		t.Errorf("creatorToken present on a state that carries none: %v", st)
	}
	att := st["attachments"].([]any)
	if len(att) != 1 {
		t.Fatalf("attachments = %v", att)
	}
	if a := att[0].(map[string]any); a["broadcastID"] != "GHJKMN" || a["label"] != "pc" || a["live"] != true || a["viewerCount"] != 7.0 {
		t.Errorf("attachment = %v", a)
	}
	parts := st["participants"].([]any)
	if len(parts) != 2 {
		t.Fatalf("participants = %v", parts)
	}
	if p := parts[0].(map[string]any); p["nickname"] != "tuhis" || p["flags"] != float64(wire.RoomParticipantFlagStreaming) || p["kind"] != float64(wire.RoomClientWebBroadcaster) {
		t.Errorf("participant = %v", p)
	}

	for i, e := range events {
		got := lines[1+i]
		if got["type"] != "event" || got["seq"] != float64(e.Seq) || got["kind"] != float64(e.Kind) {
			t.Errorf("event %d = %v, want seq %d kind 0x%02x", i, got, e.Seq, e.Kind)
		}
		_, hasP := got["participant"]
		_, hasA := got["attachment"]
		switch e.Kind {
		case wire.RoomEventParticipantJoined, wire.RoomEventParticipantUpdated, wire.RoomEventParticipantLeft:
			if !hasP || hasA {
				t.Fatalf("event %d: participant body missing or attachment leaked: %v", i, got)
			}
			if p := got["participant"].(map[string]any); p["id"] != float64(e.Participant.ID) || p["nickname"] != e.Participant.Nickname || p["flags"] != float64(e.Participant.Flags) {
				t.Errorf("event %d participant = %v", i, p)
			}
		case wire.RoomEventAttachmentAdded, wire.RoomEventAttachmentUpdated, wire.RoomEventAttachmentRemoved:
			if hasP || !hasA {
				t.Fatalf("event %d: attachment body missing or participant leaked: %v", i, got)
			}
			if a := got["attachment"].(map[string]any); a["broadcastID"] != e.Attachment.BroadcastID || a["live"] != e.Attachment.Live || a["viewerCount"] != float64(e.Attachment.ViewerCount) {
				t.Errorf("event %d attachment = %v", i, a)
			}
		default:
			if hasP || hasA {
				t.Errorf("event %d carries a body it has no data for: %v", i, got)
			}
		}
		if got["reason"] != float64(e.Reason) {
			t.Errorf("event %d reason = %v, want %d", i, got["reason"], e.Reason)
		}
	}
	rejected := lines[7]
	if rejected["command"] != float64(wire.RoomCommandAttach) || rejected["message"] != "no slot" {
		t.Errorf("rejected = %v", rejected)
	}
	if _, has := lines[8]["command"]; has {
		t.Errorf("room-ending line carries a command field: %v", lines[8])
	}
}

// What ends the pump, and how: a truncated stream, a length prefix outside
// the record bounds, a malformed state body, and an event kind from the
// reserved (chat/voice) range. Each is an error the run loop turns into the
// close line — the sim never prints a half-parsed record, and everything
// parsed before the bad record has already been printed. A reserved kind
// is the one exception: skipped, per the wire contract.
func TestPumpStopsOnMalformedInput(t *testing.T) {
	good := eventRec(t, wire.RoomEvent{Seq: 1, Kind: wire.RoomEventRoomEnding, Reason: wire.RoomEndReasonEmpty})
	badState := stateRec(t, wire.RoomState{Seq: 1, Code: "x"})
	badState[len(badState)-1] ^= 0xff // participant count now claims entries the record lacks
	reserved := eventRec(t, wire.RoomEvent{Seq: 2, Kind: wire.RoomEventRoomEnding})
	reserved[wire.RoomRecordHeaderSize+6] = 0x40 // kind byte → reserved chat range
	cases := []struct {
		name    string
		in      []byte
		wantErr error
		lines   int
	}{
		{name: "truncated header", in: good[:1], wantErr: io.ErrUnexpectedEOF},
		{name: "truncated body", in: good[:len(good)-1], wantErr: io.ErrUnexpectedEOF},
		{name: "zero length prefix", in: []byte{0, 0, 1, 2}, wantErr: wire.ErrBadRoomRecord},
		{name: "malformed state after a good event", in: append(append([]byte{}, good...), badState...), wantErr: wire.ErrBadRoomState, lines: 1},
		// A reserved (chat/voice) kind is skipped, not fatal: the stream then
		// hits EOF with nothing printed.
		{name: "reserved event kind", in: reserved, wantErr: io.EOF},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := pump(bytes.NewReader(tc.in), json.NewEncoder(&out), io.Discard)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			got := 0
			if out.Len() > 0 {
				got = len(decodeLines(t, out.String()))
			}
			if got != tc.lines {
				t.Fatalf("printed %q, want %d lines", out.String(), tc.lines)
			}
		})
	}
}

// The close line: an application close code, found on either the probe
// or the read error, is the data and exits 0; no code at all is exit 1
// with code -1 (the harness's "nothing answered" shape).
func TestCloseLineMapsSessionErrorsToExitCodes(t *testing.T) {
	se := &webtransport.SessionError{Remote: true, ErrorCode: wire.CloseCodeRoomEnded, Message: "room ended"}
	readErr := errors.New("read: stream reset")
	if line, code := closeLine(se, readErr); code != 0 || line.Code != wire.CloseCodeRoomEnded || line.Reason != "room ended" || line.Type != "close" {
		t.Errorf("probe carries the code: %+v, exit %d", line, code)
	}
	if line, code := closeLine(errors.New("session open"), fmt.Errorf("read: %w", se)); code != 0 || line.Code != wire.CloseCodeRoomEnded {
		t.Errorf("read error carries the code: %+v, exit %d", line, code)
	}
	if line, code := closeLine(errors.New("session open"), readErr); code != 1 || line.Code != -1 || line.Reason != readErr.Error() {
		t.Errorf("no code anywhere: %+v, exit %d", line, code)
	}
}

// The pre-upgrade refusal contract (exit 3 + GAWK_ROOMSIM_DIAL_STATUS on
// stderr) versus nothing answering (exit 1).
func TestDialFailedReportsTheRelayStatus(t *testing.T) {
	var errs bytes.Buffer
	if code := dialFailed(&http.Response{StatusCode: 404}, errors.New("upgrade refused"), "https://relay", &errs); code != exitDialRejected {
		t.Fatalf("exit = %d, want %d", code, exitDialRejected)
	}
	if !strings.HasPrefix(errs.String(), "GAWK_ROOMSIM_DIAL_STATUS=404\n") || !strings.Contains(errs.String(), "refused the dial with 404: upgrade refused") {
		t.Fatalf("stderr = %q", errs.String())
	}
	errs.Reset()
	if code := dialFailed(nil, errors.New("connection refused"), "https://relay", &errs); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if strings.Contains(errs.String(), "GAWK_ROOMSIM_DIAL_STATUS") || !strings.Contains(errs.String(), "dial https://relay: connection refused") {
		t.Fatalf("stderr = %q", errs.String())
	}
}
