package store_test

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-admin/internal/store/storetest"
)

func TestWebhookCRUD(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()

	w, err := s.CreateWebhook(ctx, store.Webhook{
		Name: "ntfy", URL: "https://ntfy.example/gawk", Secret: "s3cr3t", Enabled: true, CreatedBy: "op@example.com",
	})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if w.Secret != "" {
		t.Fatalf("CreateWebhook returned a secret: %q", w.Secret)
	}

	if _, err := s.CreateWebhook(ctx, store.Webhook{Name: "ntfy", URL: "https://other.example", Secret: "x", Enabled: true}); !errors.Is(err, store.ErrDuplicateName) {
		t.Fatalf("duplicate name = %v, want ErrDuplicateName", err)
	}

	list, err := s.ListWebhooks(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListWebhooks = %d rows (err=%v)", len(list), err)
	}
	if list[0].Secret != "" {
		t.Fatalf("ListWebhooks leaked a secret")
	}

	// An empty secret on update keeps the stored one — the portal can never
	// round-trip a secret it was never shown.
	updated, err := s.UpdateWebhook(ctx, store.Webhook{ID: w.ID, Name: "ntfy", URL: "https://ntfy.example/v2", Enabled: false})
	if err != nil {
		t.Fatalf("UpdateWebhook: %v", err)
	}
	if updated.URL != "https://ntfy.example/v2" || updated.Enabled {
		t.Fatalf("update did not apply: %+v", updated)
	}
	full, err := s.GetWebhookByName(ctx, "ntfy")
	if err != nil {
		t.Fatalf("GetWebhookByName: %v", err)
	}
	if full.Secret != "s3cr3t" {
		t.Fatalf("secret after a secret-less update = %q, want it preserved", full.Secret)
	}

	// A non-empty secret replaces it.
	if _, err := s.UpdateWebhook(ctx, store.Webhook{ID: w.ID, Name: "ntfy", URL: full.URL, Enabled: true, Secret: "rotated"}); err != nil {
		t.Fatalf("UpdateWebhook(rotate): %v", err)
	}
	full, _ = s.GetWebhookByName(ctx, "ntfy")
	if full.Secret != "rotated" {
		t.Fatalf("secret after rotation = %q", full.Secret)
	}

	if err := s.DeleteWebhook(ctx, w.ID); err != nil {
		t.Fatalf("DeleteWebhook: %v", err)
	}
	if err := s.DeleteWebhook(ctx, w.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second DeleteWebhook = %v, want ErrNotFound", err)
	}
	if _, err := s.GetWebhookByName(ctx, "ntfy"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetWebhookByName after delete = %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateWebhook(ctx, store.Webhook{ID: uuid.New(), Name: "gone", URL: "https://x.example"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateWebhook(unknown) = %v, want ErrNotFound", err)
	}
}

// Even if a Webhook value carrying a secret reached an encoder, the struct tag
// must keep it out of the JSON. Belt as well as braces: ListWebhooks never
// loads one in the first place.
func TestWebhookSecretIsNeverMarshalled(t *testing.T) {
	b, err := json.Marshal(store.Webhook{Name: "n", URL: "https://x.example", Secret: "TOPSECRET"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "TOPSECRET") || strings.Contains(strings.ToLower(string(b)), "secret") {
		t.Fatalf("marshalled webhook carries its secret: %s", b)
	}
}

// R49 D8: the filter round-trips through every accessor, nil stays nil (the
// default) and an empty list stays empty (receives nothing).
func TestWebhookEventsRoundTrip(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()

	w, err := s.CreateWebhook(ctx, store.Webhook{Name: "bot", URL: "https://b.example", Secret: "cw==", Enabled: true,
		Events: []string{store.EventRoomParticipantJoined}})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Events) != 1 {
		t.Fatalf("created events = %v", w.Events)
	}
	full, err := s.GetWebhookByName(ctx, "bot")
	if err != nil || len(full.Events) != 1 || full.Events[0] != store.EventRoomParticipantJoined {
		t.Fatalf("GetWebhookByName events = %v (err=%v)", full.Events, err)
	}
	u, err := s.UpdateWebhook(ctx, store.Webhook{ID: w.ID, Name: "bot", URL: "https://b.example", Enabled: true, Events: []string{}})
	if err != nil || u.Events == nil || len(u.Events) != 0 {
		t.Fatalf("update to [] = %v (err=%v)", u.Events, err)
	}
	u, err = s.UpdateWebhook(ctx, store.Webhook{ID: w.ID, Name: "bot", URL: "https://b.example", Enabled: true})
	if err != nil || u.Events != nil {
		t.Fatalf("update to nil = %v (err=%v)", u.Events, err)
	}
	list, err := s.ListWebhooks(ctx)
	if err != nil || len(list) != 1 || list[0].Events != nil {
		t.Fatalf("ListWebhooks = %+v (err=%v)", list, err)
	}
}

// The enqueue honours both sources' filters, and where a config webhook
// shadows a UI one of the same name the CONFIG filter decides.
func TestEnqueueHonoursEachWebhooksFilter(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	for _, w := range []store.Webhook{
		{Name: "ui-default", URL: "https://a.example", Secret: "cw==", Enabled: true},
		{Name: "ui-joins", URL: "https://b.example", Secret: "cw==", Enabled: true, Events: []string{store.EventRoomParticipantJoined}},
		// Shadowed by a config webhook that wants no joins.
		{Name: "shadowed", URL: "https://c.example", Secret: "cw==", Enabled: true, Events: []string{store.EventRoomParticipantJoined}},
	} {
		if _, err := s.CreateWebhook(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	config := []store.ConfigWebhook{
		{Name: "cfg-default"},
		{Name: "cfg-joins", Events: []string{store.EventRoomParticipantJoined}},
		{Name: "shadowed"},
	}
	join := json.RawMessage(`{"event":{"data":{"nickname":"tuhis","rejoin":false}}}`)
	if _, err := s.AppendBusEvent(ctx, store.Event{Type: store.EventRoomParticipantJoined, Actor: "system", Payload: join}, "pod:1", config); err != nil {
		t.Fatal(err)
	}
	ban, err := s.AppendEventAndEnqueue(ctx, store.Event{Type: store.EventBanCreated, Actor: "op"}, config)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.ListEvents(ctx, store.EventQuery{Types: []string{store.EventRoomParticipantJoined}})
	if err != nil || len(page) != 1 {
		t.Fatalf("join rows = %d (err=%v)", len(page), err)
	}
	byEvent, err := s.ListDeliveriesForEvents(ctx, []int64{page[0].ID, ban.ID})
	if err != nil {
		t.Fatal(err)
	}
	names := func(id int64) []string {
		var out []string
		for _, d := range byEvent[id] {
			out = append(out, d.WebhookName)
		}
		sort.Strings(out)
		return out
	}
	if got := names(page[0].ID); strings.Join(got, ",") != "cfg-joins,ui-joins" {
		t.Errorf("the join was queued for %v, want exactly the two that asked for joins", got)
	}
	if got := names(ban.ID); strings.Join(got, ",") != "cfg-default,shadowed,ui-default" {
		t.Errorf("the ban was queued for %v, want the three default-filter webhooks", got)
	}
}
