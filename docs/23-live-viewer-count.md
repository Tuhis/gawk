# R18 — Live Viewer Count

Design doc for [ROADMAP R18](../ROADMAP.md#r18--live-viewer-count)
(designed 2026-07-18; **not yet implemented** — chunk breakdown Y1–Y6 below).
Both the broadcaster and every viewer of a stream see a live **"N watching"**
figure that updates promptly as viewers join and leave. This is the
`SubscriberCount` message deliberately deferred from R14 (Decision 18) and
named as future work in `docs/19`, `docs/10`, `CLAUDE.md`, and the roadmap.

**Numbering**: this is `docs/23`, reserved for R18 all along (R19 took
`docs/24`). Wire allocations already spent: `0x07`/`0x08` (R15 audio,
reserved), `0x09` (R17 resume token), `0x0A` (R19 reliable carrier). R18
takes the next free byte, **`0x0B`** (Decision 2).

**The headline design problem is cluster mode.** In single-pod mode the count
is `len(b.subs)` and the whole feature is plumbing. Under R17 relay scale-out
(`docs/22`) a broadcast fans out across N pods through an origin/edge cascade,
and the naïve count is wrong twice over: an **edge is not a viewer** (counting
it inflates the number by the number of edges), and the origin **cannot see
the real viewers behind an edge** (counting only its own locals deflates the
number to a fraction of the truth). Getting *only real viewers, counted
exactly once, summed across the fleet* is the substance of this doc
(Decision 5).

## Goal

A broadcaster (browser or native) sees how many people are currently watching,
in the viewer-count slot `docs/10` already reserved in the production UI (it
renders nothing today). Every viewer sees the same number as a live badge. The
figure is **eventually-consistent and live-ish** — correct within ~1–2 s of a
join/leave — not a hard real-time counter, and that is the right contract for
a "N watching" indicator.

Non-goals up front: no viewer identities, names, or presence list (just a
number); no historical/peak analytics (that stays `/statusz` + Prometheus);
no per-viewer adaptation.

## Background: what exists, what's missing

**The count already exists server-side.** `broadcastHub.subs` is the live
subscriber set; `externalSubsLocked()` (`internal/hub/hub.go`) already counts
**local viewers only**, excluding R17 edge sessions via the `Subscriber.internal`
flag. It surfaces today in `/statusz` (`Stats.Subscribers`,
`Stats.EdgeSessions` kept separate) and the `gawk_broadcast_subscribers`
Prometheus gauge. **What is missing is a path to push the number to clients.**

Three pieces of plumbing R18 needs already exist in embryonic form — R18 is
mostly *composing* them, not inventing mechanism:

1. **Relay → subscriber fan-out with cache + join-prime + invalidate.** The
   R5 `ClockMapping` (`0x06`) is exactly this shape: `relayClockMapping`
   caches the latest datagram (`cachedClockMapping`), fans it out live
   (`fanOutLocked`), primes late joiners in `subscribe()`, and clears it on a
   new publisher session. The viewer count reuses this template verbatim
   (Decision 3).
2. **Relay → publisher datagram writes.** The origin already writes datagrams
   *back* to the broadcaster: the publisher read loop answers TimeSync pings
   inline with `sess.SendDatagram` (`maybeAnswerTimeSync`, `server.go`). The
   channel to the broadcaster exists; R18 adds a *server-initiated* write on
   it (Decision 4) — the one genuinely new relay piece.
3. **Edge → origin datagram writes over the upstream session.** An edge's
   `pump` (`transport/edge.go`) already sends datagrams *up* to its origin —
   TimeSync pings every 2 s — and the origin's `handleInternalSubscribe` read
   loop already reads and answers them. That same session and read loop carry
   the edge's viewer-count report upward (Decision 5).

## The cluster-mode counting model (Decision 5, stated up front)

```
                         ┌────────────── ORIGIN pod O ──────────────┐
   broadcaster ─publish→ │ hub(B), role=origin                      │
        ▲                │   real local viewers:  externalSubs = a  │
        │ push global    │   edge sessions (internal subs):         │
        │ count = G      │     E1.report = e1   E2.report = e2      │
        └──────────────  │   G = a + e1 + e2   ← global total       │
                         └───┬───────────────────────────┬──────────┘
             fan G down to   │ internal sub              │ internal sub
             local viewers   ▼                           ▼
                    ┌──── EDGE pod E1 ────┐      ┌──── EDGE pod E2 ────┐
                    │ hub(B), role=edge   │      │ hub(B), role=edge   │
                    │ real viewers = e1   │      │ real viewers = e2   │
   report e1 up ────┤ (reports e1 up;     │      │ (reports e2 up;     ├─── report e2 up
                    │  forwards G down)   │      │  forwards G down)   │
                    └── viewers see G ────┘      └── viewers see G ────┘
```

**Invariant — only real viewers, counted once.** A "real viewer" is a session
that subscribed via `/subscribe/{id}` (`internal == false`). An edge session
(`/internal/subscribe/{id}`, `internal == true`) is fan-out plumbing and is
**never** counted as a viewer. The global count is

```
G(B) = Σ over all pods P of externalSubs(B, P)
```

Each real viewer connects to exactly one pod and is counted exactly once, on
that pod. The edge cascade has depth ≤ 2 by R17 construction (generation
fencing — an edge can never serve `/internal/subscribe`, so it never has
downstream edges), so an edge's whole subtree is just its own local viewers.
The aggregation is therefore one level deep: **each edge reports its local
`externalSubs` up; the origin adds its own and sums.** No tree walk, no
cluster clock, no shared store — the same "self-federate over the existing
WebTransport protocol" posture as the rest of R17.

Everything a viewer sees is the origin's **global** `G`, so a viewer on an
edge sees the same "N watching" as one on the origin. Edges forward `G`
downward verbatim (Decision 5c); they do not compute their own local number
for display.

## Direction survey (aggregation transport)

| Option | Sketch | Verdict |
|--------|--------|---------|
| **A — Edge→origin reports over the existing upstream session** | Each edge sends a `ViewerCount` datagram up its `/internal/subscribe` session (TimeSync-ping precedent); origin aggregates and fans the global total back down + to the broadcaster | **Chosen** — zero new infrastructure, reuses the session/read-loop/fan-out that already exist, event-scoped to the pods actually involved in the broadcast, and byte-identical to single-pod when there are no edges |
| B — Shared k8s store (annotate the Lease / a per-broadcast ConfigMap with each pod's count) | Every pod writes its local count to a shared object; the origin reads and sums | Rejected — k8s API write churn on every join/leave (the reconnect-storm amplifier the debounce exists to avoid), a new failure mode (API unavailability desyncs the count), and laggier than the in-band path for no benefit. R17 deliberately kept per-broadcast coordination to one tiny Lease; this would balloon it |
| C — Origin scrapes peers' `/statusz` | Origin periodically pulls every pod's `/statusz` and filters for the broadcast | Rejected — needs peer discovery the cascade doesn't otherwise require, pulls the whole fleet's stats to read one number, and the origin already has a direct session to exactly the edges that matter (Option A) |
| D — Gossip / NATS backplane | Counts propagate over a message bus | Rejected — R17's survey already rejected a backplane (docs/22); reintroducing one for a viewer number is absurd overkill |

## Decisions

1. **What counts: real, registered subscribers — nothing else.** The count is
   `externalSubs` (non-internal members of `b.subs`) aggregated per the model
   above. Explicitly **excluded**, each for a concrete reason:
   - **The broadcaster's own preview** — the broadcaster is a publisher, never
     a member of `b.subs`. Nothing to exclude; it structurally can't be
     counted.
   - **Edge sessions** — `internal == true`, already excluded by
     `externalSubsLocked()`. This is the cluster-mode correctness core.
   - **A connecting-but-not-yet-subscribed session** — not in `b.subs` until
     `Registry.Subscribe` completes after the upgrade, so a session mid-dial
     or mid-`CheckSubscribe` isn't counted. A viewer appears in the number the
     instant it can actually receive frames, which is the intuitive moment.
   - **An evicted/closed zombie** — removed from `b.subs` by `Subscriber.Close`
     (incl. the R10 4001 eviction), so it drops out at close.
   - The count **includes the viewer themselves**: a lone viewer sees
     "1 watching". A "besides you" presentation is a pure UI choice
     (Decision 8) made on the client from the raw number; the wire carries the
     honest total.

2. **Wire: one new relay-originated datagram message, `TypeViewerCount`
   (`0x0B`), fixed 6 bytes, golden-vectored in all three mirrors.** Layout,
   modeled on `ClockMapping`/`TimeSync`:

   ```
   0x0B ViewerCount (ViewerCountSize = 6 bytes):
     byte 0      uint8   Version (0x01)
     byte 1      uint8   TypeViewerCount (0x0B)
     bytes 2-5   uint32  count (big-endian)
   ```

   `uint32` is deliberate overkill for a ~15-viewer homelab (a `uint16` would
   do), chosen to match the codebase's frameID/counter integer conventions and
   leave zero doubt about overflow; the 4 extra bytes are free. `AppendViewerCount`
   / `ParseViewerCount` mirror the strict fixed-length `ClockMapping` parsers
   (exact length, version, type checks; `ErrBadLength` on mismatch). Golden
   vectors added to `gawk-server/wire/wire_test.go`,
   `gawk-app/src/transport/wire.test.ts`, and
   `gawk-broadcast/internal/wirecheck` so the Go relay, the browser
   viewer/broadcaster, and the native broadcaster stay byte-identical (R14
   Decision 1). **No existing message, datagram layout, or close code changes.**

   *Reused in both directions, disambiguated by which read loop receives it* —
   exactly how TimeSync reuses one type for ping and pong:
   - **Relay → viewers/broadcaster**: `count` = the global total `G`.
   - **Edge → origin** (up the internal session): `count` = that edge's local
     `externalSubs`.

   The message is **only ever produced by a relay** (origin aggregating, or
   edge reporting up). Viewers and broadcasters *parse* it and never append it
   — so a `ViewerCount` arriving on a route where a client is the sender is
   always illegitimate and dropped (Decision 6).

3. **Relay → viewer fan-out: reuse the `ClockMapping` cache/prime/invalidate
   template exactly.** Add `broadcastHub.cachedViewerCount []byte`. The emit
   path (Decision 4) caches the latest global-count datagram and fans it out
   with `fanOutLocked` (so it reaches local viewers *and* edge sessions —
   which is how edges receive `G` to forward). `subscribe()` primes a late
   joiner from `cachedViewerCount` alongside `cachedClockMapping`, so a viewer
   sees the count immediately on join without waiting for the next tick.
   `InvalidatePrimes` and `claimPublisherLocked` clear it alongside the other
   caches — a stale count from a dead origin or prior session must not linger
   (harmless if it did — the count isn't tied to the frame timeline — but
   clearing it keeps the cache lifecycle uniform).

4. **Relay → publisher push: a hub-held sender + a low-rate registry count
   pump. The one genuinely new piece.** Today `hub.Publisher` holds no session
   handle; the relay only ever *reads* from the publisher. R18 gives the hub
   an optional `publisherSend func([]byte)` that `handlePublish` registers
   after the upgrade (`func(b []byte) { _ = sess.SendDatagram(b) }`) and clears
   on `Publisher.Close`. `sess.SendDatagram` is goroutine-safe in quic-go, so
   the pump calling it concurrently with the read loop's TimeSync replies is
   fine (network I/O is done off the registry lock, like keyframe writes).

   The **count pump** is a single registry-wide goroutine ticking at a
   constant `viewerCountInterval` (**1 s**, a constant like `timeSyncReplyRate`
   / `drainWindow` — *not* a user knob, so the R2 "does the flag reach
   production?" rule doesn't apply; there is no flag). Each tick, for every
   **origin** hub with an active publisher:
   - compute `G = externalSubsLocked() + Σ (internal subs' reported downstream
     count)`;
   - if `G` changed since the last emit, **or** a keepalive interval (**5 s**)
     elapsed, build the `ViewerCount(G)` datagram once, then: cache it
     (`cachedViewerCount`), `fanOutLocked` it, and `publisherSend` it.

   Edge hubs are **skipped** by the pump — they don't aggregate and have no
   real broadcaster to push to (their `publisherSend` is nil; they receive `G`
   from upstream and forward it via Decision 5c). Why a periodic pump instead
   of emitting inline on every `Subscribe`/`Close`:
   - **Storm resistance for free.** The roadmap's explicit worry is a
     reconnect storm spamming both ends. A fixed ≤ 1-emit/s/broadcast cadence
     makes that structurally impossible without threading a debounce timer
     through the hot subscribe/close paths.
   - **Loss recovery.** Datagrams are lossy; the 5 s keepalive re-emit repairs
     a dropped update for already-connected viewers (new joiners are covered
     by the join-prime cache).
   - **Simplicity.** One goroutine for the whole registry, no per-hub timers,
     no callbacks in `Subscribe`/`Close`. Tests drive a single exported
     "recompute-and-emit one tick" seam rather than racing a background loop;
     the goroutine is started explicitly (like the cluster coordinator), never
     from `NewRegistry`, so existing tests stay goroutine-free.

   Cost: change latency is ~1 s (one tick) for an origin-local join and ~1–2 s
   for an edge join (edge report tick + origin emit tick). Well inside
   "updates promptly" for a watcher count; the responsiveness/simplicity trade
   is deliberate.

5. **Cluster-mode aggregation — the substance.**
   - **(a) Edge reports its local count up.** The edge `pump` already runs a
     1 s linger-check ticker; piggyback on it. Each tick, compute
     `registry.ExternalSubscribers(id)` and, if it changed since the last
     report (or a keepalive interval passed), `up.SendDatagram(AppendViewerCount(nil, n))`.
     No new goroutine, no new cadence.
   - **(b) Origin records per-edge reports and aggregates.** The origin's
     `handleInternalSubscribe` read loop owns exactly one `sub` (that edge's
     internal `Subscriber`). When a `ViewerCount` arrives there, it stores the
     value on that subscriber (`Subscriber.downstreamViewers`, an
     `atomic.Uint64`, guarded/read under the registry lock in the pump). The
     pump's `G` sums these across the origin's internal subs. An edge that has
     not yet reported contributes 0 until its first report — a brief undercount
     on fresh attach, healed within a tick or two (acceptable for a live-ish
     number; noted).
   - **(c) Edge forwards the global total down.** When the edge's `pump`
     datagram loop receives a `ViewerCount` from the origin, it routes it to
     `pub.HandleDatagram` like any other non-TimeSync/non-ClockMapping datagram
     (it needs **no** per-hop rewrite — a count is pod-independent, unlike
     `ClockMapping`). `HandleDatagram` gains a `TypeViewerCount` case that,
     **only on an edge hub** (`b.edge == true`), caches + fans it to the edge's
     local viewers (same helper as Decision 3). The `b.edge` gate is what makes
     a stray `ViewerCount` from a *real broadcaster* on an origin hub a no-op
     (Decision 6) while letting an edge forward the origin's authoritative
     total.
   - **(d) No loops, no double-counting.** `G` flows strictly downward (origin
     → edge → viewers); local counts flow strictly upward (edge → origin). The
     two directions carry different values and never cross. Each real viewer is
     summed once, on its own pod. Edge sessions are summed **never** (they are
     `internal`).
   - **(e) Re-home resilience.** On an R17 origin move, edges re-resolve and
     re-attach to the new origin (`edge.go`), re-reporting their counts within
     a tick; the demoted origin's `publisherSend` is cleared when its stale
     publisher session closes. A transient undercount for ~1–2 s during the
     handover is expected and self-heals — the same "live-ish" tolerance as
     everywhere else here.

6. **Direction/authenticity: a `ViewerCount` is only trusted from a relay
   peer.** Guarded by *where* it arrives, with no new field on the wire:
   - **Origin publisher read loop** (`handlePublish`): a `ViewerCount` from the
     broadcaster is ignored. `maybeAnswerTimeSync` returns false for it, it
     falls to `pub.HandleDatagram`, and the `TypeViewerCount` case there is
     gated on `b.edge` (false for an origin) — so a malicious/buggy broadcaster
     cannot spoof the audience number.
   - **Origin internal-subscribe read loop** (`handleInternalSubscribe`): a
     `ViewerCount` is an edge's downstream report → updates that edge sub's
     `downstreamViewers` (Decision 5b). This is the *only* place a client-sent
     `ViewerCount` is accepted, and the peer is an authenticated edge (PSK +
     generation fence, `docs/22`).
   - **Normal viewer read loop** (`handleSubscribe`): viewers send only
     TimeSync; a `ViewerCount` from a viewer is ignored exactly as any other
     non-TimeSync datagram is today.

7. **Broadcaster interception (browser + native).** Both broadcasters already
   run a relay-datagram read loop that today only catches TimeSync replies:
   - **Browser** (`gawk-app/src/transport/broadcaster.ts`, the `readDatagrams`
     loop): add a `ViewerCount` branch → set `BroadcastStats.viewerCount`.
     `BroadcasterScreen` renders it in the `docs/10` reserved slot; the stats
     overlay gets a "Watching" row.
   - **Native** (`gawk-broadcast/internal/engine/engine.go`, `readDatagrams`):
     same branch → `engine.Stats.ViewerCount` + a new `Callbacks.OnViewerCount`;
     the Gio GUI surfaces it. This removes the two "deliberately absent —
     nothing on the wire tells a publisher about subscribers" comments in
     `engine.go` and the GUI `main.go` that R14 left as breadcrumbs.
   - **"First viewer joined" falls out for free.** R14/`docs/19` wanted a
     critical-urgency notification when the first viewer arrives (KDE
     suppresses normal notifications while screen-casting). That is just the
     count crossing **0 → ≥1**, derivable client-side from `OnViewerCount` — no
     separate wire signal. Whether the native app rings it is an R14-followup
     UI choice; R18 supplies the signal.

8. **Viewer UI.** The reassembler (`gawk-app/src/transport/reassembler.ts`)
   gains a `TypeViewerCount` case mirroring `pushClockMapping` → a new
   `onSubscriberCount` callback (`viewer.ts`) → `ViewerStats.viewerCount` → a
   stats-overlay Delivery row **and** a prominent, tasteful "N watching" badge
   in `ViewerScreen` (monochrome design system, e.g. an eye glyph + count
   pill). The badge is the one production-visible surface; keeping it
   understated matches the cinematic viewer. `#/debug/*` surfaces get the
   overlay row only.

9. **Observability.** The **local** per-pod count is already
   `gawk_broadcast_subscribers` / `Stats.Subscribers`; Prometheus sums it
   across pods (`sum(gawk_broadcast_subscribers) by (broadcast)`) to the true
   global — R18 needs no new *aggregate* metric there, only the in-band count
   reaching clients. For debugging the pushed value directly, add an
   **origin-only** gauge `gawk_broadcast_viewers_global` (the `G` the origin
   emits; absent/zero on edge hubs) via the existing snapshot collector
   (`Registry.Stats()` stays the one source of truth — new `Stats.ViewersGlobal`
   field, populated only for origin hubs). `/statusz` already reports
   `subscribers` + `edgeSessions` per pod; add `viewersGlobal` on the origin so
   an operator can read the number the broadcaster sees. `docs/13`'s bottleneck
   playbook gains a one-line "broadcaster shows fewer viewers than expected →
   check edge reports / origin aggregation" row.

10. **v1 stays a bare number.** No "first viewer joined" *wire* signal
    (Decision 7 derives it), no presence list, no names, no peak/history. A
    prominent badge vs overlay-only was a roadmap open question → **both**
    (Decision 8): a subtle production badge for the viewer + the reserved
    broadcaster slot, plus overlay rows on both surfaces for the exact number.

## End-to-end path

```
Single-pod:
  Registry count pump (1 s):
    origin hub: G = externalSubs
      → cache cachedViewerCount
      → fanOutLocked  → local viewers' reassembler → onSubscriberCount → badge
      → publisherSend → broadcaster readDatagrams → BroadcastStats.viewerCount
  join: subscribe() primes cachedViewerCount to the new viewer immediately

Cluster (origin O, edges E1..Ek):
  each Ej pump (1 s tick): report externalSubs(B) up  ──ViewerCount──▶ O
  O handleInternalSubscribe read loop: store Ej.downstreamViewers
  O count pump (1 s): G = O.externalSubs + Σ Ej.downstreamViewers
      → publisherSend G to broadcaster
      → fanOutLocked G to O's local viewers AND to E1..Ek (internal subs)
  each Ej pump datagram loop: receive G ──▶ pub.HandleDatagram
      → (b.edge) cache + fan G to Ej's local viewers
  every viewer, on O or any Ej, sees the same global G
```

## Status

| Chunk | Scope | Acceptance criteria | Status |
|-------|-------|---------------------|--------|
| Y1 | **Wire** — `TypeViewerCount` (`0x0B`), `ViewerCountSize = 6`, `AppendViewerCount`/`ParseViewerCount` (strict fixed length, à la `ClockMapping`); golden vectors in all three mirrors | Byte-identical Go↔TS↔wirecheck golden vectors; parser rejects wrong length/version/type with `ErrBadLength`/`ErrBadVersion`/`ErrBadType`; round-trip property test | 📋 not started |
| Y2 | **Relay single-pod** — `cachedViewerCount` + emit helper (ClockMapping template); registry count pump (1 s tick, 5 s keepalive, change-driven), started explicitly not in `NewRegistry`; `publisherSend` registered by `handlePublish`, cleared on `Publisher.Close`; join-prime in `subscribe()` | Hub test: single broadcaster + viewers, count reaches viewers (fan-out) and broadcaster (push) and equals `externalSubs`; join primes immediately; a reconnect storm (rapid subscribe/close) emits ≤ 1/s (storm resistance); keepalive re-emits an unchanged count within 5 s; broadcaster preview never counted; count excludes an evicted/closed sub | 📋 not started |
| Y3 | **Relay cluster aggregation** — `Subscriber.downstreamViewers`; edge report on the `pump` linger ticker (change-driven + keepalive); origin `handleInternalSubscribe` records reports; pump `G = externalSubs + Σ downstream`; edge `HandleDatagram` `TypeViewerCount` case gated on `b.edge`, caches + fans down; `InvalidatePrimes`/`claimPublisherLocked` clear `cachedViewerCount` | Multi-pod (origin + ≥1 edge, fakes): `G` == total real viewers across pods; an edge session is **never** counted (add an edge, `G` unchanged; add a viewer behind it, `G` +1); every viewer (origin + edge) observes the same `G`; a `ViewerCount` from a broadcaster on an origin is a no-op (spoof guard); re-home re-reports within a tick and `G` reconverges; single-pod path byte-identical (no edges ⇒ Decision 5 dormant) | 📋 not started |
| Y4 | **Browser broadcaster + viewer** — `broadcaster.ts` read-loop branch → `BroadcastStats.viewerCount` + `docs/10` reserved slot + overlay row; `reassembler.ts` `TypeViewerCount` case → `onSubscriberCount` → `ViewerStats.viewerCount` → `ViewerScreen` badge + overlay row | Unit: reassembler routes `ViewerCount` to `onSubscriberCount`, ignores malformed (counts bad, no crash); broadcaster read loop updates the stat without disturbing TimeSync; badge renders the live number; `#/debug/*` overlay-only; count of 1 renders "1 watching" | 📋 not started |
| Y5 | **Native broadcaster (R14)** — `engine.go readDatagrams` branch → `engine.Stats.ViewerCount` + `Callbacks.OnViewerCount`; Gio GUI surface; remove the two "deliberately absent" comments; 0→1 critical-urgency "first viewer" notification (godbus) | Engine test: a `ViewerCount` datagram updates `Stats` + fires `OnViewerCount`; GUI shows the number; wirecheck golden vectors (from Y1) pass; 0→1 transition fires exactly one notification, ≥1→≥1 changes do not | 📋 not started |
| Y6 | **Observability + verification** — origin `Stats.ViewersGlobal` + `gawk_broadcast_viewers_global` gauge + `/statusz` `viewersGlobal`; `docs/13` playbook row; manual single-pod + multi-pod (kind) verify; README/ROADMAP/CLAUDE status sync | Gauge emitted only on origin hubs, bounded cardinality (obfuscated broadcast label, no per-viewer labels — R9 rule); `/statusz` shows local `subscribers`, `edgeSessions`, and origin `viewersGlobal`; manual: browser + native broadcaster both show a correct live count as viewers join/leave, single-pod and across a 2-pod kind cluster (origin + edge), edge viewers counted, edge sessions not | 📋 not started |

**Ordering**: Y1 → Y2 give a working single-pod feature (verifiable end-to-end
with the browser broadcaster once Y4 lands, or a hub test before it). Y3 adds
cluster correctness — the load-bearing chunk — and is independently testable
with fakes. Y4/Y5 are the client surfaces (parallelizable). Y6 rides last.
Nothing here blocks or is blocked by R19 (merged/opt-in) — R18 touches the
fan-out/emit path R19's carrier drain also touches, and allocates its type
byte (`0x0B`) after R19's `0x0A`.

## Verification plan (manual, Y6)

1. **Single-pod, browser broadcaster**: open a broadcast, join with 0→3
   viewers in separate tabs; the broadcaster's reserved slot and each viewer's
   badge track the count within ~1 s; close viewers and watch it fall; the
   broadcaster's own preview never adds to the number.
2. **Single-pod, native broadcaster (R14)**: same, via the Gio GUI; confirm
   the 0→1 critical notification fires once (screen-casting active, KDE) and
   that steady changes don't spam notifications.
3. **Cluster (2-pod kind, `-cluster-mode`)**: force a viewer onto a non-origin
   pod (edge pull), confirm the broadcaster's number includes edge viewers,
   the edge session itself is not counted (`/statusz`: origin `edgeSessions` ≥ 1,
   `viewersGlobal` == total real viewers), and every viewer sees the same
   global number.
4. **Re-home**: trigger an R17 origin move mid-session; the count dips briefly
   then reconverges within ~1–2 s; no stuck/ghost counts.
5. **Storm**: hammer connect/disconnect; verify ≤ ~1 emit/s/broadcast on the
   wire (packet capture or `/statusz` deltas) and no broadcaster/viewer badge
   thrash.
6. Record findings + any constant changes (`viewerCountInterval`, keepalive) in
   this doc.

## Non-goals

- **Viewer identity, names, or a presence list** — just a number.
- **Historical / peak analytics** — `/statusz` + Prometheus territory.
- **A separate "first viewer joined" wire message** — derived client-side from
  the 0→1 transition (Decision 7).
- **Hard real-time exactness** — the count is eventually-consistent within
  ~1–2 s; a live-ish watcher number does not need sub-second precision, and
  chasing it would reintroduce the storm problem the debounce exists to kill.
- **Per-viewer adaptation from the count** — unchanged non-goal.
- **Counting reliable (R19) vs datagram viewers separately for display** —
  they are all real viewers; the `reliable` split stays in `/statusz`/metrics.

## Rejected

- **Shared k8s store / peer `/statusz` scrape / gossip backplane for
  aggregation** — survey table; Option A (in-band edge reports) wins on every
  axis for this scale.
- **Counting `len(b.subs)` naïvely** — counts edge sessions as viewers and
  misses viewers behind edges; the entire cluster-mode section exists to
  replace it.
- **Emitting inline on every join/leave** — reconnect-storm amplifier; the
  fixed-cadence pump is storm-proof by construction (Decision 4).
- **A `uint16` count field** — `uint32` matches the codebase's integer
  conventions for 4 free bytes and zero overflow doubt (Decision 2).
- **Per-hop count rewrite (à la `ClockMapping`)** — a count is
  pod-independent; edges forward `G` verbatim (Decision 5c).
