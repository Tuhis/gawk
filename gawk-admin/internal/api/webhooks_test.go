package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-admin/internal/api"
	"github.com/Tuhis/gawk/gawk-admin/internal/config"
)

// withStaticWebhooks installs chart-defined webhooks — the config-sourced half
// of D9. Their secrets exist only here, never in the database.
func withStaticWebhooks() harnessOption {
	return withConfig(func(c *config.Config) {
		disabled := false
		c.StaticWebhooks = []config.StaticWebhook{
			{Name: "paging", URL: "https://ntfy.example/gawk", SecretEnv: "PAGING_SECRET", Secret: "chart-secret"},
			{Name: "parked", URL: "https://parked.example/gawk", SecretEnv: "PARKED_SECRET", Secret: "s", Enabled: &disabled},
		}
	})
}

// The merged list: chart-defined rows carry source "config", UI rows carry
// source "ui", and neither carries a secret.
func TestListWebhooksMergesBothSourcesWithoutSecrets(t *testing.T) {
	h := newHarness(t, withStaticWebhooks())

	var created wireWebhook
	h.decode(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"name": "team-chat", "url": "https://chat.example/hook", "secret": "ui-secret", "enabled": true,
	}, http.StatusCreated, &created)
	if created.Source != api.SourceUI || created.ID == "" {
		t.Fatalf("created = %+v", created)
	}

	status, raw := h.raw(http.MethodGet, "/api/v1/webhooks", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	// The strongest check available: no secret VALUE and no secret FIELD
	// appears anywhere in the response, for either source (§4.7).
	for _, forbidden := range []string{"chart-secret", "ui-secret", "secret"} {
		if strings.Contains(strings.ToLower(raw), forbidden) {
			t.Fatalf("webhook list leaks %q: %s", forbidden, raw)
		}
	}

	var body struct {
		Webhooks []wireWebhook `json:"webhooks"`
	}
	h.decode(http.MethodGet, "/api/v1/webhooks", nil, http.StatusOK, &body)
	if len(body.Webhooks) != 3 {
		t.Fatalf("webhooks = %+v", body.Webhooks)
	}
	bySource := map[string]int{}
	for _, wh := range body.Webhooks {
		bySource[wh.Source]++
		if wh.Source == api.SourceConfig && wh.ID != "" {
			t.Fatalf("a config-sourced webhook must have no database identity: %+v", wh)
		}
		if wh.Name == "parked" && wh.Enabled {
			t.Fatalf("a chart-parked webhook must render as disabled: %+v", wh)
		}
		if wh.Name == "paging" && !wh.Enabled {
			t.Fatalf("a chart webhook with no explicit enabled must render as enabled: %+v", wh)
		}
	}
	if bySource[api.SourceConfig] != 2 || bySource[api.SourceUI] != 1 {
		t.Fatalf("sources = %v", bySource)
	}
}

// Any write addressing a chart-defined webhook is 409 source_immutable (D9) —
// by name and by the path segment the portal would send.
func TestWritesToConfigWebhooksAreRefused(t *testing.T) {
	h := newHarness(t, withStaticWebhooks())

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"create with a config name", http.MethodPost, "/api/v1/webhooks",
			map[string]any{"name": "paging", "url": "https://elsewhere.example", "secret": "x", "enabled": true}},
		{"update by config name", http.MethodPut, "/api/v1/webhooks/paging",
			map[string]any{"name": "paging", "url": "https://elsewhere.example", "enabled": false}},
		{"delete by config name", http.MethodDelete, "/api/v1/webhooks/paging", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code := h.errorCode(tc.method, tc.path, tc.body, http.StatusConflict)
			if code != api.CodeSourceImmutable {
				t.Fatalf("code = %q, want %q", code, api.CodeSourceImmutable)
			}
		})
	}

	// Renaming a UI webhook ONTO a config name is the same refusal: names are
	// unique across both sources, and the database cannot see the config half.
	var created wireWebhook
	h.decode(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"name": "team-chat", "url": "https://chat.example/hook", "secret": "s", "enabled": true,
	}, http.StatusCreated, &created)
	code := h.errorCode(http.MethodPut, "/api/v1/webhooks/"+created.ID, map[string]any{
		"name": "paging", "url": "https://chat.example/hook", "enabled": true,
	}, http.StatusConflict)
	if code != api.CodeSourceImmutable {
		t.Fatalf("rename-onto-config code = %q", code)
	}
}

