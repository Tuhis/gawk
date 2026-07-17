# R17 — Relay scale-out & high availability

Design doc for [ROADMAP R17](../ROADMAP.md#r17--relay-scale-out--high-availability)
(designed 2026-07-16; W1–W6 implemented 2026-07-16 — see Implementation status & findings). Makes the relay horizontally scalable and
highly available in preparation for gawk-as-a-product: N homogeneous relay
pods behind the existing UDP LoadBalancer, any pod able to ingest or serve
any broadcast via **self-federation over the existing WebTransport wire
protocol**, and version rollouts that never break a stream — the worst
visible artifact of a planned rollout is a **≤ 1 s freeze**, per stream, per
drained pod.

User decisions anchoring the scope (2026-07-16):

1. **Scale target**: hundreds of concurrent broadcasts; ~500–1k viewers peak
   on a hot broadcast — so **one broadcast's audience must span pods**
   (this alone rejects per-broadcast sharding, Option C below).
2. **Geo scope**: single region / one Kubernetes cluster now; the topology
   must not preclude geo edges later, but they are not designed here.
3. **Self-contained**: no new stateful infrastructure to operate (no
   NATS/Redis/external etcd). Kubernetes-native primitives (Leases) are fine.
   Relays federate among themselves; the media path stays **datagrams
   end-to-end** — never a TCP broker hop.

Everything here preserves the project's standing invariants: drop-over-stall,
sub-500 ms glass-to-glass, ~1200 B datagram payloads, keyframes on reliable
uni streams with store-and-forward + supersede (R8), and the one frozen wire
format mirrored Go↔TS with golden vectors.

## Goal

- `helm upgrade` to a new relay version while a broadcast with viewers is
  live: the broadcaster's session auto-resumes (no `getDisplayMedia`
  re-prompt, same broadcast ID, same viewer URLs), every viewer's picture
  freezes for at most ~1 s and resumes by itself. No manual action on either
  end.
- `replicas: N` with the audience of one broadcast spread across pods; adding
  pods adds egress capacity roughly linearly.
- A pod crash (not a rollout) recovers automatically within a few seconds —
  best-effort, explicitly *not* held to the 1 s bound (see Non-goals).
- Single-pod deployments (`-cluster-mode` off) behave exactly as today.

## Background: everything pins a broadcast to one process today

All verified at HEAD 2026-07-16:

- The whole data plane is one in-memory `Registry` per process:
  `hubs map[string]*broadcastHub` under a single mutex
  (`gawk-server/internal/hub/hub.go:225-226`), holding per broadcast the
  cached keyframe + decoder config (`hub.go:273-276`), the cached
  ClockMapping (`hub.go:283`), publisher state/generation/grace timer, the
  subscriber set, and the ingress-loss ring. Nothing is shared or persisted.
- **A relay restart doesn't blip streams — it orphans them.**
  `CONNECT /publish/{id}` with an ID the process doesn't know returns
  `ErrNotFound` → HTTP 404 (`hub.go:393-434`); no code path creates a hub
  under a caller-supplied ID. After a restart the broadcaster must mint a new
  ID and every viewer URL dies. The viewer backoff ladder is literally
  documented as sized to "comfortably cover a relay restart"
  (`gawk-app/src/transport/viewer-session.ts:19-23`) — restart tolerance by
  patience, not by design.
- **Reclaim is hijackable today**: the only gate on reclaiming a graced ID is
  `publisherActive == false` plus the *global* publish secret
  (`hub.go:435-437`). Viewers know the ID; any secret-holder can take over a
  disconnected friend's broadcast.
- **No drain**: SIGTERM → context cancel → `webtransport.Server.Close()` —
  open sessions get no application close code
  (`internal/transport/server.go:162-169`, `cmd/gawk-server/main.go:51`).
- **No `StatelessResetKey`** in the QUIC config (`server.go:98-108`): after
  an abrupt restart, a client's packets land on a process that doesn't know
  the connection ID and cannot send a stateless reset the client would
  accept, so clients hang until the ~30 s idle timeout.
- **The viewer's first reconnect attempt waits a hard 1000 ms**
  (`viewer-session.ts:21-23`); no zero-delay path exists. This alone eats the
  entire blip budget.
- **The broadcaster has no auto-reconnect**: session death → terminal error
  UI (`transport/broadcaster.ts:942-974`,
  `features/broadcaster/BroadcasterScreen.tsx:109-116`); manual restart
  re-prompts `getDisplayMedia`.
- **The clock reference is per-process**: TimeSync answers from
  `time.Since(processStart)` (`server.go:321-327`); a ClockMapping cached on
  pod A is meaningless to a viewer talking to pod B.
- **All R2 limits and the `/statusz` HMAC key are per-process**: max
  broadcasts / subscribers, per-IP token buckets
  (`internal/transport/limiter.go`), the egress cap, and `statsKey` = 32
  random bytes per start (`hub.go:376-379`) — the same broadcast would get
  different metric labels on every pod.
- **The chart forbids scale-out on purpose**: `replicas: 1` + `strategy:
  Recreate`, comment "a second live pod behind the same Service would split
  the publisher and the subscribers"
  (`deploy/charts/gawk-server/templates/deployment.yaml:8-13`); no preStop,
  no terminationGracePeriodSeconds, no PDB, no HPA.

So this is structural work, not a `replicas: 2` tweak — but it decomposes
into two independently valuable layers: **rollout resilience** (works at
`replicas: 1`, W1–W2) and **federation** (the actual scale-out, W3–W5).

## Requirements & scale targets

| Requirement | Target |
|---|---|
| Planned rollout, per stream | worst visible freeze ≤ 1 s per drained pod; zero terminal client errors; zero broadcaster re-prompts |
| Pod crash (unplanned) | automatic recovery, best-effort ≤ ~15 s (see Non-goals) |
| Hot broadcast | 500–1k viewers spread over pods (at ~8 Mbps ≈ 4–8 Gbps cluster egress) |
| Broadcast count | hundreds concurrent (Lease API budget sized for this, Decision 7) |
| Latency cost of federation | one intra-cluster UDP hop for deltas (< 1 ms); keyframes +1 store-and-forward hop (~few ms) |
| Single-pod mode | `-cluster-mode` off ⇒ behavior byte-identical to today |

MetalLB L2 reality check (homelab): the Service VIP is announced by one node,
and with `externalTrafficPolicy: Cluster` reply traffic funnels back through
it — cluster egress is capped by that node's NIC regardless of replica count.
The v1 target (≲ 8 Gbps) fits a 10/25 GbE announcing node; BGP/ECMP or a
dedicated LB tier is the documented growth path (Deferred), not an R17 goal.

> **Corrected 2026-07-18 (finding 10): the homelab is MetalLB BGP mode, not
> L2** — MetalLB peers with the router over BGP, so the single-announcing-
> node funnel above does not apply there, and
> `externalTrafficPolicy: Local` is viable today (per-node routes + ECMP).
> The L2 analysis stands for L2 deployments.

## Options survey (settled 2026-07-16)

