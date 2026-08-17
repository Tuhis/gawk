# R38 — Local development stack (docs/41)

**Status**: designed 2026-08-14; **LD1–LD7 implemented 2026-08-17**. What the
implementation found, where it deviated from this specification and why, and
what remains a manual pass, are all in **§10** — read that alongside §4, not
instead of it. Chunks **LD1–LD7** (`LD` =
Local Dev; two-letter prefix per the R21+ convention — collides with
nothing). Phased: **A** (LD1–LD3: compose skeleton + the zero-prerequisite
lane) → **B** (LD4–LD5: the trusted-certificate lanes) → **C** (LD6–LD7:
profiles, CI smoke, docs). Phase A is the whole product claim for a
contributor with nothing but Docker installed; B unlocks second devices; C
stops the stack rotting.

**Lane B (mkcert) was withdrawn on 2026-08-17**, after implementation, when its
premise was measured to be false: no browser will open a WebTransport session
against a certificate from a locally-installed CA. The evidence and the
consequences are in **§6** and **D5**; the lane letters are left as they were so
every existing cross-reference still resolves, so this document has a **lane A**
and a **lane C** and no lane B. Firefox — the reason lane B was designed —
works on lane A (§10.3).

**How to read this document**: §1–§2 are the why and the locked decisions.
**§3–§4 are the implementation specification** — exact files, exact flags,
exact commands, exact file contents. Where §4 gives a truth table or a
command line, that is the specification, not an illustration: implement it
literally. §8 lists the chunks with their acceptance criteria and names the
test files each one adds.

---

## 1. Purpose

Getting gawk running locally today means: install Go ≥ 1.26, install Node
≥ 22, run the relay in one terminal, run Vite in another, then **copy a
certificate hash out of the relay's startup log and paste it into a settings
panel in the browser** (`README.md` §Quickstart). It works, and every step of
it is a place a new contributor or self-hoster stops.

The specific hurdles, each grounded:

- **The dev certificate is regenerated on every server start**, so the hash
  has to be re-pasted after every restart — a stale hash fails with the same
  opaque error as a wrong one (`docs/gotchas.md:99`).
- **The production viewer `#/view/{id}` has no cert-hash field** — it is
  deliberately chrome-free (`docs/gotchas.md:56`). Locally it only works
  because the broadcaster page wrote the hash to same-origin `localStorage`
  first. A fresh profile, an incognito window, or a second machine simply
  fails; the E2E harness works around this by seeding `localStorage` directly
  (`e2e/run.mjs:628`).
- **`serverCertificateHashes` was believed to be a Chromium-only mechanism.**
  The whole documented certificate path is written in Chromium's terms
  (`docs/gotchas.md:93`), and Firefox — a supported *viewer* browser in
  production — had never been observed joining a local dev relay. **This
  premise was false**, and it is what motivated the withdrawn lane B: Firefox
  implements `serverCertificateHashes` too, and joins the default lane
  unmodified (§10.3). The hurdle was real; the diagnosis was not.
- **There is no LAN path at all.** `-dev-cert-hosts` defaults to
  `localhost,127.0.0.1` (`gawk-server/internal/config/config.go:240`) and the
  app is served over plain HTTP, so a phone, a second laptop, or the gaming
  PC running a native broadcaster cannot reach a local stack.
- **`-dev-cert` mode masks the certificate path production actually uses** —
  file-backed certs with a reloader (`docs/gotchas.md:102`).
- **Nothing is containerized for development.** Three Dockerfiles exist, all
  for deployment; there is no compose file anywhere in the repo. The only
  multi-process orchestration that exists is inside `e2e/run.mjs`, which is a
  CI harness and not usable interactively.

What R38 adds: **`docker compose up`, and a working broadcast**. One stack,
with the certificate as a pluggable input so the same stack serves a
contributor who has nothing installed, a maintainer who needs Firefox, and a
self-hoster who wants to rehearse the real TLS path before touching
Kubernetes.

The second beneficiary is verification. A great deal of this project's
behaviour is only observable with a relay, a publisher and a browser running
together — and today assembling that is manual enough that it does not
happen casually. Making it one command is how "let me check that locally"
becomes a normal act rather than a small project.

## 2. Decisions (locked with the owner, 2026-08-14)

| # | Decision | Rationale |
|---|----------|-----------|
| D1 | **One compose stack; the certificate is a pluggable input selected by `CERT_MODE`.** Every service downstream of `certs/` is byte-identical across lanes. | The three lanes differ *only* in cert provenance. Modelling them as three stacks would triple the surface that can rot; modelling them as one input keeps the interesting variation in one place and means the relay/app/telemetry wiring is exercised identically by all three. |
| D2 | **Lane A (self-signed, persisted, hash auto-injected) is the default.** No prerequisites beyond Docker, works offline, no admin prompt, no account, no domain. | The default has to serve the contributor who cloned the repo to fix one thing. Anything requiring a trust-store write, a domain, or an internet round-trip is a prerequisite that some contributor will not clear, and a default nobody can run is not a default. |
| D3 | **The dev certificate is persisted to a volume, not regenerated per start.** `-dev-cert` combined with `-cert-file`/`-key-file` means "generate into these paths if absent, otherwise load them" (exact semantics: §4.2.1). | This is the single highest-value fix in the milestone and it is nearly free. It kills the re-paste loop (`docs/gotchas.md:99`), and it moves local dev onto the **file-backed** certificate path production uses instead of the dev-only in-memory one (`docs/gotchas.md:102`). It also helps the non-Docker path, which is where most relay work will still happen. |
| D4 | **New dev-only runtime config key `devCertHashHex`, read only when `isDevEnvironment()`** (`gawk-app/src/config.ts:134`). It is **not** a Helm chart value and the `gawk-app` ConfigMap does not render it. | This is what closes the `docs/gotchas.md:56` hole: with the hash arriving via `/config.js` rather than `localStorage`, the bare `#/view/{id}` route works in a fresh profile, in incognito, and on a second machine. Keeping it out of the chart is deliberate — a production deployment that wants this key has misconfigured its TLS, and offering the knob would invite exactly that. |
| D5 | ~~**Lane B is mkcert** — a local CA, one `mkcert -install`, certs for `gawk.localhost` plus the machine's LAN IP.~~ **WITHDRAWN 2026-08-17, after implementation.** | The rationale was: "the cheapest way to a genuinely *trusted* certificate — ordinary chain validation, no hashes, no 14-day cap, and it writes to the NSS store so **Firefox works**. Chrome exempts locally-installed roots from both CT enforcement and the 398-day leaf limit, so there is no hidden trap." **There is a hidden trap, and it is fatal**: browsers apply a *second*, WebTransport-specific rule that a locally-installed root can never satisfy, so the page loads over HTTPS and the relay leg never connects. Measured in both engines; see §6 for the evidence and §10.2 for how it was found. Firefox, the lane's whole justification, needs nothing beyond lane A. |
| D6 | **Lane C is a guided ACME wizard against a domain the developer controls** — DNS-01, Let's Encrypt, one **wildcard**, `lego`. | It is the only lane where the private key is genuinely the developer's, and the only one where another device joins with nothing installed. It doubles as a rehearsal of the production path (`docs/self-hosting.md` §3 requires the same CA and the same challenge type), so the person most likely to need it is already the person it teaches. |
| D7 | **Lane C issues one wildcard (`*.<sub>.<domain>`), then the developer only edits A records.** | Issue-once is what makes the lane tolerable. A new device, a new LAN IP, a DHCP move — all become DNS edits against a certificate that already covers them, with no second ACME round-trip. Wildcards require DNS-01 anyway, which this lane already uses. |
| D8 | **DNS-01 proves control of the *name*, not reachability of the *host*.** The A record points at `127.0.0.1` throughout; nothing about the dev machine is ever exposed. This is stated prominently in the wizard's first screen and in the docs. | It is the misconception that stops people attempting this lane, and it is one sentence to remove. Worth a decision row precisely because the docs must not bury it. |
| D9 | **The wizard verifies rather than instructs.** It polls the authoritative nameservers for the TXT record itself and only submits to Let's Encrypt once it can see it; it checks CAA before spending an issuance; it resolves the finished name and detects DNS-rebinding blocks; it prints the expiry date. Full register: §4.6. | Anyone can link the certbot docs. The value is in naming the failure — and every failure in this lane is obscure. Submitting before propagation is the classic one, and Let's Encrypt rate-limits failed validations at 5/hour, so an impatient user locks themselves out with no idea why. `lego` checks authoritative NS by default; the wizard's job is to surface that as "still waiting…" rather than a stall. |
| D10 | **`lego` runs as an upstream container invoked by a shell script; no new Go binary, and nothing is added to `gawk-server`'s module graph.** | CI gates every dependency against a license allowlist and regenerates per-artifact `THIRD-PARTY-NOTICES.md` (`CONTRIBUTING.md` §Adding a dependency). Importing an ACME library into the relay's module to serve a development convenience would put it in the relay's notices and its supply chain forever. A container at arm's length costs nothing and brings ~100 DNS provider plugins for free. |
| D11 | **Compose profiles carry the optional pieces**: `sim` (a `gawk-pubsim` publisher), `telemetry` (`gawk-telemetry`), `app-dev` (Vite with HMR over a bind mount). The default `up` is relay + app + config-gen only. | A default that starts five containers is a default people stop running. Each profile answers a specific question — "I want a live broadcast without screen-sharing", "I'm working on telemetry", "I'm iterating on the frontend" — and costs nothing when unused. |
| D12 | **The compose stack is a development and evaluation tool, never a supported deployment path.** Helm remains the only supported way to run gawk for real, and the compose file's header comment says so. | The moment a compose file looks deployable, someone deploys it, and then it needs the operational properties the charts have and it does not (draining, probes, cluster mode, cert renewal, capacity limits). Naming the boundary in the artifact itself is cheaper than defending it later. |
| D13 | **One hostname serves both the app and the relay**, on different ports (app `${APP_PORT}`, relay `${RELAY_PORT}/udp`). | Certificates cover names, not ports, so a second name buys nothing and costs a second DNS record, a second SAN and a second thing to explain. It also keeps `-allowed-origins` a single value. |
| D14 | **The relay's ops listener binds to `127.0.0.1` in every lane, including when the stack is exposed to the LAN.** | `CLAUDE.md` is unambiguous that the ops endpoint is never public. A LAN-exposed dev stack is the exact situation where a convenience binding becomes a habit; the compose file should not be where that habit is learned. |
| D15 | **The stack builds the images from this checkout; it never pulls `ghcr.io/tuhis/*`.** | A contributor runs the stack to see *their* change. Pulling a released image would show them someone else's. |

