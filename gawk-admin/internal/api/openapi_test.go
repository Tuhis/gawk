package api

// The drift gate for ../../openapi.yaml (R48, docs/49 D3).
//
// The document is hand-written, which makes drift the one real risk — so the
// test is deliberately paranoid and runs in BOTH directions: every registered
// route documented, every documented operation registered, every error code
// and event type in the enums, every declared role matching, every example
// decoding into the handler's own Go type, and every response type's fields
// matching the schema's properties.
//
// It is an in-package test on purpose: the wire types it holds the document to
// are unexported, and exporting them to make a test reach them would widen the
// package's surface to check a property of the package's inside.
//
// It walks the DECLARED table, never a constructed mux, so it checks the room
// routes whether or not this test process has rooms enabled.

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/moderation"
)

const openAPIPath = "../../openapi.yaml"

// httpMethods are the keys of a path item that are operations. Anything else
// there (a shared `parameters`, a `summary`) is not one.
var httpMethods = []string{"get", "put", "post", "delete", "patch", "head", "options", "trace"}

type oasDoc struct {
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Responses map[string]oasResponse     `json:"responses"`
		Schemas   map[string]json.RawMessage `json:"schemas"`
	} `json:"components"`
}

type oasOp struct {
	OperationID string                 `json:"operationId"`
	Roles       []string               `json:"x-gawk-roles"`
	Requires    string                 `json:"x-gawk-requires"`
	Parameters  []oasParam             `json:"parameters"`
	RequestBody *oasBody               `json:"requestBody"`
	Responses   map[string]oasResponse `json:"responses"`
}

type oasParam struct {
	Name   string          `json:"name"`
	In     string          `json:"in"`
	Schema json.RawMessage `json:"schema"`
}

type oasBody struct {
	Content map[string]oasMedia `json:"content"`
}

type oasResponse struct {
	Ref     string              `json:"$ref"`
	Content map[string]oasMedia `json:"content"`
}

type oasMedia struct {
	Schema  json.RawMessage `json:"schema"`
	Example json.RawMessage `json:"example"`
}

// loadDoc reads openapi.yaml the same way the server does: YAML in, JSON out.
func loadDoc(t *testing.T) *oasDoc {
	t.Helper()
	raw, err := os.ReadFile(openAPIPath)
	if err != nil {
		t.Fatalf("reading %s: %v", openAPIPath, err)
	}
	asJSON, err := yaml.YAMLToJSON(raw)
	if err != nil {
		t.Fatalf("%s is not valid YAML: %v", openAPIPath, err)
	}
	var doc oasDoc
	if err := json.Unmarshal(asJSON, &doc); err != nil {
		t.Fatalf("%s is not a document this test understands: %v", openAPIPath, err)
	}
	if len(doc.Paths) == 0 {
		t.Fatalf("%s declares no paths", openAPIPath)
	}
	return &doc
}

// operations flattens the document into one entry per documented operation.
func (d *oasDoc) operations(t *testing.T) map[string]oasOp {
	t.Helper()
	out := map[string]oasOp{}
	for path, item := range d.Paths {
		for _, m := range httpMethods {
			raw, ok := item[m]
			if !ok {
				continue
			}
			var op oasOp
			if err := json.Unmarshal(raw, &op); err != nil {
				t.Fatalf("%s %s: %v", strings.ToUpper(m), path, err)
			}
			out[strings.ToUpper(m)+" "+path] = op
		}
	}
	return out
}

// resolveResponse follows a local $ref into components.responses.
func (d *oasDoc) resolveResponse(r oasResponse) (oasResponse, error) {
	if r.Ref == "" {
		return r, nil
	}
	name, ok := strings.CutPrefix(r.Ref, "#/components/responses/")
	if !ok {
		return oasResponse{}, fmt.Errorf("unsupported response $ref %q", r.Ref)
	}
	out, found := d.Components.Responses[name]
	if !found {
		return oasResponse{}, fmt.Errorf("dangling response $ref %q", r.Ref)
	}
	return out, nil
}

