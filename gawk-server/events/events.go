// Package events is the event contract of a gawk deployment (R51, docs/52).
//
// Every event gawk emits — on the R50 NATS bus and in a webhook — is a
// CloudEvents 1.0 event in the JSON structured format (D1), whose `data` is
// described by one JSON Schema 2020-12 file per event type (D2, `schema/`),
// listed in one AsyncAPI 3.0 catalogue (D3, `asyncapi.yaml`). This package
// holds the envelope type, the Go form of every `data` shape, the type
// constants, the one encoder, and the embedded schema files, so that the relay
// (the bus producer), gawk-admin (the webhook producer, projector and server
// of the documents) and any third module import one definition rather than
// restating it — the `wire` posture (CLAUDE.md: reuse, never mirror).
//
// It is deliberately public, and deliberately small: no CloudEvents SDK, no
// validator, no protocol binding. Producers marshal typed structs; the tests
// in this package prove the structs match the schemas (D7), and the schemas
// are what a consumer codes against.
//
// The rules a new event type has to follow are in docs/52 D6; the tests here
// enforce them. Adding a type means, in one PR: a `Type*` constant here, its
// Data struct in data.go, `schema/<type>.json`, `testdata/vectors/<type>.json`
// plus its fixture in fixtures_test.go, and a message in `asyncapi.yaml` —
// under the `webhook` channel too if it may reach a webhook. `go test` names
// whatever is missing.
package events

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The CloudEvents attributes every event carries verbatim (D1).
const (
	// SpecVersion is the CloudEvents specification version.
	SpecVersion = "1.0"
	// ContentType is the media type of an event on the wire — the JSON
	// structured format — on both channels. It is the `Content-Type` of a
	// webhook delivery and the NATS `Content-Type` header.
	ContentType = "application/cloudevents+json"
	// DataContentType is the media type of `data`. Always JSON: `data` is a
	// JSON value inside the envelope, never a base64 blob.
	DataContentType = "application/json"

	// SourceAdmin is the `source` of every portal-originated event.
	SourceAdmin = "/gawk/admin"
	// sourceRelayPrefix + the pod name is the `source` of a relay-published
	// event; see SourceRelay.
	sourceRelayPrefix = "/gawk/relay/"

	// TypePrefix is the reverse-DNS namespace every type starts with (D6 a).
	TypePrefix = "fi.ioio.gawk."

	// SchemaIDBase is the `$id` prefix of every data schema, and the prefix
	// of every event's `dataschema` (D2). It is an IDENTIFIER, not a promise
	// of a URL: the schema resolves in this package, in the repository at
	// the release tag, and on every gawk-admin deployment at
	// /api/v1/schemas/events/<type>.json. Making the literal URL resolve on
	// the project site is deferred (docs/52 §2, Rejected).
	SchemaIDBase = "https://gawk.ioio.fi/schemas/events/"
)

