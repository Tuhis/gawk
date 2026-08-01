// Package portal performs the XDG ScreenCast portal handshake and hands back a
// PipeWire fd (R14, docs/19).
//
// The engine owns this rather than delegating it because `pipewiresrc` cannot:
// screen content is only reachable through a portal-granted PipeWire fd, so
// somebody has to do the D-Bus dance, and doing it ourselves keeps the whole
// capture path in our hands.
//
// The share picker appears **every time capture starts** — by decision: we ask
// what to share on every Start rather than persisting the choice. (The portal
// supports a restore token that would skip the picker on later runs; we
// deliberately do not request one, so persist_mode is never set.) The user
// gets their own desktop's share picker (KDE, GNOME — we don't draw it, we
// don't theme it), which is precisely why the GUI needs no source picker of its
// own.
//
// Do not gate any of this on Wayland: the portal works on X11 GNOME sessions
// too. Gate on the portal call succeeding, and let the error name the portal
// when it doesn't.
package portal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/godbus/dbus/v5"
)

const (
	busName    = "org.freedesktop.portal.Desktop"
	objectPath = "/org/freedesktop/portal/desktop"
	scIface    = "org.freedesktop.portal.ScreenCast"
)

// Source types (SelectSources `types` bitmask).
const (
	sourceMonitor = 1 << 0
	sourceWindow  = 1 << 1
)

// SourceType is what the picker actually returned, read from the per-stream
// `source_type` property of Start's results (ScreenCast interface v3+).
//
// This is the mode switch for the whole of R35 (docs/39 AD1): the desktop's
// picker offers both kinds in one dialog, so what came back *is* the user's
// choice and no separate mode UI can disagree with it. The property is
// optional, and SourceUnknown — an older portal, or a backend that omits it —
// deliberately behaves exactly like a monitor, i.e. exactly like pre-R35.
type SourceType uint32

const (
	// SourceUnknown: the portal reported no source_type. Treated as a monitor
	// everywhere, because "we could not tell" must degrade to today's
	// behavior rather than into a half-configured app mode.
	SourceUnknown SourceType = 0
	// SourceMonitor: a whole screen. The pre-R35 path, untouched.
	SourceMonitor SourceType = sourceMonitor
	// SourceWindow: one window. The only value that turns on app mode.
	SourceWindow SourceType = sourceWindow
	// SourceVirtual is the portal's third kind (a virtual display). Nothing
	// here treats it as a window: it has no owning application to pair audio
	// with, so it takes the monitor path.
	SourceVirtual SourceType = 1 << 2
)

// IsWindow reports whether this stream is a single window — the one condition
// that turns on the whose-audio step and app-mode audio.
func (t SourceType) IsWindow() bool { return t == SourceWindow }

func (t SourceType) String() string {
	switch t {
	case SourceMonitor:
		return "monitor"
	case SourceWindow:
		return "window"
	case SourceVirtual:
		return "virtual"
	default:
		return "unknown"
	}
}

// Cursor modes.
const (
	cursorHidden   = 1 << 0
	cursorEmbedded = 1 << 1
)

// Portal Request response codes.
const (
	responseSuccess   = 0
	responseCancelled = 1
	responseEnded     = 2
)

// ErrCancelled is returned when the user dismissed the share dialog. It is a
// normal outcome, not a failure: the shells say "you cancelled", never a stack
// trace, and — critically — a cancelled picker must not be mistaken for a
// relay problem (R1's reclaim bug was exactly this confusion in the browser).
var ErrCancelled = errors.New("portal: the screen share was cancelled")

// ErrUnavailable is returned when no ScreenCast portal is reachable — the
// error a machine with no xdg-desktop-portal backend gets.
var ErrUnavailable = errors.New("portal: no ScreenCast portal available")

// StartResult is what the portal's Start told us about the granted stream.
//
// Before R35 only NodeID was kept and the rest of the properties were dropped
// on the floor. They are the whole of this milestone's input: SourceType is the
// mode (docs/39 D1) and Size is the fit input for the encode geometry (D2) —
// available *before* the pipeline launches, which is what lets the caps be
// pinned to fitted dimensions instead of letterboxing something later.
type StartResult struct {
	// NodeID is the PipeWire node's global object id.
	NodeID uint32
	// SourceType is the picker's answer, or SourceUnknown on a portal that
	// does not report one.
	SourceType SourceType
	// Width and Height are the stream's reported size in pixels; both zero
	// when the portal omitted `size` (it is optional in the spec). Zero means
	// "no fit input", which the geometry treats as today's exact caps.
	Width, Height int
}

// HasSize reports whether the portal gave us a usable fit input.
func (r StartResult) HasSize() bool { return r.Width > 0 && r.Height > 0 }

