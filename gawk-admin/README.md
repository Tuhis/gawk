# gawk-admin — the moderation portal

[![Service coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2FTuhis%2Fgawk%2Fbadges%2Fgawk-admin.json)](../docs/43-coverage-reporting.md) [![Portal coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2FTuhis%2Fgawk%2Fbadges%2Fgawk-admin-ui.json)](../docs/43-coverage-reporting.md)

The operator's enforcement surface: see every live broadcast on the fleet,
**kill** one everywhere at once, **ban** its ID or its publisher's IP, and get
paged about it. R39; the design, the decisions and the rejected alternatives
are in [`docs/42`](../docs/42-admin-moderation-portal.md) — this file is only
how to build and run it.

It is **optional and off by default**. Nothing else in the stack requires it:
the relay enforces bans on its own, from `Ban` custom resources that a
`kubectl apply` can create with this service completely down.

## What it is made of

| Path | |
|---|---|
| `cmd/gawk-admin` | One binary, two modes: the default serves; `gawk-admin migrate` applies the schema and exits. |
| `migrations/` | Forward-only SQL, embedded. Never applied by the serving process. |
| `internal/store` | Postgres — the system of record (bans, events, webhooks, deliveries). |
| `internal/kube` | `Ban` CR client, the reconciler/janitor, leader election. |
| `internal/relayscan` | Headless-DNS pod discovery + the relay's `/internal/admin/*` scrape. |
| `internal/auth` | OIDC JWT validation (cached JWKS) and role authorization. |
| `internal/api` | `/api/v1`. |
| `internal/portal` | The SPA, embedded with `go:embed`. |
| `ui/` | That SPA (Vite + React + TypeScript + CSS modules). |
| `deploy/` | Dockerfile + Helm chart. |

Two things about the module are worth knowing before you build it:

- It is the **fourth top-level Go module**, and it `replace`s `gawk-server`
  with the local tree. That is deliberate (docs/42 D13): `gawk-server/moderation`
  is public so the relay and the admin plane cannot drift apart on
  normalization, target matching or expiry semantics — the same reason `wire`
  is public. **The image therefore builds from the repository root**, not from
  this directory.
- The Go build **does not depend on npm**. A fresh clone compiles and runs; the
  portal then serves a page that says the bundle was never built. Build the UI
  when you want the UI.

## Build

```sh
# The service. From this directory.
go build ./cmd/gawk-admin

# The portal bundle, into internal/portal/dist where go:embed picks it up.
cd ui && npm ci && npm run build
```

## Test

```sh
go vet ./...
go test ./...
```

**The database-backed tests skip unless you give them a database.** That is
intentional — `go test ./...` stays green on a machine with no Postgres rather
than failing and training everyone to ignore red. It also means a green run
proves less than it looks like it does, so run them for real before you touch
the store, the reconciler or a migration:

```sh
docker run -d --rm --name gawk-admin-pg \
  -e POSTGRES_USER=gawk -e POSTGRES_PASSWORD=gawk -e POSTGRES_DB=gawkadmin \
  -p 55432:5432 postgres:18-alpine

export GAWK_ADMIN_TEST_DSN='postgres://gawk:gawk@127.0.0.1:55432/gawkadmin?sslmode=disable'
go test ./...
```

Each test creates and drops its own database off that DSN, so they run in
parallel and under `-race` without seeing each other's rows. CI does exactly
this with a service container, and then asserts the tests did not skip — a job
that silently skipped this suite would prove nothing.

The UI:

```sh
cd ui && npm ci && npm run lint && npm test && npm run build
```

## Run it locally

You need three things: a Postgres, a Kubernetes cluster with the `Ban` CRD (for
anything that writes bans), and an identity provider. The first is above; the
other two:

**The CRD.** Install it from the relay chart, or apply it directly:

```sh
helm template ../gawk-server/deploy/charts/gawk-server --set moderation.enabled=true \
  | kubectl apply -f -   # or: kubectl apply -f <(helm template … | yq 'select(.kind=="CustomResourceDefinition")')
```

**An identity provider.** Any spec-compliant OIDC provider works; the portal is
a **public client** (authorization code + PKCE) and there is no client secret
anywhere. A throwaway Keycloak:

```sh
docker run -d --rm --name gawk-idp -p 8081:8080 \
  -e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e KC_BOOTSTRAP_ADMIN_PASSWORD=admin \
  quay.io/keycloak/keycloak:27.0 start-dev
```

Then, in the admin console: create a realm, create a **public** client
`gawk-admin` with standard flow on and redirect URI `http://localhost:8090/*`
(add `http://localhost:5173/*` if you use the Vite dev server), give that
client a **client role** named `operator`, and assign it to your user. The
default roles-claim path is Keycloak's client-roles shape
(`resource_access.{audience}.roles`, with `{audience}` taken from
`-oidc-audience`), so with the client ID and the audience both `gawk-admin`
below, nothing else needs configuring. Point `-oidc-audience` at a separate
resource-server client and the `operator` role has to live on *that* client.

Now run it:

```sh
go run ./cmd/gawk-admin migrate \
  -pg-dsn 'postgres://gawk:gawk@127.0.0.1:55432/gawkadmin?sslmode=disable'

go run ./cmd/gawk-admin \
  -addr :8090 \
  -pg-dsn 'postgres://gawk:gawk@127.0.0.1:55432/gawkadmin?sslmode=disable' \
  -external-url http://localhost:8090 \
  -oidc-issuer http://localhost:8081/realms/gawk \
  -oidc-client-id gawk-admin \
  -oidc-audience gawk-admin \
  -relay-scan-target 127.0.0.1 \
  -relay-admin-token dev-token \
  -namespace default \
  -log-level debug
```

`migrate` first, always: the serving process **never** runs DDL. If the schema
is older than the binary's minimum it refuses to serve — `/readyz` false and a
log line saying so — rather than issuing queries against a shape it does not
understand.

Every flag has a `GAWK_ADMIN_*` environment fallback (flag > env > default), and
every knob appears in the startup log line, which is the only confirmation
surface for what a pod actually parsed.

For UI work, `npm run dev` in `ui/` proxies `/api`, `/auth`, `/healthz` and
`/readyz` to `http://127.0.0.1:8090` (override with `GAWK_ADMIN_TARGET`), so
point it at the process above or at a port-forward:

```sh
kubectl -n production port-forward svc/gawk-admin 8090:8090
```

Remember to add the dev origin to the IdP client's redirect URIs, or the
authorization request is refused before it ever reaches us.

## Deploy

Chart in `deploy/charts/gawk-admin`, image `ghcr.io/tuhis/gawk-admin`, chart at
`oci://ghcr.io/tuhis/charts/gawk-admin`. Chart version == appVersion == image
tag, always.

**The chart does not create a database.** It takes a connection to one that
already exists — `postgres.dsn`, or `postgres.dsnSecretRef: {name, key}` — and
refuses to render with neither. A CloudNativePG cluster is just the Ref form:
CNPG's generated `<cluster>-app` Secret carries the DSN under `uri`. The
database has to be up before `helm install`, because the migration Job is a
`pre-install` hook.

The full operator-facing story — bringing that Postgres (with a
copy-pasteable CNPG `Cluster` manifest), the IdP recipe, why IP bans need
`externalTrafficPolicy: Local`, what a portal compromise does and does not
yield — is in
[`docs/self-hosting.md`](../docs/self-hosting.md#9-gawk-admin--the-moderation-portal-r39).
