# R20 — E2E Testing in CI: Real-Browser Streaming Verification per PR

Design doc for [ROADMAP R20](../ROADMAP.md#r20--e2e-testing-in-ci)
(designed 2026-07-18; Z1 done + Z2/Z3 implemented 2026-07-18; **both tiers
green in real CI** — tier-1 `e2e` on every PR, `e2e-cluster` on the
2026-07-18 release PRs; Z4 burn-in + a re-run on the new self-hosted
`ioio-k8s` runners pending; Z5 implemented 2026-07-19 — **the browser
broadcaster works headless via tab capture**, findings 12–15). Adds a
GitHub Actions E2E stage that
proves **streaming actually works** — a real Chromium viewer decodes and
renders real frames published through the real relay binary — before a
release can ship, in both **single-pod** and **cluster-mode** relay
configurations. Today every automated gate stops below the browser; this is
the layer where field regressions keep surfacing, and releases auto-deploy
to the homelab.

Two user decisions anchor the scope (2026-07-18):

1. **Cluster-mode verification runs in PR pipelines only — specifically,
   on release-please PRs only.** Release-please refreshes the release PR
   after every merge to main, so the cluster tier still re-runs against each
   merged change while a release is pending — full release gating at minimal
   minutes. The trade-off is accepted: a cluster-breaking PR merges green
   and the failure surfaces on the release PR (one step removed in
   attribution, never past a release).
2. **The GitHub free tier is a design constraint, but not at the expense of
   quality.** The repo is private (`Tuhis/gawk`), so every runner minute is
   billable against the 2,000 free minutes/month — the pipeline is designed
   to a minutes budget (Decision 2) with explicit guardrails, and the
   quality bar is held by *where* tests run (every PR for the cheap tier,
   every release for the expensive one), not by thinning the assertions.

**Chunk letters**: R18's design doc (`docs/23`, designed the same day)
claimed `Y1–Y6` — hence `Z1–Z5` here.

## Goal

A regression that breaks streaming turns a check red on the PR that would
ship it. Concretely: CI starts the real `gawk-server`, publishes real H.264
video through the real native-broadcaster engine, opens the **production
viewer surface** in headless Chromium, and asserts — from the viewer's own
diagnostics — that frames are received, decoded, and rendered. The same
verdict is produced against a 2-replica cluster-mode relay on a real kind
cluster before every release. The manual pre-release ritual ("start a
broadcast, open a viewer, squint") becomes a machine's job.

## Background: what CI proves today, and the gap

- **Go side is well covered, including against the real binary.**
  `go test -race` runs real in-process WebTransport servers
  (`gawk-server/internal/transport/server_test.go`), the cluster logic has
  an in-process 3-pod twin against a fake clientset
  (`cluster_integration_test.go`), and
  `gawk-broadcast/internal/engine/relay_integration_test.go` already
  **builds and runs the actual `gawk-server`**, publishes the committed
  MPEG-TS/H.264 fixture (`internal/mpegts/testdata/sample.ts`) through the
  real engine, and reads `/statusz` — in CI, today.
- **The browser leg does not exist.** `gawk-app`'s vitest suite runs under
  jsdom: no WebCodecs, no WebTransport, no worker pipeline, no rendering.
  Yet that is exactly where regressions surface — nearly every roadmap item
  since R11 carries a "manual browser verify pending" tail, and the R10/R16
  field findings were all browser-side.
- **Releases auto-deploy.** CI is publish-only and the homelab deploys
  every release automatically (docs/05) — there is no human between a green
  release PR and production. The release PR's checks are therefore the last
  (and currently missing) line of defense.
- **R17 left cluster verification manual.** The kind/k3s two-pod smoke and
  the loadgen scale proof are pending manual drills (docs/22 "Verification
  plan"); the in-process twins stand in. The cluster tier here automates
  the two-pod smoke; the 200-viewer scale proof stays a manual drill
  (non-goal).

## CI environment constraints (facts, verified 2026-07-18)

These shaped every decision below; recorded so they aren't re-derived:

- **Private repo → billable minutes.** 2,000 free min/month (Free plan),
  Linux at 1× / ~$0.008 per overage minute. Current CI costs roughly
  13–15 billable minutes per push event across its five jobs.
- **`ubuntu-latest` for private repos = 2 vCPU / 7 GB RAM / ~14 GB SSD.**
  The 4-core runners are public-repo only. One VM must host relay (+ kind
  in tier 2), publisher, Chrome, and Node — CPU contention is the expected
  flake source, so assertions must be flow-shaped, not rate-shaped
  (Decision 6). `ubuntu-slim` (1 vCPU, 15-min kill) is unsuitable.
- **No GPU on any hosted runner.** Chrome software-decodes; the publisher
  is a fixture, not an encoder. The E2E covers the software path only —
  hardware encode/decode remain on-device manual verifies (non-goal).
- **Runners are ephemeral** — a fresh kind cluster per run is mandatory,
  and is treated as a feature: every run re-proves chart install from
  scratch; no stale-state flakes.
- **`kubectl port-forward` is TCP-only** — it cannot carry WebTransport.
  UDP reaches the cluster via kind `extraPortMappings` (`protocol: udp`)
  + a NodePort Service (`service.type` is already a chart value). The TCP
  ops endpoint (`:2112`, R9) port-forwards fine — that's the assertion
  side-channel.
- **Docker and Chrome are preinstalled** on `ubuntu-latest`; kind needs no
  nested virtualization. `kind create cluster` reaches Ready in ~20–45 s
  plus ~30–60 s node-image pull (pinned version).

## Direction survey (settled 2026-07-18)

| Leg | Options considered | Verdict |
|-----|--------------------|---------|
| Browser automation | playwright-core + preinstalled system Chrome; @playwright/test with downloaded browsers; Puppeteer; Selenium | **playwright-core + system Chrome** — the `gawk-app:verify` skill already proved the recipes (overlay scraping, init scripts, worker introspection) on this exact app; no browser download step (~1 min + disk saved) |
| Publisher leg | Synthetic native-engine fixture publisher (new `cmd/gawk-pubsim`); browser broadcaster via capture-automation flags; browser broadcaster via injected fake source | **`gawk-pubsim`** for the core tiers — reuses the `relay_integration_test.go` fixture-source pattern as a standalone CLI; deterministic, no capture automation. Browser broadcaster is the pre-registered stretch (Z5, Decision 9) |
| Cluster substrate | Real kind cluster; k3s; out-of-cluster kubeconfig mode added to `main.go`; fake clientset | **kind** — the binary is `rest.InClusterConfig()`-only and the Lease machinery needs a real API server anyway; kind also exercises the real chart, kube-proxy, and downward API. Rejecting the `main.go` kubeconfig path keeps prod code free of test-only branches |
| Success signal | Viewer diagnostics JSON (R9 Copy diagnostics); overlay row scraping; canvas pixel readback; relay-side `/statusz` only | **Diagnostics JSON** as primary (machine-readable, already aggregates the funnel), screenshot non-black check as secondary, `/statusz`/metrics for relay-side and cluster assertions. Relay-side alone is insufficient — it can't see decode/render failures, which is the gap this item exists to close |

## Decisions

1. **Two tiers, both PR-triggered; nothing runs on push-to-main.**
   - **Tier 1 — single-pod browser E2E**: every `pull_request` event, like
     the other CI jobs. Relay (`-dev-cert`) + `gawk-pubsim` + headless
     Chrome viewer on the runner, no containers.
   - **Tier 2 — cluster-mode E2E**: `pull_request` gated with
     `if: startsWith(github.head_ref, 'release-please--')` (user
     decision 1), plus `workflow_dispatch` as an on-demand escape hatch
     for cluster-touching work. A skipped tier-2 check reports "skipped",
     which GitHub treats as satisfying a required check — so the gate
     semantics survive the eventual required flip (Decision 8).
   - Neither tier runs on `push: main`: every change reaches main through
     a PR, and the release PR re-runs both tiers against the merged
     aggregate — a main-push re-run would double the cost for near-zero
     information. This is a deliberate deviation from the existing jobs
     (which run on both) and is recorded in the workflow comment.

2. **Minutes budget with guardrails.** Estimated steady-state cost:
   Tier 1 ≈ 4–6 min per PR push; Tier 2 ≈ 6–8 min per release-PR refresh
   (≈ one per main merge while a release is pending). At recent activity
   levels that adds roughly 20–30 % to current usage — inside the free
   tier with headroom. Guardrails, all landing with Z2:
   - **`concurrency` group per workflow + ref with
     `cancel-in-progress: true`** on the E2E jobs — a force-push
     supersedes the previous run instead of paying for it. (The existing
     jobs can join the group later; out of scope here.)
   - **`timeout-minutes` hard caps**: 10 (tier 1) / 15 (tier 2) — a hung
     QUIC handshake must cost minutes, not hours.
   - **Measured, not assumed**: Z4 records actual per-run minutes and the
     monthly projection in this doc's findings; if the projection crosses
     ~1,600 min/month total, the recorded fallback is trigger-narrowing
     (e.g. tier 1 skips draft PRs), never assertion-thinning (user
     decision 2).
   - No path filters (repo convention, `ci.yml` header comment) — tier 2's
     branch-name condition is a trigger scope, not a path filter.

3. **Publisher leg: `gawk-broadcast/cmd/gawk-pubsim` — the fixture
   publisher as a standalone CLI.** Reuses the exact mechanics of
   `relay_integration_test.go`'s `fixtureSource`: demux
   `internal/mpegts/testdata/sample.ts` into access units
   (`mpegts.NewDemuxer` + `engine.HasIDR`), feed them through the real
   `engine.Session` via the existing `MediaSourceFactory` seam — but
   **looping** the fixture with live timestamps (`clock.NowUs()` per AU,
   exactly as the test does) at a realistic pace (~30 fps flag-tunable)
   for a `-duration`, and printing the minted broadcast ID as a
   machine-readable line for the harness. Flags mirror `gawk-loadgen`
   (`-url`, `-insecure`, `-duration`, plus `-secret` for publish-secret
   setups). Notes:
   - It imports engine + wire only — **no Gio, so it builds without cgo
     or the apt header set** (Go compiles only imported packages); the
     E2E job stays header-free like the `server` job.
   - Standing value beyond CI: `gawk-loadgen` finally gets a publisher
     that isn't the gaming PC — the manual W6 scale drill becomes
     self-contained (`pubsim` + `loadgen` against any relay).
   - Frame IDs, keyframe cadence (fixture IDRs), Annex-B with empty
     extradata — all inherited from the engine; the viewer's `isAnnexB`
     sniff already handles it (proven by the R14 path).

4. **Browser harness: new top-level `e2e/` directory.** Own minimal
   `package.json` (playwright-core, a tiny runner — no test-framework
   ceremony needed for two scenarios), driving the **production viewer
   surface** (`#/view/<id>`) of the built app (`npm run build` + `vite
   preview`, the verify-skill pattern). Top-level because it spans all
   three components and `gawk-app` must not grow E2E deps in its own job.
   The harness seeds the persisted transport settings (server URL + dev
   cert hash, the keys owned by `useTransportStore`) via
   `addInitScript` localStorage writes before navigation. Debug surfaces
   (`#/debug/*`) are deliberately not used — the production path is the
   one that ships.

5. **TLS: `-dev-cert` + `serverCertificateHashes`, no trust-store work.**
   The relay's `-dev-cert` generates an in-memory P-256 cert and logs
   `cert_hash_hex`; the harness scrapes it from the log and seeds it into
   the app's existing dev-cert-hash setting
   (`transport/connection.ts` → `serverCertificateHashes`). Fallback if
   headless Chrome misbehaves with hash-pinned WebTransport (Z1 spike
   verifies): launch with `--ignore-certificate-errors-spki-list=<hash>`
   (already documented in `connection.ts` comments and `gawk-devcert`
   output). One of the two must work; Z1 records which.

6. **Success signal: the viewer's own diagnostics, with flow-shaped
   thresholds.** Primary: an `addInitScript` wraps
   `navigator.clipboard.writeText` (the exact stub
   `ViewerScreen.test.tsx` already uses), the harness opens the stats
   overlay (`Ctrl+Alt+Shift+D`), clicks **Copy diagnostics**, and parses
   the captured JSON (R9's rolling ~10 s window). Assertions are
   presence-of-flow, sized for a contended 2-core runner:
   - received fps > 0 and decoded fps above a low floor (`DECODED_FPS_FLOOR`,
     ≥ 8 with the fixture paced at 30 — lowered from 10 after the 2-core
     runner narrowly missed it on otherwise-healthy runs) sustained across
     two samples ~5 s apart;
   - rendered fps > 0; no sustained "Awaiting keyframe" stall age;
   - ingress-loss ≈ 0 (loopback link — loss here means a real bug);
   - drop counters not growing unbounded between the two samples.
   Secondary: a Playwright screenshot of the canvas element asserts
   non-black, non-uniform pixels (composited capture — no
   `preserveDrawingBuffer`, no canvas readback; the R16 finding stands).
   **Never** assert absolute fps floors near the source rate or any
   latency number — those are performance claims a shared runner cannot
   make (non-goal).

7. **Cluster tier: real chart, two pods, one kind node; edge-serving
   proven from the ops endpoints.** Pinned kind + node-image versions;
   cluster config maps a fixed NodePort via `extraPortMappings`
   (`protocol: udp`). The job builds the `gawk-server` image (reusing the
   existing docker job's GHA cache scope), `kind load docker-image`s it,
   and installs the real chart with `replicaCount=2`,
   `config.clusterMode=true`, `service.type=NodePort`, and throwaway test
   secrets (`internalPsk`, `statelessResetKey`, `resumeTokenKey`,
   `statsKey`) — exercising the chart's own replicas>1 guard. Then:
   - `gawk-pubsim` publishes via the NodePort (lands on some pod → that
     pod takes the origin Lease);
   - the browser viewer connects (tier-1 assertions, unchanged);
   - `gawk-loadgen` drives ~12 additional subscriber sessions so
     kube-proxy's UDP conntrack spreads new 5-tuples across both pods
     (P[all-on-one] ≈ 2⁻¹² — negligible, and retried once if hit);
   - assertions via TCP port-forward to each pod's ops endpoint:
     `/statusz` shows the broadcast on the origin pod; the edge pod shows
     ≥ 1 subscriber served via edge pull (R17 role labels / edge-leg
     metrics families); the browser viewer's diagnostics stay green
     regardless of which pod serves it.
   This automates docs/22's pending **two-pod smoke** ("publisher lands
   on either pod; lease shows holder + addr + generation" / "viewer on
   the non-origin pod plays") — docs/22's status gains a pointer here
   when Z3 goes green. The homelab drills (conntrack empiricism, ECMP,
   rollout blip timing, MetalLB behavior) are **not** absorbed: kind has
   none of that physics (non-goal).

8. **Flake policy: advisory burn-in, then required; flakes are findings.**
   Both tiers land as non-required checks. After a burn-in (target: ~2
   weeks or ~20 consecutive green runs on release PRs, whichever is
   longer) they flip into the required set — tier 2's skipped-on-normal-
   PRs state satisfies required-check semantics (Decision 1). Retry
   budget: up to `MAX_VIEWER_RETRIES` (5) in-harness retries of the browser
   scenario (relaunch page, not relay); no job-level auto-rerun. Every flake gets a dated entry in
   this doc's findings — a flaky E2E that gets rerun into submission is
   worse than none.

9. **Browser-broadcaster tier is a pre-registered stretch (Z5),
   droppable.** Covering the real Chromium broadcaster path (capture →
   encode → send) needs `getDisplayMedia` automation. The spike order:
   (a) `--auto-select-desktop-capture-source` in new headless (known
   fiddly; ignored when combined with `--use-fake-ui-for-media-stream`);
   (b) headful Chrome under `xvfb-run`; (c) a test-only hook injecting a
   synthetic `BroadcastMediaSource` (the seam exists in
   `media/capture.ts` / `transport/broadcaster.ts`; smallest possible
   runtime surface, e.g. only compiled in dev builds). **Kill criteria
   (pre-registered, R12-T4 style)**: if no path yields a stable
   ≤ 3-minute job within the Z5 timebox, document the rejection here and
   stop — `gawk-pubsim` already exercises wire/relay/viewer, the encode
   leg keeps its R13 probe-matrix unit coverage, and the browser-encode
   path remains on the manual verify list. A documented rejection is a
   valid completion.

10. **Chromium-only in v1; versions pinned except Chrome.** Firefox
    (main-thread pipeline, hidden-`<video>` capture fallbacks) is
    deferred — its CI story is harder and the broadcaster side is
    Chromium-anyway. Revisit-if: a Firefox-specific viewer regression
    ships that tier 1 would have caught with a second engine. kind,
    node image, kubectl, helm are version-pinned in the workflow; the
    preinstalled Chrome deliberately floats with the runner image —
    tracking current-Chrome breakage (à la the Chrome 152 `getStats()`
    removal, docs/13 D7) is part of the value.

## End-to-end path

```
Tier 1 (every PR):                     Tier 2 (release PRs only):
  go build gawk-server, gawk-pubsim      docker build gawk-server image
  gawk-server -dev-cert  ──┐             kind create (pinned, UDP portmap)
  scrape cert_hash_hex     │             kind load image; helm install
  gawk-pubsim → mint ID ───┤               (replicas=2, clusterMode=true,
  npm build + vite preview │                NodePort, test secrets)
  playwright-core:         │             gawk-pubsim → NodePort (origin)
    seed localStorage      │             gawk-loadgen ~12 subs (spread)
    open #/view/<id> ──────┘             playwright viewer → NodePort
    overlay → Copy diagnostics           tier-1 assertions
    assert flow + non-black              + per-pod /statusz + metrics:
                                           origin holds broadcast,
                                           edge served ≥1 subscriber
```

## Status

| Chunk | Scope | Acceptance criteria | Status |
|-------|-------|---------------------|--------|
| Z1 | **`gawk-pubsim` + harness spike** — the fixture-publisher CLI (Decision 3); `e2e/` skeleton (Decision 4); local proof that headless system Chrome does WebTransport against `-dev-cert` via `serverCertificateHashes` (Decision 5 — the load-bearing unknown) and decodes the fixture | `gawk-pubsim` publishes the looping fixture against a local relay, visible in `/statusz` (Go test for the loop/timestamp logic; CLI smoke documented); harness run locally green end-to-end with the exact command recorded; diagnostics JSON captured via the clipboard stub with decoded frames > 0; **spike verdict recorded in this doc**: hash-pinned WebTransport in headless — works / needs SPKI flag / needs headful+Xvfb | ✅ done 2026-07-18 — **verdict: works, no flag, no Xvfb** (findings below) |
| Z2 | **Tier-1 CI job** — wire the harness into `ci.yml` (new `e2e` job, `pull_request` only); guardrails (concurrency cancel-in-progress, `timeout-minutes: 10`); assertions per Decision 6 | Job green on a real PR; deliberately-broken runs go red (kill `gawk-pubsim` mid-test → flow assertions fail; wrong broadcast ID → fails) — the test-the-test check; measured runtime ≤ 6 min recorded here; a force-push cancels the superseded run; job is advisory (not required yet) | ✅ green on every PR since 2026-07-18 (test-the-test passed locally — both breakages red); advisory (not required yet); per-run minutes accounting rolls into Z4 |
| Z3 | **Tier-2 kind job** — pinned kind + UDP `extraPortMappings`; image build (shared GHA cache scope) + `kind load`; real chart install (replicas=2, clusterMode, NodePort, test secrets); pubsim + loadgen + browser viewer; per-pod ops assertions (Decision 7); `release-please--*` gate + `workflow_dispatch` | Green on a release PR (or `workflow_dispatch`); chart's replicas>1-requires-clusterMode guard exercised; origin/edge split proven from per-pod `/statusz`/metrics with the browser viewer green; measured runtime ≤ 10 min recorded; docs/22 status updated to point at this as the automated two-pod smoke | ✅ green on the 2026-07-18 release PRs (runs `29659639321` / `29659067892`): step 18 asserted the origin/edge split from per-pod `/statusz` (origin `publisherActive` + `edgeSessions: 1`, edge serving 7 real subscribers, `framesRelayed` within 9) with the browser viewer green; job wall ≈ 4m44s. This executed docs/22's pending two-pod smoke in real CI. Re-run on the new self-hosted `ioio-k8s` runners pending (migration landed 2026-07-19, after these GitHub-hosted greens) |
| Z4 | **Burn-in → required + budget findings** — flake log, minutes accounting, required-check flip; README/ROADMAP/CLAUDE status sync | ≥ 2 weeks / ~20 consecutive green release-PR runs with every flake root-caused in this doc's findings; measured monthly minutes projection recorded and within budget (Decision 2, incl. the trigger-narrowing fallback assessment); both tiers in the required set with tier 2 skipping cleanly on normal PRs; docs synced | 📋 not started |
| Z5 | **Browser-broadcaster stretch (droppable)** — spike per Decision 9 order; if viable, a tier-1 variant where the browser publishes (pubsim retired from that scenario or kept as the cluster-tier publisher) | Either: browser-publisher scenario stable in CI at ≤ +3 min with capture path + encode funnel asserted from broadcaster diagnostics; **or** a documented rejection with per-path findings (flags/Xvfb/injection) and the kill criteria verdict — both are valid completions | 🔧 implemented 2026-07-19 — **spike verdict: viable in headless, via tab capture** (findings 12–14); second tier-1 step `node run.mjs --browser-broadcast` publishes from the production broadcaster surface with the funnel asserted from broadcaster diagnostics; test-the-test passed locally (both breakages red, finding 15); ~17 s wall locally — green-in-CI + the ≤ +3 min measurement pending the first CI run |

Ordering: Z1 → Z2 ship the every-PR value first and de-risk the one real
unknown (headless WebTransport) before any CI wiring; Z3 needs nothing from
Z5 and lands the release gate; Z4 runs as a calendar-time wrapper around
normal development; Z5 is last and droppable. Nothing here blocks other
roadmap work — R15/R18/R19 implementation PRs would simply start getting
tier-1 verdicts once Z2 lands.

## Implementation status & findings (2026-07-18, Z5 2026-07-19)

Z1 done; Z2/Z3 implemented and **now green in real CI** (finding 10 below) —
tier-1 `e2e` on every PR and `e2e-cluster` on the 2026-07-18 release PRs, so
Z3's green-on-a-release-PR acceptance is met. What remains is Z4's
calendar-gated burn-in (and a re-run on the new self-hosted runners, finding
11). Z5 implemented 2026-07-19 (findings 12–15) — the browser-broadcaster
spike succeeded on Decision 9's first path, so the injection hook (c) was
never built and Xvfb (b) never needed.

1. **Z1 spike verdict: hash-pinned WebTransport in headless Chrome works
   outright.** New-headless Chrome for Testing 149 (playwright-core driving
   the playwright Chromium build locally; CI uses the runner's system
   Chrome) connected to `gawk-server -dev-cert` purely via
   `serverCertificateHashes` — the harness seeds the scraped
   `cert_hash_hex` into the app's persisted `gawk.certHashHex` before
   navigation. No `--ignore-certificate-errors-spki-list`, no headful, no
   Xvfb. The full production pipeline ran in that browser: nested
   transport/decode workers, WebCodecs, WebGL sink, adaptive pacing +
   interpolation (rendered fps reads ≈2× received — the R12 α=0.5
   mid-slots; the flow floors are unaffected).
2. **Tier-1 local run is green end to end** — exact commands in
   `e2e/README.md` (`node run.mjs` after building `e2e/bin/` + the app).
   Measured flow on this dev box: received/decoded ≈ 30/30 fps, 140 frames
   completed between the two captures, canvas luma stddev ≈ 71 (colorful),
   relay `/statusz` zero ingress loss. The one local-env wrinkle (not a CI
   concern): a distro with no system Chrome needs
   `libnss3/libnspr4/libasound2t64`, extractable user-space via
   `apt-get download` + `dpkg -x` + `LD_LIBRARY_PATH`.
3. **Test-the-test passed (Z2's meta-criterion, run locally)**: a wrong
   broadcast ID → red (timeout waiting for the first completed frame,
   through the in-harness retries); killing the publisher mid-run → red
   (`last frame 4081 ms ago, want < 3000`, then the retries time out since
   the publisher stays dead). A third breakage was observed by accident
   during bring-up: a stale relay holding the port while the harness
   pinned the fresh (dead) relay's hash → the viewer never connects — the
   cert-mismatch failure mode fails closed as designed.
4. **The tier-2 flow was rehearsed locally on a real kind cluster** with
   exactly the workflow's pins (kind v0.30.0, `kindest/node:v1.34.0`,
   helm v3.19.0) — this executed docs/22's pending **two-pod smoke** for
   real: chart install with `replicas=2` + `clusterMode` + NodePort 30443 +
   throwaway fleet secrets came up clean; pubsim + 12 `gawk-loadgen`
   viewers + the browser all rode the kind UDP `extraPortMappings`;
   per-pod `/statusz` showed the split (origin: `publisherActive`, 8 local
   subscribers, `edgeSessions: 1`; edge: `role: "edge"`, 4 local
   subscribers, framesRelayed within 5 of the origin's) and the browser
   viewer stayed green against the cluster. The kubectl pin (v1.34.0) is
   the one thing the rehearsal didn't exercise (local kubectl served); the
   first CI run covers it.
5. **Edge pods verify internal TLS against the system root pool** — the
   R17 edge dialer deliberately never sets `InsecureSkipVerify`, so a
   self-signed E2E cert would break the origin→edge pull. The fix needs
   zero code: `SSL_CERT_FILE=/tls/tls.crt` via the chart's `extraEnv`
   makes Go's root pool the mounted self-signed cert itself (a self-signed
   cert verifies as its own root; SANs must cover
   `config.internalServerName`, hence `-hosts localhost,127.0.0.1` and
   `internalServerName=localhost`). Worth remembering for any
   non-cert-manager cluster deploy.
6. **Chart change**: `service.nodePort` (empty default renders nothing) so
   kind can map a fixed host port; exercised by the rehearsal.
   `config.connBurstLimit=50` in the tier-2 install because every session
   arrives SNAT'd from one host IP through the kind portmap — the default
   3/s bucket would reject part of the 13-session join herd.
7. **The fixture moved** from `internal/mpegts/testdata/sample.ts` to a new
   `internal/fixture` package (embedded as `fixture.TS`): `go:embed`
   requires colocation, and embedding is what makes `gawk-pubsim` a
   self-contained binary for the manual W6 drill. The mpegts/engine/gst
   tests now read `fixture.TS`/the new path; provenance README moved with
   it.
8. **`gawk-loadgen`'s `gaps` counter over-counts by design since R8**:
   keyframes ride reliable streams, so the datagram frameID sequence skips
   one ID per GOP — exactly 2 gaps/s/viewer at the fixture cadence, which
   the kind rehearsal reproduced (24 gaps/s, 12 viewers). Read `gaps` as a
   delta over that structural baseline, never as absolute loss (comment
   now also on the counter itself).
9. **Advisory status**: both jobs land as non-required checks; the Z4
   burn-in (≥ 2 weeks / ~20 green release-PR runs, every flake root-caused
   here) gates the required flip. Runtime budget will be measured from the
   first real runs (estimate from local timings: tier 1 well under the
   6-minute target; tier 2's cluster bring-up was ~1 min on a warm image —
   CI's cold node-image pull adds the documented ~1 min).
10. **Both tiers went green in real CI on 2026-07-18** (the Z2/Z3 acceptance
    half that a dev box can't self-certify). Tier-1 `e2e` runs on every PR
    and has been green (e.g. renovate PRs). Tier-2 `e2e-cluster` ran on the
    release-please PRs (runs `29659639321` and `29659067892`): the full job
    — kind create, image build + load, `helm install` (replicas=2 +
    clusterMode + NodePort + fleet secrets + `SSL_CERT_FILE`), pubsim +
    12 loadgen + browser viewer, then `cluster-assert.sh` — completed
    green in ≈ 4m44s, with step 18 logging
    `PASS: origin=… edges=…`, origin `publisherActive` +`edgeSessions: 1`,
    the edge pod serving 7 real subscribers, and the browser viewer's flow
    green against the cluster. This is docs/22's two-pod smoke executed in
    CI, and it discharges R17's kind-smoke drill. The R18 viewer-count
    assertion (origin `viewersGlobal` == Σ per-pod real viewers) was added
    to `cluster-assert.sh` 2026-07-19 and will be exercised on the next
    release-PR run.
11. **CI runner migration (2026-07-19) postdates those greens.** The
    2026-07-18 green runs were on GitHub-hosted runners; on 2026-07-19 all
    workflows moved to self-hosted `ioio-k8s` runners (kind-in-a-pod, buildx
    with host networking for the 1480-MTU pod network). The cluster logic
    under test is unchanged, but `e2e-cluster` has not yet re-run green on
    the new substrate — the next release-please PR (or a `workflow_dispatch`)
    re-proves it there. This is a required-flip precondition alongside the
    Z4 burn-in.
12. **Z5 spike verdict (2026-07-19): the browser-broadcaster leg is viable
    in headless Chrome — via tab capture, never screen capture.** Decision
    9's path (a) splits in two once you look at pixels:
    `--auto-select-desktop-capture-source=Entire screen` **grants** and
    delivers a steady 30 fps whose every frame is **solid black** (the
    headless "monitor" surface has no content — useless for an E2E that
    asserts non-black video), while
    `--auto-select-tab-capture-source-by-title=<title>` grants tab capture
    (`displaySurface: "browser"`) with real pixels at the full 30 fps —
    including from a **background** tab (being captured keeps the tab
    painting and rAF unthrottled; spike-measured 91 frames/3 s). Two
    non-paths, confirmed so nobody retries them:
    `--auto-accept-this-tab-capture` leaves the getDisplayMedia promise
    pending forever (it only auto-accepts `preferCurrentTab` requests,
    which the app doesn't make), and adding `--use-fake-ui-for-media-stream`
    to the desktop flag turns the grant into `NotReadableError` — Decision
    9's "ignored when combined" caveat confirmed. Paths (b) Xvfb and (c)
    the injected-source hook were never needed.
13. **Z5 scenario shape** (`node run.mjs --browser-broadcast`, a second
    tier-1 CI step with `GAWK_E2E_OUT_DIR=out-browser-broadcast` keeping
    failure artifacts apart): no pubsim — a second headless Chromium opens
    an animated-canvas tab (title `gawk-e2e-motion-source`; full-viewport,
    moving gradient so the viewer's non-uniform screenshot check holds)
    plus the production `#/broadcast` surface, clicks the real "Start a
    stream" button (the launch flag auto-selects the anim tab in the
    picker), waits for the LIVE topbar and scrapes the minted code from
    it, then asserts the encode funnel from the broadcaster's own
    Copy-diagnostics JSON: median capture/encoder/sent fps ≥ 10 / ≥ 5 /
    > 0, encoded frames + keyframes + keyframe streams + datagrams +
    bytes all growing between two captures ~5 s apart, zero
    keyframe-stream failures on loopback, encoder drops bounded by
    encodes, and the codec an `avc1.*` variant (negotiation landed on
    `avc1.4D4034` "realtime" software — no GPU headless/in CI). The
    unchanged viewer scenario then runs against the scraped ID with the
    codec assertion relaxed to `/^avc1\./` (negotiated, not
    fixture-determined). Two harness accommodations: the publish-secret
    prompt (which defaults **on** for dev/loopback hosts) is disabled by
    seeding `window.__GAWK_CONFIG__ = { requirePublishSecret: false }` in
    the init script, and the relay-side assertion now counts **active**
    publishers only, so a retried broadcaster attempt's grace-period hub
    can't trip the exactly-one check. Measured locally: the whole mode is
    ~17 s wall with a 30/30/30 funnel; the ≤ +3 min CI budget has an
    order of magnitude of headroom on paper, CI's own number pending.
14. **The R11 worker offload does not engage in headless Chrome 149 — the
    scenario records the pipeline placement instead of asserting it.**
    From a proper localhost origin, the worker scope has `VideoEncoder`
    and `WebTransport` but **no `MediaStreamTrackProcessor`**, and
    `MediaStreamTrack` transfer throws `DataCloneError` (with
    `--enable-blink-features=MediaStreamTrackTransfer` the message merely
    changes to "could not be serialized"), so `createBroadcastSession`'s
    capability probe correctly falls back to the main-thread pipeline —
    which held the full 30/30/30 funnel anyway; both placements are
    legitimate production paths (Firefox is always main-thread). Two
    corollaries worth keeping: (a) whether stock *headful* Chrome engages
    the worker path is exactly R11's still-pending manual verify — the
    overlay's Pipeline row answers it in one glance on the gaming PC; (b)
    don't probe worker capabilities from `about:blank` — its opaque
    origin is not a secure context, which hides even WebCodecs/
    WebTransport and made the first probe read entirely wrong.
15. **Z5 test-the-test passed (run locally)**: a capture flag matching no
    tab → red (getDisplayMedia never settles, the scenario times out
    waiting for the LIVE stage, through the one broadcaster retry);
    closing the publisher browser right after LIVE → red (the viewer
    times out waiting for its first completed frame). Both exit 1.
16. **Second tier-1 viewer pass: R19 resilient mode (2026-07-22)** — from
    review finding `PRODUCT-1` (docs/24 finding 10). The harness only ever
    subscribed in default datagram mode, so the browser's reliable-carrier
    reader and the `?delivery=reliable` negotiation had no end-to-end
    coverage anywhere. `node run.mjs` now runs the viewer scenario a second
    time with `gawk:resilient-mode` seeded before app boot (the mode is
    negotiated at connect, so the persisted toggle is the only way in) and
    adds `assertCarrierFlow`: `deliveryMode === 'reliable'` — the
    `reliable-requested` degradation is a *failure* here, since both ends
    are this build — plus carrier rotation and record growth between the two
    captures. Aborted carriers are **recorded, not asserted**: a reset GOP
    tail is the relay shedding a slow subscriber, which a contended runner
    can legitimately provoke.
    Shape notes: it reuses the running relay, publisher and preview, so the
    marginal cost is one browser session (~12 s locally, artifacts tagged
    `resilient-<attempt>` so neither pass overwrites the other's evidence);
    it gets 1 retry rather than the main pass's 5, because it runs *after* a
    scenario that already proved flow on this broadcast, so a failure points
    at the carrier path rather than at runner weather; and the plain tier-1
    watchdog moves 300 s → 360 s to keep the same headroom the
    browser-broadcast mode has. It runs in the plain tier-1 step only —
    tier 2's value is the origin/edge split (asserted per pod in
    `cluster-assert.sh`, and `?delivery=reliable` is deliberately never
    propagated origin→edge), and the browser-broadcast step spends its
    budget on the encode funnel. **No loss is injected**: behaviour under
    loss is owned by a Go relay-integration test
    (`gawk-server/internal/transport/resilient_loss_test.go`, docs/24
    finding 10), which gets determinism a browser on a shared runner cannot.
    Test-the-test: seeding the toggle off makes all four assertions fire
    (`deliveryMode = datagrams`, `carrierStreams = 0`, no rotation, no
    records) through the retry, and the run exits 1.
17. **Striped viewer pass + the large-frame fixture (2026-07-29, R30
    docs/35 §12)** — the default fixture's ~2–4-chunk deltas sit under one
    stripe share, so the R30 controller correctly engages nothing against
    it and a striped pass would have asserted the designed no-op. A second
    committed clip (`fixture.TSLarge`: 720p temporal noise, deltas
    p10=12/median=15/max=20 chunks, property pinned by
    `TestLargeFixtureDeltasExceedBurstThreshold`) published via
    `gawk-pubsim -fixture large` gives tier-1 a **third** publisher and a
    striped viewer pass: `gawk:stripe-mode` seeded `'on'` (a zero-loss
    loopback never fires the auto detector), `assertStripeFlow` checking
    capability, `stripeNeeded ≥ 2`, `stripeActive ≥ 2`, zero leg deaths,
    frames completing through the stripe and bounded incompletes. The base
    pass doubles as the reverse control (`auto` on small frames stays
    unstriped — its unchanged assertions prove the hold). Tier 2 re-runs
    the external browser pass striped (`GAWK_E2E_STRIPE`, quoted — bare
    `on` is YAML boolean true) against a large-fixture broadcast through
    the kind NodePort, meeting real kube-proxy 5-tuple hashing, and
    `cluster-assert.sh` asserts the durable engagement counters
    (`stripeTransitions` + `stripeSuppressedDatagrams` summed across pods)
    because the browser session is closed by the time the script runs.
    Burst-buffer PHYSICS stay in Go
    (`gawk-server/internal/transport/stripe_loss_test.go`) for the same
    determinism reason as finding 16's loss ownership.
    Test-the-test came free: the first local run failed red through the
    retry on `assertFlow`'s codec pin — the 720p clip's SPS is
    `avc1.42C01F` (baseline level 3.1) where the small clip is level 1.3 —
    proving the shared flow assertions run inside the striped pass; the
    pass now pins the large clip's codec exactly, like the default pin, so
    a regenerated clip that shifts profile/level fails loudly rather than
    decoding as a surprise.

## Verification plan (the meta-question: how we know the E2E itself works)

1. **Test-the-test (Z2/Z3 acceptance)**: deliberately broken runs must go
   red — publisher killed mid-run (flow collapses), wrong broadcast ID
   (no frames at all), relay started without `-dev-cert` while the harness
   pins the hash (connect fails). Green-when-broken is this pipeline's
   only real failure mode and it is checked explicitly.
2. **Flake burn-in (Z4)**: advisory period with every flake root-caused
   before any check becomes required.
3. **Budget check (Z4)**: measured minutes per run × observed run counts
   vs the 2,000-minute envelope, recorded here.
4. **Cross-validation**: the first release cut after Z3 goes green is
   manually spot-verified end-to-end once (browser against the deployed
   homelab relay) — confirming the CI verdict predicts the deployed
   reality it exists to protect.

## Non-goals

- **Performance/latency benchmarking** — a contended 2-core runner cannot
  make glass-to-glass or fps-ceiling claims; assertions are flow-shaped
  only. The R5/R12 instruments remain the measurement story, on real
  hardware.
- **Hardware codec paths** — no runner GPU; gaming-PC encode and on-device
  verifies (iPhone fullscreen, native broadcaster on real hardware) stay
  manual by nature.
- **The homelab drills** — conntrack flush empiricism, ECMP re-hash,
  rollout blip timing, MetalLB behavior (docs/22): kind has none of that
  physics. Automating the two-pod smoke is in scope; the drills are not.
- **The 200-viewer scale proof** (docs/22 W6) — stays a manual drill;
  tier 2 uses `gawk-loadgen` only as a ~12-session spread driver. (Z1's
  `gawk-pubsim` does make the manual drill self-contained as a side
  effect.)
- **Firefox / multi-engine matrix in v1** — Decision 10, with a recorded
  revisit-if.
- **CI-driven deploys** — locked decision (docs/05): CI publishes, the
  cluster deploys itself; this item adds gates, not deploy paths.
- **Load/soak/chaos testing in CI** — beyond a smoke-scale spread driver,
  nothing long-running or adversarial on billable minutes.

## Rejected

- **Self-hosted runner / persistent test cluster** — maintenance and
  queueing for a solo project, a CI runner with network position inside
  the homelab (rubs against the "no cluster credentials in GitHub"
  boundary), and accumulated cluster state as a flake source. Ephemeral
  kind is cheaper than it looks and stateless by construction.
- **Out-of-cluster kubeconfig mode in `cmd/gawk-server`** — a prod-code
  test-only branch that still needs a real API server for Leases, while
  skipping kube-proxy/Service behavior kind gives for free.
- **Skipping the cluster tier entirely (single-pod-only E2E)** — cluster
  mode is exactly where a browser-invisible regression class lives
  (lease/edge/drain paths); user decision 1 places it on release PRs
  rather than dropping it.
- **`@playwright/test` with downloaded browsers / Puppeteer / Selenium** —
  the verify-skill recipes already exist for playwright-core + system
  Chrome; a downloaded browser adds ~1 min + disk per run for no
  additional coverage in v1 (revisited if Decision 10's Firefox
  revisit-if fires).
- **Clipboard-permission juggling** — granting real clipboard permissions
  to read the diagnostics is strictly worse than the existing
  `writeText`-stub pattern from `ViewerScreen.test.tsx`.
- **Canvas readback for the non-black check** — R16: readback of the
  WebGL canvas needs `preserveDrawingBuffer` and lies on some engines;
  Playwright's composited screenshot asserts the same thing without
  touching the render path.
- **MetalLB-on-kind for LoadBalancer parity** — NodePort exercises the
  same kube-proxy UDP path the E2E needs; LB-specific behavior is homelab
  drill territory.
- **Path filters on the E2E jobs** — repo convention (`ci.yml` header);
  tier 2's release-branch condition is trigger scoping, not path
  filtering.
