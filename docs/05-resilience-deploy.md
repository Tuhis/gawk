# v0.5 — Resilience, packaging & CI/CD (Milestone D)

Status (2026-07-12): **implemented; automated verification passed** (server
`-race` suite incl. the new restart/keepalive integration tests, frontend
vitest suite, both docker images built and acceptance-tested locally, both
Helm charts `kubectl apply --dry-run=server`-clean against the homelab
cluster, workflows actionlint-clean). **First release cycle completed
2026-07-12**: both components released as v0.5.0, all four artifacts
(2 images, 2 OCI charts) verified on GHCR. Pending: manual browser verify
(checklist below) and the first real cluster install.

Scope note: the original D3 ("raw k8s manifests") was superseded during
planning — deployment is Helm-based with a full CI pipeline, and the
**frontend is deployed too**. Two components, separately versioned
(SemVer 2 from conventional commits), with **chart version == appVersion ==
image tag** per component, always.

## D1 — publisher-gone resilience

### Server: `-max-idle-timeout` / `-keepalive-period` (`internal/config`, `internal/transport`)

Subscribers already survived publisher death structurally (independent
sessions), and re-priming on publisher return was already covered by the C2
config-before-keyframe re-emission. The actual gap: with the broadcaster
away, no datagrams flow, and idle subscriber sessions died at the hardcoded
30s QUIC idle timeout.

- New flags (env fallbacks as usual): `-max-idle-timeout` (default `30s`)
  and `-keepalive-period` (default `10s`, `0` disables; validated
  `< max-idle-timeout`), wired into `quic.Config`.
- **The keepalive is the mechanism, not a bigger timeout**: the effective
  QUIC idle timeout is the *minimum* of both endpoints' advertised values,
  and browsers advertise ~30s — raising the server value alone is a no-op.
  Server-sent PINGs reset both endpoints' idle timers, keeping idle viewers
  connected across broadcaster-away gaps indefinitely with no browser-side
  change (Chrome exposes no keepalive knob).

Tests (`internal/transport/server_test.go`):

- `TestSubscriberSurvivesPublisherRestart` (the D1 acceptance test): with
  idle timeout 2s / keepalive 250ms, a subscriber receives session 1's
  stream, the publisher disconnects, a **3s quiet window** passes (longer
  than the idle timeout — only keepalives hold the session), the subscriber
  is still connected, and a new publisher with a different codec and reset
  frameIDs resumes flow to the *same* subscriber session.
- `TestIdleSubscriberTimesOutWithoutKeepalive` (negative control): keepalive
  disabled, idle 1s → the idle subscriber is dropped. Pins that the test
  above is actually exercising the keepalive.
- Helper: `startTestServerCfg` — `startTestServer` with a caller-supplied
  `config.Config` for non-default timeouts.

### Viewer: auto-reconnect (`gawk-app/src/transport/viewer-session.ts`)

`ViewerPipeline` stays single-shot; the new `ViewerSession` wrapper builds a
fresh pipeline per attempt (decoder/reassembler/WebTransport
teardown-recreate for free) with an injectable pipeline factory, so the
policy is vitest-testable with fake timers and no WebCodecs/WebTransport.

Policy — shaped by a browser limitation: **`WebTransportError` exposes no
HTTP status**, so 429-stream-full, a bad cert hash and a wrong URL are
indistinguishable in JS. Don't try to branch on a status code that isn't
there; the rule that sidesteps it:

- **Never-connected is fatal**: if the user-initiated `start()` rejects, the
  error surfaces immediately, no retries.
- **Drop-after-connect is retryable**: backoff
  `min(1000·2^(attempt−1), 15000)` ms → 1s, 2s, 4s, 8s, then 15s; gives up
  after 10 attempts (~100s, comfortably covers a relay restart) with
  `onError`. The attempt counter resets on every successful connect; failed
  reconnect attempts consume the budget, so an unreachable relay burns out
  instead of looping forever.

Wiring subtlety: `ViewerPipeline.fail()` fires `onError` *then* `onEnded`
(via its own `stop()`). The wrapper treats inner `onError` as reason-capture
only and acts on inner `onEnded` — acting on `onError` would flash a fatal
error before every reconnect.

