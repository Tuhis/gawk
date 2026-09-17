package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-admin/internal/api"

	"github.com/Tuhis/gawk/gawk-server/events"
)

// The event contract's two routes (R51, docs/52 EC3), through the /api/v1
// mux like the OpenAPI document: no token needed, JSON, cacheable with an
// ETag, and a name that is no schema answers the documented 404 envelope
// rather than falling through to the catch-all.
func TestTheEventContractIsServedWithoutAToken(t *testing.T) {
	h := newHarnessWithoutPostgres(t)
	h.identity.Roles = []string{"someone-else"}

	status, body := h.raw(http.MethodGet, "/api/v1/asyncapi.json", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/v1/asyncapi.json without the operator role = %d, want 200; body: %.200s", status, body)
	}
	if !strings.Contains(body, `"asyncapi"`) || !strings.Contains(body, events.TypeBroadcastKilled) {
		t.Fatalf("the served document does not look like the catalogue: %.200s", body)
	}
	// Every schema the catalogue references is served by this same
	// deployment, under the name the reference carries.
	var catalogue map[string]any
	if err := json.Unmarshal([]byte(body), &catalogue); err != nil {
		t.Fatal(err)
	}
	served := 0
	var walk func(any)
	walk = func(node any) {
		switch n := node.(type) {
		case map[string]any:
			for k, v := range n {
				if k == "$ref" {
					ref, _ := v.(string)
					if name, ok := strings.CutPrefix(ref, "/api/v1/schemas/events/"); ok {
						served++
						status, schema := h.raw(http.MethodGet, "/api/v1/schemas/events/"+name, nil)
						if status != http.StatusOK || !strings.Contains(schema, `"$id"`) {
							t.Errorf("GET %s = %d: %.200s", ref, status, schema)
						}
					}
					continue
				}
				walk(v)
			}
		case []any:
			for _, v := range n {
				walk(v)
			}
		}
	}
	walk(catalogue)
	if served != len(events.Types()) {
		t.Fatalf("the catalogue references %d served schemas, want one per type (%d)", served, len(events.Types()))
	}

	// common.json, which every schema $refs, is served too.
	if status, schema := h.raw(http.MethodGet, "/api/v1/schemas/events/common.json", nil); status != http.StatusOK || !strings.Contains(schema, `"$defs"`) {
		t.Fatalf("GET common.json = %d: %.200s", status, schema)
	}

	// A name that is no schema: the documented envelope, with the code a
	// client branches on — not the catch-all's, not a bare 404.
	if code := h.errorCode(http.MethodGet, "/api/v1/schemas/events/nope.json", nil, http.StatusNotFound); code != api.CodeNotFound {
		t.Fatalf("unknown schema answered code %q", code)
	}
}

// Conditional requests work on both, so a validator that re-fetches the
// schema per event pays for the bytes once.
func TestTheEventContractIsCacheable(t *testing.T) {
	h := newHarnessWithoutPostgres(t)
	for _, path := range []string{"/api/v1/asyncapi.json", "/api/v1/schemas/events/" + events.SchemaFile(events.TypeRoomAttached)} {
		resp := h.do(http.MethodGet, path, nil)
		etag := resp.Header.Get("ETag")
		if resp.StatusCode != http.StatusOK || etag == "" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			t.Fatalf("%s: status %d, ETag %q, Content-Type %q", path, resp.StatusCode, etag, resp.Header.Get("Content-Type"))
		}
		req, err := http.NewRequest(http.MethodGet, h.srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("If-None-Match", etag)
		again, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = again.Body.Close()
		if again.StatusCode != http.StatusNotModified {
			t.Fatalf("%s with If-None-Match = %d, want 304", path, again.StatusCode)
		}
	}
}
