// Package openapi serves the /api/v1 contract (R48, docs/49 D2).
//
// The document is embedded, so the binary and its description ship together
// and cannot be separated by a deployment. Exactly one thing about it is
// deployment-specific — the base URL — and exactly one thing is build-specific
// — which binary answered. Both are substituted here, once, at start-up:
// converting and rewriting on every request would be work repeated for an
// answer that never changes.
//
// **It is served unauthenticated**, like /auth/config. Nothing in it is
// secret: the repository is public, so the route surface already is, and a
// bot author's first command is `curl …/api/v1/openapi.json`. Hiding it would
// cost them and the portal's own API page, and protect nothing.
package openapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"sigs.k8s.io/yaml"

	gawkadmin "github.com/Tuhis/gawk/gawk-admin"
)

// Options configure the served copy.
type Options struct {
	// ExternalURL is the deployment's own base URL — the same -external-url
	// that anchors the OIDC redirect and the portalUrl in a webhook. It
	// replaces servers[0].url, so a client generated from the served document
	// calls the deployment it was fetched from.
	//
	// Empty leaves the repository's placeholder in place rather than writing
	// an empty server URL, which would generate a client that calls nothing.
	ExternalURL string
	// Version is the build string (the ldflags -X version). It rides out as
	// x-gawk-build so a version mismatch between the repository file and a
	// served copy can be traced to the binary that answered.
	Version string
	// Roles maps each SYMBOLIC role name in the document's `x-gawk-roles` to
	// the claim value this deployment actually expects.
	//
	// The repository's copy says `operator` because that is what the API
	// means; the claim value is configurable (`-operator-role`), so on a
	// deployment that renamed it, a bot author following the repository copy
	// would mint a token with the wrong role and get a 403 with no hint why.
	// Substituting here is the same move `servers[0].url` makes: everything
	// deployment-specific in the served copy is what that deployment expects.
	//
	// An unmapped symbolic name is left as written — R49's `rooms-reader` is
	// not configurable, so it needs no entry.
	Roles map[string]string
}

// Document is the served contract: JSON bytes and the ETag of those bytes.
type Document struct {
	json []byte
	etag string
	// version is info.version as the embedded file declares it — the
	// gawk-admin release version, kept by release-please.
	version string
}

// New converts, rewrites and hashes the embedded document once.
//
// A failure here is a broken build, not a runtime condition: the document is
// compiled in, so if it does not parse, no deployment of this binary would
// ever serve a valid one. main.go turns that into a refusal to start.
func New(opts Options) (*Document, error) {
	asJSON, err := yaml.YAMLToJSON(gawkadmin.OpenAPIYAML)
	if err != nil {
		return nil, fmt.Errorf("openapi: the embedded document is not valid YAML: %w", err)
	}

	// A map rather than a typed struct: this package must not become a second,
	// partial declaration of the document's shape. It touches two keys and
	// leaves everything else byte-for-byte as written.
	var doc map[string]any
	if err := json.Unmarshal(asJSON, &doc); err != nil {
		return nil, fmt.Errorf("openapi: the embedded document is not an object: %w", err)
	}

	version, _ := infoVersion(doc)
	if version == "" {
		return nil, fmt.Errorf("openapi: the embedded document declares no info.version")
	}

	if opts.ExternalURL != "" {
		servers, ok := doc["servers"].([]any)
		if !ok || len(servers) == 0 {
			return nil, fmt.Errorf("openapi: the embedded document declares no servers to rewrite")
		}
		first, ok := servers[0].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("openapi: servers[0] is not an object")
		}
		first["url"] = opts.ExternalURL
	}
	if opts.Version != "" {
		doc["x-gawk-build"] = opts.Version
	}
	substituteRoles(doc, opts.Roles)

	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("openapi: re-encoding the document: %w", err)
	}
	sum := sha256.Sum256(out)
	return &Document{
		json:    out,
		etag:    `"` + hex.EncodeToString(sum[:]) + `"`,
		version: version,
	}, nil
}

// substituteRoles rewrites every operation's `x-gawk-roles` from the symbolic
// names the repository file carries to the claim values this deployment
// expects.
//
// It walks `paths` rather than taking a list of operations, because the one
// thing worse than an unsubstituted role is a substitution that misses an
// operation somebody added later: a walk cannot be forgotten.
func substituteRoles(doc map[string]any, roles map[string]string) {
	if len(roles) == 0 {
		return
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		return
	}
	for _, item := range paths {
		operations, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for _, op := range operations {
			fields, ok := op.(map[string]any)
			if !ok {
				continue
			}
			declared, ok := fields["x-gawk-roles"].([]any)
			if !ok {
				continue
			}
			out := make([]any, 0, len(declared))
			for _, r := range declared {
				name, ok := r.(string)
				if !ok {
					out = append(out, r)
					continue
				}
				if actual, mapped := roles[name]; mapped && actual != "" {
					name = actual
				}
				out = append(out, name)
			}
			fields["x-gawk-roles"] = out
		}
	}
}

func infoVersion(doc map[string]any) (string, bool) {
	info, ok := doc["info"].(map[string]any)
	if !ok {
		return "", false
	}
	v, ok := info["version"].(string)
	return v, ok
}

// Version is info.version as the document declares it.
func (d *Document) Version() string { return d.version }

// JSON is the served bytes, for a caller that wants them without HTTP.
func (d *Document) JSON() []byte { return d.json }

// Handler serves the document.
//
// It is cacheable — five minutes, plus an ETag — because it changes only when
// the binary does, and a generator or a Swagger UI page re-fetches it on every
// load. That is the opposite of the portal's `no-store` rule, and deliberately
// so: a stale broadcast list is a stale kill button, while a stale contract is
// a contract, and this one is immutable for the life of the process.
func (d *Document) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("ETag", d.etag)
		if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, d.etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(d.json)
	})
}

// etagMatches implements enough of RFC 9110 §13.1.2 for the two forms a client
// actually sends: the exact tag, and `*`. A comma-separated list is compared
// entry by entry, because that is what a browser sends after it has seen the
// document more than once.
func etagMatches(header, etag string) bool {
	for _, candidate := range splitList(header) {
		if candidate == "*" || candidate == etag {
			return true
		}
		// A weak validator is still this exact body: the document is either
		// byte-identical or it is a different build.
		if len(candidate) > 2 && candidate[:2] == "W/" && candidate[2:] == etag {
			return true
		}
	}
	return false
}

func splitList(header string) []string {
	out := []string{}
	start := 0
	for i := 0; i <= len(header); i++ {
		if i < len(header) && header[i] != ',' {
			continue
		}
		field := header[start:i]
		for len(field) > 0 && (field[0] == ' ' || field[0] == '\t') {
			field = field[1:]
		}
		for len(field) > 0 && (field[len(field)-1] == ' ' || field[len(field)-1] == '\t') {
			field = field[:len(field)-1]
		}
		if field != "" {
			out = append(out, field)
		}
		start = i + 1
	}
	return out
}
