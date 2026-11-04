package ask

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newOpenAIService(t *testing.T, f *fakeServer, c Checker, mut func(*Options)) *Service {
	t.Helper()
	p, err := NewOpenAI(OpenAIOptions{BaseURL: f.URL + "/v1/", Model: "qwen2.5-coder:7b"})
	if err != nil {
		t.Fatal(err)
	}
	o := Options{Provider: p, Checker: c}
	if mut != nil {
		mut(&o)
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// NQ1: a tool call round trip against the recorded chat-completions shape
// (as Ollama, llama.cpp and vLLM serve it).
func TestOpenAIToolCallRoundTrip(t *testing.T) {
	f := newFake(t,
		openAIToolCall("call_1", sqlInput("SELECT os FROM rollups")),
		openAIFinal("Platforms.", "SELECT os, count(*) FROM rollups GROUP BY os"))
	c := &fakeChecker{}
	s := newOpenAIService(t, f, c, nil)
	ans, err := s.Ask(context.Background(), "bob", "which platforms?")
	if err != nil {
		t.Fatal(err)
	}
	if ans.SQL != "SELECT os, count(*) FROM rollups GROUP BY os" || ans.Model != "qwen2.5-coder:7b" || ans.Checks != 1 {
		t.Errorf("answer = %+v", ans)
	}
	if ans.Usage.InputTokens != 6500 {
		t.Errorf("usage = %+v", ans.Usage)
	}
	reqs := f.requests()
	var first, second struct {
		Model    string           `json:"model"`
		Messages []map[string]any `json:"messages"`
		Tools    []map[string]any `json:"tools"`
		Stream   bool             `json:"stream"`
	}
	_ = json.Unmarshal([]byte(reqs[0]), &first)
	_ = json.Unmarshal([]byte(reqs[1]), &second)
	if first.Model != "qwen2.5-coder:7b" || first.Stream || first.Messages[0]["role"] != "system" || first.Messages[1]["content"] != "which platforms?" {
		t.Errorf("first request = %s", reqs[0])
	}
	if fn := first.Tools[0]["function"].(map[string]any); fn["name"] != "check_sql" {
		t.Errorf("tool = %v", first.Tools[0])
	}
	if len(second.Messages) != 4 || second.Messages[3]["role"] != "tool" || second.Messages[3]["tool_call_id"] != "call_1" || second.Messages[3]["content"] != "OK" {
		t.Errorf("second request = %s", reqs[1])
	}
	if f.headers[0].Get("Authorization") != "" {
		t.Error("a bearer token was sent with no key configured")
	}
}

// Ollama has sent the arguments as an object rather than a JSON string.
func TestOpenAIArgumentsAsAnObject(t *testing.T) {
	r := openAIToolCall("call_1", "")
	r.body = strings.Replace(r.body, `"arguments":""`, `"arguments":{"sql":"SELECT 1"}`, 1)
	f := newFake(t, r, openAIFinal("One.", "SELECT 1"))
	c := &fakeChecker{}
	s := newOpenAIService(t, f, c, nil)
	if _, err := s.Ask(context.Background(), "bob", "one"); err != nil {
		t.Fatal(err)
	}
	if c.asked[0] != "SELECT 1" {
		t.Errorf("checker asked %q", c.asked)
	}
}

func TestOpenAICheckCap(t *testing.T) {
	f := newFake(t, openAIToolCall("call_loop", sqlInput("SELECT 1")))
	c := &fakeChecker{}
	s := newOpenAIService(t, f, c, func(o *Options) { o.MaxChecks = 2 })
	_, err := s.Ask(context.Background(), "bob", "loop")
	if !errors.Is(err, ErrNoDraft) || len(c.asked) != 2 || len(f.requests()) != 4 {
		t.Fatalf("err = %v after %d checks and %d requests", err, len(c.asked), len(f.requests()))
	}
}

func TestOpenAIRefusal(t *testing.T) {
	f := newFake(t, reply{body: `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"content_filter"}]}`})
	s := newOpenAIService(t, f, &fakeChecker{}, nil)
	if _, err := s.Ask(context.Background(), "bob", "q"); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v", err)
	}
}

// OpenAI's own API declines in message.refusal with finish_reason "stop".
func TestOpenAIRefusalField(t *testing.T) {
	f := newFake(t, reply{body: `{"choices":[{"message":{"role":"assistant","content":null,"refusal":"I can't help with that."},"finish_reason":"stop"}]}`})
	s := newOpenAIService(t, f, &fakeChecker{}, nil)
	_, err := s.Ask(context.Background(), "bob", "q")
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "can't help") {
		t.Fatalf("err = %v", err)
	}
}

