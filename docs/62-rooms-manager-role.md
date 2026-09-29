# R60 — The `rooms-manager` role (docs/62)

**Status**: designed and implemented 2026-09-29 (RW1–RW3 in one PR,
gawk-admin 1.6.0); **verified on the reference deployment and against the
compose stack 2026-09-29** (§7). Nothing on the gawk side is open; the
Mumble bot itself lives in its own repository. Chunks **RW1–RW3**
(`RW` = Rooms Write). Touches `gawk-admin` only (kube client, API, config,
chart, fake IdP) and the docs. No relay change, no wire change, no
migration. **Depends on R42** ([docs/44](44-rooms.md)) for static rooms and
on **R49** ([docs/50](50-rooms-read-api.md)) for the role model it extends.

## 1. Purpose

A Mumble bot in its own repository ("a permanent gawk room
per channel") gives every Mumble channel that enables its `gawk` plugin a
static gawk room. On connect it makes sure the room exists (`POST
/api/v1/rooms`, where `409 room_exists` counts as success); at start-up it
deletes the rooms it created for channels that are no longer configured,
unless they are occupied. Rooms it did not create are never touched.

R49 gave that bot the read half — `rooms-reader`. The write half today
needs `operator`, which can also kill broadcasts, ban users and read
publisher IPs, and the bot must never hold it. This milestone adds a second
narrow role that grants **static room creation and deletion, and nothing
else**, and makes gawk-admin enforce the bot's "only what I created" rule
itself rather than trusting the bot's own bookkeeping.

## 2. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **A new role, `rooms-manager` (`-rooms-manager-role`, `GAWK_ADMIN_ROOMS_MANAGER_ROLE`, chart `oidc.roomsManagerRole`), grants exactly `POST /api/v1/rooms`, `DELETE /api/v1/rooms/{name}` and `GET /api/v1/me`.** Not the room reads (that is `rooms-reader`; the bot holds both), not `rotate-secret`, not `end`. The route table names it beside `operator` on those three entries, and `x-gawk-roles` says so. | docs/50 D1's model: one role per capability, no hierarchy. A bot that only provisions rooms needs no roster; one that only reads needs no writes. `/me` is included for the same reason as for `rooms-reader`: a service identity probes its own token there. Secret rotation is left out because the bot creates open rooms and a manager rotating an operator-gated room's secret would lock its users out. |
| D2 | **Every room the portal creates records its creator**: annotation `gawk.ioio.fi/room-created-by` on the `Room` CR, set to the caller's token `sub`. Operator-created rooms are stamped too. The value never leaves the cluster — it is not in any API response, event or log line. | Ownership has to be a fact on the object, not in the bot's storage, or a leaked token (or a bot bug) could delete any room it can name. `sub` is the stable, provider-scoped identifier; `email` is informational (identity.go) and a service account has none. Stamping operator creations costs nothing and keeps the rule uniform. |
| D3 | **A caller holding `rooms-manager` but not `operator` may delete only a static room it created**: decodable, `kind: static`, and `room-created-by` equal to its own `sub`. Anything else is `403` with the new code `room_not_owned`. An operator is unaffected — it deletes any room, as today. A room created before R60 carries no stamp and is therefore operator-only. | This is the bot's own rule, "rooms the bot did not create are never touched",, enforced where it cannot be bypassed. A dynamic room is never created through the API, so it can never be owned — "static rooms only" follows from ownership without a second rule. `403` and a distinct code: the caller is authenticated and the room exists, but this identity may not act on it, which is neither `forbidden` (the role is held) nor a `409` (no state change would make it allowed). |
| D4 | **The ownership check and the delete are one guarded operation**: `kube.RoomClient.DeleteExisting` takes a check function, reads the CR, runs the check, and deletes with a **UID precondition**. The `end` route's dynamic-only check moves into the same guard. | Checking in the handler and deleting in a second call would let a CR deleted and re-created under the same name between the two be deleted unchecked. The UID precondition closes that; a `resourceVersion` precondition would not be usable, since the home pod updates the room's status (lease, attachments) continually. A spec edit between read and delete that changes the kind is not guarded, and needs cluster write access to make. |
| D5 | **Create is the same operation for both roles**: the same validation (no dynamic-shaped slug, `maxBroadcasts` 0..64, a 128-character display name), `withAttachSecret` allowed, `409 room_exists` for a taken code whoever holds it. | The bot creates open rooms, but a manager minting a gated room is harmless and a second create path would be a second thing to keep in sync. `409` for a room the caller does not own is what the bot wants too: it treats an existing slug as provisioned, and D3 then keeps it from deleting a room an operator made. |
| D6 | **Knobs**: `-rooms-manager-role` (D1), default `rooms-manager`, with its `GAWK_ADMIN_*` env and chart value; `off` (or the chart value empty) grants it nowhere, and the served OpenAPI document then drops it. That is the only knob. | docs/50 §7 and R59's convention: an empty `GAWK_ADMIN_*` variable reads as unset, so "off" is spelled `off`. |
| D7 | **The fake IdP's service client carries both bot roles** (`-service-role` becomes a comma-separated list, default `rooms-reader,rooms-manager`), so the compose stack and the kind e2e tier exercise the bot's real token shape. | The dev stack exists so a consumer can be built against it; a provisioning bot needs create and delete there, not just reads. |

### Rejected

- **Granting `rooms-reader` the writes, or making `rooms-manager` imply
  reads** — D1; a role hierarchy is what the R39 model avoids.
- **An ownership list in `gawk-admin`'s Postgres** — the `Room` CR is the
  room's only durable record (docs/44 D20: no row ahead of the CR); an
  annotation lives and dies with the object it describes.
- **Keying ownership on the OIDC client ID (`azp`) instead of `sub`** —
  both are stable for a Keycloak service account; `sub` is already what
  every other identity check and audit row uses, and works for providers
  that do not issue `azp`.
- **A per-creator room quota** — deferred. A leaked `rooms-manager` token
  can create rooms until it expires, each one a small CR in the namespace,
  and none of them does anything until someone joins. The token lifetime
  and IdP revocation bound it (self-hosting §9.3). Revisit if a second
  kind of manager client appears.
- **Exposing `createdBy`, or an "owned by you" flag, on the room
  objects** — not needed by the bot, which records what it created. It
  can be added later without breaking the contract (additive field).

## 3. Where it plugs in

| Piece | Where it is today | What RW changes |
|---|---|---|
| Kube client | `internal/kube/rooms.go` `CreateStatic`, `DeleteExisting`, `RoomObject` | `AnnotationRoomCreatedBy`; `StaticRoom.CreatedBy` → the annotation; `RoomObject.CreatedBy` read back; `DeleteExisting(ctx, name, check)` with a UID precondition. |
| API | `internal/api/rooms.go` `handleCreateRoom`, `deleteRoom`; `api.go` route table, `resolveRoles`, OpenAPI role map | `RoleRoomsManager`; the three entries; the creator stamp; the owner guard for manager-only callers; `CodeRoomNotOwned`. |
| Config | `internal/config` `-rooms-reader-role` | `-rooms-manager-role`, `Config.RoomsManagerRole`, logged in the start-up summary. |
| Chart | `values.yaml` `oidc.roomsReaderRole`, `deployment.yaml` | `oidc.roomsManagerRole`, `GAWK_ADMIN_ROOMS_MANAGER_ROLE` rendered `off` when empty; CI asserts both renderings. |
| OpenAPI | `openapi.yaml` | `createRoom`/`deleteRoom`/`getMe` gain `rooms-manager`; `room_not_owned` in the `ErrorCode` enum and the status table; `deleteRoom` documents the ownership rule. |
| Fake IdP | `cmd/gawk-fakeidp` `-service-role` | A comma-separated role list (D7). |
| e2e | `e2e/admin-assert.sh` | The bot token carries exactly the two roles and is still 403 on `/broadcasts`. |
| Docs | self-hosting §9.7, §9.8; docs/41; docs/42 knob table; docs/44 §4.11; ROADMAP | The role, its recipe, what a leaked token yields. |

## 4. Chunks and acceptance criteria

| Chunk | Scope | Verified by |
|---|---|---|
| **RW1** | Kube client: creator annotation on create, `CreatedBy` on read, guarded `DeleteExisting` with a UID precondition (D2, D4) | `internal/kube` tests against client-go's fake: a create with `CreatedBy` stamps the annotation and `Get` returns it; a check that refuses leaves the CR (and its Secret) in place and returns the check's error; a nil check deletes as before; the delete request carries the read object's UID as a precondition (a CR replaced under the same name between read and delete is not deleted). |
| **RW2** | API, config, chart: the role, the three route entries, the stamp, the owner guard, `room_not_owned`, the end route through the guard, OpenAPI (D1, D3, D5, D6) | API tests: a token with only `rooms-manager` gets 201 on create and 200 on `/me`, and **403 on every other route in the table** (the R49 loop); it deletes a room it created (204), and gets `403 room_not_owned` for a room another subject created, an unstamped room, a dynamic room and an undecodable one — each left in place; an operator still deletes all of them; `end` still refuses a static room with `409 room_not_dynamic`; the created CR carries the caller's `sub`; `-rooms-manager-role off` grants nothing and the served document drops the role. Config test for the default, the env, and `off`. `TestOpenAPIMatchesRoutes` and `redocly lint` green. CI: `helm template` renders `oidc.roomsManagerRole` and `off` for an empty value. |
| **RW3** | Fake IdP role list, e2e assertion, docs, ROADMAP (D7) | Fake IdP test: the client-credentials token carries both default roles, and a single `-service-role` still yields one. `admin-assert.sh` asserts the bot's `/me` roles are exactly `rooms-reader` and `rooms-manager`. Self-hosting §9.8 gives the recipe; run once against the compose stack (`ADMIN_ROOMS=1`: create, delete, then 403 on an operator's room) and once against Keycloak on the reference deployment. |

