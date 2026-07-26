package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/ingest"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/readapi"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/sessions"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/store"
)

const (
	testSession = "000102030405060708090a0b"
	testBcast   = "1a2b3c4d5e6f"
)

func seededAPI(t *testing.T) (*readapi.API, *store.Store) {
	t.Helper()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	st, err := store.New(store.Options{Root: t.TempDir(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	w := sessions.NewWriter(sessions.Options{
		Store: st, Now: func() time.Time { return now },
		Finalize: sessions.RollupFinalizer(st, nil, nil, func() time.Time { return now }),
	})
	samples := make([]ingest.Sample, 0, 20)
	for i := range 20 {
		samples = append(samples, ingest.Sample{TMs: float64(i) * 2000, Stats: map[string]any{
			"receivedFps": 60.0, "decoderFps": 60.0, "timeSinceLastFrameMs": 16.0,
			"deliveryMode": "datagrams", "capToRenderMs": 88.0,
			"keyframeStreamsReceived": float64(i / 2), "reorderGapResyncs": 0.0,
		}})
	}
	if err := w.Accept(ingest.Accepted{
		SessionID: testSession, BroadcastKey: testBcast, Role: "viewer", Seq: 0, Final: true,
		App:         ingest.AppInfo{Version: "0.33.2", Surface: "viewer", Browser: "Chrome 152", OS: "Windows"},
		StartedAtMs: now.UnixMilli(), ReceivedAt: now, Samples: samples,
	}); err != nil {
		t.Fatal(err)
	}

	api, err := readapi.New(readapi.Options{Store: st, Now: func() time.Time { return now }, DashboardBase: "http://dash"})
	if err != nil {
		t.Fatal(err)
	}
	return api, st
}

func newServer(t *testing.T, enableSQL bool) (*Server, *httptest.Server) {
	t.Helper()
	api, _ := seededAPI(t)
	s, err := New(Options{API: api, EnableSQL: enableSQL, Now: func() time.Time {
		return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv
}

func rpc(t *testing.T, srv *httptest.Server, method string, params any, id int) response {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		body["params"] = params
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", method, err)
	}
	return out
}

func callTool(t *testing.T, srv *httptest.Server, name string, args map[string]any) map[string]any {
	t.Helper()
	r := rpc(t, srv, "tools/call", map[string]any{"name": name, "arguments": args}, 1)
	if r.Error != nil {
		t.Fatalf("%s: rpc error %+v", name, r.Error)
	}
	res, ok := r.Result.(map[string]any)
	if !ok {
		t.Fatalf("%s: result is not an object: %#v", name, r.Result)
	}
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("%s: tool error %v", name, res["content"])
	}
	return res
}

func TestInitializeAndToolsList(t *testing.T) {
	_, srv := newServer(t, false)

	init := rpc(t, srv, "initialize", map[string]any{"protocolVersion": ProtocolVersion}, 1)
	if init.Error != nil {
		t.Fatalf("initialize: %+v", init.Error)
	}
	res := init.Result.(map[string]any)
	if res["protocolVersion"] != ProtocolVersion {
		t.Errorf("protocolVersion = %v", res["protocolVersion"])
	}
	if res["instructions"] == "" {
		t.Error("no instructions — a model needs to know where to start")
	}

	list := rpc(t, srv, "tools/list", nil, 2)
	tools := list.Result.(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, tv := range tools {
		tm := tv.(map[string]any)
		names[tm["name"].(string)] = true
		if tm["description"] == "" {
			t.Errorf("tool %v has no description", tm["name"])
		}
		if tm["inputSchema"] == nil {
			t.Errorf("tool %v has no input schema", tm["name"])
		}
	}
	for _, want := range []string{"list_broadcasts", "list_sessions", "diagnose", "get_session", "compare", "fleet_summary", "live"} {
		if !names[want] {
			t.Errorf("tool %q missing", want)
		}
	}
	// D11: absent unless explicitly enabled.
	if names["query_sql"] {
		t.Error("query_sql is exposed by default; it must be opt-in")
	}
}

func TestQuerySQLAppearsOnlyWhenEnabled(t *testing.T) {
	_, srv := newServer(t, true)
	list := rpc(t, srv, "tools/list", nil, 1)
	tools := list.Result.(map[string]any)["tools"].([]any)
	var found bool
	for _, tv := range tools {
		if tv.(map[string]any)["name"] == "query_sql" {
			found = true
		}
	}
	if !found {
		t.Error("query_sql absent despite EnableSQL")
	}
	// With no engine wired it must SAY so rather than pretend.
	r := rpc(t, srv, "tools/call", map[string]any{
		"name": "query_sql", "arguments": map[string]any{"sql": "select 1"},
	}, 2)
	res := r.Result.(map[string]any)
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Error("query_sql with no engine did not report an error")
	}
}

func TestEveryToolRoundTripsAgainstASeededStore(t *testing.T) {
	_, srv := newServer(t, false)
	cases := []struct {
		tool string
		args map[string]any
	}{
		{"list_broadcasts", nil},
		{"list_sessions", nil},
		{"diagnose", map[string]any{"sessionId": testSession}},
		{"get_session", map[string]any{"sessionId": testSession}},
		{"compare", map[string]any{"sessionId": testSession}},
		{"fleet_summary", nil},
		{"live", nil},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			res := callTool(t, srv, tc.tool, tc.args)
			// Both a text rendering (for models that read content) and
			// structuredContent (for those that parse) must be present.
			if res["structuredContent"] == nil {
				t.Error("no structuredContent")
			}
			content, ok := res["content"].([]any)
			if !ok || len(content) == 0 {
				t.Fatalf("no content: %#v", res["content"])
			}
			text := content[0].(map[string]any)["text"].(string)
			var parsed any
			if err := json.Unmarshal([]byte(text), &parsed); err != nil {
				t.Errorf("content text is not JSON: %v", err)
			}
		})
	}
}

// One implementation, two façades: the MCP result and the HTTP response must
// be identical, or the two surfaces have drifted.
func TestMCPAndHTTPReturnTheSameData(t *testing.T) {
	api, _ := seededAPI(t)
	s, err := New(Options{API: api})
	if err != nil {
		t.Fatal(err)
	}
	mcpSrv := httptest.NewServer(s)
	defer mcpSrv.Close()
	httpSrv := httptest.NewServer(api.Handler())
	defer httpSrv.Close()

	res := callTool(t, mcpSrv, "diagnose", map[string]any{"sessionId": testSession})
	viaMCP, _ := json.Marshal(res["structuredContent"])

	resp, err := http.Get(httpSrv.URL + "/v1/sessions/" + testSession + "/diagnose")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var httpBody any
	if err := json.NewDecoder(resp.Body).Decode(&httpBody); err != nil {
		t.Fatal(err)
	}
	viaHTTP, _ := json.Marshal(httpBody)

	if string(viaMCP) != string(viaHTTP) {
		t.Errorf("MCP and HTTP disagree:\n mcp: %s\nhttp: %s", viaMCP, viaHTTP)
	}
}

// Default responses through MCP must respect the same ceiling as the HTTP
// façade — the wrapper adds no firehose of its own.
func TestMCPDefaultResponsesStayBounded(t *testing.T) {
	_, srv := newServer(t, false)
	for _, tool := range []string{"diagnose", "get_session", "list_sessions", "fleet_summary"} {
		res := callTool(t, srv, tool, map[string]any{"sessionId": testSession})
		b, _ := json.Marshal(res["structuredContent"])
		if len(b) > readapi.ResponseCeilingBytes {
			t.Errorf("%s response is %d bytes, over the %d ceiling", tool, len(b), readapi.ResponseCeilingBytes)
		}
	}
}

// A tool error must come back IN the result with isError, not as a protocol
// error: the model needs to see what went wrong so it can choose differently.
func TestToolErrorsAreReportedInBand(t *testing.T) {
	_, srv := newServer(t, false)
	r := rpc(t, srv, "tools/call", map[string]any{
		"name": "diagnose", "arguments": map[string]any{"sessionId": "ffffffffffffffffffffffff"},
	}, 1)
	if r.Error != nil {
		t.Fatalf("a missing session became a protocol error: %+v", r.Error)
	}
	res := r.Result.(map[string]any)
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Error("a missing session was not reported as a tool error")
	}
}

func TestUnknownToolAndMethod(t *testing.T) {
	_, srv := newServer(t, false)
	r := rpc(t, srv, "tools/call", map[string]any{"name": "nope"}, 1)
	res := r.Result.(map[string]any)
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Error("an unknown tool was not reported as an error")
	}
	r = rpc(t, srv, "resources/list", nil, 2)
	if r.Error == nil || r.Error.Code != -32601 {
		t.Errorf("unknown method error = %+v, want -32601", r.Error)
	}
}

// MCP clients send `notifications/initialized` on connect; a notification has
// no id and must get no body.
func TestNotificationsGetNoBody(t *testing.T) {
	_, srv := newServer(t, false)
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("notification status = %d, want 202", resp.StatusCode)
	}
}

func TestRejectsNonPostAndGarbage(t *testing.T) {
	_, srv := newServer(t, false)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", resp.StatusCode)
	}

	resp, err = http.Post(srv.URL, "application/json", bytes.NewReader([]byte("{not json")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Error == nil || out.Error.Code != -32700 {
		t.Errorf("parse error = %+v, want -32700", out.Error)
	}
}
