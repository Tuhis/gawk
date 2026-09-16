# R51 — Event contract: CloudEvents envelope, JSON Schema data, AsyncAPI document (docs/52)

**Status**: designed 2026-09-16; **not started**. Chunks **EC1–EC4** (`EC` =
Event Contract). Cross-cutting by design: a new **public** package in
`gawk-server`, a rewrite of the webhook wire format in `gawk-admin`, one
served document, and the rules that every later event has to follow. It is
numbered after R50 but **lands before R50's relay publisher and before
R49's activity webhooks**: R50 ([docs/51](51-relay-event-bus.md)) publishes
this envelope, R49 ([docs/50](50-rooms-read-api.md)) delivers it, and R48
([docs/49](49-admin-openapi.md)) stops describing webhooks in OpenAPI and
links here instead. It changes no wire type, no media path and nothing on
the public relay listener.

## 1. Purpose

gawk is about to have two event channels describing the same transitions
with two hand-rolled envelopes. The webhook one exists: `payload.go:20-37`
fixes `schema: gawk.moderation-event.v1`, four `X-Gawk-*` headers and a
`sha256=<hex>` signature over `timestamp + "." + body` (`Sign()`,
`payload.go:230`), all documented in prose in docs/42 §4.10 and
self-hosting §9.5. The bus one was drafted in docs/51 D3 as
`gawk.event.v1` with `schema`, `type`, `occurredAt`, `pod`, `seq`. Neither
has a schema file, a served description, a naming rule, a versioning rule
or a test that would fail when a field quietly changed shape. Review of
that draft (PR #322) already caught one consequence — two channels using
one type string for two payload shapes.

The owner's framing (2026-09-16): **this is a public project; plan for
longevity, not quick wins.** Webhooks have no production consumer yet
(docs/42 §4.10's ops pager is the motivating example, not a deployment),
so the webhook wire format can be replaced now at no cost to anyone, and
only now. Both channels therefore get **one standard envelope
(CloudEvents 1.0), one data schema per event type (JSON Schema 2020-12),
one machine-readable catalogue (AsyncAPI 3.0), one signing standard for
HTTP delivery (Standard Webhooks), and one lifecycle rule** — before
anything external is built against either channel.

What already exists, so the reader does not go looking:

- **The webhook wire contract**: `internal/notify/payload.go` — the header
  and schema constants (`:20-37`), the `Payload` struct whose *missing*
  fields enforce docs/42 D8 (`:98-142`), `buildPayload` (`:151`), `Sign`
  (`:230`); `notify.go:429` `DeliveryID` (a UUIDv5 per delivery row) and
  the request construction at `notify.go:472-476`; `sign_test.go:106`
  `TestSignatureVectors` pins the construction with a fixed secret,
  timestamp and body.
- **The event vocabulary**: `store.Event*` (`store.go:126-138`), the
  webhook-safe payload keys (`store.go:147-169`) and `store.Event`
  (`store.go:201-215`), with `BroadcastID` documented as "never a webhook
  payload".
- **Golden-vector discipline**: `gawk-server/wire/room_test.go:10-14` —
  vectors computed by hand, restated byte-identically in every mirror,
  never regenerated from code. The event contract copies the discipline,
  not the format.
- **Public relay packages** that `gawk-admin` imports through its
  `replace` (`gawk-admin/go.mod:10`): `wire`, `adminapi`, `moderation`,
  `oidcroles`, `rooms`. A sixth joins them here.
- **CI path filters**: `.github/workflows/ci.yml:171-191` — `server` on
  `gawk-server/`, `app` on `gawk-server/wire/` too (the mirror rule),
  `admin` on either module, `admin_ui` on `gawk-admin/(ui|internal/portal)/`
  only. That last line matters twice below.
- **The standards**, as read for this design: CloudEvents 1.0 core
  (`type` "SHOULD be prefixed with a reverse-DNS name"; `source` + `id`
  "MUST be unique for each distinct event", a re-sent duplicate "MAY have
  the same `id`"; extension names lower-case alphanumeric, at most 20
  characters), its JSON format (`application/cloudevents+json`; `data`
  as a JSON value when `datacontenttype` is JSON; the published
  `cloudevents.json` envelope schema), its NATS protocol binding
  (structured mode signalled by a `Content-Type` header starting
  `application/cloudevents`; binary mode needs NATS 2.2+ headers), the
  CloudEvents primer's versioning guidance (compatible change: keep
  `type`; incompatible: new `type`, produce both for a while), Standard
  Webhooks (`webhook-id`, `webhook-timestamp`, `webhook-signature:
  v1,<base64>` over `id.timestamp.body`, space-separated multiple
  signatures, `whsec_`-prefixed base64 secrets, `webhook-id` as the
  receiver's idempotency key), and the AsyncAPI NATS binding v0.1.0
  (operation `queue` only; nothing for JetStream).

## 2. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **Every event gawk emits, on the bus and in a webhook, is a CloudEvents 1.0 event in the JSON structured format** — one `application/cloudevents+json` object, no gawk envelope around or inside it. Attribute mapping: `specversion` `"1.0"`; `id` — on the bus `<pod>:<seq>` (docs/51 D3's dedup key, unchanged), for a portal-originated moderation event a UUIDv5 over the `moderation_events` row id in a fixed namespace (stable across retries and across receivers); `source` — `/gawk/relay/<pod>` for relay-published events, `/gawk/admin` for portal-originated ones; `type` per D6; `subject` — the fleet's HMAC'd key of the broadcast or room the event is about (the one identity that may appear anywhere, docs/42 D8 / docs/44 D16), absent for events without one; `time` RFC 3339 UTC; `datacontenttype` `application/json`; `dataschema` per D2; `data` the typed payload. **No extension attributes in v1.** When `gawk-admin` forwards a bus event to a webhook it forwards the same `id`, `source`, `type`, `subject` and `time` — it is an intermediary, not a new producer — and only `data` is projected (D4). | The draft envelope's fields map one to one onto attributes that a graduated CNCF spec already names, and every event tool a consumer might reach for (Knative, Argo Events, Redpanda Connect, n8n, generic webhook receivers) parses this without gawk-specific code. Structured mode on both channels means one parser, `nats sub` output that reads as the event, and no dependence on NATS header support; binary mode would put the same bytes in headers for no consumer that asked. Relative `source` values are allowed by the spec ("an absolute URI is RECOMMENDED"), and the only absolute base gawk owns is the reference deployment's domain, which must not be baked into every self-hosted deployment's events; `source` + `id` is still unique per producer, which is all the spec requires. Extensions are a forever commitment that also become headers in binary-mode tooling; anything an event needs to say belongs in `data`, under a schema. Forwarding unchanged attributes is what makes the webhook and the bus *the same event* to a consumer that sees both. |
| D2 | **One JSON Schema 2020-12 file per event type describes `data`**, in a new **public** package `gawk-server/events` (`events/schema/<type>.json`, exported through an `embed.FS`), with `$id` `https://gawk.ioio.fi/schemas/events/<type>.json` and that `$id` as the event's `dataschema`. Schemas never set `additionalProperties: false`, mark each property that may carry a raw identifier with `x-gawk-sensitive: true` (D4), and share common definitions (the hex key, `clientKind`, `reason` vocabularies) from one `common.json`. **The `$id` is an identifier, not a promise of a URL**: the schema resolves in the repository at the release tag and on every `gawk-admin` deployment at `/api/v1/schemas/events/<type>.json` (D3); making the literal URL resolve on the project site is deferred (§2 Rejected). | The `data` shape is the part a consumer codes against, and JSON Schema is what every language's tooling generates types and validators from. The package is public for exactly the reason `wire` is (CLAUDE.md: reuse, never mirror): the relay produces these types, `gawk-admin` consumes, projects and serves them, and a third module would import rather than restate. Sensitive fields live in the *same* schema as their webhook projection so one type never has two schemas (the PR #322 finding). The identifier uses the project's domain because the CloudEvents `type` prefix (D6) already does and a second namespace would be a second thing to keep forever; deferring resolution is stated rather than hidden because the app image builds from `gawk-app/` alone (`ci.yml:234`) and cannot copy a relay package's files without the root-context build `gawk-admin` needed. |
| D3 | **An AsyncAPI 3.0 document is the event catalogue and the registry**: `gawk-server/events/asyncapi.yaml`, embedded beside the schemas, hand-written and reviewed like code (the R48 D1 posture). Two channels: `bus` (address `gawk.{scope}.{key}.{type}`, protocol `nats`; the JetStream stream, retention and the durable consumer are described in prose and under `x-gawk-jetstream`, because the NATS binding v0.1.0 carries only `queue`) and `webhook` (address `{url}`, protocol `http`, POST, the D5 headers as the message headers schema). One message per event type: `contentType: application/cloudevents+json`, payload = the envelope with `type` fixed and `data` `$ref`ing the D2 file, `x-gawk-since`, `x-gawk-status` (`stable` | `deprecated`, D6). **A message is webhook-eligible if and only if it is listed under the `webhook` channel**; the D7 test holds `store.WebhookEventTypes()` to that list. `gawk-admin` serves it at `GET /api/v1/asyncapi.json` (D2's `internal/openapi` handler pattern: embedded, bundled once at start-up with the `$ref`s rewritten to the deployment's `/api/v1/schemas/events/…` URLs, `ETag`, unauthenticated) and the OpenAPI document's `info.description` links to it. The `@asyncapi/cli validate` step runs in the `admin-ui` CI job, **whose path filter (`ci.yml:191`) is widened to `gawk-server/events/**` and `gawk-admin/openapi.yaml`** — the second because R48 D4's `redocly lint` step has the same gap today: a change to `openapi.yaml` alone does not trigger the job that lints it. | AsyncAPI is to event channels what OpenAPI is to HTTP: channels, operations, messages, bindings, and `$ref` to plain JSON Schema, with a validator CLI and generators in the same ecosystem the R48 document already joined. Putting the document beside the schemas keeps its `$ref`s relative and lintable in place, and gives the contract one home in one public package for both producers. Channel membership as the eligibility rule replaces a Go-side list nobody can see with a line in the document a consumer reads. Serving it beside `openapi.json` is one more embedded file through a handler that exists (R48 OA2). The filter fix is a CI correctness finding, made here because this is the change that would otherwise ship a second unlinted document. |
| D4 | **One data schema, two projections.** A bus event carries every property; the webhook delivery of the same event carries the schema **minus every property marked `x-gawk-sensitive: true`** (raw broadcast IDs, room codes; IPs are never in any event, docs/51 D3), **plus two delivery-added properties**, `summary` (the one human sentence docs/42 §4.10 promises every receiver) and `portalUrl` (the deep link into the portal, HMAC'd key only), which every data schema declares as optional through `common.json`, the relay never sets, and `gawk-admin` fills on delivery; otherwise nothing changes — same `type`, same `dataschema`, same required set once sensitive properties are excluded (a sensitive property is never `required`). The projection is one function in `gawk-admin` driven by the schema's marks, and the R39 fixture assertion ("no key of any webhook body matches a broadcast-ID or room-code fixture") runs over every type. `nickname`, `label` and `reason` are free text and **not** sensitive: the self-hosting §9.5 posture ("send to a channel you would be comfortable having read") covers them, as it does today. | The alternative — a webhook type per bus type, or a second schema per type — is the two-shapes problem in a different coat. Marking the fields where the schema is written makes docs/42 D8 visible to the consumer in the document rather than only enforced in Go, and driving the projection from the marks means adding a sensitive field without marking it fails the D7 fixture test, not a reviewer's memory. |
| D5 | **Webhook delivery follows Standard Webhooks.** Headers `webhook-id` (= the CloudEvents `id`), `webhook-timestamp` (Unix seconds), `webhook-signature: v1,<base64(HMAC-SHA256(key, id + "." + timestamp + "." + body))>`; `Content-Type: application/cloudevents+json`. The four `X-Gawk-*` headers, `sha256=<hex>`, `PayloadSchema` and the timestamp-only signed material are removed, not kept alongside. **Key derivation rule**: a secret that starts with `whsec_` is base64-decoded after the prefix and the bytes are the key; any other secret string is the key verbatim. Portal-created webhooks generate `whsec_` secrets; chart-provided ones may be either. The header format permits space-separated multiple signatures; rotation is not built here, the format leaves room for it. The receiver's replay window stays `|now − timestamp| ≤ 300 s`. The synthetic test send becomes a CloudEvent of type `fi.ioio.gawk.webhook.test` from `/gawk/admin`, still writing no row. | A receiver author should find a verification library, not a paragraph. Standard Webhooks is the one open specification for this, with verifiers in every mainstream language, and its signed material (`id.timestamp.body`) is a strict superset of today's (`timestamp.body`) — the replay argument in `payload.go:221-225` holds and gains an idempotency key. The key rule exists because those libraries expect `whsec_` and decode it themselves; a receiver must be able to paste the same string into its library that the operator put in the Secret. Removing rather than dual-running is the owner's call to refactor while nothing consumes: two signature schemes would be two forever. |
| D6 | **Naming, versioning and lifecycle are rules the D7 tests enforce.** (a) `type` is `fi.ioio.gawk.<scope>.<event>`, e.g. `fi.ioio.gawk.room.participant_joined`; the NATS subject's last token is `<event>`. (b) A schema is **additive within a type**: properties are only added, only as optional; a closed vocabulary (`reason`, `clientKind`) may grow and consumers are told, in the document, to treat unknown values as unknown; nothing is removed, renamed or retyped. An additive revision keeps `type` and `dataschema` — a deliberate deviation from the primer's "dataschema SHOULD change", stated in the document: because no schema closes `additionalProperties`, an event validated against the schema a consumer was built with keeps validating, and the current schema is always served at the same URL. (c) An incompatible change is a **new type** with a version suffix (`…participant_joined.v2`) and a new schema file; the old type keeps being emitted with `x-gawk-status: deprecated` and `x-gawk-sunset: <date>` for **at least two `gawk-server` minor releases and never less than 90 days** before removal. (d) A type is `stable` from the release that ships it (`x-gawk-since` names that milestone); it enters through a design doc, and ships in one PR with its schema, its golden vector, its AsyncAPI message and, if webhook-eligible, its `store.WebhookEventTypes()` entry — the D7 tests fail on any subset. (e) No CloudEvents `type` string is ever equal to a `store.Event*` moderation type string: the store keeps its short internal names (`room.ended`) as row types, and the D7 test asserts disjointness — this is docs/51 D3's rule, restated where it is enforced. | Rules that live only in prose are the R2 lesson; each of these has a test in D7. Reverse-DNS is the spec's SHOULD and the domain is the one gawk already publishes under. The deprecation window is the primer's "produce both for some time" made measurable, and it matches R48 D9's additive-only promise so a consumer of the HTTP API and of the events gets one story. |
| D7 | **Drift gates are Go tests, in both modules, with a test-only validator.** In `gawk-server/events`: every `Type*` constant has a schema file and an AsyncAPI message and vice versa; every `$id` matches its file name and the D2 pattern; no schema sets `additionalProperties: false`; no sensitive property is `required`; every golden vector (one fully-populated event per type, hand-written, `testdata/vectors/<type>.json`) validates against the vendored CloudEvents `cloudevents.json` envelope schema **and** its data schema, and marshalling the Go fixture reproduces the vector byte for byte; every `deprecated` message has an `x-gawk-sunset`, and a sunset in the past fails the test **by design** — that failure is the reminder to open the removal PR, and it cannot be silenced except by removing the type or, with a dated note in the catalogue, moving the sunset. In `gawk-admin`, through the one row-type → CloudEvents-type table in `events` (§3): the `webhook` channel's message set equals the image of `store.WebhookEventTypes()`; every `store.AllEventTypes()` entry maps to a catalogue message; the D4 projection of every vector contains no `x-gawk-sensitive` property and still validates; the R39 raw-ID/IP fixture assertion over every projected vector; a Standard Webhooks vector (fixed key, id, timestamp, body → the exact `webhook-signature`, computed once with `openssl` and pinned) and the `whsec_` derivation rule; the type-string disjointness of D6 (e). Validation uses `github.com/santhosh-tekuri/jsonschema/v6` (draft 2020-12, Apache-2.0) **in `_test.go` files only**; the import-containment test forbids it from any non-test file in either module. It still enters `go.mod`/`go.sum`, so the `licenses` and `notices` jobs get their entry. | The R48 D3 shape, applied to events: a contract nobody is forced to update is a contract that lies. The validator is the one dependency this design adds, and it is confined to tests because the production code never validates — producers marshal typed structs, and the tests prove the structs match the schemas. Vendoring the CloudEvents envelope schema (one file, Apache-2.0, with its version noted) is what lets the tests say "this is a CloudEvent" rather than "this looks like one". |
| D8 | **What this changes in R48, R49 and R50** — recorded here and as dated notes in each doc. R48 D7: OpenAPI 3.1's `webhooks` section is **not** used; the OpenAPI document links to the AsyncAPI document and the D3 (d) enum of `GET /api/v1/events`' `type` filter is `store.AllEventTypes()`; webhook eligibility is checked by this milestone's tests. R49 D7: the activity webhook body is the D4 projection of the bus event's `data`, not a hand-shaped `Payload`; R49 D8's filter validates against the `webhook` channel's message types plus the `room.participant_rejoined` token. R50 D3: the envelope is this document's; the NATS message carries a `Content-Type: application/cloudevents+json` header (the binding's structured-mode signal) beside `Nats-Msg-Id`; the ingested row's `payload` is the whole CloudEvent and its `source` column holds the CloudEvents `id`; the relay's `internal/eventbus` imports `gawk-server/events` for the types and marshals nothing else. | One place says what moved, so a reader of any of the three does not have to diff them. |

### Rejected

- **`github.com/cloudevents/sdk-go`** — the relay's dependency set is a
  security property (docs/51 D8) and the envelope is one struct with eight
  fields; the SDK brings protocol bindings and clients gawk does not use.
  Conformance comes from the vendored envelope schema in tests (D7).
- **Binary content mode** (attributes in NATS or HTTP headers) — a second
  parser for the same bytes, a NATS-version dependency, and headers are
  where size limits bite; nothing asked for it (D1).
- **CloudEvents batch format** — the bus coalesces instead (docs/51 D3);
  a webhook carries one event.
- **Extension attributes** (`gawkseq`, `gawkrole`, `gawkactor`) — every
  extension is permanent and leaks into headers under binary-mode tooling;
  `seq` is inside `id`, the rest is `data` (D1).
- **Protobuf or Avro with a schema registry** — the right answer at a
  different scale; JSON on the wire is decided in docs/51, and a registry
  service would be a fifth deployable for a catalogue that fits in one file.
- **Documenting bus events in the OpenAPI document** — OpenAPI has no
  channel or subject vocabulary; `webhooks` covers HTTP delivery only, and
  even that leaves the bus undocumented. One catalogue, in the format built
  for it (D3).
- **Plain schema files without AsyncAPI** — workable, but channels,
  bindings and the eligibility rule would be prose again, and there would
  be no linter for the whole.
- **A `tag:` URI or a GitHub URL as `$id`** — `tag:` is permanent and
  never resolves, but tooling resolves `$ref`s against `$id` bases and
  non-hierarchical URIs break that; a GitHub URL ties the identifier to a
  hosting provider. The project domain is already the `type` namespace (D2).
- **Making `https://gawk.ioio.fi/schemas/events/…` resolve in this
  milestone** — it needs the app image to build from the repository root
  (as `gawk-admin` does) or a second static route on the site; deferred
  with that reason, and the deployment-served copy plus the repository
  cover every consumer until then (D2).
- **Keeping `X-Gawk-*` and `sha256=` alongside Standard Webhooks** — two
  signing schemes forever, to preserve a format with no consumer (D5).
- **Changing `dataschema` on additive revisions** (the primer's SHOULD) —
  it would make every additive change a URL change for consumers whose
  validation already passes; D6 (b) states the deviation and why.
- **Type versions on every type from day one** (`….v1`) — noise on the
  common case; the suffix appears when and only when a `v2` exists (D6).
- **An AsyncAPI rendering in the SPA's API view** — the AsyncAPI React
  component is a large bundle for a page that already embeds Swagger UI;
  the view links to `/api/v1/asyncapi.json` and to the repository file.

## 3. Where it plugs in

| Piece | Where it is today | What EC changes |
|---|---|---|
| Event contract package | — | **New** public `gawk-server/events`: `Event` (the envelope), one `Data` struct per type, `Type*` constants, `Marshal` (the one encoder: no HTML escaping, no trailing newline — `payload.go:208-216`'s reasons), `schema/*.json`, `asyncapi.yaml`, the vendored `cloudevents.json`, `testdata/vectors/`, the D7 tests, an import-containment test for the validator. |
| Webhook payload | `internal/notify/payload.go` (`Payload`, `buildPayload`, `Sign`, the `X-Gawk-*` constants), `notify.go:429-476` | `Payload` and `PayloadSchema` deleted; `project(ev) events.Event` builds the CloudEvent from the row (D1, D4); `Sign` becomes the Standard Webhooks construction and `DeliveryID` is replaced by the event's `id`; headers per D5; `sign_test.go` vectors replaced by the D7 vector. |
| Secrets | `webhooks.secret` (`0001_initial_schema.up.sql:59`), `config.StaticWebhook.SecretEnv` (`config.go:43-51`), the portal's create form | The D5 key-derivation rule in one function used by both sources; the portal generates `whsec_` secrets; no schema change. |
| Store | `store.Event*`, `store.Event`, the payload-key allowlist (`store.go:147-169`) | Unchanged as the row vocabulary; `store.AllEventTypes()` / `store.WebhookEventTypes()` (R48 OA1) map row types to CloudEvents types through one table in `events`; the payload-key allowlist is superseded by the D4 marks for webhook bodies and stays for portal rendering. |
| Serving | R48 OA2's `internal/openapi` (`Handler()`, embed, `servers` rewrite, `ETag`) | The same package serves `GET /api/v1/asyncapi.json` (bundled, `$ref`s rewritten) and `GET /api/v1/schemas/events/{name}.json`; both are table entries with empty roles (R48 D3). |
| OpenAPI | `gawk-admin/openapi.yaml` (R48) | No `webhooks` section; `info.description` links to the AsyncAPI document; the `type` enum on `GET /events` is `AllEventTypes()` (D8). |
| CI | `admin-ui` job (`ci.yml:1029`); the `admin_ui` filter (`ci.yml:191`) | `npx asyncapi validate ../../gawk-server/events/asyncapi.yaml` after `redocly lint`; the filter gains `gawk-server/events/` and `gawk-admin/openapi.yaml`; `licenses`/`notices` for the validator and the CLI. |
| Relay publisher (R50) | docs/51 D3's `gawk.event.v1` draft | Imports `events`; publishes `events.Marshal(ev)` with the `Content-Type` header (D8). |
| Docs | docs/42 §4.10, self-hosting §9.5, `gawk-admin/README.md`, CLAUDE.md's public-package list | §4.10 gets a dated "superseded by docs/52 D5" note; §9.5 rewritten around Standard Webhooks and the CloudEvents body with a verifier snippet; CLAUDE.md's `gawk-admin` bullet lists `events` among the `replace`d packages (release-coupling rule applies). |

## 4. Chunks and acceptance criteria

| Chunk | Scope | Verified by |
|---|---|---|
| **EC1** | The `gawk-server/events` package: envelope and data types for the R39 moderation events and the docs/51 D3 bus types (types only; R50 EB2 wires the producers), `schema/`, `common.json`, `asyncapi.yaml`, vendored `cloudevents.json`, vectors, the D7 relay-side tests, the validator containment test; the CI filter widening; `licenses`/`notices` (D1, D2, D3, D6, D7) | `go test ./events/...` green; deleting one schema file, one vector, one AsyncAPI message or one `Type*` constant each fails a named test (asserted by four `t.Run` negative cases over an in-memory mutated copy, the R48 OA1 shape); a vector with `additionalProperties: false` injected into its schema fails; `npx asyncapi validate` passes; a PR touching only `gawk-server/events/asyncapi.yaml` runs the `admin-ui` job (checked once on the PR's own run); `go build ./...` of the relay does not link the validator (`go version -m` on the binary shows no `jsonschema`). |
| **EC2** | `gawk-admin` webhook rewrite: CloudEvents body via the D4 projection, Standard Webhooks headers and signing, the `whsec_` rule, the test send, the `notify` and `sign` tests, docs/42 §4.10 note (D1, D4, D5) | `TestSignatureVectors` replaced by the pinned Standard Webhooks vector; a delivery verified by an independent implementation of the spec (the test reimplements verification from the spec text, not by calling `Sign`); the R39 fixture assertion over every projected vector; `whsec_`-prefixed and raw secrets both verify under their respective keys; a body containing `&` is sent unescaped; the retry of a delivery repeats `webhook-id`; `POST /webhooks/{name}/test` delivers a `fi.ioio.gawk.webhook.test` event and writes no row; the portal's webhook form generates and displays a `whsec_` secret exactly once. |
| **EC3** | Serving: `/api/v1/asyncapi.json` bundled with rewritten `$ref`s, `/api/v1/schemas/events/{name}.json`, `ETag`, the OpenAPI link, the API view's link (D2, D3) | Handler tests: both routes unauthenticated, `application/json`, `If-None-Match` → 304, unknown schema name → 404 in the error envelope; every `$ref` in the served AsyncAPI resolves to a served schema URL under the request's `-external-url`; `TestOpenAPIMatchesRoutes` green with the two entries; `redocly lint` green with no `webhooks`; the served AsyncAPI validates with `asyncapi validate` fetched from the dev stack. |
| **EC4** | Docs: self-hosting §9.5 rewrite with a verifier snippet in one language, §9.8 pointer to the catalogue, `gawk-admin/README.md`, `docs/README.md` index row, CLAUDE.md public-package list, `docs/gotchas.md` if anything earned it, this document's status, the ROADMAP row and entry | Review; the §9.5 snippet executed once against the dev stack's dispatcher and a portal-created `whsec_` webhook; the dated notes present in docs/42 §4.10, docs/49 D7, docs/50 D7 and docs/51 D3. |

Success criterion, end to end: on the dev stack (docs/41), a ban created in
the portal arrives at a receiver running an off-the-shelf Standard
Webhooks verifier as a CloudEvent whose `data` validates against the schema
fetched from the same deployment's `/api/v1/schemas/events/`; the AsyncAPI
document fetched from `/api/v1/asyncapi.json` validates and lists that
event under the `webhook` channel; and in the repository, adding an event
type without its schema, vector or catalogue entry fails `go test` before
it reaches CI.

**Dependencies**: R48 OA1 and OA2 (the route table, the two `EventTypes`
helpers, the serving package) before EC2 and EC3; EC1 stands alone. R50
EB1 depends on EC1; R49 RA4 depends on EC2; R50 EB4 and R49 RA5 revise the
AsyncAPI document rather than the OpenAPI `webhooks`.

## 5. Security considerations

- **The envelope changes nothing about what leaves the deployment.** A
  webhook body still carries no raw broadcast ID, room code or IP; D4 makes
  the rule visible in the schema and D7 keeps the R39 fixture assertion
  over every type. `subject` is the HMAC'd key, as `broadcastKey` and
  `roomKey` were.
- **Signing gets stronger, not merely different**: the signed material adds
  the delivery's id, so a captured delivery can be neither re-dated nor
  re-identified; the replay window is unchanged. The `whsec_` rule is a key
  *derivation*, never a place a key is stored in a new form — secrets stay
  in Kubernetes Secrets or the `webhooks` table exactly as today
  (self-hosting §9.7's inventory is unchanged).
- **Served documents expose nothing the repository does not** (R48 D2's
  argument): schemas and the catalogue are public files; examples and
  vectors carry fictional identifiers.
- **One new dependency, tests only, containment-tested** (D7). The
  production binaries gain no parser and no validator.

## 6. Operations

- **Reading the contract**: `/api/v1/asyncapi.json` and
  `/api/v1/schemas/events/<type>.json` on any deployment, or
  `gawk-server/events/` at the release tag.
- **Writing a receiver**: verify with a Standard Webhooks library using the
  secret as configured, reject `|now − webhook-timestamp| > 300 s`, parse
  the body as a CloudEvent, dispatch on `type`, validate `data` against
  `dataschema` if you want to; treat unknown `type`s and unknown enum
  values as unknown (D6). `webhook-id` is your idempotency key.
- **Adding an event type**: design doc, then one PR with the schema, the
  vector, the AsyncAPI message, the Go type, and — if it may reach a
  webhook — the `webhook` channel entry and `WebhookEventTypes()`. The
  tests list what is missing.
- **Retiring one**: mark `deprecated` with a sunset, keep emitting through
  the window, remove after; the test refuses a sunset in the past.