Success criterion, end to end, on the reference deployment: a
client-credentials token carrying `rooms-reader` and `rooms-manager`
creates a static room, reads it back with a join link, and deletes it; the
same token gets `403 room_not_owned` deleting a room an operator created
and `403 forbidden` from `/api/v1/broadcasts` and `rotate-secret`; the
operator's audit log shows the bot's subject as the actor of the create and
the delete.

## 5. Security considerations

- **What a leaked `rooms-manager` token yields**: it can create static
  rooms until it expires and delete the rooms that identity created. It
  cannot delete or modify any other room, read a roster, rotate a secret,
  end a dynamic room, or reach anything an operator can. The self-hosting
  text says this beside the `rooms-reader` sentence.
- **A create is an existence oracle for static slugs**: `409 room_exists`
  tells a manager-only caller that a code is taken, and a static room code
  is joinable (docs/44 D16). The bot holds `rooms-reader` too, which lists
  every code outright, so this adds nothing for the intended holder; a
  manager-only token must still guess a slug to learn one.
- **The creator annotation holds a `sub`** — an opaque provider ID, on an
  object only the portal's ServiceAccount and cluster admins can read. It
  is never served.

## 6. Operations

- **Turning it on**: rooms must be enabled on both charts (self-hosting
  §10); assign `rooms-manager` as a client role of the `gawk-admin` client
  to the bot's service account (§9.8). Nothing changes for a deployment
  that does neither.
