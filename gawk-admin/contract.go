// Package gawkadmin exists for exactly one reason: `//go:embed` cannot reach
// outside its own directory, and `openapi.yaml` lives at the module root
// because that is where a human browsing the repository, `redocly lint` and
// release-please's `extra-files` entry all look for it (R48, docs/49 D2, D8).
//
// So this file is the embed, and nothing else. `internal/openapi` owns every
// decision about what is done with the bytes.
package gawkadmin

import _ "embed"

// OpenAPIYAML is the /api/v1 contract, verbatim.
//
// It is the source form: YAML, because that is what is reviewable in a diff.
// The served form is JSON, converted once at start-up.
//
//go:embed openapi.yaml
var OpenAPIYAML []byte