`ViewPage` grew a `reconnecting` status (muted notice, not the red error
box; Stop works mid-backoff). Tests: `viewer-session.test.ts` (backoff
sequence, delayed reconnect, counter reset, budget exhaustion, stop-during-
backoff, initial-failure-is-fatal).

## D2 — container images

- `gawk-server/deploy/Dockerfile` (build context `gawk-server/`):
  `golang:1.25` → `gcr.io/distroless/static-debian12:nonroot` (uid 65532),
  `CGO_ENABLED=0 -trimpath -ldflags "-s -w -X main.version=$VERSION"`.
  ~16.5 MB. **`gawk-echo` ships in the image deliberately**: the server is
  HTTP/3-only (no TCP listener) and kubelet can't speak h3, so the chart's
  probes exec `/gawk-echo` against localhost; it doubles as an in-container
  diagnostic. Distroless has no shell — ENTRYPOINT and probe commands must
  stay exec-form.
- `gawk-app/deploy/Dockerfile` (context `gawk-app/`): `node:22-alpine`
  build → `nginxinc/nginx-unprivileged:1.29-alpine` (port 8080, non-root
  out of the box). ~54 MB. nginx.conf: immutable caching for `/assets/`
  (content-hashed), `no-cache` + SPA fallback for `/`.
- The `-race` test suite needs CGO and runs host-side/CI only — the image
  build runs no tests.

Acceptance (both passed 2026-07-12):

```sh
cd gawk-server
docker build -f deploy/Dockerfile -t gawk-server:dev .
docker run --rm -d --name gawk -p 4433:4433/udp gawk-server:dev -dev-cert
docker logs gawk 2>&1 | grep cert_hash_hex
go run ./cmd/gawk-echo -url https://localhost:4433/echo -cert-hash <hex>  # real verification
docker stop gawk

cd ../gawk-app
docker build -f deploy/Dockerfile -t gawk-app:dev .
docker run --rm -d -p 8080:8080 gawk-app:dev   # curl localhost:8080 → the SPA
```

## D3 — Helm charts

Charts live **inside** their component (`gawk-server/deploy/charts/gawk-server/`,
`gawk-app/deploy/charts/gawk-app/`) so release-please's path scoping versions chart
changes with the component — that placement is what makes chart version ==
image tag hold for free. Both `Chart.yaml`s carry
`# x-release-please-version` markers on `version:` and `appVersion:`; the
image tag defaults to `.Chart.AppVersion`.

`gawk-server` chart:

- Deployment hardcodes `replicas: 1` + `strategy: Recreate` — the hub is an
  in-memory single-publisher pub/sub; two overlapping pods behind one
  Service would split publisher and subscribers. Not a values knob on
  purpose.
- All probes are **exec probes running `/gawk-echo`** (startup, liveness,
  readiness) — httpGet/tcpSocket cannot reach an h3-only server. No
  `-cert-hash` → verification skipped: correct for a localhost self-probe
  and immune to cert renewals. Each probe costs one QUIC dial; trivial at
  this scale.
- `config.quietProbeLogs` defaults **`true`** in the chart — those same
  probes hit `/echo` forever on a tight period and would otherwise spam
  `session started`/`session ended` INFO lines; the binary itself defaults
  it `false` so local/dev runs still log everything. Set it `false` in the
  chart to see probe traffic while debugging.
- cert-manager `Certificate` (default issuer `letsencrypt-production`,
  ClusterIssuer; use `-staging` for first rollouts) → secret mounted at
  `/tls`; the server's `tlsutil.Reloader` picks up renewals per-handshake,
  **no pod restart needed**.
- Service `LoadBalancer` UDP 4433 (MetalLB; optional `loadBalancerIP` pin).
- `config.allowedOrigins` **must contain the frontend's origin** (the
  gawk-app ingress host, `https://` included) — the server checks `Origin`
  on CONNECT; empty allows all, don't ship that to prod. `CheckOrigin`
  exempts loopback `RemoteAddr` so the in-pod exec probe (no `Origin`
  header) still passes — see gotcha below.

