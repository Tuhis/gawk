package ask

// The Anthropic provider (docs/84 D4): the Messages API through the official
// Go SDK, with check_sql as a client tool, the system prompt cached, and
// server-side refusal fallbacks.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/anthropics/anthropic-sdk-go/v2/packages/param"
	"github.com/anthropics/anthropic-sdk-go/v2/shared/constant"
)

// DefaultAnthropicModel is docs/84 OD2's choice.
const DefaultAnthropicModel = "claude-sonnet-5-5"

// anthropicMaxTokens is a non-streaming request's output cap: a draft is a
// sentence and a query, and thinking shares the cap.
const anthropicMaxTokens = 16000

// AnthropicOptions configure the provider.
type AnthropicOptions struct {
	APIKey string
	Model  string
	// BaseURL overrides the API's address (a proxy, or a test's fake).
	BaseURL string
	// HTTPClient is the transport; nil is the SDK's.
	HTTPClient *http.Client
	// MaxRetries is the SDK's retry budget for 408/409/429/5xx and
	// connection errors; 0 means 2, negative none.
	MaxRetries int
}

type anthropicProvider struct {
	client anthropic.Client
	model  string
}

// NewAnthropic builds the provider. The key is the one given: the SDK's own
// environment lookup (ANTHROPIC_API_KEY, profiles) is switched off, so what
// the pod's flags say is what it uses.
func NewAnthropic(o AnthropicOptions) (Provider, error) {
	if o.APIKey == "" {
		return nil, errors.New("ask: the anthropic provider needs an API key (-ask-api-key-file)")
	}
	if o.Model == "" {
		o.Model = DefaultAnthropicModel
	}
	retries := o.MaxRetries
	if retries == 0 {
		retries = 2
	} else if retries < 0 {
		retries = 0
	}
	opts := []option.RequestOption{
		option.WithoutEnvironmentDefaults(),
		option.WithAPIKey(o.APIKey),
		option.WithMaxRetries(retries),
	}
	if o.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(o.BaseURL))
	}
	if o.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(o.HTTPClient))
	}
	return &anthropicProvider{client: anthropic.NewClient(opts...), model: o.Model}, nil
}

func (p *anthropicProvider) Name() string  { return "anthropic" }
func (p *anthropicProvider) Model() string { return p.model }

// fallbackCapable is whether model takes `fallbacks: "default"`. Haiku has no
// server-side fallback and would reject the field.
func fallbackCapable(model string) bool { return !strings.Contains(model, "haiku") }

func checkTool() anthropic.BetaToolUnionParam {
	return anthropic.BetaToolUnionParam{OfTool: &anthropic.BetaToolParam{
		Name:        CheckToolName,
		Description: param.NewOpt(CheckToolDescription),
		Strict:      param.NewOpt(true),
		InputSchema: anthropic.BetaToolInputSchemaParam{
			Properties: map[string]any{"sql": map[string]any{"type": "string", "description": "One DuckDB statement."}},
			Required:   []string{"sql"},
			ExtraFields: map[string]any{
				"additionalProperties": false,
			},
		},
	}}
}

