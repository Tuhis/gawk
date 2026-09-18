# R50 — Relay event bus over NATS JetStream (docs/51)

**Status**: designed 2026-09-16; **shipped 2026-09-18** (EB1–EB5). Chunks **EB1–EB5** (`EB` =
Event Bus). Touches `gawk-server` (a publisher package, hooks at the
existing fan-out points, knobs, chart values), `gawk-admin` (a consumer,
one migration, the events feed, chart values) and the docs. No wire
change, no media-path change, nothing on the public relay listener.
**Optional everywhere and off by default**: a deployment without NATS is
byte-identical to today. R49 ([docs/50](50-rooms-read-api.md)) **depends on
this milestone** for its activity events; with the bus off, R49's read API
still works and its activity webhooks simply never fire. **The message
format is R51's** ([docs/52](52-event-contract.md), revised 2026-09-16):
CloudEvents 1.0 structured JSON, one JSON Schema per type, an AsyncAPI
catalogue; EB1 depends on its EC1.

## 0. What shipped (2026-09-18)

As designed, with four things worth recording because a reader of the design
alone would look for them in the wrong place:

- **The contract package is R51's, shipped before this** ([docs/52](52-event-contract.md),
  EC1–EC4). `internal/eventbus` imports `gawk-server/events` for the envelope
  and the data types and encodes nothing itself; EB1 shipped no schemas.
- **Attachment events are published from `attachLocked`, not from
  `broadcastLocked`.** The participant-facing funnel misses exactly the attaches
  with no participants to notify — a mint's first broadcast, an adoption's
  re-attach — and a consumer that learned about streams only there would never
  see them.
- **A re-home is a first-class fact, not an inference — and the contract grew
  to say it.** D9 asked for `reason: home_moved` on the departures; R51's
  first cut of `room.participant_left` had no `reason` at all, so R50 added
  one (`left` | `timeout` | `room_ended` | `home_moved`) and a **new bus type**,
  `room.home_changed`, published by the pod that ADOPTS the room. Both are
  additive under docs/52 D6 (b), and both landed while nothing consumed the
  contract, which is the window for it.

  Why the new type rather than the departures alone: a pod that loses a room
  because it is being *deleted* may publish nothing at all, so the old side is
  best-effort. The adopting pod is the one participant in a re-home that is
  certain to be alive, and its `source` names the new home. A consumer
  tracking where a room lives follows that.

  What a re-home looks like on the bus: zero or more
  `room.participant_left{reason: home_moved}` from the old home, then
  `room.home_changed` from the new one, then `room.attached` per stream and
  `room.participant_joined{rejoin: true}` as the people reconnect. No
  `room.closed` and no `room.opened` — the room never stopped.

  The reasons come from the room and the session rather than from each caller:
  `ReleaseHome` and `EndRoom` mark the room, an eviction marks the session, and
  the one leave path attaches whichever applies when the session actually goes.
  The room's reason outranks the session's — "the room ended" explains a
  departure better than "its control queue overflowed".
- **The feed's `type` vocabulary split in two.** R51 holds
  `store.AllEventTypes()` equal to the contract's moderation row table, so the
  ingested activity types live in `store.ActivityEventTypes()` and
  `store.FeedEventTypes()` is the union the `?type=` filter and the OpenAPI
  enum use. An activity row is never delivered to a webhook, which is why it
  needs no CloudEvents mapping of its own.

## 1. Purpose

Everything `gawk-admin` knows about what is happening on the relays right
now, it learns by asking: `relayscan` scrapes every pod's ops listener on
demand (2 s cache), and the reconciler sweeps `Room` CRs once a minute to
notice that a room ended. That is fine for a portal a human refreshes. It
is the wrong shape for *events* — "a person joined this room", "this
broadcast went away" — because the only way to turn a scrape into an event
is to scrape periodically and diff, and the first draft of R49 did exactly
that: a leader-side poll of the merged room view every five seconds.

The owner rejected periodic polling outright (2026-09-16). The relay knows
the moment a participant joins, an attachment goes away, a publisher
starts, stalls or ends; it already fans each of those out to room
participants over the control stream and, in cluster mode, to the `Room`
CR. Those same points can publish to a bus. This milestone is that bus:
**the relay publishes lifecycle events to NATS JetStream; `gawk-admin`
consumes them.** Nothing polls.

Owner decisions, 2026-09-16:

- **Scope: everything** — room lifecycle, attachment and participant
  changes, broadcast lifecycle, *and* viewer-count and stall deltas.
- **JetStream, at-least-once**: a persisted stream, a durable consumer;
  a portal restart or a leadership move loses nothing.
- **NATS is operator-provided**, URL and credential in the values files,
  the Postgres posture. Default off.
- **R49 keeps its events and takes them from here**; R49 therefore
  depends on R50.

What already exists, so the reader does not go looking:

- **Room fan-out points**: `roomsrv.Registry.broadcastLocked` emits every
  `wire.RoomEvent` (`RoomEventParticipantJoined/Left/Updated`,
  `AttachmentAdded/Removed/Updated`, `RoomEnding`) to participants; the
  `Options.OnRoomEnded / OnRoomEmpty / OnAttachmentsChanged` callbacks
  (`registry.go:142-148`) already carry the same transitions to the
  cluster store. `Refresh()` (`registry.go:1275`) is the one *internal*
  poll — attachment live/viewer state, on an interval, inside one process
  — and is out of scope: it feeds `AttachmentUpdated`, which becomes the
  viewer-count delta on the bus.
- **Broadcast lifecycle points**: hub registration and publisher session
  start/reclaim/end in `internal/hub` and `internal/transport`;
  `Options.OnPublisherStalled` (`hub.go:323`) for the away/back
  transition; the R18 `ViewersGlobal` computation (`hub.go:456`).
- **Identifiers**: `Registry.ObfuscateID` / the room `Obfuscate` option
  give the fleet's HMAC'd key for any ID or code; the `Room` CR's
  `status.key` is the same digest.
- **A dependency-containment test** for the relay's one auth library
  (`internal/ops/auth_import_test.go`): a source walk that fails the
  moment a forbidden import appears outside its one sanctioned package.
- **In `gawk-admin`**: leader election (`internal/kube/leader.go`, the
  reconciler and the dispatcher run on the leader), the `moderation_events`
  table and the dispatcher that turns an insert into signed webhook
  deliveries (docs/42 §4.10), the reconciler's room sweep
  (`kube/reconcile.go:155`) that this milestone makes redundant.

