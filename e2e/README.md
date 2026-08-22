# gawk E2E harness (R20, docs/25)

A real Chromium viewer decoding real relayed frames, as a CI gate. Tier 1
(every PR) runs the relay + `gawk-pubsim` + the production viewer on one
machine; tier 2 (release-please PRs + `workflow_dispatch`) does the same
against a 2-replica cluster-mode relay on kind; the Z5 browser-broadcaster
mode replaces pubsim with a second Chromium publishing from the production
broadcaster surface. Assertions are **flow-shaped**
(frames arrive/decode/render, drops bounded, zero loopback ingress loss) —
never fps ceilings or latency numbers; see docs/25 Decision 6.

This directory is top-level because the harness spans all three components;
`gawk-app` deliberately grows no E2E deps.

## Tier 1 locally

```sh
# once: binaries + built app
mkdir -p e2e/bin
(cd gawk-server && go build -o ../e2e/bin/gawk-server ./cmd/gawk-server)
(cd gawk-broadcast && CGO_ENABLED=0 go build -o ../e2e/bin/gawk-pubsim ./cmd/gawk-pubsim)
(cd gawk-telemetry && CGO_ENABLED=0 go build -o ../e2e/bin/gawk-telemetry ./cmd/gawk-telemetry)  # --telemetry only
(cd gawk-app && npm ci && npm run build)
(cd e2e && npm ci)

# the run
cd e2e && node run.mjs
```

## The telemetry pass (R28, docs/33)

```sh
cd e2e && node run.mjs --telemetry
```

Every other R28 test runs against fakes, in-memory sinks or handler-level
calls. This is the only thing that proves the pipe EXISTS end to end: the relay
starts with a fleet telemetry key and mints a real `TelemetryHello` (0x0D) onto
a real uni stream; the production viewer parses it and POSTs a real batch
cross-origin (preflight included); and a real `gawk-telemetry` verifies the
token, stores the session, and answers `diagnose()` about it.

The assertion that matters is **the join**: the `sessionId` in the relay's
`/statusz` `subscriberDetails` is the same one in the stored session. Before
TM1 those were two datasets with no shared key, which is the gap R28 exists to
close — so it is the one claim worth proving against real processes.

It also checks what only real data can: that a clean loopback stream is
diagnosed **healthy** (the boring case must not invent problems), that the
stored `browser` is a coarse class and never a raw UA, that `relayCoverage` is
not `"none"` when the relay was scraped, and that the live projection and
dashboard serve.

Its first run found five defects the unit suite structurally could not:

| Defect | Why unit tests missed it |
|---|---|
| A method-scoped mux route (`"POST /v1/ingest"`) 404'd the CORS preflight, so the handler's OPTIONS branch was unreachable dead code and the browser blocked every POST | Handler tests call `ServeHTTP` directly, bypassing the mux |
| A late batch re-created a finalized session, writing a **second** permanent rollup row (one viewer → three rows) | Needed a real client flushing on its own schedule against a real idle sweep |
| `relayCoverage` was hardcoded `"none"` — the TM4 join was never wired into the row | No test joined a real scrape against a real session |
| `diagnose()` called a clean 30 fps stream "decoder choking" — it compared median received against **p05** decoded, so any warmup ramp looked like starvation | Synthetic fixtures had no warmup |
| Legitimate `null`s (`avSkewMs` with no audio, `connection` — no browser ships `getStats()`) were counted as data-quality anomalies, tripping the "a client bug is likely" flag on a healthy session | Fixtures sent tidy objects, not a real `ViewerStats` |

**Default-off is asserted on every other pass**, not only here: the standard
viewer run records every request to a telemetry path and fails if there is
one. Without a fleet key the relay sends no hello, so a viewer must issue
literally zero — and the same filter counted 3 when telemetry was on, so the
guard is load-bearing rather than vacuous.

`-session-idle` must exceed the client's 10 s flush interval. A shorter one
finalizes a live session between its own flushes; that is how the duplicate
rows above were produced.

The runner spawns `gawk-server -dev-cert` (scraping `cert_hash_hex` from its
log), `gawk-pubsim` (scraping `GAWK_PUBSIM_ID=` from stdout), and
`vite preview`; seeds the viewer's persisted transport settings via
localStorage; opens `#/view/<id>` in headless Chrome; reads two
Copy-diagnostics JSON captures ~5 s apart through a stubbed
`clipboard.writeText`; screenshots the canvas (composited — never canvas
readback, the R16 finding stands); and checks the relay's `/statusz`.
Artifacts (child logs, diagnostics JSONs, screenshots, browser console) land
in `out/`.