func TestWebhookCRUDLifecycle(t *testing.T) {
	h := newHarness(t)

	var created wireWebhook
	h.decode(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"name": "ntfy", "url": "https://ntfy.example/gawk", "secret": "s3cr3t", "enabled": true,
	}, http.StatusCreated, &created)

	// A duplicate UI name is a different conflict from a config collision.
	if code := h.errorCode(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"name": "ntfy", "url": "https://other.example", "secret": "x", "enabled": true,
	}, http.StatusConflict); code != api.CodeDuplicateName {
		t.Fatalf("duplicate UI name code = %q", code)
	}

	// Update without a secret keeps the stored key.
	var updated wireWebhook
	h.decode(http.MethodPut, "/api/v1/webhooks/"+created.ID, map[string]any{
		"name": "ntfy", "url": "https://ntfy.example/v2", "enabled": false,
	}, http.StatusOK, &updated)
	if updated.URL != "https://ntfy.example/v2" || updated.Enabled {
		t.Fatalf("updated = %+v", updated)
	}
	full, err := h.store.GetWebhookByName(t.Context(), "ntfy")
	if err != nil || full.Secret != "s3cr3t" {
		t.Fatalf("secret after a secret-less update = %q (err=%v)", full.Secret, err)
	}

	h.decode(http.MethodDelete, "/api/v1/webhooks/"+created.ID, nil, http.StatusNoContent, nil)
	if code := h.errorCode(http.MethodDelete, "/api/v1/webhooks/"+created.ID, nil, http.StatusNotFound); code != api.CodeNotFound {
		t.Fatalf("delete of a gone webhook code = %q", code)
	}
}

func TestWebhookValidation(t *testing.T) {
	h := newHarness(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"blank name", map[string]any{"name": "  ", "url": "https://x.example", "secret": "s", "enabled": true}},
		{"relative url", map[string]any{"name": "a", "url": "/hook", "secret": "s", "enabled": true}},
		{"missing secret on create", map[string]any{"name": "a", "url": "https://x.example", "enabled": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code := h.errorCode(http.MethodPost, "/api/v1/webhooks", tc.body, http.StatusBadRequest); code != api.CodeBadRequest {
				t.Fatalf("code = %q", code)
			}
		})
	}
	// An unknown field is rejected rather than silently ignored: on an
	// enforcement API a mistyped knob must not quietly take a default.
	if code := h.errorCode(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"name": "a", "url": "https://x.example", "secret": "s", "enabled": true, "enabledd": true,
	}, http.StatusBadRequest); code != api.CodeBadRequest {
		t.Fatalf("unknown-field code = %q", code)
	}
}

// Test-send works for BOTH sources: a chart-defined webhook is immutable here,
// not untestable (§4.7).
func TestTestSendWorksForBothSources(t *testing.T) {
	h := newHarness(t, withStaticWebhooks())

	var created wireWebhook
	h.decode(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"name": "team-chat", "url": "https://chat.example/hook", "secret": "s", "enabled": true,
	}, http.StatusCreated, &created)

	for _, name := range []string{"paging", "team-chat"} {
		var result struct {
			OK         bool   `json:"ok"`
			Status     int    `json:"status"`
			DeliveryID string `json:"deliveryId"`
		}
		h.decode(http.MethodPost, "/api/v1/webhooks/"+name+"/test", map[string]any{}, http.StatusOK, &result)
		if !result.OK || result.Status != 200 || result.DeliveryID == "" {
			t.Fatalf("test result for %s = %+v", name, result)
		}
	}
	if len(h.test.names) != 2 || h.test.names[0] != "paging" || h.test.names[1] != "team-chat" {
		t.Fatalf("the dispatcher was asked for %v", h.test.names)
	}

	if code := h.errorCode(http.MethodPost, "/api/v1/webhooks/nonesuch/test", map[string]any{},
		http.StatusNotFound); code != api.CodeNotFound {
		t.Fatalf("unknown webhook test code = %q", code)
	}
}

// With no dispatcher wired, a test-send says so rather than pretending it
// delivered — the default Tester refuses instead of lying.
func TestTestSendWithoutADispatcher(t *testing.T) {
	h := newHarness(t, withStaticWebhooks(), func(o *api.Options, _ *harness) { o.Tester = nil })
	code := h.errorCode(http.MethodPost, "/api/v1/webhooks/paging/test", map[string]any{}, http.StatusServiceUnavailable)
	if code != api.CodeUnavailable {
		t.Fatalf("code = %q", code)
	}
}