| Option | Sketch | Verdict |
|--------|--------|---------|
| **A — Uniform pods over a pub/sub backplane (NATS core)** | every pod ingests/serves any broadcast; media fanned through broker subjects; caches re-derived per pod | **Runner-up** — see "revisit if" triggers below |
| **B — Self-federating origin/edge cascade** | the publisher's pod is the per-broadcast *origin* (k8s Lease); other pods *edge-pull* on demand over the existing WebTransport subscribe path and re-fan-out locally | **Chosen** |
| C — Broadcast-sharded pods + rendezvous routing | each broadcast lives wholly on one pod; a connect-time lookup (or QUIC-CID-aware router) steers clients to it | Rejected: caps a broadcast's audience at one pod — fails the 500–1k-viewer requirement. The routing is also ugly: browser WebTransport can't follow redirects, so sharding needs per-pod public endpoints + a pre-connect lookup API, or a CID-routing LB tier |
| D — Active/standby failover | one live relay + a hot spare (lease/VRRP) | Rejected: availability only, no horizontal scaling; still bounded by one pod |

**Option A honestly**: it buys homogeneous stateless-ish pods, battle-tested
clustering, and trivial rollouts (clients just land elsewhere; the broker
holds the fan-out). Core NATS is at-most-once — philosophically aligned with
drop-over-stall for deltas. The costs: a new stateful system to deploy,
monitor, and upgrade (violates the self-contained decision); **a TCP hop in
the middle of the media path** (broker connections are TCP — retransmit
jitter under in-cluster loss, exactly the head-of-line behavior datagrams
were chosen to avoid); keyframes exceed NATS's default 1 MiB payload
(`MaxKeyframeBytes` defaults to 8 MiB) so the reliable-keyframe path needs
chunking or JetStream, re-solving a problem R8 already solved end-to-end; and
the per-process clock/caching machinery still needs the same per-hop
translation work as Option B. **Revisit A if**: gawk goes multi-region and
needs a backbone (NATS leafnodes are genuinely good there), or the k8s-Lease
coupling of Option B proves operationally painful, or W3–W5's federation
complexity blows past its budget. Record the trigger, don't drift.

**Why B wins**: it satisfies every constraint simultaneously — self-contained
(the only new dependency is the k8s API the chart already lives on),
datagrams end-to-end, and the per-hop semantics come for free because **a
relay is already a subscriber-shaped consumer**: the edge reuses join-prime
(cached keyframe + config + clock mapping on attach), store-and-forward
keyframe fan-out with supersede, and drop-newest delta queues, all of which
compose across hops by construction. It reuses the existing Go server and
wire code on both sides of the internal link (the R14 review's "reuse `wire`,
never mirror it" argument, one layer up). It keeps pods homogeneous (origin
vs edge is a *per-broadcast role*, not a deployment split). And it is the
standard shape live-streaming infrastructure converges on (origin/edge
cascades), which extends to geo PoPs later without a redesign — a remote edge
speaks the same protocol over WAN.

## Decisions

1. **Two layers, shipped in order: rollout resilience first, federation
   second.** W1+W2 make streams survive relay restarts at `replicas: 1`
   (blip ≈ pod restart time on Recreate; ≤ 1 s once RollingUpdate lands) and
   are independently the fix for today's "restart orphans every stream"
   defect. W3–W5 add the origin/edge cascade. A `-cluster-mode` flag
   (default off) keeps federation dormant: off means no k8s API client, no
   internal route, single-pod behavior byte-identical to today — dev setups
   and `#/debug/*` flows never notice R17 happened.

2. **Drain = close-first-while-Ready — never "unready, then linger".**
   The intuitive sequence (mark NotReady, keep serving established flows
   until they migrate) is wrong for UDP Services: kube-proxy performs stale
   conntrack cleanup when an endpoint leaves the ready set, so established
   flows can be re-DNAT'd mid-stream to a pod that doesn't know their QUIC
   connection IDs — and the exact timing varies by k8s version / proxy mode
   (iptables/nftables/IPVS, ProxyTerminatingEndpoints). The design must be
   correct under *any* flush timing, so drain is active and immediate: on
   SIGTERM (the rollout signal — no preStop hook needed), while the pod is
   still Ready and its conntrack entries still point at it, it (a) sends
   `4002 CLOSE_CODE_SERVER_DRAINING` to every session, staggered over
   ≤ 1 s, (b) releases its Leases (Decision 7), then (c) exits.
   `terminationGracePeriodSeconds: 10` — the drain is fast by design, not
   leisurely. A `/readyz` endpoint (drain-aware, on the ops mux next to
   `/healthz`, `internal/ops/ops.go:38-42`) is still added, but for
   scale-down/HPA hygiene — it is *not* the rollout-correctness mechanism.
   W1 includes an **empirical homelab check** of both conntrack behaviors
   (readiness flip vs pod deletion, does an established flow survive?) and
   records the findings here.

3. **Shared QUIC `StatelessResetKey` across all pods — the fast-death
   mechanism.** Today the key is unset (`server.go:98-108`), so nothing can
   tell a client "your connection is gone" after an abrupt kill or a
   conntrack re-DNAT; clients wait out the ~30 s idle timeout. With one
   chart-provided 32-byte key shared by every pod, *any* pod receiving
   packets for an unknown connection ID sends a stateless reset the client
   **accepts** (the reset token is derived per-CID from the static key, so
   every pod computes the token the original pod advertised). Media flows
   constantly in both directions, so detection is ~1 RTT after the flush.
   This is what makes graceful 4002 an optimization rather than a
   correctness requirement — the abrupt path (crash, missed close, flushed
   conntrack) converges on the same fast reconnect. Implementation note:
   `http3.Server.ListenAndServe` builds its own `quic.Transport` with no
   key, so the server switches to an explicitly constructed
   `quic.Transport{StatelessResetKey: …}` + `ListenEarly` + serve-listener;
   W1 pins the exact API. Also fixes today's `replicas: 1` restart stall.

4. **Client fast-reconnect policy (both clients).** `4002` ⇒ reconnect
   **now** (0 ms first retry); any *abrupt* post-connect session error (a
   stateless reset surfaces as a generic error, not a close code) ⇒ fast
   first retry ≤ 250 ms; subsequent attempts follow the existing ladder
   (1 s → 15 s cap). The never-connected ⇒ fatal rule
   (`viewer-session.ts:6-9`) stays — it's what keeps bad-cert/wrong-URL
   failures from looping. `4000` stays terminal; `4001` keeps its normal
   ladder. Close codes still ride `wt.closed` and its settle race
   (README gotcha) — `4002` handling slots into the same
   `lastCloseCode` path (`viewer-session.ts:164-181`).

