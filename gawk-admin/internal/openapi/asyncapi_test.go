package openapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-admin/internal/openapi"

	"github.com/Tuhis/gawk/gawk-server/events"
)

func getPath(t *testing.T, h http.Handler, path string, header map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

// refs collects every `$ref` value in a JSON document.
func refs(node any, out *[]string) {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			if k == "$ref" {
				if s, ok := v.(string); ok {
					*out = append(*out, s)
				}
				continue
			}
			refs(v, out)
		}
	case []any:
		for _, v := range n {
			refs(v, out)
		}
	}
}

// The whole point of serving the catalogue: a receiver author with no token
// can fetch it, and every reference in it resolves to a schema this same
// deployment serves (docs/52 EC3).
func TestServesTheCatalogueWithResolvableReferences(t *testing.T) {
	opts := openapi.Options{ExternalURL: "https://admin.gawk.test", Version: "1.2.3-abcdef"}
	catalogue, err := openapi.NewAsyncAPI(opts)
	if err != nil {
		t.Fatalf("NewAsyncAPI: %v", err)
	}
	schemas, err := openapi.NewSchemas(opts)
	if err != nil {
		t.Fatalf("NewSchemas: %v", err)
	}

	resp := getPath(t, catalogue.Handler(), "/api/v1/asyncapi.json", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if len(resp.Header.Values("Set-Cookie")) != 0 {
		t.Fatal("the catalogue set a cookie")
	}
	if resp.Header.Get("ETag") == "" {
		t.Fatal("no ETag")
	}
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("the served body is not JSON: %v", err)
	}
	if v, _ := doc["asyncapi"].(string); !strings.HasPrefix(v, "3.") {
		t.Fatalf("asyncapi = %q, want a 3.x document", v)
	}
	if doc["x-gawk-build"] != "1.2.3-abcdef" {
		t.Fatalf("x-gawk-build = %v", doc["x-gawk-build"])
	}
	if catalogue.Version() == "" {
		t.Fatal("Version() is empty: a consumer cannot tell which contract it read")
	}

	var all []string
	refs(doc, &all)
	external := 0
	for _, ref := range all {
		if strings.HasPrefix(ref, "#") {
			continue // intra-document
		}
		external++
		const base = "https://admin.gawk.test/api/v1/schemas/events/"
		name, ok := strings.CutPrefix(ref, base)
		if !ok {
			t.Errorf("$ref %q was not rewritten under %s", ref, base)
			continue
		}
		if _, served := schemas.Lookup(name); !served {
			t.Errorf("$ref %q points at %s, which this deployment does not serve", ref, name)
		}
	}
	if external != len(events.Types()) {
		t.Fatalf("found %d external $refs, want one per event type (%d)", external, len(events.Types()))
	}
}

// With no -external-url the references are root-relative, which resolves
// against whatever origin the document was fetched from — never an empty or
// repository-relative path.
func TestAnUnsetExternalURLYieldsRootRelativeReferences(t *testing.T) {
	catalogue, err := openapi.NewAsyncAPI(openapi.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	_ = json.Unmarshal(catalogue.JSON(), &doc)
	var all []string
	refs(doc, &all)
	for _, ref := range all {
		if strings.HasPrefix(ref, "#") {
			continue
		}
		if !strings.HasPrefix(ref, openapi.SchemasPath) {
			t.Errorf("$ref %q is not root-relative under %s", ref, openapi.SchemasPath)
		}
	}
}

// Every schema is served under its file name, keeps its `$id` (the identifier
// `dataschema` carries), and has its sibling references rewritten so a
// validator that fetches by URL can follow them.
func TestServesEverySchemaWithItsIdentifierIntact(t *testing.T) {
	schemas, err := openapi.NewSchemas(openapi.Options{ExternalURL: "https://admin.gawk.test/"})
	if err != nil {
		t.Fatal(err)
	}
	files, _ := events.SchemaFiles()
	if len(schemas.Names()) != len(files) {
		t.Fatalf("serving %d schemas, embedded %d", len(schemas.Names()), len(files))
	}
	for _, typ := range events.Types() {
		name := events.SchemaFile(typ)
		d, ok := schemas.Lookup(name)
		if !ok {
			t.Errorf("%s is not served", name)
			continue
		}
		resp := getPath(t, d.Handler(), openapi.SchemasPath+name, nil)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("ETag") == "" {
			t.Errorf("%s: status %d, ETag %q", name, resp.StatusCode, resp.Header.Get("ETag"))
		}
		var doc map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if doc["$id"] != events.SchemaID(typ) {
			t.Errorf("%s: $id = %v, want %s (identifiers do not follow the deployment)", name, doc["$id"], events.SchemaID(typ))
		}
		var all []string
		refs(doc, &all)
		for _, ref := range all {
			if strings.HasPrefix(ref, "#") {
				continue
			}
			target, _, _ := strings.Cut(ref, "#")
			if !strings.HasPrefix(target, "https://admin.gawk.test/api/v1/schemas/events/") {
				t.Errorf("%s: $ref %q was not rewritten to a served URL", name, ref)
				continue
			}
			if _, served := schemas.Lookup(strings.TrimPrefix(target, "https://admin.gawk.test/api/v1/schemas/events/")); !served {
				t.Errorf("%s: $ref %q points at a schema that is not served", name, ref)
			}
		}
	}
	if _, ok := schemas.Lookup("common.json"); !ok {
		t.Error("common.json is not served, and every schema $refs it")
	}
	for _, unknown := range []string{"nope.json", "", "../events.go", "fi.ioio.gawk.room.attached"} {
		if _, ok := schemas.Lookup(unknown); ok {
			t.Errorf("Lookup(%q) found a schema", unknown)
		}
	}
}

func TestCatalogueIfNoneMatchAnswers304(t *testing.T) {
	catalogue, err := openapi.NewAsyncAPI(openapi.Options{ExternalURL: "https://admin.gawk.test"})
	if err != nil {
		t.Fatal(err)
	}
	h := catalogue.Handler()
	etag := getPath(t, h, "/api/v1/asyncapi.json", nil).Header.Get("ETag")
	if resp := getPath(t, h, "/api/v1/asyncapi.json", map[string]string{"If-None-Match": etag}); resp.StatusCode != http.StatusNotModified {
		t.Fatalf("If-None-Match = %d, want 304", resp.StatusCode)
	}
	// Two deployments, two ETags: the references differ.
	other, _ := openapi.NewAsyncAPI(openapi.Options{ExternalURL: "https://b.example"})
	if getPath(t, other.Handler(), "/api/v1/asyncapi.json", nil).Header.Get("ETag") == etag {
		t.Fatal("two deployments with different base URLs share an ETag")
	}
}
