# R70 — The broadcast moderation timeline (docs/72)

**Status**: designed 2026-10-15 in a Claude Design pass (canvas
"Stream moderation timeline", two iterations with the owner); **MT1–MT6
implemented 2026-10-15**; MT7, the owner's pass on the reference
deployment, is open. Chunks **MT1–MT7** (`MT` = Moderation Timeline). Touches
`gawk-server/screening` (one additive batch field), `gawk-server/events`
(one additive field on `content_flag.raised`), `gawk-sampler` (sends its
window rule), and mostly `gawk-admin`: a migration, the flag lifecycle, three
knobs, one read route and a new portal view. **Builds on R40**
([docs/71](71-content-flagging.md)); revises its D9 and D10 where §3 says so.

## 1. Purpose

R40 pages the operator with one flag at a time: a score, a window, an age.
That is enough to say "look at this broadcast", and not enough to say what to
do once you have. The same 0.6 score means a game cutscene when it appears
once in a two-hour stream, and content that should be banned when it never
goes away. The difference is the **history**, and the portal does not show it.

R70 adds a drill-down per broadcast: a timeline of how it was scored and
flagged, the incidents on it, and one place to decide each incident. It also
fixes the two ways R40's lifecycle makes that decision noisier than it needs
to be: a score hovering at the threshold pages again on every re-trip, and a
broadcaster who returns to the content after an operator looked is paged
like a fresh accident.

## 2. Owner decisions (2026-10-15)

| # | Question | Decision |
|---|---|---|
| OD1 | What does the view show? | **A per-broadcast timeline**: publisher sessions, a score bar per time bucket (the highest score among the flagged labels), one strip per label, the incidents, and the moderation events, on one time axis; a pattern readout and per-incident history beside it (canvas, iteration 1). |
| OD2 | Where do the actions go? | **Split by what they act on** (iteration 2): acknowledge and dismiss act on an *incident* and live in a decision panel beside the timeline; end and ban act on the *broadcaster* and are reached from the panel's Violation step or a quiet Broadcast actions menu. Watch stays in the header. |
| OD3 | How is a flag judged? | **Look, then decide.** Look = *Watch & acknowledge* (one click that opens the watch link and acknowledges) or *Acknowledge only*. Decide = *Not a violation* (dismiss with a reason and a mute scope) or *Violation* (end, with the ban length inside the same form). |
| OD4 | Flicker | **One incident pages once.** A re-trip shortly after a clear rejoins the same incident instead of raising and paging a new flag. |
| OD5 | A return after review | **Pages as such.** A new incident on a broadcast whose earlier incident a person acknowledged or dismissed is marked *returned after review*, and the Violation choice is suggested. |
| OD6 | Levels | **Every level is configurable**: the floor, the threshold and the window rule stay the sampler's knobs and are now *reported* to `gawk-admin` rather than assumed; the new critical level, the sustained-run length and the rejoin window are `gawk-admin` knobs. Nothing in the UI hard-codes a score. |

Three questions the second iteration left open were decided during the build
(the owner said "build it"); each is a default that is cheap to change:

| # | Question | Default taken |
|---|---|---|
| OD7 | The rejoin window's length | **10 minutes, a knob** (`-content-flag-rejoin`, `off` restores R40's behaviour). |
| OD8 | Is opening the stream evidence enough to acknowledge? | **Yes**: *Watch & acknowledge* is the primary button; *Acknowledge only* stays beside it for an operator who has already seen the stream elsewhere. |
| OD9 | Should *returned after review* kill on its own? | **No.** It changes the page and the suggestion only. Automatic kills stay R40's D14/D15 policies, off by default. |

