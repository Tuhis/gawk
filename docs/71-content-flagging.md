# R40 — Automatic content flagging (docs/71)

**Status**: designed 2026-10-12 (owner decisions OD1–OD14); **CF0–CF8
implemented 2026-10-13** (§8.1 records CF0, §9 the implementation
contract); CF9, the calibration run on the reference deployment, is open.
Chunks **CF0–CF9** (`CF` = Content Flagging, reserved for R40 in the
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
| OD5 | Which model? | **NudeNet's detector** (YOLOv8 body-part detector, ONNX). Its per-label output is what makes OD3 expressible. Resolution (320n vs 640m) and default thresholds are set by CF9's calibration on the real deployment (OD12). |
| OD6 | Where does the sampler live? | **A fifth top-level module, `gawk-sampler`**, with its own image, chart and release-please component — the `gawk-telemetry`/`gawk-admin` shape. |
| OD7 | Which broadcasts are screened? | **All of them, with an operator-managed exempt list** of static rooms (D11). |
| OD8 | Reporting to authorities? | **Out of scope.** R40 stops at flag, kill and ban; reporting is the operator's own business. This doc and the self-hosting guide say so; nothing in the product files or prepares a report. |
| OD9 | How do broadcasters learn they are screened? | **A paragraph in the default R23 terms, plus a relay-advertised notice** the broadcaster apps show as a "Content screening" mark among the server's details (D13; first built as a line on the broadcast screen, moved 2026-10-13, §9.8). |
| OD10 | Compute and cadence? | **CPU only; one sample per broadcast every 5 s.** Overload stretches the interval and raises a metric, never silently drops broadcasts (D6). |
| OD11 | What does dismissing a flag do? | **Mutes re-flagging of that broadcast for a fixed time** (default 30 min), after which a persisting signal flags again. |
| OD12 | When is the model calibrated? | **At the end, on real gawk** (revised 2026-10-12; the first draft opened with an offline calibration spike). The sampler ships with an `observe` mode that scores every broadcast and counts what it *would* have flagged; CF9 runs it on the reference deployment and sets the defaults from real broadcasts. Only the licence check stays up front (CF0). |
| OD13 | Where does observe-mode data live? | **In `gawk-admin`'s Postgres**, after weighing `gawk-telemetry` (§3.1): sampled scores in tables of their own, observed flags beside real ones, and a replay endpoint the portal and CF9 use. Prometheus keeps only operational metrics. |
| OD14 | Does an observed flag notify? | **Yes, as a low-priority event**, `content_flag.observed`, so the operator can join live while the trigger is on screen. It is routed (or filtered out) per webhook with R49's `events` filter. |

## 3. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **The sampler reads keyframes from a new relay ops route, `GET /internal/screening/broadcasts/{id}/keyframe`**, authorized by its own bearer, `-screening-token` (`GAWK_SCREENING_TOKEN`), distinct from `-admin-api-token`. Empty token ⇒ the route is not registered (404). It returns the hub's cached keyframe — the `StreamFrame` bytes **verbatim** (24-byte header, embedded `DecoderConfig`, payload) — with `X-Gawk-Codec` (the hub's probed family) and an `ETag` of publisher session + frame ID; `If-None-Match` gets `304`. `404` when the pod holds no keyframe for that ID. | The keyframe is already cached per broadcast (`internal/hub/hub.go`, `cachedKeyframe`) and refreshed every ~500 ms (the broadcasters' GOP), so one GET is a fresh, self-contained frame. The alternative tap, `/internal/subscribe/{id}`, delivers the *whole stream*: hundreds of broadcasts × several Mbps into one sampler to look at one frame every 5 s, and it lives under `internal/` where a fifth module cannot import it. A separate token is least privilege: the admin token also reads publisher IPs and is held by the portal. `304` makes an unchanged frame cost nothing (a static screen, a paused game); an away broadcaster is not sampled at all (D2). |
| D2 | **The sampler never discovers broadcasts itself; `gawk-admin` serves it a target list**: `GET /api/v1/screening/targets` (role `flagger`, filtered to the polling pod's shard — D22), one entry per live *origin* broadcast **whose publisher is active** (`publisherActive` in the relay's admin rows) with its raw ID, HMAC key, the ops address of the pod holding it, and `muted`/`exempt` booleans. Exempt broadcasts are listed (so the sampler's metrics see them) but marked skip. A broadcast in its away grace period is not listed: its cached keyframe is a frozen frame no one is broadcasting, and re-scoring it would re-raise a flag the session's end just resolved. When a broadcast leaves its shard's list the sampler drops its window and `recent` and POSTs nothing more for it; as a backstop, `gawk-admin` answers a tripped POST for a broadcast whose publisher is not active with `202 ended` and raises nothing. (Added 2026-10-13 in PR review.) | `gawk-admin` already scrapes every relay pod (`internal/relayscan`) and is the one place that knows rooms, exemptions and mutes. A sampler holding the relay admin token to scan pods itself would hold publisher IPs it never needs. One list also means one answer to "is this broadcast screened?". |
| D3 | **Shared types live in a new public package, `gawk-server/screening`**: the target-list response, the `gawk.content-flag.v1` request body (docs/42 §4.11 plus the additive `mode`, `state` and `recent` fields of D9, D14 and D19, noted in docs/42 §4.11 too), the sample-batch and replay types, the keyframe route path and headers. `gawk-admin` and `gawk-sampler` both import it; a semantic change needs commits scoped to both in the same PR. | The repo rule: shared Go is a public package in `gawk-server`, never mirrored (`wire`, `events`, `adminapi`, `oidcauth` precedent). |
| D4 | **The sampler is Go; decode runs in an `ffmpeg` subprocess per sample, inference in-process through ONNX Runtime** (`onnxruntime_go`, the shared library in the image). The sampler converts the frame to a bitstream ffmpeg can read from a pipe — AVCC → Annex-B with SPS/PPS from the `avcC` extradata, Annex-B as is, VP8/VP9 wrapped in IVF — and reads back one RGB frame scaled to the model's input. Each subprocess runs with a wall-clock timeout, `RLIMIT_AS`, and **`RLIMIT_CORE` = 0**. | Crash isolation was the open design question: a malformed bitstream must kill at most one sample, never the sampler. A cgo libavcodec would put a decoder CVE into the sampler's address space. A fork per sample every 5 s × 100 broadcasts is 20 forks/s, which is cheap. ONNX Runtime sees only a fixed-shape float tensor the sampler built, so in-process is safe. A core dump would write a frame to disk, which the evidence rule (D8) forbids. |
| D5 | **The image ships a minimal ffmpeg** built in a Dockerfile stage with `--disable-everything` and only the `h264`, `vp8` and `vp9` decoders, the `h264`/`ivf` demuxers, the `pipe` protocol, the `rawvideo` encoder and muxer, and the `scale`, `format`, `buffer` and `buffersink` filters (`swscale` alone does not enable the CLI's `scale` filter) — everything the D4 pipe from bitstream to one scaled RGB frame needs, and nothing else. (Completed 2026-10-13 during the build: the pipe also needs `pad` (§9.5), the `h264`, `vp8` and `vp9` parsers and the `vp9_superframe_split` bitstream filter, and the ffmpeg CLI links `transpose`, `trim` and `vflip` itself; `gawk-sampler/deploy/build-ffmpeg.sh` is the list.) **Model weights are not in the image**: the chart fetches them in an init container from a pinned URL and verifies a pinned SHA-256 into an `emptyDir`, or mounts an operator-provided volume. (The pinned URL is GitHub's API asset route, fetched with `Accept: application/octet-stream`: NudeNet's release-download URLs redirect anonymous clients to a sign-in page, as of 2026-10-13.) | Smallest decoder CVE surface for the three codecs any broadcaster sends (web: H.264/VP9/VP8; desktop and iOS: H.264 Annex-B; nobody sends AV1 or HEVC). Keeping weights out of the GHCR image means gawk never redistributes them, whatever NudeNet's weights licence turns out to be (CF0 verifies it); the SHA pin keeps a swapped file from loading. |
| D6 | **Scheduling**: one sample per screened broadcast every `-interval` (5 s, OD10), spread across the interval rather than in bursts, through a worker pool of `-workers`. When the pool cannot keep up, each broadcast's effective interval stretches evenly, `gawk_sampler_interval_effective_seconds` rises, and a chart-provided `PrometheusRule` fires above 2× the target. Every listed broadcast stays in the rotation. | A sampler that silently skips some broadcasts under load is the "silent backlog" the DSA posture forbids; stretching keeps coverage complete and makes the shortfall visible. |
| D7 | **Scoring**: a sample's score is the maximum confidence among the **flagged labels** — default `FEMALE_GENITALIA_EXPOSED,MALE_GENITALIA_EXPOSED,ANUS_EXPOSED` (OD3; `-labels`). A broadcast's window is its last `-window` samples (default 6 ≈ 30 s); it **trips** when at least `-window-hits` samples (default 4) score ≥ `-threshold`, and **clears** only when the whole window is below it. Defaults for `-threshold`, `-window` and `-window-hits` are placeholders (a conservative `0.8` / `6` / `4`) until CF9's calibration replaces them in this table and in §5. A new publisher session resets the window. | Never decide on a single frame: games produce skin-toned false positives constantly. Hysteresis keeps a flickering score from raising and clearing flags every few seconds. The label set is configurable because "explicit only" is a policy, not a constant. |
| D8 | **Evidence is metadata only.** Frames exist only in the sampler's memory between fetch and score; they are never written to disk, logged, sent in a flag, or exposed on a debug endpoint. The flag carries scores, the window, the labels that fired and their counts. | If the flag is right, keeping the material is itself the legal hazard. The operator inspects by joining live (OD4). |
| D9 | **Flag lifecycle (in `gawk-admin`)**. (Revised by R70, [docs/72](72-moderation-timeline.md) D3–D4: a clear of an *unacknowledged* flag is held for `-content-flag-rejoin` and a re-trip inside it is the same incident, and a raise after a reviewed incident is marked `returnedAfterReview`.) While its window is tripped, the sampler POSTs `/api/v1/content-flags` with `"state": "tripped"` after every sample; when the window **clears** (D7) it POSTs once with `"state": "cleared"`. A flag is **unresolved** while `open` or `acknowledged`. The first tripped POST for a broadcast with no unresolved flag **raises** one: a `content_flags` row, a `content_flag.raised` event (the webhook *is* the page), and the broadcast pinned in the portal (`201`). Further tripped POSTs **update** the unresolved flag (`200`). States: `open` → `acknowledged` → `resolved` (`cleared`, `dismissed`, `killed`, `auto_killed`, `ended`). A cleared POST resolves the unresolved flag as `cleared`, whether or not it was acknowledged, so a later episode in the same session raises a **new** flag and pages again. The signal is **live** after a tripped POST until a cleared POST, and becomes **unknown** when no POST has arrived for `-content-flag-stale-after` (default `2m`, a `gawk-admin` knob). An `open` flag whose signal is live or unknown emits `content_flag.reminded` every `-content-flag-repage` (10 m). Acknowledging stops reminders and deadlines; it does not resolve. A kill through R39 resolves as `killed`; the publisher session ending resolves as `ended`. (Revised 2026-10-13 in PR review: the first draft judged freshness by "3 × interval", an interval `gawk-admin` never learns and that D6 stretches under load, and gave an acknowledged flag no way to close.) | "A flag must reach a human" made mechanical. Updates instead of new flags keep one row per incident. Acknowledge is a separate step so the operator can say "I'm looking" from a phone before deciding. An explicit clear separates "the content went away" from "the sampler went quiet": reminders stop for the first, never for the second, because a dead or overloaded sampler is not evidence of a clean broadcast. Stale-after is long (24 samples at 5 s) so D6's stretching never makes a live signal look unknown; it only bounds how long a silent sampler can hold a signal live. Resolving an acknowledged flag on clear is what makes a second episode page. |
| D10 | **Dismiss mutes the broadcast for `-content-flag-mute` (30 m, OD11)** (revised by R70, [docs/72](72-moderation-timeline.md) D5: a dismissal now carries a reason and a scope — `episode`, `duration` (this mute) or `session`): POSTs during the mute return `202 muted` and change nothing; the target list marks it `muted`. When the mute expires, a still-tripped window raises a **new** flag. Auto-kill and deadline kill never act on a muted broadcast. | OD11. A fixed time still catches a broadcaster who switches content after the operator has looked. `202` lets the sampler count mutes in its metrics without treating them as errors. |
| D11 | **Exemption is a field on the `Room` CR, `spec.screening: exempt`**, set from the portal (operator only, audited as `room.screening_changed`). Only a **static room with an attach secret** can be exempt; the portal refuses others. A broadcast is exempt while attached to an exempt room. Broadcasts outside any room are always screened. | OD7. The `Room` CR is the room's only durable record (docs/62 rejected a Postgres side-list for the same reason). An open or dynamic room could be joined by anyone, so exempting it would hand an abuser an unscreened lane; the attach secret is what makes "my friends' room" a trust boundary. |
| D12 | **No flagging without a pager.** The target list carries `flagRoute: false` when no enabled webhook would deliver `content_flag.raised` (config-sourced or UI-created, honouring R49's per-webhook `events` filter), and `POST /api/v1/content-flags` then answers `503 no_flag_route`. A sampler in `flag` mode samples nothing while `flagRoute` is false, logs once per minute, and exports `gawk_sampler_paused{reason="no_flag_route"} 1`; the portal shows a banner. `observe` mode (D19) is unaffected — it raises nothing. | The DSA point in §1: detecting without anyone to tell is worse than not detecting. Pausing at the source is the only place this can't be bypassed by a misconfigured webhook list. |
| D13 | **Disclosure (OD9)**: the relay gains `-screening-notice` (bool, default false; chart `screening.notice`), advertised as an **appended field of `RelayIdentity`** (docs/40 §4.4 — trailing bytes are that message's extension mechanism; the flags byte stays strict). **When the knob is on, the relay also sends that `RelayIdentity` on every publish session**, on its own server-opened uni stream at session start (the `TelemetryEndpoint` shape); with the knob off, publish sessions stay byte-identical. The broadcaster apps show it as a **"Content screening" mark, with a check, among the server's details**, only when the server says so, and never on the broadcast screen (§9.8): `gawk-app` on each row of the server picker, from the picker's probe; the desktop on the selected server's row in Settings and iOS on each server in Settings and on its page, from their probes and, for the server a broadcast is up on, that broadcast's publish session's identity. Its tooltip, or on iOS its VoiceOver value, is "This server automatically screens streams for explicit content." (iOS: a value, not a hint, corrected 2026-10-15 — a hint marks the element actionable, and the accessibility audit failed the mark's hit area on the server's page.) The default terms (`gawk-app/src/features/terms/BundledTerms.tsx` §6; `public/terms.html` is only the operator-override slot) gain a screening paragraph, and the bundled terms version moves so broadcasters are asked to accept them again; operators with custom terms write their own. (Path corrected 2026-10-13 during the build.) (Revised 2026-10-13 in PR review: the first draft relied on the `/echo` probe alone, but `gawk-app` probes only inside the server picker — docs/40 SP6, "no probe traffic without user action" — so a web broadcaster on the default relay would never have seen the notice.) | `RelayIdentity` was built as the managed-mode extension point, so no new wire type. The publish session is the one channel every broadcaster opens, and it opens exactly when the notice matters; amending SP6 to probe on mount was the alternative, but it would weaken a no-unrequested-traffic rule for a notice the publish session can carry for free. An old client that receives the stream drops it: `gawk-app` logs an unexpected uni-stream type and continues (`transport/broadcaster.ts`), and CF1 asserts the same for the Rust engine. (Since §9.8 `gawk-app` reads the stream and drops it quietly: its mark comes from the picker's probe.) A separate knob (rather than "a sampler exists") because the relay cannot see the sampler, and an operator who screens must be able to say so — the chart docs pair the two. The mark is a notice, never a gate (docs/29 D8). |
| D14 | **Auto-kill tier (OD1), default off**: `gawk-admin -content-flag-auto-kill` (`off` or a policy `score>=0.9,hits>=10/12`). Every tripped POST carries **`recent`**: the publisher session's last `-report-samples` (default 12, a sampler knob) per-sample scores, oldest first, each the max over the flagged labels — including scores below the floor. `gawk-admin` evaluates the policy over `recent` alone, on each tripped POST, never by the sampler: it needs at least the policy's sample count (`10/12` needs 12) and otherwise does not kill. `recent` starts empty on a new publisher session and on a sampler restart (the window is in memory), so a restart can only delay a kill, never cause one. The policy's score may be any value in 0–1; the floor does not apply, because `recent` is not filtered by it. The body caps `recent` at 64 entries, and `gawk-admin` refuses to start if the policy names more samples than that. On a match it kills through R39's kill path with a cooldown ban of `-kill-cooldown`, actor `system:content-flag`, and resolves the flag `auto_killed` — the page still goes out. (Data source added 2026-10-13 in PR review.) | The sampler holds `flagger`, which grants *flag-only* rights (docs/42 D17): it must never be able to kill. Putting every automated action in `gawk-admin` keeps one actuator and one audit trail. A kill needs exact per-sample history: the window summary cannot express "10 of the last 12 at ≥ 0.9", and the D20 sample tables err toward *more* trips by design, which is acceptable for calibration and wrong for a kill, and arrive in batches unsynchronised with flag POSTs. Carrying the scores in the POST makes the decision self-contained and its input auditable in the flag row. |
| D15 | **Deadline kill (OD2), default off**: `gawk-admin -content-flag-deadline` (`off` or a duration, e.g. `30m`). An `open` (unacknowledged), unmuted flag whose signal is **live** (D9) at the deadline is killed exactly as in D14 and resolved `auto_killed` with `reason: "deadline"`. A signal that is unknown at the deadline is not killed; the flag keeps re-paging. | OD2: re-paging is the default; the kill is a switch for an operator who would rather lose a stream than leave one up overnight. A cleared flag is already resolved, and an unknown signal is not evidence enough to kill on. |
| D16 | **Events**: `gawk-server/events` gains four types, and `ContentFlagRaisedData` gains the additive fields the §4.11 body already carries (`samplerId`, `window`, `scores`, `labels`) plus `flagId`. **Moderation rows** — in `store.AllEventTypes()`, held equal to `events.ModerationRowTypes()`, delivered to every unfiltered webhook like `raised`: `content_flag.reminded`, `content_flag.resolved` (with `resolution`), and `room.screening_changed` (D11's audit, the `room.secret_rotated` shape). **Opt-in**: `content_flag.observed` (reusing the raised data type) is *not* a moderation row — it is calibration data, stored with category `screening` and pruned by D17 — so it is in `WebhookEventTypes()` and `FeedEventTypes()` but not `AllEventTypes()`, and a webhook receives it **only if its R49 `events` filter names it**, the class R49 created for room activity. Schemas, golden vectors, AsyncAPI, the webhook channel and the OpenAPI `EventType` enum all follow. No event ever carries a publisher IP (R39 prohibition). (Revised 2026-10-13 in PR review: the first draft left `room.screening_changed` unregistered and made `observed` reach every unfiltered webhook, so a fresh install in `observe` mode would page its existing pager.) | The reserved `content_flag.raised` today has only `actor`, `broadcastKey`, `broadcastId` and `reason` — not enough to page usefully. Additive fields keep existing consumers valid. OD14's "low priority" needs a mechanism: opt-in delivery is it, and it keeps D19's promise that nothing pages before the operator switches to `flag`. |
| D17 | **Retention**: `content_flags` rows in `flag` mode are part of the moderation audit trail and are never pruned by `-activity-retention`. Observed flags, their `content_flag.observed` rows (category `screening`, D16) and the D20 sample tables are calibration data, pruned after `-screening-sample-retention` (30 d) by the leader's hourly pruning tick. | Real flags are scores and timestamps — the record of having acted expeditiously is the point. Calibration data has no value after the run that used it. |
| D18 | **The sampler authenticates with the OIDC client-credentials grant** (`-oidc-issuer`, `-oidc-client-id`, client secret from a Secret), caching the token until shortly before expiry. Its client carries only `flagger`. | docs/42 §4.11 as frozen; the same recipe as R49/R60's bots. |
| D19 | **`-mode`: `observe` or `flag`; the chart defaults to `observe`.** In `observe` the sampler runs the whole pipeline — fetch, decode, score, hysteresis — and POSTs exactly as D9 describes, with `"mode": "observe"`. **`mode` is required on every POST**: `observe` or `flag`; absent or any other value is `400` — it decides between paging and silence, so no default is safe. `gawk-admin` stores an observe POST as an **observed flag**: `content_flags.mode = 'observe'`, one `content_flag.observed` event on the raise (OD14, opt-in delivery per D16), never pinned, never reminded, never killed by D14/D15, no acknowledge or dismiss; it resolves `cleared` on the cleared POST, `cleared` with `reason: "stale"` after `-content-flag-stale-after` without a POST, or `ended` with the session. Observed POSTs are exempt from D12 (they page no one) and from mutes. A `gawk-sampler score <dir>` subcommand runs the same decode-free scoring path over a local image directory and prints per-label recall at candidate thresholds, for an offline recall check that never touches the repo or the cluster. | OD12. Calibrating on the real deployment measures what matters — false trips on the games people actually stream, and latency on the actual nodes — which no offline corpus reproduces. Defaulting to `observe` means every new install, not just ours, sees its own false-trip rate before anything pages. Keeping observed flags in the same table, with the raw ID, is what makes them watchable live. Recall cannot be measured on live traffic (there should be nothing to find), hence the offline subcommand. |
| D20 | **Score samples are stored in `gawk-admin`, above a floor, in both modes.** The sampler batches its samples every 5 s to `POST /api/v1/screening/samples` (role `flagger`). Table `screening_samples`: broadcast key and raw ID, publisher session, a per-session sequence number, time, and the score of every label in `-labels`; **only samples where some flagged label scores ≥ `-sample-floor` (0.3) are stored**. Table `screening_sessions`: one row per publisher session with its running count of samples taken and first and last sample time. **`gawk-admin` assigns the sequence numbers, never the sampler**: each batch reports, per publisher session, how many samples it took and each stored sample's position among them (1-based); in one transaction `gawk-admin` locks the session row, numbers each stored sample as the count before the batch plus its position, and adds the batch's count. **A sample is a scored frame**: only a fetch that ends in a score is counted. A fetch that fails — `404` (the origin moved or the broadcast ended between target refreshes), a network error, a decode failure or timeout — is not counted, does not enter the window, and shows only in the sampler's metrics. A `304` is a sample: the frame is unchanged, so it repeats the previous score. Both tables are separate from `moderation_events` and pruned by D17. (Numbering and the sample definition revised 2026-10-13 in PR review: the first draft numbered in the sampler, which restarted at 1 on every sampler restart or re-home and collided with stored rows.) | A score histogram (the first draft's plan) cannot replay a window rule: it loses which broadcast a score came from and in what order. The sequence number makes the gaps explicit — and because failed fetches are not counted, a gap always means "scored below the floor", never "not sampled", so a burst of `404`s around an origin move cannot read as clean samples. Numbering in `gawk-admin` makes the count survive sampler restarts and re-homing with no run concept: a new sampler just adds to the same count. What remains inexact errs toward more would-trips, the safe direction for calibration: a crashed sampler's unsent batch (≤ 5 s) is lost, and replay treats a restart as continuous where the live sampler began a fresh window. Two samplers on one session at once would double its density; that is prevented by assignment (each broadcast has exactly one sampler), not by numbering. A sample below the floor counts as below every threshold ≥ the floor, so replay stays exact while the table holds only the interesting few percent. Session rows give the broadcast-hours denominator. Storing in `flag` mode too lets a later recalibration use the same data. |
| D21 | **Replay**: `POST /api/v1/screening/replay` (role `operator`) takes `threshold`, `window`, `windowHits`, optional `labels` and a time range, and replays D7 over the D20 tables. It returns broadcast-hours, the would-trip count per 100 broadcast-hours, and each would-trip (broadcast key and ID, session, time, peak score). The portal's **Calibration** tab is a form over it, with the observed flags listed beside it. A `threshold` below the floor is refused (`400`). | CF9's question — "what would *this* setting have done?" — answered from stored data for any candidate, not only the one the sampler ran with. An endpoint rather than a script, so it uses the portal's auth and the same code serves CF9 and any later operator. |
| D22 | **Horizontal scaling: a StatefulSet with a fixed pod count, sharded by broadcast key; default `replicas: 2`** (owner decision 2026-10-13). Each pod takes its shard index from its stable name and the count from the chart (`-shard-index`, `-shard-count`) and polls `GET /api/v1/screening/targets?shard=<i>&of=<n>`; `gawk-admin` returns only the broadcasts whose `screening.Shard(broadcastKey, n)` equals `i` — the function lives in `gawk-server/screening` (D3), so the split is computed in one place. No pod talks to another and nothing is elected; a broadcast's key never changes, so it stays on one pod while live, which is the "exactly one sampler per session" D20 relies on. **Missing shards are loud**: `gawk-admin` records each poll's `(i, n)`; taking the `n` of the most recent poll as expected, any index of `0…n-1` that has not polled for 30 s is missing — exported as `gawk_admin_screening_shards_missing`, shown as a portal banner beside D12's, and alerted by the chart's `PrometheusRule` after `for: 2m` (which also rides out a resize). The chart sets `podManagementPolicy: Parallel`, a PodDisruptionBudget of `maxUnavailable: 1`, and a preferred pod anti-affinity across nodes, so one node loss costs at most one shard. **Known limits, accepted**: (a) a StatefulSet never runs two pods of one name, so a pod on an unreachable node is **not replaced until someone acts**: after the 300 s `unreachable` toleration the pod is only gracefully deleted, sits in Terminating/Unknown because the kubelet cannot confirm it, and the controller will not recreate the name while that object exists. The shard returns when the kubelet comes back, the Node object is deleted, or an operator runs `kubectl delete pod gawk-sampler-<n> --grace-period=0 --force` or applies the `node.kubernetes.io/out-of-service` taint; a plain delete does nothing. The share is unscreened until then. The missing-shard alert makes it visible within 2 minutes and its runbook text (in the `PrometheusRule` annotation and the self-hosting guide) names exactly those two recovery steps. (Corrected 2026-10-13 in PR review: the first draft said the gap ended by itself after ~5 min.); (b) changing `replicas` is a rollout that reshuffles most broadcasts and resets their windows, and while old and new pods overlap some broadcasts are sampled twice or not at all — a deliberate operator change, never autoscaled. | Simplest scaling with no coordination: the only new `gawk-admin` logic is a filter and a staleness check. Two replicas by default so a single pod restart or rollout never leaves *every* broadcast unscreened, and so the sharded path is the one every install runs rather than a rarely-exercised option. The DSA posture (§1) forbids silent gaps, not all gaps — (a) is surfaced within 2 minutes. |

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
- **`gawk-admin` assigning shards to live samplers** (rendezvous hashing
  over registered pods) — it would move a dead node's share to the
  surviving pods within one poll, which D22's StatefulSet cannot, but adds
  a registration table and assignment logic; declined for simplicity
  (2026-10-13). The cost accepted with it: after a node loss a shard stays dark until the
  operator force-deletes its pod (D22 (a)), bounded only by how fast the
  missing-shard alert is answered. Revisit if that ever happens in
  practice.
- **A single-broadcast test mode in the sampler** for paging a phone on
  the reference deployment — every link it proved is shown without it
  (CF9).
- **Autoscaling the sampler** — every resize reshuffles and resets windows
  (D22 (b)).
- **Observe-mode data in Prometheus** — histograms lose order and origin,
  so they cannot replay a window rule (D20).
- **Observe-mode data in `gawk-telemetry`** — weighed in §3.1 and
  declined (OD13).
- **Exempting dynamic or open rooms** — an unscreened lane for anyone (D11).
- **Report-to-authority tooling** — out of scope (OD8).

### 3.1 Why observe data lives in `gawk-admin`, not `gawk-telemetry`

| | `gawk-admin` | `gawk-telemetry` |
|---|---|---|
| Store | Postgres (CNPG, HA) | NDJSON on one PVC, one replica, no HA |
| Window replay | Postgres window functions | DuckDB over NDJSON — the better time-series engine |
| Retention | New knob, leader-pruned (the activity pattern) | 30 days already |
| Raw broadcast ID | Holds it today | **Forbidden** by its zero-PII rule (docs/33 D8) |
| Notification | Webhooks with per-webhook filters | **None** — alerting is a non-goal |
| Reader | OIDC `operator`, beside flags, kills and bans | A diagnostics surface (basic auth on the reference deployment) |
| Sampler ingest | The `flagger` client it already has | New server-to-server auth; ingest today is public and gated by relay-minted per-session tokens |
| R40 footprint | Already required | Makes a third optional deployable required |

Telemetry is the better time-series store, but OD14 decides it: an observed
flag is only worth a notification if the operator can click Watch, which
needs the raw ID telemetry may not hold, and telemetry cannot notify.
Splitting samples into telemetry and flags into admin would put one
calibration in two stores. The cost of `gawk-admin` is write volume it
otherwise refuses (`internal/store/activity.go`: "a write per five seconds
per live thing" is not an audit trail); the floor and the separate tables
(D20) are the answer. Estimate, to be measured in CF9: if 5 % of game
samples clear the floor, 100 concurrent broadcasts write ~1 row/s, ~2.6 M
rows (~0.5–1 GB with indexes) over 30 days. The reference deployment's 2 Gi
CNPG volume needs watching, so the table size is exported as a metric and
the self-hosting guide says so.

## 4. Where it plugs in

| Piece | Today | What R40 changes |
|---|---|---|
| Relay ops listener | `internal/ops/admin.go` `/internal/admin/*` under `-admin-api-token` | `GET /internal/screening/broadcasts/{id}/keyframe` under `-screening-token` (D1); `registryOptions`, flags, `GAWK_*`, chart `screening.token`/`screening.tokenSecretRef` |
| Relay hub | `cachedKeyframe`, `codec`, `avccConfig` (`internal/hub/hub.go`) | A read accessor returning the cached frame, session ID, frame ID and probed codec under the hub lock |
| Wire | `RelayIdentity` (0x11), trailing bytes tolerated | One appended notices byte, bit 0 = screening (D13); Go, `wire.ts` and `crates/wire` with byte-identical golden vectors |
| `gawk-server/screening` | — | New public package (D3) |
| `gawk-server/events` | `content_flag.raised` reserved | Four types, additive fields (D16) |
| `gawk-server/rooms` | `RoomSpec` | `Screening string` (`""`/`exempt`) with validation (D11) |
| `gawk-admin` API | `POST /api/v1/content-flags` returns 404 (`api.go`) | The route (role `flagger`), `GET /api/v1/screening/targets` and `POST /api/v1/screening/samples` (role `flagger`), `POST /api/v1/screening/replay` (operator), operator routes `GET /api/v1/content-flags`, `POST /api/v1/content-flags/{id}/acknowledge` and `/dismiss`, `PATCH` of a room's screening field; OpenAPI and `x-gawk-requires: screening` |
| `gawk-admin` store | `moderation_events`, webhook dispatcher | Migration `content_flags` (with `mode`), `screening_samples`, `screening_sessions` (D20); sample pruning; leader job for reminders and the deadline (D9, D15) |
| `gawk-admin` SPA | Broadcasts view, Rooms view | Flagged broadcasts pinned on top with score, window, age, state and Acknowledge / Dismiss / Watch / Kill; the D12 banner; the room exemption toggle |
| `gawk-sampler` | — | New module, image, chart, release-please component, CI lane |
| Broadcaster surfaces | `RelayIdentity` parsed only from `/echo` probes; publish sessions dispatch server uni streams by type | Read `RelayIdentity` on the publish session too, and show the disclosure line (D13) in `gawk-app`, the desktop engine/UI, and `gawk-ios` (a `gawk-ios`-scoped commit in the same PR) |
| `gawk-app` terms | `src/features/terms/BundledTerms.tsx` (the bundled default; `public/terms.html` is the operator override) | Screening paragraph in §6; `BUNDLED_TERMS_VERSION` moves |

## 5. `gawk-sampler` knobs

Flag > `GAWK_SAMPLER_*` env > default; all in the chart's values and the
start-up log (the R2 rule).

| Flag | Default | Meaning |
|---|---|---|
| `-mode` | `observe` (chart) | `observe` or `flag` (D19). |
| `-admin-url` | *(required)* | `gawk-admin` base URL (in-cluster Service). |
| `-oidc-issuer` / `-oidc-client-id` | *(required)* | Client-credentials identity (D18); secret from `GAWK_SAMPLER_OIDC_CLIENT_SECRET`. |
| `-screening-token` | *(required)* | Bearer for the relay keyframe route (D1). |
| `-oidc-scope` | *(empty)* | Optional `scope` on the client-credentials request. The secret is sent in the form body (`client_secret_post`). |
| `-model` | `/models/nudenet.onnx` | Model path (D5). |
| `-model-input` | `320` until CF9 | Square input size (320 or 640). |
| `-labels` | `FEMALE_GENITALIA_EXPOSED,MALE_GENITALIA_EXPOSED,ANUS_EXPOSED` | Flagged labels (D7). |
| `-threshold` / `-window` / `-window-hits` | `0.8` / `6` / `4` until CF9 | Hysteresis (D7). |
| `-interval` | `5s` | Target per-broadcast sample interval (D6). |
| `-workers` | `GOMAXPROCS` | Concurrent decode+infer slots. |
| `-decode-timeout` | `2s` | Per-sample ffmpeg wall clock (D4). |
| `-decode-memory-limit` | `1073741824` | Per-sample ffmpeg `RLIMIT_AS`, in bytes (D4). |
| `-ffmpeg` | `ffmpeg` | The decoder binary (D5); the image sets its own. |
| `-onnxruntime-lib` | `/usr/lib/libonnxruntime.so` | The ONNX Runtime shared library (D4). |
| `-targets-refresh` | `5s` | How often the target list is re-read. |
| `-sampler-id` | `$POD_NAME` | `samplerId` in flags. |
| `-shard-index` | the ordinal of `$POD_NAME` (`gawk-sampler-1` ⇒ 1) | This pod's share (D22). A pod name without an ordinal and no explicit value ⇒ refuse to start. |
| `-shard-count` | chart `replicas` (default `2`); `1` outside the chart | How many shares there are (D22). `-shard-index` ≥ `-shard-count` ⇒ refuse to start. |
| `-report-samples` | `12` | How many recent per-sample scores each tripped POST carries for the auto-kill tier (D14); 1–64. |
| `-sample-floor` | `0.3` | A sample below it in every flagged label is not stored (D20). |
| `-log-level` / `-log-format` | `info` / `text` | Logging. Logs name a broadcast by its key, never its raw ID. |
| `-metrics-addr` | `:9090` | Prometheus — operational metrics only (latency, effective interval, decode failures, POST errors); `off` disables it. Never routed. |

`gawk-admin` gains `-content-flag-repage` (`10m`), `-content-flag-mute`
(`30m`), `-content-flag-auto-kill` (`off`), `-content-flag-deadline`
(`off`), `-content-flag-stale-after` (`2m`, D9), `-screening-sample-retention` (`30d`), and starts using
`-flagger-role` — `-screening` with it `off` refuses to start (the chart
refuses to render it), since no sampler could then post. The relay gains
`-screening-token` and `-screening-notice`.

The sampler runs as a **StatefulSet of `replicas` pods (default 2), each
screening a fixed share of the broadcasts** (D22). A restart loses only that
pod's window state, which refills in 30 s; the sample count lives in
`gawk-admin` (D20).

## 6. Chunks and acceptance criteria

| Chunk | Scope | Verified by |
|---|---|---|
| **CF0** | Licence check, before any code: the NudeNet code licence and the weights' licence, and whether the deployment shape in D5 (weights fetched at deploy time, never in the image) is compatible with them | A short note in §8 recording both licences and their sources. **Gate**: if the weights' licence forbids this use, stop and return to OD5 before CF1. |
| **CF1** | Relay: keyframe route, token, notice knob, `RelayIdentity` notices byte in all three wire copies (D1, D13) | Hub test: the accessor returns the verbatim `StreamFrame`, and a new publisher session changes the ETag. Ops tests: 404 with no token configured, 401 on a wrong token, 404 for an unknown ID, 200 with `X-Gawk-Codec` for web AVCC, desktop Annex-B and VP8/VP9 fixtures, 304 on a matching `If-None-Match`; the admin token is **not** accepted on the route, nor the screening token on `/internal/admin/*`. Wire golden vectors byte-identical across Go, TS and Rust; an old-shape `RelayIdentity` still parses. Transport tests: with `-screening-notice` on, a publish session receives exactly one `RelayIdentity` stream carrying the bit; with it off, a publish session receives none (byte-identical to today); subscribe and `/internal/subscribe` sessions never receive it. The released desktop engine's publish-session reader, fed the new stream, drops it and keeps broadcasting (the `gawk-app` reader already does). Knobs reach `registryOptions`, env and chart (`helm template` assertion). |
| **CF2** | Shared types and events: `gawk-server/screening` (including the sample batch and replay types), the four event types, the additive fields, `RoomSpec.Screening`, `screening.Shard` (D3, D11, D16, D22) | Package tests round-trip the §4.11 body with `mode`, `state` and `recent`; the body validator rejects an absent or unknown `mode`, an unknown `state`, and `recent` over 64 entries or outside 0–1. `Shard` is deterministic, in range for every `n` ≥ 1, pinned by golden values (so a change to it is a visible break), and passes a chi-square uniformity test at p > 1e-6 for `n` = 2…8 over a committed set of 100 000 keys — a bound a fair hash fails about once in a million runs, so the test is neither flaky nor tuned to a seed. Events: the four new types (D16) with schemas, golden vectors, AsyncAPI and the OpenAPI enum; `store.AllEventTypes()` = `events.ModerationRowTypes()` holds with `reminded`, `resolved` and `room.screening_changed` in it and `observed` out of it, and `observed` is in `WebhookEventTypes()`; the existing schema/vector drift tests green; no publisher-IP field anywhere (an existing prohibition test extended to the new types). Rooms validation rejects `exempt` on dynamic or secret-less static rooms. |
| **CF3** | `gawk-admin` API and store: migration, `POST /content-flags` (raise/update/muted), targets with exemption, mutes and the shard filter, missing-shard detection, `flagRoute` / `503 no_flag_route`, operator list/acknowledge/dismiss, room screening `PATCH`, observed flags, sample ingest and pruning, replay (D2, D9–D12, D17, D19–D21) | DB tests: first tripped POST 201 + one event + one row; repeat POST 200 and no new event; a POST without `mode` is 400; a cleared POST resolves the unresolved flag `cleared` (open or acknowledged) and the next tripped POST raises a new flag with a new `content_flag.raised`; a tripped POST 5 s after acknowledging updates the acknowledged flag rather than raising a duplicate; POST during mute 202; mute expiry then POST raises a new flag. Delivery: an unfiltered webhook receives `raised`, `reminded`, `resolved` and `room.screening_changed` but never `observed`; a webhook whose filter names `content_flag.observed` receives it. Toggling a room's screening writes one `room.screening_changed` row. A `flagger` token reaches exactly the three flagger routes — `POST /api/v1/content-flags`, `GET /api/v1/screening/targets`, `POST /api/v1/screening/samples` — and is 403 everywhere else (the R49 loop); operator routes refuse `flagger`. Targets: origin rows only, broadcasts in the away grace period (`publisherActive: false`) left out, exempt broadcasts marked, `flagRoute: false` — and `POST /content-flags` in `flag` mode `503 no_flag_route` — when no enabled webhook accepts `content_flag.raised` (including a filter that excludes it). Shards (D22): for `of=2` the two shards' lists are disjoint and their union is the full list; a missing or out-of-range `shard`/`of` is `400`; with `of=2`, a shard that stops polling is reported missing after 30 s (gauge and banner) and clears when it returns; a poll with a new `of` re-bases the expected set. `TestOpenAPIMatchesRoutes` and `redocly lint` green. Observed flags: the first POST emits exactly one `content_flag.observed` and no `raised`; nothing pins or reminds them; they are accepted with `flagRoute: false` and during a mute, and resolve `cleared` on the cleared POST, or `cleared` with `reason: "stale"` after stale-after without one. Samples: a batch stores its rows numbered as the session's prior count plus position, and adds its count; a batch from a restarted sampler continues the same session's numbering with no collision; two concurrent batches for one session get disjoint numbers (row lock); pruning removes observed flags and samples older than the retention and never a `flag`-mode row. Replay: a fixture session with known gaps yields the hand-computed would-trips for three settings; a threshold under the floor is `400`; `flagger` gets 403 on replay. Migration passes the forward-only/expand–contract CI check. |
| **CF4** | `gawk-admin` lifecycle jobs and portal UI: reminders, deadline kill, auto-kill tier, session-end resolution, pinned flags, actions, banner, exemption toggle (D9, D14, D15) | Leader-job tests with a fake clock: an `open` flag re-pages every repage interval while its signal is live **and while it is unknown** (no POST for longer than stale-after — a silent sampler never silences a page), and stops once it is acknowledged or cleared; a sampler POSTing every 15 s (D6 at 3×) keeps the signal live; deadline kill off by default, on at `30m` kills an unacknowledged flag with a live signal and not an acknowledged, muted or unknown one; auto-kill `off` never kills; a `10/12` policy kills once with actor `system:content-flag` when `recent` holds ≥ 10 of 12 at ≥ 0.9, and not when `recent` has fewer than 12 entries (new session, restarted sampler) or 9 hits; session end resolves `ended`, and a tripped POST after an `ended` resolution for a broadcast whose publisher is not active is `202 ended` and raises nothing (no flag, no event). SPA tests for the pinned row, the actions, the banner, and the Calibration tab (form → replay → results; observed flags listed with Watch). |
| **CF5** | `gawk-sampler`: target polling, fetch with ETag, bitstream conversion, ffmpeg subprocess, ONNX inference and YOLOv8 post-processing, hysteresis, POSTs, `observe` mode and the `score` subcommand, metrics, client-credentials (D4, D6–D8, D18, D19) | Conversion tests: AVCC → Annex-B and IVF wrapping decode to the expected frame with the real ffmpeg. Post-processing matches NudeNet's Python reference on ≥ 10 fixture images to within 1e-3 per box score (reference outputs committed, images synthetic or licence-clean). Hysteresis table tests (trip, hold, clear, session reset): every sample while tripped POSTs `state: tripped` with `recent` holding the last `-report-samples` scores (fewer after a reset), and the clear POSTs `state: cleared` exactly once; a tripped broadcast that drops out of the target list (publisher away) gets no further POST, and its window and `recent` start empty if it returns. A truncated or garbage bitstream fails that one sample and the next sample of every other broadcast still succeeds; a decoder that hangs is killed at `-decode-timeout`. Scheduler test: 3× overload stretches every broadcast's interval and leaves none unsampled. Shard config: `gawk-sampler-1` with no explicit index takes index 1; a pod name without an ordinal, or an index ≥ the count, refuses to start; every targets poll carries `shard` and `of`. A static check that no code path writes frame bytes to disk or logs, and a test that the subprocess runs with `RLIMIT_CORE` 0. In `observe` mode a tripped window POSTs with `mode: observe`; in both modes samples below the floor are never sent, and each batch reports per session the count of samples taken and each sent sample's position among them (D20) — a below-floor sample counts but is not sent, a `404`, a decode failure or a timeout neither counts nor changes the window, and a `304` counts with the previous score; in `flag` mode with `flagRoute: false` nothing is sampled. Both input sizes load and score. |
| **CF6** | Packaging: Dockerfile (minimal ffmpeg stage, ONNX Runtime), chart (StatefulSet with `replicas: 2`, `podManagementPolicy: Parallel`, PodDisruptionBudget, preferred node anti-affinity, init-container model fetch with SHA-256, `PrometheusRule` for the interval and missing shards, optional NetworkPolicy limiting egress to the relay ops port and `gawk-admin`), release-please component, CI lane, image built from the repo root (D5, D6) | `helm template` assertions for every knob, the init container failing on a SHA mismatch, both rules (the missing-shard one with `for: 2m`), `-shard-count` rendered from `replicas` (2 by default, and following an override), and the PDB and anti-affinity. Image smoke run: `ffmpeg -decoders` lists exactly h264/vp8/vp9; the image's ffmpeg decodes an H.264 Annex-B, a VP8 and a VP9 fixture through the exact D4 command line into one RGB frame of the model's input size; the sampler starts, loads a test model and reports ready. Release-please dry run shows the new component. |
| **CF7** | Disclosure: terms paragraph, and the "Content screening" mark in `gawk-app`, desktop and `gawk-ios` (D13, §9.8) | Each app marks a server "Content screening" among its details when the server's probe (and, on desktop and iOS, the selected server's publish session) carries the screening bit, and not otherwise; no app ever marks a server that doesn't screen, and no broadcast screen shows anything (unit tests per app; a tier-1 e2e against a relay with `-screening-notice`: the web picker marks the deployment's row and the live page shows nothing, while the `--browser-broadcast` pass, without the flag, finds no mark). `gawk-ios` change in its own scoped commit. |
| **CF8** | End-to-end tier, docs, manual pass | A compose/kind tier with **two sampler replicas** and a **test model** that scores a synthetic pattern (solid magenta frames) as the flagged label, so CI never needs NSFW content. Several `gawk-pubsim` broadcasts are split across both pods, each sampled by exactly one (per-pod metrics); deleting one pod raises the missing-shard gauge until it returns, and its broadcasts are sampled again after. The flag path: `gawk-pubsim` publishes magenta → a flag is raised → the webhook receiver gets `content_flag.raised` → the same pattern under `observe` raises only `content_flag.observed` and stores samples a replay counts → dismiss → no flag during the mute → a regular pattern clears the window. Docs: self-hosting (enabling the sampler, the Keycloak `flagger` client recipe, the screening notice, the "no pager, no sampling" rule, OD8's out-of-scope note), `docs/gotchas.md`, `ROADMAP.md`, `CLAUDE.md`'s module list. Deploy to the reference deployment in `observe` mode, which starts CF9. |
| **CF9** | Calibration on real gawk, then switch to `flag` (OD12, D19) | The reference deployment runs `observe` for **≥ 100 broadcast-hours spanning ≥ 20 distinct games**, at least a week. From the Calibration tab's replay and the latency metrics: choose `-model-input` (640 only if its p99 latency × the observed peak broadcast count fits the sampler's CPU at a 5 s interval), and `-threshold` / `-window` / `-window-hits` such that replay gives **< 1 would-trip per 100 broadcast-hours**; during the run the owner joins live on `content_flag.observed` notifications, and checks each remaining would-trip against what was being streamed. The table size after the run is recorded beside the §3.1 estimate. Offline, `gawk-sampler score` over a public explicit-content set gives recall at the chosen threshold, recorded as a number only. The values go into D7, §5 and the chart defaults; §8 records the run. Paging is verified link by link, with no test hook in the production sampler: (1) during the `observe` run, samples arriving in `gawk-admin` from both shards prove the relay route, the screening token, the network path, decode, the model and the sampler's real `flagger` client; (2) the existing webhook test send (`POST /api/v1/webhooks/{name}/test`) reaches the operator's phone; (3) the first `content_flag.observed`, if one occurs, reaches the quiet channel its opt-in filter names. The flag-mode path from POST to page is `gawk-admin` code that CF3 tests and CF8 runs end to end with the test model. Then the reference deployment switches to `flag`, and game footage streams for an hour with no flag. (Revised 2026-10-13: the first draft paged the phone through a second sampler in a `-broadcast` test mode, which needed its own target-list parameter, an exempt room and special cases in the shard and sample logic to keep it from disturbing CF9; the links above cover the same chain.) |

## 7. Open after this doc

- CF0's licence verdict, and CF9's numbers (D7 defaults, model size).
- Whether two sampler replicas keep up at the fleet's real broadcast count
  — CF9's latency figure × the live count says; raising `replicas` (D22) is
  the answer if not.

## 8. Licence check and calibration

### 8.1 Licence check (CF0, 2026-10-13)

| What | Licence | Source |
|---|---|---|
| NudeNet code (`notAI-tech/NudeNet`, default branch `v3`) | **AGPL-3.0** — `LICENSE` and `LICENSE.md` both carry the AGPL-3.0 text since 2023-11-06; GitHub's licence detection agrees. `setup.py` still declares MIT, a stale classifier the licence files override. | `LICENSE`, `LICENSE.md`, the commit history of both, `setup.py` |
| Detector weights (`320n.onnx`, `640m.onnx`, release `v3.4-weights`, 2024-06-30) | **No separate licence.** The release notes are empty and the README says nothing about the weights, so they are published under the repository's AGPL-3.0. They are fine-tuned from Ultralytics YOLOv8n/v8m, which is itself AGPL-3.0. | the release, the README's model table |

**Verdict: the gate passes; OD5 stands.** AGPL-3.0 restricts conveying and
modifying, not running: a deployment that downloads the unmodified weights
and runs them is a use the licence grants. D5's shape is exactly what keeps
gawk on the right side of that line — the chart fetches the file at deploy
time from NudeNet's own release (pinned URL and SHA-256), so gawk never
conveys it, and nothing gawk distributes is a modified version of it.

Two consequences shape the build:

- **No NudeNet code enters the repository.** The repository's licence policy
  (CONTRIBUTING.md) admits no copyleft code, so the sampler's pre- and
  post-processing are reimplemented from the YOLOv8 output format, not
  ported from NudeNet's Python (§9.5). The label vocabulary is restated as
  data, which interoperability needs.
- **No weights enter the repository or an image.** CI and the dev stack
  run a generated test model (§9.6). The post-processing reference vectors
  are numbers derived from running the model, never the model.

The minimal ffmpeg in the image is LGPL-2.1-or-later (built without
`--enable-gpl` and `--enable-nonfree`) and runs as a separate executable,
never linked into the sampler. The image carries its licence text and the
exact source URL and configure line it was built from.

### 8.2 Calibration (CF9)

*Filled in by the CF9 run on the reference deployment.*

## 9. Implementation contract (2026-10-13)

Decisions the build took that the sections above leave open. Each is a
contract between at least two modules; the code cites these numbers.

### 9.1 Publisher epoch

The design keys windows (D7), sample numbering (D20), `recent` (D14) and the
`ended` resolution (D9) on the **publisher session**, but the relay's only
session handle, `publisherSessionId`, is the R28 telemetry handle and is empty
whenever telemetry is off. The relay therefore mints a **publisher epoch**:
16 lower-case hex characters from `crypto/rand`, minted on every
publisher-slot claim (new session, reclaim, takeover) in
`claimPublisherLocked`, present whatever telemetry is set to, and kept for the
hub's life after the publisher leaves. It is carried as
`adminapi.Broadcast.PublisherEpoch` (`publisherEpoch`) on
`/internal/admin/broadcasts`, as the `X-Gawk-Publisher-Epoch` header on the
keyframe route, and as `publisherEpoch` in every `gawk-server/screening`
body. It is an opaque identity, never a capability.

### 9.2 Keyframe route

`GET /internal/screening/broadcasts/{id}/keyframe` (`screening.KeyframeRoutePattern`),
registered on the ops listener only when `-screening-token` is non-empty.
`Authorization: Bearer <token>`, compared in constant time; any other
credential — the admin token, an admin JWT — is `401`, and the screening
token is `401` on `/internal/admin/*`. The relay refuses to start when
`-screening-token` equals `-admin-api-token`: one string in both places
would hand the sampler the publisher IPs D1's separate token exists to keep
from it. Answers:

| Case | Status |
|---|---|
| No hub, no cached keyframe, **or no active publisher** | `404` |
| `If-None-Match` equals the current ETag | `304`, ETag and epoch headers, no body |
| Otherwise | `200`, `Content-Type: application/vnd.gawk.stream-frame`, the StreamFrame verbatim, `ETag: "<epoch>-<frameID>"`, `X-Gawk-Codec`, `X-Gawk-Publisher-Epoch`, `Cache-Control: no-store` |

The "no active publisher" `404` goes one step past D1's wording: a broadcast
in its away grace still caches the frame its broadcaster last sent, and D2
already decided that frame must not be scored. The target list leaves those
broadcasts out; the route gives the same answer to a sampler acting on a
list up to one refresh old.

### 9.3 Wire: the notices byte

`RelayIdentity` gains `Notices` (one byte), appended after the name and
written **only when non-zero**, so every existing vector and every relay with
`-screening-notice` off stays byte-identical. Bit 0
(`RelayNoticeScreening`, `0x01`) is the screening notice; the other bits are
reserved and **ignored by parsers** — this is an appended field, the message's
extension mechanism, not the strict flags byte. A parser reads the first byte
after the name, when present, as `Notices`, and still ignores anything after
it. Golden vector, byte-identical in Go, `wire.ts` and `crates/wire`:

    RelayIdentity{ServerVersion: "1.42.0", Name: "gawk home", Notices: 0x01}
    01110006312e34322e30096761776b20686f6d6501

The existing "tolerates trailing bytes" vectors (`…a1b2c3`) now parse with
`Notices = 0xa1` — still a successful parse, which is what those tests
protect; they assert the new field too.

On publish sessions the relay sends this `RelayIdentity` (same version and
name as `/echo`) on its own server-opened uni stream at session start, only
when `-screening-notice` is on, from its own goroutine like the `/echo` send.
Subscribe and `/internal/subscribe` sessions never receive it. `/echo`
carries the bit too, so the desktop and iOS pre-live probes see it.

### 9.4 `gawk-admin` surface

Routes (all under `x-gawk-requires: screening`, registered only with
`gawk-admin -screening`, chart `screening.enabled`; off, they fall to the
catch-all `404` exactly as R39's reserved route did):

| Route | Role | Answers |
|---|---|---|
| `POST /api/v1/content-flags` | `flagger` | `201 raised`, `200 updated` / `resolved`, `202 muted` / `ended` / `ignored` (`screening.FlagResponse`); `400`; `503 no_flag_route` (flag mode only) |
| `GET /api/v1/screening/targets?shard=&of=` | `flagger` | `screening.TargetsResponse`; `400` on a missing or out-of-range pair |
| `POST /api/v1/screening/samples` | `flagger` | `200 {"stored": n}` |
| `POST /api/v1/screening/replay` | `operator` | `screening.ReplayResponse`; `400` for a threshold under the floor |
| `GET /api/v1/content-flags` | `operator` | `{"contentFlags": [ContentFlag…], "screening": Status}` — query `state=unresolved\|resolved\|all` (default `unresolved`), `mode=flag\|observe\|all` (default `flag`), `limit` |
| `POST /api/v1/content-flags/{id}/acknowledge` | `operator` | the flag; `409` unless `open` |
| `POST /api/v1/content-flags/{id}/dismiss` | `operator` | the flag, resolved `dismissed`, the broadcast muted for `-content-flag-mute`; `409` when already resolved |
| `PATCH /api/v1/rooms/{name}` body `{"screening": "exempt"\|""}` | `operator` | the room; `400 screening_not_allowed` unless static with an attach secret; also needs `-rooms` |

A `ContentFlag` (portal view) carries `id`, `mode`, `state`
(`open`/`acknowledged`/`resolved`), `resolution`
(`cleared`/`dismissed`/`killed`/`auto_killed`/`ended`), `resolutionReason`
(`stale`, `deadline`, `policy`), `broadcastId`, `broadcastKey`,
`publisherEpoch`, `samplerId`, `raisedAt`, `updatedAt`, `lastPostAt`,
`acknowledgedAt`/`acknowledgedBy`, `resolvedAt`/`resolvedBy`, `signal`
(`live`/`unknown`/`cleared`), `window`, `scores`, `labels`, `labelCounts`,
`peakScore`, `reminders` (count) and `mutedUntil`. `Status` carries
`flagRoute`, `expectedShards`, `missingShards` (indices) and `lastPollAt`, which
the portal's two banners (D12, D22) read. `GET /api/v1/broadcasts` rows gain
`contentFlag`: the broadcast's unresolved `flag`-mode flag, if any, which the
broadcasts view pins to the top (the R39 `FlaggedPinSlot` seam).

As built (2026-10-13), beyond the table: `GET /api/v1/me` reports
`features.screening`, which is what the SPA gates its screening views on;
the flag view carries `links.watch` (built from `-app-base-url`, like a
broadcast row's), omits its optional fields rather than sending `null`, and
always sends `labels` as an array; a `409` from acknowledge or dismiss
carries code `content_flag_state`; the room `PATCH` is registered under
`screening` and answers `404` with rooms off; in `flag` mode the `503
no_flag_route` and the `202 muted` apply to a cleared POST as well as a
tripped one; `room.screening_changed` carries `screening: exempt|screened`;
and the sampler's free-text `summary` stays in the portal — it is never
delivered as a page's `reason`. The D2 `ended` backstop **fails open**: a
POST is answered from the relay scan, and on a scan error or a partial
scan the session is treated as live, while the leader resolves `ended`
only from a complete scan — so a flaky pod never ends a real flag.

Every state the lifecycle needs is in Postgres, because `gawk-admin` runs
several replicas behind one Service: `content_flags` (one row per flag, both
modes, with `recent` and the last POST's body stored for audit),
`screening_mutes` (broadcast ID → until), `screening_sessions`,
`screening_samples`, and `screening_shard_polls` (one row per shard index:
`of`, sampler ID, last poll time), which every replica reads to compute
`missingShards` and `gawk_admin_screening_shards_missing`. The reminder,
deadline, stale and session-end jobs run on the leader's tick, like ban
expiry.

### 9.5 Model input and score

The sampler reproduces NudeNet's own pre- and post-processing, reimplemented
in Go from the YOLOv8 output format — **no NudeNet code is vendored or
ported** (§8.1). Pre-processing: the frame is scaled to fit the model's
square input with its aspect ratio kept, padded with black on the right and
bottom, RGB, scaled to 0–1, NCHW — NudeNet pads to a square before resizing,
which is the same image. That takes ffmpeg's `pad` filter, so D5's filter
list gains `pad`. Post-processing: per anchor, the class with the highest
score is a candidate when that score is ≥ 0.2; a class-agnostic NMS at
IoU 0.45 with a 0.25 score floor keeps the boxes. A label's sample score is
its highest kept box, 0 when it has none.

Four build-time choices sit on top of that:

- ONNX Runtime's own telemetry is off: `detect.Init` sets
  `ORT_DISABLE_TELEMETRY=1` before the library initializes, overriding the
  environment. The official Linux builds otherwise report to Microsoft by
  default, which a moderation sidecar must not do, and 1.29.0 segfaults
  starting that telemetry on the distroless runtime
  (microsoft/onnxruntime#32173; caught by the image's runtime-check stage).
- ffmpeg runs with `-xerror`, so a concealed (corrupt) frame is a decode
  failure — not counted, not windowed (D20) — rather than a smeared
  picture that gets scored.
- The service clips boxes to the whole square input, because ffmpeg does not
  report where the picture ends inside the padding; NudeNet clips to the
  picture. That can change an NMS decision only between overlapping boxes
  at the picture's edge. `score` and the reference test clip to the picture.
- `-sample-floor` above `-threshold` is refused at start-up: replay (D21)
  could never test the threshold the sampler is running.

### 9.6 Test model and fixtures

CI and the dev stack never load NudeNet. `gawk-sampler/testdata/models/`
holds a generated ONNX model per input size (320 and 640), with NudeNet's
input and output shapes (`[1,3,S,S]` → `[1,22,A]`), whose only output is
`FEMALE_GENITALIA_EXPOSED` at anchor 0 scoring the frame's magenta share,
`Σ R·B·(1−G) / Σ max(R,G,B)` — a ratio rather than a mean, because the
black letterbox would dilute a mean: a 16:9 magenta frame would score 0.56
and never trip at 0.8. A solid magenta frame scores 1 and the default pubsim fixture
scores far below any threshold the sampler accepts as a default. Its generator script is committed beside it. `gawk-pubsim`
gains `-fixture magenta`: a committed solid-magenta H.264 clip with the
default fixture's GOP. The post-processing reference vectors (CF5) are the
real 320n model's raw outputs on public-domain photographs (sources listed
beside them), committed as the candidate rows only, with NudeNet's detections
for each; the photographs themselves are not committed.

### 9.7 Dev stack

The compose stack gains a `screening` profile (docs/41): two samplers
(`gawk-sampler-0`, `gawk-sampler-1`, the StatefulSet's shape) on the test
model, a `gawk-pubsim` publishing `-fixture magenta`, and a webhook receiver
that records deliveries. `gawk-admin` runs with `-screening` there and the fake
IdP's client-credentials grant serves a `flagger` client beside the rooms bot.
The relay runs with a dev screening token and the notice on. CI's `dev-stack`
job runs the profile and asserts the flag path end to end, so the lane cannot
rot unnoticed.

### 9.8 Where the disclosure shows (owner, 2026-10-13)

D13 was first built as one line on every broadcast screen, Ready and live.
The owner moved it before merge: a prominent line on the screen a broadcaster
looks at while live reads as an invitation to test what gets through. The
apps now mark a server **"Content screening"**, with a check, among its
details, and only when it screens:

- `gawk-app`: each row of the server picker, under the host, from the
  picker's own probe (SP6's on-open probe of saved servers).
- desktop: the selected server's row in Settings, from the header probe or
  this broadcast's publish session; the full sentence is the row's
  accessible description.
- `gawk-ios`: each server in Settings, and the server's own page, from the
  probe or, for the relay a broadcast is up on, its publish session; the
  sentence is VoiceOver's hint.

Two rules carry over to any later surface. **No mark ever says a server
doesn't screen**: a cross would point the people screening exists for at
the servers without it. And the terms stay the primary disclosure: the
bundled paragraph now says the server lists show it.

The relay and the wire are unchanged: `/echo` and the publish session still
carry the notices byte. `gawk-app` no longer has a use for the publish
session's copy, so it reads that stream and drops it without a warning; the
desktop and iOS apps still use theirs, for the server the broadcast is
up on and only while it is up: a session's word that outlived its broadcast,
or followed the selection to another server, would mark a server that doesn't
screen (PR #500 review).