## 2. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **The relay publishes to NATS JetStream from a new `internal/eventbus` package, fed by the existing fan-out points through a non-blocking, bounded channel.** Every hook call does `select { case ch <- ev: default: dropped++ }` and returns; a goroutine drains the channel and publishes with `PublishAsync`, tracking acks. Media, control-stream fan-out and CR writes never wait on NATS. Overflow and unacked publishes are counted (`gawk_eventbus_dropped_total{reason}`) and logged at warn with a rate limit. | The relay's first rule is "drop rather than stall" (CLAUDE.md) and it applies to its own telemetry. At-least-once is the *bus's* promise from the moment a publish is acked; before that point the relay owes nothing that would cost a frame. A consumer that needs the truth after a drop reconciles against the read API (R49) — which is the contract R49 D6 states. |
| D2 | **One stream, `GAWK_EVENTS`, subjects `gawk.<scope>.<key>.<type>`**, where `<scope>` is `broadcast` or `room`, `<key>` is the fleet's HMAC'd key (never a raw ID or code), and `<type>` is the event type's last segment. Stream retention `limits`, `max_age` 24 h, `max_bytes` from values (default 256 MiB), `discard old`, replicas from values. **`gawk-admin` creates and updates the stream and its consumer at start-up** (idempotent `AddStream`/`UpdateStream`); the relay only publishes. Every message carries two NATS headers: `Nats-Msg-Id` (D3) and `Content-Type: application/cloudevents+json`, the CloudEvents NATS binding's structured-mode signal. A publish with no stream yet (`nats: no response from stream`) counts as a drop. | Subjects show up in NATS monitoring, server logs and any operator's `nats sub '>'`; the HMAC'd key is what belongs there (docs/44 D16, docs/42 D8), and it is also what the subject filter a consumer would want keys on. The consumer owns the stream because it owns the retention and the durable consumer's semantics; the relay's NATS user then needs only publish permission on `gawk.>` (D6). Limits retention with a day of history is what lets a consumer that was down for an hour catch up, and what bounds the disk an idle stream takes. |
| D3 | **Every message is a CloudEvents 1.0 event in JSON structured format, under the R51 contract** ([docs/52](52-event-contract.md) D1): `id` = `<pod>:<seq>` (`seq` per-pod, monotonic, starting at a random offset on process start), `source` = `/gawk/relay/<pod>`, `type` = `fi.ioio.gawk.<scope>.<event>`, `subject` = the HMAC'd key, `time`, `dataschema` = the type's schema `$id`, and `data` typed per the schema in `gawk-server/events`. *(Revised 2026-09-16; the first draft defined a `gawk.event.v1` envelope with `schema`, `type`, `occurredAt`, `pod`, `seq`, which maps onto these attributes one to one.)* **`data` carries raw broadcast IDs and room codes** (the bus is internal infrastructure on the k8s-API tier, D6), never publisher IPs; those properties are marked `x-gawk-sensitive` in the schema and stripped from webhook deliveries (docs/52 D4). The `Nats-Msg-Id` header equals the CloudEvents `id`, which is what JetStream deduplicates on inside its 2 min window and what `gawk-admin` deduplicates on forever (D5). Event types (the `<event>` segment; every one has a schema file, a golden vector and an AsyncAPI message per docs/52 D6 (d)), with what `data` carries — *R51 fixed the property names on 2026-09-17: `broadcastId` (sensitive) / `broadcastKey`, `roomCode` (sensitive) / `roomKey`, `displayCode` (sensitive), `role`, `startedAt`, `createdAt`, `viewersLocal` / `viewersGlobal`, `label`, `live`, `viewers`, `participantId`, `nickname`, `clientKind`, `streaming`, `speaking`, `rejoin`, and closed `reason` vocabularies; the Go structs are in `gawk-server/events/data.go` and EB2 fills them*: | The `Ban` and `Room` CRs already carry raw IDs on the internal tier and nothing has argued for hashing them there; a consumer that must render a join link (R49) needs the code. IPs are omitted because no consumer needs them and the portal's scan already has them behind the operator role. Per-pod `seq` plus `pod` is a dedup key that needs no coordination; the random start offset is what keeps a restarted pod from re-issuing the IDs it used a minute ago. The envelope decision — and why it is CloudEvents rather than the draft's own — lives in docs/52 so the bus and the webhooks cannot drift apart on it. |
| | `broadcast.started` (id, key, role origin/edge, startedAt), `broadcast.publisher_away` / `broadcast.publisher_back` (from `OnPublisherStalled`), `broadcast.ended` (id, key, reason: gc/killed/replaced), `broadcast.viewers` (id, key, viewersLocal, viewersGlobal; **coalesced to at most one per broadcast per `-eventbus-viewer-interval`, default 5 s, and only on change**); `room.opened` (code, kind, displayCode, key, createdAt), `room.closed` (code, key, reason: grace/creator/operator), `room.attached` / `room.detached` (code, key, broadcastId, label), `room.attachment_updated` (live, viewers — same coalescing), `room.participant_joined` / `room.participant_left` / `room.participant_updated` (code, key, participantId, nickname, clientKind, streaming, speaking). | The bus's room lifecycle types are **deliberately not** `room.created` / `room.ended`: those are `store.Event*` moderation types (`store.go:136-137`) recorded inline by the API with the operator as actor, and a bus type with the same string would make one webhook `type` mean two categories and two payload shapes (R48 D3 (d)). Rule: **no bus type string is ever a `store.Event*` moderation type**; docs/52 D6 (e) states it for every channel and docs/52 D7 asserts the two sets are disjoint. Viewer deltas were the owner's explicit ask; coalescing is what keeps a thousand-viewer broadcast from being a thousand messages a second while still giving a dashboard a five-second-fresh number. Every other type is a discrete transition and is published as it happens. |
| D4 | **Which pod publishes what.** Room events: the **home pod** only (it is the one that emits them to participants; proxies see nothing). Broadcast events: the **origin** pod; an edge pod publishes nothing about a broadcast it pulls, except its own `viewersLocal` inside `broadcast.viewers`, which the origin does not know — so `broadcast.viewers` is the one type both roles publish, distinguished by `role`. | One publisher per fact, or a consumer sees every transition once per pod. The origin/edge split is the R17 vocabulary (`/statusz` `role`), and `viewersGlobal` is already computed on the origin from the edge leases (R18). |
| D5 | **`gawk-admin` consumes on the leader** with a durable pull consumer (`gawk-admin`, explicit ack, `AckWait` 30 s, `MaxDeliver` unlimited, `DeliverPolicy` all on first creation) and **ingests each message as a row in `moderation_events` with `category = 'activity'`** and a `source` column holding the CloudEvents `id` (= `Nats-Msg-Id`), `UNIQUE`; a duplicate insert is a no-op and still acks. Viewer/attachment-updated deltas are **not** stored (they update an in-memory live view, D7, and ack); they are bus-only types in `internal/eventbus`, not `store.Event*` constants. The row's `payload` carries the whole CloudEvent verbatim (raw IDs allowed in the portal-only jsonb, as today) and a `summary` from `store.SummarizeActivity`. **Two bus types are mapped, not ingested as themselves**: `room.closed` becomes exactly the row the reconciler's sweep writes today — moderation `room.ended`, actor `system`, `roomPayload`, deduplicated through `RoomEndedSince` against an operator-ended row (`api/rooms.go:290`) — and `room.opened` becomes an `activity` row of that type (today nothing records a dynamic room's birth; a static room's `room.created` stays the operator's moderation row, so an operator-created room has one row of each, different types, no dedup needed). **The reconciler's room sweep is retired** when the bus is on: the sweep's `room.ended` now arrives from the relay, with its reason, through that mapping, so a webhook with no filter keeps receiving it byte for byte (R49 D8); with the bus off the sweep runs as before. Retention: the janitor prunes `activity` rows older than `-activity-retention` (default `72h`). | Leader-only keeps ordering per subject and reuses the election the dispatcher already runs on; a work-queue across two replicas would need a second ordering story for no gain at this volume. The `UNIQUE` source is what turns at-least-once into exactly-once at the table. Storing deltas would be a write per five seconds per live thing, for data nobody audits. Retiring the sweep is the point of the milestone — one fewer poll — and keeping it behind the off switch is what makes "off is byte-identical" true. |
| D6 | **NATS is operator-provided and default off.** Relay: `-eventbus-url` (`GAWK_EVENTBUS_URL`, empty = off), `-eventbus-creds-file` (an NKey/JWT `.creds`; chart `eventbus.credsSecretRef`), `-eventbus-subject-prefix` (default `gawk`), `-eventbus-viewer-interval`; all through `registryOptions`. `gawk-admin`: `-eventbus-url`, `-eventbus-creds-file`, `-eventbus-stream` (default `GAWK_EVENTS`), `-eventbus-max-bytes`, `-eventbus-replicas`, `-activity-retention`. **The relay's NATS user has publish-only permission on `gawk.>`; `gawk-admin`'s has JetStream API and consume permission on the stream; neither has anything else.** Self-hosting gets the `nats-io/k8s` Helm recipe with JetStream enabled and the two accounts, and a `nats sub` one-liner to watch the bus. TLS to NATS is on by default (`tls://`), with the platform trust store; `-eventbus-insecure` exists for the docs/41 compose lane only, warns at startup, and is not a chart value. | The Postgres posture (docs/42 §4.13): the charts do not own a stateful thing's upgrade story. Permission-scoped users mean a relay compromise can publish noise but not read the stream or reconfigure it, and a portal compromise (docs/self-hosting §9.7) cannot impersonate a relay on subjects it does not consume from anyway. Everything through `registryOptions` is docs/44 D17 / the R2 lesson. |
| D7 | **A bus-fed live view in `gawk-admin` is the hook, not the deliverable.** The consumer keeps the last `broadcast.viewers` and `room.attachment_updated` per key in memory; nothing reads it in this milestone except a `bus` section on `GET /api/v1/relays` (last message time per pod, lag, stream state). `relayscan` stays the source for `/broadcasts` and `/rooms`, on request, 2 s cache — that is request-driven, not periodic, and it is the ground truth the bus is measured against. | Replacing the scan with a bus-fed cache changes the portal's truth model (a scan that fails says so per pod; a cache that stopped receiving looks like a quiet fleet) and deserves its own milestone with that failure mode designed. The `bus` section exists so an operator can see the bus is alive before R49's webhooks depend on it. |
| D8 | **Dependency containment, like the OIDC rule.** `github.com/nats-io/nats.go` may be imported by exactly one relay package, `internal/eventbus`, and one `gawk-admin` package, `internal/eventbus`; a source-walk test in each module (the `auth_import_test.go` shape) fails on any other importer. `internal/eventbus` in the relay imports nothing from `transport`, `hub` or `roomsrv`; it takes `Event` values on a channel. | The relay's data plane dependency set is a security property (`ops/auth.go` header comment). A NATS client is a network client with reconnect logic and TLS; it belongs in one place with one owner, behind a channel, where the media path cannot reach it and it cannot reach the media path. |
| D9 | **Ordering and gaps are documented, not hidden.** Within one pod's publishes, `seq` is total; JetStream preserves publish order per stream. Across pods there is no order and none is claimed. A consumer detects a gap in one pod's `seq` and may reconcile against the read API; `gawk-admin` logs a gap at info with the pod and the size, and R49's webhook payloads carry nothing that depends on the gap having been closed. An adoption (a room's home pod changes) re-issues participant IDs; the new home pod publishes `room.participant_joined` for each rejoining participant with `rejoin: true`, and the old one published `room.participant_left` with `reason: home_moved` when it released — so a consumer that wants to suppress the pair can, and one that does not sees the truth. | The first R49 draft had to *guess* an adoption from a poll diff. The publisher knows; saying so on the event is cheaper than every consumer inferring it. |
| D10 | **Direct subscribers are a supported consumer pattern, opt-in by the operator.** A third consumer (the bot itself, a dashboard) may be given its own NATS user with subscribe-only permission on a subject filter, e.g. `gawk.room.>`. The self-hosting text says what that grants: the same visibility as R49's `rooms-reader` role, raw codes included, minus the join link. | The owner may prefer the bot on the bus to the bot behind a webhook; both work from the same events, and the bus does not need `gawk-admin` at all for that. It is the operator's grant to make, on the IdP-free tier, and the docs say what it is worth. |

