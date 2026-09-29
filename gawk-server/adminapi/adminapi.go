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

// SchemaRooms names GET /internal/admin/rooms (R49, docs/50 D2).
const SchemaRooms = "gawk.admin.rooms.v1"

// Room is one row of GET /internal/admin/rooms: a room THIS POD IS HOME FOR,
// with the live roster and attachment state that exist nowhere else — the
// Room CR never holds the roster (docs/44 D5). Proxy rows are not listed: a
// proxying pod knows only a session count, and the home pod is the truth.
type Room struct {
	// Code is the RAW, normalized room code — a joinable secret (docs/44
	// D16), carried here on docs/42 D8's terms: ClusterIP-only and
	// credential-gated. It equals the Room CR's name.
	Code string `json:"code"`
	// Key is the HMAC'd handle /statusz and the metrics use.
	Key string `json:"key"`
	// Kind is "static" or "dynamic" (rooms.KindStatic / KindDynamic).
	Kind        string    `json:"kind"`
	DisplayCode string    `json:"displayCode"`
	DisplayName string    `json:"displayName,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	// EmptySince is set while the room has no participants and its empty
	// grace is running.
	EmptySince   *time.Time        `json:"emptySince,omitempty"`
	Attachments  []RoomAttachment  `json:"attachments"`
	Participants []RoomParticipant `json:"participants"`
}

// RoomAttachment is one broadcast attached to a room, in attach order.
type RoomAttachment struct {
	// BroadcastID is the RAW broadcast ID (docs/50 D5): what makes a watch
	// link work.
	BroadcastID string `json:"broadcastId"`
	Label       string `json:"label,omitempty"`
	// Live is false while the broadcaster is away (within its grace).
	Live       bool      `json:"live"`
	Viewers    int       `json:"viewers"`
	AttachedAt time.Time `json:"attachedAt"`
}

// RoomParticipant is one control session in a room.
type RoomParticipant struct {
	// ID is the per-room participant ID; a new home pod re-issues it.
	ID       int    `json:"id"`
	Nickname string `json:"nickname"`
	// ClientKind is the R51 contract's closed vocabulary
	// (events.ClientKind*): web-viewer, web-broadcaster or native.
	ClientKind string `json:"clientKind"`
	// Streaming is true while one of the room's attachments is this
	// participant's broadcast.
	Streaming bool `json:"streaming"`
	// Speaking is reserved for a voice bridge and false today.
	Speaking bool `json:"speaking"`
	// Identity is reserved for an authenticated identity and empty today
	// (docs/44 §4.11).
	Identity string `json:"identity,omitempty"`
}

// RoomsResponse is the body of GET /internal/admin/rooms.
type RoomsResponse struct {
	Schema string `json:"schema"`
	Pod    string `json:"pod"`
	Rooms  []Room `json:"rooms"`
}
