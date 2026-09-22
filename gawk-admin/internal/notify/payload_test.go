package notify

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"

	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// The values a delivery must not contain, except where docs/52 D9 says it
// must. Each is planted somewhere an event legitimately carries it — the raw
// ID column, an IP ban's target, a portal-only payload key, the room code —
// so a projection that copied "just one more useful field" would surface
// here.
//
// Two classes since D9. An IP, an operator's kubectl note and an attach
// secret are in no event at all and may appear NOWHERE. A broadcast ID and a
// room code are the event's own identity and belong in exactly two places:
// the `subject` attribute and the property the schema names for them. Finding
// one anywhere else is still a leak — it means some other property was
// copied through — which is why the second class is checked against the body
// with those two places removed.
const (
	poisonRawID        = "ZXQ7K2"
	poisonRawIPv4      = "203.0.113.7"
	poisonCIDR         = "203.0.113.0/24"
	poisonRawIPv6      = "2001:db8::dead:beef"
	poisonIPv6CIDR     = "2001:db8::/64"
	poisonOperatorNote = "kubectl-emergency-note"
	// A dynamic room code has the broadcast-ID shape and IS a join
	// capability (docs/44 D16); a static slug is one too, if less secret.
	poisonRoomCode = "R7K3MX"
	poisonRoomSlug = "tuhisroom"
	// The one-time attach secret — never in any payload, ever (docs/44 §5).
	poisonAttachSecret = "AttachSecretValue12345678"
)

// neverPoisons may appear nowhere in a delivery.
var neverPoisons = []string{poisonRawIPv4, poisonCIDR, poisonRawIPv6, poisonIPv6CIDR, poisonOperatorNote,
	poisonAttachSecret}

// identityPoisons are the joinable identifiers D9 delivers: allowed in
// `subject` and in their own `data` property, nowhere else.
var identityPoisons = []string{poisonRawID, poisonRoomCode, poisonRoomSlug}

// poisonedEvent is one event of the given type carrying a raw broadcast ID in
// every place an event can hold one, and addresses in every place a ban can
// put one.
func poisonedEvent(eventType string) store.Event {
	payload := map[string]any{
		store.PayloadReason: "terms violation", // operator text: deliberately NOT poisoned
		store.PayloadSummary: "a broadcast ban was created by " +
			"juho@example.com",
		"target": map[string]any{"type": "ip", "value": poisonCIDR},
		// enforcement is read through a closed vocabulary: a value that is
		// not exactly "pending" is dropped rather than forwarded.
		store.PayloadEnforcement: poisonRawID,
		"banId":                  "11111111-2222-3333-4444-555555555555",
		"sourceBroadcastId":      poisonRawID,
		"publisherIp":            poisonRawIPv4,
		"peer":                   poisonRawIPv6,
		"v6Target":               poisonIPv6CIDR,
		"operatorNote":           poisonOperatorNote,
		"cooldownSeconds":        600,
		// The room keys: the raw code under the portal-only key (as the
		// producers write it), which buildEvent reads into the roomCode
		// property a delivery now carries (D9) — and, the trap, the raw code
		// planted under roomKey too, where only a hex digest may come out,
		// because that is the key the portal link is built from.
		store.PayloadRoom:        poisonRoomCode,
		store.PayloadRoomKind:    "dynamic",
		store.PayloadRoomKey:     poisonRoomCode,
		store.PayloadDisplayCode: poisonRoomSlug,
		"attachSecret":           poisonAttachSecret,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return store.Event{
		ID:           99,
		Type:         eventType,
		OccurredAt:   time.Date(2026, 8, 20, 15, 4, 5, 0, time.UTC),
		Actor:        "juho@example.com",
		BroadcastKey: "3f9a1c2b4d5e",
		BroadcastID:  poisonRawID,
		Payload:      raw,
	}
}

// decode splits a rendered delivery into its envelope and its data map.
func decode(t *testing.T, body []byte) (map[string]any, map[string]any) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("delivery is not JSON: %v (%s)", err, body)
	}
	data, _ := m["data"].(map[string]any)
	if data == nil {
		t.Fatalf("delivery has no data object: %s", body)
	}
	return m, data
}

