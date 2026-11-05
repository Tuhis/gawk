// Package ask drafts DuckDB SQL from an operator's question in English (R82,
// docs/84): the "Ask" box above the telemetry console's editor.
//
// # Mode B, by construction (docs/84 OD1, D2)
//
// A provider sees exactly one conversation: the system prompt (prompt.go,
// built from the schema and the glossary, never from a row), the operator's
// question, the model's own drafts, and check's answers. Draft's Request is the
// only way anything reaches a provider, and check is the only function that
// answers it: sqlengine.Explainer, which binds the draft in a shadow catalogue
// of EMPTY tables and returns "OK" or the error text. Nothing here can read
// the store, so nothing from it can reach a model — not a row, not a count,
// not a value.
//
// # It drafts; it does not run (D3)
//
// The answer is SQL and one sentence saying how the question was read. The
// operator reads both and presses Run, under the console's allowlist and
// budget, unchanged. In the NQ0 spike the dangerous failure was SQL that ran
// and returned a confident, impossible number; only a human reading the
// result catches that.
package ask

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/sqlengine"
)

// Defaults (docs/84 D7).
const (
	DefaultMaxChecks   = 4
	DefaultTimeout     = 120 * time.Second
	DefaultRatePerHour = 30
	// MaxQuestionBytes bounds a question. A question, not a document.
	MaxQuestionBytes = 2000
)

// Context sizes (docs/84 D5a).
const (
	ContextFull = "full"
	ContextSlim = "slim"
)

var (
	// ErrRefused is a model that declined the question.
	ErrRefused = errors.New("ask: the model declined to answer")
	// ErrNoDraft is a model that answered without a usable draft: no SQL, a
	// statement the console would refuse, or no answer within the checks it
	// had.
	ErrNoDraft = errors.New("ask: the model did not produce a usable draft")
	// ErrBusy is an operator who already has a question in flight.
	ErrBusy = errors.New("ask: you already have a question in flight; wait for its answer")
	// ErrRateLimited is an operator over the hourly cap.
	ErrRateLimited = errors.New("ask: question limit reached")
	// ErrBadQuestion is an empty or oversized question.
	ErrBadQuestion = errors.New("ask: bad question")
)

// CheckFunc is the model's one tool: check a draft, get "OK" or an error.
type CheckFunc func(ctx context.Context, sql string) string

// Request is everything a provider may send. Nothing else reaches it.
type Request struct {
	System   string
	Question string
	// MaxChecks is how many check calls the question may make; the provider
	// stops asking after a couple of requests past it.
	MaxChecks int
	// Check is the only function that answers the model. It counts its own
	// calls to enforce the cap and is NOT safe for concurrent use: a provider
	// that runs tool calls in parallel must still call it one at a time.
	Check CheckFunc
}

// Usage is a question's token count, summed over its requests.
type Usage struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
}

func (u *Usage) add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
}

// Reply is a provider's final turn.
type Reply struct {
	Text  string
	Usage Usage
	// Model is the model that answered, which a server-side fallback can make
	// differ from the one asked for.
	Model string
}

// Provider drafts. Two kinds (D4): anthropic.go and openai.go.
type Provider interface {
	// Name is the provider kind: "anthropic" or "openai".
	Name() string
	// Model is the configured model.
	Model() string
	// Draft runs one question's conversation to its final turn.
	Draft(ctx context.Context, req Request) (Reply, error)
}

// Checker is what the ask loop needs from the engine: the shadow-catalogue
// check and the learned schema the prompt describes.
type Checker interface {
	Explain(ctx context.Context, sql string) (string, error)
	Schema() ([]sqlengine.ViewSchema, error)
}

