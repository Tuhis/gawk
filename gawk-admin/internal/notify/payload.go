package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"

	"github.com/Tuhis/gawk/gawk-server/events"
)

// The wire contract of a webhook delivery: Standard Webhooks headers around a
// CloudEvents body (docs/52 D1, D5). These are the exact strings a receiver
// matches on, so they are constants here and nowhere else.
const (
	// HeaderID carries the CloudEvents `id`, repeated on every retry of the
	// same event: the receiver's idempotency key.
	HeaderID = "webhook-id"
	// HeaderTimestamp is Unix seconds at send time.
	HeaderTimestamp = "webhook-timestamp"
	// HeaderSignature is `v1,<base64>` — see Sign.
	HeaderSignature = "webhook-signature"

	// ContentType is the body's media type: a CloudEvent in JSON structured
	// format.
	ContentType = events.ContentType

	// SignatureVersion labels the scheme inside webhook-signature. The
	// header format permits space-separated multiple signatures, which is
	// where a key rotation would go; nothing here sends more than one.
	SignatureVersion = "v1"
)

// testSummary is the one sentence a test delivery carries. A test send exists
// to prove the pipe end to end — including that the receiver renders
// `summary` — so it must read like a real notification on a phone, not like
// an empty probe.
const testSummary = "test notification from the gawk-admin portal: this webhook is configured correctly"

// eventNamespace derives the CloudEvents `id` of a portal-originated event
// from its moderation_events row id (docs/52 D1).
//
// Deriving rather than generating makes the id STABLE across retries and
// across receivers: the same event announces the same id to every webhook on
// every attempt, so a receiver that already acted on attempt 2 can ignore
// attempt 3 after its own 200 was lost, and two receivers can agree they saw
// one event. A fresh UUID per attempt would make every retry look like a new
// page.
var eventNamespace = uuid.MustParse("5b3f0c2e-9a7d-4f16-8e21-c4d0a6b7e8f9")

// EventID is the CloudEvents `id` — and the webhook-id — of the event
// recorded in the given moderation_events row.
func EventID(rowID int64) string {
	return uuid.NewSHA1(eventNamespace, []byte(strconv.FormatInt(rowID, 10))).String()
}

// portalPath is the route every broadcast-scoped delivery's portalUrl points
// at (`ui/src/router/router.ts` pins `#/broadcasts` as the default route
// precisely so a cold click from a push notification lands somewhere real).
//
// An event that names a broadcast appends `?key=<broadcastKey>` — the HMAC'd
// key, never the raw ID (docs/42 D8) — which the portal reads as a pre-filled
// filter, so a paged operator lands ON the offending row instead of visually
// matching a 12-hex key against a fleet-sized table.
const portalPath = "/#/broadcasts"

// roomsPortalPath is the deep link for a room event (R42): the rooms view,
// filtered by the HMAC'd room key the same way broadcasts are by theirs.
const roomsPortalPath = "/#/rooms"

func portalURL(externalURL, broadcastKey string) string {
	return deepLink(externalURL, portalPath, broadcastKey)
}

func deepLink(externalURL, path, key string) string {
	if externalURL == "" {
		return ""
	}
	if key == "" {
		return externalURL + path
	}
	return externalURL + path + "?key=" + url.QueryEscape(key)
}

// isRoomEvent reports whether a row type belongs to the room vocabulary
// (store.EventRoom*), which deep-links to the rooms view.
func isRoomEvent(rowType string) bool {
	return strings.HasPrefix(rowType, "room.")
}