// TestOpenAPIMatchesRoutes is D3 (a)–(f), plus the negative cases that prove
// each half of it can actually fail.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	doc := loadDoc(t)
	ops := doc.operations(t)

	t.Run("routes and operations agree", func(t *testing.T) {
		report(t, checkRoutes(ops, RouteTable()))
	})

	t.Run("error codes are the document's enum", func(t *testing.T) {
		report(t, checkErrorCodes(doc, allErrorCodes()))
	})

	t.Run("allErrorCodes lists every Code constant", func(t *testing.T) {
		report(t, checkErrorCodeConstants(t, allErrorCodes()))
	})

	t.Run("event types are the document's enum", func(t *testing.T) {
		report(t, checkEventTypes(doc, ops, store.AllEventTypes()))
	})

	t.Run("every example decodes into its Go type", func(t *testing.T) {
		report(t, checkExamples(doc, ops))
	})

	t.Run("every schema's properties are the Go type's fields", func(t *testing.T) {
		report(t, checkFixtures(doc, marshalFixtures(t, schemaFixtures())))
	})

	// The three negative cases docs/49 OA1 asks for, against in-memory mutated
	// copies. A drift test that cannot fail is decoration.
	t.Run("negative: an undocumented route fails", func(t *testing.T) {
		shrunk := map[string]oasOp{}
		for k, v := range ops {
			if k == "GET /api/v1/relays" {
				continue
			}
			shrunk[k] = v
		}
		mustFail(t, checkRoutes(shrunk, RouteTable()), "a route missing from the document")
	})

	t.Run("negative: a renamed error code fails", func(t *testing.T) {
		renamed := append([]string(nil), allErrorCodes()...)
		renamed[0] = "bad_request_v2"
		mustFail(t, checkErrorCodes(doc, renamed), "an error code the document does not know")
	})

	t.Run("negative: a response field the document lacks fails", func(t *testing.T) {
		fixtures := marshalFixtures(t, schemaFixtures())
		fixtures["Room"] = withExtraKey(t, fixtures["Room"], "moderatorNote", "a field nobody documented")
		mustFail(t, checkFixtures(doc, fixtures), "a Go field missing from the schema")
	})
}

func report(t *testing.T, errs []error) {
	t.Helper()
	for _, err := range errs {
		t.Error(err)
	}
}

func mustFail(t *testing.T, errs []error, what string) {
	t.Helper()
	if len(errs) == 0 {
		t.Fatalf("the check passed with %s injected: it cannot detect the drift it exists for", what)
	}
}

// ---------------------------------------------------------------- (a), (b), (e)

func checkRoutes(ops map[string]oasOp, table []Route) []error {
	var errs []error
	seen := map[string]bool{}
	ids := map[string]string{}

	for _, r := range table {
		key := r.Method + " " + r.Pattern
		seen[key] = true
		op, ok := ops[key]
		if !ok {
			errs = append(errs, fmt.Errorf("(a) %s is registered but not documented: add it to openapi.yaml", key))
			continue
		}
		if op.OperationID == "" {
			errs = append(errs, fmt.Errorf("(a) %s has no operationId: a generated client would have no name for it", key))
		}
		if prev, dup := ids[op.OperationID]; dup {
			errs = append(errs, fmt.Errorf("(a) operationId %q is used by both %s and %s", op.OperationID, prev, key))
		}
		ids[op.OperationID] = key

		if !sameSet(op.Roles, r.Roles) {
			errs = append(errs, fmt.Errorf("(e) %s: x-gawk-roles is %v, the route table says %v",
				key, op.Roles, r.Roles))
		}
		if op.Requires != r.Requires {
			errs = append(errs, fmt.Errorf("(b) %s: x-gawk-requires is %q, the route table says %q — "+
				"a consumer reads this to tell which routes a given deployment serves",
				key, op.Requires, r.Requires))
		}
	}

	for key := range ops {
		if seen[key] {
			continue
		}
		errs = append(errs, fmt.Errorf("(b) %s is documented but no route registers it: "+
			"a contract that describes an operation nobody serves is worse than no contract", key))
	}
	sortErrs(errs)
	return errs
}