func TestRetryAfterTakesSecondsOrADate(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "7")
	if retryAfter(h) != 7*time.Second {
		t.Error(retryAfter(h))
	}
	h.Set("Retry-After", time.Now().Add(10*time.Second).UTC().Format(http.TimeFormat))
	if d := retryAfter(h); d < 8*time.Second || d > 10*time.Second {
		t.Errorf("an HTTP-date Retry-After = %s", d)
	}
}

func TestOpenAIRetriesA429(t *testing.T) {
	f := newFake(t,
		reply{status: 429, headers: map[string]string{"Retry-After": "0"}, body: `{"error":"busy"}`},
		openAIFinal("One.", "SELECT 1"))
	rt := &countingTransport{base: http.DefaultTransport}
	p, _ := NewOpenAI(OpenAIOptions{BaseURL: f.URL, Model: "m", HTTPClient: &http.Client{Transport: rt}})
	s, _ := New(Options{Provider: p, Checker: &fakeChecker{}})
	ans, err := s.Ask(context.Background(), "bob", "q")
	if err != nil {
		t.Fatal(err)
	}
	if ans.SQL != "SELECT 1" || rt.n.Load() != 2 {
		t.Errorf("answer %+v after %d attempts, want 2", ans, rt.n.Load())
	}
}

func TestOpenAIGivesUpOnA429ThatPersists(t *testing.T) {
	f := newFake(t, reply{status: 429, headers: map[string]string{"Retry-After": "0"}, body: `{"error":"busy"}`})
	// Attempts are counted at the client: the transport may itself resend a
	// request whose reused connection broke, which the server would count.
	rt := &countingTransport{base: http.DefaultTransport}
	p, err := NewOpenAI(OpenAIOptions{BaseURL: f.URL, Model: "m", HTTPClient: &http.Client{Transport: rt}})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := New(Options{Provider: p, Checker: &fakeChecker{}})
	_, err = s.Ask(context.Background(), "bob", "q")
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Status != 429 || rt.n.Load() != 3 {
		t.Fatalf("err = %v after %d attempts, want 3", err, rt.n.Load())
	}
}

type countingTransport struct {
	base http.RoundTripper
	n    atomic.Int32
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.base.RoundTrip(r)
}

func TestOpenAITimeout(t *testing.T) {
	f := newFake(t, reply{delay: 5 * time.Second, body: "{}"})
	s := newOpenAIService(t, f, &fakeChecker{}, func(o *Options) { o.Timeout = 200 * time.Millisecond })
	if _, err := s.Ask(context.Background(), "bob", "q"); err == nil || !strings.Contains(err.Error(), "no answer within") {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenAIMalformedToolArgument(t *testing.T) {
	f := newFake(t, openAIToolCall("call_bad", `{"sql": `), openAIFinal("One.", "SELECT 1"))
	s := newOpenAIService(t, f, &fakeChecker{}, nil)
	if _, err := s.Ask(context.Background(), "bob", "q"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.requests()[1], "malformed arguments") {
		t.Errorf("second request = %s", f.requests()[1])
	}
}

func TestOpenAISendsTheKeyWhenSet(t *testing.T) {
	f := newFake(t, openAIFinal("One.", "SELECT 1"))
	p, _ := NewOpenAI(OpenAIOptions{BaseURL: f.URL, Model: "m", APIKey: "local-key"})
	s, _ := New(Options{Provider: p, Checker: &fakeChecker{}})
	if _, err := s.Ask(context.Background(), "bob", "q"); err != nil {
		t.Fatal(err)
	}
	if f.headers[0].Get("Authorization") != "Bearer local-key" {
		t.Errorf("Authorization = %q", f.headers[0].Get("Authorization"))
	}
}

func TestOpenAINeedsAURLAndAModel(t *testing.T) {
	if _, err := NewOpenAI(OpenAIOptions{Model: "m"}); err == nil {
		t.Error("no base URL accepted")
	}
	if _, err := NewOpenAI(OpenAIOptions{BaseURL: "http://x"}); err == nil {
		t.Error("no model accepted")
	}
}
