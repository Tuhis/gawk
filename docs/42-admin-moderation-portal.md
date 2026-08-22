# R39 — Admin portal for moderation (docs/42)

**Status**: designed 2026-08-20; **AP1–AP8 implemented 2026-08-20**. One item outstanding and it is the milestone-closing one — the manual pass on the reference deployment (§11.3), which needs a real cluster, a real identity provider and a live broadcast. Implementation record: §11.

---

## 1. Purpose

Give the operator enforcement tools the platform has never had: **stop a
fraudulent or malicious broadcast fleet-wide**, **ban it (and optionally its
publisher's IP) from coming back**, and see enough live state to act — without
killing relay pods and every innocent broadcast on them. The portal is an
*actuator*: for observation it deep-links into the R31 telemetry UI rather
than duplicating it.

This milestone also lays the rails R40 (automatic content flagging) runs on:
the kill/ban API, the operator-notification surface, the terminal close code,
and the named schema for content-flag events. R40 *produces* into this
machinery; it does not grow a second enforcement path.

Everything here was grounded against the code on 2026-08-20; file:line
references are to that state.

---

## 2. Decisions (locked with the owner, 2026-08-20)

| # | Decision | Rationale |
|---|---|---|
| D1 | **A separate `gawk-admin` service** — the fourth top-level module, with its own chart and image — not an admin listener inside `gawk-server`. | Keeps the relay image and attack surface unchanged; the portal's heavy dependencies (OIDC client, Postgres driver, embedded SPA) never enter the data plane. Gives R40's sampler an obvious neighbour. Cost: a fourth release-please component (accepted). |
| D2 | **Enforcement state is projected into a `Ban` CRD that relay pods watch; Postgres is `gawk-admin`'s system of record.** The data plane never calls the admin plane at runtime. | The ban check sits on the publish path and must answer from pod-local memory. Watching CRs (the `cluster.go` informer pattern) means enforcement survives `gawk-admin` outages *and* relay cold-starts while admin is down — the k8s API (replicated etcd) is the always-on distribution layer. A poll-the-admin design has a fail-open window on relay cold start; rejected (§7). |
| D3 | **Postgres** is `gawk-admin`'s database (audit trail, kill history, events, webhooks; R40 content flags later). Behind a store interface. | Owner's call over embedded SQLite: a real DB the R40 event stream can grow into, with `psql` tooling — and the shared store every replica of a horizontally-scaled `gawk-admin` reads and writes (D16), which file-on-PVC could never be. CNPG is the operator-grade way to run one, and the chart supports it without templating a line of it: the chart takes a DSN, and CNPG's generated `<cluster>-app` Secret is one (§4.13). |
| D4 | **Ban handles: broadcast ID and publisher IP (CIDR), each independently timed or permanent.** One target per `Ban` CR. | The resume token is `HMAC(key, broadcastID)` — stateless — so banning the ID *is* banning the token (`internal/transport/resume.go:76-92`). IP is the only handle that spans a re-mint loop. The publish secret is fleet-global and useless as identity (`docs/40` D4). Honest limit, stated in the UI: with no accounts, nothing stronger than ID+IP exists. |
| D5 | **A plain "kill" is a ban on the ID with a default 10-minute cooldown** (configurable). "Kill + ban" takes explicit durations and optionally the IP. | The broadcaster auto-reclaims with its resume token within seconds, so a kill with no ID ban resurrects before the portal refreshes. The cooldown means the operator is never racing auto-resume while deciding on a real ban. |
| D6 | **New terminal close code `4006` (`CloseCodeTerminatedByOperator`)**, sent to the publisher *and* to viewers. Terminal everywhere: no reconnect, no auto-resume. | Owner chose viewer-visible transparency over reusing 4000. Costs the full mirror pass (wire.ts, Rust crate, wirecheck, golden vectors) and viewer/broadcaster terminal handling — priced into AP1. |
| D7 | **The portal is internet-exposable, protected by OIDC**: the SPA is an OIDC public client (code flow + PKCE), and the API is authenticated by the provider-issued JWT on every request, authorized by IdP-managed roles carried in the token (D17). Chart default remains ClusterIP + port-forward; public exposure is an explicit opt-in. | Deliberate deviation from the roadmap sketch's "never routed publicly": a paged operator (R40) must be able to judge and kill from a phone, not a laptop with kubectl. For this surface, OIDC — not network placement — is the auth boundary. The telemetry read listener's posture is unchanged. |
| D8 | **The raw-broadcast-ID invariant relaxes only on authenticated admin surfaces**: the OIDC-gated portal and the credential-gated relay admin endpoints (§4.5). Public `/statusz` stays HMAC'd. **Webhook payloads never carry raw IDs or IPs** — they carry the HMAC'd key and a portal link. | The operator needs the raw ID to join and judge a stream. Webhooks transit third-party push infrastructure (ntfy/Slack/Matrix); a raw ID is a join capability and must not land there. |
| D9 | **Operator notifications are generic signed webhooks — plural, managed two ways**: chart values define config-sourced webhooks (visible but immutable in the UI), and the portal can create/edit/delete its own (stored in Postgres). Every event fans out to all enabled webhooks, each HMAC-SHA256-signed with its own secret. | ntfy/Slack/Discord/Matrix all consume a bare webhook; no vendor coupling. Chart-defined ones keep the paging pipe GitOps-reviewable; UI-defined ones let the operator add a channel from a phone. Smallest surface that satisfies R40's "a flag must reach a human". |
| D10 | **Dynamic settings = read-only effective-config view only.** Each relay pod reports its parsed config (secrets redacted); the portal displays it per pod. No write path of any kind. | GitOps stays the only mutation channel (CLAUDE.md deploy model). The read view is cheap and genuinely useful for "which pod has the stale flag?" debugging. |
| D11 | **Content-flag naming and schema are fixed now; the endpoint ships in R40.** The noun is **content flag** everywhere — `POST /api/v1/content-flags`, event type `content_flag.raised` — never bare "flag" (reads as feature flag). | R40 integrates against a frozen contract (§4.11) without R39 shipping dead code. |
| D12 | **Fleet enumeration is a per-pod scrape of a new authenticated relay endpoint** (`GET /internal/admin/broadcasts` on the ops listener; static bearer token **or** an OIDC JWT — §4.5), not Lease listing. | Works identically in single-pod and cluster mode (Leases only exist in cluster mode); returns live stats Leases don't have; reuses the exact discovery pattern `gawk-telemetry`'s scraper already proves (`gawk-telemetry/internal/relayscrape/scrape.go`). |
| D13 | **The `Ban` CRD Go types + ban-evaluation logic live in a new public package `gawk-server/moderation`**, imported by `gawk-admin` via a `replace` directive. | The relay and the admin service must agree byte-for-byte on normalization, target matching, and expiry semantics. Precedent: `wire` is public for exactly this reason (R14 Decision 1); `gawk-broadcast` already imports `gawk-server` via `replace` (`gawk-broadcast/go.mod:29`). |
| D14 | **The CRD manifest ships in the `gawk-server` chart** (templated, gated on `moderation.enabled`, annotated `helm.sh/resource-policy: keep`). | The relay is the enforcement owner and works without `gawk-admin` installed (file source, §4.14); the reverse is not true. `templates/`, not `crds/`, so chart upgrades update the schema; `keep` so uninstall never deletes live bans. |
| D15 | **Ban rejections answer HTTP `451` pre-upgrade** (Unavailable For Legal Reasons); `403` stays "resume token / secret rejected". | Pre-upgrade rejection cannot carry a close code, and browsers can't read the status of a failed WebTransport dial (docs/22 finding 12) — but the native broadcasters *can*, and 451 lets them say "banned" instead of "auth failed". A browser broadcaster sees a generic dial failure; accepted and documented. |
| D16 | **`gawk-admin` is horizontally scalable from v1**: `replicaCount` defaults to **2**, the API is stateless (D17), and singleton background work (reconciler/janitor, webhook dispatcher) runs under Kubernetes-Lease leader election. | HA of the moderation plane is a first-class requirement — it is a main reason the system of record is a shared database (D3) rather than pod-local state. Baking single-writer assumptions in now and unwinding them later would be the expensive order of operations. |
| D17 | **API authentication is the OIDC-issued JWT itself, and authorization is roles inside it** — `Authorization: Bearer` on every request, validated statelessly (JWKS signature, issuer, audience, expiry), then a required **role from a claim in the token** (Keycloak *client roles*: `operator` now, `flagger` reserved for R40). No hardcoded identity allowlists, no cookies, no server-side sessions, no CSRF machinery. The SPA keeps a **short-lived access token** valid via **refresh-token rotation**. | Stateless auth is what makes D16 trivial and deletes the cookie/CSRF class. Roles live in the IdP where access is managed and audited — no redeploy to grant or revoke an operator. Revocation is bounded by the access-token lifetime: removing the role (or revoking the refresh token) takes effect at the next refresh, which is why access tokens must be short (§4.8, §5). |
| D18 | **Database migrations are decoupled from the application**: versioned, forward-only SQL applied by a dedicated migrate step (a Helm hook Job) before rollout, under an expand–contract backward-compatibility policy — the previous app release MUST work against the migrated schema (§4.15). | Startup-embedded migrations race multi-replica rollouts (D16), couple schema change to pod restarts, and hide schema changes from review and ops. Backward compatibility is what makes rollback "redeploy the previous version" instead of "restore the database". |

---

## 3. Where it plugs into the existing code (grounded)

- **The publish path** — `gawk-server/internal/transport/server.go:504-700`
  (`handlePublish`). The publish-secret gate at `:513-521` is the model for
  the ban gate: pre-upgrade, constant-time where applicable, metric +
  logged rejection. The ID-ban check slots immediately after
  `broadcastid.Normalize` (`:530`), *before* `resume.verify` (`:541`); the
  IP-ban check covers both the claim path and the mint path (`:638`).
- **The fleet-wide teardown that already exists** — in cluster mode, Lease
  deletion propagates "broadcast ended" to every pod's informer
  (`internal/cluster/cluster.go:508-570` → `Server.HandleLeaseDeleted`,
  `server.go:493-500`). R39's kill reuses this for cluster bookkeeping but
  does **not** rely on it for the kill itself (§4.3) — pods act on the Ban CR
  directly, which also makes kills work with `-cluster-mode` off.
- **The guard that forces a new entry point** —
  `Registry.expireBroadcast` deliberately refuses to touch a hub with an
  active publisher (`internal/hub/hub.go:1849`), so a racing janitor can
  never kill a live broadcast. That guard stays; the admin kill gets an
  explicit `Registry.TerminateBroadcast` instead (§4.3).
- **The pod's map of live publisher sessions** — `Server.trackPublisher` /
  `s.publishers` (`server.go:386`); `HandleLeaseLost` (`server.go:408-431`)
  already closes a publisher with an arbitrary code. The kill closes the
  publisher through the same map with 4006.
- **What a kill must purge** — the prime caches
  (`Registry.InvalidatePrimes`, `hub.go:958-980`), the lazily-allocated DVR
  rings (`hub.go:679-682`; docs/26 D11 — a surviving ring replays banned
  content to DVR cursors), a pending grace timer (`hub.go:1902-1906`), and
  the counter fold (`hub.go:1858-1893`; docs/35 finding 3 — a missed fold
  makes a Prometheus counter go backwards).
- **The ops listener** — TCP `-metrics-addr` default `:2112`
  (`internal/ops/ops.go:39-55`), ClusterIP-only
  (`deploy/charts/gawk-server/templates/service-metrics.yaml`), plus a
  headless twin for per-pod scraping. The relay admin endpoints (§4.5) join
  it; the headless Service is how `gawk-admin` finds every pod.
- **The posture template** — `gawk-telemetry`'s embedded no-external-assets
  SPA (`gawk-telemetry/internal/dashboard/dashboard.go`), its two-Service
  split, and the chart-level `{{ fail }}` guard
  (`gawk-telemetry/deploy/charts/gawk-telemetry/templates/ingress.yaml:2-4`)
  that makes "exposed without auth" un-mis-configurable. `gawk-admin` copies
  the SPA embedding and the fail-guard shape (adapted to OIDC, §4.13).
- **HMAC'd broadcast keys** — `Registry.ObfuscateID` (`hub.go:1547-1552`,
  12 hex chars, keyed by `-stats-key`); the telemetry UI keys its broadcast
  view by the same value (`gawk-telemetry/internal/readapi/resolve.go:44-48`,
  route `#/broadcast/<key>`). The relay admin endpoint returns raw + HMAC'd
  side by side so the portal can deep-link without holding `statsKey`.