`gawk-app` chart: 2 stateless nginx replicas, ClusterIP, Ingress
(`nginx-int` class, cert-manager annotation, TLS). Plain httpGet probes —
only the relay is h3-only.

Verified: `helm lint` clean on both; rendered output
`kubectl apply --dry-run=server` clean against the homelab (validates the
Certificate against the cert-manager CRD/webhook and the Ingress against the
nginx admission webhook). The `gawk` namespace was created for real —
server-side dry-run can't see a namespace it didn't persist.

## D4 — CI + release automation

- `release-please-config.json` + `.release-please-manifest.json`: monorepo
  manifest mode, **one combined release PR**
  (`"separate-pull-requests": false` — see the gotcha below; the first
  cycle ran with separate PRs and they conflicted on the shared manifest),
  components `gawk-server` (release-type `go` — changelog+tag only; the
  binary gets its version via ldflags, the chart via extra-files, no
  redundant version.txt) and `gawk-app` (release-type `node` — bumps
  package.json/lock). The combined PR only includes components with
  releasable commits, and merging it still cuts **separate per-component
  tags and releases**: `gawk-server-vX.Y.Z` / `gawk-app-vX.Y.Z`. Both were
  seeded at **0.4.0** (matching the project milestone numbering;
  package.json aligned) so the first releases came out 0.5.0. `extra-files`
  paths are **package-relative** (`deploy/charts/<component>/Chart.yaml`).
- `.github/workflows/ci.yml` (PR + main): Go vet + `-race` tests
  (`CGO_ENABLED=1`), frontend `npm ci`/lint/test/build, `helm lint` +
  `helm template`, docker build validation (no push, GHA cache with
  per-image `scope=`). No path filters on purpose (small repo, required-
  check semantics).
- `.github/workflows/release-please.yml` (main): release-please action →
  per-component outputs gate `publish-server` / `publish-app` jobs that push
  `ghcr.io/tuhis/<component>:X.Y.Z` (+`latest`) and
  `oci://ghcr.io/tuhis/charts/<component>` with
  `helm package --version X.Y.Z --app-version X.Y.Z` (belt-and-suspenders
  over the Chart.yaml bump). Login is `GITHUB_TOKEN`; packages inherit the
  repo's private visibility.

**First cycle (completed 2026-07-12):** release PRs opened at 0.5.0 with
the Chart.yaml bumps visible in the diffs → merged → per-component tags +
GitHub Releases created → publish jobs ran and pushed all 4 GHCR packages
(verified via push digests in the job logs). Two setup snags, both fixed:
the repo setting "Allow GitHub Actions to create and approve pull requests"
had to be enabled first, and the separate release PRs conflicted on the
shared manifest (→ `"separate-pull-requests": false`, gotcha below).

### Deploying (manual, publish-only CD by design)

```sh
# once per namespace — classic PAT with read:packages ONLY
# (fine-grained PATs don't cover GHCR):
kubectl -n gawk create secret docker-registry ghcr-pull \
  --docker-server=ghcr.io --docker-username=Tuhis --docker-password=<PAT>

helm registry login ghcr.io -u Tuhis   # paste the same PAT
helm upgrade --install gawk-server oci://ghcr.io/tuhis/charts/gawk-server \
  --version 0.5.0 -n gawk -f my-values.yaml
helm upgrade --install gawk-app oci://ghcr.io/tuhis/charts/gawk-app \
  --version 0.5.0 -n gawk -f my-app-values.yaml
```

Deploy-time placeholders to fill in a values file: relay DNS name
(`certificate.dnsNames`), frontend host (`ingress.host`),
`config.allowedOrigins` (= the frontend origin), `imagePullSecrets`,
optional MetalLB IP pin.

## Gotchas hit in this milestone

- **`"separate-pull-requests": true` self-conflicts in a monorepo** — every
  release PR edits the shared `.release-please-manifest.json`, so merging
  one strands the other on a conflict, and release-please does *not*
  reliably refresh the surviving PR (observed: post-merge run left it
  stale; the fix was manually merging main into the PR branch and resolving
  the manifest). Fixed by switching to one combined release PR
  (`"separate-pull-requests": false`) — components still get separate tags,
  releases and publish jobs; a component without releasable commits simply
  isn't in the PR.