## 3. Where it plugs into the existing code (grounded)

Almost all of the machinery exists; this milestone assembles it. Every row is
a fact about the tree as of 2026-08-14 — verify before changing, and if one
is stale, the design note is what is wrong, not the code.

| Piece | Where it is today | What R38 does with it |
|---|---|---|
| Dev cert generation | `gawk-server/internal/tlsutil/devcert.go`: `GenerateDevCert(hosts []string, validity time.Duration)`, `MaxDevCertValidity` (14 d), `CertHashHex(*x509.Certificate)` (hex SHA-256 of DER — **the value the browser wants**), `SPKIFingerprint` (base64 SHA-256 of SPKI — the value the *Chrome flag* wants; do not confuse them) | Reused unchanged. LD1 adds `WriteCertPair` beside them. |
| Certificate selection | `gawk-server/cmd/gawk-server/main.go`, `func certSource(cfg config.Config, log *slog.Logger)` — a three-arm switch: `cfg.DevCert` → ephemeral in-memory; `cfg.CertFile != ""` → `tlsutil.NewReloader`; default → error | **This is the function LD1 changes** (§4.2.1). |
| Reloader | `tlsutil.NewReloader(certPath, keyPath string, log)` — calls `tls.LoadX509KeyPair` eagerly and **returns an error if the pair cannot be loaded** | Unchanged. That eager error is what preserves fail-fast for production (§4.2.1 row 3). |
| Standalone cert writer | `gawk-server/cmd/gawk-devcert/main.go` (`-out`, `-hosts`, `-days`) — encodes the PEM pair inline | Stays as the host-side tool, but LD1 refactors its PEM encoding to call the shared `WriteCertPair` so there is one encoder. |
| Cert flags | `-dev-cert` (`GAWK_DEV_CERT`), `-dev-cert-hosts` (`GAWK_DEV_CERT_HOSTS`, default `localhost,127.0.0.1`), `-cert-file` (`GAWK_CERT_FILE`), `-key-file` (`GAWK_KEY_FILE`) — `internal/config/config.go:236–240` | No new flags. LD1 makes an existing *combination* meaningful. |
| Origin allowlist | `-allowed-origins` (`GAWK_ALLOWED_ORIGINS`), enforced in `internal/transport/server.go:247–270`: exact string match via `slices.Contains`, **with a loopback bypass** (`isLoopbackAddr(r.RemoteAddr)`) so in-container probes work without an Origin header | Set explicitly in every lane. The loopback bypass is why the healthcheck (§4.1) needs no origin. |
| Runtime frontend config | `gawk-app/public/config.js` (shipped empty) → `window.__GAWK_CONFIG__`; getters in `gawk-app/src/config.ts`; loaded by `<script src="/config.js">` at `gawk-app/index.html:12` | Gains `devCertHashHex` (LD2). The stack serves a generated `config.js` in its place — the same mechanism the Helm chart uses via its ConfigMap. |
| Dev-surface gate | `isDevEnvironment()` — `gawk-app/src/config.ts:134` — true for Vite dev **or** a bundle served from `localhost`/`127.0.0.1`/`::1` | Gates the new key (D4). Note it is **hostname-based**, so lane C (`gawk.dev.example.com`) is **not** a dev environment by this test — which is correct, because that lane has a trusted cert and emits no hash. |
| Publish-secret prompt | `requiresPublishSecret()` — `config.ts:96` — falls back to `isDevEnvironment()`, i.e. **true on localhost** | The generated `config.js` must set `requirePublishSecret: false` explicitly, or lane A's broadcaster prompts for a secret the stack never set. `e2e/run.mjs:649` sets it for the same reason. |
| Relay URL default | `defaultServerUrl()` — `gawk-app/src/state/transportStore.ts:53–66` (config.js `relayUrl` → `gawk.ioio.fi` hostname check → `https://localhost:4433`) | Unchanged. The generated `config.js` sets `relayUrl` explicitly so no lane depends on the fallback. |
| App image | `gawk-app/deploy/Dockerfile` → `nginxinc/nginx-unprivileged:1.31-alpine`, **listens on 8080**, root `/usr/share/nginx/html`, conf at `/etc/nginx/conf.d/default.conf` (from `gawk-app/deploy/nginx.conf`). That conf already sets `Cache-Control: no-cache` on `location /`, so a regenerated `config.js` is never served stale. | Reused. LD3 mounts a dev conf over `default.conf` to alias `/config.js` (§4.1.2). |
| Relay image | `gawk-server/deploy/Dockerfile` → `gcr.io/distroless/static-debian12:nonroot`. **No shell.** Contains `/gawk-server` and `/gawk-echo`; `ENTRYPOINT ["/gawk-server"]`; build context is `gawk-server/` | Reused. No shell is why the healthcheck must be exec-form, and why `gawk-devcert` is not available in-container — which is *why* D3 puts generation in the server. |
| Connectivity probe | `gawk-server/cmd/gawk-echo`: `-url` (default `https://localhost:4433/echo`), `-cert-hash` (**empty skips verification**), `-origin`, `-message`, `-count` (default 3), `-timeout` (default 5s) | Becomes the compose healthcheck (§4.1). |
| Telemetry image | `gawk-telemetry/deploy/Dockerfile` — **build context is the repo root**, cgo + `-tags duckdb`. Flags: `-ingest-addr` (`:8080`), `-read-addr` (`:8081`), `-data-dir` (`/data`), `-telemetry-key`, `-session-idle` (`2m`), `-relay-addrs`, `-stats-key` | The `telemetry` profile (LD6). `-session-idle` must exceed the client's 10 s flush interval — `e2e/README.md` records what happens when it doesn't. |
| Synthetic publisher | `gawk-broadcast/cmd/gawk-pubsim`: `-url` (default `https://127.0.0.1:4433`), `-secret`, `-insecure` (skip TLS verify — **the lane-A knob**), `-duration`, `-fps`, `-audio`, `-fixture`. Prints `GAWK_PUBSIM_ID=<code>` on stdout | The `sim` profile (LD6). **It has no Dockerfile** — LD6 adds `dev/Dockerfile.pubsim`. |
| Frontend dev server | `npm run dev` → Vite on `:5173`; `e2e/run.mjs:1623–1631` runs `vite preview` on `:4173` | The `app-dev` profile (LD6). |

## 4. Design (implementation specification)

### 4.0 Files this milestone creates or changes

Exhaustive. An implementer should not create files outside this list without
saying why.

**Created**

