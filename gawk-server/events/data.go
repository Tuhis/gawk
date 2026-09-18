package events

// The `data` of every event type, one struct per type (docs/52 D2), each a
// mirror of `schema/<type>.json`. The D7 tests hold the two together: every
// vector must both validate against the schema and be the byte-exact
// marshalling of the fixture built from the struct.
//
// Field tags are the contract's property names. A property that may carry a
// raw broadcast ID or a room code is marked `x-gawk-sensitive: true` in the
// schema — it is present on the bus (internal infrastructure, docs/51 D6) and
// stripped from every webhook delivery (D4). Nothing here ever carries an IP.
//
// Two conventions:
//
//   - `omitempty` on every string that is optional in the schema, so an
//     absent value is absent rather than "". Booleans and counters that the
//     schema requires carry no `omitempty`: a false or a zero is a value.
//   - Delivery is embedded LAST in every struct. Its two properties are the
//     ones gawk-admin adds on delivery (D4) and the relay never sets, so the
//     schemas declare them optional through common.json and they marshal
//     after the type's own properties.

// Delivery holds the two delivery-added properties every data schema declares
// optional (D4): a producer leaves them empty; gawk-admin fills them when it
// posts a webhook.
type Delivery struct {
	// Summary is the one human sentence docs/42 §4.10 promises every
	// receiver, so a webhook-to-push bridge (ntfy) needs no templating.
	Summary string `json:"summary,omitempty"`
	// PortalURL is the deep link into the portal — filtered by the HMAC'd
	// key, never by a raw ID. A notification carries no capability; acting
	// requires logging in.
	PortalURL string `json:"portalUrl,omitempty"`
}

// Closed vocabularies. Consumers are told (asyncapi.yaml) to treat an unknown
// value as unknown, because a vocabulary may grow additively (D6 b).
const (
	// EnforcementPending is the only enforcement value: present when the
	// Kubernetes object that would MAKE a moderation event true had not been
	// written when the event was recorded, absent when record and
	// enforcement agree (gawk-admin's store.EnforcementState).
	EnforcementPending = "pending"

	// Room kinds.
	RoomKindStatic  = "static"
	RoomKindDynamic = "dynamic"

	// Client kinds — the JSON spelling of wire.RoomClient*.
	ClientKindWebViewer      = "web-viewer"
	ClientKindWebBroadcaster = "web-broadcaster"
	ClientKindNative         = "native"

	// Broadcast roles — the R17 vocabulary /statusz uses.
	RoleOrigin = "origin"
	RoleEdge   = "edge"

	// Why a broadcast ended.
	BroadcastEndedGC       = "gc"
	BroadcastEndedKilled   = "killed"
	BroadcastEndedReplaced = "replaced"

	// Why a room closed.
	RoomClosedGrace    = "grace"
	RoomClosedCreator  = "creator"
	RoomClosedOperator = "operator"

	// Why a participant left. Only the first is somebody leaving: the other
	// three are the room happening TO them, and a consumer that announces
	// departures wants to tell them apart.
	ParticipantLeft = "left"
	// ParticipantLeftTimeout: the control session stopped keeping up and was
	// evicted (wire close 4001, non-terminal — a fresh session restores it).
	ParticipantLeftTimeout = "timeout"
	// ParticipantLeftRoomEnded: the room ended under them. It follows a
	// room.closed for the same room.
	ParticipantLeftRoomEnded = "room_ended"
	// ParticipantLeftHomeMoved: the room moved to another pod and they are
	// reconnecting there. It follows a room.home_changed published by the NEW
	// home, and the joins that follow carry rejoin: true — nobody left.
	ParticipantLeftHomeMoved = "home_moved"
)

// ---------------------------------------------------------------------------
// Moderation events (portal-originated; one per moderation_events row)
// ---------------------------------------------------------------------------