// sameSet compares two role lists ignoring order, and treating nil and empty
// as the same thing: `x-gawk-roles: []` and an absent key both mean "no role",
// which is what an unauthenticated route declares.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------- (c)

func checkErrorCodes(doc *oasDoc, codes []string) []error {
	documented, err := enumOf(doc, "ErrorCode")
	if err != nil {
		return []error{err}
	}
	return compareVocabularies("(c) error code", codes, documented,
		"internal/api's Code* constants", "openapi.yaml's ErrorCode enum")
}

// checkErrorCodeConstants closes the one gap a hand-written allErrorCodes
// leaves: a Code* constant somebody added and forgot to list. It reads the
// package's own source rather than trusting a second hand-written list.
func checkErrorCodeConstants(t *testing.T, listed []string) []error {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		return []error{fmt.Errorf("reading the api package directory: %w", err)}
	}
	fset := token.NewFileSet()
	declared := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			return []error{fmt.Errorf("parsing %s: %w", name, err)}
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, s := range gd.Specs {
				vs, ok := s.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, ident := range vs.Names {
					if !strings.HasPrefix(ident.Name, "Code") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						continue
					}
					declared[ident.Name] = v
				}
			}
		}
	}
	if len(declared) == 0 {
		return []error{fmt.Errorf("found no Code* constants at all: this check proved nothing")}
	}
	have := map[string]bool{}
	for _, c := range listed {
		have[c] = true
	}
	var errs []error
	for name, value := range declared {
		if !have[value] {
			errs = append(errs, fmt.Errorf("(c) const %s = %q is not in allErrorCodes(), "+
				"so nothing holds the document to it", name, value))
		}
	}
	sortErrs(errs)
	return errs
}

// ---------------------------------------------------------------------- (d)

func checkEventTypes(doc *oasDoc, ops map[string]oasOp, types []string) []error {
	documented, err := enumOf(doc, "EventType")
	if err != nil {
		return []error{err}
	}
	errs := compareVocabularies("(d) event type", types, documented,
		"store.AllEventTypes()", "openapi.yaml's EventType enum")

	// The filter parameter and the response field must be the SAME enum, not
	// two lists that happen to agree today.
	op, ok := ops["GET /api/v1/events"]
	if !ok {
		return append(errs, fmt.Errorf("(d) GET /api/v1/events is not documented"))
	}
	found := false
	for _, p := range op.Parameters {
		if p.Name != "type" || p.In != "query" {
			continue
		}
		found = true
		var schema struct {
			Items struct {
				Ref string `json:"$ref"`
			} `json:"items"`
		}
		if err := json.Unmarshal(p.Schema, &schema); err != nil {
			errs = append(errs, fmt.Errorf("(d) the `type` parameter's schema: %w", err))
			break
		}
		if schema.Items.Ref != "#/components/schemas/EventType" {
			errs = append(errs, fmt.Errorf(
				"(d) the `type` filter's items are %q, want a $ref to EventType — "+
					"the filter and the response field are one vocabulary", schema.Items.Ref))
		}
	}
	if !found {
		errs = append(errs, fmt.Errorf("(d) GET /api/v1/events documents no `type` query parameter"))
	}

	// WebhookEventTypes is a SUBSET of AllEventTypes, by construction. R49 is
	// where the two first differ; nothing may leave the subset behind.
	all := map[string]bool{}
	for _, tpe := range types {
		all[tpe] = true
	}
	for _, tpe := range store.WebhookEventTypes() {
		if !all[tpe] {
			errs = append(errs, fmt.Errorf(
				"(d) %q is webhook-eligible but is not a stored event type at all", tpe))
		}
	}
	sortErrs(errs)
	return errs
}