5. **Broadcaster auto-resume: reconnect the transport, keep everything
   else.** On session death mid-broadcast the pipeline no longer fails
   terminally: capture and encoder stay alive (frames drop while
   disconnected — live-edge, no buffering), the transport reconnects with
   the existing reclaim-first logic (`/publish/{id}`, mint only on
   `'connect'`-phase failure — semantics unchanged from R1), and on
   re-attach the pipeline **forces the next frame to be a keyframe** via a
   new hook on `KeyframeCadence` (`media/encoder.ts:51-66` — today only
   encoder recreation forces one). The forced keyframe is sufficient to
   prime a *fresh* pod's caches immediately: the packetizer embeds the
   current `DecoderConfig` in **every** keyframe stream
   (`broadcaster.ts:810-845`), so config + keyframe + (re-emitted)
   ClockMapping arrive together and late-join priming works within ~RTT
   instead of up to one 500 ms GOP. TimeSync restarts per connection (it
   already does — fresh estimator per transport). UI: a "reconnecting"
   broadcaster state (amber, attempt counter) between `live` and `error`;
   the ladder exhausting (~100 s) still lands on the terminal error state.
   **No `getDisplayMedia` re-prompt, ever.**

6. **Resume vs restart needs no server-side epoch.** `generation`
   (`hub.go:447`) never leaves the process — it only fences grace-timer
   races. The viewer-visible signal is **frameID continuity**, and R10's
   field fixes already handle both directions: resume (encoder survives,
   frameIDs continue) ⇒ an ordinary forward gap ⇒ freeze-on-gap until the
   forced keyframe; true restart (new pipeline, frameIDs reset) ⇒
   serially-backwards keyframe ⇒ immediate reorder resync
   (`noteStreamKeyframe`, `wire.frameIdAhead`). So a resume claim is a plain
   `StartPublish` — the cache invalidation it does (`hub.go:449-459`) is
   correct in every case (on a fresh pod there's nothing to invalidate; on
   the same pod the forced keyframe refills within ~RTT).

7. **Restart-survivable IDs via resume tokens (wire `0x09`).**
   `/publish/{id}` acquires a second gate: a **resume token**, required for
   *every* non-mint claim — reclaim of a live/graced hub *and* claim of an
   ID unknown to the receiving pod (which now **creates** the hub instead of
   404ing; this is what makes broadcasts survive restarts and re-home across
   pods). Token = HMAC-SHA256 over the normalized broadcast ID, truncated.
   Key precedence (**revised in the PR #47 security review** — the original
   put the secret derivation first): the chart-managed `-resume-token-key`
   **wins** when set — the publish secret is distributed to every
   broadcaster, so a key derived from it is computable by every
   broadcaster and would gate nothing *between* secret-holders, while the
   explicit key stays server-side (rotating it revokes all tokens); else
   `K_resume = HKDF(publish-secret, "gawk-resume-v1")` (zero-config;
   rotating the secret revokes); dev fallback: per-process random (parity
   with today's process-lifetime reclaim).
   Stateless: any pod can mint and verify with no shared storage.
   Constant-time compare; never logged. Delivery is **in-band** — the
   browser WebTransport API exposes no response headers — as a new wire
   message `TypeResumeToken = 0x09` sent server→publisher right after the
   session upgrade (alongside the `BroadcastAnnounce` flow), mirrored Go↔TS
   with golden vectors. **0x07/0x08 are reserved by R15 (docs/20); R17
   allocates 0x09+ and close codes 4002/4003 only.** The client holds the
   token in memory next to `broadcastId` (in-memory reclaim is today's
   behavior too; sessionStorage persistence is Deferred). Hijack scope,
   stated honestly: with the explicit key this **closes the graced-ID
   hijack** (Background) for real; in secret-derived mode it stops everyone
   who lacks the publish secret but *no one who holds it* (any
   secret-holder can compute any ID's token offline — the fleet runbook in
   docs/05 therefore provisions the explicit key). Residual risk in both
   modes: any token holder can squat that specific ID until key rotation.

8. **Per-broadcast origin registry = one Kubernetes Lease per broadcast**
   (`coordination.k8s.io/v1`, name `gawk-bc-<id>`, in-namespace; new
   ServiceAccount + Role limited to Leases). Holder = pod name; annotations
   carry the origin's dialable address (`<podIP>:4433`, downward-API
   `POD_IP`) and an `originGeneration` counter bumped on every claim.
   **Claim rules** (all CAS on `resourceVersion`): create-if-absent (this is
   also where cluster `MaxBroadcasts` is enforced ≈ count of gawk leases);
   take-if holder empty (released on drain) ∨ holder pod gone/not-Ready ∨
   `renewTime` stale ∨ **claimant presents a valid resume token**
   (force-take — the broadcaster-in-hand is ground truth; this is what makes
   re-homing event-driven instead of TTL-bound). Renew every ~5 s with
   `leaseDurationSeconds: 15` **only while a publisher session is active**;
   during grace the holder stamps a `graceDeadline` annotation and stops
   renewing. GC composes: the origin's grace expiry (today's
   `hub.go:698-742`) additionally deletes the Lease; a leaderless **janitor**
   on every pod CAS-deletes leases stale past the grace period (covers a
   dead origin); edge pods watch (one label-selected informer per pod) and
   close their local viewers with `4000` when the lease disappears —
   cluster-wide "broadcast ended" without a new wire message. API budget at
   target scale: hundreds of leases × 1 renew/5 s ⇒ low-hundreds QPS
   worst-case against the API server, one watch per pod — fine; the renew
   interval is a flag if it ever isn't.