// Stream is a granted screen-capture stream.
type Stream struct {
	// NodeID is the PipeWire node's global object id, to consume via
	// `pipewiresrc path=<NodeID>` (not target-object, which matches a node
	// name/serial rather than the global id — see internal/gst/pipeline.go).
	NodeID uint32
	// SourceType and Size come from the same Start results as NodeID; see
	// StartResult. They are what the engine branches the mode on and sizes the
	// encode from — no second portal call, no new permission (docs/39 D1).
	SourceType    SourceType
	Width, Height int
	// FD is the PipeWire remote fd. The caller owns it and must Close it —
	// it is passed to the child via ExtraFiles.
	FD *os.File
	// Version is the portal's ScreenCast interface version, for diagnostics.
	Version uint32

	session dbus.ObjectPath
	caller  Caller
}

// HasSize reports whether the portal reported a stream size.
func (s *Stream) HasSize() bool { return s.Width > 0 && s.Height > 0 }

// Close releases the fd and the portal session.
func (s *Stream) Close() error {
	var err error
	if s.FD != nil {
		err = s.FD.Close()
		s.FD = nil
	}
	if s.caller != nil {
		s.caller.Close()
		s.caller = nil
	}
	return err
}

// Caller is the D-Bus surface the handshake needs. It is a seam: there is no
// session bus on a CI runner, and the state machine (version gating, restore
// tokens, cancellation) is worth testing regardless.
type Caller interface {
	// ScreenCastVersion reports the portal's ScreenCast interface version.
	ScreenCastVersion(ctx context.Context) (uint32, error)
	// Call invokes a ScreenCast method and waits for its Request response.
	// The implementation injects handle_token into opts and predicts the
	// Request object path from it. args are the positional arguments that
	// precede the options vardict.
	Call(ctx context.Context, method string, opts map[string]dbus.Variant, args ...any) (response uint32, results map[string]dbus.Variant, err error)
	// OpenPipeWireRemote returns the PipeWire fd for a started session.
	OpenPipeWireRemote(ctx context.Context, session dbus.ObjectPath) (*os.File, error)
	// Close drops the connection.
	Close()
}

// Options configure Open.
type Options struct {
	// Caller is injectable for tests; nil dials the session bus.
	Caller Caller
}

// Open runs the handshake and returns a live stream.
//
// CreateSession → SelectSources → Start → OpenPipeWireRemote. Every step's
// failure names the portal, because "it didn't work" on a machine whose
// desktop lacks a backend is otherwise indistinguishable from a bug in us.
func Open(ctx context.Context, opts Options) (*Stream, error) {
	caller := opts.Caller
	if caller == nil {
		c, err := dialSessionBus()
		if err != nil {
			return nil, err
		}
		caller = c
	}

	version, err := caller.ScreenCastVersion(ctx)
	if err != nil {
		caller.Close()
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	session, err := createSession(ctx, caller)
	if err != nil {
		caller.Close()
		return nil, err
	}

	if err := selectSources(ctx, caller, session); err != nil {
		caller.Close()
		return nil, err
	}

	res, err := start(ctx, caller, session)
	if err != nil {
		caller.Close()
		return nil, err
	}

	fd, err := caller.OpenPipeWireRemote(ctx, session)
	if err != nil {
		caller.Close()
		return nil, fmt.Errorf("portal: OpenPipeWireRemote failed: %w", err)
	}

	return &Stream{
		NodeID:     res.NodeID,
		SourceType: res.SourceType,
		Width:      res.Width,
		Height:     res.Height,
		FD:         fd,
		Version:    version,
		session:    session,
		caller:     caller,
	}, nil
}

func createSession(ctx context.Context, c Caller) (dbus.ObjectPath, error) {
	opts := map[string]dbus.Variant{
		"session_handle_token": dbus.MakeVariant(newToken("gawk_session")),
	}
	resp, results, err := c.Call(ctx, "CreateSession", opts)
	if err != nil {
		return "", fmt.Errorf("portal: CreateSession failed: %w", err)
	}
	if err := responseErr(resp, "CreateSession"); err != nil {
		return "", err
	}
	var handle dbus.ObjectPath
	v, ok := results["session_handle"]
	if !ok {
		return "", errors.New("portal: CreateSession returned no session_handle")
	}
	// The spec says session_handle is a string ('s'), though some portals have
	// historically sent an object path ('o'). Accept either rather than fail on
	// a desktop that is merely idiosyncratic.
	switch val := v.Value().(type) {
	case string:
		handle = dbus.ObjectPath(val)
	case dbus.ObjectPath:
		handle = val
	default:
		return "", fmt.Errorf("portal: session_handle has unexpected type %T", val)
	}
	if !handle.IsValid() {
		return "", fmt.Errorf("portal: invalid session_handle %q", handle)
	}
	return handle, nil
}

func selectSources(ctx context.Context, c Caller, session dbus.ObjectPath) error {
	opts := SelectSourcesOptions()
	resp, _, err := c.Call(ctx, "SelectSources", opts, session)
	if err != nil {
		return fmt.Errorf("portal: SelectSources failed: %w", err)
	}
	return responseErr(resp, "SelectSources")
}

// SelectSourcesOptions builds the SelectSources vardict. Exported for tests:
// the cursor choice here is a user-visible decision, not an incidental
// argument.
//
// persist_mode is deliberately never set: we ask what to share on every Start
// rather than persisting the choice, so no restore token is ever requested and
// the picker appears every session (docs/19).
func SelectSourcesOptions() map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"types":    dbus.MakeVariant(uint32(sourceMonitor | sourceWindow)),
		"multiple": dbus.MakeVariant(false),
		// Embedded, because the browser path embeds the cursor and silently
		// losing the pointer is a viewer-visible regression (Decision 13).
		"cursor_mode":  dbus.MakeVariant(uint32(cursorEmbedded)),
		"handle_token": dbus.MakeVariant(newToken("gawk_sources")),
	}
}

