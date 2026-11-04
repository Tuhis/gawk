package ask

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// NQ1: a tool call round trip against the recorded Messages API shape.
func TestAnthropicToolCallRoundTrip(t *testing.T) {
	f := newFake(t,
		anthropicToolUse("toolu_01", sqlInput("SELECT os, count(*) FROM rollups GROUP BY os")),
		anthropicFinal("Sessions per platform.", "SELECT os, count(*) AS sessions FROM rollups GROUP BY os"))
	c := &fakeChecker{}
	s := newAnthropicService(t, f, c, nil)

	ans, err := s.Ask(context.Background(), "alice", "how many sessions per os?")
	if err != nil {
		t.Fatal(err)
	}
	if ans.SQL != "SELECT os, count(*) AS sessions FROM rollups GROUP BY os" || ans.Interpretation != "Sessions per platform." {
		t.Errorf("answer = %+v", ans)
	}
	if ans.Checks != 1 || ans.Check != "OK" || ans.Model != "claude-sonnet-5-5" || ans.Provider != "anthropic" {
		t.Errorf("answer = %+v", ans)
	}
	if ans.Usage.CacheReadTokens != 34000 || ans.Usage.InputTokens != 240 {
		t.Errorf("usage = %+v", ans.Usage)
	}

	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	var first, second map[string]any
	_ = json.Unmarshal([]byte(reqs[0]), &first)
	_ = json.Unmarshal([]byte(reqs[1]), &second)

	if first["model"] != "claude-sonnet-5-5" {
		t.Errorf("model = %v", first["model"])
	}
	sys := first["system"].([]any)[0].(map[string]any)
	if sys["cache_control"].(map[string]any)["type"] != "ephemeral" {
		t.Errorf("system prompt not cached: %v", sys)
	}
	if first["fallbacks"] != "default" {
		t.Errorf("fallbacks = %v, want \"default\"", first["fallbacks"])
	}
	if got := f.headers[0].Get("anthropic-beta"); !strings.Contains(got, "server-side-fallback-2026-07-01") {
		t.Errorf("anthropic-beta = %q", got)
	}
	// The SDK sends the key as a bearer token.
	if f.headers[0].Get("Authorization") != "Bearer sk-test" {
		t.Error("the configured key was not sent")
	}
	tool := first["tools"].([]any)[0].(map[string]any)
	if tool["name"] != "check_sql" || tool["strict"] != true ||
		tool["input_schema"].(map[string]any)["additionalProperties"] != false {
		t.Errorf("tool = %v", tool)
	}
	if first["output_config"].(map[string]any)["effort"] != "medium" {
		t.Errorf("output_config = %v", first["output_config"])
	}

	// The second request replays the assistant turn as it came (thinking
	// block and signature included) and answers the call in a user turn.
	msgs := second["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("second request has %d messages", len(msgs))
	}
	if !strings.Contains(reqs[1], `"signature":"sig-toolu_01"`) {
		t.Error("the thinking block was not replayed")
	}
	res := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if res["type"] != "tool_result" || res["tool_use_id"] != "toolu_01" || res["content"] == nil {
		t.Errorf("tool result = %v", res)
	}
	if !strings.Contains(reqs[1], `"OK"`) {
		t.Error("the check's verdict did not reach the model")
	}
	// The model's check, then the final SQL's own check for the operator,
	// which the model never sees: it differs from the draft it checked.
	if len(c.asked) != 2 || c.asked[0] != "SELECT os, count(*) FROM rollups GROUP BY os" || c.asked[1] != ans.SQL {
		t.Errorf("checker asked %q", c.asked)
	}
}

// NQ1: the 4-check cap. A model that keeps checking is told it has no checks
// left, and then given up on; the engine is asked exactly four times.
func TestAnthropicCheckCap(t *testing.T) {
	f := newFake(t, anthropicToolUse("toolu_loop", sqlInput("SELECT 1")))
	c := &fakeChecker{}
	s := newAnthropicService(t, f, c, nil)
	_, err := s.Ask(context.Background(), "alice", "loop forever")
	if !errors.Is(err, ErrNoDraft) {
		t.Fatalf("err = %v, want ErrNoDraft", err)
	}
	if len(c.asked) != DefaultMaxChecks {
		t.Errorf("checker asked %d times, want %d", len(c.asked), DefaultMaxChecks)
	}
	if n := len(f.requests()); n != DefaultMaxChecks+2 {
		t.Errorf("%d requests, want %d", n, DefaultMaxChecks+2)
	}
	if last := f.requests()[len(f.requests())-1]; !strings.Contains(last, "no checks left") {
		t.Error("the model was never told the checks ran out")
	}
}

// NQ1: a refusal is reported as one, with its category.
func TestAnthropicRefusal(t *testing.T) {
	r := anthropicMessage("refusal")
	r.body = strings.Replace(r.body, `"stop_details":null`, `"stop_details":{"type":"refusal","category":"cyber","explanation":"declined"}`, 1)
	f := newFake(t, r)
	s := newAnthropicService(t, f, &fakeChecker{}, nil)
	_, err := s.Ask(context.Background(), "alice", "anything")
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "cyber") {
		t.Fatalf("err = %v", err)
	}
}

