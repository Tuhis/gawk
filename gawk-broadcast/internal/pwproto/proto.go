// Package pwproto is the newline-delimited JSON spoken between gawk-broadcast
// and its PipeWire helper (R35, docs/39 D4).
//
// It is its own package because both ends must agree and neither may import
// the other: the helper is a separate binary that links libpipewire, and the
// engine must stay buildable and testable without it. The protocol is small on
// purpose — four operations in, five events out — because every message here is
// a place two processes can disagree.
//
// Framing is one JSON object per line, in both directions. There is no
// handshake and no ack: **stdin EOF is the teardown call**. The engine closing
// the pipe is what ends the helper, which makes "the engine died" and "the
// engine asked us to stop" the same code path, and leaves nothing to get wrong
// on the exit that matters most.
package pwproto

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// Op names an operation the engine asks of the helper.
type Op string

const (
	// OpWatch starts (or continues) reporting the emitting-app list. It is
	// idempotent, and the helper reports apps from the moment it connects
	// anyway — the op exists so the engine can express "I am listening now"
	// without a second channel.
	OpWatch Op = "watch"
	// OpCapture creates the virtual sink if needed and links every audio
	// stream of Binary into it. Sending it again with a different binary
	// re-targets: the old links are dropped and the new ones made, with the
	// sink — and therefore the gst pipeline — untouched. That is what makes
	// the mid-session "switch to whole-system audio" a re-link rather than a
	// renegotiation (docs/39 D5).
	OpCapture Op = "capture"
	// OpRelease drops the links and the sink, leaving the daemon as it was
	// found. The helper stays alive and keeps reporting apps.
	OpRelease Op = "release"
	// OpPing asks for a Pong. Only used by tests and by the engine's
	// liveness check; the helper is otherwise event-driven.
	OpPing Op = "ping"
)

// Request is one line on the helper's stdin.
type Request struct {
	Op Op `json:"op"`
	// Binary is the target application's `application.process.binary`
	// (AD2) for OpCapture. Empty for every other op.
	Binary string `json:"binary,omitempty"`
}

// EventType names an event the helper reports.
type EventType string

const (
	// EventReady is emitted once, after the helper has connected to PipeWire
	// and completed its first registry round-trip. Until it arrives the app
	// list is not yet meaningful — "no apps" and "not yet looked" are
	// different claims, and the GUI must not render the first as the second.
	EventReady EventType = "ready"
	// EventApps carries the full list of applications currently emitting
	// audio. Sent on every change, never as a delta: the list is short, the
	// consumer renders it whole, and a delta protocol would be a second place
	// for the two ends to drift.
	EventApps EventType = "apps"
	// EventSink reports the virtual sink after OpCapture created it. Serial is
	// what the gst pipeline addresses (`target-object=<serial>`).
	EventSink EventType = "sink"
	// EventLinks reports how many ports are currently linked from the target
	// into the sink. Zero while the target is silent, which is the signal the
	// GUI's silence hint is driven by (D6) — a link count, not a level meter.
	EventLinks EventType = "links"
	// EventFatal means the helper is about to exit and audio will not come
	// back without a new helper. Audio is subordinate: the engine degrades
	// (D6) and video never notices.
	EventFatal EventType = "fatal"
	// EventPong answers OpPing.
	EventPong EventType = "pong"
)

// App is one application currently emitting audio.
type App struct {
	// Binary is `application.process.binary` — the identity links are
	// maintained against (AD2), and the value the engine persists as the
	// last-used choice.
	Binary string `json:"binary"`
	// Name is `application.name`, for display. Falls back to the node
	// description and then to the binary, so it is never empty.
	Name string `json:"name"`
	// Streams counts this application's live audio streams. A game with a
	// music stream and an effects stream shows 2, which is exactly the case
	// single-node capture cannot serve (docs/39 §7).
	Streams int `json:"streams"`
}

