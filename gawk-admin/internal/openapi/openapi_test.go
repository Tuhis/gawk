package openapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-admin/internal/openapi"
)

func newDoc(t *testing.T, opts openapi.Options) *openapi.Document {
	t.Helper()
	d, err := openapi.New(opts)
	if err != nil {
		t.Fatalf("openapi.New: %v", err)
	}
	return d
}

func get(t *testing.T, h http.Handler, header map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

// The whole point of serving it: a bot author with no token can fetch it, and
// a client generated from it calls the deployment it came from.
func TestServesTheDocumentUnauthenticated(t *testing.T) {
	d := newDoc(t, openapi.Options{ExternalURL: "https://admin.gawk.test", Version: "1.2.3-abcdef"})
	resp := get(t, d.Handler(), nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	// No cookie, ever — the rule that runs through every response of this
	// binary, and this is the one route that is not behind the middleware.
	if len(resp.Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("the contract set a cookie: %v", resp.Header.Values("Set-Cookie"))
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=300" {
		t.Fatalf("Cache-Control = %q", cc)
	}
	if resp.Header.Get("ETag") == "" {
		t.Fatal("no ETag: every re-fetch would transfer the whole document")
	}

	var doc struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Version string `json:"version"`
		} `json:"info"`
		Servers []struct {
			URL string `json:"url"`
		} `json:"servers"`
		Build string `json:"x-gawk-build"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("the served body is not JSON: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Fatalf("openapi = %q, want a 3.1 document", doc.OpenAPI)
	}
	if len(doc.Servers) == 0 || doc.Servers[0].URL != "https://admin.gawk.test" {
		t.Fatalf("servers[0].url = %+v, want the deployment's own -external-url", doc.Servers)
	}
	if doc.Info.Version != d.Version() {
		t.Fatalf("info.version = %q but Version() says %q", doc.Info.Version, d.Version())
	}
	if doc.Info.Version == "" {
		t.Fatal("info.version is empty: a consumer cannot tell which contract it read")
	}
	if doc.Build != "1.2.3-abcdef" {
		t.Fatalf("x-gawk-build = %q, want the binary's build string", doc.Build)
	}
}

// An unconfigured -external-url leaves the placeholder rather than writing an
// empty server URL, which would generate a client that calls nothing.
func TestAnUnsetExternalURLKeepsThePlaceholder(t *testing.T) {
	d := newDoc(t, openapi.Options{})
	var doc struct {
		Servers []struct {
			URL string `json:"url"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(d.JSON(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Servers) == 0 || doc.Servers[0].URL == "" {
		t.Fatalf("servers = %+v, want the repository's placeholder", doc.Servers)
	}
}

func TestIfNoneMatchAnswers304(t *testing.T) {
	d := newDoc(t, openapi.Options{ExternalURL: "https://admin.gawk.test"})
	h := d.Handler()
	etag := get(t, h, nil).Header.Get("ETag")

	for _, header := range []string{etag, "W/" + etag, "*", `"something-else", ` + etag} {
		resp := get(t, h, map[string]string{"If-None-Match": header})
		if resp.StatusCode != http.StatusNotModified {
			t.Fatalf("If-None-Match: %s = %d, want 304", header, resp.StatusCode)
		}
	}

	resp := get(t, h, map[string]string{"If-None-Match": `"a-different-build"`})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a stale If-None-Match = %d, want 200 with the current document", resp.StatusCode)
	}
}

// A deployment that renamed the operator role must serve a document naming the
// role it actually expects.
//
// The repository file says `operator` because that is what the API means, but
// `-operator-role` renames the claim value. A bot author reading the served
// document and minting a token with `operator` on such a deployment would get
// a 403 with nothing to explain it.
func TestTheServedRolesAreThisDeploymentsClaimValues(t *testing.T) {
	d := newDoc(t, openapi.Options{Roles: map[string]string{"operator": "gawk-mod"}})

	var doc struct {
		Paths map[string]map[string]struct {
			Roles []string `json:"x-gawk-roles"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(d.JSON(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}

	substituted, unauthenticated := 0, 0
	for path, item := range doc.Paths {
		for method, op := range item {
			switch {
			case len(op.Roles) == 0:
				unauthenticated++
			case op.Roles[0] == "gawk-mod":
				substituted++
			default:
				t.Fatalf("%s %s still says %v; a token carrying that is refused here",
					method, path, op.Roles)
			}
		}
	}
	if substituted == 0 {
		t.Fatal("no operation carried a substituted role: the walk found nothing")
	}
	if unauthenticated != 1 {
		t.Fatalf("%d operations declare no role, want exactly 1 (the contract itself)", unauthenticated)
	}
}

// With no mapping — every deployment that kept the default — the document is
// served exactly as written.
func TestTheSymbolicRolesSurviveWithNoMapping(t *testing.T) {
	d := newDoc(t, openapi.Options{})
	if !strings.Contains(string(d.JSON()), `"x-gawk-roles":["operator"]`) {
		t.Fatal("the served document does not carry the symbolic role it was written with")
	}
}

// The ETag has to follow the SERVED bytes, not the embedded file: two
// deployments of the same binary serve different documents, because
// servers[0].url differs.
func TestTheETagFollowsTheServedBytes(t *testing.T) {
	a := newDoc(t, openapi.Options{ExternalURL: "https://a.example"})
	b := newDoc(t, openapi.Options{ExternalURL: "https://b.example"})
	ea := get(t, a.Handler(), nil).Header.Get("ETag")
	eb := get(t, b.Handler(), nil).Header.Get("ETag")
	if ea == eb {
		t.Fatal("two deployments with different base URLs share an ETag: a cache would serve one the other's document")
	}
}