func enumOf(doc *oasDoc, schema string) ([]string, error) {
	raw, ok := doc.Components.Schemas[schema]
	if !ok {
		return nil, fmt.Errorf("openapi.yaml has no %s schema", schema)
	}
	var s struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", schema, err)
	}
	if len(s.Enum) == 0 {
		return nil, fmt.Errorf("%s declares no enum", schema)
	}
	return s.Enum, nil
}

func compareVocabularies(what string, code, document []string, codeName, docName string) []error {
	var errs []error
	inDoc := map[string]bool{}
	for _, v := range document {
		inDoc[v] = true
	}
	inCode := map[string]bool{}
	for _, v := range code {
		inCode[v] = true
	}
	for _, v := range code {
		if !inDoc[v] {
			errs = append(errs, fmt.Errorf("%s %q is in %s but not in %s", what, v, codeName, docName))
		}
	}
	for _, v := range document {
		if !inCode[v] {
			errs = append(errs, fmt.Errorf("%s %q is in %s but not in %s", what, v, docName, codeName))
		}
	}
	sortErrs(errs)
	return errs
}

// ---------------------------------------------------------------------- (f)

// opWiring names the Go types an operation's examples must decode into.
//
// Every table entry needs one, so adding a route forces the question "what
// shape does this actually answer?" to be written down somewhere a test reads.
type opWiring struct {
	// request is a fresh pointer to the request body's Go type, or nil for an
	// operation with no body.
	request func() any
	// success maps a status code to a fresh pointer to its response type. Any
	// status >= 400 that is not listed here decodes into the error envelope,
	// because that is what the package guarantees for every failure.
	success map[string]func() any
}

func wirings() map[string]opWiring {
	return map[string]opWiring{
		// The contract itself: an OpenAPI document, not a shape this package
		// declares, so it is documented with no schema and has nothing to
		// round-trip.
		"GET /api/v1/openapi.json": {},

		"GET /api/v1/me": {success: map[string]func() any{
			"200": func() any { return &meJSON{} },
		}},
		"GET /api/v1/broadcasts": {success: map[string]func() any{
			"200": func() any { return &broadcastsPageJSON{} },
		}},
		"POST /api/v1/broadcasts/{id}/kill": {
			request: func() any { return &killRequest{} },
			success: map[string]func() any{
				"201": func() any { return &killResultJSON{} },
				"202": func() any { return &killResultJSON{} },
				"409": func() any { return &banConflictJSON{} },
			},
		},
		"GET /api/v1/bans": {success: map[string]func() any{
			"200": func() any { return &bansPageJSON{} },
		}},
		"POST /api/v1/bans": {
			request: func() any { return &createBanRequest{} },
			success: map[string]func() any{
				"201": func() any { return &banJSON{} },
				"202": func() any { return &banJSON{} },
				"409": func() any { return &banConflictJSON{} },
			},
		},
		"DELETE /api/v1/bans/{id}": {success: map[string]func() any{
			"202": func() any { return &banJSON{} },
		}},
		"GET /api/v1/events": {success: map[string]func() any{
			"200": func() any { return &eventsPageJSON{} },
		}},
		"GET /api/v1/relays": {success: map[string]func() any{
			"200": func() any { return &relaysPageJSON{} },
		}},
		"GET /api/v1/webhooks": {success: map[string]func() any{
			"200": func() any { return &webhooksPageJSON{} },
		}},
		"POST /api/v1/webhooks": {
			request: func() any { return &webhookRequest{} },
			success: map[string]func() any{"201": func() any { return &webhookJSON{} }},
		},
		"PUT /api/v1/webhooks/{id}": {
			request: func() any { return &webhookRequest{} },
			success: map[string]func() any{"200": func() any { return &webhookJSON{} }},
		},
		"DELETE /api/v1/webhooks/{id}": {},
		"POST /api/v1/webhooks/{name}/test": {success: map[string]func() any{
			"200": func() any { return &TestResult{} },
		}},
		"GET /api/v1/rooms": {success: map[string]func() any{
			"200": func() any { return &roomsPageJSON{} },
		}},
		"POST /api/v1/rooms": {
			request: func() any { return &createRoomRequest{} },
			success: map[string]func() any{"201": func() any { return &roomWithSecretJSON{} }},
		},
		"POST /api/v1/rooms/{name}/rotate-secret": {success: map[string]func() any{
			"200": func() any { return &roomWithSecretJSON{} },
		}},
		"POST /api/v1/rooms/{name}/end": {},
		"DELETE /api/v1/rooms/{name}":   {},
	}
}