9. **Edge pull: a non-origin pod subscribes upstream like any viewer.**
   A viewer lands on any pod (the UDP LB spreads flows). If the pod holds
   the hub (origin), it serves as today. Otherwise it resolves the Lease,
   dials the origin **at the annotated pod IP — never the Service VIP**
   (guard 1 against loops), with TLS verified by setting
   `tls.Config.ServerName` to the public cert hostname (the cert-manager
   cert validates against it; no per-pod certs, no InsecureSkipVerify;
   requires the issuing CA in the pod trust store — true for Let's Encrypt),
   and issues `CONNECT /internal/subscribe/{id}?psk=…&gen=…`. The internal
   route is gated by a chart PSK and by **generation fencing**: a pod serves
   it only if it currently believes it is origin for exactly that
   `originGeneration` (guard 2) — rejections are plain HTTP statuses
   (404 not-origin / 409 stale-generation / 401 bad PSK), readable because
   the dialer is Go (webtransport-go exposes the status; browsers can't see
   it, but browsers never dial this route). A pod resolving the Lease to
   itself short-circuits (guard 3). Guards 1+2 bound the cascade **depth to
   2 structurally** (origin → edge → viewer); there is no configuration in
   which an edge feeds another edge. On attach the edge receives the normal
   join-prime (cached keyframe with embedded config + ClockMapping) and
   re-ingests everything into a local hub through a `RemotePublisher`
   adapter — datagrams verbatim, keyframe streams re-emitted byte-identical
   (the hub already relays the exact ingested `StreamFrame` bytes;
   `keyframeSeq` is hop-local supersede state and never on the wire, so
   store-and-forward + supersede compose per hop). Local viewers attach to
   that hub exactly as they would on the origin.

10. **Edge hubs are derived state, not broadcasts.** They are excluded from
    `MaxBroadcasts` and their upstream session is exempt from
    `MaxSubscribers`/viewer stats on the origin (the internal route *is* the
    edge marker — needed for accounting, for selective close on demote, and
    for the loop guards). Lifecycle: created on first local subscriber
    demand while the Lease exists; upstream unsubscribed after the last
    local viewer leaves plus a short linger (~15 s); **no 5-minute grace on
    upstream loss** — the Lease is the liveness truth. **Prime-cache rule:
    an edge invalidates its cached keyframe/config/mapping whenever its
    upstream session ends**, and the re-subscribe join-prime refills them —
    a stale prime from origin A can never be served alongside post-re-home
    deltas from origin B, at the cost of one prime per re-attach. Internal
    sessions run tight QUIC timers (idle ~4 s, keepalive ~1 s — in-cluster,
    cheap) so upstream death is detected fast even without the watch.

11. **On losing Lease holdership, an origin demotes itself.** The watch
    tells a pod its Lease was force-taken (NAT rebind, publisher re-homed
    while the old session still looks half-alive — QUIC idle can take 30 s).
    The demoted pod (a) closes its stale publisher session, (b) closes
    downstream **edge** sessions with `4003 CLOSE_CODE_ORIGIN_MOVED`
    (internal-only; the Go edge client re-resolves on any close — 4003 is
    for log clarity), and (c) **becomes an edge itself** for its
    still-connected local viewers, pulling from the new origin. Nobody
    chases viewers across pods; depth ≤ 2 holds across re-homes. Edge
    re-resolves are jittered (herd is bounded by pod count — single digits).

12. **Clock chain: rewrite the ClockMapping per hop; no cluster clock.**
    Each pod keeps its per-process monotonic epoch (`server.go:321-327` —
    deliberately NTP-immune, unchanged). The edge runs a Go port of the
    client's TimeSync estimator (lowest-RTT-of-8, 2 s cadence — mirror of
    `transport/time-sync.ts`) over its upstream session, giving
    origin↔edge offset; it rewrites the cached/forwarded 10-byte
    ClockMapping's offset (broadcaster↔origin + origin↔edge =
    broadcaster↔edge) before serving it to local viewers, whose own
    TimeSync stays against their own pod. Error grows ~rtt/2 ≤ ~0.5 ms per
    in-cluster hop — negligible against the ms-scale capture→render metric.
    Viewers reconnecting to a different pod are already safe: both clock
    legs are per-connection and re-sync from scratch
    (`viewer.ts:524-536` guards on both).

13. **Fleet semantics of every R2 limit (and the SNAT trap).**
    Per-pod (capacity guards, unchanged mechanics): `MaxTotalSubscribers`,
    egress bandwidth cap (also the natural HPA signal later), queue depths.
    Cluster-wide: `MaxBroadcasts` moves to Lease-create (Decision 8);
    per-broadcast `MaxSubscribers` becomes a per-pod local-viewer cap —
    the cluster-wide audience cap is approximate (per-pod cap × replicas),
    documented as such. **Per-IP rate limiting**: with MetalLB L2 +
    `externalTrafficPolicy: Cluster`, cross-node traffic is SNAT'd and the
    relay often sees *node* IPs — at rollout, an entire pod's audience
    reconnects within ~1 s through a handful of SNAT addresses and the 3/s
    bucket (`limiter.go`) would throttle them: reconnecting viewers burn
    ladder attempts, fresh joiners fail **fatally** (never-connected rule).
    Fix: a trusted-CIDR limiter bypass (node/pod CIDRs, chart value),
    extending the existing loopback bypass — per-IP limiting is honestly
    "best-effort under etp=Cluster". **`externalTrafficPolicy: Local` is
    rejected as the fix**: under MetalLB L2 it would pin all service traffic
    to the one announcing node's pods, defeating scale-out. Real client IPs
    return with BGP/ECMP or a CID-aware LB tier (Deferred).
    *2026-07-18 note: the rejection is L2-scoped. The homelab runs MetalLB
    BGP mode (finding 10), where `Local` is the recommended setting — the
    chart exposes `service.externalTrafficPolicy` and `podAntiAffinity`
    for it; the SNAT trap and `trustedCidrs` remain correct for
    `etp=Cluster` under any LB mode.*