// Event is one line on the helper's stdout.
type Event struct {
	Event EventType `json:"event"`
	// Apps is set on EventApps.
	Apps []App `json:"apps,omitempty"`
	// Serial and NodeID are set on EventSink. Serial is what addresses the
	// node in a gst pipeline; NodeID is the global id, carried for diagnostics
	// (it is what `pw-dump` shows).
	Serial uint32 `json:"serial,omitempty"`
	NodeID uint32 `json:"nodeId,omitempty"`
	// Channels is the sink's channel count, set on EventSink. It mirrors the
	// default sink's layout rather than being stereo (D3) — a stereo sink
	// under a 5.1 game would silently drop the centre channel.
	Channels int `json:"channels,omitempty"`
	// Binary and Links are set on EventLinks. Links is a pointer because zero
	// is the *most* meaningful value it takes — no links means the target has
	// gone silent, which is what drives the GUI's silence hint — so it must be
	// distinguishable from "this event does not carry a link count" rather
	// than folded into it by omitempty.
	Binary string `json:"binary,omitempty"`
	Links  *int   `json:"links,omitempty"`
	// Message carries EventFatal's reason, and any non-fatal detail worth
	// logging alongside another event.
	Message string `json:"message,omitempty"`
	// Version is the libpipewire version the helper linked against, set on
	// EventReady. Recorded because "which PipeWire?" is the first question
	// every report in this area needs answered.
	Version string `json:"version,omitempty"`
}

// Writer serializes events onto a stream, one per line.
//
// It is not safe for concurrent use; the helper writes from one goroutine by
// construction, and that is easier to keep true than a mutex is to keep
// correct.
type Writer struct {
	enc *json.Encoder
	w   *bufio.Writer
}

// NewWriter wraps w. Every Write flushes: the engine is waiting on these lines
// to render a picker, and a buffered event is an event that never happened.
func NewWriter(w io.Writer) *Writer {
	bw := bufio.NewWriter(w)
	return &Writer{enc: json.NewEncoder(bw), w: bw}
}

func (w *Writer) Write(e Event) error {
	if err := w.enc.Encode(e); err != nil {
		return err
	}
	return w.w.Flush()
}

// Reader decodes one message type from a stream of lines.
//
// Unparseable lines are an error rather than a skip: the two ends are the same
// build of the same repo, so a line that does not parse means something is
// wrong that silence would hide.
type Reader[T any] struct {
	sc *bufio.Scanner
}

// NewReader wraps r. maxLine bounds one message; the app list is the largest
// thing on the wire and a machine with hundreds of audio streams is still
// kilobytes.
func NewReader[T any](r io.Reader, maxLine int) *Reader[T] {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), maxLine)
	return &Reader[T]{sc: sc}
}

// MaxLine is the accepted message size. Generous by three orders of magnitude
// relative to any real app list, and still a bound.
const MaxLine = 1 << 20

// Next decodes the next message. It returns io.EOF at the end of the stream —
// which, on the helper's stdin, is the instruction to exit.
func (r *Reader[T]) Next() (T, error) {
	var v T
	for {
		if !r.sc.Scan() {
			if err := r.sc.Err(); err != nil {
				return v, err
			}
			return v, io.EOF
		}
		line := r.sc.Bytes()
		if len(line) == 0 {
			continue // a blank line is framing noise, not a message
		}
		if err := json.Unmarshal(line, &v); err != nil {
			return v, fmt.Errorf("pwproto: cannot decode %q: %w", truncate(line), err)
		}
		return v, nil
	}
}

// LinkCount is Links dereferenced, and whether it was set.
func (e Event) LinkCount() (int, bool) {
	if e.Links == nil {
		return 0, false
	}
	return *e.Links, true
}

// WithLinks returns e carrying a link count.
func (e Event) WithLinks(n int) Event {
	e.Links = &n
	return e
}

func truncate(b []byte) string {
	const max = 120
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}