- **The bot cannot delete a room it made**: the room predates R60 (no
  stamp), or the bot's service account was re-created in the IdP and has a
  new `sub`. An operator deletes it from the portal.
- **Turning it off**: `oidc.roomsManagerRole: ""` in the chart (or
  `-rooms-manager-role off`). Rooms the bot created stay until an operator
  deletes them.

## 7. Verification on the reference deployment (2026-09-29)

Keycloak realm `production`, gawk-admin 1.6.0. The §9.8 recipe was followed
in Keycloak: client roles `rooms-reader` and `rooms-manager` on the
`gawk-admin` client (neither existed before; R49's had not been created
either), and a confidential client for the bot with only the
client-credentials grant, an audience mapper to `gawk-admin`, and both roles
on its service account. Its secret stays in Keycloak until the Mumble bot is
built.

With that client's token, against `gawk-admin.ioio.fi`:

| Request | Result |
|---|---|
| `GET /me` | 200, roles `rooms-manager`, `rooms-reader` |
| `POST /rooms` `r60-bot-test` | 201, with a join link |
| `GET /rooms/r60-bot-test` | 200, `kind: static`, `managed: true` |
| `POST /rooms` again | 409 `room_exists` |
| `DELETE /rooms/tuhistestlab` (a room kept in git) | 403 `room_not_owned`; the CR is unchanged |
| `GET /broadcasts`, `POST …/rotate-secret`, `POST …/end` | 403 `forbidden` |
| `DELETE /rooms/r60-bot-test` | 204; then `GET` is 404 |

`moderation_events` records `room.created` and `room.ended` for the test
room with the service account's `sub` as the actor. The run was repeated
after an unrelated power outage the same evening, with identical results.
That meets the §4 success criterion; what is left is the bot itself, in its
own repository.

### Compose stack (RW3)

The same recipe ran against the docs/41 stack with `ADMIN_ROOMS=1`, as
self-hosting §9.8 describes it: a `gawk-rooms-bot` / `dev-rooms-bot-secret`
client-credentials token from the fake IdP, whose `/me` carries exactly
`rooms-reader` and `rooms-manager`. The bot created `gaming-cs2` (201),
read it back under another spelling (`Gaming-CS2`, 200), got `409
room_exists` creating it again, `403 room_not_owned` deleting a room the
operator created (`op-room`, left in place), `403 forbidden` on
`/broadcasts` and `rotate-secret`, and deleted its own room (204, then 404).
The Events view recorded the bot's creates and deletes under
`service-account-gawk-rooms-bot` and the operator's under its email. The run
used only the admin slice (`docker compose up admin`), as its own compose
project on other ports, beside a stack another worktree had running.
