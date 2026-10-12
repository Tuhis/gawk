# R40 — Automatic content flagging (docs/71)

**Status**: designed 2026-10-12 (owner decisions OD1–OD11), not started.
Chunks **CF0–CF8** (`CF` = Content Flagging, reserved for R40 in the
roadmap). Touches the relay (one ops route, one knob, one appended wire
field), `gawk-admin` (the reserved content-flag contract made real, a flag
lifecycle, a migration, portal UI), `gawk-server/events`, the three
broadcaster surfaces (one disclosure line) and a **new fifth top-level
module, `gawk-sampler`**. **Depends on R39** ([docs/42](42-admin-moderation-portal.md)),
whose §4.11 froze the contract this milestone implements, and on **R42**
([docs/44](44-rooms.md)) for the room exemption.

## 1. Purpose

R23 gave the platform usage terms and R39 gave the operator a fleet-wide
kill and durable bans, but both assume the operator *finds out*. A
single-operator platform has no 24/7 eyes. R40 closes the gap between "an
abusive broadcast starts" and "the operator learns of it" from hours to
seconds: a sampler looks at every live broadcast's latest keyframe every
few seconds, and a sustained explicit-nudity signal pages the operator and
pins the broadcast in the portal.

What it is honest about: the instrument is a general nudity detector used
as a **proxy**. The category we most want off the platform is CSAM; the
tooling for that (PhotoDNA, CSAI Match) is gated to vetted platforms and
matches only *known* material, so it is unavailable and inapplicable to live
screen capture. A nudity detector detects skin, not legality — a human
decides. This is not a nudity platform, so any sustained explicit signal is
grounds to look.

**Legal posture (DSA)**: under Art. 7 (Good Samaritan), voluntary
own-initiative detection does not forfeit the Art. 6 hosting exemption, but
knowledge gained must be acted on expeditiously. An unwatched flag queue is
worse than no scanner, so the design guarantees a flag always has a route to
a human (D12) and keeps re-paging until someone acknowledges it (D9).

## 2. Owner decisions (2026-10-12)

| # | Question | Decision |
|---|---|---|
| OD1 | What may the screener do on its own at very high confidence? | **An optional auto-kill tier, built and tested, default off.** |
| OD2 | What happens to a flag nobody acknowledges? | **Re-page while the signal persists; after a deadline, kill.** The deadline kill is **built but off by default**, like OD1 — re-paging is the only default action. |
| OD3 | Which content? | **Sexual / explicit nudity only** (exposed genitalia, anus). Partial nudity (breasts, buttocks, swimwear) does not count — game footage triggers it constantly. No gore/violence classifier. |
| OD4 | How does the operator judge a flag? | **Join the live broadcast, unblurred**, through the portal's existing Watch link. No viewer change. |
| OD5 | Which model? | **NudeNet's detector** (YOLOv8 body-part detector, ONNX). Its per-label output is what makes OD3 expressible. Resolution (320n vs 640m) and default thresholds are set by CF0's calibration. |
| OD6 | Where does the sampler live? | **A fifth top-level module, `gawk-sampler`**, with its own image, chart and release-please component — the `gawk-telemetry`/`gawk-admin` shape. |
| OD7 | Which broadcasts are screened? | **All of them, with an operator-managed exempt list** of static rooms (D11). |
| OD8 | Reporting to authorities? | **Out of scope.** R40 stops at flag, kill and ban; reporting is the operator's own business. This doc and the self-hosting guide say so; nothing in the product files or prepares a report. |
| OD9 | How do broadcasters learn they are screened? | **A paragraph in the default R23 terms, plus a relay-advertised notice** the broadcaster surfaces show as one line (D13). |
| OD10 | Compute and cadence? | **CPU only; one sample per broadcast every 5 s.** Overload stretches the interval and raises a metric, never silently drops broadcasts (D6). |
| OD11 | What does dismissing a flag do? | **Mutes re-flagging of that broadcast for a fixed time** (default 30 min), after which a persisting signal flags again. |

