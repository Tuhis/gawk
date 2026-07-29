package readapi

// TH10's HTTP surface over the query engine (docs/36 UD18).
//
// The endpoint exists in every build. What changes with the build tag is the
// ANSWER: a build with no engine says so in a shape the console can render as
// an explanation, rather than 404ing and leaving an editor that silently does
// nothing. "There is no engine here" and "your query was wrong" must never look
// the same on screen.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/sqlengine"
)

// SQLEngine is TH10's engine as this package needs it. An interface so the API
// can be built without one, and so a test can drive the endpoint's refusal and
// error paths without a C toolchain.
type SQLEngine interface {
	Query(sql string) (*sqlengine.Result, error)
	Views() []sqlengine.ViewDoc
}

// QueryStatus is what the console asks for before it renders anything.
type QueryStatus struct {
	// Enabled is whether an engine is actually wired here. The flag being on is
	// not the same thing (§8 Q1) and the console must not conflate them.
	Enabled bool                `json:"enabled"`
	Reason  string              `json:"reason,omitempty"`
	Views   []sqlengine.ViewDoc `json:"views,omitempty"`
	// RowLimit and TimeoutMs are stated up front so a truncated result is not a
	// surprise the operator has to infer.
	RowLimit  int   `json:"rowLimit"`
	TimeoutMs int64 `json:"timeoutMs"`
}

// QueryStatus reports whether the console can work here.
func (a *API) QueryStatus() QueryStatus {
	s := QueryStatus{
		RowLimit:  sqlengine.DefaultRowLimit,
		TimeoutMs: sqlengine.DefaultTimeout.Milliseconds(),
	}
	if a.sql == nil {
		s.Reason = sqlengine.ErrNoEngine.Error()
		s.Views = sqlengine.Views()
		return s
	}
	s.Enabled = true
	s.Views = a.sql.Views()
	return s
}

func (a *API) handleQuery(w http.ResponseWriter, r *http.Request) {
	if a.sql == nil {
		// 501, not 500: the request was fine and the deployment is not. The
		// console renders the body as prose.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(a.QueryStatus())
		return
	}
	var body struct {
		SQL string `json:"sql"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&body); err != nil {
		http.Error(w, "malformed query request", http.StatusBadRequest)
		return
	}
	res, err := a.sql.Query(body.SQL)
	if err != nil {
		// A refusal and a syntax error are both the caller's problem, and both
		// must come back readable rather than as a bare 500 — TH10's "fails
		// safely with a readable message".
		status := http.StatusBadRequest
		if !errors.Is(err, sqlengine.ErrRefused) && !errors.Is(err, sqlengine.ErrNoEngine) {
			status = http.StatusUnprocessableEntity
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, res, nil)
}