// BroadcastKilledData is `fi.ioio.gawk.broadcast.killed`.
type BroadcastKilledData struct {
	// Actor is who did it: an operator's identity, or `system`.
	Actor string `json:"actor"`
	// BroadcastKey is the HMAC'd key of the broadcast.
	BroadcastKey string `json:"broadcastKey,omitempty"`
	// BroadcastID is the raw, joinable ID. Sensitive: bus only.
	BroadcastID string `json:"broadcastId,omitempty"`
	// Reason is the operator's free text.
	Reason string `json:"reason,omitempty"`
	// Enforcement is EnforcementPending or absent.
	Enforcement string `json:"enforcement,omitempty"`
	Delivery
}

// BanCreatedData is `fi.ioio.gawk.ban.created`. A ban on a publisher IP has
// no broadcast identity at all; the IP is never in any event.
type BanCreatedData struct {
	Actor        string `json:"actor"`
	BroadcastKey string `json:"broadcastKey,omitempty"`
	BroadcastID  string `json:"broadcastId,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Enforcement  string `json:"enforcement,omitempty"`
	Delivery
}

// BanExpiredData is `fi.ioio.gawk.ban.expired`.
type BanExpiredData struct {
	Actor        string `json:"actor"`
	BroadcastKey string `json:"broadcastKey,omitempty"`
	BroadcastID  string `json:"broadcastId,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Enforcement  string `json:"enforcement,omitempty"`
	Delivery
}

// BanRemovedData is `fi.ioio.gawk.ban.removed`.
type BanRemovedData struct {
	Actor        string `json:"actor"`
	BroadcastKey string `json:"broadcastKey,omitempty"`
	BroadcastID  string `json:"broadcastId,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Enforcement  string `json:"enforcement,omitempty"`
	Delivery
}

// ContentFlagRaisedData is `fi.ioio.gawk.content_flag.raised` — reserved for
// R40 (docs/42 §4.11), fixed here so the vocabulary exists before anything
// produces it.
type ContentFlagRaisedData struct {
	Actor        string `json:"actor"`
	BroadcastKey string `json:"broadcastKey,omitempty"`
	BroadcastID  string `json:"broadcastId,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Delivery
}

// RoomCreatedData is `fi.ioio.gawk.room.created`: an operator created a
// static room in the portal.
type RoomCreatedData struct {
	Actor string `json:"actor"`
	// RoomKey is the fleet's HMAC'd handle for the room (docs/44 D16).
	// Absent until a pod has homed the room.
	RoomKey string `json:"roomKey,omitempty"`
	// RoomCode is the joinable code. Sensitive: bus only.
	RoomCode string `json:"roomCode,omitempty"`
	// Kind is RoomKindStatic or RoomKindDynamic.
	Kind string `json:"kind,omitempty"`
	Delivery
}

// RoomEndedData is `fi.ioio.gawk.room.ended`: a room ended, by an operator
// (Actor is their identity) or by the relay on its own (Actor `system`, and
// Reason says why once R50's bus feeds this row).
type RoomEndedData struct {
	Actor    string `json:"actor"`
	RoomKey  string `json:"roomKey,omitempty"`
	RoomCode string `json:"roomCode,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Delivery
}

// RoomSecretRotatedData is `fi.ioio.gawk.room.secret_rotated`.
type RoomSecretRotatedData struct {
	Actor    string `json:"actor"`
	RoomKey  string `json:"roomKey,omitempty"`
	RoomCode string `json:"roomCode,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Delivery
}

// ---------------------------------------------------------------------------
// Bus events (relay-published; docs/51 D3)
// ---------------------------------------------------------------------------

// BroadcastStartedData is `fi.ioio.gawk.broadcast.started`.
type BroadcastStartedData struct {
	BroadcastID  string `json:"broadcastId,omitempty"`
	BroadcastKey string `json:"broadcastKey"`
	// Role is RoleOrigin or RoleEdge — which side of the R17 cascade this
	// pod plays for the broadcast.
	Role string `json:"role"`
	// StartedAt is RFC 3339 UTC.
	StartedAt string `json:"startedAt"`
	Delivery
}

