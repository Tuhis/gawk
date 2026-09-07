// Command gawk-roomsim is a synthetic room participant (R42 RM3, docs/44
// §4.5): it dials CONNECT /room/{code} (or mints a room from a live
// broadcast with -mint), sends RoomHello on the one control stream, and
// prints every RoomState and RoomEvent it receives as one JSON line each on
// stdout — so a shell harness can assert on the roster, the attachments and
// the room's HMAC'd key with jq, the way gawk-loadgen lets it assert on
// close codes. It speaks exactly the wire a browser participant speaks.
//
// stdout is machine-readable only:
//
//	{"type":"state","seq":1,"code":"TUHISROOM","key":"...","attachments":[...],"participants":[...]}
//	{"type":"event","seq":2,"kind":1,"participant":{...}}
//	{"type":"close","code":4007,"reason":"..."}
//
// A pre-upgrade refusal (404 unknown room, 403 wrong token, 429 full, 451
// banned, 503 no home reachable) is reported on stderr as
//
//	GAWK_ROOMSIM_DIAL_STATUS=404
//
// with exit code 3 — the same contract as gawk-pubsim's, and for the same
// reason: a harness must be able to tell "the relay said no, and why" from
// "nothing answered". Exit 0 when -duration elapses with the session still
// up or the relay closed it; 1 on any other failure; 2 on bad flags.
package main

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// exitDialRejected mirrors gawk-pubsim's: the relay refused the dial with
// an HTTP status, distinct from a crash (1) and a bad flag (2).
const exitDialRejected = 3

// options is the parsed command line; parseArgs is what the unit test
// covers, so every validation rule lives there and not in main.
type options struct {
	url      string
	insecure bool
	nick     string
	duration time.Duration
	// path is the CONNECT path with its query, built from the flags.
	path string
}