func checkExamples(doc *oasDoc, ops map[string]oasOp) []error {
	var errs []error
	wire := wirings()

	for _, r := range RouteTable() {
		key := r.Method + " " + r.Pattern
		if _, ok := wire[key]; !ok {
			errs = append(errs, fmt.Errorf("(f) %s has no example wiring: "+
				"name the Go types its examples must decode into, in wirings()", key))
		}
	}

	keys := make([]string, 0, len(ops))
	for k := range ops {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		op := ops[key]
		w := wire[key]

		if op.RequestBody != nil {
			for mediaType, media := range op.RequestBody.Content {
				if len(media.Example) == 0 {
					errs = append(errs, fmt.Errorf("(f) %s request (%s) has no example: "+
						"an undemonstrated body is an unchecked body", key, mediaType))
					continue
				}
				if w.request == nil {
					errs = append(errs, fmt.Errorf("(f) %s documents a request body but wirings() names no Go type", key))
					continue
				}
				if err := decodeStrict(media.Example, w.request()); err != nil {
					errs = append(errs, fmt.Errorf("(f) %s request example: %w", key, err))
				}
			}
		}

		statuses := make([]string, 0, len(op.Responses))
		for s := range op.Responses {
			statuses = append(statuses, s)
		}
		sort.Strings(statuses)

		for _, status := range statuses {
			resp, err := doc.resolveResponse(op.Responses[status])
			if err != nil {
				errs = append(errs, fmt.Errorf("(f) %s %s: %w", key, status, err))
				continue
			}
			for mediaType, media := range resp.Content {
				if len(media.Example) == 0 {
					errs = append(errs, fmt.Errorf("(f) %s %s (%s) has no example", key, status, mediaType))
					continue
				}
				target, err := responseTarget(w, status)
				if err != nil {
					errs = append(errs, fmt.Errorf("(f) %s %s: %w", key, status, err))
					continue
				}
				if err := decodeStrict(media.Example, target); err != nil {
					errs = append(errs, fmt.Errorf("(f) %s %s example: %w", key, status, err))
				}
			}
		}
	}
	sortErrs(errs)
	return errs
}

// responseTarget picks the Go type a status's example must decode into. Every
// unlisted 4xx/5xx is the error envelope — that is the package's guarantee, so
// the test states it once rather than repeating it 60 times in wirings().
func responseTarget(w opWiring, status string) (any, error) {
	if f, ok := w.success[status]; ok {
		return f(), nil
	}
	code, err := strconv.Atoi(status)
	if err != nil {
		return nil, fmt.Errorf("status %q is not a number", status)
	}
	if code >= 400 {
		return &errorEnvelope{}, nil
	}
	return nil, fmt.Errorf("a %s response carries a body but wirings() names no Go type for it", status)
}

func decodeStrict(raw json.RawMessage, target any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("does not decode into %T: %w", target, err)
	}
	return nil
}

// ------------------------------------------------- the fixture-key second pass

