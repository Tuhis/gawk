package portal

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/godbus/dbus/v5"
)

// fakeCaller drives the handshake without a session bus (CI has none, and the
// state machine — version gating, restore tokens, cancellation — is worth
// testing regardless of whether a desktop is present).
type fakeCaller struct {
	version    uint32
	versionErr error

	calls []recordedCall
	// responses maps a method name to what the portal answers.
	responses map[string]fakeResponse

	fdErr  error
	closed bool
}

type recordedCall struct {
	method string
	opts   map[string]dbus.Variant
	args   []any
}

type fakeResponse struct {
	code    uint32
	results map[string]dbus.Variant
	err     error
}

func newFakeCaller(version uint32) *fakeCaller {
	return &fakeCaller{
		version: version,
		responses: map[string]fakeResponse{
			"CreateSession": {results: map[string]dbus.Variant{
				"session_handle": dbus.MakeVariant("/org/freedesktop/portal/desktop/session/1/gawk"),
			}},
			"SelectSources": {},
			"Start": {results: map[string]dbus.Variant{
				"streams":       streamsVariant(42),
				"restore_token": dbus.MakeVariant("token-from-portal"),
			}},
		},
	}
}

// streamsVariant builds an a(ua{sv}) the way the portal sends it.
func streamsVariant(nodeID uint32) dbus.Variant {
	type stream struct {
		NodeID uint32
		Props  map[string]dbus.Variant
	}
	return dbus.MakeVariant([]stream{{NodeID: nodeID, Props: map[string]dbus.Variant{}}})
}

func (f *fakeCaller) ScreenCastVersion(ctx context.Context) (uint32, error) {
	return f.version, f.versionErr
}

func (f *fakeCaller) Call(ctx context.Context, method string, opts map[string]dbus.Variant, args ...any) (uint32, map[string]dbus.Variant, error) {
	f.calls = append(f.calls, recordedCall{method: method, opts: opts, args: args})
	r := f.responses[method]
	if r.results == nil {
		r.results = map[string]dbus.Variant{}
	}
	return r.code, r.results, r.err
}

func (f *fakeCaller) OpenPipeWireRemote(ctx context.Context, session dbus.ObjectPath) (*os.File, error) {
	if f.fdErr != nil {
		return nil, f.fdErr
	}
	// A real fd we can close, standing in for the PipeWire remote.
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	w.Close()
	return r, nil
}

func (f *fakeCaller) Close() { f.closed = true }

func (f *fakeCaller) callFor(method string) (recordedCall, bool) {
	for _, c := range f.calls {
		if c.method == method {
			return c, true
		}
	}
	return recordedCall{}, false
}

func TestOpenHappyPath(t *testing.T) {
	f := newFakeCaller(4)
	s, err := Open(context.Background(), Options{Caller: f})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if s.NodeID != 42 {
		t.Errorf("NodeID = %d, want 42", s.NodeID)
	}
	if s.FD == nil {
		t.Error("no PipeWire fd")
	}
	// The order is the spec's, and getting it wrong fails at runtime only.
	want := []string{"CreateSession", "SelectSources", "Start"}
	if len(f.calls) != len(want) {
		t.Fatalf("made %d calls, want %d", len(f.calls), len(want))
	}
	for i, m := range want {
		if f.calls[i].method != m {
			t.Errorf("call %d = %q, want %q", i, f.calls[i].method, m)
		}
	}
}

// Decision 13: the browser path embeds the cursor, so silently losing the
// pointer would be a viewer-visible regression.
func TestCursorIsAlwaysEmbedded(t *testing.T) {
	opts := SelectSourcesOptions()
	mode, ok := opts["cursor_mode"].Value().(uint32)
	if !ok || mode != cursorEmbedded {
		t.Errorf("cursor_mode = %v, want embedded (%d)", opts["cursor_mode"].Value(), cursorEmbedded)
	}
}

// We ask what to share on every Start rather than persisting the choice
// (docs/19): persist_mode and restore_token are never sent, so the desktop's
// picker appears each session — even if a portal happens to hand back a token,
// we never keep or replay it.
func TestNeverRequestsPersistence(t *testing.T) {
	opts := SelectSourcesOptions()
	if _, present := opts["persist_mode"]; present {
		t.Error("persist_mode is set; we must not ask the portal to persist the choice")
	}
	if _, present := opts["restore_token"]; present {
		t.Error("restore_token is set; we must not replay a previous grant")
	}

	// End to end: even a portal that returns a restore_token (the fake does)
	// produces a Stream with no notion of one — the field is gone, and the
	// handshake carries nothing forward.
	f := newFakeCaller(4)
	s, err := Open(context.Background(), Options{Caller: f})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	call, ok := f.callFor("SelectSources")
	if !ok {
		t.Fatal("SelectSources was never called")
	}
	if _, present := call.opts["persist_mode"]; present {
		t.Error("SelectSources was sent persist_mode on the wire")
	}
	if _, present := call.opts["restore_token"]; present {
		t.Error("SelectSources was sent restore_token on the wire")
	}
}