// Event types. `fi.ioio.gawk.<scope>.<event>`; the NATS subject's last token
// is `<event>` (D6 a). One constant per schema file, per vector and per
// AsyncAPI message — the D7 tests hold the four sets equal.
//
// A type is never versioned until a `v2` exists (docs/52 §2, Rejected):
// additive change keeps the type, an incompatible change is a new
// `….v2` type beside the deprecated old one.
const (
	// Moderation events: portal-originated, one per moderation_events row
	// (R39, R42). Their `source` is SourceAdmin and their `id` is a UUIDv5
	// over the row (docs/52 D1). ModerationType maps the row's short type to
	// these.
	TypeBroadcastKilled   = TypePrefix + "broadcast.killed"
	TypeBanCreated        = TypePrefix + "ban.created"
	TypeBanExpired        = TypePrefix + "ban.expired"
	TypeBanRemoved        = TypePrefix + "ban.removed"
	TypeContentFlagRaised = TypePrefix + "content_flag.raised" // reserved for R40, never produced yet
	TypeRoomCreated       = TypePrefix + "room.created"
	TypeRoomEnded         = TypePrefix + "room.ended"
	TypeRoomSecretRotated = TypePrefix + "room.secret_rotated"

	// Bus events: relay-published from the fan-out points docs/51 D3 names
	// (R50 wires the producers; the types and schemas are fixed here first).
	// Their `source` is SourceRelay(pod) and their `id` is BusID(pod, seq).
	//
	// The room lifecycle types are `opened` / `closed`, deliberately NOT
	// `created` / `ended`: those are the moderation row types above, and one
	// string meaning two payload shapes is the docs/51 D3 finding this
	// package exists to prevent (D6 e).
	TypeBroadcastStarted       = TypePrefix + "broadcast.started"
	TypeBroadcastPublisherAway = TypePrefix + "broadcast.publisher_away"
	TypeBroadcastPublisherBack = TypePrefix + "broadcast.publisher_back"
	TypeBroadcastEnded         = TypePrefix + "broadcast.ended"
	TypeBroadcastViewers       = TypePrefix + "broadcast.viewers"
	TypeRoomOpened             = TypePrefix + "room.opened"
	TypeRoomClosed             = TypePrefix + "room.closed"
	TypeRoomAttached           = TypePrefix + "room.attached"
	TypeRoomDetached           = TypePrefix + "room.detached"
	TypeRoomAttachmentUpdated  = TypePrefix + "room.attachment_updated"
	TypeRoomParticipantJoined  = TypePrefix + "room.participant_joined"
	TypeRoomParticipantLeft    = TypePrefix + "room.participant_left"
	TypeRoomParticipantUpdated = TypePrefix + "room.participant_updated"

	// TypeWebhookTest is the synthetic event POST /webhooks/{name}/test
	// sends (docs/52 D5). Webhook-only: it is never on the bus and never a
	// stored row, because a test send is not a moderation action.
	TypeWebhookTest = TypePrefix + "webhook.test"
)

// Types returns every event type this contract defines, in catalogue order.
//
// A function returning a fresh slice rather than an exported slice variable:
// a package-level slice is writable by any importer, and this list is what
// the D7 tests hold the schema files, the vectors and the catalogue to.
func Types() []string {
	return []string{
		TypeBroadcastKilled,
		TypeBanCreated,
		TypeBanExpired,
		TypeBanRemoved,
		TypeContentFlagRaised,
		TypeRoomCreated,
		TypeRoomEnded,
		TypeRoomSecretRotated,

		TypeBroadcastStarted,
		TypeBroadcastPublisherAway,
		TypeBroadcastPublisherBack,
		TypeBroadcastEnded,
		TypeBroadcastViewers,
		TypeRoomOpened,
		TypeRoomClosed,
		TypeRoomAttached,
		TypeRoomDetached,
		TypeRoomAttachmentUpdated,
		TypeRoomParticipantJoined,
		TypeRoomParticipantLeft,
		TypeRoomParticipantUpdated,

		TypeWebhookTest,
	}
}

// IsType reports whether t is a type this contract defines.
func IsType(t string) bool {
	for _, known := range Types() {
		if known == t {
			return true
		}
	}
	return false
}

// moderationTypes is the one row-type → CloudEvents-type table (docs/52 §3):
// gawk-admin's moderation_events rows keep their short internal names
// (`broadcast.killed`, `room.ended`) and every one of them maps to exactly
// one type here. The keys are gawk-admin's store.Event* strings, restated
// here because this module cannot import that one; gawk-admin's own D7 test
// holds store.AllEventTypes() equal to this table's keys.
var moderationTypes = map[string]string{
	"broadcast.killed":    TypeBroadcastKilled,
	"ban.created":         TypeBanCreated,
	"ban.expired":         TypeBanExpired,
	"ban.removed":         TypeBanRemoved,
	"content_flag.raised": TypeContentFlagRaised,
	"room.created":        TypeRoomCreated,
	"room.ended":          TypeRoomEnded,
	"room.secret_rotated": TypeRoomSecretRotated,
}

// ModerationType maps a moderation_events row type to its CloudEvents type.
// ok is false for a row type this contract does not know, which a producer
// must treat as a bug rather than invent a type for.
func ModerationType(rowType string) (string, bool) {
	t, ok := moderationTypes[rowType]
	return t, ok
}

