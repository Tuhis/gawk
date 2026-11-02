// Package status holds what GET /whip/status reports (docs/83 D9): the fixed
// reason sets, the JSON shape, the per-key records and D7's backoff ladder.
//
// The reason strings are a contract with gawk-app (§5.6 gives each one copy),
// so they are defined here once and nowhere else in the gateway. The app maps
// an unknown reason to its generic line, so adding one is backwards
// compatible; renaming one is not.
package status

// Warning reasons (warnings[].reason). A warning never ends a session.
const (
	// WarnGOPLong: the measured keyframe interval (the median of the last
	// spacings, D7) is above the warn bound (1.1 s by default) and at most
	// the end bound (2.2 s). Carries gopMs.
	WarnGOPLong = "gop_long"
)

// lastError reasons (lastError.reason). The first group ends a session; the
// second refuses a POST before any session exists.
const (
	// ReasonLost: OBS went away without a DELETE (ICE or consent failure, the
	// PeerConnection closing). The engine closed with code 0, so the
	// broadcast waits in the relay's grace (D8).
	ReasonLost = "lost"
	// ReasonGOPTooLong: the gateway ended the session because the median
	// keyframe interval exceeded the end bound, or two consecutive gaps
	// exceeded the early bound before enough spacings existed (D7). Carries
	// gopMs. Starts the backoff.
	ReasonGOPTooLong = "gop_too_long"
	// ReasonBFrames: the stream carries B slices, which the viewer pipeline
	// cannot play live (decode order must equal presentation order). Starts
	// the backoff.
	ReasonBFrames = "bframes"
	// ReasonInvalidH264: the gateway could not rebuild a decodable H.264
	// stream (malformed packetization, an unparseable or unsupported SPS, no
	// parameter sets for the keyframes). Starts the backoff.
	ReasonInvalidH264 = "invalid_h264"
	// ReasonKilled: the relay closed the publisher with 4006 (an operator
	// kill, R39). No backoff: the relay's own ban cooldown answers the
	// reconnect with 451.
	ReasonKilled = "killed"

	// ReasonCodec: the offer has no H.264 packetization-mode 1 video (406).
	// Carries codec ("HEVC" or "AV1") when the offer named one of those.
	ReasonCodec = "codec"
	// ReasonSimulcast: the offer has several video layers (RIDs, a=simulcast
	// or several video m-sections) (406).
	ReasonSimulcast = "simulcast"
	// ReasonGatewayFull: this pod is at maxSessions (or maxSessionsHard for
	// an exempt reconnect) (503 + Retry-After).
	ReasonGatewayFull = "gateway_full"
	// ReasonRelayBusy: the relay answered 429 (caps, rate limit), or could
	// not be reached at all (the gateway answers 503 then).
	ReasonRelayBusy = "relay_busy"
	// ReasonRejected: the relay refused the publish secret or the resume
	// token (401/403). Never banned: the relay keeps 451 for that.
	ReasonRejected = "rejected"
	// ReasonBanned: the relay answered 451 (the ID or the client IP is
	// banned).
	ReasonBanned = "banned"
	// ReasonReserved: the relay answered 409 (the ID is reserved, e.g. for a
	// PoP).
	ReasonReserved = "reserved"
	// ReasonClientIPUnknown: the gateway could not determine the client IP
	// from X-Forwarded-For although the peer is a trusted proxy (400, D4).
	ReasonClientIPUnknown = "client_ip_unknown"
)

// Codec names lastError.codec carries for ReasonCodec.
const (
	CodecHEVC = "HEVC"
	CodecAV1  = "AV1"
)

// WarningReasons and ErrorReasons are the complete fixed sets, for tests
// that walk every reason (WG7: "every warning and refusal carries a reason
// from the fixed set").
var (
	WarningReasons = []string{WarnGOPLong}
	ErrorReasons   = []string{
		ReasonLost, ReasonGOPTooLong, ReasonBFrames, ReasonInvalidH264, ReasonCodec,
		ReasonSimulcast, ReasonGatewayFull, ReasonRelayBusy, ReasonRejected, ReasonBanned,
		ReasonReserved, ReasonClientIPUnknown, ReasonKilled,
	}
)

// StartsBackoff reports whether a session end with this reason starts D7's
// backoff: the reasons the gateway ends a session for itself.
func StartsBackoff(reason string) bool {
	switch reason {
	case ReasonGOPTooLong, ReasonBFrames, ReasonInvalidH264:
		return true
	}
	return false
}

// States (state).
const (
	StateIdle  = "idle"
	StateLive  = "live"
	StateEnded = "ended"
)
