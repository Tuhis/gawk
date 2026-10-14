// Package screening is the contract of R40's automatic content flagging
// (docs/71 D3): what the relay's keyframe route serves, what gawk-admin's
// flagger API accepts and answers, and the window rule both the sampler and
// gawk-admin's replay apply.
//
// It is PUBLIC (not internal/) for the reason `wire`, `events` and `adminapi`
// are: gawk-admin and gawk-sampler import it through their repo-root
// `replace`, so the three parties compile one definition instead of mirroring
// JSON tags. A semantic change here needs commits touching gawk-admin/ AND
// gawk-sampler/ in the same PR (CONTRIBUTING.md), or release-please cuts a
// relay release and leaves the deployed consumers on the old contract.
//
// Keep it dependency-light: the standard library and internal/broadcastid,
// nothing else. No frame bytes ever pass through a type here (D8): the
// contract carries scores, windows and identities only.
package screening

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strconv"
)

// Schema names. Versioned strings, not implied by the route, so a consumer
// pins the shape it parses (the docs/42 §4.5 discipline).
const (
	// SchemaContentFlag names the POST /api/v1/content-flags body, frozen by
	// docs/42 §4.11 and extended additively by docs/71.
	SchemaContentFlag = "gawk.content-flag.v1"
	// SchemaTargets names the GET /api/v1/screening/targets response (D2).
	SchemaTargets = "gawk.screening.targets.v1"
	// SchemaSamples names the POST /api/v1/screening/samples body (D20).
	SchemaSamples = "gawk.screening.samples.v1"
	// SchemaReplay names the POST /api/v1/screening/replay response (D21).
	SchemaReplay = "gawk.screening.replay.v1"
)

// gawk-admin routes the sampler calls, relative to -admin-url.
const (
	PathContentFlags = "/api/v1/content-flags"
	PathTargets      = "/api/v1/screening/targets"
	PathSamples      = "/api/v1/screening/samples"
)

// Sampler modes (D19). Required on every content-flag POST: it decides
// between paging and silence, so no default is safe.
const (
	ModeObserve = "observe"
	ModeFlag    = "flag"
)

// ValidMode reports whether m is one of the two modes.
func ValidMode(m string) bool { return m == ModeObserve || m == ModeFlag }

// Window states a content-flag POST reports (D9).
const (
	// StateTripped is sent after every sample while the window is tripped.
	StateTripped = "tripped"
	// StateCleared is sent once, when a tripped window clears.
	StateCleared = "cleared"
)

// The keyframe route on the relay's ops listener (D1). It is authorized by
// its own bearer, -screening-token, never by the admin token, and is not
// registered at all when that token is empty.
const (
	// KeyframeRoutePattern is the Go 1.22 ServeMux pattern the relay
	// registers.
	KeyframeRoutePattern = "GET /internal/screening/broadcasts/{id}/keyframe"
	// HeaderCodec carries the hub's probed codec family ("h264", "vp8",
	// "vp9", "av1"): the sampler's choice of bitstream conversion (D4).
	HeaderCodec = "X-Gawk-Codec"
	// HeaderPublisherEpoch carries the publisher session the frame belongs
	// to (adminapi.Broadcast.PublisherEpoch). A change of value is a new
	// publisher session: the sampler resets that broadcast's window (D7).
	HeaderPublisherEpoch = "X-Gawk-Publisher-Epoch"
	// KeyframeContentType is the media type of the 200 body: one StreamFrame
	// message verbatim (24-byte header, embedded DecoderConfig, payload).
	KeyframeContentType = "application/vnd.gawk.stream-frame"
)

// KeyframePath is the request path for one broadcast's cached keyframe.
func KeyframePath(broadcastID string) string {
	return "/internal/screening/broadcasts/" + url.PathEscape(broadcastID) + "/keyframe"
}

// KeyframeETag is the strong entity tag of a cached keyframe: publisher
// session and frame ID, so a new session never matches an old tag even when
// its frame IDs restart at the same numbers (D1).
func KeyframeETag(publisherEpoch string, frameID uint32) string {
	return `"` + publisherEpoch + "-" + strconv.FormatUint(uint64(frameID), 10) + `"`
}

// DefaultLabels is the flagged label set (D7, OD3): explicit nudity only.
// Partial nudity is deliberately absent — game footage triggers it
// constantly. A fresh slice per call, because the default is policy that a
// caller must not be able to edit for everyone.
func DefaultLabels() []string {
	return []string{"FEMALE_GENITALIA_EXPOSED", "MALE_GENITALIA_EXPOSED", "ANUS_EXPOSED"}
}

