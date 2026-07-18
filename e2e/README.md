# gawk E2E harness (R20, docs/25)

A real Chromium viewer decoding real relayed frames, as a CI gate. Tier 1
(every PR) runs the relay + `gawk-pubsim` + the production viewer on one
machine; tier 2 (release-please PRs + `workflow_dispatch`) does the same
against a 2-replica cluster-mode relay on kind. Assertions are **flow-shaped**
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

## Flake policy

One in-harness retry of the browser scenario (relaunch the browser, never the
relay/publisher — docs/25 Decision 8), logged loudly as `RETRY:`. Recurring
attempt-1 failures are findings for docs/25, not noise to rerun away.