func parseArgs(args []string) (options, error) {
	fs := flag.NewFlagSet("gawk-roomsim", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o options
	code := fs.String("code", "", "room code to join (a 6-char dynamic code or a static slug)")
	creator := fs.String("creator", "", "creator token (32 hex chars) to present on join")
	attach := fs.String("attach", "", "a static room's attach secret to present on join")
	mint := fs.Bool("mint", false, "mint a dynamic room from a live broadcast instead of joining (-broadcast and -resume required)")
	broadcast := fs.String("broadcast", "", "broadcast ID to mint from (-mint)")
	resume := fs.String("resume", "", "the broadcast's resume token, 32 hex chars (-mint)")
	label := fs.String("label", "roomsim", "the minted attachment's tile label (-mint)")
	create := fs.String("create-secret", "", "the relay's room-create secret, if it requires one (-mint)")
	fs.StringVar(&o.url, "url", "https://127.0.0.1:4433", "relay base URL")
	fs.BoolVar(&o.insecure, "insecure", false, "skip TLS verification (dev certs)")
	fs.StringVar(&o.nick, "nick", "roomsim", "nickname in the roster")
	fs.DurationVar(&o.duration, "duration", 10*time.Second, "how long to hold the control session; 0 = until the relay closes it or SIGTERM")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	switch {
	case *mint && *code != "":
		return o, errors.New("-mint and -code are mutually exclusive")
	case *mint:
		if *broadcast == "" || *resume == "" {
			return o, errors.New("-mint requires -broadcast and -resume")
		}
		if raw, err := hex.DecodeString(*resume); err != nil || len(raw) != wire.ResumeTokenSize {
			return o, fmt.Errorf("-resume must be %d hex chars", 2*wire.ResumeTokenSize)
		}
		q := url.Values{}
		q.Set("broadcast", *broadcast)
		q.Set("resume", *resume)
		q.Set("label", *label)
		q.Set("name", o.nick)
		if *create != "" {
			q.Set("create", *create)
		}
		o.path = "/room/new?" + q.Encode()
	case *code == "":
		return o, errors.New("one of -code or -mint is required")
	default:
		if *creator != "" {
			if raw, err := hex.DecodeString(*creator); err != nil || len(raw) != wire.RoomCreatorTokenSize {
				return o, fmt.Errorf("-creator must be %d hex chars", 2*wire.RoomCreatorTokenSize)
			}
		}
		q := url.Values{}
		q.Set("name", o.nick)
		if *creator != "" {
			q.Set("creator", *creator)
		}
		if *attach != "" {
			q.Set("attach", *attach)
		}
		o.path = "/room/" + url.PathEscape(*code) + "?" + q.Encode()
	}
	o.url = strings.TrimRight(o.url, "/")
	return o, nil
}

// JSON shapes. Byte fields (key, creatorToken) are hex so jq can compare
// them with /statusz keys and pubsim's `ROOM` line directly.
type attachmentJSON struct {
	BroadcastID string `json:"broadcastID"`
	Label       string `json:"label"`
	Live        bool   `json:"live"`
	ViewerCount uint32 `json:"viewerCount"`
}

type participantJSON struct {
	ID       uint16 `json:"id"`
	Kind     uint8  `json:"kind"`
	Flags    uint8  `json:"flags"`
	Nickname string `json:"nickname"`
}

type stateJSON struct {
	Type         string            `json:"type"`
	Seq          uint32            `json:"seq"`
	Flags        uint8             `json:"flags"`
	YourID       uint16            `json:"yourID"`
	Code         string            `json:"code"`
	DisplayName  string            `json:"displayName,omitempty"`
	CreatorToken string            `json:"creatorToken,omitempty"`
	Key          string            `json:"key"`
	Attachments  []attachmentJSON  `json:"attachments"`
	Participants []participantJSON `json:"participants"`
}

type eventJSON struct {
	Type        string           `json:"type"`
	Seq         uint32           `json:"seq"`
	Kind        uint8            `json:"kind"`
	Participant *participantJSON `json:"participant,omitempty"`
	Attachment  *attachmentJSON  `json:"attachment,omitempty"`
	Reason      uint8            `json:"reason"`
	Command     uint8            `json:"command,omitempty"`
	Message     string           `json:"message,omitempty"`
}

type closeJSON struct {
	Type   string `json:"type"`
	Code   int64  `json:"code"`
	Reason string `json:"reason,omitempty"`
}

func toAttachment(a wire.RoomAttachment) attachmentJSON {
	return attachmentJSON{BroadcastID: a.BroadcastID, Label: a.Label, Live: a.Live, ViewerCount: a.ViewerCount}
}

func toParticipant(p wire.RoomParticipant) participantJSON {
	return participantJSON{ID: p.ID, Kind: p.Kind, Flags: p.Flags, Nickname: p.Nickname}
}

func stateLine(st wire.RoomState) stateJSON {
	out := stateJSON{Type: "state", Seq: st.Seq, Flags: st.Flags, YourID: st.YourID, Code: st.Code, DisplayName: st.DisplayName,
		Key: hex.EncodeToString(st.Key), Attachments: []attachmentJSON{}, Participants: []participantJSON{}}
	if len(st.CreatorToken) > 0 {
		out.CreatorToken = hex.EncodeToString(st.CreatorToken)
	}
	for _, a := range st.Attachments {
		out.Attachments = append(out.Attachments, toAttachment(a))
	}
	for _, p := range st.Participants {
		out.Participants = append(out.Participants, toParticipant(p))
	}
	return out
}

func eventLine(e wire.RoomEvent) eventJSON {
	out := eventJSON{Type: "event", Seq: e.Seq, Kind: e.Kind, Reason: e.Reason, Command: e.Command, Message: e.Message}
	switch e.Kind {
	case wire.RoomEventParticipantJoined, wire.RoomEventParticipantLeft, wire.RoomEventParticipantUpdated:
		p := toParticipant(e.Participant)
		out.Participant = &p
	case wire.RoomEventAttachmentAdded, wire.RoomEventAttachmentRemoved, wire.RoomEventAttachmentUpdated:
		a := toAttachment(e.Attachment)
		out.Attachment = &a
	}
	return out
}

func main() {
	o, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "gawk-roomsim: %v\n", err)
		os.Exit(2)
	}
	os.Exit(run(o))
}

func run(o options) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if o.duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.duration)
		defer cancel()
	}

	d := &webtransport.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: o.insecure},
		QUICConfig: &quic.Config{
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
			MaxIdleTimeout:                   30 * time.Second,
			KeepAlivePeriod:                  10 * time.Second,
		},
	}
	defer d.Close()
	dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second)
	rsp, sess, err := d.Dial(dialCtx, o.url+o.path, nil)
	dialCancel()
	if err != nil {
		return dialFailed(rsp, err, o.url, os.Stderr)
	}
	defer sess.CloseWithError(0, "roomsim done")

	stream, err := sess.OpenStreamSync(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gawk-roomsim: open control stream: %v\n", err)
		return 1
	}
	hello, err := wire.AppendRoomHello(nil, wire.RoomHello{Protocol: wire.RoomProtocolVersion, ClientKind: wire.RoomClientNative, Nickname: o.nick})
	if err != nil {
		fmt.Fprintf(os.Stderr, "gawk-roomsim: encode hello: %v\n", err)
		return 1
	}
	rec, err := wire.AppendRoomRecord(nil, hello)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gawk-roomsim: frame hello: %v\n", err)
		return 1
	}
	if _, err := stream.Write(rec); err != nil {
		fmt.Fprintf(os.Stderr, "gawk-roomsim: send hello: %v\n", err)
		return 1
	}

	enc := json.NewEncoder(os.Stdout)
	readErr := make(chan error, 1)
	go func() { readErr <- pump(stream, enc, os.Stderr) }()

	select {
	case <-ctx.Done():
		// Duration elapsed or SIGTERM with the session still up: a clean
		// client-side leave.
		return 0
	case err := <-readErr:
		return report(sess, err, enc)
	}
}