// buildEvent renders a persisted moderation event as the CloudEvent a
// webhook receives: the envelope of docs/52 D1 around the D4 projection of
// the type's data.
//
// It is the one place a moderation_events row becomes an event. The typed
// data carries the raw broadcast ID or room code the row holds, exactly as the
// relay puts them on the bus, and since docs/52 D9 the projection removes
// nothing — the raw identifiers are delivered on purpose. What keeps
// everything ELSE in the row out of a delivery is that this function builds
// typed structs from named fields only: the jsonb may hold addresses and
// CIDRs, and nothing here reads it except through the accessors store closes
// the vocabulary of (EnforcementState, RoomKey, TargetType) and the named
// PayloadString keys.
func buildEvent(ev store.Event, externalURL string) (events.Event, error) {
	typ, ok := events.ModerationType(ev.Type)
	if !ok {
		// A row type the contract does not know is a bug in whichever
		// producer wrote it — never something to invent a type for.
		return events.Event{}, fmt.Errorf("notify: no event type for row type %q", ev.Type)
	}
	// Read through the accessor, never as a raw string: it is what closes the
	// vocabulary, so this field can only ever be "" or "pending" whatever a
	// producer wrote into the payload.
	enforcement := string(ev.EnforcementState())
	summary := ev.PayloadString(store.PayloadSummary)
	if summary == "" {
		// Every event written by internal/api and internal/kube carries a
		// summary already. This fallback exists so an event from some future
		// producer still satisfies "summary present on every delivery" — and
		// it calls the ONE summariser (store.SummarizeWithEnforcement) rather
		// than growing a second one that could drift from it. The target TYPE
		// comes from the row (never its value, which may be an address), so an
		// IP ban is not announced as a ban on the broadcast it came from.
		summary = store.SummarizeWithEnforcement(ev.Type, ev.TargetType(), ev.BroadcastID, ev.Actor, ev.EnforcementState())
	}
	reason := ev.PayloadString(store.PayloadReason)

	var (
		data     any
		subject  string
		delivery events.Delivery
	)
	if isRoomEvent(ev.Type) {
		// Through the accessor, never as a raw string: it is what closes the
		// vocabulary to a hex digest, so a room CODE written under the key by
		// mistake does not end up in the portal link.
		roomKey := ev.RoomKey()
		roomCode := ev.PayloadString(store.PayloadRoom)
		displayCode := ev.PayloadString(store.PayloadDisplayCode)
		kind := ev.PayloadString(store.PayloadRoomKind)
		// The subject is the cleartext code (docs/52 D9); the key stays in
		// `data` and in the portal link, which is keyed by it.
		subject = roomCode
		delivery = events.Delivery{Summary: summary, PortalURL: deepLink(externalURL, roomsPortalPath, roomKey)}
		switch typ {
		case events.TypeRoomCreated:
			data = events.RoomCreatedData{Actor: ev.Actor, RoomKey: roomKey, RoomCode: roomCode, DisplayCode: displayCode, Kind: kind}
		case events.TypeRoomEnded:
			data = events.RoomEndedData{Actor: ev.Actor, RoomKey: roomKey, RoomCode: roomCode, DisplayCode: displayCode, Kind: kind, Reason: reason}
		case events.TypeRoomSecretRotated:
			data = events.RoomSecretRotatedData{Actor: ev.Actor, RoomKey: roomKey, RoomCode: roomCode, DisplayCode: displayCode, Kind: kind}
		default:
			return events.Event{}, fmt.Errorf("notify: no data shape for %s", typ)
		}
	} else {
		subject = ev.BroadcastID
		delivery = events.Delivery{Summary: summary, PortalURL: portalURL(externalURL, ev.BroadcastKey)}
		switch typ {
		case events.TypeBroadcastKilled:
			data = events.BroadcastKilledData{Actor: ev.Actor, BroadcastKey: ev.BroadcastKey, BroadcastID: ev.BroadcastID, Reason: reason, Enforcement: enforcement}
		case events.TypeBanCreated:
			data = events.BanCreatedData{Actor: ev.Actor, BroadcastKey: ev.BroadcastKey, BroadcastID: ev.BroadcastID, Reason: reason, Enforcement: enforcement}
		case events.TypeBanExpired:
			data = events.BanExpiredData{Actor: ev.Actor, BroadcastKey: ev.BroadcastKey, BroadcastID: ev.BroadcastID, Reason: reason, Enforcement: enforcement}
		case events.TypeBanRemoved:
			data = events.BanRemovedData{Actor: ev.Actor, BroadcastKey: ev.BroadcastKey, BroadcastID: ev.BroadcastID, Reason: reason, Enforcement: enforcement}
		case events.TypeContentFlagRaised:
			data = events.ContentFlagRaisedData{Actor: ev.Actor, BroadcastKey: ev.BroadcastKey, BroadcastID: ev.BroadcastID, Reason: reason}
		default:
			return events.Event{}, fmt.Errorf("notify: no data shape for %s", typ)
		}
	}
	full := events.New(typ, EventID(ev.ID), events.SourceAdmin, subject, ev.OccurredAt.UTC(), data)
	return project(full, delivery)
}