// withoutIdentity renders a delivery with the two places an identity is
// allowed — the `subject` attribute and the `broadcastId` / `roomCode` /
// `displayCode` properties — removed, so what is left can be scanned for an
// identifier that reached it through anything else.
func withoutIdentity(t *testing.T, body []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("delivery is not JSON: %v (%s)", err, body)
	}
	delete(m, "subject")
	if data, _ := m["data"].(map[string]any); data != nil {
		for _, k := range []string{"broadcastId", "roomCode", "displayCode"} {
			delete(data, k)
		}
	}
	rest, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(rest)
}

// TestNoIPOrStraySecretInAnyDelivery is docs/42 D8 as docs/52 D9 leaves it,
// at the contract level, over EVERY event type the store declares —
// including R40's reserved content_flag.raised — plus the synthetic test
// event.
//
// What D9 changed: the broadcast ID and the room code are delivered, in
// `subject` and in their own property. What it did not: no IP, no operator
// note, no attach secret, and no identifier smuggled through some OTHER
// property — every payload key here is poisoned, and only the two places the
// contract names may come back carrying one.
//
// The type list is READ FROM internal/store's source rather than written out
// here, so an event type added later is covered the moment it is declared: a
// producer that ships a new event type cannot also ship a leak by forgetting
// to add a case to this test.
func TestNoIPOrStraySecretInAnyDelivery(t *testing.T) {
	for _, eventType := range storeEventTypes(t) {
		t.Run(eventType, func(t *testing.T) {
			body := render(t, poisonedEvent(eventType), "https://admin.example.com")
			rendered := string(body)
			for _, p := range neverPoisons {
				if strings.Contains(rendered, p) {
					t.Errorf("delivery leaked %q (D8: no IP address, operator note or attach secret ever appears in a delivery)\n%s", p, rendered)
				}
			}
			for _, p := range identityPoisons {
				if rest := withoutIdentity(t, body); strings.Contains(rest, p) {
					t.Errorf("delivery leaked %q outside subject and its own property (D9 delivers an identity in two places, not everywhere)\n%s", p, rendered)
				}
			}
			envelope, data := decode(t, body)
			typ, ok := events.ModerationType(eventType)
			if !ok || envelope["type"] != typ {
				t.Errorf("type = %v, want %s", envelope["type"], typ)
			}
			if envelope["source"] != events.SourceAdmin {
				t.Errorf("source = %v, want %s", envelope["source"], events.SourceAdmin)
			}
			if envelope["dataschema"] != events.SchemaID(typ) {
				t.Errorf("dataschema = %v, want %s", envelope["dataschema"], events.SchemaID(typ))
			}
			if envelope["id"] != EventID(99) {
				t.Errorf("id = %v, want the row-derived %s", envelope["id"], EventID(99))
			}
			assertProjected(t, typ, data)
			validateData(t, typ, data)
			if summary, _ := data["summary"].(string); strings.TrimSpace(summary) == "" {
				t.Error("delivery has no summary: a webhook-to-push bridge would render an empty notification (§4.10)")
			}
			// The deep link still filters by the HMAC'd key, because that is
			// what the portal's routes take. A room event links to the rooms
			// view; with the poisoned (non-hex) roomKey dropped it carries no
			// filter at all rather than the code — the one place the code is
			// still refused, since a link keyed by it would not resolve.
			wantURL := "https://admin.example.com/#/broadcasts?key=3f9a1c2b4d5e"
			// The subject is the cleartext identity (D9): the raw ID for a
			// broadcast event, the code for a room one.
			wantSubject := poisonRawID
			if strings.HasPrefix(eventType, "room.") {
				wantURL = "https://admin.example.com/#/rooms"
				wantSubject = poisonRoomCode
				if _, leaked := data["roomKey"]; leaked {
					t.Errorf("a non-hex roomKey (a raw room code) was forwarded as the key: %v", data["roomKey"])
				}
				if data["roomCode"] != poisonRoomCode {
					t.Errorf("roomCode = %v, want the raw code %q (D9)", data["roomCode"], poisonRoomCode)
				}
			} else if data["broadcastId"] != poisonRawID {
				t.Errorf("broadcastId = %v, want the raw ID %q (D9)", data["broadcastId"], poisonRawID)
			}
			if data["portalUrl"] != wantURL {
				t.Errorf("portalUrl = %v, want %q", data["portalUrl"], wantURL)
			}
			if subject, _ := envelope["subject"].(string); subject != wantSubject {
				t.Errorf("subject = %q, want the cleartext %q (D9)", subject, wantSubject)
			}
		})
	}

	t.Run(events.TypeWebhookTest, func(t *testing.T) {
		body, err := events.Marshal(testEvent(time.Now(), "https://admin.example.com"))
		if err != nil {
			t.Fatal(err)
		}
		_, data := decode(t, body)
		assertProjected(t, events.TypeWebhookTest, data)
		validateData(t, events.TypeWebhookTest, data)
	})
}

