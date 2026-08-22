package notify

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
)

// The wire contract of a webhook delivery (docs/42 §4.10). These are the exact
// strings a receiver matches on, so they are constants here and nowhere else.
const (
	HeaderEvent     = "X-Gawk-Event"
	HeaderDelivery  = "X-Gawk-Delivery"
	HeaderTimestamp = "X-Gawk-Timestamp"
	HeaderSignature = "X-Gawk-Signature"

	// ContentType is fixed: every payload is JSON.
	ContentType = "application/json"

	// PayloadSchema names the payload shape. It is versioned so R40's
	// content-flag events can extend the body without silently changing what
	// an existing receiver parses.
	PayloadSchema = "gawk.moderation-event.v1"

	// SignaturePrefix labels the algorithm inside X-Gawk-Signature, so a
	// future second algorithm is distinguishable rather than ambiguous.
	SignaturePrefix = "sha256="
)

// EventTest is the type of the synthetic event POST /webhooks/{name}/test
// sends (§4.7). It is deliberately NOT a store event type: a test send writes
// no row to moderation_events, because a test is not a moderation action and
// must not pollute the audit trail.
const EventTest = "test"

// testSummary is the one sentence a test delivery carries. A test send exists
// to prove the pipe end to end — including that the receiver renders `summary`
// — so it must read like a real notification on a phone, not like an empty
// probe.
const testSummary = "test notification from the gawk-admin portal: this webhook is configured correctly"

// portalPath is the route every payload's portalUrl points at (§4.10's
// example, and `ui/src/router/router.ts`, which pins `#/broadcasts` as the
// default route precisely so a cold click from a push notification lands
// somewhere real).
//
// An event that names a broadcast appends `?key=<broadcastKey>` — the HMAC'd
// key, never the raw ID (D8) — which the portal reads as a pre-filled filter,
// so a paged operator lands ON the offending row instead of visually matching
// a 12-hex key against a fleet-sized table.
const portalPath = "/#/broadcasts"

func portalURL(externalURL, broadcastKey string) string {
	if externalURL == "" {
		return ""
	}
	if broadcastKey == "" {
		return externalURL + portalPath
	}
	return externalURL + portalPath + "?key=" + url.QueryEscape(broadcastKey)
}

// Payload is the webhook body (docs/42 §4.10).
//
// **What is missing from this struct is the point.** There is no field for a
// raw broadcast ID and none for an IP address, so D8's "no raw ID and no IP
// ever appears in a payload" is enforced by the type rather than by a rule
// somebody has to remember: a webhook body cannot carry what the struct cannot
// hold. The same trick guards secrets in api.webhookJSON.
//
// Reason is the deliberate exception to "nothing else crosses": ban reasons are
// operator-private context that the operator chose to route to this receiver
// (§5, "webhooks carry them ... the self-hosting doc warns that the webhook
// receiver sees reasons").
type Payload struct {
	Schema     string `json:"schema"`
	Type       string `json:"type"`
	OccurredAt string `json:"occurredAt"`
	Actor      string `json:"actor"`
	// BroadcastKey is the HMAC'd key — never the joinable ID (D8).
	BroadcastKey string `json:"broadcastKey,omitempty"`
	Reason       string `json:"reason,omitempty"`
	// PortalURL is how the receiver acts: the notification carries no
	// capability, only a link to a surface that demands a login.
	PortalURL string `json:"portalUrl,omitempty"`
	// Summary is one human sentence so a dumb webhook-to-push bridge (ntfy)
	// needs no templating (§4.10). Never omitempty: a receiver that renders
	// only `summary` must never receive a body without it.
	Summary string `json:"summary"`
	// Enforcement is "pending" when the Kubernetes object that would MAKE this
	// event true had not been written when it was recorded — and is absent
	// otherwise. An event is a statement of something that happened, so a
	// delivery announcing a kill the relays were never told about has to say
	// so; `summary` already reads as pending, and this is the machine-readable
	// half of the same fact.
	//
	// A bare string from a CLOSED vocabulary (store.EnforcementState), not an
	// object mirroring the HTTP API's {inSync, detail}:
	//
	//   - Absence means in sync, exactly as it does on the HTTP ban body. That
	//     keeps every existing receiver's bytes unchanged and makes this an
	//     additive field — which is why the schema string does not move.
	//   - `detail` is portal copy ("the reconciler retries within a minute, so
	//     do not re-submit") addressed to the operator holding the mutation's
	//     response. A push notification's human half is `summary`; shipping a
	//     second sentence would give a dumb bridge two competing texts.
	//   - A closed scalar vocabulary cannot carry a raw ID or an address the
	//     way a free-form string or a nested object could, so D8 stays
	//     structural rather than becoming a per-field review (see
	//     store.Event.EnforcementState).
	Enforcement string `json:"enforcement,omitempty"`
}

