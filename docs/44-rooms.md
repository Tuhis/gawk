# R42 — Rooms (docs/44)

**Status**: designed 2026-09-03 with the owner (decisions D1–D20 below);
**UI/UX design pass done the same day** (§4.9 revision, §12);
**implemented 2026-09-04** (chunks RM1–RM9, §11). The manual pass on the
reference deployment (§10) is open.

## 1. Purpose

gawk today has exactly one shape: a broadcast, one publisher fanned out to
many viewers, joined by a six-character code. The way it is actually used
is a group of friends sitting in a voice channel while **one or more of
them** stream. Every extra streamer means another code to paste, another
tab, and no notion that the tabs belong together.

A **room** is the missing object: a logical collection of broadcasts that
people join *as participants* rather than as viewers of one stream. The
broadcast itself does not change — `/publish`, `/subscribe`, the keyframe
cache, the DVR ring, the cluster edge-pull path are all untouched. A room
is control-plane state layered on top: which broadcasts are attached,
who is present, and (later) what else the room can do.

This milestone builds the room as that logical construct plus the viewer
and broadcaster experience around it. It deliberately leaves explicit
hooks for two integrations the owner has already decided to want, voice
via a Mumble bridge and room text chat (§4.11), so their arrival is an
addition to the room model rather than a rewrite of it.

**The room is worth having on its own**: multi-POV watching with instant
switching and a roster is the thing the friend group is missing today.
The integrations are why the model is shaped the way it is.

## 2. Decisions (locked with the owner, 2026-09-03)

| # | Decision | Rationale |
|---|---|---|
| D1 | **Rooms are a logical construct over unchanged broadcasts.** A broadcast is attached to at most one room; attaching adds a way in and never removes one — the broadcast's own code keeps working while attached. | Zero change to the media path is what keeps this milestone bounded. Room participants learn attached broadcast IDs from the roster; that is acceptable inside a code-gated room, and it is what makes "open this POV full-screen" a plain `#/view/` link. |
| D2 | **Two kinds of room: static and dynamic.** Static rooms are preconfigured with a stable slug (`TuhisRoom`) and reached by link only. Dynamic rooms are minted on demand with a six-character code and joined exactly like a broadcast, from the same join box. | The owner's split. Static rooms are the "our group's room" that survives everything; dynamic rooms are the zero-setup path for a one-off evening. |
| D3 | **One join box, and the relay resolves a typed code to a broadcast or a room.** Dynamic room codes come from the same alphabet and length as broadcast IDs; the relay guarantees the two namespaces are disjoint at mint time (§4.2). | The viewer never chooses "room or broadcast". A reserved first character was considered (§7) and dropped: it would cut broadcast ID entropy for every broadcast, change `broadcastid.Mint`, and create a rollout window in which a live broadcast and a new room could share a prefix. |
| D4 | **Static rooms are `Room` custom resources** the relay pods watch, following the R39 `Ban` CRD pattern, managed by `gawk-admin` (or `kubectl`), with a file source for non-Kubernetes deployments. | Cluster-safe by construction; survives rollouts and admin outages; the relay keeps working with no admin installed. |
| D5 | **Dynamic rooms are `Room` CRs too, created by the relay at mint** and carrying everything another pod needs to adopt the room: code, kind, creator-token fingerprint, attached broadcasts, and the home-pod lease. **Never the participant list.** | Owner's requirement: a room must survive its home pod. Participants are too dynamic for a CR and they reconnect on their own; the roster is rebuilt from live control sessions, not from storage (§4.5). |
| D6 | **Home-pod ownership by consistent hash with lease fencing; other pods proxy room control to the home.** Media is not proxied — a participant's `/subscribe` sessions go wherever the UDP load balancer sends them, as today. | Same shape as the R17 origin registry (`internal/cluster`): one holder, generation CAS, force-take on a stale lease. Reusing it means one janitor, one informer pattern, one set of failure modes. |
| D7 | **A dynamic room ends when its last participant leaves, after a short empty grace (default 60 s).** Broadcasters count as participants. Static rooms never end; they are empty. | The owner chose "last participant leaves" over "last broadcast detaches" so viewers can wait in a room for the streamer. The grace makes a reload or a pod drain invisible. |
| D8 | **Authority: an attach secret for static rooms, a creator token for dynamic rooms. Viewers are gated by nothing beyond the code or link.** Anyone with a dynamic code may attach; the creator token may detach anyone. | Mirrors `-publish-secret` (fleet-level gate) and the R17 resume token (stateless HMAC, any pod verifies) so no new secret-handling machinery appears. |
| D9 | **Proof of broadcast ownership at attach is the broadcast's resume token.** | It already exists, it is already stateless, and it is exactly the credential that says "I am this broadcast's publisher" (`internal/transport/resume.go`). |
| D10 | **Participants have a nickname per session, no authentication.** Remembered in the browser, unique only within the room (the relay suffixes collisions). | Enough for a roster, chat, and a voice speaker label. Leaves an explicit slot for a stronger identity later (a browser keypair, or a Mumble certificate hash through the bridge) without forcing it now. |
| D11 | **Room view layout: grid of all attached POVs by default, a focus mode (one large, rest small), and a "hide videos" toggle** that keeps the participant in the room with no media sessions at all. Low-rate thumbnails for the non-focused tiles are a listed future enhancement, not v1. | Owner's call. Hide-videos is the "join for control or for a future feature, save bandwidth" mode and it is what the voice-only phone participant will use. |
| D12 | **The broadcaster's room experience is the participant room view plus their own broadcast's controls.** Attaching from the web broadcaster page transitions that page into the room view. | One view, not two products. The Claude Design pass (§12) decides how the controls sit inside the view. |
| D13 | **Native broadcasters attach via a flag/profile field and get an "open room view" action** that launches the browser room view with the broadcaster's room credentials applied. | Owner's choice of the widest option. The native apps get no room UI of their own; the browser is the room UI. |
| D14 | **Room control travels over a new WebTransport route, `CONNECT /room/{code}`, as length-prefixed `wire` records on one bidirectional stream.** Media stays on ordinary per-broadcast `/subscribe` sessions. | Keeps the datagram path byte-identical and the relay a byte forwarder for media. Four new wire types, mirrored in all four implementations with golden vectors (D15). |
| D15 | **New wire types 0x13–0x16 and close code 4007 (`RoomEnded`, terminal)**, allocated in `gawk-server/wire/wire.go` and mirrored in `wire.ts`, `gawk-broadcast/internal/wirecheck`, `gawk-broadcast-windows/crates/wire`. | The convention. The native mirrors need only the attach subset at runtime but carry every vector. |
| D16 | **Room codes are joinable secrets, exactly like broadcast IDs: never exposed raw, never hashed unkeyed.** `/statusz`, metrics and telemetry key rooms by the same per-process HMAC. Static slugs are treated identically even though they are less secret. | The existing invariant, extended. |
| D17 | **Every new knob is plumbed through `registryOptions`**: `-rooms`, `-room-empty-grace`, `-max-rooms`, `-max-room-broadcasts`, `-max-room-participants`, `-room-create-secret`, flags + `GAWK_*` envs + Helm values. `-rooms` defaults **off**. | The R2 lesson. Default-off keeps every existing deployment byte-identical until the operator turns it on; `-room-create-secret` is the hosted-deployment gate that lets dynamic-room creation be a paid or invited feature without touching static rooms. |
| D18 | **Chat and voice are designed as capabilities of the room event model now and built later.** The room snapshot carries a `capabilities` list; the event and command type spaces reserve ranges; the participant record carries a `speaking` flag from day one. The `Room` CR spec has an `integrations` block, empty in v1. | Owner's instruction that the design anticipate significant integrations. §4.11 says exactly what is reserved and why, so the later milestones are additive. |
| D19 | **Typed codes resolve through a new `#/join/<code>` route that tries `/room/` first, then `/subscribe/`.** Links stay unambiguous: `#/view/<id>`, `#/room/<code>`. | The relay is HTTP/3-only, so a browser `fetch` to a resolve endpoint is not dependable; a WebTransport `CONNECT` is. One extra handshake on a typed broadcast code is the price, paid only by the join box. |
| D20 | **The admin portal manages static rooms** (create, rotate the attach secret, delete, and end a dynamic room) through `Room` CRs, and a room ends fleet-wide by CR deletion, which every pod's informer sees. | Same actuation path R39 built for bans; `gawk-admin` already has the RBAC and the CRD tooling. |