### Rejected

- **Periodic polling anywhere in `gawk-admin`** — owner decision
  2026-09-16. This includes the R49 watcher draft and, once the bus is on,
  the reconciler's room sweep (D5). `relayscan` is request-driven and
  stays (D7).
- **Core NATS (fire-and-forget)** — a portal restart would silently lose
  events that webhooks had promised; JetStream's cost is a stream the
  consumer creates once (D2).
- **The relay writing events into Postgres or the k8s API** — the relay
  has no Postgres and must not grow one; CR writes per join would be
  docs/44 D5's rejected write amplification on the API server.
- **A NATS subchart in the relay chart** — the Postgres posture (D6).
- **Raw-ID-free payloads on the bus** — the consumer that needs a join
  link would then need a second lookup per event; the bus is on the
  internal tier where CRs already carry raw IDs (D3). Subjects stay
  HMAC'd.
- **Publishing from every pod that knows about a broadcast** — once per
  fact (D4).
- **Blocking publishes with retry in the relay** — D1; the drop counter
  is the honest signal.
- **Replacing `relayscan` with the bus-fed cache in this milestone** — D7.
- **Kafka, Redis Streams, an HTTP push to `gawk-admin`** — NATS was the
  owner's choice; the HTTP push would make the relay know the portal
  exists (docs/42 D2).