// NQ1: a 429 is retried by the SDK and the question still answered.
func TestAnthropicRetriesA429(t *testing.T) {
	f := newFake(t,
		reply{status: 429, headers: map[string]string{"retry-after-ms": "10"}, body: `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`},
		anthropicFinal("Count.", "SELECT count(*) FROM rollups"))
	// Attempts are counted at the client (see countingTransport).
	rt := &countingTransport{base: http.DefaultTransport}
	p, _ := NewAnthropic(AnthropicOptions{APIKey: "k", BaseURL: f.URL, HTTPClient: &http.Client{Transport: rt}})
	s, _ := New(Options{Provider: p, Checker: &fakeChecker{}})
	ans, err := s.Ask(context.Background(), "alice", "how many?")
	if err != nil {
		t.Fatal(err)
	}
	if ans.SQL != "SELECT count(*) FROM rollups" || rt.n.Load() != 2 {
		t.Errorf("answer %+v after %d attempts, want 2", ans, rt.n.Load())
	}
}

// A 4xx is the provider's answer, readable, not retried.
func TestAnthropicReportsAnAPIError(t *testing.T) {
	f := newFake(t, reply{status: 400, body: `{"type":"error","error":{"type":"invalid_request_error","message":"model: not found"}}`})
	s := newAnthropicService(t, f, &fakeChecker{}, nil)
	_, err := s.Ask(context.Background(), "alice", "how many?")
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Status != 400 || !strings.Contains(err.Error(), "model: not found") {
		t.Fatalf("err = %v", err)
	}
	if len(f.requests()) != 1 {
		t.Errorf("a 400 was retried: %d requests", len(f.requests()))
	}
}

// NQ1: the overall timeout ends a question that would not answer.
func TestAnthropicTimeout(t *testing.T) {
	f := newFake(t, reply{delay: 5 * time.Second, body: "{}"})
	s := newAnthropicService(t, f, &fakeChecker{}, func(o *Options) { o.Timeout = 200 * time.Millisecond })
	started := time.Now()
	_, err := s.Ask(context.Background(), "alice", "slow")
	if err == nil || !strings.Contains(err.Error(), "no answer within 200ms") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Errorf("took %s", time.Since(started))
	}
}

// NQ1: a malformed tool argument is answered as an error the model can fix,
// and the fixed call goes through.
func TestAnthropicMalformedToolArgument(t *testing.T) {
	f := newFake(t,
		anthropicToolUse("toolu_bad", `{"query":"SELECT 1"}`),
		anthropicToolUse("toolu_good", sqlInput("SELECT 1")),
		anthropicFinal("One.", "SELECT 1"))
	c := &fakeChecker{}
	s := newAnthropicService(t, f, c, nil)
	ans, err := s.Ask(context.Background(), "alice", "one")
	if err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if !strings.Contains(reqs[1], "malformed arguments") || !strings.Contains(reqs[1], `"is_error":true`) {
		t.Errorf("the malformed call was not answered as an error: %s", reqs[1])
	}
	if ans.Checks != 2 {
		t.Errorf("checks = %d, want 2 (the malformed call counts)", ans.Checks)
	}
}

// Haiku has no server-side fallback, so it is not asked for one.
func TestAnthropicHaikuSendsNoFallbacks(t *testing.T) {
	f := newFake(t, anthropicFinal("One.", "SELECT 1"))
	p, _ := NewAnthropic(AnthropicOptions{APIKey: "k", BaseURL: f.URL, Model: "claude-haiku-5-5"})
	s, _ := New(Options{Provider: p, Checker: &fakeChecker{}})
	if _, err := s.Ask(context.Background(), "a", "q"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.requests()[0], "fallbacks") || f.headers[0].Get("anthropic-beta") != "" {
		t.Errorf("haiku request asked for fallbacks: %s", f.requests()[0])
	}
}

// Haiku 4.5 and older reject effort; current models take it.
func TestAnthropicEffortOnlyWhereTheModelTakesIt(t *testing.T) {
	for model, want := range map[string]bool{"claude-haiku-4-5": false, "claude-haiku-5-5": true, "claude-sonnet-5-5": true} {
		f := newFake(t, anthropicFinal("One.", "SELECT 1"))
		p, _ := NewAnthropic(AnthropicOptions{APIKey: "k", BaseURL: f.URL, Model: model})
		s, _ := New(Options{Provider: p, Checker: &fakeChecker{}})
		if _, err := s.Ask(context.Background(), "a", "q"); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(f.requests()[0], `"effort"`); got != want {
			t.Errorf("%s: effort sent = %v, want %v", model, got, want)
		}
	}
}

func TestAnthropicNeedsAKey(t *testing.T) {
	if _, err := NewAnthropic(AnthropicOptions{}); err == nil {
		t.Fatal("no key accepted")
	}
}
