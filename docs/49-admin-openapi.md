# R48 — OpenAPI contract for the `gawk-admin` API (docs/49)

**Status**: designed 2026-09-15; **not started**. Chunks **OA1–OA4** (`OA` =
OpenAPI; two-letter prefix per the R21+ convention). `gawk-admin` only —
its Go module, its SPA, its chart values and the self-hosting guide. Nothing
here touches the relay, the wire format or any other module. **R49**
([docs/50](50-rooms-read-api.md)) is the first milestone that adds routes
under this contract and is written to depend on the drift check this one
ships. **Webhooks are not described here** (D7, revised 2026-09-16): the
event contract for both webhooks and the R50 bus is R51
([docs/52](52-event-contract.md)), and this document links to it.

## 1. Purpose

`gawk-admin` has served a JSON API under `/api/v1` since R39 (docs/42 §4.7),
and R42 added the rooms routes to it. It is a real API: bearer-JWT
authenticated, a fixed error envelope whose `code` values the SPA branches
on, cursor pagination, three-outcome mutations. But its only consumer is the
SPA that ships in the same binary, and its only description is the route
table in docs/42 §4.7 — which §11.1 already records as having drifted from
what shipped (the `202` grade, the composite cursor, the rooms routes came
later). There is no machine-readable contract, no served description, and
nothing that fails when a handler's JSON changes shape.

The owner's question that prompted this (2026-09-15): *does gawk-admin or
the relay expose HTTP APIs external software could use?* The answer today
is "yes, if you read the Go". The next milestone's consumer is a Mumble bot
in a separate repository (R49); the deliverable that makes that possible
without reading the Go is a published, served, drift-checked OpenAPI
document. The contract, not a client, is what gawk ships (owner decision).

What already exists, so the reader does not go looking:

- **The routes**: `gawk-admin/internal/api/api.go:219-246`, registered
  one by one on a `net/http` mux behind `protect()` (Authn + RequireRole).
  Plain handlers; no framework, no annotations, no reflection.
- **The error envelope**: `{"error":{"code","message"}}` with the `Code*`
  constants at `api.go:60-71` and the room codes in `rooms.go:42-50`. The
  package comment calls the codes *contract, not log text*.
- **Response types**: hand-written structs per handler (`roomJSON`,
  `relayJSON`, the ban shapes in `store`), most with doc comments that say
  which fields are portal-only and why.
- **Webhook payloads**: `internal/notify/payload.go` — one `Payload`
  struct, `gawk.moderation-event.v1`, signed with `Sign()`; the signature
  construction is already documented for receivers in docs/self-hosting
  §9.5.
- **A test that walks every route**:
  `TestRoutesAreBehindTheInjectedRoleCheck` (`api_test.go:142`) proves
  each registered route is behind the role check. It is the shape the
  drift check copies: enumerate the mux, assert a property of every entry.
- **Serving conventions**: `/auth/config` is the one unauthenticated JSON
  route, pinned to `{issuer, clientId, audience}`; the SPA is embedded and
  its no-external-assets test (`internal/portal`) enforces the CSP as a
  build property.
- **CI**: the `admin`, `admin-ui`, `licenses` and `notices` jobs in
  `.github/workflows/ci.yml`; `admin-ui` already runs `npm run lint`.