## 3. Where it plugs in

| Piece | Where it is today | What EB changes |
|---|---|---|
| Relay publisher | R51's public `gawk-server/events` (envelope, data types, schemas, catalogue) | **New** `gawk-server/internal/eventbus`: `Publisher` (connect with reconnect, `PublishAsync`, ack tracking, drop counters), `Nop` when off; it imports `events` for the types and `events.Marshal` for the bytes, and adds no encoding of its own; the `nats.go` import lives here only (D8). |
| Room hooks | `roomsrv.Options` callbacks (`registry.go:142-148`); `broadcastLocked` | A new `Options.OnEvent func(eventbus.Event)` called at each `RoomEvent` emission and at mint/end, home pod only (D4); `Refresh()`'s `AttachmentUpdated` feeds `room.attachment_updated` with coalescing in the publisher. |
| Broadcast hooks | `hub.Options.OnPublisherStalled` (`hub.go:323`); registration and publisher-session paths in `hub`/`transport` | `OnBroadcastEvent` beside the stall hook for started/ended/replaced; the R18 viewer computation feeds `broadcast.viewers` through the coalescer; origin/edge rule in the transport (D4). |
| Relay knobs | `registryOptions` in `cmd/gawk-server/main.go`; chart `values.yaml` | The D6 flags, envs and `eventbus.*` values; `config.Sanitized()` renders the creds file as `<set>`. |
| Relay CI | `internal/ops/auth_import_test.go` | A sibling test for `nats.go` (D8); `licenses`/`notices` entries (Apache-2.0). |
| Portal consumer | — | **New** `gawk-admin/internal/eventbus`: stream/consumer ensure, the leader-run consume loop, ingest into `store`, the live view (D7); its own import-containment test. |
| Store + migration | `migrations/0001_initial_schema.up.sql`; `moderation_events` | `0002_activity_events.up.sql`: `category text NOT NULL DEFAULT 'moderation'`, `source text NULL UNIQUE`; `store.AllEventTypes()` grows the stored activity types (`room.opened`, `room.attached`, `room.detached`, `room.participant_*`, `broadcast.*`); R48's `store.WebhookEventTypes()` does **not** grow here (R49 D6 is where four of them become webhook-eligible); `SummarizeActivity`; `ListEvents` category filter; `PruneActivityEvents`. Expand-only; the previous release ignores both columns. |
| Reconciler | `kube/reconcile.go:155` `SweepRoomsOnce` | Skipped when the bus is configured (D5). |
| Relays route / SPA | `internal/api/relays.go`; `RelaysView.tsx`; `EventsView.tsx` | `bus` section on `/relays`; the Events view's category filter (default `moderation`) and the activity rows' rendering. |
| OpenAPI (R48) | `gawk-admin/openapi.yaml` | The `category` query parameter on `/events`, the `bus` object on relays, the activity event types in the `type` filter's enum (`AllEventTypes()`). R48's drift test forces it. |
| AsyncAPI (R51) | `gawk-server/events/asyncapi.yaml`, `schema/` | Every D3 type: a schema file, a golden vector, a message under the `bus` channel with `x-gawk-since: R50`; none under `webhook` (R49 D6 adds four there). docs/52 D7's tests force it. |
| Docs | docs/self-hosting §9 and §10; `docs/gotchas.md` | A new §12 "The event bus (NATS)": the nats-io chart recipe with JetStream, the two accounts and their permissions, the direct-subscriber grant (D10), the `nats sub` one-liner; §9.7 gains the "what a relay's NATS credential yields" sentence. |