No system Chrome? Point `GAWK_E2E_CHROME` at any Chromium ≥ ~140, e.g. a
playwright download (`npx playwright install chromium`). On a bare distro the
browser may also need `libnss3 libnspr4 libasound2t64` (extractable per-user
via `apt-get download` + `dpkg -x` + `LD_LIBRARY_PATH` if you can't install).

The tier-1 run then repeats the viewer scenario once in **R19 resilient mode**
(`gawk:resilient-mode` seeded before app boot, artifacts tagged
`resilient-<attempt>`), asserting that the viewer negotiates
`?delivery=reliable` and that its carrier reader keeps consuming rotating
carrier streams — the only automated exercise of the browser half of that
path. It reuses the running relay/publisher/preview, so it costs one browser
session. Loss is deliberately **not** injected here: behaviour under loss is
covered deterministically by `gawk-server/internal/transport/resilient_loss_test.go`
(a UDP forwarder dropping 15 % of the relay→viewer packets). See docs/25
finding 16 and docs/24 finding 10.

Then an **R25 audio pass** (docs/28 NA7, artifacts tagged `audio-<attempt>`).
It launches a *second* publisher — `gawk-pubsim -audio`, replaying the
committed Opus fixture through the real engine send path — and points one
viewer at it. A second publisher rather than a flag on the first, deliberately:
tier-1's standing assertion is that the **no-audio path stays intact**, and
that only means something while a video-only broadcast is still running beside
this one. (It is also why `assertRelaySide` takes an expected active-publisher
count instead of hardcoding 1.)

The assertions are flow-shaped like the rest: `audioState` active, the
advertised format (`opus` / 48000 / 2 ch) as the *viewer* parsed it, packets
arriving, and ≥ 80 % of them decoding. NA7 pre-registered a kill criterion for
the sink rows — if a browser exposes no `AudioWorklet` sink, `overflowDrops`
and `gapsSkipped` go unasserted and the harness **says so in its output**
rather than passing quietly (docs/25's no-silent-caps rule). In practice
headless Chrome does drive the sink, so they are asserted.

## Browser broadcaster (Z5)

```sh
cd e2e && node run.mjs --browser-broadcast
```

No pubsim: a second headless Chromium drives the production `#/broadcast`
surface. The capture picker is auto-granted by launching that browser with
`--auto-select-tab-capture-source-by-title=gawk-e2e-motion-source`, pointing
at an animated-canvas tab the harness opens first — **tab** capture, because
headless *screen* capture grants but delivers solid black frames (docs/25
finding 10). The harness scrapes the minted code from the LIVE topbar,
asserts the encode funnel from the broadcaster's own Copy-diagnostics JSON
(capture/encode/sent flowing, keyframes + streams growing, an `avc1.*`
codec), then runs the normal viewer scenario against that broadcast. The
worker offload path doesn't engage headlessly (no worker MSTP, no track
transfer — finding 12), so the broadcaster runs the main-thread pipeline;
the placement is recorded, not asserted.

## fMP4 muxer playback check (R22)

```sh
cd e2e && node run.mjs --muxer-check
```

Standalone and fast (~10 s): no relay, publisher or preview. Bundles the
production R22 muxer + MSE presenter with the committed H.264 fixture
(rolldown, `gawk-app/src/e2e/muxer-check-entry.ts`), loads the bundle into a
blank headless-Chrome page, and asserts the muxed bytes actually PLAY in a
`MediaSource` `<video>` — frames present via `requestVideoFrameCallback`,
`currentTime` advances, dimensions match the SPS (docs/27 Decision 10). This
is the CI proof the fMP4 is real media; the iPhone-native *presentation* stays
manual (docs/27 MF5). Runs first in the `e2e` job; artifacts in
`out-muxer-check/`. Prerequisites: `npm ci` in `gawk-app` (rolldown bundles
straight from source — no `npm run build` needed) and `npm ci` here.

## Tier 2 locally (kind)

Mirror the `e2e-cluster` job in `.github/workflows/ci.yml` — it is written to
be replayable line by line: build + `kind load` the image, `gawk-devcert` →
TLS secret, `helm install` with replicas=2 + clusterMode + NodePort 30443 +
throwaway fleet secrets + `SSL_CERT_FILE=/tls/tls.crt` (edge pods verify
their internal dials against the mounted self-signed cert — the edge dialer
never skips verification), then pubsim + `gawk-loadgen -viewers 12` +
`node run.mjs --external` + `./cluster-assert.sh gawk-e2e` +
`./moderation-assert.sh gawk-e2e <id> <admin-token>`.

The moderation step runs **last** on purpose: it kills the broadcast every
step before it depends on, and then bans the publisher's IP. It is also the
only place the harness observes *why* sessions ended — `gawk-loadgen
-expect-close-code 4006` reads the application close code off the session
itself (so 4000, a code-less transport death, and a session nothing closed all
fail differently), and `gawk-pubsim` prints `GAWK_PUBSIM_DIAL_STATUS=<n>` and
exits 3 when a dial is refused with an HTTP status, which is what makes
docs/42 D15's "451 so a native broadcaster can say *banned*" testable rather
than merely intended. Both are diagnostics: without the flag loadgen behaves
exactly as before, and pubsim's extra line only appears on a refused dial.

## Env knobs

| Var | Default | Meaning |
|---|---|---|
| `GAWK_E2E_SERVER_BIN` / `GAWK_E2E_PUBSIM_BIN` | `e2e/bin/...` | tier-1 binaries |
| `GAWK_E2E_CHROME` | first system Chrome found | browser executable |
| `GAWK_E2E_APP_DIR` | `../gawk-app` | built app checkout |
| `GAWK_E2E_RELAY_PORT` / `GAWK_E2E_OPS_PORT` / `GAWK_E2E_PREVIEW_PORT` | 4433 / 2112 / 4173 | ports |
| `GAWK_E2E_URL` / `GAWK_E2E_CERT_HASH` / `GAWK_E2E_ID` / `GAWK_E2E_OPS` | — | `--external` inputs |
| `GAWK_E2E_OUT_DIR` | `out` | artifact directory (CI keeps the two tier-1 steps apart) |

## Flake policy

One in-harness retry of the browser scenario (relaunch the browser, never the
relay/publisher — docs/25 Decision 8), logged loudly as `RETRY:`. Recurring
attempt-1 failures are findings for docs/25, not noise to rerun away.
