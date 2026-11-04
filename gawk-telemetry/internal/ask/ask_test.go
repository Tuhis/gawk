package ask

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/schema"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/sqlengine"
)

// NQ2: a binder error reaches the model, and the fixed draft is what the
// operator gets.
func TestABinderErrorReachesTheModelAndTheFixIsReturned(t *testing.T) {
	bad := "SELECT plaform FROM rollups"
	good := "SELECT os FROM rollups"
	f := newFake(t,
		anthropicToolUse("toolu_1", sqlInput(bad)),
		anthropicToolUse("toolu_2", sqlInput(good)),
		anthropicFinal("Each session's platform.", good))
	verdict := `ERROR: Binder Error: Referenced column "plaform" not found in FROM clause! Candidate bindings: "os"`
	c := &fakeChecker{verdicts: map[string]string{bad: verdict}}
	s := newAnthropicService(t, f, c, nil)
	ans, err := s.Ask(context.Background(), "alice", "platforms")
	if err != nil {
		t.Fatal(err)
	}
	if ans.SQL != good || ans.Checks != 2 || ans.Check != "OK" {
		t.Errorf("answer = %+v", ans)
	}
	if !strings.Contains(f.requests()[1], `Referenced column \"plaform\" not found`) {
		t.Error("the binder error did not reach the model")
	}
	// The final SQL was the last one checked, so it is not checked again.
	if len(c.asked) != 2 {
		t.Errorf("checker asked %d times: %q", len(c.asked), c.asked)
	}
}

// The operator is told when the draft would not bind, rather than finding
// out from Run.
func TestTheFinalSQLsVerdictIsReported(t *testing.T) {
	f := newFake(t, anthropicFinal("x", "SELECT nope FROM rollups"))
	c := &fakeChecker{verdicts: map[string]string{"SELECT nope FROM rollups": "ERROR: Binder Error: nope"}}
	s := newAnthropicService(t, f, c, nil)
	ans, err := s.Ask(context.Background(), "alice", "q")
	if err != nil {
		t.Fatal(err)
	}
	if ans.Check != "ERROR: Binder Error: nope" || ans.Checks != 0 {
		t.Errorf("answer = %+v", ans)
	}
}

// A final check that could not run is not passed off as a pass.
func TestAFinalCheckThatFailedIsSaid(t *testing.T) {
	f := newFake(t, anthropicFinal("x", "SELECT 1"))
	s := newAnthropicService(t, f, &fakeChecker{err: errors.New("worker gone")}, nil)
	ans, err := s.Ask(context.Background(), "alice", "q")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ans.Check, "NOT CHECKED") {
		t.Errorf("check = %q", ans.Check)
	}
}

