// Package mcp exposes the read API as an MCP server over streamable HTTP
// (docs/33 TM7 + §4.6).
//
// **It is a thin wrapper over the TM6 handlers, never a second
// implementation.** Every tool below calls the same `readapi.API` method the
// HTTP façade calls, so the two cannot drift — a test asserts that a query
// through MCP and the same query through HTTP return identical bytes.
//
// Transport is streamable HTTP on the READ listener (owner decision), not
// stdio: the service runs in the cluster and the operator's Claude Code runs
// on a laptop, so one deployment reachable through a port-forward or an
// internal Ingress beats a binary that has to be installed locally. It shares
// the read listener's exposure posture — never on the public path that carries
// ingest (D14).
//
// The protocol surface is implemented directly rather than through an SDK.
// What MCP needs here is three JSON-RPC methods over one POST endpoint;
// carrying a dependency tree for that would work against the same
// dependency-light instinct that kept DuckDB out of the runtime (D11).
package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/readapi"
)

// ProtocolVersion is the MCP revision this server speaks.
const ProtocolVersion = "2025-06-18"

// maxRequestBytes bounds one JSON-RPC request. Tool arguments here are a
// session id and a couple of filters.
const maxRequestBytes = 64 << 10

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Tool is one exposed capability.
type Tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	call        func(args map[string]any) (any, error)
}

// Server is the MCP endpoint.
type Server struct {
	api   *readapi.API
	now   func() time.Time
	tools []Tool
}

// Options configure the server.
type Options struct {
	API *readapi.API
	Now func() time.Time
	// EnableSQL exposes the DuckDB passthrough. Absent unless explicitly
	// enabled (D11): DuckDB is a query OPTION, not a runtime dependency, and
	// an arbitrary-SQL tool on an ops surface should be a deliberate act.
	EnableSQL bool
	// SQL runs a query when EnableSQL is set. Nil ⇒ the tool reports that the
	// deployment has no engine wired, rather than pretending.
	SQL func(query string) (any, error)
}

// New builds the MCP server.
func New(opts Options) (*Server, error) {
	if opts.API == nil {
		return nil, fmt.Errorf("mcp: API is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{api: opts.API, now: opts.Now}
	s.tools = s.buildTools(opts)
	return s, nil
}

// Tools returns the exposed tool list (for tests and docs).
func (s *Server) Tools() []Tool { return s.tools }

func (s *Server) buildTools(opts Options) []Tool {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	num := func(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }
	obj := func(props map[string]any, required ...string) map[string]any {
		m := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			m["required"] = required
		}
		return m
	}

	tools := []Tool{
		{
			Name:  "list_broadcasts",
			Title: "List broadcasts",
			Description: "Recent broadcasts, worst-first, with session counts and the worst verdict " +
				"seen. Start here when you do not know which broadcast to look at.",
			InputSchema: obj(map[string]any{
				"since": str("How far back, as a Go duration (\"24h\") or RFC3339 timestamp. Default 24h."),
				"limit": num("Maximum rows. Default 50."),
			}),
			call: func(a map[string]any) (any, error) {
				return s.api.ListBroadcasts(s.since(a), intArg(a, "limit"))
			},
		},
		{
			Name:  "list_sessions",
			Title: "List sessions",
			Description: "Per-session summaries (one row per viewer or broadcaster), newest first. " +
				"Each row carries its severity, stall count and relay coverage, plus a `distrust` " +
				"note when the session's own data quality makes its verdict unreliable.",
			InputSchema: obj(map[string]any{
				"broadcast": str("Filter to one obfuscated broadcast key (12 hex chars)."),
				"role":      str("\"viewer\" or \"broadcaster\"."),
				"verdict":   str("Filter by severity: ok, warn, bad, unknown."),
				"since":     str("How far back. Default 24h."),
				"limit":     num("Maximum rows. Default 50."),
			}),
			call: func(a map[string]any) (any, error) {
				return s.api.ListSessions(readapi.ListSessionsQuery{
					BroadcastKey: strArg(a, "broadcast"),
					Role:         strArg(a, "role"),
					Verdict:      strArg(a, "verdict"),
					Since:        s.since(a),
					Limit:        intArg(a, "limit"),
				})
			},
		},
		{
			Name:  "diagnose",
			Title: "Diagnose a session",
			Description: "Run the docs/13 bottleneck playbook over one session and return RANKED " +
				"verdicts with their evidence — never raw samples. Each piece of evidence is tagged " +
				"relay | client | derived; a verdict resting only on client testimony is confidence-" +
				"capped, because a wedged client's own accounting is the least reliable evidence in " +
				"the system. Rules whose signals were unavailable are listed rather than silently " +
				"skipped. START HERE for \"why was this session bad?\" — it is far cheaper than " +
				"fetching the timeline.",
			InputSchema: obj(map[string]any{
				"sessionId": str("The 24-hex session id, from list_sessions."),
			}, "sessionId"),
			call: func(a map[string]any) (any, error) {
				return s.api.Diagnose(strArg(a, "sessionId"))
			},
		},
		{
			Name:  "get_session",
			Title: "Get a session timeline",
			Description: "A DOWNSAMPLED timeline (about 40 points over a curated field set) plus every " +
				"event. Use this only after diagnose() when you need to see a shape it could not " +
				"explain; name `fields` and raise `points` to widen it deliberately.",
			InputSchema: obj(map[string]any{
				"sessionId": str("The 24-hex session id."),
				"fields":    str("Comma-separated stats fields. Default: a curated set for the role."),
				"points":    num("Target number of points. Default 40."),
			}, "sessionId"),
			call: func(a map[string]any) (any, error) {
				var fields []string
				if f := strArg(a, "fields"); f != "" {
					fields = splitCSV(f)
				}
				return s.api.GetSession(strArg(a, "sessionId"), fields, intArg(a, "points"))
			},
		},
		{
			Name:  "compare",
			Title: "Compare a session to the fleet",
			Description: "Place one session against the fleet median for its own class (same delivery " +
				"mode). Answers \"is this bad, or is this normal here?\" — and says so when the " +
				"baseline is too thin to mean anything.",
			InputSchema: obj(map[string]any{
				"sessionId": str("The 24-hex session id."),
				"since":     str("Baseline window. Default 24h."),
			}, "sessionId"),
			call: func(a map[string]any) (any, error) {
				return s.api.Compare(strArg(a, "sessionId"), s.since(a))
			},
		},
		{
			Name:  "fleet_summary",
			Title: "Fleet summary",
			Description: "Percentiles across sessions, grouped by delivery mode (default), browser, os " +
				"or resolution. The \"overall average\" a single session is judged against.",
			InputSchema: obj(map[string]any{
				"since":   str("How far back. Default 24h."),
				"groupBy": str("deliveryMode | browser | os | resolution."),
			}),
			call: func(a map[string]any) (any, error) {
				return s.api.FleetSummary(s.since(a), strArg(a, "groupBy"))
			},
		},
		{
			Name:  "live",
			Title: "Live fleet state",
			Description: "What is happening RIGHT NOW: live broadcasts with their viewers, severities " +
				"and per-side freshness. A session whose client stopped reporting reads stale, and " +
				"one that never reported reads unknown — never ok.",
			InputSchema: obj(map[string]any{}),
			call: func(map[string]any) (any, error) {
				return s.api.LiveSnapshot(), nil
			},
		},
	}

	// D11: absent unless explicitly enabled. An arbitrary-SQL tool on an ops
	// surface should be a deliberate act, not a default.
	if opts.EnableSQL {
		tools = append(tools, Tool{
			Name:  "query_sql",
			Title: "Ad-hoc SQL (DuckDB)",
			Description: "Run DuckDB SQL over the raw NDJSON partitions. For questions no endpoint " +
				"covers yet. Optional and disabled by default.",
			InputSchema: obj(map[string]any{"sql": str("The query.")}, "sql"),
			call: func(a map[string]any) (any, error) {
				if opts.SQL == nil {
					return nil, fmt.Errorf("query_sql is enabled but no engine is wired in this deployment")
				}
				return opts.SQL(strArg(a, "sql"))
			},
		})
	}
	return tools
}

// ServeHTTP implements the streamable-HTTP MCP transport: one POST endpoint
// carrying JSON-RPC.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
		return
	}
	resp := s.dispatch(req)
	// A notification (no id) gets no body — that is the JSON-RPC contract, and
	// MCP clients send `notifications/initialized` on connect.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeRPC(w, resp)
}