## 3. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **The sampler reports its window rule** in every sample batch: an additive `rule` object (`threshold`, `window`, `windowHits`) beside the existing `floor` and `labels`. `gawk-admin` stores the latest per publisher session (`screening_sessions.threshold`, `window`, `window_hits`, nullable). A batch without `rule` (a sampler older than R70) leaves them null, and the timeline says "threshold unknown" instead of drawing a guess. | The threshold is the sampler's (docs/71 D7), and so is the floor. A second copy of it in `gawk-admin`'s config would drift the first time an operator tuned one chart and not the other — exactly the R2 lesson. Reporting it per session also makes an old incident render against the rule that was in force *then*. |
| D2 | **The critical level is a `gawk-admin` knob**, `-screening-critical` (default `0.7`, in (0, 1]). It draws the top band of the score chart, counts "time at critical", and feeds the pattern and the suggestion (D8). It never flags or kills anything. | It is a reading aid for the person deciding, not a detector setting, so it belongs to the component that shows the decision. A critical level at or below a session's threshold is legal (the UI then draws one band). |
| D3 | **A clear of an unacknowledged incident is held for the rejoin window** (`-content-flag-rejoin`, default `10m`; `off` = R40's immediate resolve). A cleared POST for an *open* flag records `cleared_at` and resolves nothing; a tripped POST within the window clears `cleared_at`, increments `trips` and updates the same flag — no new row, no event, no page. The leader's sweep resolves a flag whose `cleared_at` is older than the window as `cleared`, with R40's `content_flag.resolved`. A clear of an **acknowledged** flag resolves at once, as in R40, so a later trip is a new incident (D4). Observed flags follow the same rule, with no events. Revises docs/71 D9. | Flicker is detector noise: a score at 0.5 against a 0.5 threshold trips, clears and trips for as long as the scene lasts, and R40 raised and paged a new flag each time (canvas, "Flicker"). Holding the clear rather than re-opening a resolved flag keeps every event true: a webhook never sees `resolved` followed by silent activity on the same flag. A held clear reads as signal `cleared`, so reminders stop exactly as for a resolved one. |
| D4 | **A new flag-mode incident on a broadcast with a reviewed incident in the last 24 h is `returnedAfterReview`.** Reviewed = acknowledged, or resolved `dismissed`. The flag row stores it; `content_flag.raised` carries `returnedAfterReview: true` (additive, schema and vectors updated) and its summary says so. Nothing is killed because of it (OD9). | Coming back after a person looked is the strongest evidence the timeline can show. Putting it in the event lets the pager route it at a higher priority without the portal knowing anything about the pager. 24 h bounds the lookback because broadcast IDs are re-minted eventually. |
| D5 | **Dismiss takes a reason and a scope.** Reason: `game_scene`, `not_explicit`, `detector_error`, `other` (stored as the flag's `resolution_reason`, delivered as `content_flag.resolved`'s `reason`). Scope: `episode` (until the sampler next reports the window cleared for this session), `duration` (`-content-flag-mute`, R40's default), `session` (until the publisher session ends). The `episode` and `session` mutes are bound to the publisher session and capped at 24 h. An empty body is `duration` with no reason, so R40 clients keep working. The flag row keeps `muted_until` and `mute_scope` so the timeline can draw the mute. Revises docs/71 D10. | A fixed 30-minute mute is too long for a cutscene and too short for a game the operator has accepted for the evening. A reason recorded at dismissal is what the *next* operator needs when the same broadcast pages again. |
| D6 | **A ban on a broadcast ID resolves that broadcast's flags as `killed`**, exactly as `POST /broadcasts/{id}/kill` does. Before R70 only the kill route did, so kill-and-ban from the portal left the flag open until the session-end sweep resolved it `ended`. | The Violation step bans; the flag must record that a person acted, not that the stream happened to end. |
| D7 | **One read route**, `GET /api/v1/screening/timeline?key=<broadcastKey>&from=&to=&bucket=` (operator): the broadcast's sessions with their rule (D1), the stored samples bucketed server-side, its incidents (both modes), its moderation events, the levels, and a computed summary, pattern and suggestion (D8). Keyed by the HMAC'd key, so a timeline outlives the broadcast and its URL carries no joinable ID. Range defaults to the broadcast's last 24 h of sessions, at most 48 h; buckets default to about 300 across the range and are capped at 1 500. A bucket holds the max score per label and the count of stored samples, above-threshold samples and critical samples in it; a bucket with no stored samples is omitted, and the sessions' spans tell "sampled, below the floor" from "not sampled". | One request draws the page. Bucketing on the server keeps the response bounded whatever the range; the chart never needs raw samples. Samples are pruned after `-screening-sample-retention` (30 d), so an older incident shows its row and events with an empty chart, which the view says. |
| D8 | **Pattern and suggestion are computed by `gawk-admin`, never by the sampler, and never act.** Per session, a *run* is consecutive sampled frames at or above that session's threshold (a missing sequence number is a below-floor sample and breaks the run); its length is its sample count times the session's mean interval. `none`: no sample at the threshold. `recurring`: two or more runs at least `-screening-sustained-after` (default `3m`) long, or any `returnedAfterReview` incident. `sustained`: one such run. `flicker`: three or more runs, or an incident with two or more trips. `isolated`: anything else. Suggestion: `recurring` → Violation, 7-day ban; `sustained` with a peak at or above critical → Violation, 24-hour ban; `isolated` → Not a violation; otherwise none. | The canvas's "Reading the timeline" board made four shapes and four likely decisions; writing them down once, server-side, keeps the phone and the desktop from disagreeing. A suggestion pre-selects a choice; a person still confirms every action. |
| D9 | **The view is `#/broadcasts/<key>`**, reached from every broadcast row, every pinned flag, the Calibration tab's observed flags and replay trips. Desktop: header with Watch and a Broadcast actions menu, the summary tiles, the timeline (whole stream / last hour / last 15 min, label strips that toggle), the incident list, and a sticky decision panel for the selected incident. Under 720 px the panel becomes a bottom bar that shows only the current step's buttons, with the dismiss and violation forms in a bottom sheet. An observed incident's panel has no actions: it says what would have paged. | The canvas, iteration 2. The bottom bar holds one primary button, then two, then a sheet — never five at once — because the portal's most important use is a phone in someone's hand. |
| D10 | **The Violation form reuses R39's kill-and-ban sequence** (IP ban first, then the ID ban, each tolerating `409 duplicate_active`), moved out of the Broadcasts view into a shared module so both call the same code. "Just end it" is R39's kill with its cooldown. | The ordering in that sequence exists because of a real race (an ID ban invalidates the fleet cache the IP ban resolves `publisher` against). A second copy would be the copy that forgets it. |

### Rejected

- **A threshold in `gawk-admin`'s own config** — it would drift from the
  sampler's (D1).
- **Re-opening a resolved flag on a quick re-trip** — the webhook would have
  seen `resolved` for an incident that then carried on (D3).
- **Suppressing the re-trip's page but raising a new row** — the incident
  list would still show four flags for one scene.
- **Killing automatically on *returned after review*** — OD9.
- **Client-side pattern computation** — two clients, two definitions (D8).
- **Thumbnails on the timeline** — docs/71 D8 still holds: no frame is ever
  kept.

## 4. The levels

Every level the view draws or reasons with, and where it is set:

| Level | Knob | Default | Owner | Used for |
|---|---|---|---|---|
| Floor | `gawk-sampler -sample-floor` (chart `config.sampleFloor`) | `0.3` | sampler, reported per batch | Which samples are stored; the chart's "sampled, below floor" band |
| Threshold | `gawk-sampler -threshold` (chart `config.threshold`) | `0.8` (placeholder until CF9) | sampler, reported per batch (D1) | The trip rule; the dashed line; runs and "time above threshold" |
| Window / hits | `gawk-sampler -window`, `-window-hits` | `6` / `4` | sampler, reported per batch (D1) | The trip rule, shown in the timeline's caption |
| Critical | `gawk-admin -screening-critical` (chart `screening.criticalScore`) | `0.7` | `gawk-admin` (D2) | Top band; "time at critical"; the 24-hour suggestion |
| Sustained run | `gawk-admin -screening-sustained-after` (chart `screening.sustainedAfter`) | `3m` | `gawk-admin` (D8) | `sustained` / `recurring` patterns |
| Rejoin window | `gawk-admin -content-flag-rejoin` (chart `screening.contentFlag.rejoin`) | `10m` | `gawk-admin` (D3) | Flicker coalescing |

## 5. Chunks and acceptance criteria

| Chunk | Scope | Verified by |
|---|---|---|
| **MT1** | Contract: `screening.SampleBatch.Rule` (additive, validated when present); `events.ContentFlagRaisedData.ReturnedAfterReview` with its JSON Schema, golden vectors and AsyncAPI; the sampler sends `rule` (D1, D4) | Package tests: a batch with a valid rule validates, one with an invalid rule (threshold outside (0,1], hits > window) is `ErrInvalid`, one without `rule` still validates. The events drift tests stay green with the new field in schema, vectors and AsyncAPI. Sampler test: every batch it posts carries its configured rule. |
| **MT2** | `gawk-admin` migration 0005 and store: session rule columns; `content_flags.trips`, `cleared_at`, `returned_after_review`, `muted_until`, `mute_scope`; `screening_mutes.scope`, `publisher_epoch`; held clears, rejoin and the sweep's resolution (D3); returned-after-review (D4); dismiss reason and scope with epoch-bound mutes (D5) | DB tests: with rejoin on, a cleared POST for an open flag leaves it unresolved with `cleared_at` set, signal `cleared`, and writes no event; a tripped POST within the window updates the same flag, `trips` = 2, no event; the sweep resolves a flag whose clear is older than the window as `cleared` with one `content_flag.resolved`; a cleared POST for an acknowledged flag resolves at once; with rejoin `off` every R40 test passes unchanged. A raise after an acknowledged-then-cleared incident on the same broadcast carries `returnedAfterReview` in the row and the event; a raise after one that was only cleared does not. Dismiss: the reason lands in `resolution_reason` and the event's `reason`; an unknown reason or scope is 400; `episode` mutes until the next cleared POST for that session and no longer; `session` and `episode` mutes do not apply to a new publisher session; `duration` behaves as R40; an empty body still dismisses. The session rule columns follow the latest batch and stay null for a batch without `rule`. Migration passes the forward-only/expand check. |
| **MT3** | Knobs: `-screening-critical`, `-screening-sustained-after`, `-content-flag-rejoin` — flags, `GAWK_ADMIN_*` envs, chart values, the start-up log (D2, D3, D8) | Config tests: defaults, env override, invalid values refused at start-up (critical outside (0,1], negative durations, `off` accepted for rejoin). `helm template` renders each value into its env var. |
| **MT4** | `GET /api/v1/screening/timeline` with sessions, buckets, incidents, events, levels, summary, pattern and suggestion; the ban route resolves flags (D6, D7, D8); OpenAPI | Handler tests over a seeded store: buckets aggregate the max per label and the counts; an empty bucket is omitted; the range defaults and caps hold; a session without a rule yields `thresholdKnown: false` and pattern `unknown`; the five patterns each from a fixture; the suggestion table; `flagger` is 403, `operator` 200; an unknown key is 200 with no sessions. `POST /bans` with a broadcast-ID target resolves the broadcast's open flag `killed`. `TestOpenAPIMatchesRoutes` and `redocly lint` green. |
| **MT5** | Portal: the `#/broadcasts/<key>` view — chart, label strips, incidents, decision panel, phone bottom bar and sheet; Watch & acknowledge; dismiss form; Violation form on the shared kill-and-ban module; links in from Broadcasts and Calibration (D9, D10) | SPA tests: the view renders a fixture timeline (bars, threshold line at the session's threshold, critical band at the configured level, the "threshold unknown" state); the panel walks Look → Decide → Not a violation → dismiss with the chosen reason and scope, and Look → Decide → Violation → the kill-and-ban calls in R39's order; Watch & acknowledge opens the watch link and posts acknowledge; an observed incident shows no actions; the bottom bar shows one step's buttons at a time under 720 px; the Broadcasts view's existing kill-and-ban tests pass on the extracted module. |
| **MT6** | Docs: this doc's status, `docs/71` cross-references at D9 and D10, `self-hosting.md` §13 (the levels table, the timeline), `ROADMAP.md`, `docs/README.md` | Read-through. |
| **MT7** | Deploy to the reference deployment and the owner's pass on a phone and a desktop | The owner opens a real broadcast's timeline, acknowledges, dismisses with a scope, and reports. |

## 6. Open after this doc

- Whether *returned after review* deserves a different pager route than a
  normal raise is the operator's webhook configuration, not gawk's; the field
  is there to route on.
- The pattern thresholds (`sustained-after`, the suggestion's ban lengths)
  are first guesses, tuned in the same CF9 run that tunes the threshold.

## 7. Notes from the build (2026-10-15)

- **The phone bar is CSS, not a second component.** Under 720 px the same
  panel is fixed to the bottom of the screen; it renders only the current
  step's controls on every width, so the "one step at a time" rule is
  tested once (jsdom has no layout, so the breakpoint itself was checked
  in headless Chromium screenshots at 390 px, not in a unit test).
- **A timeline without a reported rule reads `pattern: unknown`**, not
  `none`: there are samples, the runs just cannot be told.
- **The suggestion's ban lengths are fixed** (7 days for `recurring`, 24
  hours for a `sustained` run at critical); only the levels behind them are
  knobs. They are first guesses, tuned with CF9.