14. **Fleet observability.** `statsKey` becomes a chart-provided shared
    secret (per-process random stays the dev fallback) so a broadcast keeps
    **one obfuscated identity across pods** in `/statusz` and
    `gawk_broadcast_*` metrics (`hub.go:376-379, 571-575` — key sourcing
    changes, obfuscation doesn't). New label/rows: per-broadcast **role**
    (`origin`/`edge`) on metrics and `/statusz`; a new **edge-upstream
    ingress-loss window** (edge-leg loss, same ring mechanism as
    `hub/ingress.go`) kept as a separate metric family from the
    broadcaster-leg window — never mixed, per the client_golang
    two-label-sets constraint (docs/13); `route=internal` in
    `gawk_connections_total`. Aggregation rules documented: ingress counts
    once (origin), egress sums across pods, `framesRelayed` is per-pod
    fan-out work. `/statusz` stays per-pod (it's a debugging surface);
    cluster views are Prometheus queries (M8's Grafana dashboard remains
    deferred).

15. **Deployment topology.** Chart: `replicas` becomes a real value
    (default 2 once W5 lands), `strategy: RollingUpdate` with `maxSurge: 1`
    / `maxUnavailable: 0` (serializes drains — at most one pod's sessions
    blip at a time, and a re-connecting client always has ready pods to
    land on), PodDisruptionBudget `minAvailable: 1`,
    `terminationGracePeriodSeconds: 10`, readiness switched to
    `httpGet :2112/readyz` (drain-aware; startup/liveness keep the
    `gawk-echo` exec probe — the HTTP/3 port still can't be probed by
    kubelet), headless Service for pod DNS not required (edges dial pod IPs
    from Lease annotations). New Secrets: stateless-reset key, stats key,
    internal PSK, resume-token key (when not derived from the publish
    secret) — all also flags + `GAWK_*` envs **plumbed through
    `registryOptions`/config per the R2 review rule** (CLAUDE.md). Until W5
    flips the strategy, the chart stays `Recreate` — the first rollout that
    *ships* this machinery still takes one final full blip.

16. **Allocation discipline.** Wire types: R17 uses `0x09`
    (`TypeResumeToken`) and nothing else; 0x07/0x08 stay reserved for R15
    audio. Close codes: `4002 CLOSE_CODE_SERVER_DRAINING` (non-terminal,
    reconnect-now), `4003 CLOSE_CODE_ORIGIN_MOVED` (internal edge sessions
    only). Golden vectors are append-only; existing message layouts are
    frozen (the relay must keep forwarding old-client traffic verbatim
    during a rolling version skew — pods of version N and N+1 coexist
    mid-rollout, so **every internal-protocol change must be
    skew-tolerant**: the internal subscribe carries the protocol version in
    its query and an origin refuses unknown-major dialers with a plain 426,
    leaving the edge to retry until the rollout completes).

## Architecture

```
                          UDP LoadBalancer :4433 (one VIP)
        broadcaster ──────────────┼──────────── viewers land anywhere
        (lands anywhere)          │
   ┌──────────────────────────────┼──────────────────────────────┐
   │ k8s cluster                  ▼                              │
   │   ┌────────────┐      ┌────────────┐      ┌────────────┐    │
   │   │ pod A      │      │ pod B      │      │ pod C      │    │
   │   │ ORIGIN(X)  │◄─────│ EDGE(X)    │      │ EDGE(X)    │    │
   │   │ hub X      │ WT   │ hub X'     │      │ hub X'     │    │
   │   │ + ORIGIN(Y)│ sub  │ (derived)  │◄──┐  │ (derived)  │    │
   │   └─────┬──────┘      └─────┬──────┘   │  └─────┬──────┘    │
   │         │ local             │ local    │        │ local     │
   │         ▼ viewers           ▼ viewers  │        ▼ viewers   │
   │                                        │                    │
   │   Lease gawk-bc-X: holder=A, addr=10.0.0.5:4433, gen=3 ─────┘
   │   Lease gawk-bc-Y: holder=A, …          (edges resolve+watch)
   └──────────────────────────────────────────────────────────────┘
```

- **Origin** (per broadcast): terminates the publisher, runs
  ingress-loss/grace/caches exactly as today, serves local viewers *and*
  edge sessions (internal route), renews the Lease.
- **Edge** (per broadcast, on demand): Go upstream subscriber +
  `RemotePublisher` into a local hub; serves local viewers; TimeSync
  estimator against origin; rewrites ClockMapping; invalidates primes on
  upstream loss; lingers ~15 s past its last viewer.
- **Every pod** runs the same binary; roles are per-broadcast and dynamic
  (pod A is origin for X while edging for Z).

## Failure-mode walkthroughs

**Edge pod drains (viewer's pod).** SIGTERM → pod (still Ready) sends 4002
to its viewers over ≤ 1 s → each viewer reconnects with 0 ms delay → LB
lands the new flow on a ready pod → origin: served from cache; edge:
lease-resolve + upstream attach (or already attached for other viewers) →
join-prime → first frame. Expected freeze ≈ handshake (1–2 RTT) + prime
≈ **100–300 ms**. The canvas holds the last frame throughout (existing
behavior).

**Origin pod drains.** 4002 to publisher, viewers, and edges; Lease holder
cleared; pod exits. Broadcaster reconnects instantly with its resume token →
lands on any ready pod → claim succeeds (empty holder) → `originGeneration`
bumps → forced keyframe primes the new origin within ~RTT. Edges got 4002
too → jittered re-resolve → re-attach → join-prime (fresh caches). Viewers
on edges see freeze ≈ publisher reconnect + forced keyframe + edge re-attach
≈ **300–600 ms**. Local viewers of the old origin reconnect like the
edge-drain case.

**Full 3-replica rollout.** maxSurge 1 / maxUnavailable 0 ⇒ pods drain one
at a time; each stream blips at most once per pod it touched, each ≤ 1 s,
minutes apart. Worst case the publisher re-homes up to R times (a reconnect
can land on a not-yet-drained old pod — kube-proxy picks among ready
endpoints; accepted, each re-home is an independent ≤ 1 s blip).

**Origin pod crashes (no 4002).** Three clocks race, all bounded: clients'
packets hit a flushed/re-DNAT'd path → some live pod sees unknown CIDs →
**stateless reset** (shared key) → abrupt-error fast retry (≤ 250 ms);
edges' internal sessions idle out in ~4 s; the Lease goes stale in ≤ 15 s
(or the holder pod leaves Ready endpoints sooner) → broadcaster's
reconnect force-takes regardless of TTL. Recovery ≈ endpoint-removal
propagation + retries ≈ **2–10 s** — within the crash non-goal, not the
rollout bound.

**NAT rebind / split brain.** The broadcaster's NAT rebinds; its packets
arrive as a new flow, possibly on pod B, while pod A still holds a
half-alive publisher session. B's unknown-CID → stateless reset →
broadcaster fast-reconnects with token → **force-takes** the Lease at B.
A's watch fires → A kills the stale publisher session, closes edges with
4003, demotes itself to edge for its local viewers. Viewers freeze ≤ ~1–2 s
(this is the unplanned path). No wire message exists for "you were
superseded" — the Lease is the arbiter.

**Broadcaster resume vs. restart (viewer's eye).** Resume: frameIDs
continue → forward gap → freeze-on-gap → forced keyframe → clean. Restart
(user manually restarts a broadcast): frameIDs reset → serially-backwards
keyframe → immediate reorder resync (R10 machinery). Both paths already
exist client-side; R17 adds no viewer protocol change.

## Chunks

Sequenced so each lands value alone; W1+W2 are useful at `replicas: 1`
forever (and are the priority if R17 gets paused).

### W1 — Fast death detection + drain + viewer fast-reconnect

Server: shared `StatelessResetKey` (flag/env/Secret; explicit
`quic.Transport` — pin the exact listen API); SIGTERM drain sequence
(4002 staggered ≤ 1 s → lease release stub → exit); `4002` constant in Go
`wire` + TS `wire.ts`; ops `/readyz` (drain-aware). Client (viewer):
close-code-aware retry delays (4002 ⇒ 0 ms; abrupt post-connect ⇒ ≤ 250 ms;
ladder after), reconnect-pill copy for the drain case. Chart: readiness →
`httpGet :2112/readyz`, `terminationGracePeriodSeconds: 10`, PDB template
(inert at 1 replica). Strategy stays Recreate until W5.

| Goal | Verified by |
|---|---|
| Drain sends 4002 to every open session before exit, ≤ 1 s | Go unit: fake sessions record close code/order/timing on SIGTERM-triggered drain |
| Viewer retry-delay policy | TS unit table: 4002→0 ms, abrupt→≤250 ms, 4001→1 s ladder, 4000→terminal, never-connected→fatal |
| Stateless reset accepted cross-process | Go integration: two servers sharing a key on one socket pair — client of A gets reset from B in <1 RTT; without shared key, no reset |
| Restart stall fixed at replicas:1 | Homelab: `kill -9` + restart under live viewers ⇒ sessions error ≤ 2 s (vs ~30 s today), reconnect on the ladder |
| Rollout drill (Recreate) | Homelab: `kill -TERM` under broadcaster + 3 viewers ⇒ all clients see 4002 ≤ 1 s, pod exits ≤ 3 s |
| Conntrack reality documented | Empirical findings (readiness-flip vs deletion; established-flow survival) recorded in this doc |

### W2 — Restart-survivable broadcasts (resume tokens + broadcaster auto-resume)

Wire: `TypeResumeToken = 0x09` Go+TS + golden vectors. Server: HKDF key
derivation, token mint on first publish, token-gated `/publish/{id}` for
existing **and unknown** IDs (unknown+valid-token creates the hub);
constant-time verify; token never logged. Client (broadcaster): store token;
auto-resume (keep capture+encoder, transport-only reconnect, reclaim-first
semantics preserved, forced keyframe via the new `KeyframeCadence` hook,
ClockMapping re-emit, drop-frames-while-disconnected, reconnecting UI state,
ladder → terminal). Follows CODE-REVIEW.md test-first where behavior is a
bug fix (the 404-orphan) and adds goldens for 0x09.

| Goal | Verified by |
|---|---|
| Token crypto + gating | Go unit: valid/invalid/cross-ID tokens; no-secret vs secret modes; reclaim without token rejected (the hijack regression test); unknown-ID+token creates hub |
| Byte-identical 0x09 across languages | Golden vectors Go↔TS |
| Resume forces a self-sufficient keyframe | TS unit: after simulated session death + reconnect, next emitted frame is a keyframe stream with embedded config; frameIDs continue (no reset) |
| Broadcaster survives relay restart unattended | Homelab replicas:1: relay pod restart under live broadcast ⇒ same broadcast ID end-to-end, viewer URLs unchanged, **zero `getDisplayMedia` prompts**, viewers recover ≤ pod-restart + 1 s |
| No regression for mint/manual flows | Existing R1 reclaim/mint tests untouched; manual restart still reclaim-first |

### W3 — Lease-based origin registry (dormant behind `-cluster-mode`)

`internal/cluster`: Lease claim/renew/release/force-take with
`originGeneration` CAS fencing; janitor; one label-selected informer per
pod; claim wired into `StartPublish`, release into drain + grace-GC;
cluster `MaxBroadcasts` at Lease-create. Chart: ServiceAccount, Role
(Leases only), RoleBinding, `POD_IP` downward API. `-cluster-mode` defaults
off ⇒ no k8s client, byte-identical single-pod behavior.

| Goal | Verified by |
|---|---|
| Claim semantics | Go unit (fake clientset): two claimants ⇒ one winner; force-take with token beats a live holder; stale generation loses CAS; janitor deletes only stale-past-grace |
| Lifecycle == broadcast lifecycle | Unit: publish creates lease; drain clears holder; grace-GC deletes; edge-side watch close(4000) on deletion |
| API budget | Unit asserts renew cadence (≤ 1 renew / lease / 5 s, none during grace) |
| Dormant by default | Full existing Go test suite passes with cluster-mode off and no fake clientset wired |
| Two-pod smoke | kind/k3s: publisher lands on either pod; lease shows holder + addr + generation |

### W4 — Edge-pull data path (behind `-cluster-mode`)

`/internal/subscribe/{id}` (PSK + `gen` + protocol-version params, HTTP
status rejections 401/404/409/426); Go upstream client (dial lease pod IP,
`ServerName` = cert host, idle 4 s / keepalive 1 s); `RemotePublisher`
re-ingest (datagrams verbatim; keyframe streams byte-identical); Go
TimeSync estimator port + per-hop ClockMapping rewrite; edge hub lifecycle
(demand-create, prime invalidation on upstream loss, ~15 s linger, no
grace); edge exemptions (MaxBroadcasts/MaxSubscribers/viewer stats) + role
labels.

| Goal | Verified by |
|---|---|
| Loop guards | Go unit: edge-subscribe to a non-origin ⇒ 404; stale gen ⇒ 409; self-resolve short-circuits; depth ≤ 2 invariant asserted in an in-process 3-registry harness |
| Prime staleness impossible | Unit: upstream drop invalidates cached keyframe/config/mapping; re-attach re-primes; a viewer joining between drop and re-prime waits (no stale serve) |
| Byte-fidelity across the hop | Golden: keyframe stream + delta datagrams byte-identical after `RemotePublisher` re-ingest |
| Clock rewrite math | Unit: fake clocks — broadcaster↔origin + origin↔edge compose to broadcaster↔edge within estimator error |
| Two-pod E2E | kind/k3s: viewer on the non-origin pod plays (join-prime ≤ 300 ms); capture→render within ~1 ms of a direct-origin viewer; origin `/statusz` shows one edge session, not a viewer |

### W5 — Origin move + multi-replica rollouts

Holdership-loss handling (kill stale publisher, close edges 4003,
self-demote to edge); jittered edge re-resolve; drain releases leases (W1
stub goes real); limiter trusted-CIDR bypass; chart flip: `replicas: 2`
default, RollingUpdate maxSurge 1 / maxUnavailable 0, PDB minAvailable 1,
the deployment.yaml:8-13 "must be 1 replica" comment replaced by the new
invariants.

| Goal | Verified by |
|---|---|
| Demote path | Go unit: on lease loss — stale publisher closed, edges closed 4003, local viewers re-served via self-edge; depth stays ≤ 2 |
| Herd behavior | Unit: N edges re-resolve with jitter, bounded retries |
| Limiter bypass | Unit: trusted CIDR skips bucket; loopback unchanged |
| **The headline drill** | 3-replica homelab rollout under live broadcast with viewers spread over ≥ 2 pods: every viewer freeze ≤ 1 s (overlay stall age + wall clock), publisher auto-resumes each re-home ≤ 1 s, same ID throughout, zero terminal errors, zero re-prompts, zero limiter rejections |
| Split-brain | Drill: force a second publisher path (simulated rebind) ⇒ force-take fences the old origin ≤ 1 s; old origin demotes; viewers continue |
| Crash mid-rollout | Drill: `kill -9` the origin during a rollout ⇒ recovery within the crash non-goal (≤ ~15 s), depth ≤ 2 throughout (`/statusz` roles) |

### W6 — Fleet observability + scale proof + docs

Shared `statsKey` Secret; role labels; edge-upstream loss window (separate
metric family); `route=internal` counters; a synthetic-viewer load tool
(Go, N subscribe sessions counting gaps/freezes); README gotchas + docs/05
runbook + this doc's findings pass (record measured blips + conntrack
findings), CLAUDE.md/ROADMAP status sync.

| Goal | Verified by |
|---|---|
| One identity per broadcast fleet-wide | Two pods, same broadcast: identical obfuscated label in both `/statusz` + `gawk_broadcast_*` |
| Loss attribution stays clean | Unit: broadcaster-leg vs edge-leg windows never share a family; ingress counted once (origin) |
| Scale proof | Load test: 1 broadcast × 200+ synthetic viewers across 3 pods holds bitrate; per-pod egress ≈ bitrate × (local viewers + edges); ingress-loss ~0; no keyframe-drop storms |
| Docs | Runbook covers multi-replica install/upgrade/rollback; gotchas + findings recorded; statuses synced |

## Implementation status & findings (2026-07-16)

**W1–W6 implemented 2026-07-16; all automated gates green.** The homelab
drills (rollout/crash/rebind blip measurements, the W1 conntrack empiricism,
the kind/k3s two-pod smoke, and the W6 load-tool scale proof against the real
cluster) are **pending** — the in-process twins below stand in until then.

Per-chunk status:

| Chunk | Status | Notes |
|---|---|---|
| W1 | ✅ implemented | Drain unit + ctx-cancel integration tests; stateless-reset proven cross-process via an in-test UDP proxy (shared key ⇒ session dies < 2 s; mismatched key ⇒ client keeps waiting). Homelab `kill -9`/`kill -TERM` drills pending |
| W2 | ✅ implemented | Both bug fixes landed test-first (red → green): the tokenless-reclaim hijack and the unknown-ID 404 orphan. 0x09 golden vectors Go↔TS. Homelab restart-under-live-broadcast drill pending |
| W3 | ✅ implemented | Fake-clientset unit suite (claim/force-take/CAS retry/janitor/renew budget/informer); lease lifecycle == broadcast lifecycle proven through the real server. kind/k3s smoke pending |
| W4 | ✅ implemented | In-process 3-pod E2E: byte-identical prime + deltas across the hop, origin counts edges-not-viewers, depth ≤ 2 (edge 404s internal subscribes), stale gen ⇒ 409. Prime-staleness + clock-rewrite units |
| W5 | ✅ implemented | In-process origin-move drill: force-take ⇒ stale publisher killed, edges 4003 ⇒ re-resolve, self-demote serves local viewers, roles land right. Jitter + trusted-CIDR units. Real-cluster headline drill pending |
| W6 | ✅ implemented | Shared statsKey, role gauge + edge-session gauge, split edge-leg loss families (attribution unit-proven), `route=internal` counters, `gawk-loadgen`. Scale proof against the real cluster pending |

Findings and deviations from the design as written:

1. **This milestone is R17/docs/22, not R16/docs/21.** The design was drafted
   as R16 concurrently with the iOS-native-fullscreen work, which landed
   first and took R16 + docs/21. Everything (docs, code comments, roadmap)
   uses R17/docs/22; the allocations themselves (0x09, 4002, 4003) are
   unchanged.
2. **`replicas` defaults to 1, not the designed 2.** Default 2 was incoherent
   with `-cluster-mode` defaulting off (two pods without federation is the
   pre-R17 split-brain), and would have flipped the existing homelab install
   to a broken topology on upgrade. Instead: `replicas` is a real value, the
   chart **fails to render** `replicas > 1` without `config.clusterMode`, and
   the strategy is RollingUpdate (maxSurge 1 / maxUnavailable 0) even at 1
   replica — a single-pod upgrade is now a ≤1 s blip instead of a Recreate
   gap, which supersedes the "stays Recreate until W5" plan.
3. **A drain flush delay exists** (`drainFlushDelay`, 250 ms): the drain-test
   found the final 4002 close capsule can be outrun by `wt.Close()`'s
   CONNECTION_CLOSE, downgrading a clean drain signal to an abrupt drop. The
   drain sleeps once more after the last close before teardown.
4. **Fleet-shared Secrets are load-bearing, not optional.** The in-process
   drills reproduced both failure modes: per-process resume-token keys ⇒
   re-homing 403s (the demote drill failed exactly this way first), and
   per-process stats keys ⇒ one broadcast under N metric identities. The
   startup log prints `resume_token_key_mode` so a misconfigured fleet is
   visible; cluster-mode refuses to start without `-internal-psk` /
   `-internal-server-name`.
5. **Edge hubs must not run the origin lease hooks.** First implementation
   had an edge hub's grace-GC deleting *the origin's* Lease (cluster-wide
   broadcast kill from derived state). Both lifecycle hooks are now gated on
   the hub's origin role.
6. **The primed ClockMapping usually beats the first TimeSync pong** on a
   fresh edge attach. Rather than serving it un-rewritten (wrong by an
   arbitrary inter-pod epoch) or dropping it until the broadcaster's next
   5 s re-send, the edge holds the newest un-translatable mapping and emits
   it the moment the estimator has its first sample.
7. **Demote needs the publisher session indexed by broadcast ID** — closing
   "the stale publisher" on lease loss requires finding it; the transport now
   tracks live publisher sessions per ID. No ping-pong results: the
   re-homed broadcaster's old session object is already abandoned client-side
   (its callbacks are generation-fenced), so closing it triggers nothing.
8. **The edge claim and the come-home order matters**: a broadcaster
   re-homing onto a pod that is currently an *edge* for its broadcast would
   409 against the edge's own upstream pull. `/publish/{id}` stops the local
   edge pull (synchronously) before claiming the hub.
9. **Server uni-stream accept order is NOT open order — the rebase onto R14
   proved it** (2026-07-18). The announce (0x03) and resume token (0x09)
   ride two server-initiated uni streams, and the design assumed old clients
   "read only the first stream" safely. webtransport-go demultiplexes
   incoming streams concurrently, so the native engine (`gawk-broadcast`,
   merged to main after this PR forked) saw the token beat the announce in
   roughly half of real dials — its single-accept strict-parse read then
   reported "no broadcast code". The browser is *probably* order-preserving
   in practice (and the R17 `broadcaster.ts` dispatches by type anyway), so
   the old-browser skew story stands, but the guarantee is weaker than the
   comment claimed. Adaptation (in this PR, on top of the rebase): the
   engine reads server uni streams for the session's life and dispatches by
   wire type (unknown types ignored — the same rule as `broadcaster.ts`),
   `engine.Config.ResumeToken` rides every `/publish/{id}` claim as the
   `resume` param, `Callbacks.OnResumeToken` hands the token to the shells,
   and app/CLI persist it as `lastResumeToken` beside `lastBroadcastId`
   (0600 config — it is a credential). Never assume accept order equals
   open order on any WebTransport client.
10. **The homelab is MetalLB BGP mode, not L2** (2026-07-18, user-reported —
    the design's "L2 reality check" premise was wrong for this cluster).
    Consequences: the single-announcing-node egress funnel does not apply,
    and `externalTrafficPolicy: Local` — rejected in Decision 13 *under L2*
    — is the recommended setting here: per-node BGP routes + router ECMP
    give both pod spread and **real client IPs** (exact per-IP rate
    limiting; `trustedCidrs` demoted to covering in-cluster hairpin-SNAT
    dials only). The chart now exposes `service.externalTrafficPolicy`
    (empty = k8s default Cluster; existing installs unchanged) and
    `podAntiAffinity` (`soft`/`hard`/`none`, default soft) — with `Local`,
    a node without a relay pod serves no external traffic, so replicas
    must actually spread. Known cost: when the ECMP path set changes (a
    rollout withdrawing a node's route), the router re-hashes flows and
    live QUIC connections can land on a different pod mid-stream — the
    shared `StatelessResetKey` turns that into a ~1 RTT reset + fast
    reconnect instead of an idle-timeout hang; measure it in the drills
    (below). Router-side resilient hashing is the mitigation if the blip
    reads ugly.

### Post-implementation review fixes (2026-07-16, PR #47)

The PR #47 review found six issues; all are fixed on the branch, each bug
test-first (red → green) per CODE-REVIEW.md:

9. **A lingered-out edge pull left its derived hub in the ordinary 5-minute
   grace.** A viewer joining that window attached to a hub with no upstream
   behind it (`handleSubscribe` runs `EnsureEdge` only on `ErrNotFound`) and
   after up to 5 minutes received a wrong terminal 4000 while the broadcast
   was still live at the origin — violating Decision 10's "the Lease is the
   liveness truth". The linger-out now deletes the hub atomically-if-still-
   viewerless (`Registry.ExpireEdgeIfViewerless`); a viewer that races the
   ≤1 s linger window keeps the hub and the pull re-attaches for it.
10. **`stop()` racing an in-flight resume dial leaked a zombie publisher**
    — the CODE-REVIEW.md bug class verbatim: `tryResume`'s post-dial code
    re-armed wt/sender/TimeSync after `teardown()` had already run, then
    abandoned them. The fresh transport is now torn down when the dial
    lands after stop, and a dial *failure* after stop schedules nothing.
11. **A demoted origin's grace expiry deleted the NEW origin's lease** ~5
    minutes after any re-home where the old origin had no local viewers
    (its hub stayed `role=origin`, so grace-GC fired `OnBroadcastExpired` →
    `Coordinator.Delete`, which deleted unconditionally) — ending the live
    broadcast cluster-wide and silently dropping the new origin's
    holdership via every informer's DeleteFunc. `Delete` is now
    **holder-gated + CAS-fenced**: only the holder-of-record (or anyone,
    once a drain `Release` cleared the holder) deletes, with a
    resourceVersion precondition so a racing claim wins.
12. **`EdgePublish` set `edge=true` before a claim that can fail**,
    mislabeling whatever hub it raced (role metrics, loss attribution,
    lease lifecycle hooks). The role now flips only on a successful claim.
13. **Role flips leaked ingress-loss counts across metric families**: the
    per-hub cumulative counters are attributed by *current* role at scrape
    time, so a demote/come-home moved everything accumulated on one leg
    under the other family. Every role flip now folds the counters into the
    old leg's lifetime totals first (`setRoleLocked`) — the families stay
    unmixed across transitions (Decision 14 holds for the hub's whole life).
14. **Resume-token key precedence flipped — the security fix** (Decision 7
    revised in place): the explicit `-resume-token-key` now wins over the
    publish-secret derivation. A key derived from the broadcaster-
    distributed publish secret is computable by every secret-holder, so the
    original "closes the graced-ID hijack" claim held only against
    non-secret-holders — exactly the actors who couldn't publish anyway.
    The explicit server-side key makes the token a real per-broadcast
    ownership proof between broadcasters; the docs/05 fleet runbook now
    provisions it, and `resume_token_key_mode=explicit-key` in the startup
    log is the deployment check. (Also from the review, no behavior change:
    the internal-subscribe PSK is now query-escaped in the edge dialer.)

## Verification plan (manual, after W5/W6)

The homelab drills above, run against the real chart on the real cluster,
under a real broadcast from the gaming PC with ≥ 3 browser viewers plus the
synthetic load tool; measure blips with the stats overlay (stall age,
live-edge drift, gap resyncs) against wall clock, and record a
measured-blip table (drain / rollout / crash / rebind × viewer / broadcaster)
in this doc. Plus the W1 conntrack empiricism: flip readiness on a pod with
an established flow, then delete the pod, and record what actually happened
on this cluster's kube-proxy mode — the design tolerates either outcome, but
the doc should state which one is real here.

With `externalTrafficPolicy: Local` on the BGP homelab (finding 10), add the
**ECMP re-hash drill**: under a live broadcast spread over both pods, roll
one pod and watch the viewers attached to the *surviving* pod — a route
withdrawal re-hashes flows at the router, and any connection re-hashed onto
the other pod should show a ~1 RTT stateless-reset reconnect, not a hang.
Record that blip in the measured-blip table alongside the drain numbers,
and verify real client IPs appear in the relay logs (the limiter bypass no
longer masking them).

## Non-goals

- **Zero-blip rollouts / QUIC session migration.** quic-go cannot serialize
  live TLS+QUIC session state to another process, and the ≤ 1 s budget
  doesn't need it. Reconnect-fast is the mechanism. (Client-initiated QUIC
  connection migration across pod IPs is likewise out — the LB doesn't route
  by CID; see Deferred.)
- **Crash RTO under the 1 s bound.** Unplanned deaths converge in ~2–10 s
  (stateless reset + endpoint propagation + lease force-take), bounded by
  the browser's ~30 s idle timeout in the absolute worst case (untunable
  from JS; the server-side `MaxIdleTimeout` floor is the min-of-endpoints
  rule, docs/05). Target: ≤ ~15 s, best-effort.
- **Geo edges / multi-region** — the cascade extends there (a PoP edge is
  the same protocol over WAN), designed later.
- **HPA / autoscaling** — manual `replicas` in v1; the egress cap is the
  natural custom metric when this lands (scale-down reuses the same drain).
- **Cross-pod `/statusz` aggregation UI** — Prometheus is the fleet view;
  M8's Grafana dashboard stays the deferred owner of that story.
- **Multi-tenant auth/quotas** — product work, orthogonal to topology; the
  publish secret + resume tokens are the v1 access model.
- **MoQ** — still deferred project-wide; this design deliberately reuses the
  existing wire protocol rather than adopting a draft one.

## Deferred (wanted, not now)

- **BGP/ECMP (or cloud NLB) + real client IPs** — lifts the MetalLB-L2
  single-node egress funnel and restores exact per-IP rate limiting
  (etp=Local becomes viable under BGP with pod-local announcement).
  *Partially landed 2026-07-18: the homelab turned out to already run BGP
  mode, and the chart now exposes `service.externalTrafficPolicy` +
  `podAntiAffinity` (finding 10). What stays deferred is router-side
  resilient hashing and any LB tier beyond MetalLB.*
- **QUIC-CID-aware routing tier** (QUIC-LB draft; e.g. quilkin) — survives
  client NAT rebinds without a reconnect and makes LB→pod affinity explicit
  instead of conntrack-shaped.
- **Resume-token persistence** (sessionStorage) so a broadcaster tab reload
  can reclaim without re-minting; deliberate scope cut in W2 (matches
  today's in-memory reclaim).
- **Origin placement preference** (e.g. bias the Lease claim toward the pod
  with the most viewers of that broadcast) — pure optimization; measure
  first.
- **Wire-level `SubscriberCount`/viewer-presence for publishers** — noted in
  R14 as a possible future wire+relay change; a fleet makes it slightly
  harder (needs cross-pod summing); still not R17's business.

## Rejected

- **NATS/Redis backplane (Option A)** — violates the self-contained
  constraint and puts TCP in the media path; runner-up with explicit
  "revisit if" triggers recorded above. Don't re-propose without one of the
  triggers firing.
- **Broadcast sharding (Option C)** — caps a hot broadcast at one pod;
  fails a user-confirmed requirement. Also needs per-pod public endpoints or
  a CID router just to route CONNECTs (browsers can't follow redirects on
  WebTransport).
- **Active/standby (Option D)** — HA without scale-out.
- **"NotReady, then drain" rollout sequencing** — wrong under kube-proxy's
  UDP conntrack flush semantics (Decision 2). Close-first while Ready.
- **`externalTrafficPolicy: Local` to fix limiter SNAT** — under MetalLB L2
  it pins all traffic to the announcing node (Decision 13). *L2-scoped:
  under the homelab's actual BGP mode it is the recommended setting
  (finding 10).*
- **A cluster-wide clock** (NTP-derived wall clock for TimeSync) — the
  per-process monotonic epoch was chosen deliberately (NTP steps must not
  jump every client's offset, `server.go:321-322`); per-hop offset
  composition needs no shared epoch (Decision 12).
- **Extending `TypeBroadcastAnnounce` (0x03) to carry the resume token** —
  golden vectors freeze existing layouts; new message `0x09` instead
  (Decision 16).
- **WebRTC/SFU cascade** — project-wide rejection stands (CLAUDE.md).
