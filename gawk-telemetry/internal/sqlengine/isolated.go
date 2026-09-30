package sqlengine

// The isolated engine: every query runs in a short-lived child process.
//
// DuckDB's `memory_limit` bounds its buffer manager and nothing else. The JSON
// reader's allocations — every file's schema sniffed on every bind
// (union_by_name), every scan's read buffers — happen outside it, and DuckDB's
// allocator keeps what it freed for the life of the process. In production
// (2026-09-30) the view probe made the service query every five minutes, and
// each round left 150–500 MiB behind: `duckdb_memory()` reported 0 bytes while
// the resident set climbed to the pod's 2 GiB limit, stalled /healthz, and got
// the pod killed every 30–90 minutes. Lowering memory_limit changed nothing
// (down to 64 MiB, where the queries themselves started failing); closing and
// reopening the database returned only about half. Only process exit returned
// all of it.
//
// So the service never opens DuckDB itself. Each Query re-executes the
// service's own binary as a one-shot worker (WorkerEnv set): the request goes
// in on stdin, the answer comes back as JSON on stdout, and whatever DuckDB
// allocated leaves with the process. The worker runs the in-process engine
// (Open) unchanged, so the allowlist, the budget, the view registration and
// the drift retry are the same code on either side.
//
// Workers run one at a time — the engine's single connection was already the
// serialisation point, and one worker is one budget — and a worker still
// running at the query's deadline is killed.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// WorkerEnv marks a process as a one-shot query worker. The service's main
// checks IsWorker before anything else and hands the process to RunWorker.
const WorkerEnv = "GAWK_TELEMETRY_SQL_WORKER"

// IsWorker reports whether this process was started as a query worker.
func IsWorker() bool { return os.Getenv(WorkerEnv) == "1" }

// workerRequest is what the service sends a worker on stdin. An empty SQL asks
// for the view catalogue only.
type workerRequest struct {
	Options Options `json:"options"`
	SQL     string  `json:"sql,omitempty"`
	// Views is the set of views to register; nil registers all of them.
	// Registering a view sniffs every file in its tree, and a worker pays that
	// on every query, so it registers only what the statement names.
	Views []string `json:"views"`
}

// workerResponse is the worker's one line of stdout. Views covers only the
// views the worker was asked to register.
type workerResponse struct {
	Result *Result   `json:"result,omitempty"`
	Views  []ViewDoc `json:"views,omitempty"`
	Err    string    `json:"error,omitempty"`
}

// referencedViews is the views stmt names, by whole-word match. A false
// positive (a view's name inside a string literal) costs one registration; a
// view the statement really reads is never missed, because an identifier —
// quoted or not — is always a whole word.
func referencedViews(stmt string) []string {
	var out []string
	for _, d := range viewDocs(nil) {
		if viewNameRE(d.Name).MatchString(stmt) {
			out = append(out, d.Name)
		}
	}
	return out
}

func viewNameRE(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(name) + `\b`)
}

// workerKillGrace is how long a killed worker gets to release its pipes before
// Wait stops waiting for them.
const workerKillGrace = 2 * time.Second

// workerStderrLimit bounds how much of a failed worker's stderr is kept for
// the error message.
const workerStderrLimit = 4 << 10

// RunWorker serves one request from in and writes one response to out, and
// returns the process exit code. It opens the in-process engine, so it answers
// ErrNoEngine's text in a build without one.
func RunWorker(in io.Reader, out io.Writer) int {
	var req workerRequest
	enc := json.NewEncoder(out)
	if err := json.NewDecoder(in).Decode(&req); err != nil {
		_ = enc.Encode(workerResponse{Err: "sqlengine: malformed worker request: " + err.Error()})
		return 2
	}
	var only map[string]bool
	if req.Views != nil {
		only = map[string]bool{}
		for _, v := range req.Views {
			only[v] = true
		}
	}
	scoped := func(views []ViewDoc) []ViewDoc {
		if only == nil {
			return views
		}
		out := []ViewDoc{}
		for _, v := range views {
			if only[v.Name] {
				out = append(out, v)
			}
		}
		return out
	}
	e, err := openScoped(req.Options, only)
	if err != nil {
		_ = enc.Encode(workerResponse{Err: err.Error()})
		return 1
	}
	defer e.Close()
	resp := workerResponse{Views: scoped(e.Views())}
	if req.SQL != "" {
		res, err := e.Query(req.SQL)
		if err != nil {
			resp.Err = err.Error()
		} else {
			resp.Result = res
			resp.Views = scoped(res.Views)
		}
	}
	if err := enc.Encode(resp); err != nil {
		return 1
	}
	return 0
}