// A cancelled picker is a normal outcome and must be distinguishable: the
// browser's equivalent confusion (R1) silently minted a new broadcast when the
// user simply dismissed the dialog.
func TestCancelledPickerIsTyped(t *testing.T) {
	f := newFakeCaller(4)
	f.responses["Start"] = fakeResponse{code: responseCancelled}
	_, err := Open(context.Background(), Options{Caller: f})
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("error = %v, want ErrCancelled", err)
	}
	if !f.closed {
		t.Error("caller not closed after a cancelled handshake: the D-Bus connection leaks")
	}
}

func TestNoPortalIsTyped(t *testing.T) {
	f := newFakeCaller(0)
	f.versionErr = errors.New("no such interface")
	_, err := Open(context.Background(), Options{Caller: f})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if !f.closed {
		t.Error("caller not closed when no portal was available")
	}
}

// Every failure path must release the D-Bus connection.
func TestFailurePathsCloseTheCaller(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeCaller)
	}{
		{"CreateSession errors", func(f *fakeCaller) {
			f.responses["CreateSession"] = fakeResponse{err: errors.New("bus error")}
		}},
		{"CreateSession returns no handle", func(f *fakeCaller) {
			f.responses["CreateSession"] = fakeResponse{results: map[string]dbus.Variant{}}
		}},
		{"SelectSources cancelled", func(f *fakeCaller) {
			f.responses["SelectSources"] = fakeResponse{code: responseCancelled}
		}},
		{"Start returns no streams", func(f *fakeCaller) {
			f.responses["Start"] = fakeResponse{results: map[string]dbus.Variant{}}
		}},
		{"OpenPipeWireRemote fails", func(f *fakeCaller) {
			f.fdErr = errors.New("no fd for you")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCaller(4)
			tc.setup(f)
			if _, err := Open(context.Background(), Options{Caller: f}); err == nil {
				t.Fatal("Open succeeded; want an error")
			}
			if !f.closed {
				t.Error("caller not closed on this failure path: the D-Bus connection leaks")
			}
		})
	}
}

// Some portals have historically sent session_handle as an object path rather
// than the spec's string. Accept either rather than fail on a desktop that is
// merely idiosyncratic.
func TestSessionHandleAcceptsStringOrObjectPath(t *testing.T) {
	for _, v := range []dbus.Variant{
		dbus.MakeVariant("/org/freedesktop/portal/desktop/session/1/gawk"),
		dbus.MakeVariant(dbus.ObjectPath("/org/freedesktop/portal/desktop/session/1/gawk")),
	} {
		f := newFakeCaller(4)
		f.responses["CreateSession"] = fakeResponse{results: map[string]dbus.Variant{"session_handle": v}}
		s, err := Open(context.Background(), Options{Caller: f})
		if err != nil {
			t.Fatalf("session_handle as %s: %v", v.Signature(), err)
		}
		s.Close()
	}
}

func TestParseStartResults(t *testing.T) {
	// A restore_token in the results is ignored, not an error.
	nodeID, err := ParseStartResults(map[string]dbus.Variant{
		"streams":       streamsVariant(99),
		"restore_token": dbus.MakeVariant("tok"),
	})
	if err != nil {
		t.Fatalf("ParseStartResults: %v", err)
	}
	if nodeID != 99 {
		t.Errorf("nodeID = %d, want 99", nodeID)
	}

	if _, err := ParseStartResults(map[string]dbus.Variant{}); err == nil {
		t.Error("accepted results with no streams")
	}
}

// The Request path must match what the portal derives from our handle_token,
// or we subscribe to the wrong signal and hang.
func TestRequestPathMatchesSpec(t *testing.T) {
	got := requestPath(":1.234", "gawk_sources_1")
	want := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/1_234/gawk_sources_1")
	if got != want {
		t.Errorf("requestPath = %q, want %q", got, want)
	}
}

// Handle tokens must be unique per request and contain only characters legal
// in a D-Bus object path element.
func TestTokensAreUniqueAndPathSafe(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok := newToken("gawk_x")
		if seen[tok] {
			t.Fatalf("duplicate token %q", tok)
		}
		seen[tok] = true
		if !dbus.ObjectPath("/org/freedesktop/portal/desktop/request/1_1/" + tok).IsValid() {
			t.Fatalf("token %q makes an invalid object path", tok)
		}
	}
}

func TestStreamCloseReleasesFD(t *testing.T) {
	f := newFakeCaller(4)
	s, err := Open(context.Background(), Options{Caller: f})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if !f.closed {
		t.Error("caller not closed")
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