| Path | What |
|---|---|
| `docker-compose.yml` | The stack (§4.1). Header comment carries D12. |
| `.env.example` | Every variable in §4.1.3, with defaults and comments. Copied to `.env` by `dev/certs.sh` if absent. |
| `dev/certs.sh` | `CERT_MODE` dispatch + the lane C wizard (§4.4). |
| `dev/config-gen.sh` | Renders `dev/generated/config.js` (§4.2.2). Runs inside the `config-gen` service. |
| `dev/nginx-dev.conf` | `gawk-app/deploy/nginx.conf` plus the `/config.js` alias (§4.1.2). Header comment names the file it is a copy of. |
| `dev/Caddyfile` | HTTPS front for lane C (§4.1.2). |
| `dev/Dockerfile.pubsim` | Builds `gawk-pubsim` (LD6); context `gawk-broadcast/`. |
| `dev/generated/.gitkeep` | Keeps the bind-mount source directory in the repo. |
| `gawk-server/cmd/gawk-server/certsource_test.go` | LD1's truth table (§4.2.1). |
| `.github/workflows/` — a `dev-stack` job (may live in the existing `ci.yml`) | LD7's smoke test. |

**Changed**

| Path | Change |
|---|---|
| `gawk-server/cmd/gawk-server/main.go` | `certSource` gains the generate-if-absent arm; the file-backed arm logs identity (§4.2.1). |
| `gawk-server/internal/tlsutil/devcert.go` | Add `WriteCertPair` + `LoadOrGenerate` (§4.2.1). |
| `gawk-server/internal/tlsutil/devcert_test.go` | Tests for both. |
| `gawk-server/cmd/gawk-devcert/main.go` | Use `WriteCertPair` instead of encoding PEM inline. |
| `gawk-app/src/config.ts` | `devCertHashHex` field + `getDevCertHashHex()` (§4.2.3). |
| `gawk-app/src/config.test.ts` | Tests for the getter incl. the non-dev-origin case. |
| `gawk-app/src/state/transportStore.ts` | Fallback to the config hash (§4.2.3). |
| `gawk-app/src/state/transportStore.test.ts` | Precedence tests. |
| `gawk-app/public/config.js` | Document the key in the comment block, marked dev-only. |
| `.gitignore` | `dev/generated/*` (except `.gitkeep`), `certs/`, `.env`. |
| `README.md` | Quickstart rewritten (LD7). |
| `docs/self-hosting.md` | Cross-reference to lane C as the rehearsal (LD7). |
| `docs/gotchas.md` | Entries at `:56` and `:99` updated (LD7). |

**Explicitly not changed**: `gawk-app/deploy/nginx.conf`,
`gawk-app/deploy/charts/**` (D4 — the ConfigMap must not learn this key), any
Dockerfile under a `deploy/` directory, and anything under `e2e/`.

### 4.1 The compose stack

#### 4.1.1 Services

Five services; two are profile-gated (a third and fourth arrive in LD6).
`${VAR}` refers to §4.1.3.

| Service | Definition |
|---|---|
| **`relay`** | `build: {context: ./gawk-server, dockerfile: deploy/Dockerfile}`. Ports: `"${BIND_ADDR}:${RELAY_PORT}:4433/udp"` and `"127.0.0.1:2112:2112"` (D14 — this one is **always** `127.0.0.1`, never `${BIND_ADDR}`). Volume: `certs:/certs`. Environment: `GAWK_CERT_FILE=/certs/cert.pem`, `GAWK_KEY_FILE=/certs/key.pem`, `GAWK_ALLOWED_ORIGINS=${APP_ORIGIN}`, `GAWK_LOG_LEVEL=${LOG_LEVEL:-info}`, and **in lane A only** `GAWK_DEV_CERT=1` + `GAWK_DEV_CERT_HOSTS=${CERT_HOSTS}`. `restart: unless-stopped`. |
| **`relay` healthcheck** | `test: ["CMD", "/gawk-echo", "-url", "https://127.0.0.1:4433/echo", "-count", "1", "-timeout", "2s"]`, `interval: 5s`, `timeout: 5s`, `retries: 12`, `start_period: 5s`. **Exec form is mandatory** — the image has no shell. No `-cert-hash` (empty skips verification) and no `-origin` (the loopback bypass at `server.go:249` admits it). |
| **`config-gen`** | `image: alpine:3.21`, `entrypoint: ["/bin/sh", "/dev/config-gen.sh"]`, volumes `certs:/certs:ro` + `./dev:/dev-scripts:ro` + `./dev/generated:/out`. Environment: `RELAY_URL`, `CERT_MODE`, `OUT=/out/config.js`. `depends_on: {relay: {condition: service_healthy}}`. It installs nothing: `openssl` is in the `alpine` base. Exits 0 when the file is written. |
| **`app`** | `build: {context: ./gawk-app, dockerfile: deploy/Dockerfile}`. Port `"${BIND_ADDR}:${APP_PORT}:8080"`. Volumes: `./dev/nginx-dev.conf:/etc/nginx/conf.d/default.conf:ro` and `./dev/generated:/gawk-gen:ro`. `depends_on: {config-gen: {condition: service_completed_successfully}}`. |
| **`caddy`** | **Lanes B and C only** (`profiles: ["tls"]`, enabled by `dev/certs.sh` writing `COMPOSE_PROFILES` into `.env`). `image: caddy:2-alpine`, port `"${BIND_ADDR}:${APP_PORT}:443"`, volumes `./dev/Caddyfile:/etc/caddy/Caddyfile:ro` + `certs:/certs:ro`. Reverse-proxies to `app:8080`. When it is present, `app` publishes no port. |

Named volume `certs` holds `cert.pem` + `key.pem` and is the only thing the
three lanes disagree about. `dev/generated/` is a **bind mount**, not a
volume, so a developer can read the rendered `config.js` without entering a
container.

#### 4.1.2 Why the nginx conf is copied

`config.js` must be served from the site root (`index.html:12` loads
`/config.js`), and the app image is `nginx-unprivileged` running as a
non-root user that cannot write into `/usr/share/nginx/html`. Copying a
generated file over the shipped one at startup is therefore not available.

The alias is: `dev/nginx-dev.conf` is `gawk-app/deploy/nginx.conf` verbatim,
plus

```nginx
    # R38: the generated runtime config, bind-mounted read-only.
    location = /config.js {
        alias /gawk-gen/config.js;
        add_header Cache-Control "no-cache";
    }
```

It is a copy, and the header comment must say so and name
`gawk-app/deploy/nginx.conf` as the original. A second `server` block in an
additional `conf.d` file is **not** an acceptable alternative — two blocks on
the same listen/server_name make which one wins an nginx implementation
detail.

#### 4.1.3 `.env`

`dev/certs.sh` creates `.env` from `.env.example` on first run and rewrites
the derived values. Everything has a working default; a developer in lane A
never opens it.

| Variable | Default | Meaning |
|---|---|---|
| `CERT_MODE` | `devcert` | `devcert` \| `acme` |
| `GAWK_HOST` | `localhost` | The hostname browsers use. Lane C: `gawk.<sub>.<domain>` |
| `APP_PORT` | `8080` (lane C: `8443`) | Published app port |
| `RELAY_PORT` | `4433` | Published relay UDP port |
| `BIND_ADDR` | `127.0.0.1` | `0.0.0.0` to expose to the LAN (opt-in, §5) |
| `APP_ORIGIN` | derived | `http://localhost:8080` (A) or `https://${GAWK_HOST}:${APP_PORT}` (C). Goes to `GAWK_ALLOWED_ORIGINS` **and** must equal the browser's Origin exactly — no trailing slash |
| `RELAY_URL` | derived | `https://${GAWK_HOST}:${RELAY_PORT}` |
| `CERT_HOSTS` | `localhost,127.0.0.1` | Lane A SANs; `dev/certs.sh` appends `${LAN_IP}` when set |
| `LAN_IP` | unset | Auto-detected by `dev/certs.sh`; only used when the developer opts into LAN exposure |
| `ACME_DOMAIN` | unset | Lane C: the registrable domain, e.g. `example.com` |
| `ACME_SUBDOMAIN` | `dev` | Lane C: wildcard is `*.${ACME_SUBDOMAIN}.${ACME_DOMAIN}` |
| `ACME_EMAIL` | unset | Lane C: Let's Encrypt account address |
| `ACME_STAGING` | `0` | `1` uses the LE staging endpoint (tests always do) |
| `LEGO_DNS_PROVIDER` | unset | Lane C-1: `lego` provider code, e.g. `cloudflare` |
| `LOG_LEVEL` | `info` | Relay log level |

### 4.2 Lane A — self-signed, persisted, auto-injected (`CERT_MODE=devcert`)

