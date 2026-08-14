# R38 — Local development stack (docs/41)

**Status**: designed 2026-08-14; not started. Chunks **LD1–LD7** (`LD` =
Local Dev; two-letter prefix per the R21+ convention — collides with
nothing). Phased: **A** (compose skeleton + lane A, the zero-prerequisite
default) → **B** (the trusted-certificate lanes: mkcert, then guided ACME)
→ **C** (profiles, CI smoke, docs). Phase A is the whole product claim for a
contributor with nothing but Docker installed; B is what unlocks Firefox and
second devices; C is what stops the stack rotting.

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
- **`serverCertificateHashes` is a Chromium mechanism.** The whole documented
  certificate path is written in Chromium's terms (`docs/gotchas.md:93`).
  Firefox is a supported *viewer* browser in production, and there is no
  evidence anyone has ever joined a local dev relay from it.
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
behaviour is only observable with a browser, a relay and a publisher running
together — and today assembling that is manual enough that it does not
happen casually. Making it one command is how "let me check that locally"
becomes a normal act rather than a small project.

## 2. Decisions (locked with the owner, 2026-08-14)

| # | Decision | Rationale |
|---|----------|-----------|
| D1 | **One compose stack; the certificate is a pluggable input selected by `CERT_MODE`.** Every service downstream of `certs/` is byte-identical across lanes. | The three lanes differ *only* in cert provenance. Modelling them as three stacks would triple the surface that can rot; modelling them as one input keeps the interesting variation in one place and means the relay/app/telemetry wiring is exercised identically by all three. |
| D2 | **Lane A (self-signed, persisted, hash auto-injected) is the default.** No prerequisites beyond Docker, works offline, no admin prompt, no account, no domain. | The default has to serve the contributor who cloned the repo to fix one thing. Anything requiring a trust-store write, a domain, or an internet round-trip is a prerequisite that some contributor will not clear, and a default nobody can run is not a default. |
| D3 | **The dev certificate is persisted to a volume, not regenerated per start.** `-dev-cert` combined with `-cert-file`/`-key-file` means "generate into these paths if absent, otherwise load them". | This is the single highest-value fix in the milestone and it is nearly free. It kills the re-paste loop (`docs/gotchas.md:99`), and it moves local dev onto the **file-backed** certificate path production uses instead of the dev-only in-memory one (`docs/gotchas.md:102`). It also helps the non-Docker path, which is where most relay work will still happen. |
| D4 | **New dev-only runtime config key `devCertHashHex`, read only when `isDevEnvironment()`** (`gawk-app/src/config.ts:134`). It is **not** a Helm chart value and the `gawk-app` ConfigMap does not render it. | This is what closes the `docs/gotchas.md:56` hole: with the hash arriving via `/config.js` rather than `localStorage`, the bare `#/view/{id}` route works in a fresh profile, in incognito, and on a second machine. Keeping it out of the chart is deliberate — a production deployment that wants this key has misconfigured its TLS, and offering the knob would invite exactly that. |
| D5 | **Lane B is mkcert** — a local CA, one `mkcert -install`, certs for `gawk.localhost` plus the machine's LAN IP. | It is the cheapest way to a genuinely *trusted* certificate: ordinary chain validation, no hashes, no 14-day cap, and it writes to the NSS store so **Firefox works**. Chrome exempts locally-installed roots from both CT enforcement and the 398-day leaf limit, so there is no hidden trap. |
| D6 | **Lane C is a guided ACME wizard against a domain the developer controls** — DNS-01, Let's Encrypt, one **wildcard**, `lego`. | It is the only lane where the private key is genuinely the developer's, and the only one where another device joins with nothing installed. It doubles as a rehearsal of the production path (`docs/self-hosting.md` §3 requires the same CA and the same challenge type), so the person most likely to need it is already the person it teaches. |
| D7 | **Lane C issues one wildcard (`*.dev.example.com`), then the developer only edits A records.** | Issue-once is what makes the lane tolerable. A new device, a new LAN IP, a DHCP move — all become DNS edits against a certificate that already covers them, with no second ACME round-trip. Wildcards require DNS-01 anyway, which this lane already uses. |
| D8 | **DNS-01 proves control of the *name*, not reachability of the *host*.** The A record points at `127.0.0.1` throughout; nothing about the dev machine is ever exposed. This is stated prominently in the wizard and the docs. | It is the misconception that stops people attempting this lane, and it is one sentence to remove. Worth a decision row precisely because the docs must not bury it. |
| D9 | **The wizard verifies rather than instructs.** It polls the authoritative nameservers for the TXT record itself and only submits to Let's Encrypt once it can see it; it checks CAA before spending an issuance; it resolves the finished name and detects DNS-rebinding blocks; it prints the expiry date. | Anyone can link the certbot docs. The value is in naming the failure — and every failure in this lane is obscure. Submitting before propagation is the classic one, and Let's Encrypt rate-limits failed validations at 5/hour, so an impatient user locks themselves out with no idea why. `lego` checks authoritative NS by default; the wizard's job is to surface that as "still waiting…" rather than a stall. |
| D10 | **`lego` runs as an upstream container invoked by a shell script; no new Go binary, and nothing is added to `gawk-server`'s module graph.** | CI gates every dependency against a license allowlist and regenerates per-artifact `THIRD-PARTY-NOTICES.md` (`CONTRIBUTING.md` §Adding a dependency). Importing an ACME library into the relay's module to serve a development convenience would put it in the relay's notices and its supply chain forever. A container at arm's length costs nothing and brings ~100 DNS provider plugins for free. |
| D11 | **Compose profiles carry the optional pieces**: `sim` (a `gawk-pubsim` publisher), `telemetry` (`gawk-telemetry`), `app-dev` (Vite with HMR over a bind mount). The default `up` is relay + app only. | A default that starts five containers is a default people stop running. Each profile answers a specific question — "I want a live broadcast without screen-sharing", "I'm working on telemetry", "I'm iterating on the frontend" — and costs nothing when unused. |
| D12 | **The compose stack is a development and evaluation tool, never a supported deployment path.** Helm remains the only supported way to run gawk for real, and the compose file says so in its header. | The moment a compose file looks deployable, someone deploys it, and then it needs the operational properties the charts have and it does not (draining, probes, cluster mode, cert renewal, capacity limits). Naming the boundary in the artifact itself is cheaper than defending it later. |
| D13 | **One hostname serves both the app and the relay**, on different ports (app `:8443`, relay `:4433/udp`). | Certificates cover names, not ports, so a second name buys nothing and costs a second DNS record, a second SAN and a second thing to explain. It also keeps `-allowed-origins` a single value. |
| D14 | **The relay's ops listener binds to `127.0.0.1` in every lane, including when the stack is exposed to the LAN.** | `CLAUDE.md` is unambiguous that the ops endpoint is never public. A LAN-exposed dev stack is the exact situation where a convenience binding becomes a habit; the compose file should not be where that habit is learned. |

