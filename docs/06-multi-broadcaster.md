# R1 — Multi-Broadcaster Support: Design + Implementation Plan

Design doc for roadmap item [R1](../ROADMAP.md#r1--multi-broadcaster-support).
Same shape as [`implementation-tasks.md`](implementation-tasks.md): the spec
sections below are the contract; the [milestone tables](#milestones--delegable-chunks)
break the work into PR-sized chunks (E1–G2) with acceptance criteria. Each
chunk is written to be implementable from this document alone.

## Progress

| Chunk | Status |
|-------|--------|
| E1 wire: announce message, close code, ID helpers | done |
| E2 hub → registry refactor | done |
| E3 broadcast lifecycle (grace timer, GC) | done |
| E4 transport routes + `/statusz` | done |
| F1 TS wire mirror + URL construction + announce read | done |
| F2 `ViewerSession` terminal states | done |
| F3 routing params, ViewPage join flow | done |
| F4 BroadcastPage ID/link UI + reclaim | done |
| G1 manual multi-broadcast browser verify | done |
| G2 docs close-out | done |
| Post-implementation code review + hardening (see [below](#post-implementation-review-2026-07-13)) | done |

## Context

Today the relay is implicitly single-broadcaster: one `/publish` slot, one
subscriber set, one cached decoder config + keyframe. R1 makes broadcasts
first-class: multiple simultaneous, independent 1-to-many sessions, each
identified by a short shareable **broadcast ID**. Starting a broadcast mints
an ID; the broadcaster's UI shows it plus a join link (`#/view/<id>`);
viewers join by link or by typing the code. Everything that is currently
singleton hub state becomes per-broadcast, with the existing semantics
carried over unchanged *within* a broadcast.

## Locked decisions (settled 2026-07-12)

1. **ID minting happens server-side, announced over WebTransport.** The
   broadcaster CONNECTs to `/publish` with no ID; the server mints the ID,
   registers the broadcast, and sends a `BroadcastAnnounce` message on a
   **server-initiated unidirectional stream**. Rationale: the WebTransport JS
   API exposes no CONNECT response headers/body, and a `POST /broadcasts`
   HTTPS mint endpoint is off the table because the relay is
   HTTP/3-over-UDP only — there is no TCP listener (that's why the k8s
   probes are exec probes), and adding one means a second port, dual-protocol
   Service, and CORS. Client-minting was considered and rejected in favor of
   keeping the server in control of the ID space (and of the R2 secret check
   later).
2. **Abandoned broadcasts are garbage-collected after a grace period**,
   default **5 minutes**, flag `-broadcast-grace` / `GAWK_BROADCAST_GRACE`.
   Long enough to survive a broadcaster PC reboot or game crash-restart.
   The timer arms when the publisher session ends, cancels if the publisher
   reclaims the broadcast, and on expiry the broadcast is deleted and
   remaining viewers get a **terminal close** (see close code below) so they
   show "broadcast ended" instead of reconnect-looping.
3. **Join links auto-join.** Opening `#/view/<id>` connects immediately; the
   manual code field + Watch button remain for typed codes.
4. **Legacy ID-less routes are removed.** Only `/publish`, `/publish/{id}`
   (reclaim) and `/subscribe/{id}` exist; old clients get 404. Both
   components release in lockstep, so a clean protocol break is cheap.

Deliberately deferred (restated from the roadmap): persistent per-broadcaster
channels (needs an identity/persistence story), and all limits/auth — max
concurrent broadcasts, publish secret, rate limiting are **R2**. In R1 the
registry is bounded only by GC: every entry is backed by a live publisher
session or a running 5-minute grace timer.

## Broadcast ID

- **6 characters** from the 31-symbol alphabet `23456789ABCDEFGHJKMNPQRSTUVWXYZ`
  (no `0/O`, `1/I/L` — readable out loud, unambiguous when typed).
- Minted server-side with `crypto/rand` (uniform, rejection-sampled or
  modulo-safe). 31⁶ ≈ 8.9×10⁸ — collision-proof at ~5 concurrent broadcasts;
  unguessability is the access-control story until R2 adds rate limiting.
  Mint under the registry lock, re-roll on collision (bounded retries).
- **Lookup normalizes to uppercase**; the server rejects IDs that are not
  exactly 6 chars from the alphabet (after normalization) with 404 before
  session upgrade. Client-side input is uppercased and charset-validated
  before dialing.

## Wire additions (`gawk-server/wire` + TS mirror)

The **datagram** format (VideoChunk `0x01`, DecoderConfig `0x02`) is frozen
and unchanged — broadcast IDs live in URLs. Two additions:

### `BroadcastAnnounce` — stream-carried, not a datagram

Sent by the server to the **publisher only**, once per publish session
(mint *and* reclaim — uniform protocol), on a server-initiated
unidirectional stream (`session.OpenUniStream()`), then the stream is
closed. The client reads the stream to EOF and parses.

| Offset | Size | Field |
|--------|------|-------|
| 0 | 1 | `Version` = `0x01` (same constant as datagrams) |
| 1 | 1 | `TypeBroadcastAnnounce` = `0x03` |
| 2 | 1 | idLen (u8) |
| 3 | idLen | broadcast ID, ASCII |

**Golden vector** (ID `K7XQ2M`): `0103064b375851324d`.
Parse errors (short buffer, bad version, bad type, idLen overrun, chars
outside the alphabet) return errors, never panic; fuzz alongside the
existing `testing.F` fuzzers.

### Terminal close code

```go
// CloseCodeBroadcastEnded is the WebTransport application close code sent
// to subscribers when their broadcast is garbage-collected.
const CloseCodeBroadcastEnded = 4000
```

On GC expiry the server calls `CloseWithError(4000, "broadcast ended")` on
each subscriber session. Browsers surface this via the `WebTransport.closed`
promise (`WebTransportCloseInfo.closeCode`) — it is the *only* way a viewer
can distinguish "this broadcast is over, stop" from a transient drop (which
must keep today's reconnect behavior). Constant lives in `wire` (Go) and
`wire.ts` (TS) so both sides share one definition.

### ID helpers

New tiny package `gawk-server/internal/broadcastid`:

```go
const Alphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
const Length = 6
func Mint() (string, error)              // crypto/rand
func Normalize(s string) (string, error) // uppercase; error if not valid
```

## Server: hub → registry (`internal/hub`)

The current `Hub` (publisher slot, subscriber set, cached config + keyframe,
counters — `hub.go`) becomes the **per-broadcast unit**, unexported or
renamed as convenient; a new exported `Registry` owns the map. This is a
low-risk mechanical refactor: all per-broadcast semantics carry over
verbatim —

- `StartPublish` on a broadcast still invalidates *that broadcast's* caches
  (new publisher session ⇒ frameIDs reset, config may differ);
- caches persist while the broadcaster is merely away (within grace);
- per-subscriber bounded queue with non-blocking enqueue — slow subscribers
  drop, never block; cached config re-emitted before every keyframe chunk 0;
- `Options{MaxSubscribers, QueueDepth}` apply **per broadcast** (15 viewers
  *per broadcast* — the natural extension of today's global cap; a global
  cap is R2).

### Connection interface

The hub's session abstraction widens from `DatagramSender` to:

```go
type Conn interface {
    SendDatagram([]byte) error
    CloseWithError(code uint32, reason string) error
}
```

GC needs to force-close viewer sessions with `CloseCodeBroadcastEnded`; the
`uint32` keeps `hub` decoupled from webtransport-go. The transport layer
wraps `*webtransport.Session` in a 5-line adapter (its `CloseWithError`
takes a `webtransport.SessionErrorCode`, which is a distinct uint32 type).
Test fakes grow one method.

### Registry API

```go
var ErrNotFound = errors.New("hub: broadcast not found")   // + existing ErrPublisherActive, ErrFull

func NewRegistry(log *slog.Logger, opts Options) *Registry
// Options gains BroadcastGrace time.Duration (default 5m).

// StartPublish with id == "" mints a new broadcast and returns its ID.
// With a non-empty id it reclaims: ErrNotFound if unknown/expired,
// ErrPublisherActive if that broadcast already has a live publisher.
// Reclaim cancels a pending grace timer.
func (r *Registry) StartPublish(id string) (string, *Publisher, error)

// CheckSubscribe is the read-only pre-upgrade probe: ErrNotFound / ErrFull / nil.
func (r *Registry) CheckSubscribe(id string) error
// Subscribe is authoritative and re-checks under lock (races resolved here).
func (r *Registry) Subscribe(id string, conn Conn) (*Subscriber, error)

func (r *Registry) Stats() RegistryStats
```

IDs passed in are normalized via `broadcastid.Normalize` (invalid ⇒
`ErrNotFound`). One registry-level mutex is fine at this scale (~5
broadcasts × 15 viewers); do not build per-broadcast locking unless a test
proves contention.

### Lifecycle / GC

- Broadcast created by mint; deleted only by GC.
- When `Publisher.Close()` runs and the broadcast has no publisher, arm a
  `time.AfterFunc(grace, …)` stored on the broadcast.
- Reclaim (`StartPublish(id)`) stops the timer. `AfterFunc` stop races are
  real: the fired callback must re-check under the registry lock that the
  broadcast is still publisher-less (and still the same generation — a
  simple counter incremented on each successful `StartPublish` is enough)
  before acting.
- On fire: `CloseWithError(wire.CloseCodeBroadcastEnded, "broadcast ended")`
  on every subscriber conn, close subscribers, delete the registry entry,
  log with `broadcast_id`.
- Tests use a short grace (e.g. 50ms) via `Options.BroadcastGrace`; no clock
  injection needed if assertions poll (existing `waitFor` pattern).

### Stats

```go
type RegistryStats struct {
    Totals     TotalStats            `json:"totals"`     // broadcasts, subscribers, frames/datagrams relayed+dropped, badDatagrams (sums)
    Broadcasts map[string]Stats      `json:"broadcasts"` // keyed by broadcast ID
}
```

Per-broadcast `Stats` keeps today's fields and gains
`graceRemainingSeconds` (0 while a publisher is active). Note for R2: full
IDs in `/statusz` leak join capability — acceptable for the private homelab
deployment, revisit with the R2 trust model.

## Server: routes (`internal/transport/server.go`)

| Route | Behavior |
|-------|----------|
| `CONNECT /publish` | Mint. Upgrade first (nothing can fail pre-check), then `StartPublish("")`, then open uni stream and write `BroadcastAnnounce`, then the existing datagram receive loop. If the announce write fails, close the session (broadcaster can't share a code it never saw). |
| `CONNECT /publish/{id}` | Reclaim. `r.PathValue("id")` (Go 1.22 mux) → normalize → pre-upgrade checks like today's publish handler: 404 `ErrNotFound`, 409 `ErrPublisherActive`; claim **before** upgrade, release on upgrade failure. Announce still sent (client re-confirms its ID). |
| `CONNECT /subscribe/{id}` | 404 unknown ID, 429 full — both pre-upgrade via `CheckSubscribe`; after upgrade, `Subscribe` re-checks: a race lost to the subscriber cap closes with 429, one lost to GC (`ErrNotFound`) closes with `CloseCodeBroadcastEnded` (4000) so the viewer terminal-ends instead of burning its reconnect budget against a 404. |
| `/echo`, `GET /healthz` | Unchanged. |
| `GET /statusz` | Serializes `RegistryStats` (shape above). |
| `/publish`, `/subscribe` legacy ID-less semantics | **Gone.** `/subscribe` without an ID is 404. |

`cmd/gawk-server/main.go` swaps `hub.New(...)` for
`hub.NewRegistry(log, hub.Options{MaxSubscribers: cfg.MaxSubscribers, BroadcastGrace: cfg.BroadcastGrace})`.

### Config

`-broadcast-grace` / `GAWK_BROADCAST_GRACE`, default `5m`, following the
existing string→`time.ParseDuration` pattern (`keepalive-period`). Validate
> 0. Plumb into both Helm charts' values/env like the existing duration
flags (values key `broadcastGrace`).

## Frontend (`gawk-app/src`)

The only two endpoint-URL construction sites are `broadcaster.ts`
(`new URL('/publish', serverUrl)`) and `viewer.ts` (`new URL('/subscribe',
serverUrl)`); the ID slots in there. `connection.ts` is path-agnostic and
unchanged.

- **`wire.ts`**: `parseBroadcastAnnounce(bytes): string` mirroring the Go
  layout (DataView, golden vector above), plus
  `CLOSE_CODE_BROADCAST_ENDED = 4000`.
- **`broadcaster.ts`** (`BroadcastPipeline`): constructor takes an optional
  `broadcastId` — absent ⇒ dial `/publish` (mint), present ⇒
  `/publish/<id>` (reclaim). After connect, read the first stream from
  `wt.incomingUnidirectionalStreams`, concatenate to EOF, parse the
  announce, surface via a new `onBroadcastId(id)` callback. Reading is
  async and must not gate media start — capture/encode begins immediately;
  the ID shows up in the UI when it arrives. Reassembly note: a uni stream
  read may deliver the 9 bytes in multiple chunks; buffer to EOF before
  parsing. `start()` failures are typed (`BroadcastStartError.phase`:
  `'connect'` = no session was ever established; `'capture'` = the session
  was live and the pipeline has already torn it down — a leaked session
  here would be a zombie publisher holding the ID until the tab closes).
- **`viewer.ts`** (`ViewerPipeline`): dial `/subscribe/<id>`; report
  `wt.closed`'s `WebTransportCloseInfo` outward (new callback or extended
  stop-reason) so the session wrapper can see close codes. The datagram
  read loop and `wt.closed` settle in unspecified order on a server close
  and only `wt.closed` carries the close code, so read-loop termination
  must consult `wt.closed` (short race window) before being treated as an
  anonymous drop.
- **`viewer-session.ts`** (`ViewerSession`): the ID is baked into the URL
  the session already stores and reuses, so reconnect-rejoins-same-ID is
  free. New rule: a close with `closeCode === CLOSE_CODE_BROADCAST_ENDED`
  is **terminal** — new status `'ended'`, no reconnect attempts. Everything
  else keeps today's policy (backoff 1s→15s, 10 attempts, never-connected
  is fatal). Testable with the existing injected `PipelineFactory` /
  `FakePipeline` pattern.
- **`App.tsx`**: replace the exact-match hash switch with a split parse so
  `#/view/<id>` extracts the ID and passes it to `ViewPage`. `#/view`,
  `#/broadcast`, `#/loopback` keep working; nav active-state matches on the
  route prefix.
- **`ViewPage.tsx`**: code input (uppercase + strip whitespace + validate
  charset/length before enabling Watch). If the URL carried an ID:
  auto-join via `useEffect` (guard against double-start under React strict
  mode). Joining rewrites the hash to `#/view/<id>` so the URL is shareable
  mid-watch. New UI states: `'ended'` ("broadcast ended") — terminal, and
  the never-connected failure message becomes "couldn't join — check the
  code" (the HTTP status behind a failed CONNECT is invisible to JS, so
  404/429/bad-cert are indistinguishable; this is a known platform gotcha,
  see docs/05).
- **`BroadcastPage.tsx`**: on `onBroadcastId`, show the code prominently +
  a copy-link button (`${location.origin}${location.pathname}#/view/<id>`).
  Keep the ID in page state; Stop→Start within the page session passes it
  to `BroadcastPipeline` for **reclaim**, so viewers survive a publisher
  restart (preserves the D1 behavior). If the reclaim **dial** fails
  (`phase === 'connect'`: expired ⇒ 404, zombie session ⇒ 409 —
  indistinguishable in JS), automatically fall back to a fresh mint and
  show a note: "previous broadcast expired — new code minted". Any
  post-connect failure (e.g. the user cancels the share picker) surfaces
  as an error on the same ID instead — silently minting there would
  abandon the reclaimed broadcast and re-prompt the picker.

State: the active broadcast ID can live in page-local state; do not add it
to the persisted `transportStore` (IDs are ephemeral by design — a stale
persisted ID is a footgun). `pipelineStore` appears unused by these pages;
leave it alone.

## Milestones → delegable chunks

Each chunk = PR-sized, this document is its contract. Deps form a DAG;
E-chunks and F1 can start immediately after their listed deps.

### Milestone E — server: registry, lifecycle, routes

| # | Chunk | Deps | Acceptance criteria |
|---|-------|------|---------------------|
| E1 | `wire`: `TypeBroadcastAnnounce` + append/parse, `CloseCodeBroadcastEnded`; new `internal/broadcastid` (Mint/Normalize/Alphabet) | — | Round-trip tests + the golden vector `0103064b375851324d` (ID `K7XQ2M`); parse errors on short buf/bad version/bad type/idLen overrun/invalid chars; fuzz — parse never panics. `broadcastid`: Mint yields 6 chars of the alphabet, ~uniform over 10k samples (no χ² needed — just every-symbol-appears); Normalize uppercases, rejects wrong length/charset |
| E2 | `hub`: `Registry` of per-broadcast hubs; `Conn` interface (SendDatagram + CloseWithError); `StartPublish(id)`, `CheckSubscribe`/`Subscribe`, `RegistryStats` | E1 | All existing hub semantics tests pass rewired through a registry (cache invalidation on new publisher, config-before-keyframe, slow-sub drops, per-broadcast 16th-subscriber ErrFull); **isolation test**: two broadcasts, each with fake subs — datagrams never cross IDs, stats independent; `ErrNotFound` for unknown/invalid IDs; `-race` clean |
| E3 | Lifecycle: grace timer on publisher close, reclaim cancels, expiry closes subs with 4000 + deletes entry; `-broadcast-grace` flag + Helm values | E2 | With 50ms grace: publisher closes → subs get `CloseWithError(4000, …)` and entry is gone; reclaim within grace → timer canceled, entry survives, viewers uninterrupted (caches **reset** — a reclaim is a new publisher session, frameIDs restart; only *joining while the broadcaster is away* is served from the old caches); reclaim after expiry → `ErrNotFound`; timer/reclaim race covered by a generation-check test; config test covers flag/env/default + rejects `0`; `-race` clean |
| E4 | Routes: `/publish` mint + announce uni stream, `/publish/{id}` reclaim, `/subscribe/{id}`, legacy removal, `/statusz` new shape | E3 | Go integration tests (existing harness): publish → client reads announce, ID is valid; two concurrent publisher→subscriber pairs relay independently (no cross-talk); subscribe to bogus ID → 404 pre-session; 16th subscriber on one broadcast → 429 while the other broadcast still accepts; reclaim keeps a live viewer's stream flowing across publisher restart; GC (short grace) closes a live viewer with code 4000 and `/subscribe/<id>` then 404s; `/statusz` shows `totals` + per-ID entries and `graceRemainingSeconds` moves; ID-less `/publish`//`/subscribe` → 404 |

### Milestone F — frontend

| # | Chunk | Deps | Acceptance criteria |
|---|-------|------|---------------------|
| F1 | `wire.ts` announce parse + close-code constant; pipeline URL changes; broadcaster announce-stream read → `onBroadcastId` | E1 (vectors) | Vitest: golden vector parses to `K7XQ2M`; malformed inputs throw; announce assembled from a multi-chunk stream read; `BroadcastPipeline` builds `/publish` vs `/publish/<id>`; `ViewerPipeline` builds `/subscribe/<id>` |
| F2 | `ViewerSession`: terminal `'ended'` on close code 4000, reconnect otherwise; close-info plumbed from `ViewerPipeline` | F1 | Vitest with `FakePipeline`: close info `{closeCode: 4000}` → status `'ended'`, zero reconnect attempts; other close → existing backoff/attempt behavior unchanged (existing tests still green); reconnect dials the same `/subscribe/<id>` URL |
| F3 | Routing + ViewPage: hash param parse, code field (normalize/validate), auto-join from URL, hash rewrite on join, `'ended'`/"couldn't join" states | F2 | Manual + unit where cheap: `#/view/AB2CD3` auto-joins; typed lowercase code normalized and joins; invalid code disables Watch; broadcast-ended shows terminal state with no retry spinner; `#/broadcast`/`#/loopback` unaffected |
| F4 | BroadcastPage: show ID + copy join link on announce; Stop→Start reclaims same ID; reclaim-failure → auto-mint + note | F1 | Manual: start → code + link appear, link opens a joined viewer tab; stop/start within grace → viewer tab keeps playing (new keyframe within ~2s), same code; reclaim of an expired ID falls back to a new code with the "expired" note |

### Milestone G — close-out

| # | Chunk | Deps | Acceptance criteria |
|---|-------|------|---------------------|
| G1 | Manual multi-broadcast browser verify (docs/05 runbook env) | E4, F3, F4 | Two broadcasters (separate machines/profiles) each with ≥2 viewers via join links — streams independent; broadcaster restart within grace: viewers recover on the same code; kill a broadcaster past grace: its viewers show "broadcast ended", the other broadcast unaffected; `/statusz` matches reality throughout |
| G2 | Docs sync: gotchas → README, `ROADMAP.md` R1 → done, `CLAUDE.md` updated, Progress table above filled in | G1 | README gotcha list + CLAUDE.md architecture/status sections reflect multi-broadcast routes and the announce protocol; this doc's Progress table complete |

Release note: E4 and F1 land the protocol break — server and app must
release together (the lockstep release-please flow already does this);
between merging E4 and deploying both, the deployed pair stays consistent
because deploys are per-release, not per-merge.

## Verification strategy

- **Unit (no network)**: wire round-trips + fuzz (Go and TS), broadcastid,
  registry semantics with fake `Conn`s under `-race`, `ViewerSession` policy
  via `FakePipeline`.
- **Automated Go E2E**: transport integration tests over real
  HTTP/3-loopback (existing harness) — the E4 criteria are the gate.
- **Manual browser**: once, at G1 — the same cadence every prior milestone
  used.

## Risks / watch-items

- **Announce vs. first datagrams ordering**: QUIC gives no cross-stream/
  datagram ordering; the broadcaster may see media flowing before the
  announce arrives. By design nothing waits on the announce except the UI
  code display — keep it that way (no logic keyed on "announce received").
- **Uni-stream reads on the broadcaster in Firefox**: the happy path is a
  Chromium broadcaster; if Firefox broadcasting is ever exercised, verify
  `incomingUnidirectionalStreams` behavior there (viewers are unaffected —
  they never read streams).
- **Reclaim/GC races**: the `AfterFunc` callback may fire concurrently with
  a successful reclaim; the generation check under the registry lock is the
  guard — E3 has an explicit test.
- **Opaque connect failures**: JS cannot see 404 vs 409 vs 429 vs cert
  errors on a failed CONNECT (`WebTransportError` hides the status — known
  since D1). All viewer/broadcaster failure UX must be written assuming
  only "it didn't connect" is knowable pre-session; only post-session close
  codes (4000) carry semantics.
- **`/statusz` exposes join-capable IDs** — fine for the private homelab,
  explicitly revisit in R2's trust model.

## Post-implementation review (2026-07-13)

A full code review of the R1 implementation (commit `b8eb374`) against this
document found and fixed six issues; each bug fix landed **test-first** (a
failing test reproducing the bug, then the fix). The general lessons are
distilled into [`CODE-REVIEW.md`](../CODE-REVIEW.md); the R1-specific
outcomes, now part of the contract above, were:

1. **Reclaim fallback fired on *any* `start()` failure and leaked the
   session** — cancelling the share picker after a successful reclaim dial
   minted a new ID, re-prompted the picker, and left a zombie publisher
   holding the old ID until the tab closed. Fixed with
   `BroadcastStartError.phase` + pipeline-internal teardown
   (`broadcaster.test.ts`).
2. **The announce read gated media start** — violating the locked decision
   above; a missing announce stream hung `start()` forever. The read is now
   a detached task (`broadcaster.test.ts`).
3. **Close code 4000 could lose the settle-order race** — if the datagram
   read loop settled before `wt.closed`, viewers reconnect-looped instead of
   showing "broadcast ended". `viewer.ts` now consults `wt.closed` on
   read-loop end (`viewer.test.ts`; also a README gotcha).
4. **ViewPage auto-rejoined the stale hash ID** while typing a new code
   after Stop (state-keyed effect re-runs). Auto-join now fires only on
   mount and real hashchange events (`ViewPage.test.tsx`).
5. **Post-upgrade subscribe race lost to GC closed with 429** instead of
   4000 (route table above updated; `TestSubscribeLostRaceToGCClosesWithBroadcastEnded`).
6. **`broadcastGrace` was missing from the Helm chart** despite being an E3
   acceptance criterion.

Also closed: the acceptance-criteria test gaps (E3 generation-race test,
pipeline URL/announce tests, E4 two-broadcast network isolation +
`graceRemainingSeconds` movement) and a stats leak (per-subscriber drop
counters of subscribers still live at GC time vanished from `/statusz`
totals). Note the E3 criterion above originally read "caches intact" on
reclaim — ambiguous; the implemented (and correct, v0.4-consistent) rule is
that a reclaim resets the caches like any new publisher session.