- **Actions must be allowed to open PRs** — release-please fails with
  "GitHub Actions is not permitted to create or approve pull requests"
  until Settings → Actions → General → Workflow permissions → "Allow GitHub
  Actions to create and approve pull requests" is enabled (off by default).
- **Tags created with `GITHUB_TOKEN` do not trigger workflows** — publish
  jobs must chain off release-please outputs in the *same* workflow. Never
  "simplify" to an `on: push: tags` publish workflow; it will silently never
  run.
- **release-please `extra-files` paths are package-relative** — if the first
  release PR doesn't touch Chart.yaml, this is why. Both `version:` and
  `appVersion:` need their own `# x-release-please-version` marker.
- **Per-component action outputs need bracket syntax**
  (`steps.rp.outputs['gawk-server--release_created']`) and failures are
  silent (`if:` sees an empty string) — check the publish jobs ran after the
  first merge.
- **GHCR is lowercase-only** (`tuhis`, not `Tuhis` — and
  `github.repository_owner` returns `Tuhis`, so refs are hardcoded), and
  **cluster pulls need a classic PAT** with `read:packages`; fine-grained
  PATs still don't cover GHCR. Note the PAT expiry in the runbook — it will
  eventually manifest as `ImagePullBackOff`.
- **Charts publish under `oci://ghcr.io/tuhis/charts/`** — the `charts/`
  prefix keeps the chart package from colliding with the same-named image
  package.
- **`-dev-cert` mode breaks relay-restart reconnect testing**: the cert (and
  hash) regenerates on every server start, so the viewer's auto-reconnect
  can never succeed across a relay restart in that mode. Test with a
  file-based dev cert instead: `go run ./cmd/gawk-devcert -out certs` then
  `go run ./cmd/gawk-server -cert-file certs/cert.pem -key-file certs/key.pem`.
- **Node UDP buffers** apply to the k8s nodes too (`net.core.rmem_max`) —
  fix via sysctl on the node, not in the chart.
- `erasableSyntaxOnly` is enabled in gawk-app's tsconfig — no TS constructor
  parameter properties (surfaced in test code; oxlint doesn't catch it,
  `tsc -b` does).
- **The exec probe crash-loops the pod once `allowedOrigins` is set** —
  `gawk-echo` sends no `Origin` header, so `CheckOrigin` rejected its own
  `/echo` CONNECT (`webtransport: request origin not allowed`, surfaced to
  kubelet as `received status 500`) as soon as production set a real
  origin; dev never hit it because empty `allowedOrigins` allows all. Fixed
  by exempting loopback `RemoteAddr` in `CheckOrigin` — the probe always
  runs in-pod over `127.0.0.1`, which QUIC can't spoof, so this doesn't
  weaken the check against real off-pod clients. Caught 2026-07-12 in the
  first real cluster install (`internal/transport/server.go`).

## Manual verification (to close the milestone)

Setup: file-based dev cert (see gotcha above — hash survives restarts),
Chrome inside WSL2, hash pasted into the app.

1. Broadcast + 2 viewer tabs streaming normally.
2. **Keepalive proof**: stop the broadcast, wait > 60s (well past the 30s
   idle timeout), restart it — viewer sessions must never drop (watch
   `/statusz` `subscribers`), picture resumes at the first keyframe with no
   viewer action.
3. **Reconnect proof**: kill the relay process mid-watch — viewers show
   `reconnecting` with growing backoff; restart the relay — viewers heal
   automatically.
4. **Give-up proof**: kill the relay and leave it down — viewers give up
   with a clear error after ~10 attempts (~100s); Start Watching recovers
   manually once the relay is back.
5. One browser viewer against the dockerized relay (dev-cert hash from
   `docker logs`).
6. After the first release cycle: install both charts on the homelab, open
   the deployed frontend via its ingress host, watch a stream end-to-end
   through the LoadBalancer relay.