func (p *anthropicProvider) Draft(ctx context.Context, req Request) (Reply, error) {
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: anthropicMaxTokens,
		System: []anthropic.BetaTextBlockParam{{
			Text: req.System,
			// The prompt is byte-stable between questions (prompt.go), so
			// every question after the first in five minutes reads it at a
			// tenth of the price.
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		}},
		Tools: []anthropic.BetaToolUnionParam{checkTool()},
		// docs/84 §12 was measured at medium.
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffortMedium},
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(req.Question)),
		},
	}
	if fallbackCapable(p.model) {
		// A declined question is re-served by the model the API picks for the
		// refusal's category, inside the same call.
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
	}

	var reply Reply
	// Each check is a round trip; a couple more requests than checks lets a
	// model that hit the cap still answer.
	for range req.MaxChecks + 2 {
		msg, err := p.client.Beta.Messages.New(ctx, params)
		if err != nil {
			return reply, providerError("anthropic", err)
		}
		reply.Model = string(msg.Model)
		reply.Usage.add(Usage{
			InputTokens: msg.Usage.InputTokens, OutputTokens: msg.Usage.OutputTokens,
			CacheReadTokens: msg.Usage.CacheReadInputTokens, CacheWriteTokens: msg.Usage.CacheCreationInputTokens,
		})
		switch msg.StopReason {
		case anthropic.BetaStopReasonRefusal:
			why := string(msg.StopDetails.Category)
			if msg.StopDetails.Explanation != "" {
				why = strings.TrimSpace(why + ": " + msg.StopDetails.Explanation)
			}
			if why == "" {
				return reply, ErrRefused
			}
			return reply, fmt.Errorf("%w (%s)", ErrRefused, why)
		case anthropic.BetaStopReasonMaxTokens:
			return reply, fmt.Errorf("%w: the reply ran past %d tokens", ErrNoDraft, anthropicMaxTokens)
		}

		var text strings.Builder
		var results []anthropic.BetaContentBlockParamUnion
		for _, block := range msg.Content {
			switch b := block.AsAny().(type) {
			case anthropic.BetaTextBlock:
				text.WriteString(b.Text)
			case anthropic.BetaToolUseBlock:
				results = append(results, toolResult(ctx, req, b))
			}
		}
		if len(results) == 0 {
			reply.Text = text.String()
			return reply, nil
		}
		// Append-only: the assistant turn goes back as it came, thinking
		// blocks included, and every tool result in one user message.
		params.Messages = append(params.Messages, msg.ToParam(), anthropic.NewBetaUserMessage(results...))
	}
	return reply, fmt.Errorf("%w: it was still checking drafts after %d checks", ErrNoDraft, req.MaxChecks)
}

// toolResult answers one tool call. A malformed argument is answered as an
// error the model can correct, and still counts against the cap.
func toolResult(ctx context.Context, req Request, b anthropic.BetaToolUseBlock) anthropic.BetaContentBlockParamUnion {
	if b.Name != CheckToolName {
		return anthropic.NewBetaToolResultBlock(b.ID, "ERROR: unknown tool "+b.Name+"; the only tool is "+CheckToolName, true)
	}
	sql, err := sqlArgument(b.Input)
	if err != nil {
		req.Check(ctx, "") // counted: a model that loops on bad arguments still stops
		return anthropic.NewBetaToolResultBlock(b.ID, "ERROR: "+err.Error(), true)
	}
	v := req.Check(ctx, sql)
	return anthropic.NewBetaToolResultBlock(b.ID, v, v != "OK")
}

// sqlArgument reads check_sql's {"sql": "..."} from a decoded or raw input.
func sqlArgument(in any) (string, error) {
	var raw []byte
	switch x := in.(type) {
	case string:
		raw = []byte(x)
	case json.RawMessage:
		raw = x
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return "", errors.New(`malformed arguments; want {"sql": "<statement>"}`)
		}
		raw = b
	}
	var args struct {
		SQL *string `json:"sql"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || args.SQL == nil || strings.TrimSpace(*args.SQL) == "" {
		return "", errors.New(`malformed arguments; want {"sql": "<statement>"}`)
	}
	return *args.SQL, nil
}

// ProviderError is a provider that failed, as opposed to a model that
// answered badly. Status is the HTTP status when there was one.
type ProviderError struct {
	Provider   string
	Status     int
	RetryAfter time.Duration
	Err        error
}

func (e *ProviderError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("ask: %s answered %d: %v", e.Provider, e.Status, e.Err)
	}
	return fmt.Sprintf("ask: %s: %v", e.Provider, e.Err)
}
func (e *ProviderError) Unwrap() error { return e.Err }

func providerError(name string, err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		msg := strings.TrimSpace(apiErr.RawJSON())
		if msg == "" {
			msg = http.StatusText(apiErr.StatusCode)
		}
		return &ProviderError{Provider: name, Status: apiErr.StatusCode, Err: errors.New(msg)}
	}
	return &ProviderError{Provider: name, Err: err}
}
