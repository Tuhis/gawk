package engine

import (
	"errors"
	"fmt"
	"net/http"
)

// StartPhase says where Start failed. It exists because callers must behave
// differently depending on the answer, and messages are not a contract
// (CODE-REVIEW.md, "Type your failure phases").
//
// This mirrors the browser's BroadcastStartError.phase exactly, and for the
// same reason: R1's reclaim fallback treated "the relay rejected the dial" and
// "capture failed" identically, and silently minted a new broadcast ID out
// from under a live publisher session.
type StartPhase string

const (
	// PhaseConnect: the dial itself failed, so no publisher session was ever
	// established. Safe to retry against a different broadcast ID — this is
	// the only phase in which a failed reclaim may fall back to minting.
	PhaseConnect StartPhase = "connect"
	// PhaseCapture: the relay accepted us and a publisher session existed;
	// capture or encode then failed. The session has already been torn down
	// (no zombie publisher), but falling back to a different broadcast ID
	// here would be wrong: the ID we hold is live and ours.
	PhaseCapture StartPhase = "capture"
)

// StartError is returned by Session.Start.
//
// Status carries the relay's HTTP status when the dial produced one. This is a
// genuine advantage over the browser broadcaster rather than a nicety: the JS
// WebTransport API surfaces an opaque WebTransportError with no status, so the
// browser cannot tell a bad secret from a full relay from a typo'd URL —
// docs/06 records exactly that limitation. webtransport-go's Dial returns the
// *http.Response even on rejection, so here 401/404/409/429 become sentences a
// friend can act on. Zero when the failure never reached HTTP (DNS, TLS,
// connection refused) — which is itself informative.
type StartError struct {
	Phase  StartPhase
	Status int
	Err    error
}

func (e *StartError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s phase failed (HTTP %d): %v", e.Phase, e.Status, e.Err)
	}
	return fmt.Sprintf("%s phase failed: %v", e.Phase, e.Err)
}

func (e *StartError) Unwrap() error { return e.Err }

// Message renders a StartError as a sentence for a human — the GUI and CLI
// both surface this rather than a Go error string (V5's acceptance criterion:
// "errors surface as sentences, not Go error strings").
//
// The statuses are the ones the relay's handlePublish actually returns; keep
// this in step with gawk-server/internal/transport/server.go.
func (e *StartError) Message() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "The relay rejected the publish secret. Check the secret in your settings matches the relay's."
	case http.StatusNotFound:
		return "That broadcast code no longer exists on the relay. Start a new broadcast to get a fresh code."
	case http.StatusConflict:
		return "Someone is already publishing to that broadcast code. Start a new broadcast to get a fresh code."
	case http.StatusTooManyRequests:
		return "The relay is at capacity (too many broadcasts, or too many connection attempts). Try again in a moment."
	case http.StatusUnavailableForLegalReasons:
		// R39 (docs/42 D15): the ban gate answers pre-upgrade, so there is no
		// close code to carry the reason — the status is the whole message.
		// The browser broadcaster cannot read it at all, which is exactly why
		// spelling it out here is worth doing.
		return "This broadcast ID or your address is banned by the relay operator. Contact the operator if you think this is a mistake."
	}
	if e.Phase == PhaseConnect {
		return fmt.Sprintf("Could not reach the relay: %v", e.Err)
	}
	return fmt.Sprintf("Could not start capture: %v", e.Err)
}

// AsStartError extracts a *StartError from err, if there is one.
func AsStartError(err error) (*StartError, bool) {
	var se *StartError
	ok := errors.As(err, &se)
	return se, ok
}

// ErrNoHardwareEncoder is returned when no candidate in the cascade actually
// encodes on this machine (docs/19 Decision 4). It is deliberately terminal:
// hardware encode is the entire reason this app exists, and the browser
// broadcaster already covers software encode on Linux, so the engine refuses
// and points there instead of quietly degrading.
var ErrNoHardwareEncoder = errors.New("no working hardware H.264 encoder found")

// ErrCaptureFormat is returned when every live pipeline died inside
// pipewiresrc — the compositor's screencast stream and the GStreamer chain
// could not agree on a frame format, so no frame ever reached an encoder.
// Distinct from ErrNoHardwareEncoder for the same reason ErrNoLaunchBinary
// is: reporting it as "no working hardware encoder" would send the user
// chasing GPU drivers over a portal/compositor negotiation problem.
var ErrCaptureFormat = errors.New("screen capture format negotiation failed")