The default. The relay generates a certificate into the `certs` volume on
first start and loads it on every subsequent one; `config-gen` renders its
hash into `config.js`; the app is served over plain HTTP at
`http://localhost:8080` — a secure context, so `getDisplayMedia` and
WebCodecs are available and a WebTransport dial to `https://localhost:4433`
is permitted.

#### 4.2.1 The relay change (LD1)

Add to `gawk-server/internal/tlsutil/devcert.go`:

```go
// WriteCertPair writes cert (0644) and key (0600) as PEM to the given paths.
// The encoding is the one gawk-devcert has always produced: CERTIFICATE and
// EC PRIVATE KEY blocks. Parent directories must already exist.
func WriteCertPair(certPath, keyPath string, cert tls.Certificate) error

// LoadOrGenerate returns the certificate at certPath/keyPath, generating and
// writing a fresh dev certificate for hosts when NEITHER file exists.
// generated reports whether it wrote one. If exactly one of the two paths
// exists it returns an error without writing anything.
func LoadOrGenerate(certPath, keyPath string, hosts []string, validity time.Duration) (cert tls.Certificate, generated bool, err error)
```

`cmd/gawk-devcert/main.go` is refactored to call `WriteCertPair` rather than
encoding PEM itself, so the two paths cannot drift.

`certSource` in `gawk-server/cmd/gawk-server/main.go` implements exactly this
table. Nothing else about its behaviour changes.