## 4. Chunks and acceptance criteria

| Chunk | Scope | Verified by |
|---|---|---|
| **EB1** | Relay `internal/eventbus`: the publisher with bounded channel, async publish, ack tracking, coalescer, `Nop`, knobs through `registryOptions`, import-containment test; the D3 types, schemas, vectors and catalogue entries in `gawk-server/events` (D1, D3, D6, D8; **after R51 EC1**) | Unit tests against an embedded `nats-server` with JetStream (test dependency only; `licenses` job entry): events published in `seq` order with `Nats-Msg-Id` equal to the body's `id` and the `Content-Type` header set; a published body is byte-identical to the type's golden vector for the same fixture; docs/52 D7's catalogue tests green with the new types; a full channel drops and counts, never blocks (asserted with a publisher that is stuck on a never-acking stream: the hook returns in microseconds); a missing stream counts as a drop; the coalescer emits at most one `viewers` per key per interval and nothing when unchanged; the containment test fails on a planted import in `internal/hub` (negative case kept as a `t.Run` against a temp tree). `-eventbus-url` empty → `Nop`, zero goroutines, `/statusz` and metrics byte-identical to the R42 fixtures. |
| **EB2** | Relay hooks: room fan-out points, broadcast lifecycle, stall, viewers; home/origin publish rule (D3, D4) | `roomsrv` tests: join, rename, attach, detach, refresh-driven update, end each produce exactly one event with the documented fields; a proxied session produces none on the proxy pod. `transport`/`hub` tests: a publish/reclaim/GC sequence yields `started`, `ended{reason:replaced}`, `started`, `ended{reason:gc}`; a stall yields `publisher_away` then `publisher_back`; an edge pod publishes only `viewers{role:edge}`. Adoption test (`roomcluster`): old home publishes `participant_left{reason:home_moved}` for each, new home publishes `participant_joined{rejoin:true}` for each (D9). `go test -race ./...` green. |
| **EB3** | `gawk-admin` consumer: stream/consumer ensure, leader-run loop, ingest with `source` dedup, deltas to the live view, migration `0002`, retention, sweep retirement, `bus` on `/relays` (D2, D5, D7) | Tests against the embedded server: the stream is created with the documented limits and a second start updates rather than fails; two replicas, one leader, every message ingested once; a redelivered message (ack withheld past `AckWait`) inserts no second row; deltas insert nothing and ack; `room.closed` inserts a moderation `room.ended` with actor `system`, and inserts nothing when the API recorded the operator's `room.ended` inside the dedup window; the bus type set and `store.Event*` moderation types are disjoint; a consumer restarted with `DeliverPolicy all` on a fresh durable re-reads and dedups; the janitor prunes `activity` older than the retention, never `moderation`; with `-eventbus-url` set, `SweepRoomsOnce` is not scheduled (and is, without it). `admin-schema-compat` green; `0002` immutable once merged. |
| **EB4** | Events view category filter, activity rendering, OpenAPI revision, chart values for both components, `nats-io` recipe (D6, D10) | `EventsView.test.tsx`; R48's drift test, `redocly lint` and `asyncapi validate` green; `helm template` for both charts renders the flags from `eventbus.*` and mounts the creds Secret; the dev stack (docs/41) gains an optional `nats` profile with JetStream and `-eventbus-insecure`, and its CI job round-trips one `room.participant_joined` from a synthetic room into the portal's Events view. |
| **EB5** | Docs: self-hosting §12, §9.7 sentence, `gawk-server`/`gawk-admin` READMEs, `docs/gotchas.md` if anything earned it, this document's status, the ROADMAP row and entry; docs/44 §4.11 dated note | Review; the recipe executed once on the reference deployment: a relay pod's `gawk_eventbus_published_total` climbs, `nats stream info GAWK_EVENTS` shows messages, the portal's Events view shows a `room.participant_joined` within a second of a browser joining a room. |