## 3. Where it plugs into the existing code (grounded)

Almost all of the machinery exists; this milestone assembles it.

| Piece | Where it is today | What R38 does with it |
|---|---|---|
| Dev cert generation | `gawk-server/internal/tlsutil/devcert.go` (ECDSA P-256, `MaxDevCertValidity` 14 d, `CertHashHex`) | Unchanged. Reused via D3's flag combination. |
| Standalone cert writer | `gawk-server/cmd/gawk-devcert` (`-out`, `-hosts`, `-days`) | Stays the host-side tool; the compose stack does not need it once D3 lands. |
| Cert flags | `-dev-cert` / `-dev-cert-hosts` / `-cert-file` / `-key-file` (`internal/config/config.go:238–240`) | D3 makes `-dev-cert` + `-cert-file`/`-key-file` a valid, meaningful combination. |
| Runtime frontend config | `gawk-app/public/config.js` (shipped empty) + `src/config.ts` getters | Gains `devCertHashHex` (D4). The compose stack renders a `config.js` over the shipped one — the same mechanism the Helm chart uses via its ConfigMap. |
| Relay URL default | `transportStore.ts:63–65` (config.js → hostname check → `https://localhost:4433`) | Unchanged; the rendered `config.js` sets `relayUrl` explicitly so no lane depends on the fallback. |
| Images | `gawk-server/deploy/Dockerfile` (distroless, **no shell**, `gawk-echo` included), `gawk-app/deploy/Dockerfile` (nginx-unprivileged on 8080), `gawk-telemetry/deploy/Dockerfile` (cgo + `-tags duckdb`, repo-root build context) | Reused as-is. Note the relay image has no shell and does not contain `gawk-devcert` — which is *why* D3 puts generation in the server rather than in an init container. |
| Synthetic publisher | `gawk-broadcast/cmd/gawk-pubsim`, already used by `e2e/run.mjs` | Becomes the `sim` profile (D11). |
| Health check | `gawk-echo` ships inside the relay image and is already the chart's exec probe | Becomes the compose healthcheck, so `depends_on: service_healthy` works without a shell. |
| Origin allowlist | `-allowed-origins` | Set to the app's origin in every lane, so the compose stack exercises the allowlist rather than the allow-all dev default. |

