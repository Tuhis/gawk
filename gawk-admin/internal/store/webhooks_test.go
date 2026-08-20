package store_test

import (
	"encoding/json"
	"errors"
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