// dialFailed maps a failed dial to the exit code and the stderr contract:
// a relay that answered with an HTTP status is exit 3 plus the
// GAWK_ROOMSIM_DIAL_STATUS line, anything else (nothing answered) is 1.
func dialFailed(rsp *http.Response, err error, url string, errw io.Writer) int {
	if rsp != nil {
		fmt.Fprintf(errw, "GAWK_ROOMSIM_DIAL_STATUS=%d\n", rsp.StatusCode)
		fmt.Fprintf(errw, "gawk-roomsim: the relay refused the dial with %d: %v\n", rsp.StatusCode, err)
		return exitDialRejected
	}
	fmt.Fprintf(errw, "gawk-roomsim: dial %s: %v\n", url, err)
	return 1
}

// pump decodes records until the stream ends, printing each as JSON on enc;
// records of a type the sim does not print are noted on errw and skipped.
func pump(stream io.Reader, enc *json.Encoder, errw io.Writer) error {
	var hdr [wire.RoomRecordHeaderSize]byte
	for {
		if _, err := io.ReadFull(stream, hdr[:]); err != nil {
			return err
		}
		n, err := wire.ParseRoomRecordLength(hdr[:])
		if err != nil {
			return err
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(stream, buf); err != nil {
			return err
		}
		if len(buf) < 2 {
			return wire.ErrBadRoomRecord
		}
		switch buf[1] {
		case wire.TypeRoomState:
			st, err := wire.ParseRoomState(buf)
			if err != nil {
				return err
			}
			if err := enc.Encode(stateLine(st)); err != nil {
				return err
			}
		case wire.TypeRoomEvent:
			e, err := wire.ParseRoomEvent(buf)
			if errors.Is(err, wire.ErrUnknownRoomKind) {
				// A reserved (chat/voice) kind from a newer relay: readers skip
				// it and keep their place in the sequence (wire/room.go).
				fmt.Fprintf(errw, "gawk-roomsim: unknown event kind 0x%02x at seq %d skipped\n", e.Kind, e.Seq)
				continue
			}
			if err != nil {
				return err
			}
			if err := enc.Encode(eventLine(e)); err != nil {
				return err
			}
		default:
			fmt.Fprintf(errw, "gawk-roomsim: unexpected record type 0x%02x ignored\n", buf[1])
		}
	}
}

// report prints the close line. A session the relay closed with an
// application code (4007 room ended, 4002 draining, 400 protocol) is a
// normal end for a harness — the code is the data — so it exits 0; a
// transport death with no code at all is what exit 1 flags.
func report(sess *webtransport.Session, err error, enc *json.Encoder) int {
	// Give the close capsule a bounded moment to land: the stream read's
	// error races the session close (the gawk-loadgen closeSettle note),
	// and the session's context is the authority on whether it is closed.
	settle, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	select {
	case <-sess.Context().Done():
	case <-settle.Done():
	}
	// The way gawk-loadgen reads a close code: a session-level operation on
	// a closed session fails with the peer's SessionError.
	_, oerr := sess.OpenUniStream()
	line, code := closeLine(oerr, err)
	_ = enc.Encode(line)
	if code != 0 {
		fmt.Fprintf(os.Stderr, "gawk-roomsim: control session ended without an application close code: %v\n", err)
	}
	return code
}

// closeLine maps the session's post-close probe error (oerr) and the
// stream read's error to the close line and the exit code: an application
// close code found in either is the data (exit 0); none at all is a
// transport death (code -1, exit 1).
func closeLine(oerr, err error) (closeJSON, int) {
	var se *webtransport.SessionError
	if errors.As(oerr, &se) || errors.As(err, &se) {
		return closeJSON{Type: "close", Code: int64(se.ErrorCode), Reason: se.Message}, 0
	}
	return closeJSON{Type: "close", Code: -1, Reason: err.Error()}, 1
}