| `-dev-cert` | `-cert-file`/`-key-file` | Files on disk | Behaviour |
|---|---|---|---|
| no | no | — | Error `no certificate configured: pass -dev-cert or -cert-file/-key-file` (**unchanged**) |
| no | set | both present | `NewReloader` (**unchanged**) |
| no | set | either absent | **Error** from `NewReloader`. Unchanged, and load-bearing: a production deployment whose cert Secret failed to mount must crash, never self-sign |
| yes | **not** set | — | Ephemeral in-memory cert (**unchanged** — today's `-dev-cert`) |
| **yes** | **set** | both present | **Load via `NewReloader`. Do not regenerate.** Log identity + expiry |
| **yes** | **set** | both absent | `LoadOrGenerate` writes the pair, then `NewReloader` loads it. Log identity + expiry + `generated=true` |
| **yes** | **set** | exactly one present | **Error**: `refusing to overwrite an existing <cert|key> file`. Half-generating over a stray file is how a developer loses a key they meant to keep |

Two logging changes, both in `certSource`:

1. The **file-backed** arm currently logs nothing identifying. It must log
   `not_after`, `cert_hash_hex` and `spki_fingerprint` at `info` — otherwise
   a developer on the host loop has no way to obtain the hash once the
   ephemeral path stops being the default one they use.
2. Warn at `warn` level when `NotAfter` is under **72 h** away, naming the
   remedy (`docker compose down -v` in lane A, `./dev/certs.sh renew`
   otherwise).

The generated certificate keeps `MaxDevCertValidity` (14 d) — unchanged, and
a Chromium requirement, not a choice.

#### 4.2.2 `config-gen` (LD3)

`dev/config-gen.sh`, running in the `config-gen` service:

1. Wait for `/certs/cert.pem` to exist (it will — `depends_on` is
   `service_healthy` — but poll up to 30 s and fail loudly rather than
   emitting a config with no hash).
2. In lane A only, compute the hash the browser wants:
   ```sh
   HASH=$(openssl x509 -in /certs/cert.pem -outform DER | sha256sum | cut -d' ' -f1)
   ```
   This is hex SHA-256 of the DER, i.e. exactly `tlsutil.CertHashHex`. It is
   **not** the SPKI fingerprint, which is base64 and is only for the Chrome
   launch flag.
3. Write `$OUT` atomically (write to `$OUT.tmp`, then `mv`):
   ```js
   // Generated by dev/config-gen.sh — do not edit. Regenerated on every `up`.
   window.__GAWK_CONFIG__ = {
     relayUrl: "https://localhost:4433",
     requirePublishSecret: false,
     devCertHashHex: "a1b2…"   // lane A only; omitted entirely in lane C
   };
   ```
   `requirePublishSecret: false` is **required, not decorative**:
   `requiresPublishSecret()` falls back to `isDevEnvironment()`, which is true
   on localhost, so omitting it makes lane A's broadcaster prompt for a secret
   the stack never configured.
4. Print the URL a developer should open, so `docker compose up` ends with an
   actionable line.

#### 4.2.3 The frontend change (LD2)

In `gawk-app/src/config.ts`, add to `GawkRuntimeConfig`:

```ts
  // R38 (docs/41 D4): hex SHA-256 of the relay's self-signed dev certificate
  // DER, for local stacks whose relay presents an untrusted cert. Read ONLY in
  // a dev environment, and deliberately NOT a gawk-app chart value: a
  // production deployment needing this has a TLS misconfiguration, not a
  // missing knob. The local stack's dev/config-gen.sh renders it.
  devCertHashHex?: string;
```

and the getter, following the empty-string-counts-as-unset rule every other
getter here uses:

```ts
export function getDevCertHashHex(): string {
  if (!isDevEnvironment()) return '';
  const v = getRuntimeConfig().devCertHashHex;
  return (typeof v === 'string' && v.trim()) || '';
}
```

In `gawk-app/src/state/transportStore.ts`, the resolved credential's
`certHashHex` gains this **fallback, not override**, precedence:

1. A hash stored on the resolved server entry, or entered in the ⚙ panel.
2. Otherwise `getDevCertHashHex()`.
3. Otherwise `''`.

A developer who typed a hash must win over the generated one — they typed it
because they were pointing somewhere else. Note the interaction with R37: the
config value belongs to *this deployment's* relay, so it applies to the
pinned default entry and to any entry whose normalized URL equals
`defaultServerUrl()`; it must **not** be attached to a foreign `?relay=`
override, which would be presenting one relay's hash to another.

### 4.3 Lane B — mkcert (`CERT_MODE=mkcert`) — **WITHDRAWN**

This lane was specified, implemented, and then removed on 2026-08-17 when the
first browser test of its finished form showed that **it cannot work in any
browser** — the page loads over HTTPS and the WebTransport dial to the relay is
refused, because both engines apply a WebTransport-specific rule that no
locally-installed CA can satisfy.

The measurements, the two upstream mechanisms, and the hypotheses that were
ruled out are in **§6, "A locally-trusted CA (mkcert) for the relay"**. What
replaced it: nothing — Firefox, the lane's entire justification, works on
lane A (§10.3), and lane C remains the answer for other devices.

`CERT_MODE` therefore takes `devcert` and `acme` only, and `dev/certs.sh` has
no `mkcert` subcommand.

### 4.4 Lane C — guided ACME on your own domain (`CERT_MODE=acme`)

Prerequisite: the developer controls a domain. The wizard walks the rest, and
its first screen states D8 — **the A record points at `127.0.0.1`; nothing
about this machine is exposed.**

`lego` runs as `docker run --rm -v ./certs:/certs goacme/lego:v4` (pin the
tag). Two sub-lanes:

**C-1, DNS provider API token** — when `LEGO_DNS_PROVIDER` is set:

```sh
lego --email "$ACME_EMAIL" --dns "$LEGO_DNS_PROVIDER" \
     --domains "*.${ACME_SUBDOMAIN}.${ACME_DOMAIN}" \
     ${ACME_STAGING:+--server https://acme-staging-v02.api.letsencrypt.org/directory} \
     --path /certs run
```

Provider credentials pass through as environment variables named by `lego`
(e.g. `CLOUDFLARE_DNS_API_TOKEN`); the wizard reads them from `.env` and never
echoes them. Renewal is `lego … renew --days 30`, run on every `up`.

**C-2, manual TXT** — `--dns manual`, with the wizard supervising. This is
the path that needs the hand-holding, and §4.6 is its specification. The
transcript below is the intended output shape:

```
$ ./dev/certs.sh

  Domain you control?                    example.com
  Subdomain for local dev? [dev]         dev
  → will request a wildcard for *.dev.example.com

  Checking CAA records on example.com ............ ok (Let's Encrypt permitted)
  DNS provider? [1] API token  [2] I'll add records manually        2

  Add this TXT record at your DNS host:

      name   _acme-challenge.dev.example.com
      type   TXT
      value  gfj9Xq...Rg85nM

  Waiting for it to appear on ns1.example.com ...
  (checking every 10s — normally 1–5 min, safe to leave running)
                                                   ✓ seen, validating
  ✓ Wildcard issued, valid until 2026-11-12
    → certs/cert.pem, certs/key.pem

  Now add A records for whatever you want to reach:

      gawk.dev.example.com   A  127.0.0.1
      lan.dev.example.com    A  192.168.1.50   ← this machine's LAN IP

  Then: docker compose up
```

The wizard then rewrites `.env` — `APP_PORT=8443`, `APP_ORIGIN`/`APP_URL` on
`https://${GAWK_HOST}:8443`, `COMPOSE_PROFILES=tls`, `DEV_CERT` empty — with
`GAWK_HOST=gawk.${ACME_SUBDOMAIN}.${ACME_DOMAIN}`.

Renewal: automatic in C-1. In C-2 the certificate **cannot** auto-renew, so
the wizard prints the expiry date and the stack warns under 30 days. That
limitation is stated when the lane is chosen, not discovered as a broken
stack in 90 days.

### 4.5 Profiles and the inner loop (LD6)

| Profile | Service | Definition |
|---|---|---|
| `sim` | `pubsim` | `build: {context: ./gawk-broadcast, dockerfile: ../dev/Dockerfile.pubsim}`. Command: `-url https://relay:4433 -insecure -fps 30`. `depends_on: {relay: {condition: service_healthy}}`. `-insecure` is correct here and only here: an in-container Go client dialing a self-signed relay over the compose network. Its `GAWK_PUBSIM_ID=` stdout line is the join code — the compose logs are how a developer gets it. |
| `telemetry` | `telemetry` | `build: {context: ., dockerfile: gawk-telemetry/deploy/Dockerfile}` (**repo root context**). `-session-idle 2m` (must exceed the client's 10 s flush; see `e2e/README.md`). Ingest published on `127.0.0.1` only; the read/dashboard listener is **not** published at all — reach it with `docker compose exec`. Requires `GAWK_TELEMETRY_KEY` on both relay and service. |
| `app-dev` | `app-dev` | `image: node:26-alpine`, working dir a bind mount of `./gawk-app`, command `npm run dev -- --host 0.0.0.0 --port 5173`, published on `${BIND_ADDR}:5173:5173`, with an anonymous volume over `/app/node_modules` so the container's install is not shadowed by the host's. When this profile is on, `app` does not start. |

**The compose stack is not a replacement for the editor loop, and the docs
say so.** Relay work stays `go run ./cmd/gawk-server` on the host — now
pointed at the same persisted certificate:

```sh
go run ./cmd/gawk-server -dev-cert -cert-file ../certs/cert.pem -key-file ../certs/key.pem
```

Because that is the same pair the containerised relay uses, the browser side
needs no reconfiguration when a developer switches between the container and
the host binary. That interchangeability is a direct consequence of D3 and
belongs in the README.

### 4.6 The failure register

What the wizard and the stack detect, and what they say. This table is the
specification for LD4 as much as it is documentation: each row is a check to
implement, and §8 requires each to have a test or a documented manual
reproduction.

| Failure | Detection | Message |
|---|---|---|
| DNS rebinding protection (resolver refuses to return `127.0.0.1` or RFC1918 from public DNS) | Resolve the finished name after issuance; empty or NXDOMAIN while the authoritative NS answers correctly | Print the exact `/etc/hosts` line to add |
| CAA forbids Let's Encrypt | Query CAA on the registrable domain **before** issuance | Name the offending record; do not spend an issuance |
| TXT not yet propagated | Poll the authoritative NS for the zone directly, every 10 s | "still waiting…" with elapsed time — **never submit early** |
| Failed-validation rate limit | Classify the ACME error | Explain the 5/hour limit and when it resets |
| Issuance rate limit | Classify the ACME error | Explain 50/domain/week and 5 duplicates/week |
| Cert does not cover `${GAWK_HOST}` | Compare SANs against `.env` before starting the stack | Refuse to start rather than fail opaquely in the browser |
| Cert near expiry | On every `up` | Warn under 72 h (lane A) / 30 days (lane C) |
| Clock skew | Compare container time to `NotBefore` | Name it — a backdated cert failing validation is otherwise inscrutable |
| Port already in use | Bind check before `up` | Name the port and the `.env` variable that changes it |

### 4.7 Risks to settle during implementation

Not decisions — open questions whose answers belong in this doc once the
implementer has them.

- **QUIC through Docker Desktop's UDP port publishing (macOS/Windows).** The
  relay is UDP-only, and Docker Desktop's userland proxy is a different path
  from Linux's native port publishing. LD3 must verify a real broadcast on
  macOS before the lane is claimed to work. If it degrades, the documented
  fallbacks in order are: `network_mode: host` (Linux only), or running the
  relay on the host against the same `certs/` pair (§4.5) with only the app
  containerised. Record the finding either way.
- **LAN IP detection** differs per OS; `dev/certs.sh` should ask rather than
  guess if detection is ambiguous, and must never silently pick a Docker
  bridge address.
- **`alpine`'s `openssl`** must be present in the pinned base image; if a
  future bump drops it, pin `alpine/openssl` instead. The hash computation is
  the only thing `config-gen` needs.

## 5. Security considerations

- **`certs/`, `dev/generated/*` and `.env` are gitignored, and the wizard
  refuses to write anywhere else.** A private key in a public repository is
  not a mistake with a warning attached: CAs must revoke keys reported as
  compromised within 24 hours, so a committed key becomes a broken stack for
  everyone.
- **DNS API tokens are zone-scoped where the provider supports it**, live in
  `.env`, and are never echoed by the wizard or written to a log.
- **The ops listener stays on `127.0.0.1`** in every lane, including when
  `BIND_ADDR=0.0.0.0` (D14). This is a literal instruction about the compose
  file: the ops port mapping does not interpolate `${BIND_ADDR}`.
- **`-allowed-origins` is set, not left open.** The dev default is allow-all;
  the stack sets `${APP_ORIGIN}` explicitly, so the allowlist is exercised
  locally instead of first meeting a developer in production.
- **Telemetry stays off unless its profile is selected**, matching the
  default-off posture everywhere else.
- **LAN exposure is opt-in**: `BIND_ADDR` defaults to `127.0.0.1`, and the
  README says what changing it exposes.
- **`-insecure` appears exactly once in this milestone** — the `pubsim`
  service (§4.5), an in-container Go client on the compose network. It must
  not spread to any other service or example.

## 6. Rejected alternatives

- **A locally-trusted CA (mkcert) for the relay** — this was D5, it was
  built, and it was **withdrawn on 2026-08-17 after measurement**. Do not
  re-propose it, for gawk or for any other WebTransport service.

  **Both browser engines refuse a WebTransport session whose certificate
  chains to a locally-installed root, however correctly that root is
  installed.** The failure is invisible until the relay leg: the page loads
  over HTTPS with no interstitial, `curl` returns 200, and only the
  `WebTransport` dial fails — Chrome with "Opening handshake failed", Firefox
  with "WebTransport connection rejected".

  | Engine | Evidence | Mechanism |
  |---|---|---|
  | Chrome | relay's quic-go log records the client's own alert: `CRYPTO_ERROR 0x12e (remote): TLS handshake failure (ENCRYPTION_HANDSHAKE) 46: certificate unknown … CERTIFICATE_VERIFY_FAILED` | `net/quic/crypto/proof_verifier_chromium.cc` fails anything that is not `is_issued_by_known_root` with `ERR_QUIC_CERT_ROOT_NOT_KNOWN`, unless the host is in `hostnames_to_allow_unknown_roots_` (populated only from command-line switches) |
  | Firefox 154 | `MOZ_LOG=nsHttp:5`: `Http3Session::Authenticated error=0x0` — the certificate **verified** — immediately followed by `hasThirdPartyRoots=1, servCertHashesSucceeded=0` and `CloseTransaction[reason=804b0014]` (`NS_ERROR_NET_RESET`) | a third-party root is accepted for ordinary TLS and refused for WebTransport unless the certificate was pinned with `serverCertificateHashes` |

  Ruled out by experiment, so that none of it is re-derived:

  - **Not the certificate's lifetime.** mkcert issues ~823 days; a freshly
    issued 90-day leaf from the same CA, same SANs, fails identically.
  - **Not the IPv4 literal in `RELAY_URL`.** The SANs carry
    `IP Address:127.0.0.1`, `openssl verify -verify_ip 127.0.0.1` passes
    against the mkcert root, and Chrome loads `https://127.0.0.1:8443` over
    TCP with no interstitial. (Dialing the *hostname* instead is worse, not
    better: `*.localhost` resolves to `::1` first and the relay publishes
    UDP on IPv4 only, so those dials never reach the relay at all — §10.1.)
  - **Not the origin allowlist.** No `origin rejected` is logged; the
    connections die in the TLS handshake, before any HTTP/3 request exists.
  - **Not fixable with Chrome flags.** `--ignore-certificate-errors` and
    `--ignore-certificate-errors-spki-list` do not apply to WebTransport
    (§10.3). A lane whose instructions begin "launch your browser with a
    flag" is not the lane this milestone wanted anyway.

  The two shapes that *do* work are the two that survive: a self-signed
  certificate pinned by `serverCertificateHashes` (lane A — and Firefox
  implements that mechanism too, §10.3, which is what removed the last reason
  for this lane), or a publicly-trusted certificate (lane C).

- **nip.io / sslip.io as the certificate answer** — they are DNS-only by
  design. HTTP-01 cannot validate a name that resolves to loopback or RFC1918
  (Let's Encrypt would be connecting to itself or to an unroutable address),
  and DNS-01 for an `sslip.io` name needs an NS delegation to a publicly
  reachable nameserver the developer runs — the opposite of zero-setup.
  Issuance under those shared domains also competes for one Let's Encrypt
  quota, which has been exhausted before
  ([cunnie/sslip.io#108](https://github.com/cunnie/sslip.io/issues/108)).
  sslip.io published a wildcard certificate once and had to revoke it
  immediately; they do not intend to repeat it.
- **`localhost.direct`, `lancert` and similar published-key services** —
  they do work, and they are genuinely zero-click. Rejected as a *documented
  lane* because the private key is public by construction, which means
  revocation is a matter of when: `localhost.direct` documents repeated
  reissuance after users leaked its key, and now recommends a self-signed
  certificate as its own default. A dev stack that breaks for every
  contributor simultaneously, for reasons outside this project's control, is
  worse than one that asks nothing of the contributor at all (lane A). They
  remain reasonable things for an individual to use for a one-off phone test
  — their certificates are publicly trusted, so unlike a local CA they do
  satisfy WebTransport — and the docs may mention them as such, but nothing
  in the repo depends on them.
- **Committing a certificate and key to the repository** — same revocation
  argument, arriving faster: keys in public repositories are found and
  reported, and the CA is then obliged to revoke.
- **Running our own IP-in-the-name DNS service** (the lancert/sslip.io
  design, on `ioio.fi`) — it is the only thing that gives other devices a
  zero-install path without a third party, and it is a service to operate,
  monitor and renew. Out of proportion to "test on a phone occasionally",
  which an `/etc/hosts` line also solves.
- **Auto-fetching the cert hash from the relay's ops listener** (a dev-only
  `GET /devcertz`) — considered as an alternative to D4. Rejected: it would
  make the ops listener reachable from the page's origin, which
  `CLAUDE.md` forbids, and persisting the certificate (D3) removes the
  problem it was solving.
- **Adding `gawk-devcert` to the relay image** — it would put a development
  tool in the distributed production artifact to save an init container.
  D3 removes the need entirely.
- **Copying the generated `config.js` over the shipped one at container
  start** — the app image runs unprivileged and cannot write into
  `/usr/share/nginx/html`; the nginx `alias` (§4.1.2) is the mechanism that
  needs no write permission.
- **k3d/kind + Tilt as the default dev stack** — the highest-fidelity option
  and the wrong default: it reinstates a prerequisite stack larger than the
  one this milestone exists to remove. The `e2e` tier-2 kind job
  (`docs/25`) already provides cluster-mode proof in CI, which is what that
  fidelity was for.
- **Chrome launch flags (`--ignore-certificate-errors-spki-list`) as the
  documented path** — already printed by `gawk-devcert` and staying as an
  escape hatch. It requires relaunching the browser with a modified profile,
  helps neither Firefox nor other devices, and teaches a habit worth not
  teaching.
- **Making compose a supported deployment path** — D12.

## 7. Out of scope

- Deployment via compose (D12); Helm remains the only supported path.
- The native broadcasters (`gawk-broadcast`, `gawk-broadcast-windows`) — they
  are host binaries a person runs, not stack components. The stack is a relay
  they can point at, and `docs/self-hosting.md` already covers that.
- Automatic renewal in lane C-2 (structurally impossible without a DNS API).
- Cluster mode, multi-pod federation, or anything the `e2e` kind tier covers.
- WebKit/Safari — it cannot join a broadcast at all today (`BUGS.md`), so no
  lane is designed around it.
- A `gawk`-operated DNS or certificate service of any kind.

## 8. Chunks & acceptance criteria

Phase A — the zero-prerequisite default:

| Chunk | Scope | Acceptance criteria |
|---|---|---|
| LD1 | `tlsutil.WriteCertPair` + `LoadOrGenerate`; `certSource` per the §4.2.1 truth table; identity + expiry logging on the file-backed arm; `gawk-devcert` refactored onto `WriteCertPair` | New `gawk-server/cmd/gawk-server/certsource_test.go` covers **every row of the §4.2.1 table**, including: both files absent ⇒ written, key mode `0600`, ECDSA P-256, span ≤ `MaxDevCertValidity`; both present ⇒ **`CertHashHex` identical across two consecutive starts** (this is the assertion the milestone exists for); exactly one present ⇒ error, and **neither file modified**; `-cert-file` without `-dev-cert` and a missing file ⇒ still fails fast. `devcert_test.go` covers `WriteCertPair` permissions and a PEM round-trip. Expiry warning fires under 72 h and not above. `go vet` + `CGO_ENABLED=1 go test -race ./...` green. |
| LD2 | `devCertHashHex` config field + `getDevCertHashHex()`; transportStore fallback; `public/config.js` comment | `config.test.ts`: returns the value on a `localhost` origin; **returns `''` on a non-loopback origin even when the key is set**; empty/whitespace counts as unset. `transportStore.test.ts`: an entry-stored or user-entered hash **wins**; the config value fills in only when the entry's is empty; the value is **not** applied to a foreign `?relay=` override. A grep-based test asserts `devCertHashHex` appears nowhere under `gawk-app/deploy/charts/` (D4's deliberate omission, so a later sweep cannot quietly add it). `npm test && npm run lint && npm run build` green. |
| LD3 | `docker-compose.yml`, `.env.example`, `dev/config-gen.sh`, `dev/nginx-dev.conf`, `dev/generated/.gitkeep`, `.gitignore` | On a machine with only Docker, `docker compose up` yields a working broadcast: relay healthy via the exec-form `gawk-echo` healthcheck; `dev/generated/config.js` contains `relayUrl`, `requirePublishSecret: false` and a `devCertHashHex` that **equals the relay's logged `cert_hash_hex`**; `#/broadcast` mints a code; `#/view/<code>` renders frames **in a browser profile that has never visited `#/broadcast`** (the `docs/gotchas.md:56` regression test). `docker compose restart relay` does **not** change the hash. Ops port published on `127.0.0.1` only, asserted by reading the compose file. Compose header states D12. §4.7's Docker Desktop question answered in writing. |

Phase B — trusted certificates:

| Chunk | Scope | Acceptance criteria |
|---|---|---|
| LD4 | `dev/certs.sh`: `CERT_MODE` dispatch, lane C (C-1 and C-2), `.env` authoring, and every §4.6 check | Each §4.6 row has a test or a documented manual reproduction — none may be silently unimplemented. TXT polling **never submits before the record is visible on the authoritative NS** (asserted against a stub resolver); the CAA check precedes issuance; a SAN/`${GAWK_HOST}` mismatch refuses to start; secrets never echoed to stdout or logs. `certs/`, `.env` and `dev/generated/*` gitignored, asserted by a test that greps the repo rather than by hope. Lane C tests run against the Let's Encrypt **staging** endpoint (`ACME_STAGING=1`). |
| LD5 | Lane C wired in: `dev/Caddyfile`, the `tls` profile, LAN opt-in, no `devCertHashHex` when the cert is trusted | A second device on the LAN joins with nothing installed. The generated `config.js` contains **no** `devCertHashHex` key at all (asserted), and the relay runs without `GAWK_DEV_CERT`. `BIND_ADDR` still defaults to `127.0.0.1`; the ops mapping is `127.0.0.1` even when it does not. **Firefox as the viewer** was originally lane B's criterion; it moved to lane A, where it is met (§10.3) — a trusted-CA lane was never what Firefox needed. |

Phase C — profiles, gates, docs:

| Chunk | Scope | Acceptance criteria |
|---|---|---|
| LD6 | Profiles `sim`, `telemetry`, `app-dev`; `dev/Dockerfile.pubsim` | `sim`: a viewer joins the pubsim broadcast using the `GAWK_PUBSIM_ID` from the compose logs, with no browser broadcaster. `telemetry`: ingest reachable same-origin; **the service does not start without the profile** (asserted); the read/dashboard listener is unpublished. `app-dev`: an edit under `gawk-app/src` is reflected without a rebuild, and `app` does not start alongside it. Default `up` starts exactly `relay`, `config-gen`, `app` (asserted). |
| LD7 | CI smoke job + docs: README quickstart, `docs/self-hosting.md` cross-reference, `docs/gotchas.md` sync | CI job brings lane A up, asserts a datagram round-trip via `gawk-echo` against the published port, and tears down; it is **verified to fail when the stack is broken** (break it once deliberately, record what the failure looked like). README quickstart leads with `docker compose up` and keeps the host-toolchain path as the relay inner loop, including the §4.5 shared-`certs/` invocation. `docs/gotchas.md:99` and `:56` updated to describe the new default rather than the old workaround. The §4.0 lane table lands in the README, not only here. |

## 9. Verification

Automated gates are per-chunk above. The milestone-closing manual pass is
three fresh-clone runs, each on a machine that has *only* the lane's stated
prerequisite:

1. **Lane A, Docker only, offline.** `docker compose up`, broadcast in
   Chromium, join from a second tab and from a fresh incognito profile.
   Restart the stack; the viewer still joins with **nothing re-pasted**.
2. **Lane A in Firefox.** The same stack, unmodified, with the viewer in
   **Firefox** — no CA installed and no admin password anywhere. (This was
   lane B's run before that lane was withdrawn; it is a property of lane A,
   which is why lane B's removal costs nothing.)
3. **Lane C, plus a domain.** The wizard end to end on a registrar without an
   API (the C-2 path, which is the one that needs the hand-holding), then a
   phone on the LAN joining `https://lan.<sub>.<domain>:8443` with nothing
   installed.

The standing claim across all three: a contributor who has never seen this
repository gets from `git clone` to a rendered frame without reading a
design doc, and a self-hoster who completes lane C has already performed the
DNS-01 issuance that `docs/self-hosting.md` §3 asks of them.

## 10. Implementation status (2026-08-17)

All seven chunks landed. This section is the record of what the design could
not have known: §10.1 answers the §4.7 risks, §10.2 lists every deviation from
§4 with its reason, §10.3 says what was actually verified and how, and §10.4
names what is still a manual pass.

### 10.1 The §4.7 risks, answered

- **QUIC through Docker's UDP port publishing on macOS: it works, but the
  runtime matters.** A real browser broadcast completed through the published
  port on macOS (Apple Silicon) — 52 fps, hardware H.264, a viewer decoding in
  a separate profile. The condition is the port forwarder: **Colima's default
  is SSH, which is TCP-only**, so a published UDP port accepts the mapping
  (`docker port` shows it) and silently swallows every packet. `colima start
  --port-forwarder grpc` fixes it; Docker Desktop's userland proxy handles UDP
  as-is. Neither documented fallback (`network_mode: host`, or the host relay
  of §4.5) was needed.
- **A second, sharper finding in the same area: `localhost` is the wrong
  spelling.** It resolves to `::1` first on macOS and most Linux desktops,
  Docker publishes UDP on IPv4 only, and a QUIC dial has none of the
  happy-eyeballs fallback that hides this for the app's TCP port. The symptom
  is a bare "Opening handshake failed" with **nothing in the relay's log**,
  because nothing reached it. Every lane's `RELAY_URL` therefore names an IPv4
  literal or a name whose A record the developer controls.
- **LAN IP detection** is per-OS (`ipconfig getifaddr` on macOS, `ip -4 -o
  addr` elsewhere), filters Docker's 172.16/12 pools and bridge interfaces,
  and asks rather than guessing when it finds none or several. LAN exposure
  stays opt-in.
- **`alpine`'s `openssl`: it is not there.** The base image (verified on 3.21)
  carries the libraries but no binary. Rather than pin `alpine/openssl` —
  which would mean a second image pull in the lane whose claim is that it
  works offline — `dev/config-gen.sh` computes the DER hash with busybox
  (`awk` the leaf block out, `base64 -d`, `sha256sum`). Verified byte-identical
  to both `openssl` and `tlsutil.CertHashHex`.

### 10.2 Deviations from §4, and why

| § | Specified | Implemented | Why |
|---|---|---|---|
| §4.3, D5 | **lane B (mkcert)** | **removed entirely** — no `mkcert` in `CERT_MODE`, `dev/certs.sh`, `.env.example` or the README | Built to spec, then measured against a browser for the first time and found unusable: neither engine will run WebTransport over a locally-installed root (§6 for the evidence). Nothing replaced it, because the lane's justification — Firefox — turned out to be satisfied by lane A. This is the deviation the milestone's post-implementation review actually earned. |
| §4.1.1 | `certs` as a **named volume** | a **bind mount** on `./certs` | §4.5's shared-certificate inner loop, lane C's `install_lego_output` and §5's "`certs/` is gitignored" all assume a host directory; a named volume can satisfy none of them. `certs/.gitkeep` is tracked so the directory exists before Docker can create it root-owned. |
| §4.1.1 | `config-gen` mounts `./dev:/dev-scripts` but runs `/dev/config-gen.sh` | runs `/dev-scripts/config-gen.sh` | `/dev` is the device filesystem; mounting over it is not survivable. The spec contradicted itself. |
| §4.1.1 | lane A only sets `GAWK_DEV_CERT` | `GAWK_DEV_CERT: "${DEV_CERT-1}"` | Compose cannot make an environment key conditional on a lane. An empty value is false to the relay's parser, so `dev/certs.sh` clears it for the trusted lanes. |
| §4.1.1 | in the trusted lanes `app` "publishes no port" | it publishes `127.0.0.1:8081` | Compose cannot drop a port mapping per profile either. Loopback-only preserves what the rule was protecting (no LAN-exposed plain HTTP) and keeps a debugging door. |
| §4.5 | `pubsim` builds with context `gawk-broadcast/` | context is the **repo root** | `gawk-broadcast` reaches `gawk-server/wire` through `replace ../gawk-server`, so the relay's tree must be in the context — the same constraint `gawk-telemetry`'s image documents. |
| §4.5 | — | `GAWK_ALLOWED_ORIGINS` also carries `gawk-broadcast://native` | Every native broadcaster — `pubsim` included — sends that origin, and without it the `sim` profile is rejected with a bare 403. `docs/self-hosting.md` already lists the same row for real deployments. |
| §4.5 | — | the `telemetry` service runs as root | The chart gives it `fsGroup: 65532` so it can write its volume; compose has no equivalent and a fresh named volume is root-owned. |
| §4.4 | `lego … --dns manual` | plus `--dns.resolvers` | lego finds the zone to write into by walking up the name with SOA queries, and **Docker's embedded resolver does not answer them**: it walks to the TLD and fails before printing a challenge at all. Defaults to `1.1.1.1:53,8.8.8.8:53`, overridable with `LEGO_DNS_RESOLVERS`. |
| §4.2.1 | expiry remedy named as `docker compose down -v` | `./dev/certs.sh renew` | With a bind-mounted `certs/`, `down -v` does not remove the pair. One command now covers all three lanes. |
| §4.0 | — | adds `dev/certs_test.sh`, `dev/stack_test.sh`, `certs/.gitkeep` | LD4 and LD6 require assertions ("asserted by reading the compose file", "each §4.6 row has a test"); these are where they live. |

### 10.3 What was verified, and how

Automated, and running in CI (`dev-stack` job in `ci.yml`):

- `certsource_test.go` — every row of the §4.2.1 truth table, including two
  consecutive starts serving an identical `CertHashHex`.
- `config.test.ts` / `transportStore.test.ts` — the dev-only gate, the
  fallback-not-override precedence, the foreign-`?relay=` exclusion, and a
  test that greps the chart for `devCertHashHex` (D4's deliberate omission).
- `dev/stack_test.sh` — 22 static assertions: the ops listener's binding, the
  default service set, each profile, the unpublished telemetry read listener,
  `-insecure` appearing once, and what is gitignored.
- `dev/certs_test.sh` — assertions against stub `dig`/`docker`,
  one per §4.6 row plus the two that matter most: the ACME lane **never**
  submits before the record is live on the authoritative nameservers
  (asserted by event ordering, not by timing), and a DNS provider credential
  reaches neither stdout, stderr nor the docker command line.
- The CI job additionally proves the leg nothing else sees: a datagram
  round-trip through the **published** UDP port, the rendered
  `devCertHashHex` equalling the relay's logged `cert_hash_hex`, and the
  certificate surviving `docker compose restart relay`.

**The CI job was verified to fail**, as LD7 requires, by breaking the stack
deliberately in the way that matters most: `dev/config-gen.sh` was changed to
hash the certificate's PEM text instead of its DER, which yields a
plausible-looking 64-hex value that is simply wrong. The stack still came up
healthy and the frontend still received a hash; only the comparison caught it:

```
relay logged:   e49a447a603a78465306841d3f67850e735865a20806f03680eb4503b635b9eb
config.js says: 6f146bcd30ef01cd14e19db2abc03c2b7d7bc263117a33516b98e4f5be283985
::error::the frontend was handed a hash that is not the relay's certificate
```

exit 1. A browser against that same stack fails with "Opening handshake
failed" and the relay logs nothing at all — the shape of failure this
milestone exists to keep out of a contributor's first hour. Reverting the
change turned the step green again.

By hand on macOS (Apple Silicon, Colima + gRPC forwarder):

- **Lane A, end to end.** A headless-Chrome broadcaster minted a code; a
  viewer in a **separate browser profile with empty `localStorage`** decoded
  144 frames at 52 fps over hardware H.264. That profile is the
  `docs/gotchas.md:56` regression test, passing for the first time without a
  hash pasted by hand.
- **`sim`**: a browser viewer joined the pubsim broadcast using the
  `GAWK_PUBSIM_ID=` line from the logs, with no browser broadcaster.
- **`telemetry`**: four real client batches reached the service through the
  app's same-origin `/api/telemetry/` proxy; the read listener refused a
  connection from the host and answered on the compose network.
- **`app-dev`**: an edit under `gawk-app/src` reached the served module in
  about a second, with `app` scaled to zero.
- **Lane A in Firefox** (2026-08-17, Firefox Developer Edition 154, macOS):
  the viewer joined a `sim` broadcast on the **unmodified default lane** — no
  CA installed, no admin password, nothing pasted. `/statusz` for that
  broadcast: `viewersGlobal 1`, five sessions (one primary + four stripe
  legs), `egressDatagramBytes 3,501,522`, `keyframeStreamsSent 45`,
  `dropped 0`, `sendErrors 0`. Firefox implements `serverCertificateHashes`,
  and `connection.ts` already sends it to every browser, so lane A's ECDSA
  P-256 / 14-day dev certificate (`tlsutil/devcert.go`) satisfies it exactly
  as it satisfies Chromium. This is the run that made lane B unnecessary, and
  it is the pass §9 item 2 now asks for.

  Confirmed at the glass, too: the owner watched that viewer and reports the
  picture rendering with *Decoded* climbing as expected, over the fixture's
  `avc1.42C00D` at 30 fps. So the pass covers the whole path — certificate,
  session, all five striped legs, egress, and decode — not just the half a
  relay can see.

- **Lane B** — built, then **withdrawn**; see §6. What was verified before the
  browser test (`mkcert` issuing for `gawk.localhost`/`127.0.0.1`, the chain
  validating, Caddy serving HTTPS with it, `config.js` carrying **no**
  `devCertHashHex`, the relay presenting exactly that certificate over QUIC
  via `gawk-echo -cert-hash`) all passed and none of it mattered: every one of
  those checks is server-side or TCP-side, and the rule that kills the lane is
  applied by the browser on the QUIC path alone. **The lesson is the general
  one**: a certificate lane is not verified until a *browser* has completed a
  WebTransport session over it.

A finding worth its own line: **`--ignore-certificate-errors` and
`--ignore-certificate-errors-spki-list` do not apply to WebTransport.** Chrome
fails the QUIC handshake with `QUIC_TLS_CERTIFICATE_UNKNOWN` regardless. There
is no way to fake trust for a browser test — which is why the withdrawn lane
could not be rescued with a flag, and why `serverCertificateHashes` (lane A) is
the only local mechanism that works.

### 10.4 Still manual — the remaining pass, step by step

One pass is left, and it is blocked on something a session cannot supply: a
DNS record in a zone it does not control. It is not a known defect; it is the
last mile of §9's verification plan. **This is the runbook — follow it
literally, and record the outcome where §10.5 says.**

#### A. Firefox on lane A — **done, 2026-08-17**

This was lane B's pass. It was run on lane A instead, because lane B turned
out to be impossible (§6) and lane A turned out to be sufficient. Result and
numbers are in §10.3; nothing here is outstanding.

For anyone repeating it: `./dev/certs.sh devcert && ./dev/certs.sh renew`, then
`docker compose --profile sim up -d --wait`, take the `GAWK_PUBSIM_ID=` code
from `docker compose logs pubsim`, and open
`http://localhost:8080/#/view/<code>` in Firefox. `Ctrl+Alt+Shift+D` toggles
the stats overlay; *Decoded* must climb. If it connects but never decodes,
that is a `docs/11` codec finding, not a certificate one.

A note for whoever automates this: Firefox has no extension automation here,
but it does not need any — launch it with
`-no-remote -new-instance -profile <tmp>` and `MOZ_LOG="timestamp,nsHttp:5"`,
and `Http3Session::Authenticated` states the certificate verdict, whether the
chain has third-party roots, and whether `serverCertificateHashes` matched.
That log line is what diagnosed the withdrawn lane in minutes.

#### B. Lane C: a live issuance, then a phone on the LAN

The wizard has been driven against Let's Encrypt **staging** on a real domain
through CAA, challenge extraction and authoritative polling; it timed out
waiting for a TXT record nobody added, and correctly reported that nothing had
been submitted. What remains is the second half.

1. **Choose the sub-lane.** `ioio.fi` is on Route 53, so **C-1 is available
   and is the better long-term answer** — it renews without a human:

   ```sh
   # in .env, never committed:
   LEGO_DNS_PROVIDER=route53
   AWS_ACCESS_KEY_ID=...
   AWS_SECRET_ACCESS_KEY=...
   AWS_REGION=eu-north-1
   ```

   Scope the key to that hosted zone. `dev/certs.sh` passes every non-gawk
   variable in `.env` to lego through a 0600 env file and never prints one.
   C-2 (manual TXT) needs no credentials and is what §9 asks for; do that one
   if the point is to rehearse what a person actually experiences.

2. **Rehearse on staging.** `ACME_STAGING=1` in `.env`. Staging certificates
   are untrusted by design — the browser warning is expected and is not the
   thing being tested.

   ```sh
   ACME_TXT_TIMEOUT=3600 ./dev/certs.sh acme
   ```

   The default wait is 900 s; raise it when a human has to go and edit DNS.
   The wizard prints:

   ```
       name   _acme-challenge.dev.ioio.fi
       type   TXT
       value  <43 characters>
   ```

   **The value changes on every run** — take it from the run that is waiting,
   not from an earlier one. In the Route 53 console the value must be quoted:
   `"<value>"`. The wizard polls all four `awsdns` nameservers every 10 s and
   proceeds by itself; leave it running.

3. **Expect**, in order: `✓ seen on every authoritative nameserver after Ns —
   validating`, lego's issuance lines, `→ certs/cert.pem, certs/key.pem`, the
   A records to add, and — in C-2 — the warning that this sub-lane cannot
   renew itself.

4. **Add the A records** (they point at loopback; nothing is exposed):

   ```
   gawk.dev.ioio.fi   A   127.0.0.1
   lan.dev.ioio.fi    A   <this machine's LAN address>
   ```

   If `gawk.dev.ioio.fi` then does not resolve locally while the authoritative
   server answers, the wizard says so and prints the `/etc/hosts` line — that
   is DNS-rebinding protection in the local resolver, not a mistake.

5. **Run it.** `docker compose up -d --wait`, then
   `https://gawk.dev.ioio.fi:8443`: **Start a stream**, accept the terms
   modal, share any window, and join `#/view/<code>` in a second window with
   the stats overlay (`Ctrl+Alt+Shift+D`) showing *Decoded* climbing.

6. **The pass that justifies the lane**: from a **phone on the same LAN**, with
   nothing installed on it, open `https://lan.dev.ioio.fi:8443/#/view/<code>`.
   That needs `BIND_ADDR=0.0.0.0` — `./dev/certs.sh` sets it when you give it a
   LAN address — and a **trusted** certificate, so re-run with
   `ACME_STAGING=0` first. Production rate limits: 50 certificates per
   registered domain per week, 5 duplicates of the same name set; the wizard
   explains both if you meet them.

7. **Sub-lane note for whoever renews it later**: C-2 cannot renew
   automatically — there is no DNS API to answer the next challenge with. If
   this certificate is meant to survive, use C-1.

### 10.5 Where to record the outcome

When the lane C pass completes, update **§10.3** with what was observed (codec,
fps, device), strike its entry from §10.4, and move the `ROADMAP.md` R38 row
from 🔧 to ✅ — it is the only thing still holding that row. If a pass *fails*,
the finding belongs in §10.3 too — and, if it is a browser behaviour rather
than a stack one, in `docs/gotchas.md` under the cross-browser section.

The Firefox pass is the worked example of that last clause: it failed, the
cause was a browser behaviour, and it produced §6's rejected-alternative entry,
a `docs/gotchas.md` row, and the withdrawal of D5.