## 2. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **The contract is a hand-written OpenAPI 3.1 document, `gawk-admin/openapi.yaml`, reviewed like code.** No generator, no annotations, no new Go dependency. | The API is ~25 operations on plain `net/http` handlers with hand-shaped JSON. A generator (swaggo, kin-openapi, oapi-codegen) would need every handler annotated or every type re-declared in its dialect, add a dependency to a security-sensitive module, and still not describe the three-outcome mutation grades or the "secrets never appear" rules — those are prose. A hand-written file can say them. Drift is the risk of hand-writing, and D3 is the answer. |
| D2 | **The document is embedded and served by `gawk-admin` at `GET /api/v1/openapi.json`, unauthenticated, with `servers[0].url` rewritten to the deployment's `-external-url` at serve time.** YAML is the source; the served form is JSON (converted once at start-up with `sigs.k8s.io/yaml`, already in the module graph via client-go). `Cache-Control: public, max-age=300`, an `ETag` of the bytes. | A bot author's first command is `curl https://admin…/api/v1/openapi.json`; generator tooling fetches a URL. Nothing in the document is deployment-specific except the base URL, and nothing in it is secret: the repository is public, so the route surface already is. Unauthenticated matches `/auth/config`, the other bootstrap route, and lets the D5 page load the document before login. The security headers wrap it like everything else (`auth.SecurityHeaders` is mux-level). |
| D3 | **A Go test is the drift gate, both directions.** `Routes()` builds from a declared table (`method`, `pattern`, `roles`, `requires`, handler) instead of inline `mux.Handle` calls, where `requires` names the feature that must be on for the entry to be registered (`rooms` for the R42/R49 routes, `flagger` for R40's reserved route, empty for the rest — today's `a.opts.Rooms != nil` guard at `api.go:240`, moved into data). **The test walks the table, never a constructed mux**, so it is independent of which features the test process enables; it loads `openapi.yaml` and asserts: (a) every table entry has a matching `paths.<pattern>.<method>`, with `{id}` placeholders equal; (b) every operation in the document has a table entry — no documented-but-missing route — and an operation whose entry has a `requires` carries the matching `x-gawk-requires`, so a consumer can tell which routes a given deployment serves; (c) every `Code*` constant appears in the error schema's `code` enum and vice versa; (d) the `type` enum of `GET /api/v1/events` — its filter parameter and the response's `type` field — equals `store.AllEventTypes()` and vice versa; which of those may reach a webhook is not this document's question: `store.WebhookEventTypes()`, the subset the dispatcher forwards and the R49 `events` filter validates against, is held equal to the AsyncAPI `webhook` channel by docs/52 D7 (revised 2026-09-16; the first draft checked a webhook schema here); (e) each operation's `x-gawk-roles` matches the table's roles; (f) every response example in the document decodes into the handler's Go response type with `DisallowUnknownFields`, and every request example into its request type. | The R2 lesson in API form: a contract nobody is forced to update is a contract that lies. (f) is the cheap, dependency-free half of schema validation: an example that names a field the struct lost fails, and a struct that gains a field the example lacks is caught by a second pass that marshals a fully-populated fixture and asserts every key appears in the schema's `properties`. Full JSON-Schema validation would need `kin-openapi`; it is listed in §2 Rejected with the reason. |
| D4 | **The document is also linted as OpenAPI in CI**, by `@redocly/cli lint` run from the `admin-ui` job (a pinned devDependency in `ui/package.json`, so the lockfile fixes the version). Ruleset: `recommended`, plus `operation-operationId` and `no-unused-components` as errors. | D3 checks the document against the code; this checks the document against the standard. A malformed `$ref` or a missing `operationId` would otherwise surface only when someone feeds the file to a generator. The UI job already has node and runs lint; no new job, no new runner image. **The job's path filter must be widened** (`ci.yml:191` matches `gawk-admin/(ui|internal/portal)/` only), or a PR that changes only `openapi.yaml` never runs the step that lints it — found by docs/52 D3, fixed in EC1 or OA2, whichever lands first. |
| D5 | **The SPA gets an "API" view at `#/api`, behind the normal login, rendering the served document with an embedded `swagger-ui-dist`, lazily loaded.** The in-memory access token is injected via `requestInterceptor` so "Try it out" works against the deployment the page came from; the token is never written anywhere. The bundle is a separate Vite chunk imported on first navigation, so the moderation views' load cost is unchanged. | A bot author needs to *try* a call with their own token before writing code, and an operator wants to see what a `rooms-reader` token (R49) can and cannot do. Swagger UI over Redoc because Redoc cannot execute requests. Embedded, never a CDN: the portal's CSP is `default-src 'self'` and the no-external-assets test enforces it (docs/42 §4.8); Apache-2.0, so the `licenses` and `notices` jobs need a notices entry, nothing more. The view is behind login because it is a page of the admin portal, not because the document is secret (D2). |
| D6 | **Roles are declared per operation with `x-gawk-roles: [operator]`** (R49 adds `rooms-reader` to its routes), and the single security scheme is `bearerAuth` (`http`, `bearer`, `bearerFormat: JWT`). The `info.description` states the authentication model in three sentences: OIDC-issued JWT, roles in the token, no cookies. | OpenAPI has no vocabulary for "which role"; an extension key is the honest way to say it, and D3 (e) keeps it true. A generated client sees one bearer scheme, which is exactly the shape a client-credentials bot implements. |
| D7 | **Outbound webhooks are not described in the OpenAPI document.** *(Revised 2026-09-16; the first draft put them under OpenAPI 3.1's top-level `webhooks` with the `X-Gawk-*` headers and the `Sign()` construction.)* They are one channel of the event contract in [docs/52](52-event-contract.md): a CloudEvents body, Standard Webhooks headers, one JSON Schema per event type and an AsyncAPI 3.0 catalogue served at `/api/v1/asyncapi.json`. This document's `info.description` links to that URL, and 3.1 is kept for its JSON Schema 2020-12 alignment, not for `webhooks`. | The R50 bus made the webhook body one projection of an event that also travels on NATS; OpenAPI can describe an HTTP delivery but has no vocabulary for a subject or a stream, so describing webhooks here would have split one event across two documents in two formats. One catalogue, in the format built for event channels, with the drift tests of docs/52 D7 holding it to the code. |
| D8 | **`info.version` is the `gawk-admin` release version**, maintained by release-please's `extra-files` with `type: generic` — a `# x-release-please-version` marker comment on the `version:` line, exactly the updater every existing `extra-files` entry in `release-please-config.json` already uses (the charts' `Chart.yaml`, the broadcasters' `version.go`, the Windows `Cargo.toml`). The served document carries the same value; `x-gawk-build` carries the ldflags build string. | A consumer reading the repository file and one fetching it from a deployment must agree on which version they are looking at. The `generic` updater is proven in this repository on YAML, Go and TOML files, so no fallback is designed; the `yaml`/`jsonpath` updater was the first draft and was dropped because it would have been the one unverified piece of release automation in the chunk. |
| D9 | **What the document promises, in its own words.** `info.description` states: `/api/v1` is additive within v1 — fields and operations are added, never removed or retyped; a removal is a `/api/v2`; the error `code` enum and the event `type` enum only grow; examples carry fictional identifiers. Every response that can carry a raw broadcast ID, a room code or an IP is marked `x-gawk-sensitive: true` with a one-line reason, (webhook bodies carry their own marks in the docs/52 catalogue). | The consumer needs to know what may change under them. Additive-only is what §4.10's `enforcement` and `roomKey` fields already practised without saying so; saying so makes it a rule D3 can hold the next change to. The sensitivity marks make the D8 relaxation visible where the consumer reads, not only where the maintainer does. |

### Rejected

- **Generating the document from Go** (swaggo comment annotations,
  kin-openapi reflection, oapi-codegen server stubs) — D1: annotations on
  every handler, a dependency in the auth-bearing module, and the parts
  that matter most (mutation grades, secrecy rules) are prose either way.
- **Full JSON-Schema validation of every handler's output in tests** via
  `kin-openapi` — a dependency with its own CVE history for a check that
  D3 (f)'s example round-trip plus the fixture-key pass gets most of the
  way to. Revisit if a shape drift slips past those.
- **Serving the document only behind OIDC** — the route surface is public
  in the repository; hiding the served copy would only cost the bot author
  and the D5 page's first load (D2).
- **A generated client package shipped by gawk** — owner decision
  2026-09-15: the contract is the deliverable; the bot's repository
  generates or hand-writes its client.
- **Documenting the relay's `/internal/admin/*` routes in the same
  document** — their contract is the `adminapi` Go package (docs/42 §4.5),
  they are ClusterIP-only, and their one consumer compiles the types. A
  second description would be the mirror CLAUDE.md forbids.
- **Documenting `gawk-telemetry`'s ingest** — a different module with its
  own posture (docs/33); not this milestone.
- **Redoc** for the SPA page — read-only; no "try it" (D5).

## 3. Where it plugs in

| Piece | Where it is today | What OA changes |
|---|---|---|
| Route registration | `internal/api/api.go:213-262` inline `mux.Handle` calls; the rooms block behind `a.opts.Rooms != nil` (`api.go:240`) | A `routeTable` slice `{method, pattern, roles, requires, handler}`; `Routes()` ranges over it and skips entries whose `requires` feature is off. Behaviour identical; the table is what D3 enumerates, and D3 walks the table rather than the mux so a rooms-off test still checks the rooms routes. `TestRoutesAreBehindTheInjectedRoleCheck` walks the same table. |
| Error codes | `api.go:60-71`, `rooms.go:42-50` | Unchanged; D3 (c) reads them via a small `allErrorCodes()` helper. |
| Event types | `internal/store/store.go:126-138`; no helper enumerates them today | **Two new helpers**: `store.AllEventTypes()` (every row type; the enum of the `type` filter on `GET /events`) and `store.WebhookEventTypes()`, the webhook-eligible subset (equal to `AllEventTypes()` until R49 D8 adds stored-but-not-forwarded activity types). D3 (d) reads the first; the dispatcher's filter (R49) and the docs/52 D7 catalogue test hold the second to the AsyncAPI `webhook` channel. |
| The document | — | **New** `gawk-admin/openapi.yaml` (3.1): every route in §4.7 of docs/42 as shipped (§11.1 deviations included), the rooms routes, the schemas, examples, `x-gawk-roles`, `x-gawk-sensitive`; no `webhooks` section (D7) — a link to `/api/v1/asyncapi.json` in `info.description`. |
| Serving | `cmd/gawk-admin/main.go:205-224` mux; `internal/portal` embed pattern | **New** `internal/openapi` package: `//go:embed openapi.yaml`, YAML→JSON once, `servers` rewrite, `Handler()`; registered as a table entry `GET /api/v1/openapi.json` **inside `Routes()`** (the `/api/v1/` prefix is mounted on the outer mux via `a.Routes()` at `main.go:207`, so that is where any `/api/v1/*` route lives) with empty `roles`, which `Routes()` translates to **no `protect()` wrapper** — the one such entry, and the drift test's (e) documents it as `x-gawk-roles: []`. Registration order is irrelevant: the Go 1.22+ `ServeMux` picks the most specific pattern, so the exact route wins over the `/api/v1/` catch-all wherever it is added. |
| SPA | `ui/src/router/router.ts:42` `VIEWS`; `ui/src/views/*`; `ui/src/api/client.ts` token holder | `api` added to `VIEWS`; **new** `views/ApiView.tsx` with a `React.lazy` import of the swagger-ui bundle; `requestInterceptor` reads the token from the same holder `client.ts` uses; nav link. |
| CI | `admin-ui` job (`ci.yml:1029`) | One step: `npx redocly lint openapi.yaml` from `gawk-admin/ui` with `../openapi.yaml`. `licenses`/`notices`: the swagger-ui-dist entry. |
| Release automation | `release-please-config.json` `extra-files`, all `type: generic` | One more `generic` entry for `gawk-admin/openapi.yaml`, with the `# x-release-please-version` marker on its `info.version` line (D8). |
| Docs | docs/self-hosting §9; `gawk-admin/README.md` | §9.8 "Using the API from your own software": where the document is, the bearer model, a Keycloak client-credentials recipe for a service identity, the additive-only promise. README: one paragraph and the `#/api` page. |

## 4. Chunks and acceptance criteria

| Chunk | Scope | Verified by |
|---|---|---|
| **OA1** | The route table refactor; `openapi.yaml` covering every shipped route, error code and event type; the two `EventTypes` helpers; the D3 drift test | `go test ./internal/api/...` green with the new `TestOpenAPIMatchesRoutes` covering (a)–(f); deliberately deleting one route from the document, or renaming one `Code*` constant, or adding a field to `roomJSON` without touching the document, each fails the test (asserted once by hand in the PR description, and kept as three `t.Run` negative cases against an in-memory mutated copy). `TestRoutesAreBehindTheInjectedRoleCheck` still passes over the table, and both tests pass with `Rooms == nil` in the test options (the table, not the mux, is what they walk). Every example in the document decodes into its Go type with unknown fields disallowed. |
| **OA2** | `internal/openapi`: embed, YAML→JSON, `servers` rewrite, ETag; the `GET /api/v1/openapi.json` route; release-please `extra-files`; `redocly lint` in CI (D2, D4, D8) | Handler test: unauthenticated `GET` → 200 `application/json`, `servers[0].url` equals `-external-url`, `info.version` equals the embedded file's, security headers present, no `Set-Cookie`; a request with a bad bearer is still 200 (unauthenticated by design); `If-None-Match` → 304. CI: the lint step runs in `admin-ui` and a document with a dangling `$ref` fails it (checked once in a draft commit). The next release PR after merge bumps `info.version` via the `generic` marker (the same mechanism that bumps the charts' `Chart.yaml`), checked once when that PR opens. |
| **OA3** | The SPA `#/api` view with embedded swagger-ui-dist, lazy chunk, token injection (D5) | `ApiView.test.tsx`: the view mounts, fetches `/api/v1/openapi.json` through the client, and the interceptor sets `Authorization: Bearer <token>` on a "try it out" request; `router.test.ts` covers `#/api`; the `internal/portal` no-external-assets test passes over the built bundle (no CDN); `npm run build` emits the swagger bundle as a separate chunk not referenced by the entry chunk; `licenses`/`notices` green. Manual on the docs/41 dev stack: log in, open API, expand `GET /api/v1/me`, execute, see the JSON. |
| **OA4** | Docs: self-hosting §9.8 (bearer model, service identity recipe, additive-only promise), `gawk-admin/README.md`, `docs/README.md` index row, this document's status, the ROADMAP row and entry | Review. The recipe is executed once against the dev stack's fake IdP (`cmd/gawk-fakeidp`'s `/mint`, or Keycloak if the reference deployment is used) and the resulting token calls `GET /api/v1/me` successfully. |

Success criterion, end to end: on the reference deployment, `curl
https://<admin>/api/v1/openapi.json | openapi-generator-cli validate` (or
`redocly lint`) passes; a client generated from that document in any
language lists broadcasts with a client-credentials token; the SPA's API
page executes the same call; and in the repository, adding a route
without documenting it fails `go test` before it reaches CI.

## 5. Security considerations

- **The document exposes nothing that the repository does not.** No
  deployment-specific value beyond the external URL, no secret, no
  identifier; examples are fictional. Serving it unauthenticated is
  consistent with `/auth/config` (D2).
- **The API page runs a third-party bundle inside the security-critical
  SPA.** It is embedded (CSP `'self'`), version-pinned by the lockfile,
  loaded only on the API view, and rendered from a document the same
  binary embeds — no user-supplied spec is ever loaded. The token reaches
  it through the same in-memory holder the rest of the SPA uses and is
  never persisted (docs/42 §4.8 D17 holds).
- **Sensitivity marks are documentation, not enforcement.** A route
  marked `x-gawk-sensitive` is still gated by the same role check it was
  yesterday; the mark tells the consumer what they are receiving.

## 6. Operations

- **Reading the contract**: `https://<admin>/api/v1/openapi.json`, or the
  `#/api` page after login, or `gawk-admin/openapi.yaml` at the release
  tag.
- **Changing the API**: edit the handler and the document in the same
  commit; `go test ./internal/api/...` says whether they agree. A change
  that removes or retypes anything is a `/api/v2` conversation, not a
  patch (D9).
- **Version mismatch between file and served copy** means the binary was
  built from a commit the release PR had not touched yet; the served
  `x-gawk-build` says which binary answered.