## 3. Where it plugs into the existing code (grounded)

- **ID minting**: `gawk-server/internal/broadcastid` owns the 31-character
  alphabet (`23456789ABCDEFGHJKMNPQRSTUVWXYZ`) and `Mint`. Room codes use the
  same package; disjointness is enforced by the room registry (§4.2), not by
  a second alphabet.
- **Resume tokens**: `internal/transport/resume.go` mints a 128-bit truncated
  HMAC-SHA256 over the normalized broadcast ID. The creator token is the
  same construction over the room code with a distinct domain-separation
  prefix, and attach proof is the existing resume token (D8, D9).
- **Cluster registry**: `internal/cluster` implements Lease claim / renew /
  release / force-take with generation CAS fencing and a janitor (docs/22
  W3). The room home-pod lease reuses it; the `Room` CR status carries the
  holder fields in the same shape.
- **CRD pattern**: `gawk-server/moderation` (public package, group
  `gawk.ioio.fi`) plus `deploy/charts/gawk-server/templates/crd-ban.yaml`
  and the informer in `cluster.go`. `Room` gets the same treatment in a new
  public package `gawk-server/rooms` so `gawk-admin` imports it via its
  existing `replace` (never mirrors it).
- **Hub lifecycle**: attached broadcasts are detached when the hub's grace GC
  runs or, in cluster mode, when the origin lease is deleted, both of which
  already have hooks the edge-pull path listens to.
- **Routes**: `gawk-app/src/routing.ts` is pure and unit-tested; `#/room/…`
  and `#/join/…` are two new arms of `Route`. `parseQuery` already carries
  `?relay=` (docs/40), which room links inherit unchanged.
- **Viewer count**: `TypeViewerCount` already reaches every viewer and the
  publisher; the roster shows it per attached broadcast with no new plumbing.
- **Native broadcasters**: `gawk-broadcast` profiles and the Windows GUI
  (docs/38 D12 cards) both have a natural place for a room field, and both
  already hold the resume token they need for attach proof.
- **Admin**: `gawk-admin` already watches and writes CRs, has OIDC roles and
  a webhook path; static room management is one more resource type.

## 4. Design

### 4.1 Concepts

| Term | Meaning |
|---|---|
| **Room** | A named collection of attached broadcasts plus the participants present. Identified by a **code**. |
| **Static room** | Preconfigured; code is a slug (3–32 chars, `[A-Za-z0-9-]`, case-insensitive, stored lower-case, displayed as configured). Reached by link. Never ends. |
| **Dynamic room** | Minted on demand; code is six characters from the broadcast alphabet. Reached by link or typed code. Ends after the empty grace. |
| **Participant** | A control session in a room with a nickname. Both viewers and broadcasters are participants. A participant may hold zero or more media sessions. |
| **Attachment** | A broadcast bound to a room by its publisher. At most one room per broadcast. |
| **Creator token** | A stateless HMAC over a dynamic room's code, returned once at mint. Grants detach-anyone and end-room. |
| **Attach secret** | An optional per-static-room secret required to attach. |
| **Home pod** | The pod holding the room's lease and the authoritative roster. |

### 4.2 Codes and resolution

- Dynamic codes are minted by `broadcastid.Mint` and checked, before the
  `Room` CR is created, against (a) the local hub registry, (b) the cluster
  origin leases (cluster mode) and (c) existing `Room` CRs. The CR create is
  the atomic reservation: a duplicate name is rejected by the API server. In
  single-pod mode the in-memory registry is the reservation.
- Broadcast minting gains the mirror check: `/publish` never mints an ID
  that names a live room. Both checks read the pod-local informer cache;
  the residual race is one cache-propagation window against a 1-in-31⁶
  random collision and is accepted.
- Resolution order is rooms first, then broadcasts, on both `/room/{code}`
  and the SPA's `#/join/<code>` route (D19). A static slug typed into the
  join box is refused client-side (the box accepts six characters), which
  is the "link only" decision made visible.
- The room control session is `CONNECT /room/{code}` with query params
  `name` (nickname), optional `creator` (creator token), optional `attach`
  (attach secret). It answers 404 for an unknown code, 403 for a wrong
  attach secret when one is required for the requested action, 429 at a
  limit, and 451 for a banned client IP (the R39 gate applies).

### 4.3 The `Room` CR

Group `gawk.ioio.fi`, kind `Room`, namespaced, name = normalized code.