func start(ctx context.Context, c Caller, session dbus.ObjectPath) (StartResult, error) {
	opts := map[string]dbus.Variant{}
	// parent_window is empty: we have no window to parent the dialog to at
	// this point, and the portal handles that fine.
	resp, results, err := c.Call(ctx, "Start", opts, session, "")
	if err != nil {
		return StartResult{}, fmt.Errorf("portal: Start failed: %w", err)
	}
	if err := responseErr(resp, "Start"); err != nil {
		return StartResult{}, err
	}
	return ParseStartResults(results)
}

// ParseStartResults extracts the granted stream's node id, source type and
// size from Start's results. Exported for tests.
//
// Only the node id is required. `source_type` and `size` are optional in the
// spec and every parse failure below degrades to the zero value rather than an
// error: a portal that reports an unexpected type for a property we merely
// *prefer* to have must not be able to fail a broadcast that would otherwise
// work exactly as it did before R35.
func ParseStartResults(results map[string]dbus.Variant) (StartResult, error) {
	v, ok := results["streams"]
	if !ok {
		return StartResult{}, errors.New("portal: Start returned no streams")
	}
	var streams []struct {
		NodeID uint32
		Props  map[string]dbus.Variant
	}
	if err := v.Store(&streams); err != nil {
		return StartResult{}, fmt.Errorf("portal: cannot decode streams: %w", err)
	}
	if len(streams) == 0 {
		return StartResult{}, errors.New("portal: Start returned an empty stream list")
	}
	s := streams[0]
	out := StartResult{NodeID: s.NodeID}
	if st, ok := parseSourceType(s.Props["source_type"]); ok {
		out.SourceType = st
	}
	out.Width, out.Height = parseSize(s.Props["size"])
	return out, nil
}

// parseSourceType reads the `source_type` property (spec type 'u').
//
// Some backends have been observed sending integer variants of other widths,
// so any integral type is accepted; anything else is "not reported", which is
// the monitor path.
func parseSourceType(v dbus.Variant) (SourceType, bool) {
	switch n := v.Value().(type) {
	case uint32:
		return SourceType(n), true
	case uint64:
		return SourceType(n), true
	case int32:
		if n >= 0 {
			return SourceType(n), true
		}
	case int64:
		if n >= 0 {
			return SourceType(n), true
		}
	}
	return SourceUnknown, false
}

// parseSize reads the `size` property (spec type '(ii)'), returning 0,0 when
// it is absent or unusable. A negative or zero dimension is treated as absent:
// the fit function would reject it anyway, and "no size" is a state the
// geometry already has to handle.
func parseSize(v dbus.Variant) (int, int) {
	var size struct{ W, H int32 }
	if v.Signature().String() != "(ii)" {
		return 0, 0
	}
	if err := v.Store(&size); err != nil {
		return 0, 0
	}
	if size.W <= 0 || size.H <= 0 {
		return 0, 0
	}
	return int(size.W), int(size.H)
}

func responseErr(resp uint32, method string) error {
	switch resp {
	case responseSuccess:
		return nil
	case responseCancelled:
		return ErrCancelled
	case responseEnded:
		return fmt.Errorf("portal: %s ended early", method)
	default:
		return fmt.Errorf("portal: %s returned response code %d", method, resp)
	}
}

// tokenCounter makes handle tokens unique within a process.
var tokenCounter atomic.Uint64

// newToken returns a D-Bus-safe unique token. The portal builds the Request
// object path from it, so it must contain only [A-Za-z0-9_].
func newToken(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, tokenCounter.Add(1))
}

// requestPath predicts the Request object path the portal will use, so a
// caller can subscribe to its Response signal *before* making the call —
// otherwise the response can land first and be missed.
func requestPath(sender, token string) dbus.ObjectPath {
	// The sender's unique name, with the leading ':' dropped and '.' replaced
	// by '_' (per the Request spec).
	s := strings.TrimPrefix(sender, ":")
	s = strings.ReplaceAll(s, ".", "_")
	return dbus.ObjectPath("/org/freedesktop/portal/desktop/request/" + s + "/" + token)
}