## 3. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **The sampler reads keyframes from a new relay ops route, `GET /internal/screening/broadcasts/{id}/keyframe`**, authorized by its own bearer, `-screening-token` (`GAWK_SCREENING_TOKEN`), distinct from `-admin-api-token`. Empty token ⇒ the route is not registered (404). It returns the hub's cached keyframe — the `StreamFrame` bytes **verbatim** (24-byte header, embedded `DecoderConfig`, payload) — with `X-Gawk-Codec` (the hub's probed family) and an `ETag` of publisher session + frame ID; `If-None-Match` gets `304`. `404` when the pod holds no keyframe for that ID. | The keyframe is already cached per broadcast (`internal/hub/hub.go`, `cachedKeyframe`) and refreshed every ~500 ms (the broadcasters' GOP), so one GET is a fresh, self-contained frame. The alternative tap, `/internal/subscribe/{id}`, delivers the *whole stream*: hundreds of broadcasts × several Mbps into one sampler to look at one frame every 5 s, and it lives under `internal/` where a fifth module cannot import it. A separate token is least privilege: the admin token also reads publisher IPs and is held by the portal. `304` makes a broadcaster who is away (cache persists for the GC grace) cost nothing. |
| D2 | **The sampler never discovers broadcasts itself; `gawk-admin` serves it a target list**: `GET /api/v1/screening/targets` (role `flagger`), one entry per live *origin* broadcast with its raw ID, HMAC key, the ops address of the pod holding it, and `muted`/`exempt` booleans. Exempt broadcasts are listed (so the sampler's metrics see them) but marked skip. | `gawk-admin` already scrapes every relay pod (`internal/relayscan`) and is the one place that knows rooms, exemptions and mutes. A sampler holding the relay admin token to scan pods itself would hold publisher IPs it never needs. One list also means one answer to "is this broadcast screened?". |
| D3 | **Shared types live in a new public package, `gawk-server/screening`**: the target-list response, the `gawk.content-flag.v1` request body (docs/42 §4.11, unchanged), the keyframe route path and headers. `gawk-admin` and `gawk-sampler` both import it; a semantic change needs commits scoped to both in the same PR. | The repo rule: shared Go is a public package in `gawk-server`, never mirrored (`wire`, `events`, `adminapi`, `oidcauth` precedent). |
| D4 | **The sampler is Go; decode runs in an `ffmpeg` subprocess per sample, inference in-process through ONNX Runtime** (`onnxruntime_go`, the shared library in the image). The sampler converts the frame to a bitstream ffmpeg can read from a pipe — AVCC → Annex-B with SPS/PPS from the `avcC` extradata, Annex-B as is, VP8/VP9 wrapped in IVF — and reads back one RGB frame scaled to the model's input. Each subprocess runs with a wall-clock timeout, `RLIMIT_AS`, and **`RLIMIT_CORE` = 0**. | Crash isolation was the open design question: a malformed bitstream must kill at most one sample, never the sampler. A cgo libavcodec would put a decoder CVE into the sampler's address space. A fork per sample every 5 s × 100 broadcasts is 20 forks/s, which is cheap. ONNX Runtime sees only a fixed-shape float tensor the sampler built, so in-process is safe. A core dump would write a frame to disk, which the evidence rule (D8) forbids. |
| D5 | **The image ships a minimal ffmpeg** built in a Dockerfile stage with `--disable-everything` and only the `h264`, `vp8` and `vp9` decoders, the `h264`/`ivf` demuxers, the `pipe` protocol and `swscale`. **Model weights are not in the image**: the chart fetches them in an init container from a pinned URL and verifies a pinned SHA-256 into an `emptyDir`, or mounts an operator-provided volume. | Smallest decoder CVE surface for the three codecs any broadcaster sends (web: H.264/VP9/VP8; desktop and iOS: H.264 Annex-B; nobody sends AV1 or HEVC). Keeping weights out of the GHCR image means gawk never redistributes them, whatever NudeNet's weights licence turns out to be (CF0 verifies it); the SHA pin keeps a swapped file from loading. |
| D6 | **Scheduling**: one sample per screened broadcast every `-interval` (5 s, OD10), spread across the interval rather than in bursts, through a worker pool of `-workers`. When the pool cannot keep up, each broadcast's effective interval stretches evenly, `gawk_sampler_interval_effective_seconds` rises, and a chart-provided `PrometheusRule` fires above 2× the target. Every listed broadcast stays in the rotation. | A sampler that silently skips some broadcasts under load is the "silent backlog" the DSA posture forbids; stretching keeps coverage complete and makes the shortfall visible. |
| D7 | **Scoring**: a sample's score is the maximum confidence among the **flagged labels** — default `FEMALE_GENITALIA_EXPOSED,MALE_GENITALIA_EXPOSED,ANUS_EXPOSED` (OD3; `-labels`). A broadcast's window is its last `-window` samples (default 6 ≈ 30 s); it **trips** when at least `-window-hits` samples (default 4) score ≥ `-threshold`, and **clears** only when the whole window is below it. Defaults for `-threshold`, `-window` and `-window-hits` are placeholders until CF0's calibration replaces them in this table. A new publisher session resets the window. | Never decide on a single frame: games produce skin-toned false positives constantly. Hysteresis keeps a flickering score from raising and clearing flags every few seconds. The label set is configurable because "explicit only" is a policy, not a constant. |
| D8 | **Evidence is metadata only.** Frames exist only in the sampler's memory between fetch and score; they are never written to disk, logged, sent in a flag, or exposed on a debug endpoint. The flag carries scores, the window, the labels that fired and their counts. | If the flag is right, keeping the material is itself the legal hazard. The operator inspects by joining live (OD4). |
| D9 | **Flag lifecycle (in `gawk-admin`)**. While its window is tripped, the sampler POSTs `/api/v1/content-flags` after every sample. The first POST for a broadcast without an open flag **raises** one: a `content_flags` row, a `content_flag.raised` event (the webhook *is* the page), and the broadcast pinned in the portal (`201`). Further POSTs **update** that flag's latest window (`200`). States: `open` → `acknowledged` → `resolved` (`dismissed`, `killed`, `auto_killed`, `ended`). An open flag whose signal is fresh (updated within 3 × interval) and that nobody has acknowledged emits `content_flag.reminded` every `-content-flag-repage` (10 m). Acknowledging stops reminders and deadlines; it does not resolve. A kill through R39 resolves as `killed`; the publisher session ending resolves as `ended`. | "A flag must reach a human" made mechanical. Updates instead of new flags keep one row per incident. Acknowledge is a separate step so the operator can say "I'm looking" from a phone before deciding. Reminders stop when the signal does — re-paging about a broadcast that has gone clean is noise. |
| D10 | **Dismiss mutes the broadcast for `-content-flag-mute` (30 m, OD11)**: POSTs during the mute return `202 muted` and change nothing; the target list marks it `muted`. When the mute expires, a still-tripped window raises a **new** flag. Auto-kill and deadline kill never act on a muted broadcast. | OD11. A fixed time still catches a broadcaster who switches content after the operator has looked. `202` lets the sampler count mutes in its metrics without treating them as errors. |
| D11 | **Exemption is a field on the `Room` CR, `spec.screening: exempt`**, set from the portal (operator only, audited as `room.screening_changed`). Only a **static room with an attach secret** can be exempt; the portal refuses others. A broadcast is exempt while attached to an exempt room. Broadcasts outside any room are always screened. | OD7. The `Room` CR is the room's only durable record (docs/62 rejected a Postgres side-list for the same reason). An open or dynamic room could be joined by anyone, so exempting it would hand an abuser an unscreened lane; the attach secret is what makes "my friends' room" a trust boundary. |
| D12 | **No scanner without a pager.** `GET /api/v1/screening/targets` returns `503 no_flag_route` when no enabled webhook would deliver `content_flag.raised` (config-sourced or UI-created, honouring R49's per-webhook `events` filter). The sampler then samples nothing, logs once per minute, and exports `gawk_sampler_paused{reason="no_flag_route"} 1`; the portal shows a banner. | The DSA point in §1: detecting without anyone to tell is worse than not detecting. Pausing at the source is the only place this can't be bypassed by a misconfigured webhook list. |
| D13 | **Disclosure (OD9)**: the relay gains `-screening-notice` (bool, default false; chart `screening.notice`), advertised as an **appended field of `RelayIdentity`** (docs/40 §4.4 — trailing bytes are that message's extension mechanism; the flags byte stays strict). The broadcaster surfaces that already parse `RelayIdentity` in their server probe — `gawk-app`, the desktop engine, `gawk-ios` — show one line on the broadcast screen: "This server automatically screens streams for explicit content." The default `gawk-app/public/terms.html` gains a screening paragraph; operators with custom terms write their own. | `RelayIdentity` was built as the managed-mode extension point, so no new wire type and no change to publish sessions. A separate knob (rather than "a sampler exists") because the relay cannot see the sampler, and an operator who screens must be able to say so — the chart docs pair the two. The line is a notice, never a gate (docs/29 D8). |
| D14 | **Auto-kill tier (OD1), default off**: `gawk-admin -content-flag-auto-kill` (`off` or a policy `score>=0.9,hits>=10/12`). Evaluated by `gawk-admin` on each flag update, never by the sampler. On a match it kills through R39's kill path with a cooldown ban of `-kill-cooldown`, actor `system:content-flag`, and resolves the flag `auto_killed` — the page still goes out. | The sampler holds `flagger`, which grants *flag-only* rights (docs/42 D17): it must never be able to kill. Putting every automated action in `gawk-admin` keeps one actuator and one audit trail. The tier needs a longer window than the sampler's default, which is why the policy names its own sample count. |
| D15 | **Deadline kill (OD2), default off**: `gawk-admin -content-flag-deadline` (`off` or a duration, e.g. `30m`). An `open`, unacknowledged, unmuted flag whose signal is still fresh at the deadline is killed exactly as in D14 and resolved `auto_killed` with `reason: "deadline"`. | OD2: re-paging is the default; the kill is a switch for an operator who would rather lose a stream than leave one up overnight. A flag whose signal went clean is not killed. |
| D16 | **Events**: `gawk-server/events` gains `content_flag.reminded` and `content_flag.resolved`, and `ContentFlagRaisedData` gains the additive fields the §4.11 body already carries (`samplerId`, `window`, `scores`, `labels`) plus `flagId`. Schemas, golden vectors, AsyncAPI and the webhook channel all follow; all three are webhook-deliverable. No event ever carries a publisher IP (R39 prohibition). | The reserved `content_flag.raised` today has only `actor`, `broadcastKey`, `broadcastId` and `reason` — not enough to page usefully. Additive fields keep existing consumers valid. |
| D17 | **Retention**: `content_flags` rows are part of the moderation audit trail and are never pruned by `-activity-retention`. | They are scores and timestamps — the record of having acted expeditiously is the point. |
| D18 | **The sampler authenticates with the OIDC client-credentials grant** (`-oidc-issuer`, `-oidc-client-id`, client secret from a Secret), caching the token until shortly before expiry. Its client carries only `flagger`. | docs/42 §4.11 as frozen; the same recipe as R49/R60's bots. |

### Rejected

- **Tapping `/internal/subscribe/{id}`** — whole-stream bandwidth for one
  frame, and an `internal/` client a fifth module cannot import (D1).
- **The sampler holding the relay admin token and scanning pods** — it
  would read publisher IPs it never needs (D2).
- **cgo libavcodec** — a decoder crash or CVE inside the sampler's address
  space (D4).
- **The sampler killing directly, or holding `operator`** — the flagger is
  flag-only by contract (D14).
- **A blurred watch mode in `gawk-app`** — offered, declined (OD4).
- **Partial-nudity and gore classifiers** — offered, declined (OD3);
  the label set stays a knob.
- **A GPU image variant** — no GPU in the reference deployment (OD10).
  The image's ONNX Runtime provider is a build argument, so it can be added
  later without a design change.
- **Persisting flagged frames or thumbnails, even blurred** — D8.
- **Exempting dynamic or open rooms** — an unscreened lane for anyone (D11).
- **Report-to-authority tooling** — out of scope (OD8).

## 4. Where it plugs in

| Piece | Today | What R40 changes |
|---|---|---|
| Relay ops listener | `internal/ops/admin.go` `/internal/admin/*` under `-admin-api-token` | `GET /internal/screening/broadcasts/{id}/keyframe` under `-screening-token` (D1); `registryOptions`, flags, `GAWK_*`, chart `screening.token`/`screening.tokenSecretRef` |
| Relay hub | `cachedKeyframe`, `codec`, `avccConfig` (`internal/hub/hub.go`) | A read accessor returning the cached frame, session ID, frame ID and probed codec under the hub lock |
| Wire | `RelayIdentity` (0x11), trailing bytes tolerated | One appended notices byte, bit 0 = screening (D13); Go, `wire.ts` and `crates/wire` with byte-identical golden vectors |
| `gawk-server/screening` | — | New public package (D3) |
| `gawk-server/events` | `content_flag.raised` reserved | Two types, additive fields (D16) |
| `gawk-server/rooms` | `RoomSpec` | `Screening string` (`""`/`exempt`) with validation (D11) |
| `gawk-admin` API | `POST /api/v1/content-flags` returns 404 (`api.go`) | The route (role `flagger`), `GET /api/v1/screening/targets` (role `flagger`), operator routes `GET /api/v1/content-flags`, `POST /api/v1/content-flags/{id}/acknowledge` and `/dismiss`, `PATCH` of a room's screening field; OpenAPI and `x-gawk-requires: screening` |
| `gawk-admin` store | `moderation_events`, webhook dispatcher | Migration `content_flags`; leader job for reminders and the deadline (D9, D15) |
| `gawk-admin` SPA | Broadcasts view, Rooms view | Flagged broadcasts pinned on top with score, window, age, state and Acknowledge / Dismiss / Watch / Kill; the D12 banner; the room exemption toggle |
| `gawk-sampler` | — | New module, image, chart, release-please component, CI lane |
| Broadcaster surfaces | Server probe parses `RelayIdentity` | The disclosure line (D13) in `gawk-app`, the desktop engine/UI, and `gawk-ios` (a `gawk-ios`-scoped commit in the same PR) |
| `gawk-app` terms | `public/terms.html` | Screening paragraph |

## 5. `gawk-sampler` knobs

Flag > `GAWK_SAMPLER_*` env > default; all in the chart's values and the
start-up log (the R2 rule).

| Flag | Default | Meaning |
|---|---|---|
| `-admin-url` | *(required)* | `gawk-admin` base URL (in-cluster Service). |
| `-oidc-issuer` / `-oidc-client-id` | *(required)* | Client-credentials identity (D18); secret from `GAWK_SAMPLER_OIDC_CLIENT_SECRET`. |
| `-screening-token` | *(required)* | Bearer for the relay keyframe route (D1). |
| `-model` | `/models/nudenet.onnx` | Model path (D5). |
| `-model-input` | from CF0 | Square input size (320 or 640). |
| `-labels` | `FEMALE_GENITALIA_EXPOSED,MALE_GENITALIA_EXPOSED,ANUS_EXPOSED` | Flagged labels (D7). |
| `-threshold` / `-window` / `-window-hits` | from CF0 / `6` / `4` | Hysteresis (D7). |
| `-interval` | `5s` | Target per-broadcast sample interval (D6). |
| `-workers` | `GOMAXPROCS` | Concurrent decode+infer slots. |
| `-decode-timeout` | `2s` | Per-sample ffmpeg wall clock (D4). |
| `-targets-refresh` | `5s` | How often the target list is re-read. |
| `-sampler-id` | `$POD_NAME` | `samplerId` in flags. |
| `-metrics-addr` | `:9090` | Prometheus; `off` disables it. Never routed. |

`gawk-admin` gains `-content-flag-repage` (`10m`), `-content-flag-mute`
(`30m`), `-content-flag-auto-kill` (`off`), `-content-flag-deadline`
(`off`), and starts using `-flagger-role`. The relay gains
`-screening-token` and `-screening-notice`.

The sampler runs as **one replica**. Two would double-sample and race on
updates; a restart loses only window state, which refills in 30 s. Sharding
by broadcast key is the path if one pod stops keeping up, and is not built
now.

## 6. Chunks and acceptance criteria

| Chunk | Scope | Verified by |
|---|---|---|
| **CF0** | Calibration spike, before any production code: verify the NudeNet code and weights licences; benchmark 320n vs 640m CPU time per frame on the reference node class; run both over a corpus of the owner's game captures (≥ 30 titles, ≥ 2 h sampled at 5 s) and a public NSFW evaluation set; pick `-model-input`, `-threshold`, `-window`, `-window-hits` | A §8 appendix in this doc with: the licence findings (**gate**: if the weights licence forbids this use, stop and return to OD5); per-frame p50/p99 latency for both sizes; the game corpus's false-flag rate under the chosen window (target: **< 1 flag per 100 broadcast-hours**); recall on the explicit set; the chosen defaults written into D7 and §5. No frames from the evaluation set enter the repo. |
| **CF1** | Relay: keyframe route, token, notice knob, `RelayIdentity` notices byte in all three wire copies (D1, D13) | Hub test: the accessor returns the verbatim `StreamFrame`, and a new publisher session changes the ETag. Ops tests: 404 with no token configured, 401 on a wrong token, 404 for an unknown ID, 200 with `X-Gawk-Codec` for web AVCC, desktop Annex-B and VP8/VP9 fixtures, 304 on a matching `If-None-Match`; the admin token is **not** accepted on the route, nor the screening token on `/internal/admin/*`. Wire golden vectors byte-identical across Go, TS and Rust; an old-shape `RelayIdentity` still parses. Knobs reach `registryOptions`, env and chart (`helm template` assertion). |
| **CF2** | Shared types and events: `gawk-server/screening`, the two event types, the additive fields, `RoomSpec.Screening` (D3, D11, D16) | Package tests round-trip the §4.11 body. Events: schemas, golden vectors and AsyncAPI updated; the existing schema/vector drift tests green; no publisher-IP field anywhere (an existing prohibition test extended to the new types). Rooms validation rejects `exempt` on dynamic or secret-less static rooms. |
| **CF3** | `gawk-admin` API and store: migration, `POST /content-flags` (raise/update/muted), targets with exemption and mutes, `503 no_flag_route`, operator list/acknowledge/dismiss, room screening `PATCH` (D2, D9–D12, D17) | DB tests: first POST 201 + one event + one row; repeat POST 200 and no new event; POST during mute 202; mute expiry then POST raises a new flag. A `flagger` token reaches exactly the two flagger routes and is 403 everywhere else (the R49 loop); operator routes refuse `flagger`. Targets: origin rows only, exempt broadcasts marked, `503` when no enabled webhook accepts `content_flag.raised` (including a filter that excludes it). `TestOpenAPIMatchesRoutes` and `redocly lint` green. Migration passes the forward-only/expand–contract CI check. |
| **CF4** | `gawk-admin` lifecycle jobs and portal UI: reminders, deadline kill, auto-kill tier, session-end resolution, pinned flags, actions, banner, exemption toggle (D9, D14, D15) | Leader-job tests with a fake clock: reminders every repage interval only while unacknowledged and fresh, none after the signal goes stale; deadline kill off by default, on at `30m` kills an unacknowledged fresh flag and not an acknowledged, muted or stale one; auto-kill `off` never kills, and a matching policy kills once with actor `system:content-flag`; session end resolves `ended`. SPA tests for the pinned row, the actions and the banner. |
| **CF5** | `gawk-sampler`: target polling, fetch with ETag, bitstream conversion, ffmpeg subprocess, ONNX inference and YOLOv8 post-processing, hysteresis, POSTs, metrics, client-credentials (D4, D6–D8, D18) | Conversion tests: AVCC → Annex-B and IVF wrapping decode to the expected frame with the real ffmpeg. Post-processing matches NudeNet's Python reference on ≥ 10 fixture images to within 1e-3 per box score (reference outputs committed, images synthetic or licence-clean). Hysteresis table tests (trip, hold, clear, session reset). A truncated or garbage bitstream fails that one sample and the next sample of every other broadcast still succeeds; a decoder that hangs is killed at `-decode-timeout`. Scheduler test: 3× overload stretches every broadcast's interval and leaves none unsampled. A static check that no code path writes frame bytes to disk or logs, and a test that the subprocess runs with `RLIMIT_CORE` 0. |
| **CF6** | Packaging: Dockerfile (minimal ffmpeg stage, ONNX Runtime), chart (Deployment, init-container model fetch with SHA-256, `PrometheusRule`, optional NetworkPolicy limiting egress to the relay ops port and `gawk-admin`), release-please component, CI lane, image built from the repo root (D5, D6) | `helm template` assertions for every knob, the init container failing on a SHA mismatch, and the rule. Image smoke run: `ffmpeg -decoders` lists exactly h264/vp8/vp9; the sampler starts, loads a test model and reports ready. Release-please dry run shows the new component. |
| **CF7** | Disclosure: terms paragraph, the broadcaster line in `gawk-app`, desktop and `gawk-ios` (D13) | Each surface shows the line for a relay advertising screening and not otherwise (unit tests on the probe result → UI state). `gawk-ios` change in its own scoped commit. |
| **CF8** | End-to-end tier, docs, manual pass | A compose/kind tier with a **test model** that scores a synthetic pattern (solid magenta frames) as the flagged label, so CI never needs NSFW content: `gawk-pubsim` publishes magenta → a flag is raised → the webhook receiver gets `content_flag.raised` → dismiss → no flag during the mute → a regular pattern clears the window. Docs: self-hosting (enabling the sampler, the Keycloak `flagger` client recipe, the screening notice, the "no pager, no sampling" rule, OD8's out-of-scope note), `docs/gotchas.md`, `ROADMAP.md`, `CLAUDE.md`'s module list. Manual pass on the reference deployment: a real broadcast of game footage runs for an hour with no flag, and the test-pattern broadcast pages a phone end to end. |

## 7. Open after this doc

- CF0's numbers (D7 defaults, model size, licence verdict).
- Whether one sampler replica keeps up at the fleet's real broadcast count
  — CF0's latency figure × the live count says; sharding (§5) is the
  answer if not.

## 8. Calibration (filled in by CF0)

*Empty until CF0 runs.*