```yaml
apiVersion: gawk.ioio.fi/v1alpha1
kind: Room
metadata:
  name: tuhisroom                 # normalized code; dynamic rooms use the 6-char code lower-cased
spec:
  kind: static                    # static | dynamic
  displayCode: TuhisRoom          # as configured; dynamic rooms omit
  displayName: "Tuhis' room"      # optional
  attachSecretRef:                # static only; Secret name + key
    name: room-tuhisroom
    key: attachSecret
  maxBroadcasts: 4                # per-room override of -max-room-broadcasts (optional)
  integrations: {}                # reserved (D18): e.g. mumble: {server, channelPrefix}
status:
  creatorTokenFingerprint: ""     # dynamic only: first 8 bytes of SHA-256(token); never the token
  createdAt: 2026-09-03T18:00:00Z
  attachments:                    # rebuilt by the home pod on adoption
    - broadcastID: 5UP4XW
      label: "tuhis"              # broadcaster-chosen tile label
      attachedAt: 2026-09-03T18:01:00Z
  lease:
    holder: gawk-server-7c9f      # pod name
    addr: 10.42.0.17:4433         # pod IP for internal proxying
    generation: 3                 # CAS fence, same semantics as origin leases
    renewedAt: 2026-09-03T18:05:10Z
  emptySince: null                # dynamic only; set by the home pod when the roster empties
```

Rules:

- `spec` is written by the admin portal or `kubectl` for static rooms and
  by the relay for dynamic ones; `status` is written only by the home pod.
- `attachments` carries raw broadcast IDs. The Kubernetes API is internal
  and the R39 `Ban` CR already carries them; the exposure rule (D16) is
  about public surfaces, and none of this reaches one.
- The participant list is never stored (D5).
- Non-cluster mode: no k8s client, exactly as `-cluster-mode` off behaves
  today. Static rooms come from a file source (`-rooms-file`, the R39
  §4.14 shape); dynamic rooms are in-memory only. Behaviour without
  `-rooms` is byte-identical to today (D17).

### 4.4 Lifecycle

**Dynamic room**

1. **Mint**: only a publisher creates a dynamic room. `CONNECT /room/new`
   carries the resume token of a live broadcast (the same proof as attach,
   D9) and is gated by `-room-create-secret` when set and by `-max-rooms`.
   The relay mints a code, creates the CR with itself as lease holder,
   attaches that broadcast, and returns the code and the creator token in
   the first control record. The minting session is already a participant.
   There is no viewer-side "start a room": a room with nothing attached is
   a static room's empty state, never a dynamic room's first state
   (owner decision, 2026-09-03).
2. **Live**: join, leave, attach, detach flow through the home pod (§4.5).
3. **Empty**: when the last participant leaves, the home pod sets
   `status.emptySince` and starts the empty-grace timer (default 60 s,
   `-room-empty-grace`). Any join during the grace clears it. This is what
   makes a reload, or a pod drain that briefly disconnects everyone,
   invisible.
4. **End**: on grace expiry, or on an explicit end from a creator-token
   holder, or on CR deletion from the admin portal, the home pod closes
   every control session with **4007 `RoomEnded`** (terminal: no client
   reconnect) and deletes the CR. Attached broadcasts are not touched: they
   keep streaming to anyone watching them directly (D1).

**Static room**: exists while its CR does. Empty is a state, not an end.
CR deletion closes sessions with 4007 exactly as above.

**Attachment**: created by a publisher's `Attach` command carrying its
resume token; removed by the publisher's `Detach`, by a creator-token
holder's `Detach`, by the broadcast's hub GC (the publisher has been away
longer than the broadcast grace), or by the room ending. A broadcaster
merely *away* (within the broadcast grace) stays attached and is shown
as such; that is the existing "broadcaster is away" state, surfaced per
tile.

### 4.5 Cluster mode: home pod, proxying, adoption

- **Placement**: the pod that mints a dynamic room is its first home. A
  static room has no home until its first participant arrives; the pod
  that receives that join claims it.
- **Proxying**: a pod receiving `CONNECT /room/{code}` for a room it does
  not hold opens (or reuses) an internal session to the holder's `addr`,
  `CONNECT /internal/room/{code}` with the cluster PSK and the lease
  generation, and pipes control records both ways. The participant's own
  session terminates on the receiving pod, as its media sessions do. The
  holder sees one control stream per participant either way, so roster
  logic has one code path.
- **Adoption**: a pod receiving a join, or a proxy failing with a dead
  upstream, checks the lease; if `renewedAt` is stale past the lease grace
  it force-takes with generation CAS, rebuilds attachments from the CR,
  and starts with an empty roster. Participants whose control session died
  with the old home reconnect through the normal client reconnect path
  (4007 is the only terminal room code; a plain transport loss is not) and
  arrive with their nickname. The room is whole again within one client
  reconnect interval, which is the owner's "viewers hop to another pod"
  expectation made concrete.
- **Fencing**: a stale home that comes back finds its generation rejected on
  renew, closes its sessions with a non-terminal code, and they reconnect
  to wherever the load balancer sends them.
- **Drain**: on pod drain the holder releases the lease (holder cleared,
  generation kept) after closing sessions with 4002, so the next join
  claims without waiting for staleness.
- **Janitor**: deletes dynamic `Room` CRs whose lease is stale past a
  long window *and* whose `emptySince` is older than the empty grace, and
  nothing else. Static CRs are never janitored.

### 4.6 Control protocol (wire)

One bidirectional stream per control session. Every record is
`uint16 length (BE) ‖ Version ‖ Type ‖ payload`, the same prologue shape
as the reliable carrier. Payloads are hand-encoded like every other wire
message, with golden vectors.

| Type | Name | Direction | Payload |
|---|---|---|---|
| `0x13` | `RoomHello` | client → relay | protocol version, nickname, client kind (web-viewer / web-broadcaster / native), requested capabilities bitmap |
| `0x14` | `RoomState` | relay → client | full snapshot: code, kind, display name, capabilities, attachments (ID, label, live/away, viewer count), participants (ID, nickname, kind, flags), your participant ID, your grants (creator / attach-ok), the creator token (first snapshot after a mint only), and the room's **HMAC'd key** (RM8: the telemetry handle — a client cannot compute it, so the relay hands it over; revision 2026-09-04) |
| `0x15` | `RoomEvent` | relay → client | one delta: participant joined / left / renamed / flags changed; attachment added / removed / live / away / viewer count; room ending (reason) |
| `0x16` | `RoomCommand` | client → relay | attach (broadcast ID + resume token + label), detach (broadcast ID), set nickname, end room, resync; **reserved sub-ranges** for chat (`0x40–0x4F`) and voice (`0x50–0x5F`) |

