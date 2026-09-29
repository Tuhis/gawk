package store_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/events"
)

// busRow is an activity row the way the bus ingester writes it: the whole
// CloudEvent under "event" (eventbus.activityPayload).
func busRow(t *testing.T, rowType string, data map[string]any) store.Event {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"event":   map[string]any{"type": events.TypePrefix + rowType, "data": data},
		"summary": "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store.Event{Type: rowType, Category: store.CategoryActivity, Payload: payload}
}

// R49 D8: no filter is what every webhook had before R49 — every moderation
// event, no activity event — so an existing receiver sees no change.
func TestAWebhookWithNoFilterGetsModerationOnly(t *testing.T) {
	for _, typ := range store.AllEventTypes() {
		if !store.WebhookWants(nil, store.Event{Type: typ}) {
			t.Errorf("the default filter drops moderation event %s", typ)
		}
	}
	for _, typ := range store.ActivityEventTypes() {
		if store.WebhookWants(nil, busRow(t, typ, nil)) {
			t.Errorf("the default filter forwards activity event %s", typ)
		}
	}
}

// A list is exact, and it can only ever name webhook-eligible types: an
// activity type that is not one (participant_updated) is never forwarded,
// listed or not.
func TestAWebhookFilterIsExact(t *testing.T) {
	filter := []string{store.EventRoomParticipantJoined}
	if !store.WebhookWants(filter, busRow(t, store.EventRoomParticipantJoined, map[string]any{"nickname": "tuhis"})) {
		t.Error("a listed join was not forwarded")
	}
	for _, typ := range []string{store.EventBanCreated, store.EventRoomEnded} {
		if store.WebhookWants(filter, store.Event{Type: typ}) {
			t.Errorf("%s reached a webhook that listed only joins", typ)
		}
	}
	if store.WebhookWants(filter, busRow(t, store.EventRoomDetached, nil)) {
		t.Error("a detach reached a webhook that listed only joins")
	}
	notEligible := []string{store.EventRoomParticipantUpdated}
	if store.WebhookWants(notEligible, busRow(t, store.EventRoomParticipantUpdated, nil)) {
		t.Error("participant_updated is not webhook-eligible but was forwarded")
	}
	if store.WebhookWants([]string{}, store.Event{Type: store.EventBanCreated}) {
		t.Error("an empty filter receives nothing, not the default")
	}
}

// docs/50 D6: a re-home's left{home_moved} + joined{rejoin} pair is dropped by
// default and delivered only when the webhook lists room.participant_rejoined.
func TestTheAdoptionPairNeedsTheRejoinedToken(t *testing.T) {
	left := busRow(t, store.EventRoomParticipantLeft, map[string]any{"reason": events.ParticipantLeftHomeMoved})
	joined := busRow(t, store.EventRoomParticipantJoined, map[string]any{"rejoin": true})
	subscribed := []string{store.EventRoomParticipantJoined, store.EventRoomParticipantLeft}
	for name, ev := range map[string]store.Event{"left": left, "joined": joined} {
		if store.WebhookWants(subscribed, ev) {
			t.Errorf("the %s half of an adoption reached a webhook without %s", name, store.FilterParticipantRejoined)
		}
		if !store.WebhookWants(append(subscribed, store.FilterParticipantRejoined), ev) {
			t.Errorf("the %s half was dropped although %s is listed", name, store.FilterParticipantRejoined)
		}
	}
	// An ordinary leave and join are not halves of anything.
	plainLeft := busRow(t, store.EventRoomParticipantLeft, map[string]any{"reason": events.ParticipantLeft})
	plainJoined := busRow(t, store.EventRoomParticipantJoined, map[string]any{"rejoin": false})
	if !store.WebhookWants(subscribed, plainLeft) || !store.WebhookWants(subscribed, plainJoined) {
		t.Error("an ordinary leave or join was treated as a re-home")
	}
}

func TestValidateWebhookEvents(t *testing.T) {
	if err := store.ValidateWebhookEvents(nil); err != nil {
		t.Errorf("nil: %v", err)
	}
	if err := store.ValidateWebhookEvents(store.WebhookFilterTypes()); err != nil {
		t.Errorf("every filter type: %v", err)
	}
	for _, bad := range []string{"room.participant_updated", "broadcast.started", "fi.ioio.gawk.room.attached", ""} {
		if err := store.ValidateWebhookEvents([]string{bad}); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	if !slices.Contains(store.WebhookFilterTypes(), store.FilterParticipantRejoined) {
		t.Error("the rejoined token is not a filter type")
	}
}

// Every webhook-eligible row type has a CloudEvents type, the two halves of
// the vocabulary each through its own rule.
func TestEveryWebhookTypeHasACloudEventsType(t *testing.T) {
	for _, row := range store.WebhookEventTypes() {
		typ, ok := store.CloudEventsType(row)
		if !ok || !events.IsType(typ) {
			t.Errorf("%s → %q, %v", row, typ, ok)
		}
	}
	if typ, _ := store.CloudEventsType(store.EventRoomParticipantJoined); typ != events.TypeRoomParticipantJoined {
		t.Errorf("participant_joined → %q", typ)
	}
}