// Limits the validators enforce. They bound what one request can make
// gawk-admin store or compute, not what a healthy sampler sends.
const (
	// MaxRecent caps a content flag's `recent` (D14). gawk-admin refuses
	// to start with an auto-kill policy that names more samples.
	MaxRecent = 64
	// MaxLabels caps every label list and score map.
	MaxLabels = 32
	// MaxLabelLen caps one label name.
	MaxLabelLen = 64
	// MaxSamplerIDLen caps samplerId.
	MaxSamplerIDLen = 128
	// MaxSummaryLen caps the human summary.
	MaxSummaryLen = 512
	// MaxEpochLen caps a publisher epoch.
	MaxEpochLen = 64
	// MaxBroadcastKeyLen caps a broadcast key.
	MaxBroadcastKeyLen = 64
	// MaxWindow caps a window rule's size (and so a replay's).
	MaxWindow = 64
	// MaxShards caps -shard-count. Far beyond any fleet that needs it; it
	// exists so a typo cannot make gawk-admin track a million shards.
	MaxShards = 64
)

// ErrInvalid wraps every validation failure in this package, so a handler
// maps the whole class onto one 400 with errors.Is.
var ErrInvalid = errors.New("screening: invalid")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// Shard is the one broadcast-to-sampler assignment (D22): the share of
// broadcastKey among n samplers. It is deterministic, in [0, n) for every
// n ≥ 1, and depends only on the key — which never changes while a broadcast
// is live, so a broadcast stays on one sampler for its whole life. n < 1 is
// treated as 1.
//
// The key is hashed again rather than reduced directly: it is already an HMAC
// prefix, but the uniformity of the split should not depend on how some other
// component happens to derive its keys. The golden values in the tests pin
// the function, so a change to it is a visible break: changing it reshuffles
// every live broadcast between samplers.
func Shard(broadcastKey string, n int) int {
	if n <= 1 {
		return 0
	}
	sum := sha256.Sum256([]byte(broadcastKey))
	return int(binary.BigEndian.Uint64(sum[:8]) % uint64(n))
}

// ValidateShard checks a (shard, of) pair: 1 ≤ of ≤ MaxShards, 0 ≤ shard < of.
func ValidateShard(shard, of int) error {
	if of < 1 || of > MaxShards {
		return invalid("of %d, want 1-%d", of, MaxShards)
	}
	if shard < 0 || shard >= of {
		return invalid("shard %d, want 0-%d", shard, of-1)
	}
	return nil
}

// TargetsResponse is the body of GET /api/v1/screening/targets (D2, D22):
// one shard's live origin broadcasts.
type TargetsResponse struct {
	Schema string `json:"schema"`
	// Shard and Of echo the query, so a sampler can assert it got the share
	// it asked for.
	Shard int `json:"shard"`
	Of    int `json:"of"`
	// FlagRoute is false when no enabled webhook would deliver
	// content_flag.raised (D12). A sampler in flag mode samples nothing while
	// it is false; observe mode is unaffected.
	FlagRoute bool     `json:"flagRoute"`
	Targets   []Target `json:"targets"`
}

// Target is one broadcast to screen. Only origin broadcasts whose publisher
// is active are listed: a broadcast in its away grace has a frozen cached
// keyframe nobody is broadcasting (D2).
type Target struct {
	// BroadcastID is the raw, joinable ID: the keyframe route's path segment
	// and the content flag's subject.
	BroadcastID string `json:"broadcastId"`
	// BroadcastKey is the HMAC'd key; Shard is computed over it.
	BroadcastKey string `json:"broadcastKey"`
	// PublisherEpoch identifies the publisher session the listing saw. The
	// keyframe response's HeaderPublisherEpoch is authoritative for a
	// sample; this one lets the sampler drop a window early.
	PublisherEpoch string `json:"publisherEpoch"`
	// Relay is the base URL of the ops listener of the pod holding the
	// origin, e.g. "http://10.42.0.7:2112". The keyframe route lives there.
	Relay string `json:"relay"`
	// Muted is true while a dismissal mutes the broadcast (D10). It is still
	// sampled — observe-mode data and samples continue — but flag-mode POSTs
	// answer 202 muted.
	Muted bool `json:"muted"`
	// Exempt is true while the broadcast is attached to an exempt room
	// (D11). It is listed so the sampler's metrics see it, and never sampled.
	Exempt bool `json:"exempt"`
}
