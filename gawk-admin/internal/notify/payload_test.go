package notify

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"

	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// allowedPayloadKeys is the complete set of keys a webhook body may carry
// (docs/42 §4.10).
//
// Hardcoded rather than derived from the Payload struct ON PURPOSE: deriving it
// would make any new field legal the moment it was added, and this list is
// exactly the review gate D8 needs. A new field means a deliberate edit here
// and a fresh look at whether it can carry a raw ID or an address.
var allowedPayloadKeys = map[string]bool{
	"schema":       true,
	"type":         true,
	"occurredAt":   true,
	"actor":        true,
	"broadcastKey": true,
	"reason":       true,
	"portalUrl":    true,
	"summary":      true,
	// enforcement: the deliberate edit that admitted the third webhook-safe
	// payload key. It is reviewable BECAUSE it is a closed vocabulary — the
	// only value that can appear is store.EnforcementPending, so unlike
	// `reason` and `summary` there is no operator- or producer-supplied text
	// behind it that could name a broadcast or an address (see
	// store.Event.EnforcementState, and the poison planted under the same key
	// in poisonedEvent below).
	"enforcement": true,
}

// The values a payload must never contain. Each is planted somewhere an event
// legitimately carries it — the raw ID column, an IP ban's target, a
// portal-only payload key — so a dispatcher that copied "just one more useful
// field" would surface here.
const (
	poisonRawID        = "ZXQ7K2"
	poisonRawIPv4      = "203.0.113.7"
	poisonCIDR         = "203.0.113.0/24"
	poisonRawIPv6      = "2001:db8::dead:beef"
	poisonIPv6CIDR     = "2001:db8::/64"
	poisonOperatorNote = "kubectl-emergency-note"
)

