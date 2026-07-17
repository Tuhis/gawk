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

// Stream is a granted screen-capture stream.
type Stream struct {
	// NodeID is the PipeWire node's global object id, to consume via
	// `pipewiresrc path=<NodeID>` (not target-object, which matches a node
	// name/serial rather than the global id — see internal/gst/pipeline.go).
	NodeID uint32
	// FD is the PipeWire remote fd. The caller owns it and must Close it —
	// it is passed to the child via ExtraFiles.
	FD *os.File
	// Version is the portal's ScreenCast interface version, for diagnostics.
	Version uint32

	session dbus.ObjectPath
	caller  Caller
}

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

	nodeID, err := start(ctx, caller, session)
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
		NodeID:  nodeID,
		FD:      fd,
		Version: version,
		session: session,
		caller:  caller,
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

func start(ctx context.Context, c Caller, session dbus.ObjectPath) (uint32, error) {
	opts := map[string]dbus.Variant{}
	// parent_window is empty: we have no window to parent the dialog to at
	// this point, and the portal handles that fine.
	resp, results, err := c.Call(ctx, "Start", opts, session, "")
	if err != nil {
		return 0, fmt.Errorf("portal: Start failed: %w", err)
	}
	if err := responseErr(resp, "Start"); err != nil {
		return 0, err
	}
	return ParseStartResults(results)
}

// ParseStartResults extracts the node id from Start's results. Exported for
// tests.
func ParseStartResults(results map[string]dbus.Variant) (uint32, error) {
	v, ok := results["streams"]
	if !ok {
		return 0, errors.New("portal: Start returned no streams")
	}
	var streams []struct {
		NodeID uint32
		Props  map[string]dbus.Variant
	}
	if err := v.Store(&streams); err != nil {
		return 0, fmt.Errorf("portal: cannot decode streams: %w", err)
	}
	if len(streams) == 0 {
		return 0, errors.New("portal: Start returned an empty stream list")
	}
	return streams[0].NodeID, nil
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
