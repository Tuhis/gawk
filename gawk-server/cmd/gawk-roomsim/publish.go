package main

// -publish: the sim as a party broadcaster (R74 PP11, docs/76 §6.10). A
// publish session that sends no media is all the wall needs — a party
// broadcast joins it at publish — and its 0x18 is the broadcaster's whole
// view of the wall, so both are printed for the harness.

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// syncWriter makes one Write atomic, so the publish session's lines and
// the room's never interleave (json.Encoder writes each value in one call).
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

type publishJSON struct {
	Type        string `json:"type"`
	BroadcastID string `json:"broadcastID"`
}

type localRelayJSON struct {
	Type       string `json:"type"`
	Linked     bool   `json:"linked"`
	OnWall     bool   `json:"onWall"`
	Name       string `json:"name"`
	HomeOrigin string `json:"homeOrigin"`
	WallCode   string `json:"wallCode"`
}

func localRelayLine(m wire.LocalRelayInfo) localRelayJSON {
	return localRelayJSON{Type: "localRelay", Linked: m.Linked, OnWall: m.OnWall, Name: m.Name, HomeOrigin: m.HomeOrigin, WallCode: m.WallCode}
}

// publication is a live publish session: its ID, its resume token (hex)
// and a channel that yields the exit code once the relay closes it.
type publication struct {
	sess      *webtransport.Session
	id, token string
	closed    chan int
}

// startPublish dials the publish path and waits for the announce and the
// resume token. nil means it failed, with the exit code to return.
func startPublish(ctx context.Context, d *webtransport.Transport, o options, hdr http.Header, enc *json.Encoder) (*publication, int) {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	rsp, sess, err := d.Dial(dialCtx, o.url+o.publishPath, hdr)
	cancel()
	if err != nil {
		return nil, dialFailed(rsp, err, o.url, os.Stderr)
	}
	p := &publication{sess: sess, closed: make(chan int, 1)}
	ready := make(chan struct{})
	go p.read(enc, ready)
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		fmt.Fprintln(os.Stderr, "gawk-roomsim: the relay announced no broadcast ID and resume token within 15s")
		sess.CloseWithError(0, "")
		return nil, 1
	case <-ctx.Done():
		sess.CloseWithError(0, "")
		return nil, 0
	}
	_ = enc.Encode(publishJSON{Type: "publish", BroadcastID: p.id})
	return p, 0
}

// read takes the publish session's uni streams until it ends: the announce
// and the resume token close ready, each 0x18 is printed, and the close
// is printed and reported on p.closed.
func (p *publication) read(enc *json.Encoder, ready chan struct{}) {
	var id, token string
	for {
		st, err := p.sess.AcceptUniStream(context.Background())
		if err != nil {
			line, code := closeLine(err, err)
			line.Type = "publishClose"
			_ = enc.Encode(line)
			p.closed <- code
			return
		}
		data, _ := io.ReadAll(io.LimitReader(st, 64<<10))
		_, typ, err := wire.PeekType(data)
		if err != nil {
			continue
		}
		switch typ {
		case wire.TypeBroadcastAnnounce:
			id, _ = wire.ParseBroadcastAnnounce(data)
		case wire.TypeResumeToken:
			if tok, err := wire.ParseResumeToken(data); err == nil {
				token = hex.EncodeToString(tok)
			}
		case wire.TypeLocalRelayInfo:
			if m, err := wire.ParseLocalRelayInfo(data); err == nil {
				_ = enc.Encode(localRelayLine(m))
			}
		}
		if id != "" && token != "" && ready != nil {
			p.id, p.token = id, token
			close(ready)
			ready = nil
		}
	}
}

// wallCommands reads "hide" and "show" lines and sends the broadcaster's
// own Detach and Attach on the room's control stream until in ends.
func wallCommands(in io.Reader, stream io.Writer, p *publication, errw io.Writer) {
	tok, _ := hex.DecodeString(p.token)
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		var cmd wire.RoomCommand
		switch strings.TrimSpace(sc.Text()) {
		case "hide":
			cmd = wire.RoomCommand{Kind: wire.RoomCommandDetach, BroadcastID: p.id}
		case "show":
			cmd = wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: p.id, ResumeToken: tok}
		case "":
			continue
		default:
			fmt.Fprintf(errw, "gawk-roomsim: unknown stdin command %q (want hide or show)\n", sc.Text())
			continue
		}
		if err := sendCommand(stream, cmd); err != nil {
			fmt.Fprintf(errw, "gawk-roomsim: send %s: %v\n", sc.Text(), err)
			return
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(errw, "gawk-roomsim: stdin: %v\n", err)
	}
}
