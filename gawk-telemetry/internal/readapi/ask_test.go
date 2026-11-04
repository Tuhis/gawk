package readapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/oidcauth"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/ask"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/sqlengine"
)

type stubEngine struct{}

func (stubEngine) Query(string) (*sqlengine.Result, error) { return &sqlengine.Result{}, nil }
func (stubEngine) Views() []sqlengine.ViewDoc              { return nil }

type stubAsker struct {
	operator, question string
	ans                *ask.Answer
	err                error
}

func (s *stubAsker) Ask(_ context.Context, operator, question string) (*ask.Answer, error) {
	s.operator, s.question = operator, question
	return s.ans, s.err
}

func (s *stubAsker) Status() ask.Status {
	return ask.Status{Provider: "anthropic", Model: "claude-sonnet-5-5", Context: "full", External: true, Endpoint: "api.anthropic.com"}
}

func askAPI(t *testing.T, opts Options) *httptest.Server {
	t.Helper()
	f := newFixture(t)
	opts.Store = f.store
	api, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func queryStatus(t *testing.T, srv *httptest.Server) QueryStatus {
	t.Helper()
	resp, err := http.Get(srv.URL + "/v1/query")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var s QueryStatus
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func postAsk(t *testing.T, srv *httptest.Server, body string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/query/ask", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// D8: an install that does not enable the box reports it off, and the
// endpoint says which half is missing.
func TestAskIsOffByDefault(t *testing.T) {
	srv := askAPI(t, Options{SQL: stubEngine{}})
	s := queryStatus(t, srv)
	if s.Ask.Enabled || s.Ask.Provider != "none" || s.Ask.Reason != "" {
		t.Errorf("ask = %+v", s.Ask)
	}
	code, body, _ := postAsk(t, srv, `{"question":"q"}`)
	if code != http.StatusNotImplemented || !strings.Contains(body, "no model is configured") {
		t.Errorf("POST = %d %s", code, body)
	}
}

// "No engine" and "no model" are different answers.
func TestAskWithoutAnEngineSaysSo(t *testing.T) {
	srv := askAPI(t, Options{Ask: &stubAsker{}, AskProvider: "anthropic"})
	s := queryStatus(t, srv)
	if s.Ask.Enabled || s.Ask.Provider != "anthropic" || !strings.Contains(s.Ask.Reason, "needs the query engine") {
		t.Errorf("ask = %+v", s.Ask)
	}
	code, body, _ := postAsk(t, srv, `{"question":"q"}`)
	if code != http.StatusNotImplemented || !strings.Contains(body, "no query engine here") {
		t.Errorf("POST = %d %s", code, body)
	}
}

func TestAskReportsTheProviderAndAnswersADraft(t *testing.T) {
	a := &stubAsker{ans: &ask.Answer{Interpretation: "Sessions per os.", SQL: "SELECT os, count(*) FROM rollups GROUP BY os", Check: "OK", Model: "claude-sonnet-5-5", Provider: "anthropic", Checks: 1}}
	srv := askAPI(t, Options{SQL: stubEngine{}, Ask: a, AskProvider: "anthropic"})
	s := queryStatus(t, srv)
	if !s.Ask.Enabled || s.Ask.Provider != "anthropic" || s.Ask.Model != "claude-sonnet-5-5" || !s.Ask.External {
		t.Errorf("ask = %+v", s.Ask)
	}
	code, body, _ := postAsk(t, srv, `{"question":"how many per os?"}`)
	if code != http.StatusOK {
		t.Fatalf("POST = %d %s", code, body)
	}
	var ans ask.Answer
	if err := json.Unmarshal([]byte(body), &ans); err != nil || ans.SQL != a.ans.SQL || ans.Interpretation != "Sessions per os." {
		t.Errorf("answer = %s (%v)", body, err)
	}
	if a.question != "how many per os?" || a.operator != "anonymous" {
		t.Errorf("asked %q as %q", a.question, a.operator)
	}
}

func TestAskErrorsAreReadable(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
	}{
		{ask.ErrBadQuestion, 400},
		{ask.ErrBusy, 429},
		{&ask.RateLimitError{RetryAfter: 90 * time.Second}, 429},
		{ask.ErrRefused, 422},
		{ask.ErrNoDraft, 422},
		{&ask.ProviderError{Provider: "anthropic", Status: 529, Err: errors.New("overloaded")}, 502},
		{context.DeadlineExceeded, 504},
	} {
		srv := askAPI(t, Options{SQL: stubEngine{}, Ask: &stubAsker{err: c.err}, AskProvider: "anthropic"})
		code, body, h := postAsk(t, srv, `{"question":"q"}`)
		if code != c.status || strings.TrimSpace(body) != c.err.Error() {
			t.Errorf("%v: %d %q", c.err, code, body)
		}
		if _, rl := c.err.(*ask.RateLimitError); rl && h.Get("Retry-After") != "90" {
			t.Errorf("Retry-After = %q", h.Get("Retry-After"))
		}
	}
}

func TestAskOperatorIdentity(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/query/ask", nil)
	if AskOperator(r) != "anonymous" {
		t.Error(AskOperator(r))
	}
	r.SetBasicAuth("ops", "pw")
	if AskOperator(r) != "basic:ops" {
		t.Error(AskOperator(r))
	}
	r = r.WithContext(oidcauth.NewContext(r.Context(), oidcauth.Identity{Subject: "sub-123"}))
	if AskOperator(r) != "oidc:sub-123" {
		t.Error(AskOperator(r))
	}
}

func TestAskRejectsAMalformedBody(t *testing.T) {
	srv := askAPI(t, Options{SQL: stubEngine{}, Ask: &stubAsker{}, AskProvider: "anthropic"})
	if code, _, _ := postAsk(t, srv, `not json`); code != 400 {
		t.Errorf("code = %d", code)
	}
}