- **Wire mirrors** — highest close code today is 4005
  (`gawk-server/wire/wire.go:303`). Mirrors: `gawk-app/src/transport/wire.ts:124-153`
  (+ `wire.test.ts:191-200`), `gawk-broadcast-windows/crates/wire/src/lib.rs:98-110`
  (+ `tests/golden.rs`), `gawk-broadcast/internal/wirecheck`. Viewer terminal
  handling is two `=== CLOSE_CODE_BROADCAST_ENDED` checks
  (`gawk-app/src/transport/viewer-session.ts:200,262`) plus the retry policy
  in `reconnect.ts` — all must learn 4006 or viewers reconnect-loop against
  a kill.
- **Deep-link targets** — viewer join: `#/view/<ID>` on the app origin
  (docs/31); telemetry broadcast view: `#/broadcast/<12-hex key>` (docs/36).

---

## 4. Design

### 4.1 System overview

```
                    internet
                       │
             ┌─────────┴─────────┐
             │  Ingress (opt-in) │  OIDC is the boundary here (D7)
             └─────────┬─────────┘
                       │
 ┌─────────────────────┴──────────────────────┐
 │ gawk-admin (replicas ≥ 2, stateless API)   │
 │  portal SPA + /api/v1 (OIDC JWT) ────► Postgres (external) — system of record
 │  JWKS validation · role check          bans, events, webhooks, deliveries
 │  leader-elected: reconciler/janitor,   │
 │    webhook dispatcher (signed) ◄───────┘
 │      │ writes/deletes                       scrapes (Bearer token)
 │      ▼                                          │
 │  Ban CRs (gawk.ioio.fi/v1alpha1)               │
 └──────┼──────────────────────────────────────────┼──────┐
        │ watch (informer, per pod)                ▼      │
 ┌──────┴───────────────────────────────────────────────┐ │
 │ gawk-server pods (unchanged data plane)              │ │
 │  BanSet in memory → 451 on /publish, /publish/{id}   │◄┘
 │  Ban CR seen → TerminateBroadcast(4006) + purge      │   GET /internal/admin/*
 │  ops listener :2112 gains /internal/admin/*          │   (ops listener, ClusterIP)
 └──────────────────────────────────────────────────────┘
```

**The kill flow, end to end** (operator clicks *Kill*):

1. `POST /api/v1/broadcasts/{id}/kill` → `gawk-admin` inserts an active ban
   row (target `broadcastId`, `expires_at = now + cooldown`, reason, actor),
   emits a `broadcast.killed` event, fires the webhook.
2. The reconciler creates the corresponding `Ban` CR (same transaction
   boundary: row first, CR immediately after; the 60 s reconcile loop heals
   any crash between the two).
3. Every relay pod's informer delivers the CR. Each pod that has the
   broadcast: closes a live publisher session with **4006** (origin), closes
   every local subscriber — viewers, internal edge sessions, stripe legs —
   with **4006**, purges prime caches + DVR rings + grace timer, folds
   counters, and (origin, cluster mode) deletes the Lease. The later
   lease-deletion informer event finds no hub and no-ops.
4. The broadcaster's auto-resume hits `/publish/{id}` and gets **451**
   pre-upgrade — the ban check sits before token verification, so a valid
   resume token cannot resurrect a killed broadcast, on any pod, including
   pods that never had the hub.
5. After the cooldown expires, the janitor flips the row to `expired` and
   deletes the CR. (Relays also evaluate `expiresAt` themselves at check
   time, so enforcement ends on schedule even if the janitor is down.)

### 4.2 The `Ban` CRD

Group `gawk.ioio.fi`, version `v1alpha1`, kind `Ban`, **namespaced** (same
namespace as the relay pods). The informer watches **all** Bans in the
namespace — deliberately no label selector: every Ban is relevant by
definition, and a required label would turn a `kubectl`-applied break-glass
ban that forgot it into a silently unenforced one. One target per CR (clean
expiry: an expired ban is a deleted object, never a half-patched one).

```yaml
apiVersion: gawk.ioio.fi/v1alpha1
kind: Ban
metadata:
  name: ban-id-abc123            # deterministic, see below
spec:
  target:
    type: broadcastId            # "broadcastId" | "ip"
    value: "ABC123"              # normalized uppercase ID, or CIDR ("203.0.113.7/32", "2001:db8::/64")
  expiresAt: "2026-08-20T18:00:00Z"   # RFC 3339; ABSENT = permanent
  reason: "operator text"        # informational; shown in relay logs
  createdBy: "juho@example.com"  # informational (OIDC email or "system")
```

- **Names are deterministic** so a re-ban updates rather than duplicates:
  `ban-id-<lowercased broadcast ID>` (the ID charset is DNS-safe — the Lease
  scheme `gawk-bc-<lowercased id>` at `cluster.go:40` already relies on
  this); `ban-ip-<first 12 hex of SHA-256 of the canonical CIDR>` (CIDRs
  contain `:`, `/`, `.` — hash instead of munging).
- **OpenAPI validation** in the CRD schema: `type` enum; `value` non-empty,
  max 64 chars; `expiresAt` `format: date-time`; `reason`/`createdBy` max
  512/256 chars. Printer columns: Type, Value, Expires, Age — so
  `kubectl get bans` is a usable emergency surface on its own (an operator
  with cluster access can `kubectl apply` a Ban with `gawk-admin` down, and
  the reconciler will refuse to garbage-collect CRs it has no row for only
  when it cannot reach Postgres; when it can, it adopts unknown CRs into
  Postgres as `createdBy: kubectl` rows rather than deleting them).
- **No status subresource** in v1alpha1 — relays only read.
- **`v1alpha1` `BanSpec` is additive-only** (the D18 expand–contract shape,
  written down before the first divergence instead of after): a new spec field
  must be optional with an absent-means-previous-behaviour default, and a new
  `target.type` value means a new CRD *version*, never a widened enum. The
  rule exists because the schema is owned by the **relay** chart while the
  writer is `gawk-admin`, and the two deployables routinely run different
  releases: a structural CRD silently **prunes** a spec field an older
  installed schema does not know, and enum-rejects a new `target.type` —
  fail-closed, surfacing as a permanent `202`/`enforcement: pending` with a
  reconciler Warn every minute. The upgrade ordering this implies — relay
  chart (the schema) before `gawk-admin` (the writer) — is documented in
  `docs/self-hosting.md` §9; the release-coupling rule for the shared Go
  packages is in `CONTRIBUTING.md`.

**The `gawk-server/moderation` package (D13)** is the single source of truth
for semantics, used by both sides:

```go
package moderation

type TargetType string // "broadcastId" | "ip"
type Target struct { Type TargetType; Value string }
type Record struct {
    Target    Target
    ExpiresAt *time.Time // nil = permanent
    Reason    string
    CreatedBy string
}
// CRName returns the deterministic CR name for a target.
func CRName(t Target) (string, error)
// Normalize canonicalizes a record: uppercases broadcast IDs via the same
// rules as broadcastid.Normalize, parses/canonicalizes CIDRs (a bare IP
// becomes /32 or /128), rejects malformed targets.
func Normalize(r Record) (Record, error)
// Set is the in-memory, mutex-guarded evaluation structure:
// a map for ID bans, a longest-prefix-match trie for CIDR bans.
type Set struct{ /* map[string]Record; LPM routing table over netip.Prefix */ }
func (s *Set) Replace(all []Record)          // informer resync / file reload
func (s *Set) Upsert(r Record) / Remove(t Target)
// Banned evaluates lazily against now — an expired record never matches,
// regardless of whether the janitor has deleted its CR yet.
func (s *Set) BannedID(normID string, now time.Time) (Record, bool)
func (s *Set) BannedIP(ip netip.Addr, now time.Time) (Record, bool)
```

CIDR matching is a proper **longest-prefix-match structure** (a binary radix
trie over address bits — e.g. `github.com/gaissmai/bart`, or a hand-rolled
patricia trie if a dependency is unwelcome), not a linear scan: this sits on
the publish hot path of a foundational system, ban lists grow, and prefixes
are genuinely mixed — v4 and v6, `/8` through `/32` and `/128`, overlapping
allowed (any match ⇒ banned; the most specific match's record is the one
reported/logged). ID bans stay an O(1) map. `Replace`/`Upsert`/`Remove`
rebuild or patch the trie under the mutex; reads are lock-cheap. The package
also carries the CRD Go structs + scheme registration (`AddToScheme`) used
by both the relay informer and `gawk-admin`'s client.

### 4.3 Relay-side enforcement and actuation (`gawk-server`)