// schemaFixtures is one FULLY-POPULATED value per named schema: every optional
// field set, so marshalling it produces every key the type can ever emit.
//
// It is the half of (f) that catches the opposite drift from the example pass:
// an example that names a field the struct lost fails when it is decoded, and a
// struct that gains a field no example happens to mention fails here.
func schemaFixtures() map[string]any {
	ts := "2026-09-16T09:31:00Z"
	expires := ts
	removed := ts
	cursorID := "1f1c2b3a-4d5e-4f60-8a91-0b2c3d4e5f60"
	nextID := int64(4821)

	ban := banJSON{
		ID:                cursorID,
		Target:            banTargetJSON{Type: moderation.TargetIP, Value: "203.0.113.0/24"},
		State:             store.BanActive,
		Reason:            "evading an ID ban",
		CreatedAt:         ts,
		CreatedBy:         "operator@example.org",
		ExpiresAt:         &expires,
		RemovedAt:         &removed,
		RemovedBy:         "operator@example.org",
		SourceBroadcastID: "ABC234",
		CRName:            "ip-203-0-113-0-24",
		Enforcement:       &enforcementJSON{InSync: false, Detail: DetailBanPending},
	}
	delivery := deliveryJSON{
		WebhookName: "ops-pager", State: store.DeliveryDelivered, Attempts: 1,
		LastError: "502 Bad Gateway", DeliveredAt: &removed, NextAttemptAt: &removed,
	}
	event := eventJSON{
		ID: 4821, Type: store.EventBroadcastKilled, OccurredAt: ts,
		Actor: "operator@example.org", BroadcastKey: "9f2c41ab77de", BroadcastID: "ABC234",
		Reason: "terms violation", Summary: "operator@example.org ended broadcast 9f2c41ab77de",
		Deliveries: []deliveryJSON{delivery},
	}
	placement := podPlacementJSON{Pod: "gawk-server-7c9f8b6d5-2xk4p", Role: "home", ViewersLocal: 24}
	links := linksJSON{Watch: "https://gawk.example/#/view/ABC234", Telemetry: "https://t.example/#/broadcast/9f2c41ab77de"}
	banState := banStateJSON{Banned: true, Ban: &ban}
	broadcast := broadcastJSON{
		ID: "ABC234", Key: "9f2c41ab77de", PublisherActive: true, PublisherRemoteIP: "203.0.113.7",
		StartedAt: ts, ViewersGlobal: 41, Pods: []podPlacementJSON{placement},
		Links: &links, BanState: &banState,
	}
	relay := relayJSON{
		Pod: "gawk-server-7c9f8b6d5-2xk4p", Reachable: true, Version: "2.4.1",
		Config: map[string]any{"maxIdleTimeout": "30s"}, Error: "the config endpoint did not answer",
	}
	webhook := webhookJSON{
		ID: "7b8c9d0e-1f20-4314-8526-3748596a7b8c", Name: "moderation-log",
		URL: "https://log.example.org/gawk", Enabled: true, Source: SourceUI,
	}
	room := roomJSON{
		Name: "team-standup", Kind: "static", Code: "team-standup", DisplayName: "Team standup",
		MaxBroadcasts: 4, Attachments: 2, HomeHolder: "gawk-server-7c9f8b6d5-2xk4p",
		Key: "3c7d91fe20ab", CreatedAt: ts, EmptySince: ts, HasAttachSecret: true, Managed: true,
	}
	prefix := 24
	cooldown := 900

	return map[string]any{
		"Error":             errorEnvelope{Error: errorBody{Code: CodeBadRequest, Message: "reason is required"}},
		"ErrorBody":         errorBody{Code: CodeBadRequest, Message: "reason is required"},
		"Me":                meJSON{Email: "op@example.org", Subject: "sub-1", Roles: []string{RoleOperator}, Defaults: meDefaultsJSON{KillCooldownSeconds: 600}, Features: meFeaturesJSON{Rooms: true}},
		"BanTarget":         banTargetJSON{Type: moderation.TargetBroadcastID, Value: "ABC234"},
		"Enforcement":       enforcementJSON{InSync: false, Detail: DetailUnbanPending},
		"Ban":               ban,
		"BanConflict":       banConflictJSON{Error: errorBody{Code: CodeDuplicateActive, Message: "x"}, Ban: ban},
		"BanCursor":         banCursorJSON{CreatedAt: ts, ID: cursorID},
		"BansPage":          bansPageJSON{Bans: []banJSON{ban}, NextAfter: &banCursorJSON{CreatedAt: ts, ID: cursorID}},
		"KillRequest":       killRequest{Reason: "terms violation", CooldownSeconds: &cooldown},
		"KillResult":        killResultJSON{Ban: ban},
		"CreateBanRequest":  createBanRequest{Target: banTargetRequest{Type: moderation.TargetIP, Value: "publisher", PrefixLength: &prefix}, ExpiresAt: &expires, Reason: "x", SourceBroadcastID: "ABC234"},
		"PodPlacement":      placement,
		"BroadcastLinks":    links,
		"BanState":          banState,
		"Broadcast":         broadcast,
		"BroadcastsPage":    broadcastsPageJSON{Broadcasts: []broadcastJSON{broadcast}, PodsResolved: 3, PodsAnswered: 3},
		"Delivery":          delivery,
		"Event":             event,
		"EventsPage":        eventsPageJSON{Events: []eventJSON{event}, NextAfterID: &nextID},
		"Relay":             relay,
		"RelaysPage":        relaysPageJSON{Relays: []relayJSON{relay}},
		"Webhook":           webhook,
		"WebhookRequest":    webhookRequest{Name: "moderation-log", URL: "https://log.example.org/gawk", Secret: "k", Enabled: true},
		"WebhooksPage":      webhooksPageJSON{Webhooks: []webhookJSON{webhook}},
		"WebhookTestResult": TestResult{OK: false, Status: 502, Error: "502 Bad Gateway", DeliveryID: "d-1"},
		"Room":              room,
		"RoomsPage":         roomsPageJSON{Rooms: []roomJSON{room}},
		"CreateRoomRequest": createRoomRequest{Code: "team-standup", DisplayName: "Team standup", MaxBroadcasts: 4, WithAttachSecret: true},
		"RoomWithSecret":    roomWithSecretJSON{Room: room, AttachSecret: "s3cr3t"},
	}
}