// project is the webhook projection of docs/52 D4 as D9 left it: the same
// event with the two delivery-added properties filled in, and nothing
// removed. Same `id`, `source`, `type`, `subject`, `time` and `dataschema` —
// a webhook delivery of a bus event is the same event to a consumer that sees
// both, and since D9 that holds for `data` as well.
//
// It is still a step of its own rather than a struct literal at each call
// site: `summary` and `portalUrl` are what gawk-admin adds as the delivering
// intermediary, and they are added in exactly one place. R49 delivers bus
// events through this same function.
//
// What it no longer does is strip the properties the schemas mark
// `x-gawk-sensitive`. A receiver is a place the operator chose to send
// joinable identifiers; the mark now warns them of that rather than naming a
// filter (docs/52 D9).
func project(full events.Event, delivery events.Delivery) (events.Event, error) {
	// Through JSON rather than reflection: the delivery-added properties are
	// JSON property names, and this is the one representation in which they
	// are exactly the keys.
	raw, err := json.Marshal(full.Data)
	if err != nil {
		return events.Event{}, fmt.Errorf("notify: encoding %s data: %w", full.Type, err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return events.Event{}, fmt.Errorf("notify: %s data is not an object: %w", full.Type, err)
	}
	if delivery.Summary != "" {
		data["summary"] = delivery.Summary
	}
	if delivery.PortalURL != "" {
		data["portalUrl"] = delivery.PortalURL
	}
	projected := full
	projected.Data = data
	return projected, nil
}

// testEvent is the synthetic event of a test send: `fi.ioio.gawk.webhook.test`
// from `/gawk/admin`, with a fresh id (a test is not a moderation action and
// has no row to derive one from), about no broadcast and no room.
func testEvent(now time.Time, externalURL string) events.Event {
	return events.New(events.TypeWebhookTest, uuid.New().String(), events.SourceAdmin, "", now.UTC(),
		events.WebhookTestData{Delivery: events.Delivery{
			Summary:   testSummary,
			PortalURL: portalURL(externalURL, ""),
		}})
}

// Sign returns the webhook-signature value for a delivery: the Standard
// Webhooks construction, `v1,` + base64(HMAC-SHA256(key, id + "." +
// timestamp + "." + body)).
//
// **The id and the timestamp are inside the signed material, not merely
// alongside it.** The timestamp is what makes the receiver's replay window
// enforceable — an attacker who captures a delivery cannot re-date it,
// because moving the timestamp invalidates the signature — and the id is what
// keeps a captured delivery from being re-identified as a different event.
//
// key is what config.SigningKey derives from the webhook's secret. Exported
// because the self-hosting guidance documents this construction and receivers
// verify it with off-the-shelf Standard Webhooks libraries; keeping one Go
// definition beside a pinned vector means the documented construction and the
// shipped signer cannot drift.
func Sign(key []byte, id string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, key)
	// Written in pieces rather than concatenated: identical bytes, no copy
	// of a body that may be kilobytes.
	mac.Write([]byte(id))
	mac.Write([]byte("."))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return SignatureVersion + "," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
