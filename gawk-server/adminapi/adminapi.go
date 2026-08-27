// Package adminapi is the wire contract of the relay's R39 admin API
// (docs/42 §4.5): the response types GET /internal/admin/broadcasts serves and
// gawk-admin's relayscan parses.
//
// It is PUBLIC (not internal/) for the same reason `wire` and `moderation`
// are: gawk-admin imports it through the repo-root `replace`, so both sides of
// the contract compile the same struct instead of hand-mirroring eleven JSON
// tags (CLAUDE.md: "reuse it; never mirror it"). A renamed or retyped field
// now breaks the consumer's build rather than silently parsing to a zero
// value on the moderation surface.
//
// The config response is deliberately NOT shared beyond its envelope: the
// portal renders the relay's sanitized config structurally (a map of knob
// names), so pinning the field set would break on every new relay flag.
package adminapi

import "time"

// Schema names. Versioned strings, not implied by the route, so a consumer
// pins the shape it parses (docs/42 §4.5) — the same discipline the telemetry
// ingest uses.
const (
	SchemaBroadcasts = "gawk.admin.broadcasts.v1"
	SchemaConfig     = "gawk.admin.config.v1"
)

// Broadcast is one row of GET /internal/admin/broadcasts — the
// gawk.admin.broadcasts.v1 schema's per-broadcast object.
type Broadcast struct {
	// ID is the RAW, joinable broadcast ID — the scoped relaxation of the
	// never-expose-raw-IDs invariant (docs/42 D8). The operator needs it to
	// join and judge a stream before killing it.
	ID string `json:"id"`
	// Key is ObfuscateID(ID): the same HMAC'd handle /statusz, the metrics
	// broadcast label and the telemetry UI's #/broadcast/<key> route use, so
	// the portal deep-links into telemetry without ever holding -stats-key.
	Key string `json:"key"`
	// Role is "origin" or "edge" (R17), the same vocabulary as /statusz.
	Role            string `json:"role"`
	PublisherActive bool   `json:"publisherActive"`
	// PublisherRemoteIP is filled in by the ops layer from the transport's
	// live-publisher bookkeeping; the hub has no view of session addresses.
	// Empty when there is no live publisher (or its address was unparseable).
	PublisherRemoteIP string `json:"publisherRemoteIp,omitempty"`
	// PublisherSessionID is the R28 telemetry session handle, omitted when
	// telemetry is off — the join into the broadcaster's own reports.
	PublisherSessionID string `json:"publisherSessionId,omitempty"`
	// StartedAt is when this pod registered the hub, not when the current
	// publisher session began: a broadcast that survived a reclaim keeps its
	// age.
	StartedAt time.Time `json:"startedAt"`
	// ViewersLocal counts WATCHING HUMANS on this pod: internal edge sessions
	// and R30 stripe legs are excluded, so one viewer with three legs is one
	// number here — the same rule ViewersGlobal follows (docs/35 §5.8).
	ViewersLocal int `json:"viewersLocal"`
	// ViewersGlobal is the fleet-wide count this pod computes as origin; 0 on
	// edge hubs, which receive the number from upstream (docs/23 Decision 9).
	ViewersGlobal         uint32 `json:"viewersGlobal"`
	GraceRemainingSeconds int    `json:"graceRemainingSeconds"`
	// DVRBytes is what this broadcast's R21 ring currently retains, 0 when no
	// ring was ever allocated.
	DVRBytes int `json:"dvrBytes"`
}

// BroadcastsResponse is the body of GET /internal/admin/broadcasts.
type BroadcastsResponse struct {
	Schema     string      `json:"schema"`
	Pod        string      `json:"pod"`
	Broadcasts []Broadcast `json:"broadcasts"`
}