## 4. Design

### 4.1 One stack, three lanes

```
docker compose up
   │
   ├── certs/          ← the pluggable input (CERT_MODE=devcert | mkcert | acme)
   │     cert.pem, key.pem
   │
   ├── relay      gawk-server -cert-file /certs/cert.pem -key-file /certs/key.pem
   │              -allowed-origins <app origin>   :4433/udp   ops on 127.0.0.1:2112
   │
   ├── config-gen renders /config.js  { relayUrl, devCertHashHex?, requirePublishSecret: false }
   │
   └── app        nginx (gawk-app image) with the rendered config.js mounted over the shipped one
```

`config-gen` is a tiny alpine service that waits for `certs/cert.pem`, and in
lane A computes the hash the browser needs — `openssl x509 -outform DER |
sha256sum`, which is exactly what `tlsutil.CertHashHex` computes. In lanes B
and C the certificate is trusted, so no hash is emitted and the key is
absent from `config.js` entirely.

Ordering: the relay creates the certificate (D3), so `config-gen` depends on
the relay being healthy, and `app` depends on `config-gen` completing.

### 4.2 Lane A — self-signed, persisted, auto-injected (`CERT_MODE=devcert`)

The default. The relay generates a cert into the `certs` volume on first
start and loads it on every subsequent one; `config-gen` renders its hash
into `config.js`; the app is served over plain HTTP on `http://localhost:8080`
(a secure context, so `getDisplayMedia` and WebCodecs are available).

What this fixes: the re-paste loop is gone (D3), and the bare viewer route
works without seeding `localStorage` (D4). What it does not fix: Chromium
only, and loopback only. The certificate still expires after at most 14 days
(`MaxDevCertValidity`), so the stack warns when it is within 3 days of expiry
and `docker compose run --rm certs renew` regenerates it.

### 4.3 Lane B — mkcert (`CERT_MODE=mkcert`)

The developer runs `mkcert -install` once (one admin prompt) and the stack
issues `gawk.localhost` plus the detected LAN IP. Caddy serves the app over
HTTPS on `:8443` with the same certificate; the relay loads it from the same
volume.

Because the certificate is genuinely trusted, WebTransport does ordinary
chain validation: no hashes, no 14-day cap, no `devCertHashHex`. Firefox
works. Other devices work only if the mkcert root is installed on them,
which is the lane's boundary.

### 4.4 Lane C — guided ACME on your own domain (`CERT_MODE=acme`)

Prerequisite: the developer controls a domain. The wizard (`dev/certs.sh`)
walks the rest.

**Lane C-1, DNS provider API token.** If the registrar is one `lego`
supports, a scoped token is all it needs: issuance and renewal are automatic
and the human never sees a TXT record. The wizard pushes toward this.

**Lane C-2, manual TXT.** For registrars with no API. This is the guided
path:

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

Renewal: automatic in C-1; in C-2 the certificate cannot auto-renew, so the
wizard prints the expiry and the stack warns under 30 days. That limitation
is stated up front rather than discovered as a broken stack in 90 days.

### 4.5 Profiles and the inner loop