// BroadcastPublisherAwayData is `fi.ioio.gawk.broadcast.publisher_away`: the
// publisher stalled and viewers are being held on the keepalive.
type BroadcastPublisherAwayData struct {
	BroadcastID  string `json:"broadcastId,omitempty"`
	BroadcastKey string `json:"broadcastKey"`
	Delivery
}

// BroadcastPublisherBackData is `fi.ioio.gawk.broadcast.publisher_back`.
type BroadcastPublisherBackData struct {
	BroadcastID  string `json:"broadcastId,omitempty"`
	BroadcastKey string `json:"broadcastKey"`
	Delivery
}

// BroadcastEndedData is `fi.ioio.gawk.broadcast.ended`.
type BroadcastEndedData struct {
	BroadcastID  string `json:"broadcastId,omitempty"`
	BroadcastKey string `json:"broadcastKey"`
	// Reason is BroadcastEndedGC, BroadcastEndedKilled or
	// BroadcastEndedReplaced.
	Reason string `json:"reason"`
	Delivery
}

// BroadcastViewersData is `fi.ioio.gawk.broadcast.viewers`, coalesced to at
// most one per broadcast per interval and only on change. Published by both
// roles: the origin knows the global count, each edge its own local one.
type BroadcastViewersData struct {
	BroadcastID   string `json:"broadcastId,omitempty"`
	BroadcastKey  string `json:"broadcastKey"`
	Role          string `json:"role"`
	ViewersLocal  int    `json:"viewersLocal"`
	ViewersGlobal int    `json:"viewersGlobal"`
	Delivery
}

// RoomOpenedData is `fi.ioio.gawk.room.opened`: the home pod took a room
// live — a dynamic room's birth, or a static room's first attach.
type RoomOpenedData struct {
	RoomCode string `json:"roomCode,omitempty"`
	RoomKey  string `json:"roomKey"`
	Kind     string `json:"kind"`
	// DisplayCode is the code as shown to participants (a static room's
	// slug). Sensitive: it is joinable too.
	DisplayCode string `json:"displayCode,omitempty"`
	CreatedAt   string `json:"createdAt"`
	Delivery
}

// RoomClosedData is `fi.ioio.gawk.room.closed`.
type RoomClosedData struct {
	RoomCode string `json:"roomCode,omitempty"`
	RoomKey  string `json:"roomKey"`
	// Kind is RoomKindStatic or RoomKindDynamic, as room.opened carries it.
	// gawk-admin's room.ended row has always named the kind, and the sentence
	// it renders says it ("a dynamic room ended"), so an end that did not
	// carry it would read worse than the poll it replaced.
	Kind string `json:"kind,omitempty"`
	// Reason is RoomClosedGrace, RoomClosedCreator or RoomClosedOperator.
	Reason string `json:"reason"`
	Delivery
}

// RoomAttachedData is `fi.ioio.gawk.room.attached`: a broadcast joined a
// room's tiles.
type RoomAttachedData struct {
	RoomCode     string `json:"roomCode,omitempty"`
	RoomKey      string `json:"roomKey"`
	BroadcastID  string `json:"broadcastId,omitempty"`
	BroadcastKey string `json:"broadcastKey"`
	// Label is the tile's free text.
	Label string `json:"label,omitempty"`
	Delivery
}

// RoomDetachedData is `fi.ioio.gawk.room.detached`.
type RoomDetachedData struct {
	RoomCode     string `json:"roomCode,omitempty"`
	RoomKey      string `json:"roomKey"`
	BroadcastID  string `json:"broadcastId,omitempty"`
	BroadcastKey string `json:"broadcastKey"`
	Label        string `json:"label,omitempty"`
	Delivery
}

