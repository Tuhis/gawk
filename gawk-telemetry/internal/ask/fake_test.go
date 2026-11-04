package ask

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/sqlengine"
)

// The mode B test drives the real engine's checker, which re-executes this
// test binary as its worker; it must dispatch the way the service's main does.
func TestMain(m *testing.M) {
	if sqlengine.IsWorker() {
		os.Exit(sqlengine.RunWorker(os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

// reply is one scripted HTTP answer.
type reply struct {
	status  int
	headers map[string]string
	body    string
	delay   time.Duration
}

// fakeServer answers each request with the next scripted reply (the last one
// repeats) and records every request body: what a provider sent is exactly
// what is in bodies.
type fakeServer struct {
	*httptest.Server
	mu      sync.Mutex
	script  []reply
	bodies  []string
	headers []http.Header
}

func newFake(t *testing.T, script ...reply) *fakeServer {
	t.Helper()
	f := &fakeServer{script: script}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		n := len(f.bodies)
		f.bodies = append(f.bodies, string(b))
		f.headers = append(f.headers, r.Header.Clone())
		rep := f.script[min(n, len(f.script)-1)]
		f.mu.Unlock()
		if rep.delay > 0 {
			select {
			case <-time.After(rep.delay):
			case <-r.Context().Done():
				return
			}
		}
		for k, v := range rep.headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		status := rep.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeServer) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

// --- recorded Anthropic Messages API shapes --------------------------------

func anthropicMessage(stop string, content ...string) reply {
	return reply{body: `{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-5-5",` +
		`"content":[` + strings.Join(content, ",") + `],"stop_reason":"` + stop + `","stop_sequence":null,"stop_details":null,` +
		`"usage":{"input_tokens":120,"output_tokens":40,"cache_creation_input_tokens":0,"cache_read_input_tokens":17000}}`}
}

func anthropicToolUse(id, input string) reply {
	return anthropicMessage("tool_use",
		`{"type":"thinking","thinking":"","signature":"sig-`+id+`"}`,
		`{"type":"tool_use","id":"`+id+`","name":"check_sql","input":`+input+`}`)
}

func anthropicFinal(interp, sql string) reply {
	text, _ := json.Marshal("INTERPRETATION: " + interp + "\nSQL:\n```sql\n" + sql + "\n```")
	return anthropicMessage("end_turn", `{"type":"text","text":`+string(text)+`}`)
}

func sqlInput(sql string) string {
	b, _ := json.Marshal(map[string]string{"sql": sql})
	return string(b)
}

// --- recorded OpenAI chat-completions shapes --------------------------------

func openAIToolCall(id, arguments string) reply {
	args, _ := json.Marshal(arguments)
	return reply{body: `{"id":"chatcmpl-1","object":"chat.completion","model":"qwen2.5-coder:7b","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"","tool_calls":[{"id":"` + id + `","type":"function","function":{"name":"check_sql","arguments":` + string(args) + `}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3200,"completion_tokens":30,"total_tokens":3230}}`}
}

func openAIFinal(interp, sql string) reply {
	text, _ := json.Marshal("INTERPRETATION: " + interp + "\nSQL:\n```sql\n" + sql + "\n```")
	return reply{body: `{"id":"chatcmpl-2","object":"chat.completion","model":"qwen2.5-coder:7b","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":` + string(text) + `},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3300,"completion_tokens":60,"total_tokens":3360}}`}
}

// --- a checker that needs no engine -----------------------------------------

// fakeChecker answers from a map of SQL to verdict (default "OK") and records
// what it was asked.
type fakeChecker struct {
	mu       sync.Mutex
	verdicts map[string]string
	asked    []string
	views    []sqlengine.ViewSchema
	err      error
}

func (c *fakeChecker) Explain(_ context.Context, sql string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, sql)
	if c.err != nil {
		return "", c.err
	}
	if _, err := sqlengine.Check(sql); err != nil {
		return "ERROR: " + err.Error(), nil
	}
	if v, ok := c.verdicts[sql]; ok {
		return v, nil
	}
	return "OK", nil
}

func (c *fakeChecker) Schema() ([]sqlengine.ViewSchema, error) {
	if c.views != nil {
		return c.views, nil
	}
	return []sqlengine.ViewSchema{{Name: "rollups", Columns: []sqlengine.Column{{Name: "role", Type: "VARCHAR"}, {Name: "os", Type: "VARCHAR"}, {Name: "stalls", Type: "BIGINT"}}}}, nil
}

func newAnthropicService(t *testing.T, f *fakeServer, c Checker, mut func(*Options)) *Service {
	t.Helper()
	p, err := NewAnthropic(AnthropicOptions{APIKey: "sk-test", BaseURL: f.URL, MaxRetries: 2})
	if err != nil {
		t.Fatal(err)
	}
	o := Options{Provider: p, Checker: c, Now: func() time.Time { return time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC) }}
	if mut != nil {
		mut(&o)
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
