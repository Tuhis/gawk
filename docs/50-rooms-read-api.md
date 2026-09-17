# R49 — Rooms read API and room activity events (docs/50)

**Status**: designed 2026-09-15, **revised 2026-09-16** (events come from
the R50 bus, not from a poller); **not started**. Chunks **RA1–RA5** (`RA`
= Rooms API). Touches `gawk-server` (one read-only route on the ops
listener, one public type file), `gawk-admin` (routes, a role, the webhook
event filter, SPA) and the docs. No wire change, no media-path change,
nothing on the public relay listener. **Depends on R48**
([docs/49](49-admin-openapi.md)): every route here lands in the OpenAPI
document under R48's drift check, and the first external consumer reads
that document. **Depends on R50** ([docs/51](51-relay-event-bus.md)) for
the activity events: RA4's webhooks forward events the bus delivered;
with the bus off, the read API and the role work and the activity webhooks
never fire. Depends on R42 ([docs/44](44-rooms.md)) for the rooms
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
- **Webhook events** for attach/detach and participant join/leave, so
  the bot subscribes and then fetches detail. **Revised 2026-09-16**: the
  first draft produced them from a leader-side poll of the merged room
  view every five seconds; the owner rejected periodic polling outright.
  The events now come from the relay itself over the R50 bus, and
  `gawk-admin` polls nothing.
- The bot lives elsewhere; gawk ships the API and its OpenAPI description,
  nothing bot-specific.

