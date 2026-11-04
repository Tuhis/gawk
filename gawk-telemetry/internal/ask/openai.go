package ask

// The OpenAI-compatible provider (docs/84 D4): hand-written HTTP to
// `POST {base}/v1/chat/completions` with one function tool. Ollama,
// llama.cpp's server, vLLM and LM Studio all serve this shape, so one client
// covers every local server worth naming without a second SDK in the module.
// It is how an operator points the box at a model on their own hardware
// (OD5); the chart ships none.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// OpenAIOptions configure the provider.
type OpenAIOptions struct {
	// BaseURL is the server, e.g. http://ollama.ollama.svc:11434. A trailing /v1
	// is accepted.
	BaseURL string
	Model   string
	// APIKey is sent as a bearer token when set; local servers need none.
	APIKey     string
	HTTPClient *http.Client
	// MaxRetries bounds retries of a 429 or a 5xx; 0 means 2, negative none.
	MaxRetries int
}

type openAIProvider struct {
	url     string
	model   string
	key     string
	http    *http.Client
	retries int
}

// NewOpenAI builds the provider.
func NewOpenAI(o OpenAIOptions) (Provider, error) {
	if o.BaseURL == "" {
		return nil, errors.New("ask: the openai provider needs -ask-base-url")
	}
	if o.Model == "" {
		return nil, errors.New("ask: the openai provider needs -ask-model")
	}
	base := strings.TrimSuffix(strings.TrimSuffix(o.BaseURL, "/"), "/v1")
	c := o.HTTPClient
	if c == nil {
		c = &http.Client{}
	}
	retries := o.MaxRetries
	if retries == 0 {
		retries = 2
	} else if retries < 0 {
		retries = 0
	}
	return &openAIProvider{url: base + "/v1/chat/completions", model: o.Model, key: o.APIKey, http: c, retries: retries}, nil
}

func (p *openAIProvider) Name() string  { return "openai" }
func (p *openAIProvider) Model() string { return p.model }

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content"`
	Refusal    string        `json:"refusal,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		// Arguments is a JSON string in the OpenAI shape; some servers send
		// the object itself. Both are read (sqlArgument).
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type oaiRequest struct {
	Model    string       `json:"model"`
	Messages []oaiMessage `json:"messages"`
	Tools    []any        `json:"tools"`
	Stream   bool         `json:"stream"`
}

type oaiResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      oaiMessage `json:"message"`
		FinishReason string     `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

var oaiTool = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        CheckToolName,
		"description": CheckToolDescription,
		"parameters": map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"sql": map[string]any{"type": "string"}},
			"required":             []string{"sql"},
			"additionalProperties": false,
		},
	},
}

func (p *openAIProvider) Draft(ctx context.Context, req Request) (Reply, error) {
	body := oaiRequest{
		Model: p.model,
		Messages: []oaiMessage{
			{Role: "system", Content: req.System},
			{Role: "user", Content: req.Question},
		},
		Tools: []any{oaiTool},
	}
	var reply Reply
	for range req.MaxChecks + 2 {
		resp, err := p.post(ctx, body)
		if err != nil {
			return reply, err
		}
		if resp.Model != "" {
			reply.Model = resp.Model
		}
		reply.Usage.add(Usage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens})
		if len(resp.Choices) == 0 {
			return reply, &ProviderError{Provider: "openai", Err: errors.New("the server answered with no choices")}
		}
		choice := resp.Choices[0]
		switch choice.FinishReason {
		case "content_filter":
			return reply, ErrRefused
		case "length":
			return reply, fmt.Errorf("%w: the reply ran past the server's output limit", ErrNoDraft)
		}
		m := choice.Message
		if m.Refusal != "" {
			// OpenAI's own API declines here, with finish_reason "stop".
			return reply, fmt.Errorf("%w (%s)", ErrRefused, excerpt(m.Refusal))
		}
		if len(m.ToolCalls) == 0 {
			reply.Text = m.Content
			return reply, nil
		}
		m.Role = "assistant"
		body.Messages = append(body.Messages, m)
		for _, c := range m.ToolCalls {
			var answer string
			switch sql, err := sqlArgument(decodeArguments(c.Function.Arguments)); {
			case c.Function.Name != CheckToolName:
				answer = "ERROR: unknown tool " + c.Function.Name + "; the only tool is " + CheckToolName
			case err != nil:
				req.Check(ctx, "") // counted, as in the anthropic provider
				answer = "ERROR: " + err.Error()
			default:
				answer = req.Check(ctx, sql)
			}
			body.Messages = append(body.Messages, oaiMessage{Role: "tool", ToolCallID: c.ID, Content: answer})
		}
	}
	return reply, fmt.Errorf("%w: it was still checking drafts after %d checks", ErrNoDraft, req.MaxChecks)
}

// decodeArguments unwraps the OpenAI shape's JSON-in-a-string; an object is
// passed through.
func decodeArguments(raw json.RawMessage) json.RawMessage {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return json.RawMessage(s)
	}
	return raw
}

// maxRetryWait bounds how long a Retry-After is honoured; past it, the
// operator is better told.
//
// post retries a 429 or a 5xx, honouring Retry-After (seconds or an HTTP
// date). A connection error is not retried: a local server that is down
// stays down for the next few seconds, and the operator is told at once.
const maxRetryWait = 20 * time.Second

func (p *openAIProvider) post(ctx context.Context, body oaiRequest) (*oaiResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Content-Type", "application/json")
		if p.key != "" {
			r.Header.Set("Authorization", "Bearer "+p.key)
		}
		res, err := p.http.Do(r)
		if err != nil {
			return nil, &ProviderError{Provider: "openai", Err: err}
		}
		b, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
		res.Body.Close()
		if err != nil {
			return nil, &ProviderError{Provider: "openai", Err: err}
		}
		if res.StatusCode == http.StatusOK {
			var out oaiResponse
			if err := json.Unmarshal(b, &out); err != nil {
				return nil, &ProviderError{Provider: "openai", Err: fmt.Errorf("malformed response: %v", err)}
			}
			return &out, nil
		}
		perr := &ProviderError{Provider: "openai", Status: res.StatusCode, Err: errors.New(excerpt(string(b))), RetryAfter: retryAfter(res.Header)}
		retryable := res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500
		if !retryable || attempt >= p.retries || perr.RetryAfter > maxRetryWait {
			return nil, perr
		}
		wait := perr.RetryAfter
		if wait == 0 {
			wait = time.Duration(attempt+1) * 500 * time.Millisecond
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if s, err := strconv.Atoi(v); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