// assertProjected holds a delivery's data to the one rule the projection
// still enforces since docs/52 D9: every key it carries is a property the
// type's own schema declares. Nothing is removed any more, so there is
// nothing to assert absent; TestEveryVectorProjectsWhole is what holds the
// projection to carrying the event's own properties through.
//
// The schema is the review gate: a new property is a reviewed contract
// change, so a key the schema does not declare is a struct tag that got
// ahead of the document.
func assertProjected(t *testing.T, typ string, data map[string]any) {
	t.Helper()
	raw, err := events.Schema(typ)
	if err != nil {
		t.Fatalf("schema for %s: %v", typ, err)
	}
	var schema struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	for k := range data {
		if _, declared := schema.Properties[k]; !declared {
			t.Errorf("delivery carries %q, which %s's schema does not declare; adding a property is a contract change (docs/52 D6 b), not a struct tag", k, typ)
		}
	}
}

// TestSummaryPresentWithoutOneInThePayload covers the fallback: an event
// whose jsonb carries no summary still gets one, because a receiver that
// renders only `summary` must never get an empty notification.
//
// The fallback names the broadcast from the event's BroadcastID column — the
// same value the delivery's `subject` carries (docs/52 D9) — and never from
// some other payload key: a stray raw ID planted in the jsonb must not reach
// the sentence.
func TestSummaryPresentWithoutOneInThePayload(t *testing.T) {
	const column = "ABC234"
	for _, eventType := range storeEventTypes(t) {
		t.Run(eventType, func(t *testing.T) {
			ev := poisonedEvent(eventType)
			ev.BroadcastID = column
			ev.Payload = json.RawMessage(`{"sourceBroadcastId":"` + poisonRawID + `"}`)
			_, data := decode(t, render(t, ev, "https://admin.example.com"))
			summary, _ := data["summary"].(string)
			if strings.TrimSpace(summary) == "" {
				t.Fatal("no summary and no fallback summary")
			}
			if strings.Contains(summary, poisonRawID) {
				t.Fatalf("the fallback summary took a raw ID from a stray payload key: %q", summary)
			}
			if eventType == store.EventBroadcastKilled && !strings.Contains(summary, column) {
				t.Fatalf("the fallback kill summary does not name the broadcast: %q", summary)
			}
		})
	}
}

