# R53 — OIDC for the telemetry read surface

**Status**: designed 2026-09-20 (owner decisions OD1–OD9), not started.
Chunks **TO1–TO5**, landing as **one PR** (OD9). The ROADMAP entry
([R53](../ROADMAP.md#r53--oidc-for-the-telemetry-read-surface)) carries the
summary; the decisions are restated here so the doc reads on its own.

**Relationship to R39**: this is the second surface to adopt the auth
boundary `gawk-admin` established in [docs/42](42-admin-moderation-portal.md)
D7/D17/§4.8. Wherever this doc says "as the portal", the mechanism is
docs/42's, verbatim, and is not re-argued. What *is* argued is the one thing
a second consumer forces: whether the verifier is shared or copied (§4 D2).

---

## 1. Why, and what "done" means

`gawk-telemetry`'s read listener — the dashboard, `/v1/*`, `/live`, `/mcp` —
aggregates every broadcast on the fleet. Its posture since R28
([docs/33](33-telemetry-and-diagnostics.md) D14) is *cluster-internal, with
optional basic auth for an operator who routes it through an Ingress*. The
reference deployment does exactly that: an internal Ingress at
`gawk-telemetry.kube.ioio.fi` behind one shared `admin` password in the
`gawk-fleet` Secret.

That was the right first answer and it has three costs now that R39 exists:

- **Two identity models for one operator.** The same person opens the portal
  with a Keycloak identity and a client role, then follows the portal's
  telemetry deep link (`telemetryBaseUrl/#/broadcast/<key>`, docs/42 §4.7)
  into a basic-auth prompt for a password that lives in a Secret. Granting
  or revoking a second operator is an IdP action on one surface and a
  Secret rotation on the other.
- **A shared string is not an identity.** Basic auth has no per-person
  revocation, no audit trail of who looked at what, and the credential is
  typed into a browser that caches it per origin for the session.
- **The MCP surface inherits the same string.** `docs/development.md` and
  the README hand Claude Code the read listener through a port-forward or
  the Ingress; with basic auth that means a password in a config file.

R39 already solved this for the portal — OIDC public client with PKCE,
tokens in memory, stateless JWT validation, IdP-managed roles — and the relay
adopted the same verifier for its admin routes (docs/42 §4.5). This item
makes the telemetry read surface the third consumer of that model, with the
same IdP, the same role shape and the same self-hosting recipe, so an
operator signed into the portal follows a deep link into the dashboard and
sees it open (SSO), and a second operator is one role assignment in Keycloak.

### Milestone acceptance criteria

| # | Goal | Verified by |
|---|---|---|
| G1 | With OIDC configured, every request to `/v1/*`, `/live*` and `/mcp` without a valid JWT carrying the required role is refused (401 without, 403 with a valid token lacking the role); the SPA shell and `/auth/config` stay unauthenticated | Go handler tests (TO2) |
| G2 | The dashboard runs the code+PKCE flow against a real Keycloak from **both** the internal Ingress and `kubectl port-forward` on `localhost:8081`, with tokens held in memory only and silent renewal by refresh-token rotation | UI unit tests (TO3) + manual pass |
| G3 | An operator signed into `gawk-admin` follows a telemetry deep link and lands on the broadcast view with no visible login (SSO bounce) | manual pass |
| G4 | Removing the role at the IdP locks the operator out at the next access-token refresh; an SSE stream opened earlier ends no later than that token's `exp` | Go tests (stream cap) + manual (role removal) |
| G5 | One verifier: after TO1, `gawk-server/internal/ops/auth.go` and `gawk-admin/internal/auth` contain no JWKS/throttle/discovery code of their own, the relay's containment test still passes, and the R39 AP5 suite and the relay admin-API suite pass **unchanged** | `go test` + review |
| G6 | Basic-auth mode keeps working byte-for-byte for a deployment that sets no OIDC knob; setting both modes refuses to start; the chart refuses an Ingress with neither | Go flag tests + `helm template` golden tests |
| G7 | Claude Code reaches `/mcp` through the OIDC gate, by at least one of the two paths in §4 D6, against the reference deployment | manual pass, recorded in §10 |
| G8 | Off is byte-identical: with no OIDC knob set, no response of the read listener changes except that `/auth/config` and `/readyz` gain answers | Go tests over the read mux |
| G9 | An IdP outage never takes ingest down: with discovery unresolved, `/readyz` on the read listener reports it, and the pod stays Ready because its probe is the ingest `/healthz` | `helm template` golden (probe path) + V-6 |
| G10 | A change under `common-ts/oidc-session` cannot merge without both consumers' lock files updated, and a `feat`/`fix` PR carrying it releases **both** `gawk-admin` and `gawk-telemetry`; a consumer-only PR still releases only itself | `commonlock` unit tests + the required check on a scratch PR (TO1) + the first real release after merge |

### Owner decisions (taken 2026-09-20)

| # | Decision | Choice |
|---|---|---|
| OD1 | Where the one shared verifier lives | **A public `gawk-server/oidcauth` package**, imported by relay, portal and telemetry; the relay's containment test is extended, not weakened (D2). Not a fifth Go module, not a third copy. |
| OD2 | Basic-auth mode | **Kept, exactly one mode active at a time** (D1). Not removed, not deprecated. |
| OD3 | IdP client | **A separate Keycloak client `gawk-telemetry`** with its own client-scoped role; the `gawk-admin` client is not reused via an audience mapper (D7). |
| OD4 | MCP | **The MCP authorization spec's OAuth flow first**, a client-credentials service identity as the designed-in fallback; no gawk-minted static token (D6). |
| OD5 | SPA flow code sharing | **A shared npm package in the repo** consumed by both SPAs — not a byte-identical mirror with a CI diff gate, not a free copy (D3). |
| OD5a | Where shared code lives (2026-09-21) | **A root `common-ts/` directory for shared TypeScript packages** — `common-ts/oidc-session` is the first. **Go stays as established**: shared Go is a public package in `gawk-server` (`oidcroles`, `moderation`, `events`, and now `oidcauth` per OD1); no Go module is created, moved or renamed. Languages are never mixed under one shared root. |
| OD5b | A shared change reaches every consumer (2026-09-21) | **Per-consumer lock file + CI bot bump + CI check** (D10). Each consumer commits the tree hash of every `common-ts` package it builds against; a CI job bumps stale lock files with a bot commit on the PR branch; a check fails while any is stale. The lock touch is what makes release-please attribute the change to each consumer. |
| OD6 | `/readyz` | **Served on the read listener, reporting OIDC discovery state; the pod's readinessProbe stays on the ingest `/healthz`.** Readiness is pod-wide and this pod carries the public ingest Service, so an IdP outage must never pull it out of that Service (D8). |
| OD7 | Default role name | **`telemetry-reader`** — names the capability and avoids two roles called `operator` in one realm (D7). |
| OD8 | Recipe placement | The Keycloak recipe lives in `docs/self-hosting.md` beside §9.3 (§6 here is its draft). |
| OD9 | Landing | **Everything in one PR** — TO1–TO5, one review, one release of all three components. |

## 2. Decisions restated from R39 (not re-argued)

| docs/42 | Restated here as |
|---|---|
| D7 | OIDC — not network placement — is the auth boundary of an internet-reachable admin surface. The telemetry read listener stays **internal-only by default** (docs/33 D14 is not reversed); this item changes what gates it once an operator routes it through an Ingress. |
| D17, §4.8 | Public client, code flow + PKCE, `state`/`nonce`, tokens in memory only, refresh-token rotation, no cookies, no server-side sessions, no CSRF machinery. Access tokens 5–15 min; the refresh horizon is the revocation horizon. |
| §4.8, §11.1 | The flow is hand-rolled over WebCrypto, no OIDC client library. CSP `default-src 'self'; connect-src 'self' <issuer origin>`; `frame-ancestors 'none'`; `Set-Cookie` never appears. |
| §4.5, §11.1 | Signature verification through `go-oidc`'s `RemoteKeySet` with a rate floor on the fetch; per-request verification offline; priming fetch once discovery resolves; an IdP outage degrades, never crashes. Roles via the public `oidcroles` walk, `{audience}` placeholder. |
| §5 | Client IPs appear in logs at Debug only. |

## 3. Non-goals

- **Reversing docs/33 D14.** The read listener remains ClusterIP by default;
  an Ingress is still an opt-in. This item gates it better; it does not
  recommend exposing it more.
- **Authenticating ingest.** The public ingest listener keeps the
  relay-minted session token (docs/33 D1) as its only defence. A viewer's
  browser never sees an IdP.
- **Per-user authorization inside telemetry.** One role gates the whole read
  surface, as `operator` gates the whole portal. Read-only sub-roles
  (`rooms-reader`-style, docs/50 D1) are not needed: everything here is
  already read-only.
- **Removing basic-auth mode.** It stays for the operator with no IdP, and
  for the "laptop with no network" workflow docs/33 protects — an OIDC login
  needs the IdP reachable, and that is a real trade, stated in §9.
- **A published npm package.** The shared SPA flow code (§4 D3) is a
  repo-internal workspace package, never published to a registry.
- **Grafana, RBAC on the SQL console, an audit log of queries.** docs/36's
  non-goals stand.

## 4. Decisions

### D1 — Two auth modes, exactly one at a time; OIDC is the documented Ingress path

The read listener has three configurations:

| Mode | Knobs | Gate | When |
|---|---|---|---|
| **none** (default) | nothing | none — ClusterIP + port-forward, docs/33 D14 | first install, dev |
| **basic** | `-read-user`/`-read-password` | HTTP basic auth, as today | no IdP available; offline laptop |
| **oidc** | `-oidc-issuer` + `-oidc-client-id` + `-oidc-audience` (all three) | bearer JWT + role | any Ingress; the reference deployment |

Setting knobs of both modes refuses to start with a message naming both, the
way a half-configured OIDC triple already does in the relay and the portal.
Two modes rather than one because the offline-laptop case is real (docs/33
§4.8's port-forward workflow) and because a self-hoster on a single node
should not need Keycloak to look at their own dashboard. The chart's Ingress
fail-guard becomes *"an Ingress requires `read.basicAuth.enabled` or
`read.oidc.issuer`"*, and `docs/self-hosting.md` names OIDC as the path for
anything an Ingress reaches.

**Rejected — OIDC-only, basic auth removed.** Cleaner, but it breaks the
port-forward-without-network workflow and forces an IdP on the smallest
install. The mode exclusivity rule keeps the two from ever composing into a
weaker gate.

### D2 — One verifier: a public `gawk-server/oidcauth` package, consumed by all three

Today the JWT verifier exists **twice**: `gawk-server/internal/ops/auth.go`
(relay admin API) and `gawk-admin/internal/auth` (portal). Each carries its
own copy of the `RemoteKeySet` wrapper, the JWKS fetch throttle, the capped
response body, background discovery with backoff, the priming fetch, and a
fake-issuer test harness. They agree today because R39's review made them
agree (docs/42 §11.1, twice). A third copy in `gawk-telemetry/internal/auth`
is exactly the mirror CLAUDE.md forbids and docs/42 §11.1 already paid for
once: *"a mirror hides a defect rather than doubling it."*

`auth_import_test.go` in the relay anticipated this and declined it: sharing
"verify this token" would put `go-oidc` in a package the whole relay module
can import, so `oidcroles` was cut to take decoded claims instead. It says
so explicitly: *"If a future change makes oidcroles reach for go-oidc, this
test fails — and that failure is the design question."* This is that
question, and the answer changes with a third consumer:

- **The property the test protects survives.** The containment rule is
  "only the ops auth path may reach an OIDC library". The test is a source
  walk over import paths; it gains `github.com/Tuhis/gawk/gawk-server/oidcauth`
  in its forbidden list, with `internal/ops/auth.go` the only allowed
  importer, and `oidcauth/` itself allowed to import `go-oidc`. Transport,
  hub, wire and the media path still cannot reach a verifier, directly or
  through the new package, and the test still names the offending file.
- **Nothing new enters the relay's dependency set.** `gawk-server/go.mod`
  already requires `go-oidc v3.20.0`; the package moves code between
  directories of one module.
- **What the copies would otherwise diverge on is security-critical**:
  the fetch floor, the `alg` allowlist, the capped body, the discovery
  retry. R39 found one real bug (a generation-snapshot race in the portal's
  coalescing) precisely because the two copies differed.

**Shape.** `oidcauth.New(ctx, oidcauth.Config{Issuer, Audience, RolesClaim,
Role, ClientID}, oidcauth.Options{Logger, HTTPClient, Now, JWKSFetchInterval,
JWKSFetchBurst, ResolveRetryInterval, FailureRate, FailureBurst})` returning a
`*Verifier` with `Verify(ctx, rawJWT) (Identity, error)`, `Ready()`,
`ResolveError()`, `Close()`, plus the HTTP helpers both consumers already
have: `Middleware`, `RequireRole`, `RequireAnyRole` (R49 D1),
`ConfigHandler` (`{issuer, clientId, audience}`), `SecurityHeaders(issuer)`,
and the per-IP invalid-credential limiter. `Identity` is the type
`gawk-admin/internal/identity` holds today, moved. The relay's static-token
path stays in `ops/auth.go`, wrapping the shared verifier: the static token
is the relay's own credential, not a verification mechanism.
`oidcauth/oidcauthtest` exports the fake issuer (discovery + JWKS + signer,
key rotation without racing a request, fetch counting) that both existing
test harnesses restate today, so the third consumer writes no fourth one.

**Release coupling.** `CONTRIBUTING.md`'s rule ("a relay↔admin contract
change is a `gawk-admin` change") gains `gawk-server/oidcauth/` in its list
and a second consumer: a semantic change there carries a commit touching
`gawk-admin/` **and** one touching `gawk-telemetry/` in the same PR, so
release-please cuts all three. CLAUDE.md's `gawk-admin` bullet names the
package beside `oidcroles`. The dev-only `/idp/` reverse proxy
(`-dev-oidc-proxy`, docs/42 §11.1) moves with it, so both binaries expose
the identical route for the docs/41 compose lane.

**Rejected — a fifth Go module (`gawk-oidc`).** It would keep `go-oidc` out
of the relay module's importable surface without an allowlist, but at the
price of a fifth release-please component, a fifth `replace`, and a fifth
CI matrix entry — for a package whose only consumers are already in this
repo and already build from the repo root. The allowlist is one line and
the test already exists.

**Rejected — copy into `gawk-telemetry/internal/auth` with a CI diff gate.**
The wire-mirror precedent applies to a *format* four independent toolchains
must restate; here all three consumers are Go in one repo and can import.
A byte-identical-mirror gate on ~500 lines of Go that could simply be one
package is machinery in place of a `replace` that already exists.

### D3 — The SPA flow is one repo-internal npm package, `common-ts/oidc-session`, consumed by both SPAs (OD5, OD5a)

`session.ts`, `pkce.ts` and `session.test.ts` move out of
`gawk-admin/ui/src/auth/` into **`common-ts/oidc-session/`** — the first
package under a new root directory that holds **shared TypeScript only**
(`name: "@gawk/oidc-session"`, `private: true`, `type: module`, **no build
step** — its `exports` point at the `.ts` source, which Vite and vitest
resolve directly under the `bundler` module resolution both SPAs already
use, and it has **zero runtime dependencies**). Each SPA depends on it as
`"@gawk/oidc-session": "file:../../common-ts/oidc-session"`, which `npm
ci` links; both lockfiles record it. The two app-specific values — the
`sessionStorage` key prefix and the bootstrap path — become constructor
options; `gawk-admin`'s behaviour is unchanged and its `session.test.ts`
moves with the code, so the package is tested where it lives and the
portal's own auth tests keep passing against the import.

**`common-ts/` is for TypeScript, and shared Go does not move there.**
Shared Go already has a home and a rule — public packages in `gawk-server`
(`oidcroles`, `moderation`, `adminapi`, `rooms`, `events`), consumed
through each module's `replace` — and `oidcauth` (D2) follows it. One
shared root per language, never both under one; a `common-go/` is not
created until a shared Go package exists that does *not* belong in the
relay module, which today is none.

Owner decision over the alternative I proposed (a byte-identical mirror with
a CI diff gate): one source beats a gate that only *detects* drift, and the
wire-mirror precedent is about a format four toolchains restate, not two
Vite projects in one repo. What the package costs, and how each cost is
paid:

- **Docker.** Both UI stages copy only their own `ui/` today. They change
  to mirror the repo layout — `WORKDIR /src/gawk-admin/ui` (resp.
  `gawk-telemetry`), with `COPY common-ts/oidc-session/
  /src/common-ts/oidc-session/` before `npm ci` — so the `file:` path
  resolves identically in the image and in a checkout. The layer-cache
  property the Dockerfiles document (manifests before source) is kept:
  the package's own `package.json` is copied with the manifests.
- **CI.** The `changes` job's rules for `admin`, `admin_ui`, `telemetry`,
  `telemetry_ui`, `docker_any` and `dev_stack` gain `^common-ts/`, so a
  change to the package runs every consumer's build, tests, bundle
  assertions and image build — the same reasoning the file already records
  for `gawk-server/` ("a relay change IS an admin change"). The package
  has no job of its own: it is exercised by both consumers' vitest runs
  and covered by both no-external-assets bundle tests as bundled code.
- **Releases.** A change under `common-ts/` is attributed by
  release-please to **no component**. D10 is what makes it reach every
  consumer's release.
- **Notices.** No new third-party code, so neither
  `THIRD-PARTY-NOTICES.md` changes; the docs/42 §11.1 statement that a CVE
  search for the OIDC path must target the hand-rolled flow now names
  `common-ts/oidc-session/`.

`AuthContext.tsx` is **not** in the package: it binds the session to the
portal's `ApiClient`; the telemetry UI binds it to `api/client.ts` and its
Zustand stores (D4). The rule for what is shared is *"the part that talks
to the IdP"*.

**Rejected — `gawk-app` as a third consumer.** The viewer SPA has no
authenticated surface and must not grow one; the package is for operator
UIs only.

### D4 — Bearer on every read; the live feed moves from `EventSource` to a `fetch`-driven stream, capped at `exp`

`api/client.ts` gains one seam: every `getJSON`/`postJSON` goes through the
session's `authorizedFetch`, which attaches `Authorization: Bearer`, renews
early, and throws `AuthRedirect` when it has started a full-page bounce
(docs/42 AP6's contract). In none/basic mode the seam is a plain `fetch`.

The live feed (docs/36 UD22) is the one thing a bearer token breaks:
`EventSource` cannot send a header. Three options, one taken:

- **Taken — `fetch` + `ReadableStream` with a small SSE parser** in
  `liveStore.ts`, keeping the server endpoint, the change-only hashing and
  the 2 s poll fallback exactly as they are. The client reconnects on close
  with a fresh token, through the same backoff it has today.
- **Rejected — `?access_token=` on the stream URL.** A token in a URL lands
  in Ingress access logs and browser history; the whole point of the bearer
  model is that it never does.
- **Rejected — a cookie for the stream only.** Reintroduces the cookie/CSRF
  class R39 deleted, for one endpoint.

Server side, `/live/stream` **ends the response at the token's `exp`**
(with a final `event: expired`). Without this a stream opened with a valid
token would outlive its revocation horizon indefinitely — the property G4
tests. The poll path needs no cap: each request carries a fresh check.

### D5 — What is gated, and what the page needs before it can log in

| Path | none | basic | oidc |
|---|---|---|---|
| `/` and the embedded bundle | open | basic | **open** — the shell holds no data; it is how the browser learns which IdP to bounce to |
| `GET /auth/config` | 404 | 404 | open: `{issuer, clientId, audience}` |
| `GET /v1/me` | 404 | 404 | gated: `{subject, email, roles}` — the SPA's 403 page and the MCP probe |
| `/v1/*`, `/live`, `/live/*`, `/mcp` | open | basic | **bearer + role** |

The SPA bootstraps by `GET /auth/config`: 404 means none/basic mode and the
page behaves as today; 200 starts the flow. No build-time switch, one
bundle. Security headers (docs/42 §4.8's set, `connect-src` carrying the
issuer origin) wrap the whole read mux in every mode — they are strictly
tightening, and the `frame-ancestors 'none'` half is worth having even
without OIDC. The one open question they raise is the chart library:
ECharts renders to canvas and the UI uses inline `style` attributes, both
covered by the portal's policy (`style-src 'unsafe-inline'`, `img-src
data:`), but this is asserted by the built-bundle test in TO3 and the manual
pass, not assumed (§10 V-2).

**The redirect URI is derived from the page's own origin** —
`window.location.origin + pathname` — never from a server knob. That is
what lets the same bundle log in from `https://gawk-telemetry.kube.ioio.fi/`
and from `http://localhost:8081/` on a port-forward with one client, given
both URIs are registered at the IdP (§6 recipe). `gawk-admin` needed
`-external-url` for webhook payloads; telemetry has no such consumer and
adds no knob.

### D6 — MCP: the standard challenge first, a service identity as the fallback

MCP's authorization spec (protocol revision 2025-06-18, the one
`internal/mcp` speaks) is OAuth 2.1: a resource server answers an
unauthenticated request with `401` and `WWW-Authenticate: Bearer
resource_metadata="<url>"`, serves RFC 9728 protected-resource metadata at
`/.well-known/oauth-protected-resource` naming its authorization server(s),
and the client discovers the AS, registers (RFC 7591 dynamic registration)
or uses a pre-configured client, and runs the code+PKCE flow in the
operator's browser. Claude Code implements this flow for streamable-HTTP
servers.

Two paths, both designed in, one verified before the item closes:

1. **The spec path.** `/mcp` returns the challenge; the metadata document
   names `-oidc-issuer` as the AS and `-oidc-audience` as the resource.
   Keycloak serves AS metadata and supports dynamic client registration —
   but anonymous registration is off by default (the *Trusted Hosts* policy),
   so the recipe either relaxes that policy for the operator's networks or
   pre-registers a public client `gawk-telemetry-mcp` with the loopback
   redirect URIs Claude Code uses. Which of these Claude Code needs is
   **V-4 in §10**, not an assumption. *(2026-09-22: [docs/56](56-admin-mcp.md)
   D5 takes the pre-registered public client for `gawk-admin`'s `/mcp`, and
   its V-1–V-3 spike settles this row for both services. The challenge and
   metadata helpers live in `oidcauth`, so they are written only once.)*
2. **The fallback.** A confidential client `gawk-telemetry-mcp` on the
   client-credentials grant with the role on its service account (the docs/50
   D9 shape), with a **client-level** access-token lifespan override (hours,
   not minutes) so one minted token outlives a diagnosis session, passed to
   Claude Code as a static `Authorization` header. Revocation is that
   token's lifetime; the trade is stated in the recipe and is acceptable for
   a read-only surface reached from the operator's own machine.

**Rejected — a static `-mcp-token` on the telemetry side.** It is the
relay's `-admin-api-token` shape, but the relay keeps that for a *machine
peer* (the portal) that has no browser. Claude Code has one. A gawk-minted
string is the model this item retires.

### D7 — Roles: the same knob shape as the relay's twin, default `telemetry-reader`, on a separate client (OD3, OD7)

`-oidc-roles-claim` (default `oidcroles.DefaultClaim`,
`resource_access.{audience}.roles`) and `-oidc-role` (default
**`telemetry-reader`**), mirroring the relay's
`-admin-oidc-roles-claim`/`-admin-oidc-role` and the portal's
`-oidc-roles-claim`/`-operator-role`. The role lives on a **separate
Keycloak client** (`gawk-telemetry`), so it is client-scoped and its
holder can read diagnostics and nothing else; a kill needs the portal's
`operator` on the `gawk-admin` client. Same human, two assignments, each
revocable alone. The name follows the R39 role model — one role per
capability, named for the capability (`operator`, `flagger`,
`rooms-reader`) — and the owner's call that a realm should not carry two
roles called `operator` with different powers. The blank-path/empty-role
refusal to boot carries over.

**Rejected — reusing the `gawk-admin` client with an audience mapper** so
one token opens both surfaces. Fewer IdP objects, but it makes the
telemetry SPA a second holder of a token that can kill broadcasts, and ties
the two surfaces' redirect-URI lists together. SSO already gives the
operator the "one login" experience without sharing the token.

### D8 — `/readyz` is served on the read listener; the pod's probe stays on ingest (OD6)

The read listener gains `GET /readyz`: `200` when no OIDC mode is
configured or discovery has resolved, `503` with `{"idp": "unresolved",
"error": …}` while it has not. It is what an operator curls, what the
docs/41 compose gate polls (the portal's P1 shape), and what `/v1/me`'s
error body echoes.

It is **not** the pod's readinessProbe, and the chart keeps both probes on
the ingest `/healthz`. `gawk-admin` folds `auth.Ready()` into its probe
because it runs two replicas behind one Service and an unready replica is
simply skipped. This pod is different in two ways that make the same wiring
harmful: it runs **one** replica by design (the live projection lives in
memory), and Kubernetes readiness is **pod-wide** — an unready pod leaves
*every* Service that selects it, including the public ingest Service. An
IdP outage overlapping a pod restart would then 503 every viewer's
telemetry batch at the frontend Ingress to protect a dashboard nobody can
log into anyway. The verifier's background discovery keeps retrying; until
it resolves, gated routes answer `401 idp_unavailable` (the portal's
behaviour, docs/self-hosting.md §9.3) and the SPA shows the message. State
transitions are logged.

**Rejected — probe on `/readyz` until first resolve only.** Bounds the
ingest exposure to the startup window but makes "Ready" mean two different
things over the pod's life; the honest answer is that ingest readiness and
read-side readiness are different facts, and only one of them may gate the
pod.

### D9 — Knobs, plumbed end to end

| Flag | Env | Chart | Default |
|---|---|---|---|
| `-oidc-issuer` | `GAWK_TELEMETRY_OIDC_ISSUER` | `read.oidc.issuer` | `""` |
| `-oidc-client-id` | `GAWK_TELEMETRY_OIDC_CLIENT_ID` | `read.oidc.clientId` | `""` |
| `-oidc-audience` | `GAWK_TELEMETRY_OIDC_AUDIENCE` | `read.oidc.audience` | `""` |
| `-oidc-roles-claim` | `GAWK_TELEMETRY_OIDC_ROLES_CLAIM` | `read.oidc.rolesClaim` | `resource_access.{audience}.roles` |
| `-oidc-role` | `GAWK_TELEMETRY_OIDC_ROLE` | `read.oidc.role` | `telemetry-reader` |
| `-dev-oidc-proxy` | `GAWK_TELEMETRY_DEV_OIDC_PROXY` | *(deliberately absent)* | `""` |

Issuer, client ID and audience: all three or none. `-read-user` set together
with `-oidc-issuer` refuses to start. The chart renders `read.oidc.*` into
the envs, refuses `read.ingress.enabled` with neither auth mode, refuses
both modes at once, and never renders the dev proxy (the `GAWK_DEV_CERT`
precedent). Every knob reaches `main.go`'s `config` and the startup log
line — the CLAUDE.md "plumbed through, not only into the test helper" rule.

### D10 — A `common-ts` change releases every consumer: per-consumer lock files, a CI bot bump, a CI check (OD5b)

**The problem.** release-please attributes each squash commit on `main` to
components by the **path prefixes** of the files it touched
(`gawk-admin/`, `gawk-telemetry/`, … in `release-please-config.json`). A
commit that touches only `common-ts/oidc-session/` is attributed to
nothing: both SPAs would be rebuilt from the new source at their *next*
unrelated release, and until then the deployed pair would run two different
builds of the "shared" code. `CONTRIBUTING.md` already documents this
failure for `gawk-server`'s public packages and answers it with a rule
("carry a commit touching the consumer"). A rule is what people forget;
this decision makes the tree enforce it.

**The mechanism.**

1. **A lock file per consumer, inside the consumer's release path.**
   `gawk-admin/common-ts.lock` and `gawk-telemetry/common-ts.lock`, one
   line per consumed package: `oidc-session <git tree hash>`. The tree
   hash is `git rev-parse HEAD:common-ts/oidc-session` — content-derived,
   deterministic, independent of the committer, and it changes whenever
   any byte under the package changes. Which consumers exist is not a
   second list to maintain: a consumer is any `package.json` whose
   dependencies contain a `file:../../common-ts/<pkg>` entry, and the tool
   below discovers them by walking the tree.
2. **`go run ./tools/commonlock {check|update}`** (beside `tools/icon`,
   `tools/releases`, `tools/licenses`): `update` rewrites every consumer's
   lock from the current tree; `check` exits non-zero naming each stale
   file and the command to run. Unit-tested like the other tools.
3. **A CI job, `common-ts-lock`**, `needs: changes`, gated on a new
   `common_ts` rule (`^common-ts/`), `pull_request` only, with
   `contents: write` on that job alone (the `attach-broadcast-release`
   precedent). It runs `update`; if the tree changed it commits
   `chore(common-ts): bump consumer lock files` as the bot and pushes to
   the PR branch, then runs `check`. **The check is a required status**, so
   a PR whose lock files are stale — a fork where the bot cannot push, or
   a run without the token below — is red with the exact command in the
   log, never silently merged.
4. **The push uses a token that retriggers CI.** A push made with the
   default `GITHUB_TOKEN` deliberately starts no new workflow run, which
   would leave the PR's head commit with no checks at all and branch
   protection unable to merge it. The job therefore pushes with a
   repository secret (`COMMON_TS_LOCK_TOKEN`: a fine-grained PAT or a
   GitHub App installation token with `contents: write` on this repo) and
   falls back to *check only* when the secret is absent. The retriggered
   run cancels the first through the existing `ci-<ref>` concurrency
   group, so one run per PR head is what remains — and it is the run in
   which every consumer job (D3's `^common-ts/` rules) ran against the
   bumped tree.
5. **Squash merge does the rest.** The bot commit is squashed into the
   PR's one commit on `main`, whose file list now includes both lock
   files, so release-please attributes it to `gawk-admin` **and**
   `gawk-telemetry`. The commit **type** comes from the PR title, as for
   every PR: `feat`/`fix` cut both releases, `chore`/`docs` cut none — a
   `chore` change to the shared flow releases nothing, which is correct
   and is the same rule every other path already follows.

**What it does not do.** It does not bump a consumer whose *own* code did
not change and whose lock is fresh: an ordinary telemetry-only or
admin-only PR touches no lock file and releases only itself. It does not
couple the two consumers' version numbers. It does not apply to
`gawk-server`'s public Go packages, whose consumers compile the relay tree
through `replace` and already run on every `gawk-server/` change; their
release coupling stays the `CONTRIBUTING.md` rule until a `common-go/`
exists to attach the same mechanism to.

**Rejected — a CI gate on touched paths alone** (fail unless every
consumer directory is touched): the touch is arbitrary and can be a
whitespace edit, and nothing records *which* shared content a consumer was
released with. **Rejected — release-please `linked-versions`**: it bumps
the group on every change, coupling the two version numbers permanently
for a dependency that changes rarely.

## 5. Architecture

```
                     browser (operator)
                          │
        ┌─────────────────┼──────────────────────┐
        │  GET /  (SPA)   │  GET /auth/config    │  IdP (Keycloak)
        │  open           │  open                │  ▲ code+PKCE, refresh
        │                 ▼                      │  │ (connect-src issuer)
        │      @gawk/oidc-session (common-ts/) ──┼──┘
        │                 │ Authorization: Bearer
        ▼                 ▼
  ┌─────────────────────────────────────────────┐
  │ read listener :8081 (internal Ingress /     │
  │ port-forward)                               │
  │  SecurityHeaders ─ mode switch ─ gate ──►   │
  │    /v1/*  /live  /live/stream(cap @ exp)    │
  │    /mcp (401 + resource_metadata)  /v1/me   │
  │  gate = oidcauth.Verifier (gawk-server)     │◄── JWKS (cached, floored)
  └─────────────────────────────────────────────┘
  ingest listener :8080 — unchanged (session token, docs/33 D1)
```

The verifier is the same object the relay's ops listener and the portal
construct; the telemetry binary adds a mode switch in front of it and the
`exp` cap on the one long-lived response it serves.

## 6. The identity-provider recipe (goes to `docs/self-hosting.md`)

Beside §9.3, for Keycloak:

1. Client `gawk-telemetry`: **public**, *Standard flow* on, *Direct access
   grants* off. Valid redirect URIs: `https://<telemetry host>/*` **and**
   `http://localhost:8081/*` (the port-forward; add `http://localhost:5174/*`
   for the Vite dev server). Web origins: the same origins.
2. Client role `telemetry-reader` on that client; assign it to the people
   who may read diagnostics. It grants nothing on `gawk-admin`.
3. Audience: with Keycloak's defaults, `read.oidc.audience` = the client ID.
4. Access-token lifespan and refresh rotation: already set at realm level
   for the portal (§9.3 steps 5–6); nothing new.
5. For Claude Code: §4 D6 — the path V-4 confirms, written up once verified.

## 7. What deliberately does not exist

- No cookie, no server-side session, no CSRF token, no `localStorage` token
  — asserted, as in the portal.
- No identity allowlist in any config file; no "any authenticated user"
  mode.
- No auth on ingest; no change to the relay-minted session token.
- No second copy of the verifier or of the fake issuer anywhere in the repo
  after TO1 — the containment test and G5 are the gates; no second copy of
  the SPA flow either (D3).
- No IdP dependency for the pod's liveness or readiness (D8).
- No Ingress rendered without an auth mode (chart fail-guard).

## 8. Chunks and acceptance criteria

| Chunk | Delivers | Acceptance |
|---|---|---|
| **TO1** | `gawk-server/oidcauth` + `oidcauthtest`, lifted from the two existing copies; relay `ops/auth.go` and `gawk-admin/internal/auth` reduced to thin consumers; `identity` moved; dev proxy moved; containment test extended; **`common-ts/oidc-session`** extracted from `gawk-admin/ui/src/auth/` with the two app-specific values as constructor options, both Dockerfiles re-rooted, the `changes` rules gaining `^common-ts/` (D3); **`tools/commonlock`, the two lock files, the `common-ts-lock` CI job and its required check** (D10); `CONTRIBUTING.md` (coupling list gains `oidcauth/`; a new paragraph on `common-ts/` and the lock), CLAUDE.md (`gawk-admin` bullet, a `common-ts` line in the layout) | The R39 AP5 suite, the relay admin-API suite and the portal UI auth tests pass **without edits to their assertions** (the UI tests move with the code, imports aside); `TestOnlyTheOpsAuthPathImportsAnOIDCLibrary` passes with the new forbidden path and fails when a fixture package under `internal/hub` imports `oidcauth` (test of the test); `go vet`/lint green on all three modules; the `gawk-admin` image builds and its no-external-assets bundle test passes with the package linked; `commonlock check` passes on the PR and fails on a scratch branch that edits the package without the locks; the bot bump lands on a scratch PR and retriggers CI (the token path), and the check alone goes red without the token. Zero user-visible change to the relay or the portal. |
| **TO2** | Telemetry backend: D9 knobs and mode exclusivity; gate on the read mux per D5; `/auth/config`, `/v1/me`, `/readyz` (D8); security headers in every mode; `/live/stream` cap at `exp` with `event: expired`; `401 idp_unavailable` before discovery; dev proxy | Against `oidcauthtest`: valid token → 200 on `/v1/live`, `/live/stream`, `/mcp` initialize; missing → 401 with `WWW-Authenticate`; valid without role → 403; tampered/expired/wrong `iss`/wrong `aud` → 401; verification with the issuer down succeeds from cache; a stream opened with a 10 s token ends within 10 s with `event: expired`; `/readyz` 503 before discovery and 200 after (and 200 always in none/basic mode); basic mode byte-identical to today's golden responses; both modes set → startup error naming both; `/auth/config` 404 in none/basic mode; `Set-Cookie` on no response; IPs at Debug only. |
| **TO3** | SPA: `@gawk/oidc-session` as a dependency; bootstrap on `/auth/config`; bearer via `authorizedFetch` in `api/client.ts`; `fetch`-driven SSE parser in `liveStore.ts` with the poll fallback intact; 403 page; sign-out; `no-external-assets` test still green | Unit: nothing auth-shaped in `localStorage` (asserted); bearer on every call incl. `resolveCode` POST; silent renewal before expiry; failed renewal → redirect; 401 → refresh-then-redirect; 403 → missing-role page; `event: expired` → reconnect with the new token; poll takes over when the stream fails; in none/basic mode the store behaves exactly as before (existing `liveStore.test.ts` passes). Built bundle: no external origin (existing test) and no CSP-violating construct (new static check over `dist/`); the telemetry image builds with the package linked. |
| **TO4** | MCP: `401` challenge with `resource_metadata`, `/.well-known/oauth-protected-resource`, `Origin` validation on `/mcp` as the MCP spec requires | Go tests for the challenge and the metadata document (`resource`, `authorization_servers`, `bearer_methods_supported: ["header"]`); the existing MCP-vs-HTTP byte-identity test still passes with a bearer supplied. |
| **TO5** | Chart (`read.oidc.*`, fail-guards, envs), `docs/self-hosting.md` §6 recipe + Claude Code path, `gawk-telemetry/README.md`, `docs/development.md` (Vite lane: the SPA runs the flow itself against the real IdP; `GAWK_TM_AUTH` retired), `docs/41` compose lane (telemetry joins the `fakeidp` lane), `docs/gotchas.md`, `docs/README.md`; the reference deployment switched in `~/gits/ioio` (basic auth off, `read.oidc.*` on, Keycloak client created); manual pass | `helm template` goldens: Ingress + neither mode → fail with a message naming both; both modes → fail; `read.oidc.*` → the five envs; no dev-proxy env under any values; **both probes still point at the ingest `/healthz`** (D8). Manual pass per §10 with every V row recorded. |

**One PR** (OD9): TO1–TO5 land together, titled as a `feat` so all three
components release. The PR is reviewed in chunk order — TO1's "assertions
untouched" criterion is checked on its own commits before the telemetry
commits are read — and the reference deployment switch (the GitOps repo)
happens after the release, not in the PR.

## 9. Risks

| Risk | Handling |
|---|---|
| Claude Code's MCP OAuth needs dynamic client registration that a default Keycloak refuses | D6's fallback path is designed in; V-4 decides which the recipe teaches. |
| The CSP breaks a chart (ECharts canvas/inline style) or the SQL console | Headers land in TO2 in **every** mode, so the manual pass sees it before OIDC is on anywhere; V-2. |
| A port-forward login fails because `localhost` redirect URIs were not registered | The recipe lists them; V-3 exercises exactly that. |
| The TO1 refactor changes verifier semantics invisibly | G5: both existing suites pass with their assertions untouched; a change to an assertion in TO1's commits is a review finding. |
| The `file:` package breaks one of the two Docker builds or a CI cache path | Both images build in CI on every PR already; TO1's criteria include the portal image, TO3's the telemetry image. |
| A change under `common-ts/` ships in no component's release | D10: the lock check is a required status, so the change cannot merge without touching every consumer's release path. |
| A change under `gawk-server/oidcauth/` ships in the relay release only | `CONTRIBUTING.md` rule extended to name it and all three consumers; the admin and telemetry CI jobs already run on any `gawk-server/` change. |
| The bot push leaves the PR head without checks, or cannot push at all (fork, missing secret) | D10 step 4: a retriggering token, and check-only fallback that fails red with the command to run. |
| An offline laptop can no longer open the dashboard through a port-forward | Basic/none modes remain (D1); stated in the README's exposure table. |
| A long-lived SSE connection outlives revocation | D4's `exp` cap, tested with a short token. |
| Release coupling missed: an `oidcauth` change ships in a relay release only | `CONTRIBUTING.md` rule extended to three components; the admin and telemetry CI gates already run on any `gawk-server/` change. |

## 10. Verification register (manual pass on the reference deployment)

| # | Check | Result |
|---|---|---|
| V-1 | From a phone signed into `gawk-admin`, follow a telemetry deep link: broadcast view opens with no visible login | |
| V-2 | Every dashboard view (fleet, live, history, broadcast, room, explore, SQL, rules) renders under the security headers with no CSP violation in the console | |
| V-3 | `kubectl port-forward svc/gawk-telemetry-read 8081:8081` → `http://localhost:8081/` logs in via Keycloak and loads live data | |
| V-4 | Claude Code reaches `/mcp` by the spec path (record whether DCR or a pre-registered client was needed) — or, failing that, by the service-identity fallback | |
| V-5 | Remove the `telemetry-reader` role from the signed-in user at Keycloak: the dashboard locks out at the next refresh; an open live stream ends by the old token's `exp` | |
| V-6 | Stop Keycloak for two minutes: an already-open dashboard keeps updating; a fresh tab shows the IdP-unavailable message; `/readyz` answers 503; the pod stays Ready and ingest keeps landing (watch the live view from the still-open tab) | |
| V-7 | `basicAuth` values on a scratch install still work unchanged | |

## 11. Deviations and field findings

*(empty until implementation)*

## 12. References

- [docs/42](42-admin-moderation-portal.md) D7, D17, §4.5, §4.8, §11.1 — the model
- [docs/33](33-telemetry-and-diagnostics.md) D1, D14 — the two listeners and why
- [docs/36](36-telemetry-ui-history.md) UD7, UD22 — exposure posture, the SSE feed
- [docs/50](50-rooms-read-api.md) D1, D9 — role-per-capability and service identities
- [docs/41](41-local-dev-stack.md) §4.8 — the `fakeidp` compose lane and the one-issuer-URL trick
- `gawk-server/internal/ops/auth_import_test.go` — the containment rule D2 extends
- MCP specification 2025-06-18, *Authorization*; RFC 9728; RFC 7591