| Profile | Adds | For |
|---|---|---|
| *(default)* | relay + app | "does it work" |
| `sim` | `gawk-pubsim` publishing a synthetic broadcast | Testing the viewer with no second machine and no screen-share |
| `telemetry` | `gawk-telemetry` + the same-origin ingest path on the app's server | Working on R28/R31 surfaces |
| `app-dev` | Vite dev server with a bind-mounted `src/`, replacing the nginx app | Frontend iteration with HMR |

**The compose stack is not a replacement for the editor loop, and the docs
say so.** Relay work stays `go run ./cmd/gawk-server` on the host — now
pointed at the same persisted `certs/` directory, so the browser side needs
no reconfiguration when a developer switches between the container and the
host binary. That interchangeability is a direct consequence of D3 and is
worth stating in the README: the stack and the host toolchain are two ways
to fill the same slot, not two worlds.

### 4.6 The failure register

What the wizard and the stack detect, and what they say. This table is the
acceptance criteria for LD4 as much as it is documentation.

| Failure | Detection | Message |
|---|---|---|
| DNS rebinding protection (resolver refuses to return `127.0.0.1` or RFC1918 from public DNS) | Resolve the finished name; empty or NXDOMAIN with a correct upstream record | Print the exact `/etc/hosts` line to add |
| CAA forbids Let's Encrypt | Query CAA on the registrable domain before issuance | Name the offending record; do not spend an issuance |
| TXT not yet propagated | Poll authoritative NS | "still waiting…" with elapsed time — never submit early |
| Failed-validation rate limit | ACME error classification | Explain the 5/hour limit and when it resets |
| Issuance rate limit | ACME error classification | Explain 50/domain/week and 5 duplicates/week |
| Cert does not cover the configured `relayUrl` | Compare SANs against the rendered `config.js` | Refuse to start rather than fail opaquely in the browser |
| Cert near expiry | On every `up` | Warn under 3 days (lane A) / 30 days (lanes B, C) |
| Clock skew | Compare container time to `NotBefore` | Name it — a backdated cert failing validation is otherwise inscrutable |

## 5. Security considerations

- **`certs/` and `.env` are gitignored, and the wizard refuses to write
  anywhere else.** A private key in a public repository is not a mistake with
  a warning attached: CAs must revoke keys reported as compromised within 24
  hours, so a committed key becomes a broken stack for everyone.
- **DNS API tokens are zone-scoped where the provider supports it**, live in
  `.env`, and are never echoed by the wizard.
- **The ops listener stays on `127.0.0.1`** in every lane (D14).
- **`-allowed-origins` is set, not left open.** The dev default is allow-all;
  the compose stack sets the app's origin explicitly, so the allowlist is
  exercised locally instead of first meeting a developer in production.
- **Telemetry stays off unless its profile is selected**, matching the
  default-off posture everywhere else.
- **LAN exposure is opt-in.** Ports bind to `127.0.0.1` by default; reaching
  the stack from another device requires setting the bind address, and the
  docs say what that exposes.

## 6. Rejected alternatives

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
  worse than one that asks for a single `mkcert -install`. They remain
  reasonable things for an individual to use for a one-off phone test, and
  the docs may mention them as such — but nothing in the repo depends on
  them.
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
- Windows/macOS-specific host networking beyond what Docker Desktop provides.
- WebKit/Safari — it cannot join a broadcast at all today (`BUGS.md`), so no
  lane is designed around it.
- A `gawk`-operated DNS or certificate service of any kind.

## 8. Chunks & acceptance criteria

Phase A — the zero-prerequisite default:

