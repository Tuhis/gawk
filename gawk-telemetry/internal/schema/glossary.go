package schema

// The glossary (R82, docs/84 D6): how the stored fields fit together, for the
// model that drafts SQL from an operator's question.
//
// Without it, the NQ0 spike's Sonnet got 14 of 20 questions right whatever
// else it was given, and every miss was a domain fact: that `browser` holds a
// major version, that the Windows broadcaster is a `browser` value, that a
// viewer comparison needs `role = 'viewer'`. With it, 19 of 20.
//
// It lives here, beside the field catalogue, so that a PR adding or changing a
// column documents it in the same place (docs/84 OD3). Two rules hold it to
// the code, and glossary_test.go enforces both:
//
//   - every backticked identifier resolves against the stored shape or the
//     field catalogue, so a renamed column fails the build rather than
//     teaching the model a name that no longer binds;
//   - every value it names is an enumeration the code defines (the web app's
//     describeClient, the native distributions' identities, the roles) —
//     never a value sampled from the store, because that would be data
//     leaving the homelab (docs/84 OD1).

import _ "embed"

// Glossary is the curated domain notes, as Markdown.
//
//go:embed glossary.md
var Glossary string
