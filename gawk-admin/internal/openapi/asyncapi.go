package openapi

// The event contract, served beside the HTTP one (R51, docs/52 D2, D3).
//
// Two more documents through the same handler shape as openapi.json: the
// AsyncAPI catalogue at /api/v1/asyncapi.json and one JSON Schema per event
// type at /api/v1/schemas/events/<type>.json — all embedded from the public
// gawk-server/events package, all converted and rewritten once at start-up,
// all unauthenticated for the same reason the OpenAPI document is: the
// repository is public, and a receiver author's first command is `curl`.
//
// Exactly one thing about them is deployment-specific: where the schemas can
// be fetched from. The catalogue's messages `$ref` their data schemas by
// relative path (`./schema/<type>.json`), and the schemas `$ref` their shared
// definitions the same way (`common.json#/$defs/…`), so that the files lint
// and resolve in place in the repository. The served copies rewrite those
// references to this deployment's /api/v1/schemas/events/ URLs, so a consumer
// that fetches the catalogue can follow every reference without knowing the
// repository layout. The schemas' `$id`s are NOT rewritten: they are the
// identifiers `dataschema` carries, and an identifier does not change with
// the deployment that serves it (docs/52 D2).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/Tuhis/gawk/gawk-server/events"
)

// SchemasPath is the route prefix every served schema lives under, and what
// the rewritten references point at.
const SchemasPath = "/api/v1/schemas/events/"

// schemaBase is the absolute prefix references are rewritten to: the
// deployment's own base URL when it is configured, otherwise the root-relative
// path, which resolves against whatever origin the document was fetched from.
func schemaBase(externalURL string) string {
	return strings.TrimSuffix(externalURL, "/") + SchemasPath
}

// NewAsyncAPI converts, rewrites and hashes the embedded event catalogue once.
//
// The catalogue's relative `$ref`s into schema/ become this deployment's
// schema URLs; everything else is served byte-for-byte as written, including
// the `x-gawk-*` lifecycle extensions a consumer reads. Like New, a failure
// is a broken build rather than a runtime condition.
func NewAsyncAPI(opts Options) (*Document, error) {
	asJSON, err := yaml.YAMLToJSON(events.AsyncAPI)
	if err != nil {
		return nil, fmt.Errorf("asyncapi: the embedded catalogue is not valid YAML: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(asJSON, &doc); err != nil {
		return nil, fmt.Errorf("asyncapi: the embedded catalogue is not an object: %w", err)
	}
	version, _ := infoVersion(doc)
	if version == "" {
		return nil, fmt.Errorf("asyncapi: the embedded catalogue declares no info.version")
	}
	base := schemaBase(opts.ExternalURL)
	rewriteRefs(doc, func(ref string) string {
		if name, ok := strings.CutPrefix(ref, "./"+events.SchemaDir+"/"); ok {
			return base + name
		}
		return ref
	})
	if opts.Version != "" {
		doc["x-gawk-build"] = opts.Version
	}
	return finish(doc, version)
}

// SchemaSet is every served data schema, by file name.
type SchemaSet struct {
	byName map[string]*Document
}

// NewSchemas prepares every embedded schema for serving: the relative
// `$ref`s to common.json become this deployment's URL for it, `$id` stays.
func NewSchemas(opts Options) (*SchemaSet, error) {
	files, err := events.SchemaFiles()
	if err != nil {
		return nil, fmt.Errorf("schemas: listing the embedded schemas: %w", err)
	}
	base := schemaBase(opts.ExternalURL)
	set := &SchemaSet{byName: make(map[string]*Document, len(files))}
	for _, name := range files {
		raw, ok := events.SchemaByFile(name)
		if !ok {
			return nil, fmt.Errorf("schemas: %s listed but unreadable", name)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("schemas: %s is not an object: %w", name, err)
		}
		rewriteRefs(doc, func(ref string) string {
			// A relative reference to a sibling file (`common.json#/$defs/x`)
			// becomes absolute; a local one (`#/$defs/x`) is left alone.
			if strings.HasPrefix(ref, "#") || strings.Contains(ref, "://") {
				return ref
			}
			return base + ref
		})
		served, err := finish(doc, "")
		if err != nil {
			return nil, err
		}
		set.byName[name] = served
	}
	return set, nil
}

// Lookup returns the served document for a schema file name, or false for a
// name that is no schema — the answer the route turns into a 404.
func (s *SchemaSet) Lookup(name string) (*Document, bool) {
	d, ok := s.byName[name]
	return d, ok
}

// Names lists every served schema file name, for a test or a reader.
func (s *SchemaSet) Names() []string {
	out := make([]string, 0, len(s.byName))
	for name := range s.byName {
		out = append(out, name)
	}
	return out
}

// rewriteRefs walks a JSON document and replaces the value of every `$ref`
// string through f.
func rewriteRefs(node any, f func(string) string) {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			if k == "$ref" {
				if ref, ok := v.(string); ok {
					n[k] = f(ref)
				}
				continue
			}
			rewriteRefs(v, f)
		}
	case []any:
		for _, v := range n {
			rewriteRefs(v, f)
		}
	}
}

func finish(doc map[string]any, version string) (*Document, error) {
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("re-encoding the document: %w", err)
	}
	sum := sha256.Sum256(out)
	return &Document{
		json:    out,
		etag:    `"` + hex.EncodeToString(sum[:]) + `"`,
		version: version,
	}, nil
}
