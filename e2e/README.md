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
(cd gawk-app && npm ci && npm run build)
(cd e2e && npm ci)

# the run
cd e2e && node run.mjs
```

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

## Tier 2 locally (kind)

Mirror the `e2e-cluster` job in `.github/workflows/ci.yml` — it is written to
be replayable line by line: build + `kind load` the image, `gawk-devcert` →
TLS secret, `helm install` with replicas=2 + clusterMode + NodePort 30443 +
throwaway fleet secrets + `SSL_CERT_FILE=/tls/tls.crt` (edge pods verify
their internal dials against the mounted self-signed cert — the edge dialer
never skips verification), then pubsim + `gawk-loadgen -viewers 12` +
`node run.mjs --external` + `./cluster-assert.sh gawk-e2e`.

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