**New config** (flag > env > default, per `config.go:3-5`; startup log line
added at `main.go:64-96` — the operator's confirmation surface):

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-moderation-source` | `GAWK_MODERATION_SOURCE` | `off` | `off` \| `k8s` \| `file:<path>`. `k8s`: informer on Ban CRs in `POD_NAMESPACE` (in-cluster config; independent of `-cluster-mode`). `file`: JSON array of records, reloaded on a **stat poll** (mtime+size) and on SIGHUP — the dev/compose lane (§4.14). *(Implementation deviation, §11.1: stat-polling rather than fsnotify, following `internal/tlsutil/reload.go` — no new dependency.)* |
| `-admin-api-token` | `GAWK_ADMIN_API_TOKEN` | `""` | Static bearer token for `/internal/admin/*` on the ops listener (the machine credential `gawk-admin` uses). |
| `-admin-oidc-issuer` / `-admin-oidc-audience` | `GAWK_ADMIN_OIDC_ISSUER` / `GAWK_ADMIN_OIDC_AUDIENCE` | `""` | Alternative credential for the same routes: accept OIDC JWTs with this issuer + audience, verified offline against a cached JWKS (§4.5). Both set or both empty. **When neither the token nor the issuer is configured, the routes return 404** — the surface stays dark, not merely locked. |
| `-admin-oidc-roles-claim` / `-admin-oidc-role` | `GAWK_ADMIN_OIDC_ROLES_CLAIM` / `GAWK_ADMIN_OIDC_ROLE` | see §4.5 | Roles-claim dot-path (default `resource_access.{audience}.roles`) and the role a JWT must carry (default `operator`). |

Neither knob is a `hub.Options` field, so `registryOptions`
(`main.go:183-205`) is untouched — but the **carry-all-limits discipline
still applies**: both knobs get config-parse tests and appear in the startup
log, and any *hub-level* knob a later chunk adds must cross
`registryOptions` (the R2 lesson, verbatim).

**Enforcement** in `handlePublish`:

```
513  publish-secret check              (unchanged, 401)
     + IP ban check                    → 451, outcome "banned"     ← both mint & claim
530  broadcastid.Normalize             (claim path)
     + ID ban check                    → 451, outcome "banned"     ← BEFORE resume.verify
541  resume.verify                     (unchanged, 403)
```

Rejections are pre-upgrade (D15), counted via the existing
`metrics.Connection("publish", ...)` with a new `OutcomeBanned`, and logged
with remote + target type (never the ban reason at Warn — reasons may hold
operator-private context; log reason at Debug). The publisher's remote IP
comes from `r.RemoteAddr`; `Server` additionally records each live
publisher's `netip.Addr` in its `trackPublisher` bookkeeping so IP bans can
kill live sessions.

**Actuation** — `Server` subscribes to BanSet changes:

- `HandleBanAdded(rec)` with target `broadcastId`: if `s.publishers[normID]`
  exists, close that session with 4006 + reason `"terminated by operator"`;
  then `Registry.TerminateBroadcast(normID, wire.CloseCodeTerminatedByOperator, reason)`;
  then, if this pod holds the origin Lease, `cluster.Delete` (the existing
  `OnBroadcastExpired` hook already does this — TerminateBroadcast fires it
  for origin hubs).
- Target `ip`: iterate tracked publishers, kill every broadcast whose
  publisher remote IP matches the CIDR (same path as above, per broadcast).
- **`Registry.TerminateBroadcast(id, code, reason)`** — new exported method:
  a parameterized force-expire that (a) ignores the `publisherActive` guard
  (`hub.go:1849` stays intact for the GC path), (b) closes **all**
  subscribers — viewers, `s.internal` edge sessions, stripe legs — with the
  given code instead of hardcoded 4000 (`hub.go:1918-1921` becomes
  parameterized), (c) purges prime caches, DVR rings, grace timer, and
  (d) folds every counter (reuse the `expireBroadcast` fold body — do not
  duplicate it; docs/35 finding 3). Idempotent: terminating an absent hub
  is a no-op returning false.
- Edge pods run the same handler on their own informer event: stop the edge
  pull (`EdgeManager.StopEdge`), terminate the local hub with 4006. The
  subsequent Lease-deletion event finds nothing and no-ops
  (`HandleLeaseDeleted` → `EndBroadcast` already tolerates absent hubs).

**New metrics**: `gawk_moderation_bans_active` (gauge, by target type),
`gawk_moderation_terminations_total` (counter), plus the `banned` outcome on
the existing connection counter.

### 4.4 Close code 4006

```go
// CloseCodeTerminatedByOperator (4006) is sent when the operator (or, later,
// the R40 auto-tier) terminates a broadcast: to the publisher session being
// killed, to every viewer of that broadcast, and to internal edge sessions
// so downstream pods tear down too. TERMINAL in every role: a viewer must
// not reconnect, a publisher must not auto-resume — the ID is banned for at
// least the kill cooldown, so a resume attempt would only burn the 451
// rejection budget. Allocated 2026-08-20 (R39, docs/42).
CloseCodeTerminatedByOperator = 4006
```

Mirror pass (one allocation, one PR, per the CLAUDE.md convention): `wire.ts`
constant + test, Rust `crates/wire` constant + golden vector, `wirecheck`
assertion. Client behavior:

- **Viewer** (`gawk-app`): `viewer-session.ts:200,262` change from
  `=== CLOSE_CODE_BROADCAST_ENDED` to membership in a terminal-code set
  `{4000, 4006}`; `reconnect.ts` treats 4006 as no-retry; the UI shows
  "This broadcast was ended by a moderator" (distinct from "broadcast
  ended").
- **Broadcasters** (app, `gawk-broadcast`, `gawk-broadcast-windows`): 4006 is
  terminal — stop capture/encode session teardown as on 4004, show
  "terminated by the server operator", never auto-resume. Additionally all
  three treat HTTP **451** on a publish/reclaim dial as terminal-with-message
  where the status is readable (both natives; the browser sees a generic
  dial failure — accepted, D15) and cap auto-resume retries on repeated 403/451.

### 4.5 The relay admin API (ops listener)

Two read-only routes join `internal/ops` (TCP `:2112`, ClusterIP-only), live
only when at least one credential is configured (404 otherwise).
`Authorization: Bearer <credential>` accepts **either**: the static
`-admin-api-token` (constant-time compare — the machine path `gawk-admin`
uses), **or** an OIDC JWT validated against `-admin-oidc-issuer` /
`-admin-oidc-audience` — signature via a cached, background-refreshed JWKS
(verification is offline per request; the relay never blocks on the IdP),
plus `iss`/`aud`/`exp`/`nbf`, plus the **required role** from the token's
roles claim (`-admin-oidc-roles-claim`, defaulting to the Keycloak
client-roles path `resource_access.{audience}.roles`; `-admin-oidc-role`,
default `operator` — the same role model as §4.8). The JWT path lets an
operator (or a JWT-holding tool) hit these endpoints directly with the same
identity the portal uses, and lets the IdP — not a shared string — govern
who may read raw IDs. 401 on any invalid credential, 403 on a valid token
without the role:

**`GET /internal/admin/broadcasts`**

```json
{
  "schema": "gawk.admin.broadcasts.v1",
  "pod": "gawk-server-7d4b...",
  "broadcasts": [{
    "id": "ABC123",
    "key": "3f9a1c2b4d5e",
    "role": "origin",
    "publisherActive": true,
    "publisherRemoteIp": "203.0.113.7",
    "publisherSessionId": "…",
    "startedAt": "2026-08-20T14:00:11Z",
    "viewersLocal": 12,
    "viewersGlobal": 340,
    "graceRemainingSeconds": 0,
    "dvrBytes": 8388608
  }]
}
```

`id` is the **raw** broadcast ID — the deliberate, scoped relaxation of the
invariant (D8): this listener is ClusterIP-only *and* credential-gated, strictly
more protected than today's ops surface. `key` is `Registry.ObfuscateID(id)`
so the portal deep-links into telemetry without ever holding `statsKey`.
Implementation: a new `Registry.AdminStats()` walking `r.hubs` — the
existing `Stats()` and `/statusz` stay HMAC-only and byte-identical to today.

**`GET /internal/admin/config`**

```json
{ "schema": "gawk.admin.config.v1", "pod": "…", "version": "1.42.0",
  "config": { "addr": ":4433", "clusterMode": true, "publishSecret": "<set>",
              "resumeTokenKey": "<set:explicit>", "maxBroadcasts": 200, … } }
```

Produced by a new `config.Sanitized()` that enumerates fields **explicitly**
(no reflection over strings): every secret-bearing field renders as
`"<set>"`/`"<unset>"` (the resume key also says which mode, echoing the
startup log). A unit test feeds a config full of sentinel secret values and
asserts none appear in the output — that test is the acceptance gate.

Kill/ban are **not** exposed here: the Ban CR is the single write path into
enforcement (D2); a second one would eventually disagree with it.

### 4.6 The `gawk-admin` service

Fourth top-level Go module. Layout mirrors `gawk-telemetry`:

```
gawk-admin/
  cmd/gawk-admin/main.go        # config, listeners, lifecycle; `migrate` subcommand (§4.15)
  migrations/                   # versioned forward-only SQL, applied by the migrate step — never at app startup
  internal/store/               # Postgres queries + schema-version compatibility check
  internal/kube/                # Ban CR client + reconciler/janitor (leader-only) + leader election
  internal/relayscan/           # headless-DNS pod discovery + /internal/admin/* scrape
  internal/auth/                # JWT validation (cached JWKS), role-claim authorization
  internal/api/                 # /api/v1 handlers
  internal/notify/              # webhook signer + retry queue (dispatcher runs on the leader)
  internal/portal/              # go:embed dist (telemetry pattern incl. notBuilt page)
  ui/                           # Vite + React + TS + CSS modules SPA
  deploy/                       # Dockerfile + chart
  go.mod                        # replace github.com/Tuhis/gawk/gawk-server => ../gawk-server
```

**One HTTP listener** (`-addr`, default `:8090`): the SPA, `/api/v1`,
`GET /auth/config` (unauthenticated SPA bootstrap, §4.8), `GET /healthz`,
and `GET /readyz` (readyz = Postgres reachable + schema version compatible).

**HA from day one (D16).** The API is stateless — JWT auth (D17), every
replica serves reads and writes against Postgres — so `replicaCount`
defaults to **2**. Singleton background work (the reconciler/janitor and the
webhook dispatcher) runs only on the **leader**, elected via a Kubernetes
Lease (`k8s.io/client-go/tools/leaderelection`, 15 s-class TTL); non-leaders
serve API traffic and stand by. Nothing in the design assumes a single
writer: CR names are deterministic, ban writes are guarded by the
one-active-per-target unique index, and webhook deliveries are claimed with
`FOR UPDATE SKIP LOCKED` so even a leadership handover cannot double-send.

**Postgres schema** (versioned SQL in `migrations/`, applied by the separate
migrate step — §4.15 — never by the serving process):

```sql
CREATE TABLE bans (
  id           uuid PRIMARY KEY,
  target_type  text NOT NULL CHECK (target_type IN ('broadcastId','ip')),
  target_value text NOT NULL,          -- normalized via moderation.Normalize
  state        text NOT NULL CHECK (state IN ('active','expired','removed')),
  reason       text NOT NULL DEFAULT '',
  created_at   timestamptz NOT NULL,
  created_by   text NOT NULL,
  expires_at   timestamptz,            -- NULL = permanent
  removed_at   timestamptz,
  removed_by   text,
  source_broadcast_id text,            -- raw ID the action was taken against (IP bans too)
  cr_name      text NOT NULL
);
CREATE UNIQUE INDEX bans_one_active_per_target
  ON bans (target_type, target_value) WHERE state = 'active';

CREATE TABLE moderation_events (
  id            bigserial PRIMARY KEY,
  type          text NOT NULL,          -- broadcast.killed | ban.created | ban.expired | ban.removed  (content_flag.raised reserved for R40)
  occurred_at   timestamptz NOT NULL,
  actor         text NOT NULL,          -- OIDC email, "system", or (R40) a service-identity subject
  broadcast_key text,                   -- HMAC'd, safe to export
  broadcast_id  text,                   -- raw; portal-only, never in webhooks
  payload       jsonb NOT NULL DEFAULT '{}'
);

CREATE TABLE webhooks (                  -- UI-created only; chart-defined ones come from config (§4.10)
  id         uuid PRIMARY KEY,
  name       text NOT NULL UNIQUE,       -- unique across config-sourced names too (enforced in code)
  url        text NOT NULL,
  secret     text NOT NULL,              -- HMAC signing key; never returned by the API
  enabled    boolean NOT NULL DEFAULT true,
  created_at timestamptz NOT NULL,
  created_by text NOT NULL
);

CREATE TABLE webhook_deliveries (
  id           bigserial PRIMARY KEY,
  event_id     bigint NOT NULL REFERENCES moderation_events(id),
  webhook_name text NOT NULL,            -- works for config- and UI-sourced webhooks alike
  state        text NOT NULL CHECK (state IN ('pending','delivered','failed')),
  attempts     int  NOT NULL DEFAULT 0,
  next_attempt_at timestamptz,
  last_error   text,
  delivered_at timestamptz
);
```

(R40 will add a `content_flags` table in its own migration; the events table
already accommodates its event type.)

**Reconciler / janitor** (one loop, 60 s, plus immediate runs after every
mutation; runs on the leader only): active rows ⇄ Ban CRs. Missing CR → create. Unknown CR → adopt
into Postgres (`created_by: kubectl`) — never silently delete an
operator-applied emergency ban. `expires_at ≤ now` → `state = expired`,
delete CR, emit `ban.expired`. All CR writes go through
`moderation.Normalize`/`CRName` so both sides always agree.

### 4.7 Portal HTTP API

All under `/api/v1`, JSON, authenticated by the OIDC JWT on every request
(§4.8) — no cookies exist, so there is no CSRF machinery to get wrong.
Errors are `{ "error": { "code": "string", "message": "human text" } }`.

A ban is **two writes in two systems that cannot share a transaction**: the
Postgres row (the record) and the `Ban` CR (the object relays watch, and the
only thing that actually enforces). Every mutation therefore has three
outcomes, not two, and grades itself on which of the two writes landed:

| Outcome | Answer |
|---|---|
| row written **and** CR projected | `201 Created` (kill, create ban) · `204 No Content` (unban) |
| row written, CR projection failed | `202 Accepted`, body = the ban, carrying `enforcement: {inSync: false, detail}` |
| row write failed | `503` when Postgres is unreachable, `500` otherwise — nothing is recorded, no CR is written, no event is emitted, no ban comes back |

`202` is a **success**. The record is durable and the reconciler — precisely
RFC 9110 §15.3.3's "another process or server" — closes the gap on its next
sweep. The body carries the ban because a client that just created something
needs its ID regardless, and because the one thing it must not do is
re-submit: a retry now `409`s against the row that does exist. `enforcement`
rides only on that response — list and read routes never carry it, because a
stored row has no in-flight projection to report on. It is `inSync` and not
`enforced` because it has to read correctly in **both** directions: a pending
create is recorded and *not yet enforced*, while a pending unban is lifted in
the record and the target is *still banned*.

| Route | Semantics |
|---|---|
| `GET /api/v1/me` | `{email, subject, roles}` from the validated JWT — the SPA's authorization probe (valid token without the `operator` role → 403; missing/expired token → 401 → the SPA refreshes or re-runs the OIDC flow). |
| `GET /api/v1/broadcasts` | Fleet aggregation from `relayscan` (≤2 s cache): per broadcast `{id, key, publisherActive, publisherRemoteIp, startedAt, viewersGlobal, pods: [{pod, role, viewersLocal}], links: {watch, telemetry}, banState}`. `watch` = `<appBaseUrl>/#/view/<id>`; `telemetry` = `<telemetryBaseUrl>/#/broadcast/<key>` (omitted when the base URL is unconfigured). |
| `POST /api/v1/broadcasts/{id}/kill` | Body `{reason (required, non-empty), cooldownSeconds?}` (default from `-kill-cooldown`, 600). Creates the cooldown ban, emits `broadcast.killed`, webhook. `201 {ban}`, or `202 {ban}` in the same envelope when the row committed and the CR did not. Idempotent-ish: an existing active ID ban → `409` with that ban. |
| `POST /api/v1/bans` | Body `{target: {type, value}, expiresAt: RFC3339 \| null, reason (required), sourceBroadcastId?}`. `value` for `type: "ip"` may be the literal `"publisher"` together with `sourceBroadcastId` — the server resolves the live publisher's IP via relayscan and applies the operator-confirmed prefix (§4.9). `201` with the bare ban, or `202` with the bare ban when only the CR write failed; duplicate active target → `409`. |
| `GET /api/v1/bans?state=active\|all` | Ban rows, newest first. |
| `DELETE /api/v1/bans/{id}` | Unban: `state = removed`, CR deleted, `ban.removed` event. `204` with no body when both landed. `202` **with the removed ban** when the row moved and the CR delete did not — the one case where the record reads `removed` while the target is still banned, so it answers with something to say rather than an empty success. |
| `GET /api/v1/events?afterId=&limit=` | The audit/notification feed (cursor pagination by `id`). |
| `GET /api/v1/relays` | Per-pod `{pod, reachable, version, config}` from `/internal/admin/config` — the read-only settings view (D10). |
| `GET /api/v1/webhooks` | Merged list, chart-defined + UI-created: `{id?, name, url, enabled, source: "config" \| "ui"}`. **Secrets are never returned**, for either source. |
| `POST /api/v1/webhooks` · `PUT /api/v1/webhooks/{id}` · `DELETE /api/v1/webhooks/{id}` | UI-created webhooks only (`{name, url, secret, enabled}`; names unique across both sources). Any write addressing a config-sourced webhook → `409 source_immutable` (D9). |
| `POST /api/v1/webhooks/{name}/test` | Sends a synthetic signed `test` event to that webhook (works for both sources); returns the delivery outcome. |
| *(R40, reserved — not implemented in R39)* `POST /api/v1/content-flags` | §4.11. Returns `404` in R39 (the path is documented, never squatted by anything else). |

### 4.8 OIDC and API authentication

No cookies and no server-side sessions (D17). Any spec-compliant provider
works — the issuer is a knob (Keycloak/Authelia/authentik/Google).

- **The SPA is an OIDC public client**: authorization-code flow with PKCE,
  `state` and `nonce` — **no client secret exists anywhere in the system**.
  It bootstraps from unauthenticated `GET /auth/config` →
  `{issuer, clientId, audience}`, runs a redirect flow **hand-rolled against
  WebCrypto** (`ui/src/auth/` — no OIDC client library; §11.1 records why),
  and keeps tokens **in memory only** (never `localStorage`).
- **Short-lived access tokens, kept alive by refresh tokens.** The SPA
  uses ordinary session-bound refresh tokens (not `offline_access` ones),
  with rotation enabled at the IdP for public clients, and silently renews
  the access token before expiry; a failed renewal falls back to the full
  redirect flow. Access
  tokens are meant to be **short** — 5–15 minutes recommended in
  self-hosting — because token lifetime bounds revocation (below). Refresh
  tokens also live in memory only; a page reload therefore re-runs the
  redirect flow, which against a live IdP session is an invisible bounce.
- **Every `/api/v1` request carries `Authorization: Bearer <JWT>`.** The
  backend (`github.com/coreos/go-oidc/v3`) validates statelessly:
  signature against the issuer's JWKS (cached, background-refreshed —
  per-request verification is offline), `iss`, `aud` (`-oidc-audience`),
  `exp`/`nbf`.
- **Authorization is roles carried in the token, managed in the IdP** —
  never a config-file identity list. The backend reads a string-array claim
  at the dot-path `-oidc-roles-claim`, whose default is the **Keycloak
  client-roles** shape `resource_access.{audience}.roles` (`{audience}`
  substituted from `-oidc-audience` into each segment, which is what lets an
  audience containing a dot work at all; override the path for other IdPs).
  Every R39 route requires the **`operator`** role (`-operator-role`,
  default `operator`); **`flagger`** is reserved for R40's service identity
  (§4.11). A valid token without the required role → 403. Granting or
  revoking an operator is an IdP action — assign or remove the client
  role — and takes effect at the next access-token refresh; revoking the
  refresh token (or the IdP session) ends access at the same horizon.
  There is no "any authenticated user" mode: a blanked roles-claim path or
  empty role name refuses to boot.
- **Logout** is client-side (drop the in-memory tokens), optionally
  redirecting to the IdP's end-session endpoint.
- **No cookies ⇒ no CSRF surface.** The residual browser risk is XSS
  against in-memory tokens; the mitigations are the strict CSP and the
  embedded-assets rule. CSP: `default-src 'self'; connect-src 'self'
  <issuer origin>` — the IdP is the *single* sanctioned external origin
  (assets stay embedded and tested; the issuer origin is injected into the
  header at serve time).
- **Headers on every response**: the CSP above, `frame-ancestors 'none'`,
  `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`;
  `Set-Cookie` never appears (asserted in tests). HSTS is the Ingress's
  job. Responses to invalid-credential requests are rate-limited per IP
  (brute-force damping on the only internet-facing surface).

### 4.9 Portal UI

Vite + React + TS + CSS modules, embedded via `go:embed` with telemetry's
exact mechanics: committed `dist/README.md` keeps `go build ./...` working
without npm, a `notBuilt` fallback page, `Cache-Control: no-store`, SPA
fallback routing, and the no-external-assets test.

Views (hash-routed, telemetry's tiny router pattern):

- **Broadcasts** (default): live table, 5 s auto-refresh — raw ID (with
  copy), viewers, uptime, publisher IP, pod placement, per-row **Watch**
  (viewer deep link) and **Telemetry** links, and the **Kill** / **Kill +
  ban** actions. Kill dialog: required reason, cooldown preset (default
  10 min). Kill+ban dialog: duration presets (1 h / 24 h / 7 d / permanent),
  optional "also ban publisher IP" with the resolved IP shown and prefix
  choice (v4 `/32` default; v6 defaults to `/64` — privacy-address rotation
  makes `/128` near-useless). A row slot for R40's flagged-pin exists in the
  design (pinned rows sort first, red badge) but renders nothing in R39.
- **Bans**: active + history, target, expiry countdown, reason, actor,
  Unban.
- **Events**: the audit feed, newest first.
- **Relays**: per-pod reachability + the sanitized config table (D10).
- **Webhooks**: list/add/edit/delete/disable UI-created webhooks (§4.10);
  chart-defined ones render with a lock and a `from config` badge — visible,
  testable ("send test event" works for both sources), never editable here.

**IP-ban honesty in the UI**: the ban dialog shows a warning whenever the
publisher IPs of >50 % of live broadcasts are identical — the tell that the
LB is not preserving client IPs (`externalTrafficPolicy` must be `Local`;
`docs/self-hosting.md` gains the note) and an IP ban would take out everyone.
The dialog also always states the NAT-collateral caveat in one sentence.

### 4.10 Webhook notifications

Webhooks come from two places (D9): **chart values** —
`notifications.webhooks: [{name, url, secretRef}]`, rendered into the
`-static-webhooks` knob with secrets staying in k8s Secrets, visible but
immutable in the UI — and **the portal** (the `webhooks` table, full CRUD).
Names are unique across both. Every `moderation_events` insert enqueues one
delivery **per enabled webhook**; the dispatcher runs on the leader and
claims delivery rows with `FOR UPDATE SKIP LOCKED` (a leadership handover
cannot double-send). Zero configured webhooks is fine — events always land
in the portal feed.

```
POST <webhookUrl>
Content-Type: application/json
X-Gawk-Event: broadcast.killed
X-Gawk-Delivery: <uuid>
X-Gawk-Timestamp: <unix seconds>
X-Gawk-Signature: sha256=<hex HMAC-SHA256(that webhook's secret, timestamp + "." + body)>

{ "schema": "gawk.moderation-event.v1",
  "type": "broadcast.killed",
  "occurredAt": "2026-08-20T15:04:05Z",
  "actor": "juho@example.com",
  "broadcastKey": "3f9a1c2b4d5e",
  "reason": "terms violation",
  "portalUrl": "https://admin.example.com/#/broadcasts?key=3f9a1c2b4d5e" }
```

An event that names a broadcast appends `?key=<broadcastKey>` to `portalUrl` —
still the HMAC'd key, never the raw ID (D8) — which the portal reads as a
pre-filled filter, so the paged operator lands on the offending row instead of
matching a 12-hex key against a fleet-sized table by eye.

…and the same kill recorded when its `Ban` CR write did not land — §4.7's
`202 Accepted`, delivered rather than answered:

```
{ "schema": "gawk.moderation-event.v1",
  "type": "broadcast.killed",
  "occurredAt": "2026-08-20T15:04:05Z",
  "actor": "juho@example.com",
  "broadcastKey": "3f9a1c2b4d5e",
  "reason": "terms violation",
  "portalUrl": "https://admin.example.com/#/broadcasts?key=3f9a1c2b4d5e",
  "summary": "a kill of broadcast 3f9a1c2b4d5e was recorded by juho@example.com — NOT enforced yet, the broadcast is still live",
  "enforcement": "pending" }
```

- **No raw broadcast ID and no IP address ever appears in a payload** (D8) —
  the receiver gets the HMAC'd key and a portal link; acting requires
  logging in.
- Receivers verify the signature and should reject `|now - timestamp| > 300 s`
  (documented in self-hosting).
- Retry: attempts at +5 s, +30 s, +2 m, +10 m, then `state = failed`.
  Delivery state is visible in the portal events view — a failed delivery
  must be *seen*, because R40's DSA posture ("a flag must reach a human")
  inherits this pipe.
- Plain-text-friendly receivers (ntfy): the payload also carries a
  `"summary"` field — one human sentence — so a dumb webhook-to-push bridge
  needs no templating. It is graded on enforcement like the rest of the body:
  a pending kill says a kill was *recorded* rather than that a broadcast was
  terminated, and a pending unban says the target is **still banned**.
  `store.Summarize` is the single source of that sentence, in both grades.
- **`"enforcement": "pending"` when the record is ahead of what the relays
  enforce.** An event is a statement of something that happened, and a ban is
  two writes in two systems (§4.7): when the row landed and the `Ban` CR did
  not, the delivery says so instead of announcing an enforcement that has not
  started. The field is **absent** whenever the two agree — which is why
  adding it does not rev `schema`: a receiver that never looks for it keeps
  parsing byte-identical bodies. Its vocabulary is closed (`pending` is the
  only value it can ever hold), so unlike `reason` and `summary` it carries no
  operator- or producer-supplied text, and D8 stays structural rather than
  becoming a per-value review.

### 4.11 Content-flag extension point (R40 — designed now, built in R40)

Frozen contract, named **content flags** throughout (D11):

- **Route**: `POST /api/v1/content-flags`.
- **Auth**: a valid JWT from the same OIDC provider, validated exactly as
  every other API request (§4.8). R40's sampler obtains its JWT via the
  **client-credentials grant** — a service identity the IdP manages, no
  gawk-minted tokens — carrying the **`flagger`** client role
  (`-flagger-role`), which grants *flag-only* rights. In R39 nothing is
  authorized to flag, and kill/ban rights remain bound to the `operator`
  role — never to a merely-valid token.
- **Body** (`schema: "gawk.content-flag.v1"`):

```json
{ "schema": "gawk.content-flag.v1",
  "broadcastId": "ABC123",
  "samplerId": "gawk-sampler-0",
  "window": { "start": "…", "end": "…", "samples": 12, "flaggedSamples": 9 },
  "scores": { "max": 0.97, "mean": 0.81 },
  "labels": ["…classifier labels…"],
  "summary": "sustained NSFW signal on ABC123-key" }
```

- **Effect** (R40 implements): insert a `content_flags` row, emit
  `content_flag.raised` (webhook fires — this *is* the operator page), pin
  the broadcast row in the portal. Flag events carry scores and metadata
  only — **never frames** (R40's evidence rule).
- R39's deliverables that R40 consumes, by name: `POST /broadcasts/{id}/kill`
  and `POST /bans` (the actuators), `moderation_events` + webhook (the
  notification surface), close code 4006, and this schema.

### 4.12 `gawk-admin` config knobs

Flag > `GAWK_ADMIN_*` env > default, telemetry-style; every knob lands in
the chart's values and the startup log.

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `:8090` | The single HTTP listener. |
| `-external-url` | *(required)* | Portal base URL — the OIDC redirect base and the `portalUrl` in webhook payloads. |
| `-oidc-issuer` / `-oidc-client-id` / `-oidc-audience` | *(required)* | The OIDC provider, the SPA's public-client ID, and the audience every accepted JWT must carry. **No client secret exists** — the SPA is a public client with PKCE (§4.8). |
| `-oidc-roles-claim` | `resource_access.{audience}.roles` | Dot-path to the roles array in the JWT — the default is the Keycloak client-roles shape, `{audience}` substituted into each segment; override for other IdPs. Blank ⇒ refuse to start. The placeholder is `{audience}`, shared with the relay's twin knob (§4.5) so one string means one thing. |
| `-operator-role` | `operator` | The role every R39 route requires. Blank ⇒ refuse to start. |
| `-pg-dsn` | *(required)* | Postgres DSN (chart wires it from `postgres.dsn` or `postgres.dsnSecretRef` — e.g. CNPG's `<cluster>-app`/`uri`). |
| `-relay-scan-target` | *(required)* | DNS name of the relay headless metrics Service (A-records → pods; the `relayscrape` discovery pattern). |
| `-relay-ops-port` | `2112` | Ops listener port on relay pods. |
| `-relay-admin-token` | *(required)* | Bearer for `/internal/admin/*` (must equal the relay's `-admin-api-token`). |
| `-namespace` | `POD_NAMESPACE` | Where Ban CRs live. |
| `-kill-cooldown` | `10m` | Default plain-kill ID-ban duration (D5). |
| `-static-webhooks` | `""` | JSON list `[{"name","url","secretEnv"}]` rendered from `notifications.webhooks` chart values — `secretEnv` names an env var wired from a k8s Secret, so signing keys never sit in the values file. Immutable from the UI (D9, §4.10). |
| `-app-base-url` | `""` | Watch deep links (empty hides them). |
| `-telemetry-base-url` | `""` | Telemetry deep links (empty hides them). |
| `-flagger-role` | `flagger` | Reserved for R40: the role granting *flag-only* rights to client-credentials service identities (§4.11). Unused by any route in R39. |

### 4.13 Helm & deployment footprint

**`gawk-admin` chart** (new; `ghcr.io/tuhis/gawk-admin`,
`oci://ghcr.io/tuhis/charts/gawk-admin`; chart == appVersion == image tag;
one more release-please manifest entry — the combined-release-PR model is
unchanged):

- Deployment (`replicaCount` default **2**, D16) + a PodDisruptionBudget
  (`minAvailable: 1`), ClusterIP Service `:8090`, optional Ingress.
- **Migrations run as a Helm `pre-install`/`pre-upgrade` hook Job** — same
  image, `gawk-admin migrate` subcommand,
  `helm.sh/hook-delete-policy: before-hook-creation` — so the schema is
  migrated to completion before the new Deployment rolls (§4.15).
- **The fail-guard, adapted** (the telemetry `{{ fail }}` pattern): Ingress
  enabled ⇒ `oidc.issuer`, `oidc.clientId` and `oidc.audience` must all be
  set (the role knobs have safe defaults), else `{{ fail }}` with a message
  naming this doc. (Auth is enforced in-process regardless — the guard just
  makes the chart refuse to *route* an unbootable config.)
- **The database is a prerequisite, not part of the release.** The chart
  renders no database object at all: it takes a *connection*, in the same
  house `foo` / `fooRef` dual form as every other secret here —
  `postgres.dsn` (a literal DSN) or `postgres.dsnSecretRef: {name, key}` (a
  Secret holding one; the Ref wins if both are set). With neither, the chart
  `{{ fail }}`s naming both forms. Decoupling the stateful store from the
  stateless service is the point: Postgres outlives every release of
  `gawk-admin`, `helm uninstall` must never be able to take the audit trail
  with it, and a `pre-install` hook may depend on a prerequisite where it
  could never depend on the release's own manifests (§4.15, §11.1).
  **CloudNativePG is then not a special case** — CNPG publishes
  `<cluster>-app` for the application user it creates, and that Secret's
  `uri` key is exactly this DSN, so
  `postgres.dsnSecretRef: {name: <cluster>-app, key: uri}` is the whole
  integration. The operator *and* the `Cluster` are the self-hoster's to
  apply, once, outside the release; the self-hosting doc carries the operator
  install command and a copy-pasteable two-instance `Cluster` manifest (HA of
  the moderation plane is a stated reason for the dedicated DB — D16).
- RBAC: SA + namespaced Role on `bans.gawk.ioio.fi`
  (`get,list,watch,create,update,delete`) **plus
  `coordination.k8s.io/leases` (`get,create,update`)** for leader election,
  + binding.
- Secrets follow the house `foo` / `fooRef: {name, key}` dual pattern
  (`gawk-server/deploy/.../deployment.yaml:117-123` precedent) for the
  relay admin token and each static webhook's signing secret.

**`gawk-server` chart additions**, all gated on `moderation.enabled`
(default `false`):

- The Ban **CRD** template with `helm.sh/resource-policy: keep` (D14).
- Role/binding extension: `bans.gawk.ioio.fi` `get,list,watch` (read-only —
  the relay never writes bans). Rendered when moderation is on even with
  `clusterMode` off; `POD_NAMESPACE` downward-API env likewise.
- `GAWK_MODERATION_SOURCE=k8s`, plus `adminApi.token`/`adminApi.tokenRef` →
  `GAWK_ADMIN_API_TOKEN` and optional `adminApi.oidc.{issuer,audience,rolesClaim,role}` →
  the `GAWK_ADMIN_OIDC_*` envs (§4.5).
- The headless metrics Service's gate widens from telemetry-only to
  `metrics.enabled && (telemetry-scrape || moderation.enabled)`.

**Official deployment**: portal at a public hostname (e.g.
`admin.gawk.ioio.fi`) behind OIDC — the first internet-exposed admin surface
in the stack, and D7 is its charter. Nothing client-side compiles in any
admin URL.

### 4.14 Dev and non-Kubernetes story

- **Relay enforcement** develops and tests without k8s:
  `-moderation-source=file:bans.json` (stat-poll + SIGHUP reload, §11.1) slots into
  the R38 compose lane; unit tests drive `moderation.Set` directly.
- **`gawk-admin`** requires Kubernetes (CRs) and Postgres by design. Local
  lane: `kind` (the `e2e` tier already exists) or envtest for the
  reconciler; plain `docker run postgres` for the store; OIDC against a
  local Keycloak/dex or a fake-issuer test double (go-oidc supports issuer
  override in tests).
- The kill path's automated end-to-end home is the **kind `e2e-cluster`
  tier**: two relay pods, a broadcast on pod A edge-pulled by pod B, a Ban
  CR applied → both pods drop it with 4006 and a reclaim gets 451.

### 4.15 Database migration policy (D18)

Long-term rules, set now because retrofitting them onto a live schema is
what hurts. They apply to every future `gawk-admin` migration (and to R40's).

**Mechanics.**

- Migrations are **versioned, forward-only SQL files** —
  `migrations/NNNN_short_description.up.sql` (golang-migrate's suffix, which
  the CI lint enforces), monotonically numbered, append
  only: a merged migration is immutable (fix mistakes with the next number,
  never by editing history). Plain SQL because it is reviewable in a diff
  by anyone and runs under any tooling; the runner is the `golang-migrate`
  *library* (version bookkeeping in its `schema_migrations` table +
  `pg_advisory_lock` against concurrent runs) driven by a **`gawk-admin
  migrate` subcommand in the same binary and image** — same image ⇒ the
  schema a release needs travels with the release, and release-please
  versioning covers both. Alternatives weighed: Atlas (declarative diffing —
  more magic than a review benefits from here), sqitch (a second toolchain
  in the image for no gain). Down-migrations are **not written**: rollback
  is redeploying the previous app version, which the compatibility rule
  below guarantees works.
- **Applied separately from the application, never at startup**: the Helm
  `pre-install`/`pre-upgrade` hook Job (§4.13) runs `migrate` to completion
  before the Deployment rolls; if it fails, the upgrade halts and the old
  pods keep serving (§6). Break-glass: run the subcommand by hand
  (documented in self-hosting). The serving process only **checks**: at
  startup it reads the schema version and refuses to serve
  (`readyz: false`, clear log line) if the schema is *older* than its
  minimum; a *newer* schema within the compatibility window is fine by
  construction.

**Compatibility policy — the MUST.** Expand–contract (parallel change):

- Every migration must leave the **previous app release fully functional**
  against the migrated schema. Concretely: new columns are nullable or
  defaulted; new tables/indexes are purely additive; constraints are only
  tightened once no supported release writes the old shape.
- **No renames, ever, in one step** — add the new column/table, backfill,
  dual-read (and dual-write if needed) for one release, drop later.
- **Destructive steps (drop/tighten) ship no earlier than one release
  after** the last release that referenced the old shape — the contract
  phase of a change is always a separate release from its expand phase.
- Backfills are idempotent and batched (no table-locking `UPDATE` over the
  events table).
- CI enforces what it can: a migration-lint gate (forward-only numbering,
  no edit to a merged file) and an integration job that runs the **previous
  release's store tests against the newly-migrated schema** — the
  executable form of the MUST.

---

## 5. Security considerations

- **Two auth boundaries, deliberately different.** The portal: OIDC JWTs
  carrying the IdP-managed `operator` role, safe to route publicly (D7). The relay admin
  endpoints: ClusterIP + static token or audience-scoped OIDC JWT (§4.5),
  *never* routed publicly — they stay at telemetry-read's posture. The
  portal is the only bridge between them.
- **Stateless-auth trade-offs, named**: a JWT cannot be revoked server-side
  before expiry, so the access-token lifetime *is* the revocation horizon —
  keep it short (5–15 min recommended in self-hosting; the refresh-token
  flow makes this free for the user). Removing the `operator` client role
  or revoking the refresh token / IdP session at the IdP cuts access at the
  next refresh. Tokens live in browser memory only; the strict CSP +
  embedded-assets rule are the XSS mitigation; with no cookies there is no
  CSRF surface at all.
- **Webhook signing secrets**: UI-created webhooks' secrets live in
  Postgres — a database reader can forge webhook signatures (it can already
  read every event; stated, not hidden). Chart-defined webhooks keep their
  secrets in k8s Secrets and never enter the database.
- **Raw-ID scoping table** (D8): raw IDs may appear in exactly three
  places — the credential-gated `/internal/admin/*` responses, the OIDC-gated
  portal (UI + `/api/v1`), and Postgres. They may **not** appear in:
  `/statusz` (either listener — unchanged), webhook payloads, relay logs at
  Info+ (existing discipline), or any client-visible surface.
- **A new token, not the internal PSK.** `/internal/admin/*` gets its own
  `-admin-api-token` rather than reusing `-internal-psk`: different trust
  domain (admin service vs. peer pods), independent rotation, and the PSK
  travels in URLs on the media path where this token travels in a header.
- **Ban-reason hygiene**: reasons are operator-private context. They render
  in the portal and Postgres; relays log them at Debug only; webhooks carry
  them (operator-chosen channel) but the self-hosting doc warns that the
  webhook receiver sees reasons.
- **IP bans**: prerequisite `externalTrafficPolicy: Local` on the public
  Service, or every publisher shares the LB/node IP and an IP ban is a
  fleet-wide outage switch. The UI heuristic (§4.9) plus a self-hosting
  warning are the guards; the relay cannot detect the misconfiguration
  itself.
- **The emergency levers stay documented**: rotating `-resume-token-key`
  revokes every resume token fleet-wide (`config.go:99-108`); `kubectl
  apply` of a Ban CR works with `gawk-admin` completely down.
- **What a portal compromise yields**: kill/ban of broadcasts and the raw
  IDs of live broadcasts (joinable). It does *not* yield relay config
  secrets (redacted at the source, §4.5), media, or cluster credentials
  beyond Ban-CR CRUD. State this in the doc the operator reads before
  exposing the Ingress.

---

## 6. Failure modes

| Failure | Behavior |
|---|---|
| `gawk-admin` down | Enforcement unaffected (CRs + informers). No new kills via portal; `kubectl apply` a Ban CR is the break-glass path (reconciler adopts it later). |
| Postgres down | Portal mutations 503; enforcement unaffected; reconciler pauses (CRs untouched — it never GCs when the record store is unreachable). |
| k8s API server down | Existing informer caches keep enforcing current bans on running relays; new bans/kills are delayed until it returns (same blast radius as every Lease operation today). A relay cold-starting during the outage starts with an empty ban set and logs it at Warn — the honest residual risk of D2, and strictly rarer than the poll-the-admin alternative's. |
| Janitor down / CR outlives its expiry | Relays evaluate `expiresAt` at check time (§4.2) — enforcement ends on schedule; the stale CR is cosmetic until the janitor returns. |
| Kill during grace (publisher away) | `TerminateBroadcast` purges the hub + grace timer + caches; the reclaim that would have revived it gets 451. |
| Kill vs. the Lease-deletion race | Both paths are idempotent no-ops on an absent hub; order does not matter. |
| Double kill / re-ban of an active target | `409` from the API; CR names are deterministic so no duplicates can exist. |
| Webhook endpoint down | Retries then `failed`, visible in the portal events view; events are never lost (Postgres is the record). |
| OIDC provider down | No new logins, and refresh stops — with 5–15 min access tokens the portal is effectively unavailable within minutes (validation itself stays offline via cached JWKS, on portal and relay alike). Enforcement is untouched, the relay's static-token path is unaffected, and `kubectl apply` of a Ban CR remains the auth-free break-glass — the IdP is availability-critical for the *portal*, never for *enforcement*. |
| `gawk-admin` leader dies | Another replica takes the Lease within the ~15 s TTL; API traffic (served by all replicas) never notices; in-flight webhook deliveries are safe (`SKIP LOCKED` claims). |
| Migration hook Job fails | The Helm upgrade halts **before** the new Deployment rolls; previous-release pods keep serving against the pre-migration schema (which D18 guarantees they can). Fix forward with the next migration number. |
| Clock skew | Expiries are minutes-granular; relay and admin both compare against their own clocks; NTP-level skew is immaterial, and a CR with `expiresAt` in the past is simply inert. |

---

## 7. Rejected alternatives

- **Admin listener inside `gawk-server`** — no fourth deployable, but drags
  OIDC + Postgres + an SPA into the data-plane image and couples portal
  releases to relay releases. Owner chose separation (D1).
- **ConfigMap as ban storage** — 1 MiB object limit, whole-object CAS
  writes, no per-record schema; fine for five bans, wrong the moment R40
  produces history. Rejected by the owner outright.
- **Relays poll `gawk-admin` for bans** — creates a data-plane→admin-plane
  runtime dependency and a fail-open window on relay cold start while admin
  is down. The CR watch has neither (D2).
- **Lease listing for enumeration** — cluster-mode-only, and Leases carry no
  stats; the per-pod scrape works everywhere and reuses a proven pattern
  (D12).
- **Reusing close 4000 for kills** — zero client work, but the owner chose
  viewer-visible "ended by moderator" (D6).
- **Basic auth / trusted-header SSO for the portal** — insufficient for an
  internet-exposed surface (basic auth has no session revocation, no
  phishing resistance, no IdP policy); trusted-header mode is a standing
  footgun if the proxy is ever bypassed. Full OIDC (D7).
- **Kill/ban verbs on the relay ops listener** — a second enforcement write
  path that would eventually disagree with the CR projection; rejected
  (§4.5).
- **`Ban` status subresource with per-pod acks** — observability candy at
  the cost of relay write RBAC on bans and update fan-in from every pod.
  Not needed to act; revisit only with evidence.
- **Config-file identity allowlists** (`-allowed-emails`-style) —
  authorization state belongs in the IdP, where it is managed and audited
  next to the identities themselves; a config list needs a redeploy to
  grant or revoke an operator and duplicates what client roles already do
  (D17).
- **Cookie sessions for the portal API** — stateful (a sessions table,
  server-side revocation, CSRF machinery) and an obstacle to multi-replica
  serving; the IdP-issued JWT is already a verifiable bearer credential.
  Short access-token lifetimes + refresh-token rotation recover most of
  what revocable sessions offered (D17).
- **Migrations embedded in app startup** — races multi-replica rollouts,
  couples schema change to pod restarts, and hides schema changes from
  review and ops. The separate hook Job + expand–contract policy instead
  (D18, §4.15).
- **A label selector on the Ban informer** — every Ban in the namespace is
  relevant by definition, and a required label turns a forgotten-label
  `kubectl` break-glass ban into a silent no-op (§4.2).
- **A linear scan for CIDR matching** — fine at ten bans, but this is a
  foundational check on the publish path with mixed-prefix v4/v6 targets;
  a longest-prefix-match trie costs little now and never needs revisiting
  (§4.2).

---

## 8. Out of scope (non-goals)

- User accounts or viewer-side identity — join-by-code stays locked in.
- Banning **viewers** (anonymous by design; publishing is the abuse vector).
- A public report/flag button — single-operator platform, reports arrive
  out of band.
- Content **detection** — R40, which produces into this machinery.
- Any settings **write** path (D10) — GitOps remains the only mutation
  channel.
- Fine-grained portal RBAC — R39 knows exactly two roles, `operator`
  (everything) and the reserved `flagger` (R40, flag-only); more tiers
  (read-only auditor, per-action rights) are a different milestone, though
  the role-claim mechanism (D17) is where they would slot.
- Ban federation across deployments, exportable ban lists.
- Implementing `POST /api/v1/content-flags` (schema frozen here; built in
  R40).

---

## 9. Chunks & acceptance criteria

Phase A — relay side (releasable alone; `gawk-admin` not yet required):

| Chunk | Scope | Acceptance criteria |
|---|---|---|
| AP1 | Wire: `CloseCodeTerminatedByOperator` 4006 in `wire.go` + all mirrors + client terminal handling (one allocation pass, one PR) | Constant + doc comment in Go, TS, Rust, asserted by `wire.test.ts`, `golden.rs`, `wirecheck`; golden vectors byte-identical. Viewer: 4006 lands in the terminal set at `viewer-session.ts:200,262`, `reconnect.ts` never retries it, distinct "ended by a moderator" UI string (unit-tested). Broadcasters (app, Go, Rust): 4006 terminal — no auto-resume (test per client); natives render HTTP 451 on dial as "banned" and cap auto-resume retries on repeated 403/451. All four modules' gates green in one PR. |
| AP2 | `gawk-server/moderation` package (CRD types, `Normalize`, `CRName`, `Set` with LPM trie) + `-moderation-source` (k8s informer / file / off) + publish-path enforcement | Unit: `Set` matching — ID exact-after-normalize; CIDR **longest-prefix-match over mixed v4/v6, non-/32 and overlapping prefixes, property-tested against a naive reference matcher**, incl. bare-IP canonicalization; lazy expiry (an expired record never matches without any janitor); `CRName` determinism + DNS-1123 validity for both target types. **Test-first** on the publish path: banned ID claim → 451 before `resume.verify` (a *valid* resume token still gets 451); banned IP → 451 on mint *and* claim; `OutcomeBanned` metric increments; reason absent from Warn logs (asserted). File source: reload on change + SIGHUP (test). k8s source: envtest informer add/update/delete drives `Set`; **watches all Bans in the namespace — a CR with no labels enforces** (no label selector, tested). Config-parse tests for the new knobs; startup log states the source. |
| AP3 | Kill actuation (`Registry.TerminateBroadcast`, `Server.HandleBanAdded`, IP-match kills, Lease deletion) + `/internal/admin/broadcasts` + `/internal/admin/config` + `-admin-api-token` | Go tests: TerminateBroadcast closes publisher + all subscriber kinds (viewer, internal, stripe leg) with 4006, purges prime caches + DVR rings + grace timer, folds every counter (Prometheus totals never decrease — docs/35 f3 regression test), no-ops on absent hub; origin fires `OnBroadcastExpired` → `cluster.Delete`, edge stops its pull; the GC guard at `hub.go:1849` still holds for `EndBroadcast`. IP ban kills exactly the matching live publishers. Admin routes: 404 when no credential is configured; static token constant-time-compared, 401 on mismatch; **JWT path against a fake issuer** — valid JWT with the `operator` role accepted, wrong audience/issuer/expired → 401, valid token without the role → 403, verification succeeds from the cached JWKS with the issuer unreachable; raw + HMAC'd IDs both present and consistent with `ObfuscateID`; `/statusz` byte-identical to before on both listeners (asserted). `config.Sanitized()`: sentinel-secret test proves no secret value escapes. kind e2e (`e2e-cluster` tier): two pods, edge-pulled broadcast, Ban CR applied → both pods 4006, reclaim → 451. |

Phase B — `gawk-admin` core:

| Chunk | Scope | Acceptance criteria |
|---|---|---|
| AP4 | Module scaffold, Postgres store + `migrate` subcommand (§4.15), leader election, kube reconciler/janitor, relayscan, `/api/v1` (broadcasts, kill, bans, events, relays, webhooks CRUD) | `migrate`: idempotent + advisory-locked (parallel-run test); the serving process **refuses to serve on a too-old schema** (readyz false + log line, tested) and never runs migrations itself (asserted: no DDL from the server path). Store: one-active-ban-per-target enforced (409 surfaced), state transitions active→expired/removed only. **Leader election (envtest): reconciler + dispatcher run on exactly one of two live replicas; killing the leader moves them within the lease TTL; two replicas against one Postgres produce no duplicate CR writes or double-sends.** Reconciler: row⇄CR convergence both directions; unknown CR **adopted**, never deleted; Postgres-unreachable ⇒ no CR GC; expiry flips state + deletes CR + emits `ban.expired`. Relayscan: A-record fan-out, per-pod failure degrades that pod to `reachable: false` without failing the aggregate; ≤2 s cache. API contract tests per route incl. `kill` 409-on-active-ban, `bans` target `"publisher"` IP resolution, cursor pagination on events, webhooks CRUD with config-sourced merge + `409 source_immutable` on any write to a chart-defined webhook, secrets absent from every webhook response. `readyz` false without Postgres. |
| AP5 | JWT authentication + role authorization (D17) + `/auth/config` + security headers | Against a fake issuer: valid JWT accepted; tampered signature, wrong `iss`, wrong `aud`, expired, and `nbf`-in-future each → 401; validation succeeds from cached JWKS with the issuer down; JWKS refresh picks up a rotated key. **Roles**: token with the `operator` role in the default Keycloak client-roles path passes; valid token without it → 403 on every route; `{audience}` substitution in the default claim path tested, including an audience that itself contains a dot; an overridden `-oidc-roles-claim` dot-path resolved correctly (incl. a top-level claim); missing/malformed claim → 403, never 500; blanked roles-claim path or empty `-operator-role` refuses to start; **a token minted after role removal at the IdP 403s — the refresh horizon is the revocation horizon (asserted in the flow test)**. `/auth/config` serves `{issuer, clientId, audience}` unauthenticated and nothing else. **`Set-Cookie` appears on no response** (middleware test); CSP incl. `connect-src` issuer origin /`nosniff`/`frame-ancestors` on every response; invalid-credential responses rate-limited per IP. |

Phase C — portal surface:

| Chunk | Scope | Acceptance criteria |
|---|---|---|
| AP6 | The SPA: OIDC public-client flow, broadcasts / bans / events / relays / webhooks views, kill + kill-and-ban dialogs, deep links, embedded serving | UI unit tests: auth — tokens held in memory only (nothing auth-shaped in `localStorage`, asserted), bearer header attached to every API call, **silent renewal via refresh-token rotation before access-token expiry, failed renewal → redirect flow**, 401 triggers refresh-then-redirect, 403 renders the missing-role page; webhooks view — config-sourced rows locked with badge, UI rows editable, test-send works for both; kill dialog requires reason, cooldown default from config; ban dialog presets + IP checkbox showing resolved IP with v4 `/32` / v6 `/64` defaults; the shared-IP (>50 %) warning renders; unban round-trips. Deep links: watch = `#/view/<id>`, telemetry = `#/broadcast/<key>`, hidden when base URLs unset. Embedding: committed `dist/README.md`, `notBuilt` fallback page, SPA fallback routing, `no-store`, and the **no-external-assets test over the built bundle** (telemetry's test, ported). Flagged-pin slot exists but renders nothing (asserted — R40's hook point). |
| AP7 | Webhook dispatcher: multi-webhook fan-out, signing, retries, delivery visibility, test-send | One event fans out to every enabled webhook from **both** sources, each signed with its own secret; disabled webhooks skipped; signature verifiable by an independent HMAC implementation (test vector in the doc); timestamp in signed material; retry schedule honored (fake clock), terminal `failed` after 5 attempts; delivery claims use `FOR UPDATE SKIP LOCKED` — two dispatchers over one queue never double-send (test); deliveries queryable per webhook and rendered in the events view; **no raw ID / no IP in any payload** (schema-level test over every event type); `summary` present; `/test` endpoint delivers a synthetic signed event; zero webhooks ⇒ events still recorded. |

Phase D — deployment, docs, closure:

| Chunk | Scope | Acceptance criteria |
|---|---|---|
| AP8 | `gawk-admin` chart (+ its database connection), `gawk-server` chart moderation additions (CRD, RBAC, knobs, headless-Service gate), release-please + CI wiring, docs (self-hosting §, gotchas sync, `docs/README.md` row), manual pass | `helm template` golden tests: fail-guard fires on Ingress-without-issuer/clientId/audience; `replicaCount` defaults to 2 and a PDB renders; the migration hook Job renders with the release's own image tag + hook annotations; CRD carries `resource-policy: keep`; relay Role gains read-only bans access only when `moderation.enabled`; admin Role includes leases for leader election; secrets render via both literal and `Ref` forms (relay token, per-webhook secrets); `notifications.webhooks` renders into `-static-webhooks` with secrets sourced from Secret env vars; the chart refuses to render with neither `postgres.dsn` nor `postgres.dsnSecretRef` set (the message naming both forms, the CNPG `<cluster>-app`/`uri` recipe and this doc); both forms render (literal → `value`, Ref → `valueFrom.secretKeyRef`); no `postgresql.cnpg.io` object renders under any values; the migration Job carries `pre-install,pre-upgrade`. Migration-lint CI gate (forward-only numbering, merged files immutable) + the previous-release-against-new-schema job (§4.15) exist and gate PRs. release-please manifest builds a `gawk-admin-vX.Y.Z` tag; image + chart publish jobs mirror telemetry's. Self-hosting: bring-your-own-Postgres with the ordering rule and a copy-pasteable CNPG `Cluster` manifest, IdP setup guidance (Keycloak client-roles recipe for `operator`, 5–15 min access tokens, refresh-token rotation for the public client), `externalTrafficPolicy: Local` requirement for IP bans, webhook receiver guidance (signature + replay window), manual `gawk-admin migrate` break-glass, break-glass `kubectl apply` Ban recipe. **Manual pass on the reference deployment**: kill a real broadcast from a phone via OIDC portal — viewers see "ended by a moderator", broadcaster auto-resume gets 451 and stops, re-mint works after cooldown; IP ban blocks a re-mint; unban restores; webhook lands in ntfy. |

Dependency order: AP1 → AP2 → AP3 (relay complete); AP4 → AP5 → {AP6, AP7};
AP8 last. AP1–AP3 are releasable with `moderation.source=file` before
`gawk-admin` exists.

---

## 10. Verification

The milestone-closing claim is AP8's manual pass, end to end on the
reference deployment: an operator away from home, holding only a phone,
receives a webhook push, opens the portal through OIDC, watches the
offending stream via its deep link, kills it with a ban — and every viewer
sees it end with the moderator message, the broadcaster cannot return with
its resume token or a fresh mint from the same IP, `kubectl get bans` shows
the enforcement object, and the whole exchange left an audit trail in
Postgres. Glass stays intact for every other broadcast on the fleet.

---

## 11. Implementation status (2026-08-20)

All eight chunks landed on the same day the design did. This section is the
record of what the design could not have known: §11.1 lists every deviation
from §4 with its reason, §11.2 says what is verified and by what, §11.3 is the
one pass that is still manual — **and it is the milestone-closing one** — and
§11.4 says where to record its outcome.

Per-chunk verification is not restated here: each chunk's acceptance-criteria
cell in §9 is its contract, and the tests that discharge it live next to the
code.

### 11.1 Deviations from §4, and why

| § | Specified | Implemented | Why |
|---|---|---|---|
| §4.3, §4.14 | the file ban source reloads on **fsnotify** + SIGHUP | a **stat poll** (mtime + size) + SIGHUP | `internal/tlsutil/reload.go` already solves "notice this file changed" this way, and matching it adds no dependency to the relay module for a dev-lane feature. The observable behaviour — a changed file is picked up without a restart — is unchanged; the latency is one poll interval instead of a kernel event. |
| §4.8, §6 | an unreachable IdP is discussed only as a *running* failure | **`New` does not fail on an unreachable issuer at all**: discovery retries in the background, authenticated routes answer `401 idp_unavailable` (never 500, and deliberately without spending the caller's rate-limit budget) until it resolves, and `Ready()`/`ResolveError()` feed `/readyz` | A portal that refuses to start because the IdP is briefly down is a portal that is down for longer than the IdP was — and it turns "restart the pods" into a step that can fail for a reason unrelated to the pods. The security posture is identical: no request is ever authorized while the issuer is unresolved. It also lets the CI image smoke get all the way to its one expected failure with neither Postgres nor an IdP present, instead of stopping at the IdP. |
| §4.13, §4.15 | the migration Job hooks `pre-install`/`pre-upgrade` | `pre-install`/`pre-upgrade` — **deviation withdrawn** | It was briefly implemented as `post-install`/`pre-upgrade`. A `pre-install` hook runs **before** the release's own manifests, so while the chart still rendered its own CNPG `Cluster` the Job waited for a Secret Helm would not create until the Job finished — a deadlock on every first install. The fix landed one level up instead: the chart no longer creates a database, it takes a connection to one that must already exist (§4.13). That makes the database a *prerequisite* rather than a dependency of the release — exactly what a `pre-install` hook is allowed to depend on — so the spec's wording is right as written, and §4.15's guarantee now holds on a **first install** too, not only on upgrades: migrated to completion before the Deployment rolls, and a failed migration halts the release instead of rolling pods that would come up NotReady. The Helm ordering rule that forced the detour is still worth knowing and stays in `docs/gotchas.md`. |
| §4.13 | — | the relay chart's `moderation.source` is a value (`k8s` default), not just `moderation.enabled` | `-moderation-source` accepts `file:<path>` too (§4.14), and a chart that hardcoded `k8s` would have made the compose-lane source unreachable from a Helm install for no reason. |
| §9 AP8 | *(the kind `e2e-cluster` scenario is listed under AP3)* | landed in **AP8**, as `e2e/moderation-assert.sh` | It needs the CRD template, the relay Role's `bans` access and a chart value — all deployment files — so building it from AP3 would have collided with the chunk that owns them. What it asserts and what it deliberately does not are in the script's own header. |
| §4.7 | the kill / create-ban / unban rows name only `201`, `204` and `409` — the table has **no partial-failure case** | a third outcome: `202 Accepted`, body = the ban with `enforcement: {inSync: false, detail}` | A ban is two writes in two systems with no shared transaction, so "row committed, `Ban` CR not written" is a real state the table never assigns a code to. It is not a failure — the record is durable and the reconciler heals the CR within a minute — and it is not a plain `201` either, because nothing is enforced yet. `202` is what RFC 9110 §15.3.3 defines for exactly this shape. Being a *success* matters more than the number: it is what stops a client reporting "nothing happened" and inviting a re-submit that now `409`s against the row that does exist. `enforcement` is `inSync` rather than `enforced` so the field reads correctly in both directions, and it appears only on that response, leaving every `201`/`204` and every list/read byte-identical. An earlier implementation answered `502 Bad Gateway` here; nothing acted as a gateway, and the owner overruled it. |
| §4.5, §4.8 | the JWKS is **cached and background-refreshed**; per-request verification is offline | **`oidc.RemoteKeySet`** on both sides — cached, fetched lazily on a verification miss, **no background refresher** — behind a **token-bucket floor on the fetch itself** (3/minute, burst 3), plus one **throttle-exempt priming fetch** once discovery resolves | R39 first shipped two hand-rolled caches to avoid `RemoteKeySet` fetching inline on an unknown `kid`. Reading upstream showed the premise was mostly wrong: `keysFromCache()` has **no expiry check**, so a cached key verifies forever and an IdP outage cannot break it — the offline guarantee this section actually asks for — and `keysFromRemote()` already coalesces concurrent misses. Two re-implementations of that bought nothing and cost one real bug: a generation-snapshot race in the portal's own coalescing that would have 401'd every operator through a key rotation. What upstream genuinely lacks is a **rate floor** — `verify()` fetches on *any* miss, including a known `kid` with a forged signature — so the floor sits on the HTTP transport rather than on the token's `kid`, where a wrapper would have left the commonest abuse paths unthrottled. An exhausted bucket is a **401**, never a 5xx. Priming replaces the background refresher's one useful property: without it a rolling deploy leaves every replica one IdP round trip from its first verification, so an outage between the deploy and the first operator 401s them from every fresh pod. `Ready()` still means "discovery answered", not "keys cached". |
| §4.5, §4.8 | the roles-claim dot-path is implemented per side, and the placeholder is spelled `{audience}` on the relay but `{clientId}` in the portal | one public **`gawk-server/oidcroles`**, imported by both; the placeholder is `{audience}` everywhere, and the spec text above is corrected rather than deviated from | The duplication is *why* the dotted-audience bug (an audience containing a dot shattering the path and 403ing every valid token) existed on one side only — the two copies had drifted to different tolerances for a bare-string claim and a mixed array, i.e. two different answers to "is this token authorized". `{audience}` won because it is the only identifier both callers have, and because in a Keycloak access token `resource_access.<client>.roles` carries the roles of the resource server the token was minted *for*, which is what `aud` names. The package takes decoded claims rather than an `*oidc.IDToken` **specifically so it imports no OIDC library**, which is what keeps the relay's containment test meaningful. |
| §9 AP8 | the `docker` job smoke-probes every image's HTTP endpoint | the `gawk-admin` entry carries **no probe**; it asserts the process reaches its documented cluster-less failure | The image genuinely cannot serve without Kubernetes (§4.14). The probe was written before `cmd/gawk-admin/main.go` existed and would have failed on its first CI run. A smoke that asserts something false is worse than one that asserts less. |
| §4.14 | the local lane is "kind or envtest" | `main.go` also falls back to the ordinary kubeconfig loading rules when in-cluster config is absent | That fallback is what makes "run it against kind" work without pretending to be a pod. Deliberately **not** a knob: `KUBECONFIG` is the conventional mechanism, and a flag would have to reach the chart to satisfy the carry-all-limits rule for something a pod would never set. A missing API server stays fatal — unlike the IdP, there is nowhere to write a Ban CR, and a kill button that records a row nothing enforces is worse than a refusal to start. |
| §4.8 | the SPA runs the redirect flow with a *bundled* OIDC client library | the whole public-client flow (code+PKCE, `state`/`nonce`, token endpoint, silent renew) is **hand-rolled over WebCrypto** in `ui/src/auth/` — the SPA's runtime dependencies are exactly `react` and `react-dom`, and the §4.8 text is corrected in place | The candidate libraries carry their own storage and iframe machinery, most of it for flows this SPA forbids (persistent tokens, silent-auth iframes), and auditing what a library does *not* do proved harder than owning the ~700 lines it actually needs — which the suite tests directly (PKCE vectors, `state`/`nonce` round-trips, renewal). The trade is recorded here because it is security-critical: a CVE search for this portal's OIDC path must target `ui/src/auth/`, not a dependency list — `THIRD-PARTY-NOTICES.md` says the same. |
| §9 AP2, AP4 | the k8s ban source and leader election are verified with **envtest** (a real kube-apiserver + etcd) | **client-go fakes throughout**: a fake dynamic client / ListerWatcher for the informer-drives-`Set` tests, `k8sfake.Clientset` for leader election, `dynamicfake` for the reconciler and `CRClient` | The tests were written where the code was written, on a machine without envtest binaries, and the fakes prove the logic is *called* correctly — which covers most of what AP2/AP4 ask. What they cannot prove is recorded in §11.2's "not covered" list: Lease CAS actually contended under real etcd, the chart's CRD schema accepting the reconciler's real payload, and RBAC verb coverage. Standing follow-up: an envtest-backed CI job (the runners can fetch envtest binaries the same way they fetch kind images), or extending the kind tier to install `gawk-admin` and drive one kill through the API — either discharges the first two directly. |
| §9 AP8 | "`helm template` **golden tests**" | shell assertions inside CI's `helm` job | The repository's existing idiom, and for the stated reason: a golden render has to be regenerated on every release-please version bump, because the chart version, appVersion and image tag all appear in it. The assertions are stronger than a diff anyway — several of them assert that something is *absent*. |

### 11.2 What is verified, and by what

Automated, and gating every PR:

- **The four modules' own suites.** `admin` (with a Postgres service container,
  and a step that fails if the database-backed tests *skipped* — a suite that
  silently skips is worse than one that fails), `admin-ui`, `server`, `app`,
  and the wire mirrors in all four languages.
- **`admin-migrations`** — the §4.15 migration-lint gate: forward-only
  numbering from `0001` with no gaps, no `.down.sql` anywhere, and no
  already-merged `.sql` file modified, renamed or deleted relative to the merge
  base.
- **`admin-schema-compat`** — the §4.15 MUST, executable: the *previous
  release's* store tests run against *this branch's* migrations. Until the
  first `gawk-admin-vX.Y.Z` tag exists it is a documented no-op that says so in
  its own log; it starts enforcing on the first run after that release with no
  further wiring.
- **The `helm` job's R39 assertions** — the fail-guard fires (and names the
  missing knob and this doc) for each of the three OIDC values; `replicaCount`
  defaults to 2 with a PDB, and no PDB at 1; the migration hook Job carries
  this release's own image tag, identical to the Deployment's, with the hook
  annotations and `args: ["migrate"]`; the admin Role covers Ban CRUD *and*
  leader-election Leases; secrets render through both the literal and the
  `Ref` form; `notifications.webhooks` renders into `-static-webhooks`
  carrying env var *names* and never a signing key; the chart refuses to render
  without a database and says how to give it one; both connection forms
  render; no CloudNativePG object renders under any values; the
  relay chart renders the CRD with `resource-policy: keep`, printer columns and
  **read-only** `bans` RBAC with `clusterMode` off; and — the check in the
  other direction — with `moderation.enabled=false` the relay chart renders
  nothing moderation-related at all.
- **The `docker` job** builds the `gawk-admin` image from the repo root and
  then RUNS it, pointed at a database that does not exist and an issuer that
  does not resolve. It cannot assert that `/healthz` answers — `gawk-admin`
  requires Kubernetes by design (§4.14) and exits without it — so it asserts
  the failure instead: reaching `kubernetes:` proves the binary linked, the
  entrypoint is right, every flag parsed, and the Postgres and OIDC
  construction ahead of it were wired. Building an image is not evidence that
  it starts (R31 taught this the expensive way), and asserting a `/healthz`
  this image cannot serve would have been worse than asserting nothing.
- **The kind `e2e-cluster` tier** — note it does **not** run on an ordinary
  feature PR: it is gated to release-please PRs and `workflow_dispatch`
  (docs/25 Decision 1, unchanged by R39), so the first automated run of the
  moderation scenario is the release PR, or a dispatch on demand. It applies a real `Ban` CR to a two-pod fleet
  and asserts the fleet-wide kill from outside every process: the broadcast's
  key leaves **both** pods' `/statusz`, `gawk_moderation_terminations_total`
  reaches ≥1 on **each** pod, an IP ban makes a fresh publish fail and
  increments `gawk_connections_total{route="publish",outcome="banned"}`, and
  deleting both bans returns every pod's `gawk_moderation_bans_active` to zero
  and lets a fresh mint through. It also proves the admin API answers on the
  ops listener with the token, 401s without it, and that its raw-ID → HMAC-key
  mapping agrees with `/statusz`. It also observes the two things nothing in
  the harness could see before. Synthetic viewers attach through
  `gawk-loadgen -expect-close-code 4006` before the kill, and the script waits
  until **both** pods are serving some of them — so what it asserts is that
  4006 crossed the *cascade*, read off the session itself rather than scraped
  from `kubectl logs`, which would have proved the log line and not the wire.
  A 4000, a transport death carrying no application code at all, and a session
  nothing ever closed each fail differently, because they are different
  diagnoses. And the IP-ban step asserts the *status*, not merely the failure:
  `gawk-pubsim` prints `GAWK_PUBSIM_DIAL_STATUS=451` and exits 3. That is the
  only automated evidence for the property D15 exists for — a browser sees an
  opaque dial failure, so "451 lets a native broadcaster say *banned* instead
  of *auth failed*" is a claim only a native client can make, and until now
  nothing made it.

**Not covered by anything automated**, and named rather than glossed:

- **That either of the two assertions above gates a feature PR.** They live in
  the kind tier, so a regression in the 4006 fan-out or in the readable 451
  reaches `main` and is caught by the release PR or a dispatch, not by the PR
  that caused it. What does gate every PR is the layer underneath: AP1's
  per-client and per-role 4006 tests, AP3's `TerminateBroadcast` fan-out, and
  `internal/engine`'s 451 rendering.
- **The 4006-vs-4000 ordering on an edge pod, in the residual case.** The
  origin's kill deletes the cluster Lease, and a pod that saw the lease
  deletion before its own Ban event would once have expired the hub through
  the ordinary path and closed its viewers with 4000. `HandleLeaseDeleted` now
  consults the ban set, so the arrival order no longer decides the message —
  but it consults `BannedID` only, so a lease deletion racing an **IP-only**
  ban still takes the 4000 path. Every portal kill creates an ID ban, so the
  covered case is the one that matters; the gap is reachable only from a
  break-glass `kubectl` IP ban with no accompanying ID ban, is
  presentation-only, and is arguably unknowable from an edge that holds no
  publisher entry to map the address onto.
- **The Kubernetes seam beyond what fakes can enforce** (the §11.1 envtest
  deviation): Lease optimistic concurrency under a real etcd — the fake's
  object tracker enforces no resourceVersion conflicts, so mutual exclusion
  rests on client-go's election logic being *called* correctly, never on Lease
  CAS being *contended*; the chart's CRD OpenAPI schema accepting the
  reconciler's actual CR payload — the kind tier applies hand-written Ban YAML
  and never installs `gawk-admin`, so no real API server has validated a
  `CRClient` write anywhere automated; and RBAC verb coverage. These are where
  R39's two worst silent failure classes live (a schema-rejected reconciler
  stops projecting every ban while the fleet enforces stale state; a double
  leader double-sends webhooks), so the gap is named here until the envtest CI
  job or a kind-tier `gawk-admin` install closes it.
- **Ban history at scale**: `GET /api/v1/bans?state=all` is unbounded and the
  portal renders the whole history, filtered client-side. Events got cursor
  pagination for exactly this monotonic-growth shape; ban history needs the
  same before R40's auto-kills accelerate it (a follow-up, noted rather than
  wished away — the rows are UUID-keyed, so the cursor design is not a copy of
  the events one).
- **Everything in §11.3.**

### 11.3 Still manual — the milestone-closing pass, step by step

**This is the verification §10 describes, and nothing else can stand in for
it.** It needs the reference deployment, a real identity provider, a real
broadcast and a phone — none of which a CI job or a working session can supply.
It is not a known defect; it is the last mile.

Run it in this order and record the outcome where §11.4 says.

**Setup (once).**

1. Bring up the Postgres `gawk-admin` will use — the CNPG operator plus a `Cluster`, or a database you already run — then upgrade the relay
   with `moderation.enabled=true` and a fresh `moderation.adminApi.tokenRef`
   Secret, and install `gawk-admin` behind an OIDC-gated Ingress at
   `admin.gawk.ioio.fi`. `docs/self-hosting.md` §9.2 is the runbook.
2. In the IdP: a public client with PKCE, a **client role `operator`** assigned
   to your own account, an access-token lifespan of **5–15 minutes**, and
   refresh-token rotation on. §9.3.
3. Confirm the relay's Service is `externalTrafficPolicy: Local` **before**
   step 8 — otherwise every publisher shares a node IP and the IP-ban step is
   a fleet-wide outage rather than a test. §9.4.
4. Point one chart-defined webhook at an ntfy topic on your phone.

**The pass.**

5. Start a real broadcast (native broadcaster or the web app). Open the portal
   **on the phone**, through the OIDC redirect flow, and confirm: the broadcast
   is listed, the watch deep link opens it in the viewer, and the telemetry
   deep link resolves to the same broadcast.
6. **Kill it, with a reason.** Expected, all four: every viewer's player ends
   with *"This broadcast was ended by a moderator"* (distinct from "broadcast
   ended"); the broadcaster stops and does **not** auto-resume; the
   broadcaster's own reclaim attempt is refused (the natives should say
   *banned*, a browser broadcaster sees a generic dial failure — D15, expected);
   and the ntfy notification arrives on the phone carrying **no raw broadcast
   ID and no IP**.
7. `kubectl -n production get bans` shows the cooldown ban with its printer
   columns. Wait out the cooldown (or shorten it) and confirm a **re-mint
   succeeds**.
8. **Ban the publisher's IP** from the portal, using the resolved-IP checkbox.
   Confirm a fresh mint from that address is refused, and that a broadcaster on
   a *different* address is unaffected.
9. **Unban** from the portal. Confirm publishing works again, and that
   `kubectl get bans` is empty.
10. Read the events view: every action above appears, with actor, reason and
    timestamp, and the webhook deliveries are visible against each event.
11. Sanity-check the revocation horizon: remove the `operator` client role from
    your account in the IdP and confirm the portal 403s **at the next token
    refresh** — that horizon *is* the access-token lifetime (D17), and seeing it
    once is worth more than the paragraph that says so.

**If a step fails**, the finding belongs in §11.2's "not covered" list or in
`BUGS.md`, and — if it turns out to be a browser or Kubernetes behaviour rather
than a gawk one — in `docs/gotchas.md`.

### 11.4 Where to record the outcome

When the pass completes, add what was observed to §11.2 (client, device, IdP,
and anything that behaved differently from the expectation above), strike §11.3,
and move the `ROADMAP.md` R39 row from 🔧 to ✅ — that pass is the only thing
still holding it.