// A question the operator abandoned is not reported as a timeout.
func TestACancelledQuestionIsNotATimeout(t *testing.T) {
	f := newFake(t, reply{delay: 5 * time.Second, body: "{}"})
	s := newAnthropicService(t, f, &fakeChecker{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := s.Ask(ctx, "alice", "q")
	if err == nil || strings.Contains(err.Error(), "no answer within") {
		t.Fatalf("err = %v", err)
	}
}

func TestADraftTheConsoleRefusesIsNotOffered(t *testing.T) {
	f := newFake(t, anthropicFinal("Delete it.", "COPY rollups TO '/tmp/x'"))
	s := newAnthropicService(t, f, &fakeChecker{}, nil)
	if _, err := s.Ask(context.Background(), "alice", "q"); !errors.Is(err, ErrNoDraft) {
		t.Fatalf("err = %v", err)
	}
}

func TestAnAnswerWithoutSQLSaysSo(t *testing.T) {
	f := newFake(t, anthropicMessage("end_turn", `{"type":"text","text":"I cannot answer that from telemetry."}`))
	s := newAnthropicService(t, f, &fakeChecker{}, nil)
	_, err := s.Ask(context.Background(), "alice", "q")
	if !errors.Is(err, ErrNoDraft) || !strings.Contains(err.Error(), "I cannot answer that") {
		t.Fatalf("err = %v", err)
	}
}

// NQ2: the prompt is byte-stable between two questions, so it caches.
func TestThePromptIsByteStableBetweenQuestions(t *testing.T) {
	f := newFake(t, anthropicFinal("One.", "SELECT 1"))
	s := newAnthropicService(t, f, &fakeChecker{}, nil)
	for _, q := range []string{"first question", "a different second question"} {
		if _, err := s.Ask(context.Background(), "alice", q); err != nil {
			t.Fatal(err)
		}
	}
	system := func(body string) string {
		i := strings.Index(body, `"system":`)
		j := strings.Index(body, `"tools":`)
		if i < 0 || j < i {
			t.Fatalf("no system prompt in %s", body)
		}
		return body[i:j]
	}
	reqs := f.requests()
	if system(reqs[0]) != system(reqs[1]) {
		t.Error("the system prompt changed between questions")
	}
}

func TestThePromptFollowsTheLearnedSchema(t *testing.T) {
	c := &fakeChecker{views: []sqlengine.ViewSchema{{Name: "rollups", Columns: []sqlengine.Column{{Name: "os", Type: "VARCHAR"}}}}}
	f := newFake(t, anthropicFinal("One.", "SELECT 1"))
	s := newAnthropicService(t, f, c, nil)
	_, _ = s.Ask(context.Background(), "alice", "q1")
	c.views = []sqlengine.ViewSchema{{Name: "rollups", Columns: []sqlengine.Column{{Name: "os", Type: "VARCHAR"}, {Name: "newField", Type: "BIGINT"}}}}
	_, _ = s.Ask(context.Background(), "alice", "q2")
	reqs := f.requests()
	if strings.Contains(reqs[0], "newField") || !strings.Contains(reqs[1], "- newField BIGINT") {
		t.Error("the prompt did not pick up a newly learned column")
	}
}

func TestPromptContents(t *testing.T) {
	views := []sqlengine.ViewSchema{
		{Name: "rollups", Columns: []sqlengine.Column{{Name: "os", Type: "VARCHAR"}}},
		{Name: "sessions", Columns: []sqlengine.Column{{Name: "tMs", Type: "BIGINT"}}},
	}
	full := buildPrompt(promptInput{Today: "2026-11-05", Views: views, Context: ContextFull})
	slim := buildPrompt(promptInput{Today: "2026-11-05", Views: views, Context: ContextSlim})
	for _, want := range []string{"Today is 2026-11-05.", "- os VARCHAR", "- tMs BIGINT", "### Field catalogue", "receivedFps (", schema.Glossary, "### relay", "no columns learned yet"} {
		if !strings.Contains(full, want) {
			t.Errorf("full prompt lacks %q", want)
		}
	}
	for _, want := range []string{"- os VARCHAR", schema.Glossary, "(columns not listed here; prefer rollups)"} {
		if !strings.Contains(slim, want) {
			t.Errorf("slim prompt lacks %q", want)
		}
	}
	for _, not := range []string{"- tMs BIGINT", "### Field catalogue"} {
		if strings.Contains(slim, not) {
			t.Errorf("slim prompt has %q", not)
		}
	}
	// Legacy spellings are not taught.
	for _, f := range schema.Catalogue() {
		if f.Legacy && strings.Contains(full, "\n- "+f.Name+" (") {
			t.Errorf("full prompt lists legacy field %s", f.Name)
		}
	}
	if len(slim) > len(full)/2 {
		t.Errorf("slim (%d bytes) is not much smaller than full (%d)", len(slim), len(full))
	}
}

func TestContextDefaultsByProvider(t *testing.T) {
	a, _ := NewAnthropic(AnthropicOptions{APIKey: "k"})
	o, _ := NewOpenAI(OpenAIOptions{BaseURL: "http://x", Model: "m"})
	sa, _ := New(Options{Provider: a, Checker: &fakeChecker{}})
	so, _ := New(Options{Provider: o, Checker: &fakeChecker{}})
	if sa.Status().Context != ContextFull || so.Status().Context != ContextSlim {
		t.Errorf("contexts = %s, %s", sa.Status().Context, so.Status().Context)
	}
	if _, err := New(Options{Provider: a, Checker: &fakeChecker{}, Context: "medium"}); err == nil {
		t.Error("an unknown context accepted")
	}
	if sa.Status().Model != DefaultAnthropicModel {
		t.Errorf("default model = %s", sa.Status().Model)
	}
}

// NQ2: the per-operator hourly limit.
func TestPerOperatorRateLimit(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := newFake(t, anthropicFinal("One.", "SELECT 1"))
	s := newAnthropicService(t, f, &fakeChecker{}, func(o *Options) {
		o.RatePerHour = 2
		o.Now = func() time.Time { return now }
	})
	for range 2 {
		if _, err := s.Ask(context.Background(), "alice", "q"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.Ask(context.Background(), "alice", "q")
	var rl *RateLimitError
	if !errors.As(err, &rl) || !errors.Is(err, ErrRateLimited) || rl.RetryAfter != time.Hour {
		t.Fatalf("third question: %v", err)
	}
	// Another operator has their own budget.
	if _, err := s.Ask(context.Background(), "bob", "q"); err != nil {
		t.Errorf("bob: %v", err)
	}
	// An hour on, alice may ask again.
	now = now.Add(time.Hour + time.Second)
	if _, err := s.Ask(context.Background(), "alice", "q"); err != nil {
		t.Errorf("an hour later: %v", err)
	}
	if n := len(f.requests()); n != 4 {
		t.Errorf("%d provider requests, want 4: a refused question must not reach the provider", n)
	}
}

// One question at a time per operator.
func TestOneQuestionInFlightPerOperator(t *testing.T) {
	f := newFake(t, reply{delay: 500 * time.Millisecond, body: anthropicFinal("One.", "SELECT 1").body})
	s := newAnthropicService(t, f, &fakeChecker{}, nil)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = s.Ask(context.Background(), "alice", "slow")
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := s.Ask(context.Background(), "alice", "again"); !errors.Is(err, ErrBusy) {
		t.Errorf("second question in flight: %v", err)
	}
	if _, err := s.Ask(context.Background(), "bob", "own"); err != nil {
		t.Errorf("bob blocked by alice: %v", err)
	}
	wg.Wait()
}

func TestBadQuestions(t *testing.T) {
	s := newAnthropicService(t, newFake(t, anthropicFinal("x", "SELECT 1")), &fakeChecker{}, nil)
	for _, q := range []string{"", "   ", strings.Repeat("x", MaxQuestionBytes+1), "\xff\xfe"} {
		if _, err := s.Ask(context.Background(), "alice", q); !errors.Is(err, ErrBadQuestion) {
			t.Errorf("%q: %v", q, err)
		}
	}
}

// A checker that fails is not passed off as the model's fault, and its
// failure text never reaches the model.
func TestACheckerFailureIsReportedNotSentToTheModel(t *testing.T) {
	f := newFake(t, anthropicToolUse("toolu_1", sqlInput("SELECT 1")), anthropicFinal("One.", "SELECT 1"))
	c := &fakeChecker{err: errors.New("worker crashed at /data/secret/path")}
	s := newAnthropicService(t, f, c, nil)
	_, _ = s.Ask(context.Background(), "alice", "q")
	for _, r := range f.requests() {
		if strings.Contains(r, "/data/secret/path") {
			t.Fatal("the checker's own error reached the model")
		}
	}
}

// D9: each ask is logged with who, what and how much — and no result, since
// there is none.
func TestAskIsLogged(t *testing.T) {
	var buf bytes.Buffer
	f := newFake(t, anthropicToolUse("toolu_1", sqlInput("SELECT 1")), anthropicFinal("One.", "SELECT 1"))
	s := newAnthropicService(t, f, &fakeChecker{}, func(o *Options) { o.Log = slog.New(slog.NewJSONHandler(&buf, nil)) })
	if _, err := s.Ask(context.Background(), "alice@example", "how many?"); err != nil {
		t.Fatal(err)
	}
	line := buf.String()
	for _, want := range []string{`"msg":"ask answered"`, `"operator":"alice@example"`, `"provider":"anthropic"`, `"model":"claude-sonnet-5-5"`,
		`"question":"how many?"`, `"sql":"SELECT 1"`, `"checks":1`, `"input_tokens":240`, `"duration_ms"`} {
		if !strings.Contains(line, want) {
			t.Errorf("log lacks %s: %s", want, line)
		}
	}
}

func TestParseFinal(t *testing.T) {
	for _, c := range []struct{ in, interp, sql string }{
		{"INTERPRETATION: x\nSQL:\n```sql\nSELECT 1\n```", "x", "SELECT 1"},
		{"Some preamble.\nINTERPRETATION:   y  \nSQL:\n```\nSELECT 2;\n```\ntrailer", "y", "SELECT 2;"},
		{"no format at all", "", ""},
	} {
		i, s := parseFinal(c.in)
		if i != c.interp || s != c.sql {
			t.Errorf("parseFinal(%q) = %q, %q", c.in, i, s)
		}
	}
}
