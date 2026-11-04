package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/ask"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/readapi"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/sqlengine"
)

func keyFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// D8: the box is off by default.
func TestAskIsOffByDefault(t *testing.T) {
	c, err := parseFlags([]string{"-telemetry-key", key64}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if c.askProvider != "none" || c.askTimeout != ask.DefaultTimeout || c.askMaxChecks != ask.DefaultMaxChecks || c.askRate != ask.DefaultRatePerHour {
		t.Errorf("ask defaults = %+v", c)
	}
	a, err := newAsker(c, nil, nil)
	if a != nil || err != nil {
		t.Errorf("newAsker with the box off = %v, %v", a, err)
	}
}

// D8: every flag, and its GAWK_TELEMETRY_ASK_* env var.
func TestAskFlagsAndEnv(t *testing.T) {
	kf := keyFile(t, "sk-ant-test\n")
	env := func(k string) string {
		return map[string]string{
			"GAWK_TELEMETRY_KEY":              key64,
			"GAWK_TELEMETRY_ASK_PROVIDER":     "anthropic",
			"GAWK_TELEMETRY_ASK_MODEL":        "claude-opus-5-5",
			"GAWK_TELEMETRY_ASK_BASE_URL":     "https://proxy.example.com",
			"GAWK_TELEMETRY_ASK_API_KEY_FILE": kf,
			"GAWK_TELEMETRY_ASK_CONTEXT":      "slim",
			"GAWK_TELEMETRY_ASK_TIMEOUT":      "45s",
			"GAWK_TELEMETRY_ASK_MAX_CHECKS":   "3",
			"GAWK_TELEMETRY_ASK_RATE":         "10",
		}[k]
	}
	c, err := parseFlags(nil, env)
	if err != nil {
		t.Fatal(err)
	}
	if c.askProvider != "anthropic" || c.askModel != "claude-opus-5-5" || c.askBaseURL != "https://proxy.example.com" ||
		c.askAPIKey != "sk-ant-test" || c.askContext != "slim" || c.askTimeout != 45*time.Second ||
		c.askMaxChecks != 3 || c.askRate != 10 {
		t.Errorf("from env: %+v", c)
	}

	kf2 := keyFile(t, "local")
	c, err = parseFlags([]string{"-telemetry-key", key64,
		"-ask-provider", "openai", "-ask-model", "qwen2.5-coder:32b", "-ask-base-url", "http://ollama.ollama.svc:11434",
		"-ask-api-key-file", kf2, "-ask-context", "full", "-ask-timeout", "5m", "-ask-max-checks", "6", "-ask-rate", "60"}, env)
	if err != nil {
		t.Fatal(err)
	}
	if c.askProvider != "openai" || c.askModel != "qwen2.5-coder:32b" || c.askBaseURL != "http://ollama.ollama.svc:11434" ||
		c.askAPIKey != "local" || c.askContext != "full" || c.askTimeout != 5*time.Minute || c.askMaxChecks != 6 || c.askRate != 60 {
		t.Errorf("flags over env: %+v", c)
	}
	if host, ext := c.askEndpoint(); host != "ollama.ollama.svc" || ext {
		t.Errorf("endpoint = %s, external %v", host, ext)
	}
}

func TestAskAnthropicDefaultsItsModel(t *testing.T) {
	c, err := parseFlags([]string{"-telemetry-key", key64, "-ask-provider", "anthropic", "-ask-api-key-file", keyFile(t, "k")}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if c.askModel != "claude-sonnet-5-5" {
		t.Errorf("model = %s", c.askModel)
	}
	if host, ext := c.askEndpoint(); host != "api.anthropic.com" || !ext {
		t.Errorf("endpoint = %s, external %v", host, ext)
	}
}

func TestAskRefusesAnUnusableConfiguration(t *testing.T) {
	empty := keyFile(t, "  \n")
	for _, args := range [][]string{
		{"-ask-provider", "gpt"},
		{"-ask-provider", "anthropic"},
		{"-ask-provider", "anthropic", "-ask-api-key-file", empty},
		{"-ask-provider", "anthropic", "-ask-api-key-file", "/nonexistent/key"},
		{"-ask-provider", "openai", "-ask-model", "m"},
		{"-ask-provider", "openai", "-ask-base-url", "http://x"},
		{"-ask-context", "medium"},
		{"-ask-timeout", "0s"},
		{"-ask-max-checks", "0"},
		{"-ask-rate", "0"},
	} {
		if _, err := parseFlags(append([]string{"-telemetry-key", key64}, args...), noEnv); err == nil {
			t.Errorf("parseFlags(%v) accepted", args)
		}
	}
}

// A box asked for without an engine is not built; the console reports why.
func TestAskNeedsAnEngine(t *testing.T) {
	c, err := parseFlags([]string{"-telemetry-key", key64, "-ask-provider", "anthropic", "-ask-api-key-file", keyFile(t, "k")}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if a, err := newAsker(c, nil, nil); a != nil || err != nil {
		t.Errorf("newAsker without an engine = %v, %v", a, err)
	}
	// An engine that cannot check (the in-process one) is an error, not a
	// silently unchecked box.
	if _, err := newAsker(c, plainEngine{}, nil); err == nil || !strings.Contains(err.Error(), "cannot check") {
		t.Errorf("newAsker over an engine without Explain: %v", err)
	}
	if _, err := newAsker(c, explainEngine{}, nil); err != nil {
		t.Errorf("newAsker over an Explainer: %v", err)
	}
}

type plainEngine struct{}

func (plainEngine) Query(string) (*sqlengine.Result, error) { return nil, nil }
func (plainEngine) Views() []sqlengine.ViewDoc              { return nil }

type explainEngine struct{ plainEngine }

func (explainEngine) Explain(context.Context, string) (string, error) { return "OK", nil }
func (explainEngine) Schema() ([]sqlengine.ViewSchema, error)         { return nil, nil }

var _ readapi.SQLEngine = plainEngine{}

// The basic-auth gate hands the Ask box the user it verified, and only that.
func TestBasicAuthNamesTheVerifiedOperator(t *testing.T) {
	var got string
	h := basicAuth(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = readapi.AskOperator(r)
	}), "ops", "s3cret")
	req := httptest.NewRequest(http.MethodPost, "/v1/query/ask", nil)
	req.SetBasicAuth("ops", "s3cret")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "basic:ops" {
		t.Errorf("operator = %q, want basic:ops", got)
	}
}