type isolated struct {
	opts Options
	exe  string

	// turn is the one-worker-at-a-time slot, acquired with the query's
	// context so a query queued behind another still times out.
	turn chan struct{}

	mu    sync.Mutex
	views []ViewDoc
}

// OpenIsolated opens the engine the service runs: the in-process engine,
// behind a process boundary per query. It runs one worker to learn the view
// catalogue, so an engine that cannot open fails here, at startup, rather than
// on an operator's first query.
func OpenIsolated(opts Options) (Engine, error) {
	if !Compiled() {
		return nil, ErrNoEngine
	}
	if opts.Root == "" {
		return nil, fmt.Errorf("sqlengine: Root is required")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("sqlengine: locating the worker binary: %w", err)
	}
	e := &isolated{
		opts: opts, exe: exe,
		turn:  make(chan struct{}, 1),
		views: viewDocs(func(string) bool { return false }),
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout())
	defer cancel()
	if _, err := e.run(ctx, "", nil); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *isolated) Query(q string) (*Result, error) {
	// Refused in the service, before a process is spent on it. The worker's
	// engine checks again; the allowlist does not depend on this one.
	stmt, err := Check(q)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.opts.timeout())
	defer cancel()
	started := time.Now()
	select {
	case e.turn <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-e.turn }()

	res, err := e.run(ctx, stmt, referencedViews(stmt))
	if err != nil {
		return nil, err
	}
	// The operator waited for the process too, so that is what they are told.
	res.ElapsedMs = time.Since(started).Milliseconds()
	// The worker only saw the views it registered; the result carries the
	// whole catalogue, as the in-process engine's does.
	res.Views = e.Views()
	return res, nil
}

// run executes one worker registering the named views (nil: all of them). An
// empty stmt only refreshes the catalogue.
func (e *isolated) run(ctx context.Context, stmt string, views []string) (*Result, error) {
	opts := e.opts
	// The worker's own clock ends no later than ours, so a slow query comes
	// back as DuckDB's error rather than as a kill whenever it can.
	if dl, ok := ctx.Deadline(); ok {
		opts.Timeout = time.Until(dl)
	}
	if views == nil && stmt != "" {
		// A statement that names no view registers none, not all.
		views = []string{}
	}
	req, err := json.Marshal(workerRequest{Options: opts, SQL: stmt, Views: views})
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, e.exe)
	cmd.Env = append(os.Environ(), WorkerEnv+"=1")
	cmd.Stdin = bytes.NewReader(req)
	var stdout bytes.Buffer
	stderr := &tailBuffer{limit: workerStderrLimit}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = workerKillGrace
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("sqlengine: query did not finish within %s: %w", e.opts.timeout(), ctx.Err())
	}

	var resp workerResponse
	dec := json.NewDecoder(&stdout)
	// Numbers stay the digits DuckDB printed: through float64 a BIGINT past
	// 2^53 would come back as a different number.
	dec.UseNumber()
	if err := dec.Decode(&resp); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if runErr == nil {
			runErr = err
		}
		if msg != "" {
			return nil, fmt.Errorf("sqlengine: query worker failed without answering (%v): %s", runErr, msg)
		}
		return nil, fmt.Errorf("sqlengine: query worker failed without answering (%v)", runErr)
	}
	e.merge(resp.Views)
	if resp.Err != "" {
		return nil, errors.New(resp.Err)
	}
	if resp.Result == nil {
		if stmt != "" {
			return nil, fmt.Errorf("sqlengine: query worker answered without a result")
		}
		return nil, nil
	}
	return resp.Result, nil
}

// merge takes a worker's word on the views it registered and keeps the rest.
func (e *isolated) merge(seen []ViewDoc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range seen {
		for i := range e.views {
			if e.views[i].Name == s.Name {
				e.views[i].Available = s.Available
			}
		}
	}
}

// Views is the catalogue: each view as the most recent worker to register it
// saw it.
func (e *isolated) Views() []ViewDoc {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]ViewDoc(nil), e.views...)
}

// Close has nothing to release: no worker outlives the Query that started it.
func (e *isolated) Close() error { return nil }

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	limit int
	buf   []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.limit; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string { return string(b.buf) }