// RoomAttachmentUpdatedData is `fi.ioio.gawk.room.attachment_updated`,
// coalesced like BroadcastViewersData.
type RoomAttachmentUpdatedData struct {
	RoomCode     string `json:"roomCode,omitempty"`
	RoomKey      string `json:"roomKey"`
	BroadcastID  string `json:"broadcastId,omitempty"`
	BroadcastKey string `json:"broadcastKey"`
	Live         bool   `json:"live"`
	Viewers      int    `json:"viewers"`
	Delivery
}

// RoomParticipantJoinedData is `fi.ioio.gawk.room.participant_joined`.
type RoomParticipantJoinedData struct {
	RoomCode string `json:"roomCode,omitempty"`
	RoomKey  string `json:"roomKey"`
	// ParticipantID is the room-scoped id the roster uses (wire
	// Participant.id). Not a join capability.
	ParticipantID int    `json:"participantId"`
	Nickname      string `json:"nickname,omitempty"`
	ClientKind    string `json:"clientKind"`
	Streaming     bool   `json:"streaming"`
	Speaking      bool   `json:"speaking"`
	// Rejoin is true when the same participant resumed within the rejoin
	// window rather than arriving fresh (docs/50 D6's filter token).
	Rejoin bool `json:"rejoin"`
	Delivery
}

// RoomParticipantLeftData is `fi.ioio.gawk.room.participant_left`.
type RoomParticipantLeftData struct {
	RoomCode      string `json:"roomCode,omitempty"`
	RoomKey       string `json:"roomKey"`
	ParticipantID int    `json:"participantId"`
	Nickname      string `json:"nickname,omitempty"`
	ClientKind    string `json:"clientKind"`
	// Reason is one of the ParticipantLeft* values. Absent means the plain
	// one: treat an unknown value as unknown, and an absent one as `left`.
	//
	// It exists because three of the four are not departures at all — the
	// room ended, the room moved, the session was evicted — and a consumer
	// that announced "tuhis left" for a pod rollout would be lying.
	Reason string `json:"reason,omitempty"`
	Delivery
}

// RoomHomeChangedData is `fi.ioio.gawk.room.home_changed`: this pod adopted a
// room that another pod was serving (docs/44 §4.5's re-home).
//
// It is published by the NEW home, so the CloudEvent's `source` names it — and
// that is deliberately the only place the new pod's name appears, because a
// second copy in `data` could disagree with it. PreviousPod names the other
// end when the CR still records it.
//
// The new home is the one publisher that can be relied on here: a pod that
// loses a room because it is being deleted may publish nothing at all, so a
// consumer tracking where a room lives should follow this event rather than
// the departures on the old side.
type RoomHomeChangedData struct {
	RoomCode string `json:"roomCode,omitempty"`
	RoomKey  string `json:"roomKey"`
	Kind     string `json:"kind,omitempty"`
	// PreviousPod is the pod that held the home lease before this one, as the
	// Room CR recorded it. Absent when the lease was already released or the
	// record is gone.
	PreviousPod string `json:"previousPod,omitempty"`
	Delivery
}

// RoomParticipantUpdatedData is `fi.ioio.gawk.room.participant_updated`: a
// nickname, streaming or speaking change.
type RoomParticipantUpdatedData struct {
	RoomCode      string `json:"roomCode,omitempty"`
	RoomKey       string `json:"roomKey"`
	ParticipantID int    `json:"participantId"`
	Nickname      string `json:"nickname,omitempty"`
	ClientKind    string `json:"clientKind"`
	Streaming     bool   `json:"streaming"`
	Speaking      bool   `json:"speaking"`
	Delivery
}

// ---------------------------------------------------------------------------
// Delivery-only
// ---------------------------------------------------------------------------

// WebhookTestData is `fi.ioio.gawk.webhook.test`: nothing but the two
// delivery properties, because a test has nothing to say about any broadcast
// and must not invent any.
type WebhookTestData struct {
	Delivery
}