Success criterion, end to end, on the reference deployment: with NATS
installed per §12 and both charts pointed at it, a viewer joining a room
appears in the portal's Events view within one second, a broadcast
starting and ending appears as two events with the right reasons, viewer
counts on the bus change at most every five seconds, `nats sub 'gawk.>'`
shows only HMAC'd keys in subjects and every body validates as a CloudEvent against its `dataschema` fetched from the portal, killing the `gawk-admin` leader loses
no event (the count of `activity` rows after a five-minute session equals
the count of messages in the stream for that window, minus deltas), and
with `eventbus.url` unset on both charts every test fixture that asserts
byte-identical `/statusz`, metrics and chart output still passes.

## 5. Security considerations

- **The bus is internal-tier infrastructure carrying raw broadcast IDs
  and room codes in payloads** (D3) — the same tier as the `Ban` and
  `Room` CRs. Subjects carry HMAC'd keys only. It must never be routed
  publicly, and the docs say so where the recipe is.
- **Credentials are permission-scoped** (D6): the relay's user publishes
  and nothing else; the portal's user manages one stream and consumes it;
  a direct subscriber's user subscribes to a filter. A relay compromise
  yields the ability to publish plausible events, which the portal
  records as activity — never as a moderation event, never as a ban —
  and which R49's webhooks may forward; that is stated in §9.7.
- **No IP address is ever published** (D3).
- **The relay's dependency surface grows by one client library, in one
  package, behind a channel** (D8), with a test that fails on spread.
- **TLS on by default**; the insecure switch is a flag without a chart
  value and warns at every start.

## 6. Operations

- **Turning it on**: install NATS with JetStream (§12), create the two
  users, set `eventbus.url` and `eventbus.credsSecretRef` on both charts.
  Order does not matter: a relay publishing before the stream exists
  drops and counts; the portal creates the stream at its next start.
- **Is it alive?** `GET /api/v1/relays` `bus` section (last message per
  pod, lag), `gawk_eventbus_published_total` / `_dropped_total{reason}` on
  the relay, `nats stream info GAWK_EVENTS`.
- **Drops climbing on a relay**: the NATS server is unreachable or slow,
  or the stream is missing; the relay is fine and says so — nothing on
  the media path waits for the bus (D1). Consumers reconcile against the
  read API.
- **Events stopped but the relays publish**: the portal's leader lost its
  consumer; the stream retains 24 h, so a restarted leader catches up
  with no loss (D5). If the gap exceeds retention, the missing window is
  gone; R49's consumers reconcile.
- **Turning it off**: unset `eventbus.url` on both charts; the room sweep
  resumes, activity events stop, the stream can be deleted at leisure.
