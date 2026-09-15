# R49 — Rooms read API and room activity events (docs/50)

**Status**: designed 2026-09-15; **not started**. Chunks **RA1–RA5** (`RA` =
Rooms API). Touches `gawk-server` (one read-only route on the ops listener,
one public type file), `gawk-admin` (routes, a role, a watcher, one
migration, chart values, SPA) and the docs. No wire change, no media-path
change, nothing on the public relay listener. **Depends on R48**
([docs/49](49-admin-openapi.md)): every route here lands in the OpenAPI
document under R48's drift check, and the first external consumer reads
that document. Depends on R42 ([docs/44](44-rooms.md)) for the rooms
themselves.

## 1. Purpose

A room is the object integrations attach to — docs/44 §1 names a Mumble
bridge as the first, and §4.11 reserved the hooks. The bridge itself is
not this milestone. Before a bridge, a much smaller thing is wanted: a
**Mumble bot in its own repository** that can ask *which rooms are live,
who is in this one, what is being streamed there*, post a join link into
the voice channel, and say when someone arrives or a stream attaches. That
is a read API plus a push signal, both against `gawk-admin`, which already
holds the operator's view of every room.

What `gawk-admin` knows today is the `Room` CR: spec (kind, display code,
display name, attach-secret gating, max broadcasts) and status (HMAC'd key,
creation time, home-pod lease, **attachments with raw broadcast IDs and
labels, kept current by the home pod on every attach and detach**
(`roomcluster/store.go:509`), and `emptySince`). What it does **not** know
is the roster: docs/44 D5 says the participant list is never stored, and
it lives only in the home pod's registry (`roomsrv/registry.go:183-203`),
with nickname, client kind, streaming and speaking flags and the reserved
identity field per participant, and live/away plus viewer count per
attachment. The relay already exposes a credential-gated, ClusterIP-only
admin surface for exactly this kind of live data (`/internal/admin/
broadcasts`, docs/42 §4.5), and `gawk-admin`'s `relayscan` already
scrapes it with a 2 s cache. The design extends both by one route.

Owner decisions, 2026-09-15:

- **Full detail**: attachments *and* the live roster, which means the
  relay-side route and the scan fan-out.
- **The bot authenticates as an OIDC client-credentials service identity
  carrying a new read-only role**; the operator role is not handed to a
  bot that only needs to read.
- **The read-only role receives the raw room code and a join link.**
  Posting a join link is the bot's purpose; a key-only API would be
  useless to it.
- **Poll now, and add webhook events** for attach/detach and participant
  join/leave, so the bot subscribes and then fetches detail.
- The bot lives elsewhere; gawk ships the API and its OpenAPI description,
  nothing bot-specific.