// gradedEvent is one moderation event as internal/api records it: the summary
// its handler asked store for, and the enforcement key only when the CR write
// did not land.
func gradedEvent(eventType string, target moderation.TargetType, key string, enforcement store.EnforcementState) store.Event {
	payload := map[string]any{
		store.PayloadReason: "terms violation",
		store.PayloadSummary: store.SummarizeWithEnforcement(
			eventType, target, key, "juho@example.com", enforcement),
		"banId":             "11111111-2222-3333-4444-555555555555",
		"sourceBroadcastId": poisonRawID, // portal-only, as always
	}
	if enforcement != store.EnforcementInSync {
		payload[store.PayloadEnforcement] = string(enforcement)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return store.Event{
		ID:           42,
		Type:         eventType,
		OccurredAt:   time.Date(2026, 8, 20, 15, 4, 5, 0, time.UTC),
		Actor:        "juho@example.com",
		BroadcastKey: key,
		BroadcastID:  poisonRawID,
		Payload:      raw,
	}
}

// A webhook is a statement that something happened. When the Ban CR write did
// not land, what happened is a RECORD — so the delivery has to carry the
// pending grade and a sentence that does not claim the enforcement.
//
// The in-sync half of each case is the backward-compatibility claim: absence
// of the key is what a receiver has always seen, so it must stay absent down
// to the substring.
func TestPendingEnforcementCrossesIntoTheDelivery(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		target    moderation.TargetType
		key       string
		// mustSay is the load-bearing half of the pending sentence: the word
		// an operator reading a push notification acts on.
		mustSay string
		// mustNotSay is the claim the in-sync sentence makes and the pending
		// one may not.
		mustNotSay string
	}{
		{
			name:       "a kill whose CR never landed",
			eventType:  store.EventBroadcastKilled,
			target:     moderation.TargetBroadcastID,
			key:        "3f9a1c2b4d5e",
			mustSay:    "NOT enforced yet",
			mustNotSay: "was terminated",
		},
		{
			name:       "a ban whose CR never landed",
			eventType:  store.EventBanCreated,
			target:     moderation.TargetIP,
			mustSay:    "NOT enforced yet",
			mustNotSay: "was created",
		},
		{
			// The direction that matters most on a phone: the operator lifted
			// a ban and the target is still banned.
			name:       "an unban whose CR delete never landed",
			eventType:  store.EventBanRemoved,
			target:     moderation.TargetBroadcastID,
			key:        "3f9a1c2b4d5e",
			mustSay:    "STILL banned",
			mustNotSay: "was lifted by",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pending := render(t, gradedEvent(tc.eventType, tc.target, tc.key, store.EnforcementPending), "https://admin.example.com")
			_, data := decode(t, pending)
			if data["enforcement"] != string(store.EnforcementPending) {
				t.Errorf("enforcement = %v, want %q; the receiver cannot tell a recorded action from an enforced one\n%s",
					data["enforcement"], store.EnforcementPending, pending)
			}
			typ, _ := events.ModerationType(tc.eventType)
			assertProjected(t, typ, data)
			summary, _ := data["summary"].(string)
			if !strings.Contains(summary, tc.mustSay) {
				t.Errorf("summary %q does not say %q — it is the one sentence ntfy renders", summary, tc.mustSay)
			}
			if strings.Contains(summary, tc.mustNotSay) {
				t.Errorf("summary %q claims %q, which has not happened", summary, tc.mustNotSay)
			}

			// The same event, in sync: no trace of the key.
			inSync := render(t, gradedEvent(tc.eventType, tc.target, tc.key, store.EnforcementInSync), "https://admin.example.com")
			if strings.Contains(string(inSync), "enforcement") {
				t.Errorf("an in-sync delivery grew an enforcement key: %s", inSync)
			}
			_, clean := decode(t, inSync)
			if got, _ := clean["summary"].(string); !strings.Contains(got, tc.mustNotSay) {
				t.Errorf("the in-sync summary %q lost its plain wording", got)
			}
		})
	}
}