func marshalFixtures(t *testing.T, in map[string]any) map[string]json.RawMessage {
	t.Helper()
	out := map[string]json.RawMessage{}
	for name, v := range in {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshalling the %s fixture: %v", name, err)
		}
		out[name] = raw
	}
	return out
}

func withExtraKey(t *testing.T, raw json.RawMessage, key string, value any) json.RawMessage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("injecting %s: %v", key, err)
	}
	m[key] = value
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("injecting %s: %v", key, err)
	}
	return out
}

// checkFixtures compares each fully-populated fixture with its schema, key by
// key, in both directions and recursively.
func checkFixtures(doc *oasDoc, fixtures map[string]json.RawMessage) []error {
	var errs []error
	names := make([]string, 0, len(fixtures))
	for n := range fixtures {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		raw, ok := doc.Components.Schemas[name]
		if !ok {
			errs = append(errs, fmt.Errorf("(f) there is a Go fixture for schema %q but openapi.yaml has no such schema", name))
			continue
		}
		var schema map[string]any
		if err := json.Unmarshal(raw, &schema); err != nil {
			errs = append(errs, fmt.Errorf("(f) schema %s: %w", name, err))
			continue
		}
		var value any
		if err := json.Unmarshal(fixtures[name], &value); err != nil {
			errs = append(errs, fmt.Errorf("(f) fixture %s: %w", name, err))
			continue
		}
		errs = append(errs, compareKeys(doc, name, schema, value)...)
	}

	// Every schema needs a fixture, so a schema added for a shape nothing
	// returns is caught too.
	for name := range doc.Components.Schemas {
		if _, ok := fixtures[name]; ok {
			continue
		}
		if isScalarSchema(doc, name) {
			continue
		}
		errs = append(errs, fmt.Errorf("(f) schema %q has no Go fixture: "+
			"add one to schemaFixtures() so something holds it to a type", name))
	}
	sortErrs(errs)
	return errs
}