| Chunk | Scope | Acceptance criteria |
|---|---|---|
| LD1 | Relay: `-dev-cert` + `-cert-file`/`-key-file` = generate-if-absent, load-otherwise (D3); expiry warning at startup | Go test: absent paths ⇒ cert written to both paths with 0600 on the key, ECDSA P-256, span ≤ `MaxDevCertValidity`; present paths ⇒ loaded unchanged, **no regeneration** (hash stable across restarts — asserted, this is the gotcha the milestone exists to kill); `-cert-file` without `-dev-cert` and a missing file still **fails fast** (a production deploy must never self-sign silently); partial state (cert present, key absent) is an error, not a half-generation; startup logs the expiry and warns within 3 days. |
| LD2 | `gawk-app`: `devCertHashHex` runtime key, dev-gated (D4) | Unit: key honoured only when `isDevEnvironment()`; ignored on a non-loopback production origin (asserted); empty string counts as unset, matching every other getter; hash reaches the viewer transport without any `localStorage` seeding. **`configmap.yaml` does not render the key** (asserted — D4's deliberate omission, so a later "add every config key" sweep cannot quietly add it). |
| LD3 | `docker-compose.yml` + `config-gen` + lane A end to end | `docker compose up` on a machine with only Docker produces a working broadcast: relay healthy via the in-image `gawk-echo` probe, `config.js` carrying `relayUrl` + `devCertHashHex`, `#/broadcast` mints a code, `#/view/<code>` renders frames **in a browser profile that has never visited `#/broadcast`** (the `docs/gotchas.md:56` regression test). Ops port bound to `127.0.0.1` (asserted). Compose header states D12. |

Phase B — trusted certificates:

| Chunk | Scope | Acceptance criteria |
|---|---|---|
| LD4 | `dev/certs.sh`: `CERT_MODE` dispatch, lane B (mkcert), lane C (`lego` container, C-1 and C-2), and the §4.6 failure register | Each row of §4.6 has a test or a documented manual reproduction; TXT polling never submits before the record is visible on the authoritative NS (asserted against a stub resolver); CAA check precedes issuance; SAN/`relayUrl` mismatch refuses to start; secrets never echoed; `certs/` + `.env` gitignored (asserted by a test that greps the repo, not by hope). Lane C runs against the Let's Encrypt **staging** endpoint in tests. |
| LD5 | Lanes B and C wired into the stack: Caddy HTTPS front on `:8443`, LAN bind opt-in, no `devCertHashHex` emitted when the cert is trusted | Lane B: a broadcast completes in **Firefox** as viewer (the claim that justifies the lane). Lane C: a second device on the LAN joins with nothing installed. Both: `config.js` contains no `devCertHashHex` (asserted). Default bind stays `127.0.0.1`. |

Phase C — profiles, gates, docs:

| Chunk | Scope | Acceptance criteria |
|---|---|---|
| LD6 | Profiles `sim`, `telemetry`, `app-dev` (D11) | `sim`: a viewer joins the pubsim broadcast with no browser broadcaster. `telemetry`: ingest reachable same-origin, service off by default without the profile (asserted). `app-dev`: an edit to `gawk-app/src` is reflected without a rebuild. Default `up` starts relay + app only (asserted). |
| LD7 | CI smoke + docs: a job that brings lane A up and asserts a datagram round-trip via `gawk-echo`; README quickstart rewritten; `docs/self-hosting.md` cross-reference; `docs/gotchas.md` sync | CI job green and **fails when the stack is broken** (verified by breaking it deliberately once). README quickstart leads with `docker compose up` and keeps the host-toolchain path as the relay inner loop. `docs/gotchas.md:99` and `:56` updated to describe the new default rather than the old workaround. The lane comparison table lands in the docs, not only here. |

## 9. Verification

Automated gates are per-chunk above. The milestone-closing manual pass is
three fresh-clone runs, each on a machine that has *only* the lane's stated
prerequisite:

1. **Lane A, Docker only, offline.** `docker compose up`, broadcast in
   Chromium, join from a second tab and from a fresh incognito profile.
   Restart the stack; the viewer still joins with **nothing re-pasted**.
2. **Lane B, plus mkcert.** The same, with the viewer in **Firefox**.
3. **Lane C, plus a domain.** The wizard end to end on a registrar without an
   API (the C-2 path, which is the one that needs the hand-holding), then a
   phone on the LAN joining `https://lan.dev.<domain>:8443` with nothing
   installed.

The standing claim across all three: a contributor who has never seen this
repository gets from `git clone` to a rendered frame without reading a
design doc, and a self-hoster who completes lane C has already performed the
DNS-01 issuance that `docs/self-hosting.md` §3 asks of them.
