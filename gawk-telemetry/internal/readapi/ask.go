package readapi

// R82's question box over the SQL console (docs/84 D1, D3, D8).
//
// POST /v1/query/ask takes an operator's question and answers with a DRAFT:
// SQL and one sentence on how the question was read. Nothing is run here; the
// operator reads the draft and runs it through POST /v1/query, under the
// console's allowlist and budget, as if they had written it.
//
// The box needs two things, and the answer says which one is missing: an
// engine (no engine, no console at all) and a provider (off by default). "No
// engine here", "no model configured" and "the model could not answer" are
// three different facts and never look the same on screen.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Tuhis/gawk/gawk-server/oidcauth"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/ask"
)

// Asker is the ask service as this package needs it.
type Asker interface {
	Ask(ctx context.Context, operator, question string) (*ask.Answer, error)
	Status() ask.Status
}

// AskStatus is the box's half of QueryStatus.
type AskStatus struct {
	// Enabled is whether the box can answer: a provider AND an engine.
	Enabled  bool   `json:"enabled"`
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	Context  string `json:"context,omitempty"`
	// External is a provider outside the cluster: the question leaves it
	// (rows never do), and the console says so under the box.
	External bool   `json:"external,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	// Reason is why a configured box cannot answer.
	Reason string `json:"reason,omitempty"`
}

// askStatus reports the box.
func (a *API) askStatus() AskStatus {
	switch {
	case a.ask != nil && a.sql != nil:
		s := a.ask.Status()
		return AskStatus{Enabled: true, Provider: s.Provider, Model: s.Model, Context: s.Context, External: s.External, Endpoint: s.Endpoint}
	case a.askProvider == "" || a.askProvider == "none":
		return AskStatus{Provider: "none"}
	default:
		reason := "it needs the query engine, which this deployment does not have"
		if a.sql == nil {
			r, _ := a.sqlUnavailable()
			reason += ": " + r
		}
		return AskStatus{Provider: a.askProvider, Reason: reason}
	}
}

// maxAskBody bounds the request: a question is at most ask.MaxQuestionBytes.
const maxAskBody = 16 << 10

// AskOperator is who is asking, for D7's per-operator limits and D9's log:
// the OIDC subject, the basic-auth user, or "anonymous" on an open listener.
func AskOperator(r *http.Request) string {
	if id, ok := oidcauth.FromContext(r.Context()); ok && id.Subject != "" {
		return "oidc:" + id.Subject
	}
	if u, _, ok := r.BasicAuth(); ok && u != "" {
		return "basic:" + u
	}
	return "anonymous"
}

func (a *API) handleAsk(w http.ResponseWriter, r *http.Request) {
	if a.sql == nil {
		reason, _ := a.sqlUnavailable()
		http.Error(w, "no query engine here, so there is nothing to ask: "+reason, http.StatusNotImplemented)
		return
	}
	if a.ask == nil {
		http.Error(w, "no model is configured on this deployment (-ask-provider); the box is off", http.StatusNotImplemented)
		return
	}
	var body struct {
		Question string `json:"question"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAskBody)).Decode(&body); err != nil {
		http.Error(w, "malformed ask request", http.StatusBadRequest)
		return
	}
	ans, err := a.ask.Ask(r.Context(), AskOperator(r), body.Question)
	if err != nil {
		status := http.StatusBadGateway
		var rl *ask.RateLimitError
		var pe *ask.ProviderError
		switch {
		case errors.Is(err, ask.ErrBadQuestion):
			status = http.StatusBadRequest
		case errors.As(err, &rl):
			w.Header().Set("Retry-After", strconv.Itoa(int(rl.RetryAfter.Round(time.Second)/time.Second)))
			status = http.StatusTooManyRequests
		case errors.Is(err, ask.ErrBusy):
			status = http.StatusTooManyRequests
		case errors.Is(err, ask.ErrRefused), errors.Is(err, ask.ErrNoDraft):
			// The model answered; the answer was not a usable draft.
			status = http.StatusUnprocessableEntity
		case errors.Is(err, context.DeadlineExceeded):
			status = http.StatusGatewayTimeout
		case errors.As(err, &pe):
			status = http.StatusBadGateway
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, ans, nil)
}