- `RoomState` is sent once after `RoomHello` and again after any
  adoption or proxy re-establishment; clients replace, never merge.
- `RoomEvent` carries a monotonically increasing sequence within the home
  pod's generation so a client can detect a missed delta and request a
  fresh `RoomState` (`RoomCommand.resync`). A `CommandRejected` is addressed
  to one participant and carries the *current* sequence without advancing
  it, so the others never see a gap; a gap is `seq > last + 1`, never
  `seq <= last + 1` (implementation note, 2026-09-04).
- Participant IDs are per-room, per-generation, opaque. Nicknames are
  display-only.
- **Close code 4007 `RoomEnded`**: terminal for the room session only. The
  participant's media sessions have their own lifecycle.
- The native mirrors implement `RoomHello`, `RoomState` (parse), the
  attach/detach commands, and the close code; they carry every golden
  vector regardless.

### 4.7 Participant media

A participant in grid or focus mode opens one ordinary `/subscribe/{id}`
session per shown tile. Nothing on that path knows about rooms. Hide
videos closes them all and keeps the control session. The SPA's existing
per-session pipelines (decoder, worklet, av-sync, delivery presets) are
instantiated per tile; the presets from R32 apply per tile, with the
focused tile defaulting to the user's chosen preset and the small tiles
to the lowest-cost one. **Audio follows the mode** (owner decision,
2026-09-03): in focus mode only the focused POV is audible; in grid mode
every tile has its own volume control and the tiles are **mixed** into one
output, with the control bar's speaker acting as the master. Per-tile
levels persist per browser so a participant can, for example, keep one
POV's game audio up and the others low. The broadcaster's own tile
defaults to muted (they hear their game already). Mixing is client-side:
each tile's existing audio pipeline feeds a shared `AudioContext`
destination through its own gain node, so the relay and the media path
stay unchanged.

Bandwidth is the honest cost of "grid, all live" (D11): N broadcasts at
full rate. The relay fan-out sees N subscribers for one human; the
`-max-subscribers` and fleet caps apply unchanged. Thumbnails (a periodic
keyframe-only tile fed from the cache) are the listed follow-up that
brings the grid back to roughly one broadcast's worth.

### 4.8 Broadcaster flows

**Web broadcaster** (`#/broadcast`): a "Room" control offers *new room*,
*join a room by code*, or *use a room link*. Attach sends the running
broadcast's resume token; the page transitions into the room view with
the broadcaster's own controls (stop, source, quality, stats) available
on their own tile. Starting a broadcast from *inside* a room view (a
participant who decides to stream) is the same flow in reverse and lands
in the same state.

**Native broadcasters**: `gawk-broadcast -room <code-or-slug>` (and the
matching profile field / GUI card) attaches on publish and re-attaches on
resume, holding the attach secret from the profile when the room is
static. "Open room view" launches the browser at
`#/room/<code>?rt=<creator-or-attach-grant>` where the SPA moves the grant
into session storage and rewrites the hash before rendering, the same
one-shot pattern the `?relay=` link uses for its parameter. A native
broadcast shows in the web room view as the broadcaster's own tile with
the controls the grant allows (detach, label), not the native app's
capture controls.

### 4.9 UI/UX requirements and the settled layout

The bullets below were the input to the design pass. The pass happened
in Claude Design on 2026-09-03 and its outcome is recorded right after
them (**revision 2026-09-03**); where the two disagree, the revision wins.
The canvas itself is the reference for RM4/RM5 and is linked from §12.

- Landing: the join box stays one field. A six-character code that
  resolves to a room lands in the room; a broadcast code lands in the
  viewer, as today. There is **no** "start a room" on the landing page:
  dynamic rooms are created from a running broadcast (§4.4, §4.8), and
  static rooms are reached by link.
- Room view, three modes: **grid** (default), **focus** (one large, others
  small, switch by click or number keys), **hide videos** (roster and
  controls only). Mode persists per browser.
- Roster: participants with nickname and kind (viewer / streaming), each
  attached broadcast with label, live/away state and viewer count. The
  creator sees detach actions.
- Broadcaster controls live on the broadcaster's own tile, not in a
  separate page.
- Share affordances: copy room link, copy room code (dynamic), and per
  tile "open full-screen" which is a plain `#/view/` link.
- Nickname prompt on first join, remembered; editable from the roster.
- Every state the relay can emit has a visible form: room ended (4007),
  broadcaster away, attachment removed, limit reached, wrong attach
  secret.
- Reserved space: a speaking indicator on participants and a chat panel
  slot, both hidden until their capabilities arrive (§4.11). The design
  pass should draw them so the v1 layout does not have to move later.
- Mobile: hide-videos and focus must work on a phone; grid is allowed to
  degrade to focus below a width threshold.

**Revision 2026-09-03 — what the design pass settled.** Three directions
were sketched (a docked sidebar, a cinematic dock, a tabbed stage) and the
owner chose the **cinematic dock**: the room view keeps the viewer's
existing language, video edge to edge with header and footer overlays
that appear on hover or tap and fade after the same idle period the
viewer uses today. Concretely:

- **Header overlay**: room code, streaming count, people count; copy
  link, the people-and-chat toggle, fullscreen. **Footer overlay**: the
  viewer's control bar with a Grid / Focus / Hide videos segment beside
  the playback preset pill, a master volume, more, leave. Both fade.