// poisonedEvent is one event of the given type carrying a raw broadcast ID in
// every place an event can hold one, and addresses in every place a ban can
// put one.
func poisonedEvent(eventType string) store.Event {
	payload := map[string]any{
		store.PayloadReason: "terms violation", // operator text: deliberately NOT poisoned, see below
		store.PayloadSummary: "a broadcast ban was created by " +
			"juho@example.com",
		"target": map[string]any{"type": "ip", "value": poisonCIDR},
		// The third webhook-safe key, poisoned: `enforcement` is copied out of
		// the jsonb like the other two, so it gets the same treatment. It
		// survives that only because its vocabulary is closed — a value that
		// is not exactly "pending" is dropped rather than forwarded.
		store.PayloadEnforcement: poisonRawID,
		"banId":                  "11111111-2222-3333-4444-555555555555",
		"sourceBroadcastId":      poisonRawID,
		"publisherIp":            poisonRawIPv4,
		"peer":                   poisonRawIPv6,
		"v6Target":               poisonIPv6CIDR,
		"operatorNote":           poisonOperatorNote,
		"cooldownSeconds":        600,
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

// TestNoRawIDOrIPInAnyPayload is D8 at the schema level, over EVERY event type
// the store declares — including R40's reserved content_flag.raised and the
// synthetic `test` event.
//
// The type list is READ FROM internal/store's source rather than written out
// here, so an event type added later is covered the moment it is declared: a
// producer that ships a new event type cannot also ship a payload leak by
// forgetting to add a case to this test.
func TestNoRawIDOrIPInAnyPayload(t *testing.T) {
	poisons := []string{poisonRawID, poisonRawIPv4, poisonCIDR, poisonRawIPv6, poisonIPv6CIDR, poisonOperatorNote}

	for _, eventType := range allEventTypes(t) {
		t.Run(eventType, func(t *testing.T) {
			body, err := marshal(buildPayload(poisonedEvent(eventType), "https://admin.example.com"))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			rendered := string(body)
			for _, p := range poisons {
				if strings.Contains(rendered, p) {
					t.Errorf("payload leaked %q (D8: no raw broadcast ID and no IP address ever appears in a payload)\n%s", p, rendered)
				}
			}

			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatalf("payload is not JSON: %v", err)
			}
			for k := range m {
				if !allowedPayloadKeys[k] {
					t.Errorf("payload carries key %q, which is not in the §4.10 contract; adding a field needs a D8 review, not just a struct tag", k)
				}
			}
			if summary, _ := m["summary"].(string); strings.TrimSpace(summary) == "" {
				t.Error("payload has no summary: a webhook-to-push bridge would render an empty notification (§4.10)")
			}
			if m["schema"] != PayloadSchema {
				t.Errorf("schema = %v, want %q", m["schema"], PayloadSchema)
			}
			// The deep link filters by the HMAC'd key — still no raw ID (D8):
			// the poisons check above runs over the URL too.
			if m["portalUrl"] != "https://admin.example.com/#/broadcasts?key=3f9a1c2b4d5e" {
				t.Errorf("portalUrl = %v, want the key-filtered portal deep link", m["portalUrl"])
			}
		})
	}
}

// TestSummaryPresentWithoutOneInThePayload covers the fallback: an event whose
// jsonb carries no summary still gets one, because a receiver that renders
// only `summary` must never get an empty notification.
func TestSummaryPresentWithoutOneInThePayload(t *testing.T) {
	for _, eventType := range allEventTypes(t) {
		t.Run(eventType, func(t *testing.T) {
			ev := poisonedEvent(eventType)
			ev.Payload = json.RawMessage(`{"sourceBroadcastId":"` + poisonRawID + `"}`)
			p := buildPayload(ev, "https://admin.example.com")
			if strings.TrimSpace(p.Summary) == "" {
				t.Fatal("no summary and no fallback summary")
			}
			if strings.Contains(p.Summary, poisonRawID) {
				t.Fatalf("the fallback summary names the raw broadcast ID: %q", p.Summary)
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
// The in-sync half of each case is the backward-compatibility claim: absence of
// the key is what an existing receiver has always seen, so it must stay absent
// down to the substring (§4.10, and why PayloadSchema does not move).
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
			mustSay:    "STILL banned",
			mustNotSay: "was lifted by",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pending, err := marshal(buildPayload(
				gradedEvent(tc.eventType, tc.target, tc.key, store.EnforcementPending), "https://admin.example.com"))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var m map[string]any
			if err := json.Unmarshal(pending, &m); err != nil {
				t.Fatalf("payload is not JSON: %v", err)
			}
			if m["enforcement"] != string(store.EnforcementPending) {
				t.Errorf("enforcement = %v, want %q; the receiver cannot tell a recorded action from an enforced one\n%s",
					m["enforcement"], store.EnforcementPending, pending)
			}
			for k := range m {
				if !allowedPayloadKeys[k] {
					t.Errorf("payload carries key %q, which is not in the §4.10 contract", k)
				}
			}
			summary, _ := m["summary"].(string)
			if !strings.Contains(summary, tc.mustSay) {
				t.Errorf("summary %q does not say %q — it is the one sentence ntfy renders", summary, tc.mustSay)
			}
			if strings.Contains(summary, tc.mustNotSay) {
				t.Errorf("summary %q claims %q, which has not happened", summary, tc.mustNotSay)
			}

			// The same event, in sync: byte-for-byte what a receiver has
			// always been sent, with no trace of the new key.
			inSync, err := marshal(buildPayload(
				gradedEvent(tc.eventType, tc.target, tc.key, store.EnforcementInSync), "https://admin.example.com"))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(inSync), "enforcement") {
				t.Errorf("an in-sync delivery grew an enforcement key: %s", inSync)
			}
			var clean map[string]any
			if err := json.Unmarshal(inSync, &clean); err != nil {
				t.Fatalf("payload is not JSON: %v", err)
			}
			if got, _ := clean["summary"].(string); !strings.Contains(got, tc.mustNotSay) {
				t.Errorf("the in-sync summary %q lost its plain wording", got)
			}
		})
	}
}

// TestPortalURLOmittedWhenUnconfigured: an empty -external-url yields no
// portalUrl rather than a link to nowhere.
func TestPortalURLOmittedWhenUnconfigured(t *testing.T) {
	body, err := marshal(buildPayload(goldenEvent(), ""))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "portalUrl") {
		t.Fatalf("portalUrl present with no external URL configured: %s", body)
	}
}

// TestTestPayloadCarriesNoTargetContext: the synthetic test event has nothing
// to say about any broadcast, and must not invent any.
func TestTestPayloadCarriesNoTargetContext(t *testing.T) {
	p := testPayload(time.Now(), "https://admin.example.com")
	if p.Type != EventTest {
		t.Fatalf("type = %q, want %q", p.Type, EventTest)
	}
	if p.BroadcastKey != "" || p.Reason != "" {
		t.Fatalf("test payload carries broadcast context: %+v", p)
	}
	if strings.TrimSpace(p.Summary) == "" {
		t.Fatal("test payload has no summary")
	}
}

// allEventTypes returns every moderation event type plus the synthetic test
// type, read from internal/store's source.
func allEventTypes(t *testing.T) []string {
	t.Helper()
	types := storeEventTypes(t)

	// A parse that silently found nothing would make every D8 case vacuous.
	// These four are the R39 vocabulary (§4.6) and content_flag.raised is
	// R40's reserved fifth; if any disappears, this test should be the thing
	// that notices.
	for _, want := range []string{"broadcast.killed", "ban.created", "ban.expired", "ban.removed", "content_flag.raised"} {
		if !slices.Contains(types, want) {
			t.Fatalf("event type %q not found in internal/store; parsed: %v", want, types)
		}
	}
	return append(types, EventTest)
}

// storeEventTypes parses internal/store's non-test sources and returns the
// value of every exported `Event*` string constant.
//
// Source parsing rather than a hand-copied list is what makes "a new event type
// added later is covered automatically" true. os.ReadDir + parser.ParseFile
// rather than parser.ParseDir, which is deprecated.
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
	return out
}