func (s *Server) dispatch(req request) response {
	out := response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		out.Result = map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "gawk-telemetry", "version": "1"},
			"instructions": "Per-session diagnostics for a gawk relay fleet. Start with diagnose() " +
				"on a session id — it returns ranked verdicts with evidence and costs a fraction of " +
				"a raw timeline. Fall back to get_session only when a shape needs looking at.",
		}
	case "tools/list":
		list := make([]map[string]any, 0, len(s.tools))
		for _, t := range s.tools {
			list = append(list, map[string]any{
				"name": t.Name, "title": t.Title,
				"description": t.Description, "inputSchema": t.InputSchema,
			})
		}
		out.Result = map[string]any{"tools": list}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			out.Error = &rpcError{Code: -32602, Message: "invalid params"}
			return out
		}
		result, err := s.callTool(p.Name, p.Arguments)
		if err != nil {
			// A tool error is reported IN the result with isError, not as a
			// protocol error: the model needs to see what went wrong so it can
			// choose a different call.
			out.Result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}},
				"isError": true,
			}
			return out
		}
		b, err := json.Marshal(result)
		if err != nil {
			out.Error = &rpcError{Code: -32603, Message: "result not serializable"}
			return out
		}
		out.Result = map[string]any{
			"content":           []map[string]any{{"type": "text", "text": string(b)}},
			"structuredContent": result,
		}
	default:
		out.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return out
}

func (s *Server) callTool(name string, args map[string]any) (any, error) {
	for _, t := range s.tools {
		if t.Name == name {
			if args == nil {
				args = map[string]any{}
			}
			return t.call(args)
		}
	}
	return nil, fmt.Errorf("unknown tool %q", name)
}

func writeRPC(w http.ResponseWriter, resp response) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(resp)
}

func (s *Server) since(a map[string]any) time.Time {
	now := s.now()
	v := strArg(a, "since")
	if v == "" {
		return now.Add(-24 * time.Hour)
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t
	}
	if d, err := time.ParseDuration(v); err == nil {
		return now.Add(-d)
	}
	return now.Add(-24 * time.Hour)
}

func strArg(a map[string]any, key string) string {
	s, _ := a[key].(string)
	return s
}

func intArg(a map[string]any, key string) int {
	switch v := a[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func splitCSV(s string) []string {
	var out []string
	cur := ""
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(s[i])
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