## 2. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **A new role, `rooms-reader` (`-rooms-reader-role`, `GAWK_ADMIN_ROOMS_READER_ROLE`, chart `oidc.roomsReaderRole`), grants exactly `GET /api/v1/rooms`, `GET /api/v1/rooms/{name}` and `GET /api/v1/me`.** Nothing else. The operator role keeps everything it has. `internal/auth` gains `RequireAnyRole(roles...)`; the R48 route table's `roles` column carries both for those three entries, and `x-gawk-roles` says so in the document. | The R39 role model is one role per capability, not a hierarchy — `flagger` (docs/42 §4.11) grants flag-only rights the same way. A generic `reader` over every `GET` would hand a bot publisher IPs, ban CIDRs and free-text ban reasons it has no use for. Least privilege costs one middleware variant. `/me` is included so the bot can probe its own token the way the SPA does. |
| D2 | **The relay serves `GET /internal/admin/rooms` on the ops listener**, behind the existing `AdminAuth` guard, schema `gawk.admin.rooms.v1`, types in the public `adminapi` package next to `Broadcast`. One row per room **this pod is home for**: code (raw), kind, display code, display name, key, created-at, empty-since, `attachments[]` (broadcast ID, label, live, viewers, attached-at) and `participants[]` (per-room ID, nickname, client kind as `web-viewer`/`web-broadcaster`/`native`, `streaming`, `speaking`, `identity`). Proxy rows are **not** listed (a proxying pod knows only a session count; the home pod is the truth). Built by a new `Registry.AdminRooms()` walking `r.rooms` under the lock, the way `AdminStats()` walks hubs. `/statusz` stays byte-identical. | The roster exists in exactly one place, and this listener is the sanctioned place for raw identifiers (docs/42 D8: ClusterIP-only and credential-gated). Reusing `adminapi` means `relayscan` compiles the same struct instead of mirroring twelve JSON tags (CLAUDE.md). Kill/end verbs stay absent here exactly as kill/ban do for broadcasts: the `Room` CR is the write path. |
| D3 | **`gawk-admin` merges two sources per room: the CR list (spec, key, lease, managed, attach-secret gating) and the fleet scan (live roster and attachment state from the home pod).** `relayscan.Snapshot` gains `Rooms []RoomAggregate`, scraped from every pod with the same 2 s cache; the merge keys on the normalized code (CR name = the relay's normalized code). A room whose home pod did not answer, or that no pod has homed, renders from the CR alone with `live: false`, an empty roster and the CR's attachment list without live state. | The CR is durable and complete for static facts; the scan is live and partial by design (one dead pod degrades itself, never the aggregate — `relayscan`'s first property). Rendering the union with an explicit `live` flag tells the consumer which half it is looking at, instead of an empty roster that could mean "nobody" or "unknown". |
| D4 | **Two routes, one resource shape.** `GET /api/v1/rooms` returns summaries: the R42 fields plus `live`, `links.join`, and `counts {participants, streaming, watching, attachments}`; the R42 integer `attachments` becomes `counts.attachments` (the SPA's `RoomsView` is updated in the same chunk — the only consumer). `GET /api/v1/rooms/{name}` returns the same object plus `attachments[]` and `participants[]` in full. `{name}` accepts any spelling of the code (`rooms.NormalizeCode`, as the write routes already do). `links.join` is `<appBaseUrl>/#/room/<displayCode>`, omitted when `-app-base-url` is unset — the shape `links.watch` already has on broadcasts. Attachments carry `links.watch` too. | A bot polls the list every few seconds and fetches one room on an event; rosters of up to fifty on every list row would make the list *be* the detail. One schema with two projections keeps the OpenAPI document honest (R48 D3 (f) decodes both into the same Go type). The rename is the one non-additive change this milestone makes, and it is made *before* the document has an external consumer: R48 ships the document, R49 revises it in the same PR that reshapes the route, and after R49 the additive-only promise (R48 D9) applies. Re-attaching a count field beside an array under the same name was the alternative, and it would have been the lie the drift check exists to catch. |
| D5 | **The raw room code and the join link are returned to `rooms-reader` callers; broadcast IDs in `attachments[]` are returned too.** Marked `x-gawk-sensitive` in the document with the reason. Nothing on these routes carries an IP. | Owner decision: the bot's whole purpose is to hand people a way in. A room code is a joinable secret (docs/44 D16) and this is the second scoped relaxation of that invariant after the operator portal (docs/42 D8), on the same terms: authenticated, role-gated, never in a webhook (D7). An attachment's broadcast ID is what makes `links.watch` and the room's own tiles work; hiding it while returning the room code would protect nothing. |
| D6 | **Four activity events**: `room.attached`, `room.detached`, `room.participant_joined`, `room.participant_left`, produced by a **leader-side watcher** in `gawk-admin` that polls the merged room view (D3) every `-room-watch-interval` (default `5s`) and diffs it against its previous pass: attachments by broadcast ID, participants by per-room participant ID. The first pass after start or leadership change only seeds the baseline. A join and leave inside one interval is unobserved; a home-pod move (adoption re-issues participant IDs) reads as leaves followed by joins, and the watcher suppresses the pair when the roster's nicknames are unchanged within one pass. | `gawk-admin` deliberately has no informer on `Room` CRs (docs/44 §11: the reconciler sweeps), and the roster is not in the CR at all, so a poll is the only source; the relay does not know `gawk-admin` exists and must not (it reads CRs, nothing else — docs/42 D2). Five seconds is a chat-bot's notion of "now", and the scan's 2 s cache bounds relay load whatever the interval. The adoption case is called out because it is the one way a diff lies; the suppression rule is tested. |
| D7 | **Activity events are recorded as events with `category = 'activity'`** (migration `0002`: `moderation_events.category text NOT NULL DEFAULT 'moderation'`, expand-only), excluded from the portal's Events view unless its new filter includes them, **pruned by the janitor after `-activity-retention` (default `72h`)**, and dispatched through the same delivery pipeline **only to webhooks that opted in** (D8). Payloads carry the R42 room fields (`roomKey`, `portalUrl`, `summary`) plus two new webhook-safe keys, `nickname` (participant events) and `label` (attachment events). **Never a broadcast ID, never a room code, never an IP** — the R39 rule, unchanged. | One table and one dispatcher, because the retry and the leadership semantics are already right there. A category rather than a second table because the delivery rows reference `moderation_events(id)` and the portal's feed is one query. Retention because fifty joins an evening is not audit material. `nickname` and `label` are free user text, like `reason` is free operator text — the same posture applies (self-hosting §9.5: send to a channel you would be comfortable having read) — and without them the bot would have to diff rosters itself to say who arrived. |
| D8 | **Webhooks gain an event filter.** `webhooks.events text[] NULL` (same migration), `events` in the `-static-webhooks` JSON and the chart's `notifications.webhooks[].events`, and in the portal's webhook form. `NULL`/absent means **every `moderation` event and none of the `activity` ones** — today's behaviour for every existing webhook. A listed set is exact. The dispatcher consults the filter when it enqueues deliveries; a `PUT` on a config-sourced webhook's filter is still `409 source_immutable`. | Without a filter, the ops pager on an operator's phone (docs/42 §4.10's motivating receiver) would buzz on every join. Opt-in for the new category makes this milestone invisible to every existing receiver, byte for byte. |
| D9 | **The bot's identity is an IdP-managed service account on the client-credentials grant, with the `rooms-reader` client role.** No gawk-minted token, no static secret in `gawk-admin`. Self-hosting gets the Keycloak recipe (a confidential client with service accounts enabled, the client role assigned to its service-account user, audience mapped) beside the R48 recipe. | The R40 `flagger` design already settled this shape (docs/42 §4.11); a bot is the same kind of caller. Revocation is the token lifetime, as everywhere else in the portal (§4.8). |
| D10 | **What the SPA does with it**: `RoomsView` shows the counts and a `live` mark, and expands a row into the roster and the attachment list (nickname, kind, streaming/speaking, label, live/away, viewers). The Events view gains a category filter, default `moderation`. The webhook form gains the event picker. No new view. | The operator gets the same data the bot gets, from the same route, which is also how the merge (D3) gets exercised by a human before a bot depends on it. |
| D11 | **Knobs**: `-rooms-reader-role` (D1), `-room-watch-interval` (D6), `-activity-retention` (D7); all with `GAWK_ADMIN_*` envs and chart values; the watcher runs only with `-rooms` on and only on the leader. Relay side: no new knob — `/internal/admin/rooms` is live whenever `/internal/admin/broadcasts` is, and returns an empty list with `-rooms` off. | docs/44 D17 / docs/42 §4.12 — every knob through the same path. The relay route needs no gate of its own: the credential gate is the gate, and a relay without rooms has nothing to say. |

### Rejected

- **Storing the roster in the `Room` CR** so the CR alone would do —
  docs/44 D5, for the reasons given there (write amplification on every
  join, the API server as a chat backend). Unchanged.
- **The relay pushing room events to `gawk-admin`** — the relay reads CRs
  and knows no other component (docs/42 D2). A push would couple the data
  plane to the portal's availability.
- **A public relay route for bots** (`/api/rooms` on the WebTransport
  listener) — the relay's public listener exposes HMAC'd keys only
  (docs/44 D16); raw codes belong behind OIDC, in `gawk-admin`.
- **A static bot token in `gawk-admin`** — no shared secrets in the portal
  (docs/42 D7); the IdP already does service identities.
- **A generic `reader` role over every `GET`** — D1.
- **Server-sent events / a WebSocket feed from `gawk-admin`** — noted as
  the next step if polling plus webhooks proves insufficient for a
  bridge; would be `GET /api/v1/rooms/{name}/events` (SSE). Not built.
- **Delivering activity events to every existing webhook** — D8.
- **Keeping the integer `attachments` on the list row beside the array
  on the detail** — D4.

## 3. Where it plugs in

| Piece | Where it is today | What RA changes |
|---|---|---|
| Relay room snapshot | `roomsrv.Registry.Stats()` (`registry.go:1413`, counts under HMAC keys); `Attachments(code)` (`:1348`) | **New** `Registry.AdminRooms() []adminapi.Room`: full rows with raw code, roster and attachment state, home rooms only. |
| Relay admin route | `internal/ops/admin.go:75-97` (`broadcasts`, `config`), `AdminOptions` | `GET /internal/admin/rooms` via the same `guard` and `writeAdminJSON` (`Cache-Control: no-store`); `AdminOptions.Rooms` is a `func() []adminapi.Room` supplied by the transport (nil with `-rooms` off → empty list). |
| Shared contract | `gawk-server/adminapi/adminapi.go` | `SchemaRooms = "gawk.admin.rooms.v1"`, `Room`, `RoomAttachment`, `RoomParticipant`, `RoomsResponse`. |
| Fleet scan | `gawk-admin/internal/relayscan` `scrapePod` (`:303`), `Snapshot` (`:126`) | Scrape `/internal/admin/rooms` alongside `broadcasts`; `Snapshot.Rooms`; per-pod failure degrades that pod only, as now. |
| Room routes | `internal/api/rooms.go` (`handleListRooms`, `roomJSON`, `Rooms` seam) | `roomJSON` gains `live`, `links`, `counts`, `attachments[]`, `participants[]`; `handleGetRoom`; the merge lives in a `roomview` helper both handlers and the watcher call. `Rooms.List` unchanged. |
| Roles | `internal/auth` `RequireRole` (`auth.go:611`); `internal/config` `-operator-role`, `-flagger-role`; `api.protect` | `RequireAnyRole`; `Config.RoomsReaderRole`; the R48 route table's `roles` per entry; `/me` unchanged (roles are already echoed). |
| Watcher | — | **New** `internal/roomwatch`: leader-only loop (same election the reconciler and dispatcher use), diff, record via `store`, `kick` the dispatcher. |
| Store + migration | `migrations/0001_initial_schema.up.sql`; `store.Event*`; the janitor | `0002_activity_events.up.sql`: `category` on `moderation_events`, `events` on `webhooks`; `EventRoomAttached` etc.; `PayloadNickname`, `PayloadLabel` as webhook-safe keys; `ListEvents` category filter; `PruneActivityEvents(before)`. |
| Dispatcher | `internal/notify` `buildPayload` (`payload.go:150`), enqueue on insert | Consult the webhook's filter when enqueuing; copy the two new keys into `Payload` (additive, schema string unchanged, the `enforcement`/`roomKey` precedent). |
| Static webhooks | `-static-webhooks` JSON; chart `notifications.webhooks` | `events` list per entry. |
| SPA | `ui/src/views/RoomsView.tsx`, `EventsView.tsx`, `WebhooksView.tsx`, `api/types.ts` | D10. |
| OpenAPI | `gawk-admin/openapi.yaml` (R48) | The two room operations with `x-gawk-roles: [operator, rooms-reader]`, the revised `Room` schema, four new `webhooks` entries, the `events` field on webhook objects. R48's drift test forces every one of these. |
| Docs | docs/self-hosting §9 and §10; `docs/44` §4.11 | §9.8 gains the `rooms-reader` service-account recipe and the webhook `events` filter; §10 links to the API; §4.11 of docs/44 gets a dated note that the bot-facing read surface shipped here. |

## 4. Chunks and acceptance criteria

| Chunk | Scope | Verified by |
|---|---|---|
| **RA1** | Relay: `adminapi` room types, `Registry.AdminRooms()`, `GET /internal/admin/rooms` (D2, D11) | `roomsrv` test: a room with two participants (one streaming, one not) and two attachments (one away) snapshots to the expected rows, raw code present, `speaking` false, `identity` empty; proxy-only rooms absent. `ops` test: 401/403 exactly as `broadcasts`; `-rooms` off → `{"schema":…,"rooms":[]}`; `/statusz` byte-identical to the R42 fixture (the existing assertion extended to this PR). `Cache-Control: no-store`. `go test -race ./...` green. |
| **RA2** | `gawk-admin`: relayscan rooms scrape, the merge, `GET /rooms` reshape, `GET /rooms/{name}`, `RoomsView` (D3, D4, D5, D10) | relayscan test: a pod that answers `broadcasts` but not `rooms` is `reachable: true` with `roomsErr` set and contributes no rooms. API contract tests: list row has `counts`, `live`, `links.join` (absent without `-app-base-url`); detail carries roster and attachments; a homed room with an unreachable home pod → `live: false`, roster empty, attachments from the CR without `live`/`viewers`; unknown or malformed name → 404; `?name=TuhisRoom` and `tuhisroom` resolve the same CR. `RoomsView.test.tsx` updated for `counts` and the expanded row. |
| **RA3** | `rooms-reader` role: knob, `RequireAnyRole`, route table roles, chart value, `/me` (D1, D9) | `auth` test: `RequireAnyRole` admits either role, refuses neither, panics on an empty list. API test: a token with only `rooms-reader` gets 200 on the two room `GET`s and `/me`, and **403 on every other route in the table** (the R48 table makes this a loop, not a list); a token with neither role is 403 everywhere; `helm template` renders the flag from `oidc.roomsReaderRole`. |
| **RA4** | Activity events: migration `0002`, event types and payload keys, the watcher, janitor retention, webhook filter end to end (D6, D7, D8) | Migration: `admin-schema-compat` job passes (the previous release runs against the migrated schema); `0002` is immutable once merged (CI already enforces). Watcher test with a scripted sequence of snapshots: join → `room.participant_joined` with `nickname`; detach → `room.detached` with `label`; a join and leave inside one tick → no event; an adoption (all IDs change, nicknames identical) → no events; the first tick after start → no events. Dispatcher test: a webhook with `events: null` receives a `ban.created` and **not** a `room.participant_joined`; one with `events: ["room.participant_joined"]` receives only that; a config-sourced webhook's `events` is rendered from the chart and `PUT` → `409`. Payload test: no key of the activity payload matches a broadcast ID or a room code fixture (the R39 assertion extended). Janitor: activity rows older than the retention are pruned, moderation rows never. Events view filter test. |
| **RA5** | OpenAPI revision, self-hosting §9.8 and §10, docs/44 §4.11 note, `docs/gotchas.md` if anything earned it, this document's status, the ROADMAP row and entry | R48's `TestOpenAPIMatchesRoutes` and `redocly lint` green with the new operations, schema and webhooks. The self-hosting recipe executed once against the dev stack's fake IdP (`cmd/gawk-fakeidp` minting a `rooms-reader` token) and once against Keycloak on the reference deployment. |

Success criterion, end to end, on the reference deployment: a
client-credentials token carrying only `rooms-reader` lists rooms and
fetches one that has two attached broadcasts and three participants,
seeing nicknames, labels, live state and a join link that opens the room;
the same token gets 403 from `/api/v1/broadcasts`; a webhook subscribed
to `room.participant_joined` receives a signed delivery within ten seconds
of a fourth person joining, with a nickname and no broadcast ID or room
code anywhere in the body; the ops pager webhook, untouched, receives
nothing from any of it; and `/statusz` on every relay pod is byte-identical
to before.

## 5. Security considerations

- **Two new places carry raw room codes and broadcast IDs**: the relay's
  `/internal/admin/rooms` (ClusterIP-only, credential-gated, `no-store`)
  and `gawk-admin`'s two room `GET`s to the `rooms-reader` role (OIDC,
  role-gated). Both are scoped relaxations on the docs/42 D8 terms and
  marked `x-gawk-sensitive` in the document. **Neither returns an IP.**
- **A `rooms-reader` token is a join capability for every room in the
  deployment.** Its holder can watch and join anything; it cannot kill,
  ban, end a room, read bans or reasons, or see publisher addresses. The
  self-hosting text says this in one sentence next to the recipe.
- **Webhook payloads stay free of raw IDs, codes and IPs.** The two new
  keys are display text (nickname, label). The existing assertion that no
  payload key can carry a raw identifier is extended to the activity
  events, and receivers that never opted in see no change at all (D8).
- **The watcher is a poller on the leader with a 2 s-cached scan behind
  it**; its load on the relay fleet is bounded by the cache, not by the
  interval, and a bot polling the API adds nothing beyond the cache.
- **Nicknames are unauthenticated free text chosen by participants**
  (docs/44 D10). They reach webhook receivers and the bot verbatim; the
  SPA already renders them escaped, and the bot's repository owns its own
  rendering. The `identity` field is empty until a later milestone and is
  documented as such.

## 6. Operations

- **Turning it on**: rooms must be enabled on both charts (docs/self-hosting
  §10); grant `rooms-reader` to a service account in the IdP (§9.8); give
  the bot's webhook an `events` list. Nothing else changes for a
  deployment that does neither.
- **A room shows `live: false` with participants you know are there**: the
  home pod's ops listener is unreachable from `gawk-admin` — the relays
  view shows the pod with its error, exactly as for broadcasts.
- **Events arrive late or paired**: the watcher interval (default 5 s)
  and the adoption rule (D6). A home-pod move during a rollout produces no
  events by design; a missed sub-interval join is expected.
- **The Events view is quiet but the bot is busy**: the category filter
  defaults to `moderation`; switch it.