// isScalarSchema reports whether a named schema is a plain string/number — an
// enum or a named scalar, which has no properties to compare.
func isScalarSchema(doc *oasDoc, name string) bool {
	var s struct {
		Type any `json:"type"`
	}
	if err := json.Unmarshal(doc.Components.Schemas[name], &s); err != nil {
		return false
	}
	t, ok := s.Type.(string)
	return ok && t != "object" && t != "array"
}

// compareKeys walks a schema and a value together: every key the Go type emits
// must be a documented property, and every documented property must be a key
// the fully-populated type emits.
func compareKeys(doc *oasDoc, path string, schema map[string]any, value any) []error {
	schema = resolveSchema(doc, schema)
	if schema == nil {
		return []error{fmt.Errorf("(f) %s: a $ref this test cannot resolve", path)}
	}

	switch v := value.(type) {
	case map[string]any:
		props := propertiesOf(doc, schema)
		if props == nil {
			if allowsAnything(schema) {
				return nil
			}
			return []error{fmt.Errorf("(f) %s: the Go type emits an object but the schema declares no properties", path)}
		}
		var errs []error
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sub, ok := props[k]
			if !ok {
				errs = append(errs, fmt.Errorf("(f) %s.%s is a field of the Go type but not a property of the schema", path, k))
				continue
			}
			errs = append(errs, compareKeys(doc, path+"."+k, sub, v[k])...)
		}
		names := make([]string, 0, len(props))
		for k := range props {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if _, ok := v[k]; !ok {
				errs = append(errs, fmt.Errorf("(f) %s.%s is a property of the schema but the fully-populated Go type never emits it", path, k))
			}
		}
		return errs

	case []any:
		items, ok := schema["items"].(map[string]any)
		if !ok {
			return []error{fmt.Errorf("(f) %s: the Go type emits an array but the schema declares no items", path)}
		}
		if len(v) == 0 {
			return nil
		}
		return compareKeys(doc, path+"[]", items, v[0])

	default:
		// A scalar, or null. Types are redocly's job (it validates every
		// example against its schema); this pass is about keys.
		return nil
	}
}

// resolveSchema follows a $ref and flattens the allOf/oneOf forms this document
// uses: allOf is "that schema, plus this description", oneOf is "that schema or
// null".
func resolveSchema(doc *oasDoc, schema map[string]any) map[string]any {
	for i := 0; i < 10; i++ {
		if ref, ok := schema["$ref"].(string); ok {
			name, ok := strings.CutPrefix(ref, "#/components/schemas/")
			if !ok {
				return nil
			}
			raw, ok := doc.Components.Schemas[name]
			if !ok {
				return nil
			}
			var next map[string]any
			if err := json.Unmarshal(raw, &next); err != nil {
				return nil
			}
			schema = next
			continue
		}
		for _, key := range []string{"allOf", "oneOf", "anyOf"} {
			branches, ok := schema[key].([]any)
			if !ok {
				continue
			}
			for _, b := range branches {
				sub, ok := b.(map[string]any)
				if !ok {
					continue
				}
				if t, isNull := sub["type"].(string); isNull && t == "null" {
					continue
				}
				schema = sub
				break
			}
			break
		}
		if _, again := schema["$ref"]; again {
			continue
		}
		return schema
	}
	return nil
}

func propertiesOf(doc *oasDoc, schema map[string]any) map[string]map[string]any {
	raw, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]map[string]any{}
	for k, v := range raw {
		sub, ok := v.(map[string]any)
		if !ok {
			continue
		}
		out[k] = sub
	}
	return out
}

func allowsAnything(schema map[string]any) bool {
	v, ok := schema["additionalProperties"]
	if !ok {
		return false
	}
	b, isBool := v.(bool)
	return isBool && b
}

func sortErrs(errs []error) {
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
}