// roomEvent is one room event as internal/api and the reconciler record it:
// the raw code, the display code, and the HMAC'd key under roomKey.
func roomEvent(eventType, kind, key, actor string) store.Event {
	payload := map[string]any{
		store.PayloadSummary:     store.SummarizeRoom(eventType, kind, poisonRoomSlug, actor),
		store.PayloadRoom:        poisonRoomCode,
		store.PayloadRoomKind:    kind,
		store.PayloadDisplayCode: poisonRoomSlug,
	}
	if key != "" {
		payload[store.PayloadRoomKey] = key
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return store.Event{
		ID: 7, Type: eventType, Actor: actor,
		OccurredAt: time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC),
		Payload:    raw,
	}
}

// TestRoomEventsCarryTheCodeAndLinkToTheRoomsView is RM7's acceptance row
// (docs/44 §9) as docs/52 D9 rewrote it: a webhook about a room names the
// room in cleartext — the `subject` and `roomCode` — and still carries the
// HMAC'd key beside it, because the rooms deep link is filtered by that key.
// The attach secret is still in no delivery, ever.
//
// When no pod has homed the room yet there is no key: the delivery then still
// names the room (the code is what the operator typed to create it) and links
// to the unfiltered rooms view.
func TestRoomEventsCarryTheCodeAndLinkToTheRoomsView(t *testing.T) {
	for _, eventType := range []string{store.EventRoomCreated, store.EventRoomEnded, store.EventRoomSecretRotated} {
		t.Run(eventType, func(t *testing.T) {
			body := render(t, roomEvent(eventType, "static", "9c1d2e3f4a5b", "juho@example.com"), "https://admin.example.com")
			envelope, data := decode(t, body)
			if data["roomKey"] != "9c1d2e3f4a5b" {
				t.Errorf("roomKey = %v, want the HMAC'd key the portal link is built from", data["roomKey"])
			}
			if envelope["subject"] != poisonRoomCode || data["roomCode"] != poisonRoomCode {
				t.Errorf("subject = %v, roomCode = %v, want the cleartext code %q (D9)",
					envelope["subject"], data["roomCode"], poisonRoomCode)
			}
			if data["kind"] != "static" {
				t.Errorf("kind = %v, want static", data["kind"])
			}
			if data["portalUrl"] != "https://admin.example.com/#/rooms?key=9c1d2e3f4a5b" {
				t.Errorf("portalUrl = %v, want the key-filtered rooms deep link", data["portalUrl"])
			}
			if _, has := data["broadcastKey"]; has {
				t.Errorf("a room event carried a broadcastKey: %s", body)
			}
			// The display code rides along since docs/52 D9: it is what the
			// operator named the room, and `roomCode` has normalised the
			// casing away. The attach secret is in no room event at all.
			if data["displayCode"] != poisonRoomSlug {
				t.Errorf("displayCode = %v, want the display code %q", data["displayCode"], poisonRoomSlug)
			}
			if strings.Contains(string(body), poisonAttachSecret) {
				t.Errorf("delivery leaked the attach secret (docs/44 §5: it is in no payload, ever)\n%s", body)
			}
			summary, _ := data["summary"].(string)
			if !strings.Contains(summary, "static room") {
				t.Errorf("summary %q should name the kind and nothing more", summary)
			}
			typ, _ := events.ModerationType(eventType)
			validateData(t, typ, data)

			// Not homed yet: no key and no filter, but the room is still
			// named — an operator who created it knows it by its code, and a
			// notification that named nothing would be unactionable.
			unkeyed := render(t, roomEvent(eventType, "static", "", "juho@example.com"), "https://admin.example.com")
			if strings.Contains(string(unkeyed), "roomKey") {
				t.Errorf("an unkeyed room event grew a roomKey: %s", unkeyed)
			}
			if !strings.Contains(string(unkeyed), `"portalUrl":"https://admin.example.com/#/rooms"`) {
				t.Errorf("unkeyed portalUrl should be the bare rooms view: %s", unkeyed)
			}
			unkeyedEnvelope, unkeyedData := decode(t, unkeyed)
			if unkeyedEnvelope["subject"] != poisonRoomCode || unkeyedData["roomCode"] != poisonRoomCode {
				t.Errorf("an unkeyed room event lost the room's identity: %s", unkeyed)
			}
		})
	}
}