// ModerationRowTypes returns every row type ModerationType knows, sorted.
func ModerationRowTypes() []string {
	out := make([]string, 0, len(moderationTypes))
	for k := range moderationTypes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Scope returns the `<scope>` segment of a type (`broadcast`, `room`, …), or
// "" when t is not shaped like one of ours.
func Scope(t string) string {
	scope, _, ok := split(t)
	if !ok {
		return ""
	}
	return scope
}

// Name returns the `<event>` segment of a type — the NATS subject's last
// token (D6 a) — or "" when t is not shaped like one of ours. A versioned
// type keeps its suffix: `participant_joined.v2`.
func Name(t string) string {
	_, name, ok := split(t)
	if !ok {
		return ""
	}
	return name
}

func split(t string) (scope, name string, ok bool) {
	rest, found := strings.CutPrefix(t, TypePrefix)
	if !found {
		return "", "", false
	}
	scope, name, found = strings.Cut(rest, ".")
	if !found || scope == "" || name == "" {
		return "", "", false
	}
	return scope, name, true
}

// SchemaID is the `$id` of a type's data schema and the `dataschema` of every
// event of that type (D2).
func SchemaID(t string) string {
	return SchemaIDBase + SchemaFile(t)
}

// SchemaFile is the file name of a type's data schema inside Schemas and the
// last path segment of its `$id` and of gawk-admin's serving route.
func SchemaFile(t string) string {
	return t + ".json"
}

// SourceRelay is the `source` of an event published by the named relay pod.
func SourceRelay(pod string) string {
	return sourceRelayPrefix + pod
}

// BusID is the `id` of a relay-published event: `<pod>:<seq>` (docs/51 D3),
// the JetStream dedup key and gawk-admin's forever dedup key. A per-pod
// monotonic seq needs no coordination; the random start offset docs/51
// requires is the producer's job, not this function's.
func BusID(pod string, seq uint64) string {
	return pod + ":" + strconv.FormatUint(seq, 10)
}

// Event is the CloudEvents 1.0 envelope (D1), in the attribute order the JSON
// format's examples use. It has no extension attributes, on purpose: an
// extension is a forever commitment; anything an event has to say belongs in
// Data, under a schema.
type Event struct {
	SpecVersion string `json:"specversion"`
	// ID is unique per Source: BusID on the bus, a UUIDv5 over the row for
	// a portal-originated event, a fresh UUID for a test send. A re-sent
	// duplicate — a webhook retry — carries the SAME id, which is what makes
	// it a receiver's idempotency key.
	ID     string `json:"id"`
	Source string `json:"source"`
	Type   string `json:"type"`
	// Subject is the fleet's HMAC'd key of the broadcast or room the event
	// is about — the one identity that may appear anywhere — and absent for
	// an event that is about neither.
	Subject string `json:"subject,omitempty"`
	// Time is when the occurrence happened. Marshal renders it in UTC.
	Time            time.Time `json:"time"`
	DataContentType string    `json:"datacontenttype"`
	DataSchema      string    `json:"dataschema"`
	// Data is the typed payload: one of the structs in data.go, or — after
	// gawk-admin's webhook projection — a map of the surviving properties.
	Data any `json:"data"`
}

// New fills the attributes that follow from the type — specversion,
// datacontenttype, dataschema — around the ones the producer supplies.
func New(typ, id, source, subject string, at time.Time, data any) Event {
	return Event{
		SpecVersion:     SpecVersion,
		ID:              id,
		Source:          source,
		Type:            typ,
		Subject:         subject,
		Time:            at,
		DataContentType: DataContentType,
		DataSchema:      SchemaID(typ),
		Data:            data,
	}
}

// Marshal renders an event to the exact bytes that go on the wire — and, for
// a webhook, the exact bytes that are signed.
//
// It is the one encoder. HTML escaping is off: `summary`, `reason`,
// `nickname` and `label` are human text that ends up in a push notification
// or a dashboard, and a consumer forwarding a field verbatim should show
// "spam & abuse" rather than the default encoder's "spam & abuse". The
// encoder's trailing newline is stripped so the signed material is the JSON
// value itself. Time is normalised to UTC so two producers render one instant
// one way.
func Marshal(ev Event) ([]byte, error) {
	ev.Time = ev.Time.UTC()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ev); err != nil {
		return nil, fmt.Errorf("events: marshal %s: %w", ev.Type, err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Schemas holds every data schema (`schema/<type>.json`) and the shared
// definitions they `$ref` (`schema/common.json`). gawk-admin serves them; the
// D7 tests validate every vector against them.
//
//go:embed schema/*.json
var Schemas embed.FS

// SchemaDir is the directory inside Schemas.
const SchemaDir = "schema"

// CommonSchemaFile is the shared-definitions file every data schema `$ref`s.
const CommonSchemaFile = "common.json"

// AsyncAPI is the catalogue: every channel, every message, and the rules a
// consumer reads (D3). gawk-admin serves it bundled at /api/v1/asyncapi.json.
//
//go:embed asyncapi.yaml
var AsyncAPI []byte

// CloudEventsSchema is the CloudEvents project's own JSON Schema for the JSON
// format's envelope (cloudevents/spec, `cloudevents/formats/cloudevents.json`
// at commit 7bd28f7, 2021-11-04, Apache-2.0), vendored so the D7 tests can say
// "this is a CloudEvent" rather than "this looks like one". Not used by any
// production path.
//
//go:embed cloudevents.json
var CloudEventsSchema []byte

// Vectors holds the golden vectors (`testdata/vectors/<type>.json`): one
// fully-populated example event per type, with fictional identifiers. They
// are the examples a consumer can test a parser against, and what
// gawk-admin's D7 tests project and validate; the tests in this package hold
// each one byte-identical to its Go fixture.
//
//go:embed testdata/vectors/*.json
var Vectors embed.FS

// VectorDir is the directory inside Vectors.
const VectorDir = "testdata/vectors"

// Vector returns the golden vector of a type.
func Vector(t string) ([]byte, error) {
	return fs.ReadFile(Vectors, VectorDir+"/"+t+".json")
}

// Schema returns the data schema of a type, or an error for a type that has
// none — which for a type in Types() is a broken build the D7 tests catch.
func Schema(t string) ([]byte, error) {
	return fs.ReadFile(Schemas, SchemaDir+"/"+SchemaFile(t))
}

// SchemaByFile returns an embedded schema by its file name (`<type>.json` or
// CommonSchemaFile). ok is false for a name that is not a schema — the answer
// gawk-admin's serving route turns into a 404.
func SchemaByFile(name string) (data []byte, ok bool) {
	if strings.Contains(name, "/") || !strings.HasSuffix(name, ".json") {
		return nil, false
	}
	data, err := fs.ReadFile(Schemas, SchemaDir+"/"+name)
	if err != nil {
		return nil, false
	}
	return data, true
}

// SchemaFiles lists every embedded schema file name, sorted.
func SchemaFiles() ([]string, error) {
	entries, err := fs.ReadDir(Schemas, SchemaDir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// SensitiveProperties returns the `data` property names a type's schema marks
// `x-gawk-sensitive: true` (D4): the ones that may carry a raw broadcast ID
// or a room code, which a bus event carries and a webhook delivery must not.
//
// The marks are read from the schema file — the document a consumer reads —
// rather than from a Go list, so a sensitive property added without its mark
// is caught by the fixture test (it would leak), and one marked without a Go
// change is stripped from the day it is marked.
func SensitiveProperties(t string) ([]string, error) {
	props, err := properties(t)
	if err != nil {
		return nil, err
	}
	var out []string
	for name, p := range props {
		if v, _ := p["x-gawk-sensitive"].(bool); v {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// properties decodes a type's schema and returns its top-level `properties`.
// Marks live there — on the property declaration, beside its `$ref` — never
// inside common.json, so no reference has to be followed to find them.
func properties(t string) (map[string]map[string]any, error) {
	raw, err := Schema(t)
	if err != nil {
		return nil, fmt.Errorf("events: no schema for %q: %w", t, err)
	}
	var doc struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("events: schema for %q: %w", t, err)
	}
	return doc.Properties, nil
}