## 2. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **A new role, `rooms-reader` (`-rooms-reader-role`, `GAWK_ADMIN_ROOMS_READER_ROLE`, chart `oidc.roomsReaderRole`), grants exactly `GET /api/v1/rooms`, `GET /api/v1/rooms/{name}` and `GET /api/v1/me`.** Nothing else. The operator role keeps everything it has. `internal/auth` gains `RequireAnyRole(roles...)`; the R48 route table's `roles` column carries both for those three entries, and `x-gawk-roles` says so in the document. | The R39 role model is one role per capability, not a hierarchy — `flagger` (docs/42 §4.11) grants flag-only rights the same way. A generic `reader` over every `GET` would hand a bot publisher IPs, ban CIDRs and free-text ban reasons it has no use for. Least privilege costs one middleware variant. `/me` is included so the bot can probe its own token the way the SPA does. |
| D2 | **The relay serves `GET /internal/admin/rooms` on the ops listener**, behind the existing `AdminAuth` guard, schema `gawk.admin.rooms.v1`, types in the public `adminapi` package next to `Broadcast`. One row per room **this pod is home for**: code (raw), kind, display code, display name, key, created-at, empty-since, `attachments[]` (broadcast ID, label, live, viewers, attached-at) and `participants[]` (per-room ID, nickname, client kind as `web-viewer`/`web-broadcaster`/`native`, `streaming`, `speaking`, `identity`). Proxy rows are **not** listed (a proxying pod knows only a session count; the home pod is the truth). Built by a new `Registry.AdminRooms()` walking `r.rooms` under the lock, the way `AdminStats()` walks hubs. `/statusz` stays byte-identical. | The roster exists in exactly one place, and this listener is the sanctioned place for raw identifiers (docs/42 D8: ClusterIP-only and credential-gated). Reusing `adminapi` means `relayscan` compiles the same struct instead of mirroring twelve JSON tags (CLAUDE.md). Kill/end verbs stay absent here exactly as kill/ban do for broadcasts: the `Room` CR is the write path. |
| D3 | **`gawk-admin` merges two sources per room: the CR list (spec, key, lease, managed, attach-secret gating) and the fleet scan (live roster and attachment state from the home pod).** `relayscan.Snapshot` gains `Rooms []RoomAggregate`, scraped from every pod with the same 2 s cache; the merge keys on the normalized code (CR name = the relay's normalized code). A room whose home pod did not answer, or that no pod has homed, renders from the CR alone with `live: false`, an empty roster and the CR's attachment list without live state. | The CR is durable and complete for static facts; the scan is live and partial by design (one dead pod degrades itself, never the aggregate — `relayscan`'s first property). Rendering the union with an explicit `live` flag tells the consumer which half it is looking at, instead of an empty roster that could mean "nobody" or "unknown". |
| D4 | **Two routes, one resource shape.** `GET /api/v1/rooms` returns summaries: the R42 fields plus `live`, `links.join`, and `counts {participants, streaming, watching, attachments}`; the R42 integer `attachments` becomes `counts.attachments` (the SPA's `RoomsView` is updated in the same chunk — the only consumer). `GET /api/v1/rooms/{name}` returns the same object plus `attachments[]` and `participants[]` in full. `{name}` accepts any spelling of the code (`rooms.NormalizeCode`, as the write routes already do). `links.join` is `<appBaseUrl>/#/room/<displayCode>`, omitted when `-app-base-url` is unset — the shape `links.watch` already has on broadcasts. Attachments carry `links.watch` too. | A bot lists rooms once and fetches one room when an event names it; rosters of up to fifty on every list row would make the list *be* the detail. One schema with two projections keeps the OpenAPI document honest (R48 D3 (f) decodes both into the same Go type). The rename is the one non-additive change this milestone makes, and it is made *before* the document has an external consumer: R48 ships the document, R49 revises it in the same PR that reshapes the route, and after R49 the additive-only promise (R48 D9) applies. Re-attaching a count field beside an array under the same name was the alternative, and it would have been the lie the drift check exists to catch. |
| D5 | **The raw room code and the join link are returned to `rooms-reader` callers; broadcast IDs in `attachments[]` are returned too.** Marked `x-gawk-sensitive` in the document with the reason. Nothing on these routes carries an IP. | Owner decision: the bot's whole purpose is to hand people a way in. A room code is a joinable secret (docs/44 D16) and this is the second scoped relaxation of that invariant after the operator portal (docs/42 D8), on the same terms: authenticated, role-gated, never in a webhook (D7). An attachment's broadcast ID is what makes `links.watch` and the room's own tiles work; hiding it while returning the room code would protect nothing. |
| D6 | **Four activity events for webhooks**: `room.attached`, `room.detached`, `room.participant_joined`, `room.participant_left` — **the R50 bus events of those names, as `gawk-admin`'s consumer has already ingested them** into `moderation_events` with `category = 'activity'` (docs/51 D5). This milestone adds nothing that produces them: it makes the dispatcher forward them to webhooks that opted in (D8), with the payload projection in D7. `room.participant_updated` (stored, portal-only) and the viewer/attachment deltas (not even stored, docs/51 D5) are not webhook events: `store.WebhookEventTypes()` (R48 OA1) is the moderation types plus these four; the dispatcher and the filter's validation read that one helper, and docs/52 D7 holds it equal to the AsyncAPI `webhook` channel's messages, so a type outside it cannot be forwarded, listed or documented. An adoption arrives as `participant_left{reason: home_moved}` followed by `participant_joined{rejoin: true}` (docs/51 D9); the dispatcher **drops both halves** of that pair by default and forwards them when the webhook lists `room.participant_rejoined` explicitly. *(Revised 2026-09-16; the first draft's five-second poll-and-diff watcher is withdrawn — owner decision: no periodic polling.)* | The relay knows the moment a participant joins; asking it every five seconds and diffing was the only way to get an event from a scrape, and the owner would rather have the bus. With events arriving from the producer, the two things a poll could only guess — a sub-interval join and a home-pod move — are simply stated on the event. Rename and delta events stay off the webhook path because a bot announcing every viewer-count tick is noise; the read API has the numbers. |
| D7 | **An activity webhook delivers the bus event itself**, as a CloudEvent under the R51 contract ([docs/52](52-event-contract.md) D1, D5): same `id`, `source`, `type`, `subject` (the room's HMAC'd key) and `time` as the NATS message, and `data` = the bus event's `data` **minus every property the schema marks `x-gawk-sensitive`** (docs/52 D4) — so the room code and any broadcast ID are stripped and `nickname`, `label`, `clientKind`, `streaming`, `speaking`, `rejoin` and `reason` remain. `portalUrl` and `summary` are added by the projection as they are for moderation events. **Never a broadcast ID, never a room code, never an IP** — the R39 rule, now enforced by the schema marks and the docs/52 D7 fixture test over every type. *(R51 shipped 2026-09-17: the schemas name the properties `roomCode` / `roomKey`, `broadcastId` / `broadcastKey`, `displayCode`, `participantId`, `nickname`, `clientKind` (`web-viewer` \| `web-broadcaster` \| `native`), `streaming`, `speaking`, `rejoin`, `label`, `live`, `viewers` and `reason`; the projection is `notify.project` in `gawk-admin`, and RA4 calls it.)* *(Revised 2026-09-16; the first draft hand-shaped a `gawk.moderation-event.v1` body with two new payload keys.)* The category column, the retention and the Events-view filter are R50's (docs/51 D5) and are not redefined here. | One event, two projections, one place the raw identifiers stop — and that place is declared in the schema a consumer reads rather than in a Go struct only a maintainer reads. `nickname` and `label` are free user text, like `reason` is free operator text — the same posture applies (self-hosting §9.5: send to a channel you would be comfortable having read) — and without them the bot would have to fetch the room on every event to say who arrived. |
| D8 | **Webhooks gain an event filter.** Migration `0003`: `webhooks.events text[] NULL`; `events` in the `-static-webhooks` JSON and the chart's `notifications.webhooks[].events`, and in the portal's webhook form. `NULL`/absent means **every `moderation` event and none of the `activity` ones** — today's behaviour for every existing webhook. A listed set is exact and is validated against `store.WebhookEventTypes()` — which docs/52 D7 holds equal to the AsyncAPI `webhook` channel's message types — plus the one filter-only token `room.participant_rejoined` (D6) — an unknown entry is `400 invalid_request` on the API and a start-up error for `-static-webhooks`. The dispatcher consults the filter when it enqueues deliveries; a `PUT` on a config-sourced webhook's filter is still `409 source_immutable`. | Without a filter, the ops pager on an operator's phone (docs/42 §4.10's motivating receiver) would buzz on every join. Opt-in for the new category makes this milestone invisible to every existing receiver, byte for byte. |
| D9 | **The bot's identity is an IdP-managed service account on the client-credentials grant, with the `rooms-reader` client role.** No gawk-minted token, no static secret in `gawk-admin`. Self-hosting gets the Keycloak recipe (a confidential client with service accounts enabled, the client role assigned to its service-account user, audience mapped) beside the R48 recipe. | The R40 `flagger` design already settled this shape (docs/42 §4.11); a bot is the same kind of caller. Revocation is the token lifetime, as everywhere else in the portal (§4.8). |
| D10 | **What the SPA does with it**: `RoomsView` shows the counts and a `live` mark, and expands a row into the roster and the attachment list (nickname, kind, streaming/speaking, label, live/away, viewers). The Events view gains a category filter, default `moderation`. The webhook form gains the event picker. No new view. | The operator gets the same data the bot gets, from the same route, which is also how the merge (D3) gets exercised by a human before a bot depends on it. |
| D11 | **Knobs**: `-rooms-reader-role` (D1) with its `GAWK_ADMIN_*` env and chart value. That is the only new knob: the event source is R50's and has R50's knobs. Relay side: none — `/internal/admin/rooms` is live whenever `/internal/admin/broadcasts` is, and returns an empty list with `-rooms` off. | docs/44 D17 / docs/42 §4.12 — every knob through the same path. The relay route needs no gate of its own: the credential gate is the gate, and a relay without rooms has nothing to say. |

### Rejected

- **Storing the roster in the `Room` CR** so the CR alone would do —
  docs/44 D5, for the reasons given there (write amplification on every
  join, the API server as a chat backend). Unchanged.
- **A leader-side poll-and-diff watcher in `gawk-admin`** (the first
  draft's D6) — withdrawn 2026-09-16; the owner rejected periodic polling.
  The relay publishes to the R50 bus instead, and the relay still knows
  nothing about the portal (it publishes to NATS, not to `gawk-admin`;
  docs/42 D2 holds).
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
| Room routes | `internal/api/rooms.go` (`handleListRooms`, `roomJSON`, `Rooms` seam) | `roomJSON` gains `live`, `links`, `counts`, `attachments[]`, `participants[]`; `handleGetRoom`; the merge lives in a `roomview` helper both handlers call. `Rooms.List` unchanged. |
| Roles | `internal/auth` `RequireRole` (`auth.go:611`); `internal/config` `-operator-role`, `-flagger-role`; `api.protect` | `RequireAnyRole`; `Config.RoomsReaderRole`; the R48 route table's `roles` per entry; `/me` unchanged (roles are already echoed). |
| Event source | R50's `internal/eventbus` consumer ingests `activity` rows (docs/51 D5) | Nothing. R49 consumes what is already in the table. |
| Store + migration | R50's `0002` (category, source); `store.Event*`; R48's `store.WebhookEventTypes()` | `0003_webhook_events.up.sql`: `events text[] NULL` on `webhooks`; `ListWebhooks`/`UpsertWebhook` carry the filter; `WebhookEventTypes()` gains the four D6 types and nothing else (this is the commit where it first differs from `AllEventTypes()`), and the four messages are listed under the AsyncAPI `webhook` channel in the same commit or docs/52 D7's test fails. |
| Dispatcher | `internal/notify` — after R51 EC2, the CloudEvents projection and Standard Webhooks signing (docs/52 D4, D5); enqueue on insert | Consult the webhook's filter when enqueuing (`NULL` = moderation only); drop the adoption pair unless `room.participant_rejoined` is listed; the activity types go through the same projection as every other type — nothing type-specific is added to the dispatcher. |
| Static webhooks | `-static-webhooks` JSON; chart `notifications.webhooks` | `events` list per entry. |
| SPA | `ui/src/views/RoomsView.tsx`, `EventsView.tsx`, `WebhooksView.tsx`, `api/types.ts` | D10. |
| OpenAPI / AsyncAPI | `gawk-admin/openapi.yaml` (R48); `gawk-server/events/asyncapi.yaml` (R51) | OpenAPI: the two room operations with `x-gawk-roles: [operator, rooms-reader]`, the revised `Room` schema, the `events` field on webhook objects. AsyncAPI: the four D6 messages added under the `webhook` channel (their schemas and bus entries exist since R50). R48's and R51's drift tests force every one of these. |
| Docs | docs/self-hosting §9 and §10; `docs/44` §4.11 | §9.8 gains the `rooms-reader` service-account recipe and the webhook `events` filter; §10 links to the API; §4.11 of docs/44 gets a dated note that the bot-facing read surface shipped here. |

## 4. Chunks and acceptance criteria

| Chunk | Scope | Verified by |
|---|---|---|
| **RA1** | Relay: `adminapi` room types, `Registry.AdminRooms()`, `GET /internal/admin/rooms` (D2, D11) | `roomsrv` test: a room with two participants (one streaming, one not) and two attachments (one away) snapshots to the expected rows, raw code present, `speaking` false, `identity` empty; proxy-only rooms absent. `ops` test: 401/403 exactly as `broadcasts`; `-rooms` off → `{"schema":…,"rooms":[]}`; `/statusz` byte-identical to the R42 fixture (the existing assertion extended to this PR). `Cache-Control: no-store`. `go test -race ./...` green. |
| **RA2** | `gawk-admin`: relayscan rooms scrape, the merge, `GET /rooms` reshape, `GET /rooms/{name}`, `RoomsView` (D3, D4, D5, D10) | relayscan test: a pod that answers `broadcasts` but not `rooms` is `reachable: true` with `roomsErr` set and contributes no rooms. API contract tests: list row has `counts`, `live`, `links.join` (absent without `-app-base-url`); detail carries roster and attachments; a homed room with an unreachable home pod → `live: false`, roster empty, attachments from the CR without `live`/`viewers`; unknown or malformed name → 404; `?name=TuhisRoom` and `tuhisroom` resolve the same CR. `RoomsView.test.tsx` updated for `counts` and the expanded row. |
| **RA3** | `rooms-reader` role: knob, `RequireAnyRole`, route table roles, chart value, `/me` (D1, D9) | `auth` test: `RequireAnyRole` admits either role, refuses neither, panics on an empty list. API test: a token with only `rooms-reader` gets 200 on the two room `GET`s and `/me`, and **403 on every other route in the table** (the R48 table makes this a loop, not a list); a token with neither role is 403 everywhere; `helm template` renders the flag from `oidc.roomsReaderRole`. |
| **RA4** | Activity webhooks: migration `0003`, payload keys, the webhook filter end to end, the adoption-pair rule (D6, D7, D8) — **after R50 has landed** | Migration: `admin-schema-compat` job passes (the previous release runs against the migrated schema); `0003` is immutable once merged (CI already enforces). Dispatcher test over ingested `activity` rows: a webhook with `events: null` receives a `ban.created` and **not** a `room.participant_joined`; one with `events: ["room.participant_joined"]` receives only that, as a CloudEvent whose `data` carries `nickname` and validates against the served schema; `room.detached` carries `label`; a `home_moved`/`rejoin` pair is dropped unless `room.participant_rejoined` is listed; a config-sourced webhook's `events` is rendered from the chart and `PUT` → `409`. Projection test: no property of a projected activity event matches a broadcast ID or a room code fixture, and none is marked sensitive in its schema (docs/52 D7's assertions, run over these four types). Webhook form test for the picker. |
| **RA5** | OpenAPI and AsyncAPI revision, self-hosting §9.8 and §10, docs/44 §4.11 note, `docs/gotchas.md` if anything earned it, this document's status, the ROADMAP row and entry | R48's `TestOpenAPIMatchesRoutes` and `redocly lint` green with the new operations and schema; docs/52's catalogue test and `asyncapi validate` green with the four `webhook`-channel messages. The self-hosting recipe executed once against the dev stack's fake IdP (`cmd/gawk-fakeidp` minting a `rooms-reader` token) and once against Keycloak on the reference deployment. |

Success criterion, end to end, on the reference deployment: a
client-credentials token carrying only `rooms-reader` lists rooms and
fetches one that has two attached broadcasts and three participants,
seeing nicknames, labels, live state and a join link that opens the room;
the same token gets 403 from `/api/v1/broadcasts`; with the R50 bus on, a
webhook subscribed to `room.participant_joined` receives a signed delivery
within a second or two of a fourth person joining, with a nickname and no
broadcast ID or room code anywhere in the body; the ops pager webhook,
untouched, receives nothing from any of it; with the bus off, the same
webhook receives nothing and the read API answers identically; and
`/statusz` on every relay pod is byte-identical to before.

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
- **Nothing in this milestone polls.** The read API scrapes on request
  behind the 2 s cache; the events arrive from the relay over the bus
  (docs/51 §5 covers what the bus itself exposes and to whom).
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
- **No activity webhooks arrive**: the bus is off or unhealthy — check
  the `bus` section of `GET /api/v1/relays` (docs/51 §6) before the
  webhook's `events` list. A home-pod move during a rollout produces no
  webhook events by default (the adoption pair is dropped, D6).
- **The Events view is quiet but the bot is busy**: the category filter
  defaults to `moderation`; switch it.