// TestPortalURLOmittedWhenUnconfigured: an empty -external-url yields no
// portalUrl rather than a link to nowhere.
func TestPortalURLOmittedWhenUnconfigured(t *testing.T) {
	if body := render(t, goldenEvent(), ""); strings.Contains(string(body), "portalUrl") {
		t.Fatalf("portalUrl present with no external URL configured: %s", body)
	}
}

// TestTestEventCarriesNoTargetContext: the synthetic test event has nothing
// to say about any broadcast, and must not invent any.
func TestTestEventCarriesNoTargetContext(t *testing.T) {
	ev := testEvent(time.Now(), "https://admin.example.com")
	if ev.Type != events.TypeWebhookTest || ev.Source != events.SourceAdmin || ev.Subject != "" {
		t.Fatalf("test event envelope: %+v", ev)
	}
	data, ok := ev.Data.(events.WebhookTestData)
	if !ok || strings.TrimSpace(data.Summary) == "" {
		t.Fatalf("test event data: %+v", ev.Data)
	}
	if data.PortalURL != "https://admin.example.com/#/broadcasts" {
		t.Fatalf("portalUrl = %q", data.PortalURL)
	}
	// Two test sends are two events: a fresh id each, never a row-derived
	// one, because there is no row.
	if other := testEvent(time.Now(), ""); other.ID == ev.ID || ev.ID == "" {
		t.Fatalf("test event ids: %q, %q", ev.ID, other.ID)
	}
}

// TestAnUnknownRowTypeIsRefused: a row type the contract does not know is a
// producer bug, and buildEvent must not invent a type for it.
func TestAnUnknownRowTypeIsRefused(t *testing.T) {
	ev := goldenEvent()
	ev.Type = "broadcast.frobnicated"
	if _, err := buildEvent(ev, ""); err == nil {
		t.Fatal("buildEvent accepted a row type the contract does not define")
	}
}

// storeEventTypes parses internal/store's non-test sources and returns the
// value of every exported `Event*` string constant.
//
// Source parsing rather than a hand-copied list is what makes "a new event
// type added later is covered automatically" true. os.ReadDir +
// parser.ParseFile rather than parser.ParseDir, which is deprecated.
func storeEventTypes(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("..", "store")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, ident := range vs.Names {
					if !strings.HasPrefix(ident.Name, "Event") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil || value == "" {
						continue
					}
					out = append(out, value)
				}
			}
		}
	}
	// R50's ingested activity row types are declared in the same package and
	// are scraped here too, but they are NOT deliverable: an activity row
	// carries a bus event that gawk-admin never re-emits, so it has no
	// moderation CloudEvents type and buildEvent rightly refuses it. Drop
	// them — and assert the dropped set is exactly the activity vocabulary,
	// so this cannot quietly swallow a moderation type somebody forgot to map.
	var deliverable, skipped []string
	for _, typ := range out {
		if _, ok := events.ModerationType(typ); ok {
			deliverable = append(deliverable, typ)
			continue
		}
		skipped = append(skipped, typ)
	}
	sort.Strings(skipped)
	wantSkipped := append([]string(nil), store.ActivityEventTypes()...)
	sort.Strings(wantSkipped)
	if !slices.Equal(skipped, wantSkipped) {
		t.Fatalf("undeliverable store event types = %v, want exactly the activity vocabulary %v", skipped, wantSkipped)
	}
	out = deliverable

	// A parse that silently found nothing would make every D8 case vacuous.
	for _, want := range []string{"broadcast.killed", "ban.created", "ban.expired", "ban.removed", "content_flag.raised",
		"room.created", "room.ended", "room.secret_rotated"} {
		if !slices.Contains(out, want) {
			t.Fatalf("event type %q not found in internal/store; parsed: %v", want, out)
		}
	}
	return out
}