// Answer is what the operator gets: a draft to read, never a result.
type Answer struct {
	Interpretation string `json:"interpretation"`
	SQL            string `json:"sql"`
	// Checks is how many times the model checked a draft.
	Checks int `json:"checks"`
	// Check is the shadow catalogue's verdict on the final SQL: "OK", the
	// "ERROR: …" the operator will meet when they run it, or "NOT CHECKED: …"
	// when the check itself could not run.
	Check     string `json:"check"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	ElapsedMs int64  `json:"elapsedMs"`
	Usage     Usage  `json:"usage"`
}

// Options configure the service.
type Options struct {
	Provider Provider
	Checker  Checker
	// Context is ContextFull or ContextSlim; empty picks full for anthropic
	// and slim for openai (D5a).
	Context     string
	MaxChecks   int
	Timeout     time.Duration
	RatePerHour int
	Log         *slog.Logger
	Now         func() time.Time
	// External says the provider is outside the cluster, so the console can
	// say the question leaves it (docs/84 §5). Endpoint is the host it goes
	// to, for that sentence.
	External bool
	Endpoint string
}

// Service answers questions, one at a time per operator.
type Service struct {
	opts   Options
	prompt promptCache

	mu       sync.Mutex
	inFlight map[string]bool
	asked    map[string][]time.Time
}

// New builds the service.
func New(opts Options) (*Service, error) {
	if opts.Provider == nil {
		return nil, errors.New("ask: Provider is required")
	}
	if opts.Checker == nil {
		return nil, errors.New("ask: Checker is required")
	}
	switch opts.Context {
	case "":
		opts.Context = ContextFull
		if opts.Provider.Name() == "openai" {
			opts.Context = ContextSlim
		}
	case ContextFull, ContextSlim:
	default:
		return nil, fmt.Errorf("ask: context %q is not %s or %s", opts.Context, ContextFull, ContextSlim)
	}
	if opts.MaxChecks <= 0 {
		opts.MaxChecks = DefaultMaxChecks
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.RatePerHour <= 0 {
		opts.RatePerHour = DefaultRatePerHour
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{opts: opts, inFlight: map[string]bool{}, asked: map[string][]time.Time{}}, nil
}

// Status is what the console is told about the box.
type Status struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Context  string `json:"context"`
	External bool   `json:"external"`
	Endpoint string `json:"endpoint,omitempty"`
}

// Status describes the configured provider.
func (s *Service) Status() Status {
	return Status{
		Provider: s.opts.Provider.Name(), Model: s.opts.Provider.Model(),
		Context: s.opts.Context, External: s.opts.External, Endpoint: s.opts.Endpoint,
	}
}

// RateLimitError is ErrRateLimited with when the next question is allowed.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("ask: question limit reached; try again in %s", e.RetryAfter.Round(time.Second))
}
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// admit takes operator's slot and counts the question, or says why not.
func (s *Service) admit(operator string) (release func(), err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight[operator] {
		return nil, ErrBusy
	}
	now := s.opts.Now()
	recent := s.asked[operator][:0]
	for _, t := range s.asked[operator] {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	s.asked[operator] = recent
	if len(recent) >= s.opts.RatePerHour {
		return nil, &RateLimitError{RetryAfter: time.Hour - now.Sub(recent[0])}
	}
	s.asked[operator] = append(recent, now)
	s.inFlight[operator] = true
	return func() {
		s.mu.Lock()
		delete(s.inFlight, operator)
		s.mu.Unlock()
	}, nil
}

// Ask drafts SQL for question on behalf of operator.
func (s *Service) Ask(ctx context.Context, operator, question string) (*Answer, error) {
	question = strings.TrimSpace(question)
	switch {
	case question == "":
		return nil, fmt.Errorf("%w: ask a question", ErrBadQuestion)
	case len(question) > MaxQuestionBytes:
		return nil, fmt.Errorf("%w: a question is at most %d bytes", ErrBadQuestion, MaxQuestionBytes)
	case !utf8.ValidString(question):
		return nil, fmt.Errorf("%w: not UTF-8", ErrBadQuestion)
	}
	release, err := s.admit(operator)
	if err != nil {
		return nil, err
	}
	defer release()

	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()
	started := time.Now()
	ans, err := s.draft(ctx, question)
	elapsed := time.Since(started)

	p := s.opts.Provider
	attrs := []any{
		"operator", operator, "provider", p.Name(), "model", p.Model(),
		"question", question, "duration_ms", elapsed.Milliseconds(),
	}
	if ans != nil {
		ans.ElapsedMs = elapsed.Milliseconds()
		attrs = append(attrs, "served_by", ans.Model, "sql", ans.SQL, "checks", ans.Checks, "final_check", ans.Check,
			"input_tokens", ans.Usage.InputTokens, "output_tokens", ans.Usage.OutputTokens,
			"cache_read_tokens", ans.Usage.CacheReadTokens, "cache_write_tokens", ans.Usage.CacheWriteTokens)
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && !errors.Is(err, ErrRefused) {
			err = fmt.Errorf("ask: no answer within %s: %w", s.opts.Timeout, err)
		}
		s.opts.Log.Info("ask failed", append(attrs, "err", err)...)
		return nil, err
	}
	s.opts.Log.Info("ask answered", attrs...)
	return ans, nil
}

// draft runs the conversation and turns its last turn into an Answer.
func (s *Service) draft(ctx context.Context, question string) (*Answer, error) {
	views, err := s.opts.Checker.Schema()
	if err != nil {
		return nil, err
	}
	system := s.prompt.get(promptInput{
		Today: s.opts.Now().UTC().Format("2006-01-02"), Views: views, Context: s.opts.Context,
	})

	// The one function that answers the model (D2). It counts, so the cap
	// holds whatever the provider does; past it, the model is told to answer.
	var (
		checks   int
		verdicts = map[string]string{}
		checkErr error
	)
	check := func(ctx context.Context, sql string) string {
		if checks >= s.opts.MaxChecks {
			return fmt.Sprintf("ERROR: no checks left (the limit is %d a question). Reply now with your final answer in the required format.", s.opts.MaxChecks)
		}
		checks++
		v, err := s.opts.Checker.Explain(ctx, sql)
		if err != nil {
			// The checker failing is not the draft's fault, and nothing
			// about the failure is the model's business.
			checkErr = err
			return "ERROR: the check is unavailable right now; answer with your best draft."
		}
		verdicts[normalize(sql)] = v
		return v
	}

	p := s.opts.Provider
	reply, err := p.Draft(ctx, Request{System: system, Question: question, MaxChecks: s.opts.MaxChecks, Check: check})
	if err != nil {
		if checkErr != nil {
			return nil, fmt.Errorf("%w (the SQL check also failed: %v)", err, checkErr)
		}
		return nil, err
	}
	ans := &Answer{Provider: p.Name(), Model: reply.Model, Checks: checks, Usage: reply.Usage}
	if ans.Model == "" {
		ans.Model = p.Model()
	}
	ans.Interpretation, ans.SQL = parseFinal(reply.Text)
	if ans.SQL == "" {
		return ans, fmt.Errorf("%w: it answered without SQL: %s", ErrNoDraft, excerpt(reply.Text))
	}
	stmt, err := sqlengine.Check(ans.SQL)
	if err != nil {
		return ans, fmt.Errorf("%w: it drafted a statement the console refuses (%v)", ErrNoDraft, err)
	}
	ans.SQL = stmt
	// The final SQL's verdict, for the operator. Usually the model's last
	// check already said; otherwise it is checked once more here, and the
	// answer is not sent to the model.
	if v, ok := verdicts[normalize(stmt)]; ok {
		ans.Check = v
	} else if v, err := s.opts.Checker.Explain(ctx, stmt); err == nil {
		ans.Check = v
	} else {
		// Never empty: an empty verdict would read as a pass.
		ans.Check = "NOT CHECKED: the check did not finish"
	}
	return ans, nil
}

func normalize(sql string) string { return strings.TrimSuffix(strings.TrimSpace(sql), ";") }

// parseFinal reads the reply format the prompt asks for:
//
//	INTERPRETATION: <one sentence>
//	SQL:
//	```sql
//	<the query>
//	```
func parseFinal(text string) (interpretation, sql string) {
	if _, after, ok := strings.Cut(text, "INTERPRETATION:"); ok {
		line, _, _ := strings.Cut(after, "\n")
		interpretation = strings.TrimSpace(line)
	}
	if _, after, ok := strings.Cut(text, "```sql"); ok {
		sql, _, _ = strings.Cut(after, "```")
	} else if _, after, ok := strings.Cut(text, "```"); ok {
		sql, _, _ = strings.Cut(after, "```")
	}
	return interpretation, strings.TrimSpace(sql)
}

// excerpt is the start of a model's text, for an error the operator reads.
func excerpt(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(an empty reply)"
	}
	if len(s) > 300 {
		cut := 300
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}