// buildPayload projects a persisted event onto the wire shape.
//
// It copies exactly four things out of the event — type, time, actor and the
// HMAC'd key — plus the three payload keys store declares webhook-safe
// (store.PayloadReason, store.PayloadSummary, store.PayloadEnforcement).
// Everything else in the event's jsonb is portal-only: it may hold raw IDs,
// addresses and CIDRs.
func buildPayload(ev store.Event, externalURL string) Payload {
	// Read through the accessor, never as a raw string: it is what closes the
	// vocabulary, so this field can only ever be "" or "pending" whatever a
	// producer wrote into the payload.
	enforcement := ev.EnforcementState()
	summary := ev.PayloadString(store.PayloadSummary)
	if summary == "" {
		// Every event written by internal/api and internal/kube carries a
		// summary already. This fallback exists so an event from some future
		// producer still satisfies "summary present on every payload" — and it
		// calls the ONE summariser (store.SummarizeWithEnforcement) rather
		// than growing a second one that could drift into naming a raw ID, or
		// into claiming an enforcement that has not started.
		summary = store.SummarizeWithEnforcement(ev.Type, "", ev.BroadcastKey, ev.Actor, enforcement)
	}
	p := Payload{
		Schema:       PayloadSchema,
		Type:         ev.Type,
		OccurredAt:   ev.OccurredAt.UTC().Format(time.RFC3339),
		Actor:        ev.Actor,
		BroadcastKey: ev.BroadcastKey,
		Reason:       ev.PayloadString(store.PayloadReason),
		Summary:      summary,
		Enforcement:  string(enforcement),
	}
	p.PortalURL = portalURL(externalURL, ev.BroadcastKey)
	return p
}

// testPayload is the synthetic body of a test send.
func testPayload(now time.Time, externalURL string) Payload {
	p := Payload{
		Schema:     PayloadSchema,
		Type:       EventTest,
		OccurredAt: now.UTC().Format(time.RFC3339),
		Actor:      "gawk-admin",
		Summary:    testSummary,
	}
	p.PortalURL = portalURL(externalURL, "")
	return p
}

// marshal renders a payload to the exact bytes that are both sent and signed.
//
// HTML escaping is off: `summary` and `reason` are human sentences that end up
// in a push notification, and a receiver that forwards the field verbatim
// should show "spam & abuse" rather than the default encoder's
// "spam \u0026 abuse". The encoder's trailing newline is stripped so the signed
// material is the JSON value itself.
func marshal(p Payload) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return nil, fmt.Errorf("notify: marshal payload: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Sign returns the X-Gawk-Signature value for a body: the hex HMAC-SHA256 of
// `timestamp + "." + body` under that webhook's own secret (docs/42 §4.10).
//
// **The timestamp is inside the signed material, not merely alongside it.**
// That is what makes the receiver's replay window enforceable: an attacker who
// captures a delivery cannot re-date it, because moving the timestamp
// invalidates the signature. A signature over the body alone would let a
// replayed kill notification look fresh forever.
//
// Exported because the self-hosting guidance documents this construction and
// receivers reimplement it; keeping one Go definition means the documented
// vector and the shipped signer cannot drift.
func Sign(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	// Written in three pieces rather than concatenated: identical bytes, no
	// copy of a body that may be kilobytes.
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return SignaturePrefix + hex.EncodeToString(mac.Sum(nil))
}
