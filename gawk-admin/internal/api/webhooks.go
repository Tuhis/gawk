package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/Tuhis/gawk/gawk-admin/internal/config"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
)

type webhookRequest struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Secret is WRITE-ONLY. It is required on create and optional on update,
	// where an empty value keeps the stored key — the API never returns a
	// secret, so the portal's edit form cannot round-trip one (§4.7).
	Secret  string `json:"secret,omitempty"`
	Enabled bool   `json:"enabled"`
}

// handleListWebhooks merges the two sources (D9): chart-defined webhooks from
// -static-webhooks, which are visible and immutable here, and UI-created ones
// from Postgres.
//
// Neither carries a secret in the response, and webhookJSON has no field to
// put one in.
func (a *API) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	rows, err := a.opts.Store.ListWebhooks(r.Context())
	if err != nil {
		a.fail(w, r, "list webhooks", err)
		return
	}
	out := make([]webhookJSON, 0, len(rows)+len(a.opts.Config.StaticWebhooks))
	for _, h := range a.opts.Config.StaticWebhooks {
		out = append(out, webhookJSON{Name: h.Name, URL: h.URL, Enabled: h.IsEnabled(), Source: SourceConfig})
	}
	for _, h := range rows {
		out = append(out, webhookJSON{ID: h.ID.String(), Name: h.Name, URL: h.URL, Enabled: h.Enabled, Source: SourceUI})
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": out})
}

func (a *API) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	var req webhookRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	name, ok := a.validateWebhook(w, req, true)
	if !ok {
		return
	}
	created, err := a.opts.Store.CreateWebhook(r.Context(), store.Webhook{
		Name: name, URL: strings.TrimSpace(req.URL), Secret: req.Secret,
		Enabled: req.Enabled, CreatedAt: a.now(), CreatedBy: id.Actor(),
	})
	if err != nil {
		a.fail(w, r, "create the webhook", err)
		return
	}
	a.log.Info("webhook created", "name", created.Name, "actor", id.Actor())
	writeJSON(w, http.StatusCreated, webhookJSON{
		ID: created.ID.String(), Name: created.Name, URL: created.URL,
		Enabled: created.Enabled, Source: SourceUI,
	})
}

func (a *API) handleUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	rowID, ok := a.resolveWebhookID(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req webhookRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	name, ok := a.validateWebhook(w, req, false)
	if !ok {
		return
	}
	updated, err := a.opts.Store.UpdateWebhook(r.Context(), store.Webhook{
		ID: rowID, Name: name, URL: strings.TrimSpace(req.URL), Secret: req.Secret, Enabled: req.Enabled,
	})
	if err != nil {
		a.fail(w, r, "update the webhook", err)
		return
	}
	a.log.Info("webhook updated", "name", updated.Name, "actor", id.Actor())
	writeJSON(w, http.StatusOK, webhookJSON{
		ID: updated.ID.String(), Name: updated.Name, URL: updated.URL,
		Enabled: updated.Enabled, Source: SourceUI,
	})
}

func (a *API) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	rowID, ok := a.resolveWebhookID(w, r.PathValue("id"))
	if !ok {
		return
	}
	if err := a.opts.Store.DeleteWebhook(r.Context(), rowID); err != nil {
		a.fail(w, r, "delete the webhook", err)
		return
	}
	a.log.Info("webhook deleted", "id", rowID, "actor", id.Actor())
	w.WriteHeader(http.StatusNoContent)
}

// handleTestWebhook sends a synthetic signed event. It works for BOTH sources
// (§4.7): a chart-defined webhook is immutable here, not untestable — an
// operator must be able to prove the paging pipe works without a redeploy.
//
// The send itself is AP7's: this handler only resolves the name and delegates.
func (a *API) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if _, ok := a.staticWebhook(name); !ok {
		if _, err := a.opts.Store.GetWebhookByName(r.Context(), name); err != nil {
			if _, _, known := storeStatus(err); known {
				writeError(w, http.StatusNotFound, CodeNotFound, "no such webhook")
				return
			}
			a.fail(w, r, "look up the webhook", err)
			return
		}
	}
	result, err := a.opts.Tester.TestWebhook(r.Context(), name)
	if err != nil {
		a.log.Warn("webhook test send failed", "name", name, "actor", id.Actor(), "err", err)
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// resolveWebhookID turns a path segment into a UI webhook's row ID.
//
// A segment that is not a UUID but names a CONFIG webhook is the case worth
// getting right: the portal renders both sources in one table, and a write
// aimed at a chart-defined row must say WHY it is refused (409
// source_immutable, D9) rather than 404 "no such webhook", which would read as
// a bug in the portal.
func (a *API) resolveWebhookID(w http.ResponseWriter, raw string) (uuid.UUID, bool) {
	if _, ok := a.staticWebhook(raw); ok {
		a.refuseConfigWrite(w, raw)
		return uuid.Nil, false
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such webhook")
		return uuid.Nil, false
	}
	return parsed, true
}

// validateWebhook checks a create/update body and enforces the cross-source
// name uniqueness §4.6 says is "enforced in code" — the database cannot see
// chart-defined names, so this is the only place that check can live.
func (a *API) validateWebhook(w http.ResponseWriter, req webhookRequest, create bool) (string, bool) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "name is required")
		return "", false
	}
	if _, ok := a.staticWebhook(name); ok {
		a.refuseConfigWrite(w, name)
		return "", false
	}
	target := strings.TrimSpace(req.URL)
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "url must be an absolute URL")
		return "", false
	}
	if create && strings.TrimSpace(req.Secret) == "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest,
			"secret is required: every webhook is signed with its own key")
		return "", false
	}
	return name, true
}

func (a *API) refuseConfigWrite(w http.ResponseWriter, name string) {
	writeError(w, http.StatusConflict, CodeSourceImmutable,
		"webhook "+name+" is defined by chart values and cannot be changed from the portal")
}

func (a *API) staticWebhook(name string) (config.StaticWebhook, bool) {
	for _, h := range a.opts.Config.StaticWebhooks {
		if h.Name == name {
			return h, true
		}
	}
	return config.StaticWebhook{}, false
}