- **People and chat is an optional side panel**, opened from the header
  and pinnable so it stays while the overlays fade. It holds the
  streaming list (per-POV viewer count, creator's detach), the roster
  with the reserved speaking slot per person, the reserved chat area and
  message input, nickname and the copy actions. On a phone it is a
  bottom sheet.
- **Own tile**: an accent ring and a glass bar inside the tile with
  Stop, Source, Quality, Stats and Detach; it fades with the chrome.
- **Keys**: 1–4 focus that POV, 0 returns to grid; the number sits on
  each tile while the chrome is shown. Focus shows the other POVs as
  small tiles in a glass strip at the top right, on the lowest-cost
  preset.
- **Audio** follows §4.7: focus plays the focused POV only; grid mixes
  every tile, each with its own volume pill on the tile, and the footer
  speaker is the master. Own tile muted by default.
- **Hide videos** shows a centred card ("you are still in the room,
  nothing is downloading") with the panel beside it.
- **States drawn**: room ended (4007), reconnecting after a home-pod
  move, attachment removed (toast), room full, wrong attach key on a
  static room, and the first-join nickname prompt.
- **Ways in**: the broadcaster page gets a Room panel (new room, or join
  by code / link); the landing page is unchanged, one join box that
  resolves either kind of code, and no start-a-room action (§4.4).
- Not drawn yet, left for RM4: the per-tile audio pick on a phone, the
  grid-to-focus breakpoint, and the empty static room.

### 4.10 Knobs, Helm, observability

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-rooms` | `GAWK_ROOMS` | `false` | Enable the room routes and registry |
| `-room-empty-grace` | `GAWK_ROOM_EMPTY_GRACE` | `60s` | Dynamic room survives this long empty |
| `-max-rooms` | `GAWK_MAX_ROOMS` | `10` | Live dynamic rooms per pod (fleet-wide in cluster mode, enforced at CR create) |
| `-max-room-broadcasts` | `GAWK_MAX_ROOM_BROADCASTS` | `4` | Attachments per room (static rooms may override in spec) |
| `-max-room-participants` | `GAWK_MAX_ROOM_PARTICIPANTS` | `50` | Control sessions per room |
| `-room-create-secret` | `GAWK_ROOM_CREATE_SECRET` | `""` | When set, required to mint a dynamic room; the hosted "paid / invited" gate |
| `-rooms-file` | `GAWK_ROOMS_FILE` | `""` | Static room definitions outside Kubernetes |

All through `registryOptions`, all in `values.yaml`. The chart ships the
`Room` CRD next to `crd-ban.yaml`, gated on `rooms.enabled`, with
`helm.sh/resource-policy: keep`, and extends the Role to `rooms`.

`/statusz` gains a `rooms` section keyed by the per-process HMAC of the
code, with attachment count, participant count and home/proxy role.
Metrics: `gawk_rooms_live`, `gawk_room_participants`,
`gawk_room_attachments`, `gawk_room_proxied_sessions`, plus adoption and
proxy-failure counters. Telemetry sessions carry the HMAC'd room key so a
session diagnosed in the R31 UI can be grouped with its room.

### 4.11 Anticipated integrations (designed now, built later)

The owner asked that the design leave explicit hooks. These are the
hooks, and nothing else is promised.

- **Room text chat**: a `RoomCommand` sub-range (`0x40–0x4F`) and matching
  `RoomEvent` kinds; messages fan out over the control stream from the
  home pod; no persistence in v1 of chat either. Requires only D10's
  nickname. The UI reserves the panel slot.
- **Voice via a Mumble bridge**: a room capability `voice` advertised in
  `RoomState`; `spec.integrations.mumble` on the CR (server, channel
  prefix, bridge identity); per-participant voice frames as a new
  datagram type on the *control session's* connection (so the room owns
  the voice path, not a broadcast); `speaking` on the participant record
  from day one. The bridge design lives in its own doc when it is picked
  up; this doc only guarantees that a room can advertise, that a
  participant record can carry speaker state, and that a CR can name a
  Mumble mapping.
- **Stronger identity**: the participant record has an `identity` field,
  empty in v1, intended for a key fingerprint (browser keypair or Mumble
  certificate hash).
- Not reserved: input forwarding and room-level recording. They were
  discussed and the owner did not ask for hooks; the event model does not
  preclude them.

## 5. Security considerations

- Room codes are joinable secrets: the HMAC keying rule (D16), the same
  join rate limits as `/subscribe`, and the R39 IP-ban gate apply to
  `/room/`.
- The creator token and the attach secret never appear in logs, `/statusz`,
  telemetry or webhooks; the CR stores a fingerprint only.
- Attach proof is the resume token, so attaching a broadcast you do not
  publish is exactly as hard as hijacking it (R17's guarantee).
- Room participants see attached broadcast IDs (D1). A room code is
  therefore a superset secret; the UI says so where it offers the share
  action.
- `-room-create-secret` is a fleet-level gate like `-publish-secret`: it
  rations creation, it is not identity.
- The internal `/internal/room/` route uses the cluster PSK and the lease
  generation, and, like `/internal/subscribe/`, must never be routed
  publicly.

## 6. Failure modes

| Failure | Behaviour |
|---|---|
| Home pod dies | Participants lose their control session (non-terminal), reconnect, land on any pod, which adopts or proxies. Roster is whole after one reconnect interval. Media unaffected. |
| Proxy upstream dies mid-session | Receiving pod closes the participant's control session non-terminally; the reconnect adopts or re-proxies. |
| Attached broadcast's publisher dies | Tile shows *away* within the broadcast grace; detached and removed from the CR after it. |
| API server unreachable | Existing rooms keep working from the informer cache; mint and attach (which write the CR) fail closed with 503; adoption cannot fence and is refused until the API returns. Same posture as R17's registry. |
| Empty grace expires during a mass reconnect | Impossible by construction as long as the grace exceeds the client reconnect interval; RM2 asserts the ordering in a test. |
| Code collision | Prevented by the CR reservation; a residual race is one cache window against 1-in-31⁶ and lands on a 409 for the loser. |
| Room limit reached | `429` on mint or attach with a `RoomEvent` reason the UI renders. |

## 7. Rejected alternatives

- **Reserved first character for room codes.** Disjoint by construction,
  but it changes `broadcastid.Mint`, removes one thirty-first of every
  broadcast's entropy, and opens a rollout window in which live
  broadcasts still use the reserved character. Mint-time exclusion (D3)
  gets the same user-facing guarantee for free.
- **Rooms replace broadcasts (every broadcast is a room of one).** Clean
  in theory; in practice it rewrites the join path, the native
  broadcasters and every wire mirror for a benefit nobody asked for.
- **Participants in the CR.** Every join and leave becomes an API write,
  the informer becomes the roster's hot path, and a k8s outage freezes
  presence. The owner rejected it; the reconnect-and-rebuild design is
  strictly better for the user.
- **Multiplexing media onto the control session.** Would let a room
  participant use one QUIC connection. It also makes the relay a media
  demultiplexer and forks the edge-pull path. Deferred, not rejected; the
  control protocol does not prevent it.
- **HTTP resolve endpoint for typed codes.** The relay is HTTP/3-only and
  browser `fetch` over HTTP/3 without a prior Alt-Svc hint is not
  dependable. A `CONNECT` is.

## 8. Out of scope (non-goals)

- Voice, chat, input forwarding, room recording (hooks only, §4.11).
- Thumbnails for non-focused tiles (listed follow-up).
- Accounts or persistent identity.
- Cross-relay rooms (a room lives on one fleet; a `?relay=` link says
  which).
- Any change to the media wire or the edge-pull path.

## 9. Chunks & acceptance criteria

| Chunk | Scope | Acceptance |
|---|---|---|
| **RM1** | Wire: types 0x13–0x16, close code 4007, golden vectors in all four mirrors; `gawk-server/rooms` public package with CR types and code normalization | Vectors byte-identical across Go, TS, Go-native, Rust; Windows CI triggers on the wire change; `rooms` importable from `gawk-admin` via the existing `replace` |
| **RM2** | Relay, single-pod: room registry, `/room/{code}` and `/room/new`, hello/state/event/command handling, attach with resume-token proof, creator token, empty grace, limits, all knobs via `registryOptions`, `/statusz` and metrics | Go unit: mint is disjoint from live broadcasts and vice versa; attach with a wrong token is refused; grace survives a reconnect shorter than it; every knob reaches `hub.Options` (the R2 test shape); `-rooms` off is byte-identical (full existing suite green with no room code path reachable) |
| **RM3** | Cluster: `Room` CRD + chart + RBAC, home lease on the CR status, proxying via `/internal/room/`, adoption with generation CAS, drain release, janitor, file source for non-k8s | Fake-clientset unit: two claimants ⇒ one holder; stale generation loses; janitor deletes only stale-and-empty dynamic rooms; **kind two-pod tier**: kill the home pod, participants reconnect and the roster is whole within one reconnect interval; a static room CR applied by `kubectl` is joinable on both pods |
| **RM4** | SPA participant experience: `#/room/`, `#/join/` routes, nickname, room view in grid / focus / hide-videos, roster, share actions, every relay state rendered | Routing unit tests for the new arms; component tests per mode; browser E2E: two synthetic broadcasts attached, grid shows both, focus switches within one keyframe, hide-videos closes every media session (asserted on the relay's subscriber count) |
| **RM5** | SPA broadcaster flows: new/join/link from the broadcast page, transition into the room view with own-tile controls, start-broadcast-from-room | E2E: web broadcaster attaches, appears in another participant's roster, stops, tile shows *away* then removal after grace |
| **RM6** | Native attach: `gawk-broadcast` flag + profile + GUI card, Windows GUI card, re-attach on resume, "open room view" grant hand-off | Go and Rust integration suites against a real relay: attach visible in a web participant's `RoomState`; the grant hand-off URL is rewritten before first render (SPA unit test) |
| **RM7** | Admin: static room CRUD + attach-secret rotation + end-room in `gawk-admin`; migrations under the R39 policy; webhook on room end carrying the HMAC'd key only | Portal API tests; CI migration gates; a webhook payload test asserting no raw code |
| **RM8** | Telemetry: HMAC'd room key on sessions, R31 UI groups a session with its room | Ingest unit test; UI test for the grouping |
| **RM9** | Docs: `docs/gotchas.md`, `docs/self-hosting.md` (enabling rooms, static room recipe, non-k8s file source), READMEs, `ROADMAP.md` status; the manual pass on the reference deployment | Manual pass (§10) recorded in §11 |

## 10. Verification

Automated: the per-chunk criteria above, run by the existing per-module
jobs plus the kind two-pod tier extended with the RM3 adoption scenario.

Manual, milestone-closing, on the reference deployment:

1. Two people stream from a gaming PC (native Windows) and a browser; one
   creates a dynamic room, the other joins it by code from the join box.
2. A third person joins on a phone in hide-videos mode, then switches to
   focus.
3. Kill the home pod (`kubectl delete pod`) during the session; everyone is
   back in the room without touching the page.
4. A static room created in `gawk-admin` is joined by link with a wrong
   attach secret (refused) and the right one (attached).
5. End the dynamic room from the creator's browser; every participant
   sees "room ended" and the two broadcasts are still watchable by their
   own codes.

## 11. Implementation status

**Implemented 2026-09-04** (RM1–RM9; single PR). Written the way docs/42
§11 is: deviations from §4 with reasons, what is verified and by what, and
the manual pass outcome.

### 11.1 Deviations from §4, with reasons

- **`RoomState` carries the room's HMAC'd key** (§4.6 table, revision
  2026-09-04). §4.10 wants telemetry sessions grouped by the HMAC'd room
  key, and a client cannot compute an HMAC it does not hold the key for,
  so the relay hands it over: the same 6-byte digest `TelemetryHello`
  carries for a broadcast. The alternative — the client reporting the raw
  code to telemetry for hashing there — would have put a joinable secret
  on the ingest path, which is exactly what R28's obfuscated broadcast key
  exists to avoid.
- **`CommandRejected` does not advance the room sequence.** It is
  addressed to one participant; giving it a sequence number would make
  every other participant see a gap and resync. A gap is therefore
  `seq > last + 1`, never `seq <= last + 1`.
- **Creator tokens are domain-separated from resume tokens.** Room codes
  come from the same alphabet and length as broadcast IDs, so an
  unprefixed HMAC over the code would make a broadcast's resume token the
  creator token of an identically named room. The prefix is
  `gawk-room-creator-v1‖0x00`; the broadcast construction is untouched, so
  every outstanding resume token keeps verifying.
- **Mint gate order is broadcast-known (404) before proof (403).**
  `/subscribe` already reveals whether an ID is live, so answering
  existence before the token leaks nothing, and an expired broadcast's
  still-valid token gets the honest answer.
- **A wrong attach secret is refused at join (403), not at the first
  attach.** §4.2 said "when one is required for the requested action"; a
  broadcaster that typed the key wrong must learn it now, and §4.9's
  "wrong attach secret" state needs a pre-upgrade status to render. A
  viewer that presents no secret still joins.
- **The room knobs cross `roomOptions`, not `hub.Options`.** RM2's
  acceptance criterion said "every knob reaches `hub.Options` (the R2 test
  shape)"; the room registry is its own object, so it has its own
  carry-all mapping in `cmd/gawk-server/main.go` and its own
  `TestRoomOptionsCarryAllKnobs`. The one room fact the hub needs — "is
  this ID a live room code" — is `hub.Options.IDReserved`.
- **`/statusz` gains its section through `ops.StatuszHandler`, not
  `hub.RegistryStats`**, so the hub stays room-free; with `-rooms` off the
  section is omitted and the document is byte-identical.
- **The file source polls mtime and honours SIGHUP** rather than fsnotify,
  the same deviation `-moderation-source=file` records (docs/42 §11).
- **Static CRs are not upserted on every pod at CR add.** A static room
  has no home until its first participant (§4.5), so the claiming pod
  upserts it at first join; an informer *update* on a held static room
  refreshes it, and a delete ends it everywhere with 4007 (reason
  operator).
- **Adoption after a crash is the stale-lease path**, exercised in kind by
  a `--force` pod delete; the drain-release path is unit-tested.
- **The `roomcluster` tests use a hand-rolled resourceVersion-CAS reactor**
  on the fake dynamic client (the fake has none), not envtest.
- **Native broadcasters send an idempotent `Attach` after a mint too.** The
  minted attachment has no owner participant on the relay (RM2), so
  without it a reconnected minter could not detach its own broadcast.
- **Stopping a native broadcast does not detach it**; the tile shows
  *away* until the broadcast grace expires, per §4.4. Joining a different
  room detaches first (D1).
- **`-room-create-secret` is never persisted by the native broadcasters**
  (flag/env per run): it is the operator's invite, not a broadcaster
  credential. "Open room view" on Linux needs `-app-url`, since that
  broadcaster has no compiled-in app URL; the GUI shows a caption instead.
- **The web broadcaster's own tile paints the local capture preview**, not
  a self-`/subscribe`: no extra uplink, muted by construction. Native
  broadcasts still appear as ordinary remote tiles. "Source" on the own
  tile is stop + reclaim (the pipeline has no live re-capture API); Detach
  keeps the broadcaster in the room as a participant.
- **Grid degrades to focus below 720 px**; the people-and-chat panel is a
  bottom sheet there. Master volume is per session; per-tile levels and
  the room's playback preset persist per browser.
- **The admin portal detects relay-ended dynamic rooms in its 60 s
  reconcile sweep**, not through an informer (it has none; docs/42's
  sweep is the precedent): up to one interval of webhook latency, and a
  room that ends while no leader is sweeping is missed. Portal-ended rooms
  are recorded inline with the operator as actor.
- **`room.created` / `room.secret_rotated` webhooks carry no `roomKey`
  until a pod has homed the room** — `status.key` is written by the home
  pod — and never fall back to the code; their portal link is the bare
  Rooms page. No migration was needed: the events table's `type` column is
  unconstrained text.
- **The portal refuses a static slug with the dynamic-code shape** (six
  characters of the broadcast alphabet), a rule stricter than the relay's
  own acceptance, so the join box stays unambiguous (D2's "link only").
  `POST /rooms/{name}/end` on a static room is 409 ("a static room never
  ends", D7) rather than an alias of delete; rotating the secret of a
  secret-less static room *adds* the gate instead of failing.
- **Windows: no create-secret field in the card** (the engine carries it,
  the shell passes empty; a `-room-create-secret` relay answers 403 and the
  status line says so), and the room session's lifetime is the broadcast's.
- **The telemetry room key is shape-validated, not authenticated.** The
  R28 session token binds broadcast key and role only, and a session can
  enter a room after its token was minted, so `roomKey` on a batch is a
  client-stated grouping hint (the R31 room view says so). Room resolve
  rides the existing `POST /v1/resolve` with a `room` body and lower-cases
  the code like `rooms.NormalizeCode`, so a room and a broadcast spelled the
  same get different digests. `/v1/sessions` and `/v1/broadcasts` (the MCP
  defaults) stay byte-identical; the key is on the session detail, the
  history surface (`room=` filter, `GET /v1/history/rooms/{key}`) and the
  live projection, all `omitempty`.
- **The `#/join/<code>` probe joins as a real participant.** Resolving a
  typed code is a `CONNECT /room/<code>` (D19), so a code that IS a room
  produces a momentary `guest-N` join/leave on that room's roster before
  the resolver lands in the room view proper, and a full room (429) is
  indistinguishable from "not a room" and falls through to the viewer's
  "streamer offline". A resolve-only hello is a candidate follow-up; the
  cost today is one roster blink per typed code.
- **Attach secrets are resolved at join time, not cached on the home
  pod** (review finding): the portal rotates a static room's Secret in
  place without touching the CR, so a homed room must re-read the Secret
  for every join that presents one (one Get; unreadable → 503, fail
  closed). The file source keeps its inline secret.
- **A dynamic room whose home crashed while populated is janitored on
  the stale lease alone** (review finding): nothing would ever stamp
  `emptySince` on it, and an interested participant re-dials and adopts
  within the client reconnect window, so a lease stale past the long
  window with no adoption *is* an empty room. The first stale sighting
  stamps `emptySince`; the next pass deletes.
- **Post-upgrade close codes 400 and 404 exist alongside 4007**: 400 for a
  session that broke the control protocol (no hello, malformed record),
  404 for a room that ended between the pre-upgrade check and the hello.
  Neither is reconnected.
- **The broadcast source is fleet-wide in cluster mode** (review
  finding, PR #302). The registry asks the transport
  (`Server.RoomBroadcasts`) about an attachment, and the transport answers
  from the local hub first, then from the origin lease as the R17
  coordinator's informer last saw it (`Coordinator.Lookup`, the cached
  twin of `Resolve` — the 1 Hz refresh over every attachment must never
  cost an API Get). So a `/room/new` or an `Attach` evaluated on a pod
  that is neither the broadcaster's nor a watcher's — without session
  affinity, the common case on two pods — resolves the same way
  `/subscribe` does: a held, renewing lease is *known, live*; a lease the
  origin stamped into grace (or whose renewals went stale, the crash
  case) is *known, away*; no lease is *unknown*. Lifecycle follows suit:
  the hub hooks (`PublisherClosed` → away, `BroadcastExpired` → removed)
  fire on the **origin** pod, which need not be the room's home, so on
  the home the refresh poll is the lifecycle for an off-pod attachment —
  it flips the tile to away when the lease enters grace and, with
  `roomsrv.Options.UnknownIsExpired` (set only by the cluster seams),
  removes the attachment with reason expired once the lease is gone,
  notifying participants and rewriting `status.attachments`. §6's "away
  within the broadcast grace; detached and removed from the CR after it"
  now holds across pods. Two consequences worth knowing: the poll starts
  only after the lease informer has synced (an empty cache is not "no
  leases"; until then mint and attach answer 404 for an off-pod
  broadcast — the informer's first second on a fresh pod), and the
  **viewer count is the one number that stays pod-local**: the tile
  shows the R18 fleet-global G when this pod has a hub for the broadcast
  (origin or edge — `hub.Registry.BroadcastState` reports G, not the
  local human count, as of this fix), and **0 for an off-pod broadcast
  nobody on the home pod watches**, since G is computed on the origin and
  reaches other pods only through an edge session. Single-pod mode is
  byte-identical: no coordinator, local hub only, the hooks own the
  lifecycle.
- **The drain's room-lease `Release` retries through a CAS conflict**
  (found by the two-pod transport fixture once its pods ran a real drain):
  the drain closes the home's sessions first, their leaving stamps
  `emptySince` — a status write on the same CR — and `Release` lost that
  race and gave up, logging "release failed during drain" with the holder
  still in place, so the reconnecting participant waited out the
  staleness window instead of adopting at once. It now re-reads and
  retries like `patchStatus`.

### 11.2 Verified by

| Criterion (§9) | Test |
|---|---|
| RM1 vectors byte-identical in four mirrors | `wire/room_test.go`, `wire.test.ts` "room control protocol", `wirecheck_test.go` `TestGoldenRoom*`, `crates/wire/tests/golden.rs` `golden_room_*`; close code 4007 in every constant-pin test |
| RM1 `rooms` importable from `gawk-admin` | `gawk-admin/internal/kube` imports it; the `tidy` job |
| RM2 mint disjoint from live broadcasts and vice versa | `roomsrv` `TestMintIsDisjointFromLiveBroadcasts`, transport `TestPublishNeverMintsALiveRoomCode` |
| RM2 wrong token refused | `TestAttachRequiresProofAndGrant`, transport `TestRoomMintJoinAttachAndEnd` (bad proof → `CommandRejected`), `TestRoomJoinStatusVocabulary` (403s) |
| RM2 grace survives a shorter reconnect | `TestEmptyGraceSurvivesAReconnectShorterThanIt` |
| Fleet-wide broadcast source: mint on a pod other than the publisher's, away within the refresh interval, removed after the broadcast grace, CR rewritten (review, PR #302) | transport `TestRoomMintOnAnotherPodThanThePublisherFollowsTheLease` (two in-process pods, real coordinators on one fake clientset) and `TestRoomBroadcastsAnswersLocalHubThenOriginLease`; `cluster` `TestLookupServesTheLeaseCache`; `roomsrv` `TestRefreshExpiresUnknownAttachmentsInClusterMode`; `hub` `TestBroadcastStateReportsFleetGlobalViewers` (G, not the local count) |
| Drain release survives a CAS conflict with a concurrent status write | `roomcluster` `TestReleaseRetriesThroughAConflict`; transport `TestRoomProxyPipesAndAdoptsAfterTheHomeDrains` under the fleet fixture's resourceVersion CAS reactor |
| RM2 every knob reaches the registry | `TestRoomOptionsCarryAllKnobs`, `TestRoomKnobs`, `TestSanitizedCoversEveryConfigField` |
| RM2 `-rooms` off byte-identical | `TestRoomsOffLeavesNoRoute` (no route, no `/statusz` section), the full pre-R42 suite green, chart "rooms off renders nothing" |
| RM3 two claimants ⇒ one holder; stale generation loses; janitor | `internal/roomcluster/store_test.go` |
| RM3 proxy and `/internal/room` vocabulary | `internal/transport/roomcluster_test.go` (two servers in-process) |
| RM3 kind two-pod tier | `e2e/rooms-assert.sh` in the `e2e-cluster` job: static CR joinable on both pods, `status.key` written, second joiner proxied to the home, CR delete → 4007, dynamic room minted on the pod that is not its publisher's origin, home pod killed → re-dialled joiner lands with attachments whole and the lease at a higher generation. **Ran green 2026-09-05** (dispatch 33919073089, PR #302), after three runs lost to the harness shadowing `HOME` (docs/gotchas.md) |
| RM4 routing, modes, hide-videos closes every media session | `routing.test.ts`, `RoomScreen.test.tsx` (hide-videos: zero `/subscribe` sessions, control session kept), `room-session.test.ts`, `App.room.test.tsx` (`?rt=` hand-off before first render) |
| RM4 browser E2E | `node e2e/run.mjs --rooms` (two pubsims, grid → focus by key → hide-videos asserted on the relay's subscriber counts) |
| RM5 broadcaster attaches, appears in a roster, away then removal | `BroadcasterScreen.room.test.tsx`; the relay-side away/expiry path in `TestBroadcastLifecycleHooks` and the Go native integration test |
| RM6 attach visible in another participant's `RoomState` | `gawk-broadcast/internal/engine/room_integration_test.go`, `crates/engine/tests/relay_integration.rs` (ignored; CI runs it on Linux) |
| RM6 grant hand-off rewritten before first render | `App.room.test.tsx` |
| RM7 portal API, CI migration gates, webhook carries no raw code | `gawk-admin/internal/api/rooms_test.go`, `internal/kube/rooms_test.go` (+ envtest against `crd-room.yaml`), `internal/notify/payload_test.go` `TestNoRawIDOrIPInAnyPayload` with a room-code poison; no migration was needed (`admin-migrations` unchanged) |
| RM8 ingest unit test; UI groups a session with its room | `gawk-telemetry/internal/ingest/ingest_test.go` (roomKey shapes), `internal/readapi/rooms_test.go` + `TestR31DefaultsAreByteIdentical`, `ui/src/views/RoomView.test.tsx`, router tests |
| RM9 docs | this section, `docs/gotchas.md` "Rooms (R42)", `docs/self-hosting.md` §10, `ROADMAP.md`; the §10 manual pass is **open** |

### 11.3 Manual pass (§10)

Not yet run on the reference deployment. Record each of the five steps
here with the date and outcome when it is.

## 12. The design pass (done 2026-09-03)

The room view was worked through in Claude Design against the §4.9
requirements. The canvas is the layout reference for RM4 and RM5:

- **Canvas**: https://claude.ai/code/artifact/db7dbf97-928e-4b31-ad49-6cb7b741a215
  — a Claude Code artifact owned by the maintainer. It is **private**:
  the link opens only for the owner or someone it has been shared with,
  and it is not a public document. Ask the maintainer for access or for a
  PNG/PDF export; do not treat its absence as "no design exists".
- **Page "Room view"**: grid with chrome shown, grid idle, grid with the
  people-and-chat panel pinned, focus, hide videos, the six states, the
  ways in (broadcaster Room panel, landing), and two phone frames
  (focus, people-and-chat sheet).
- **Page "Direction sketches"**: the three low-fi directions the choice
  was made from, with B (dock) marked as chosen.

The pass was low-fidelity on purpose (striped blocks for video, the
app's real tokens, icons and control bar for the chrome). Decisions taken
there are recorded in §4.9's dated revision; the hi-fi pass against the
synced component library in the gawk Design System project is optional
and, if done, is noted here with its own link.
