# gawk

Self-hosted low-latency game streaming for a small private group (~15
friends) watching one broadcaster's gaming PC. Homelab-hosted, 1 Gbps
symmetric uplink, targeting sub-500 ms glass-to-glass latency.

The primary motivation is **learning**: this deliberately explores newer,
lower-level browser tech — WebTransport datagrams + WebCodecs — instead of
the mature WebRTC/SFU route (OvenMediaEngine et al.), which would have been
the safe choice.

## How it works

```
Broadcaster (Chrome)                relay (Go)                Viewers (Chrome, ≤15)
getDisplayMedia                                              
  → VideoEncoder (WebCodecs)      WebTransport/QUIC            reassemble frames
  → chunk into ≤1200 B datagrams ──→ /publish                    → VideoDecoder
     (custom wire format)             fan-out hub ──→ /subscribe ──→ canvas
```

- **Transport is QUIC datagrams**, not streams: a lost chunk means a dropped
  frame, never stalled playback.
- The relay is a **byte forwarder**. It parses headers only to observe: it
  caches the latest decoder config and last complete keyframe, and primes
  every newly-joined viewer with them — late joiners get a picture
  immediately instead of waiting for the next keyframe.
- One frozen **wire format** (20-byte VideoChunk header + DecoderConfig
  message, big-endian) implemented twice — Go (`gawk-server/internal/wire`)
  and TypeScript (`gawk-app/src/transport/wire.ts`) — kept byte-compatible
  by shared golden test vectors.
- Codec is **negotiated**, not fixed: H.264 hardware realtime is the happy
  path, with VP9/VP8 fallbacks probed through a cascade of encoder configs.

## Repository layout

| Path | What |
|------|------|
| `ROADMAP.md` | High-level roadmap for post-v0.5 work (R1–R6), with ordering rationale and per-item scope sketches |
| `gawk-app/` | React SPA (Vite + TypeScript + Zustand). Pages: `#/view` (default), `#/broadcast`, `#/loopback` (no-network diagnostic). `deploy/`: Dockerfile + Helm chart |
| `gawk-server/` | Go relay: WebTransport endpoint, pub/sub hub, dev-cert tooling. `deploy/`: Dockerfile + Helm chart. See its [README](gawk-server/README.md) |
| `docs/` | Per-milestone design notes and gotchas (`01`–`05`), plus [`implementation-tasks.md`](docs/implementation-tasks.md) — the server design + task breakdown and current progress |
| `.github/workflows/` | CI (test/lint/build on PR + main) and release automation (release-please → GHCR images + OCI Helm charts, versions from conventional commits) |

## Quickstart (local dev)

Prerequisites: Go ≥ 1.23 (the toolchain auto-downloads what the module
needs), Node ≥ 22, Chromium-based browser.

```sh
# 1. Relay with an ephemeral dev certificate
cd gawk-server
go run ./cmd/gawk-server -dev-cert
#    → copy cert_hash_hex from the startup log

# 2. Frontend
cd gawk-app
npm install
npm run dev            # http://localhost:5173
```

Open `http://localhost:5173/#/broadcast`, paste the cert hash into the "Dev
cert hash" field, Start Broadcast, pick a screen. Open `#/view` in another
tab (same hash), Start Watching. No Chrome flags needed — the app passes
the hash via `serverCertificateHashes`.

To probe a running server without a browser:

```sh
cd gawk-server
go run ./cmd/gawk-echo -cert-hash <cert_hash_hex>   # datagram RTT test
```

### Tests

```sh
cd gawk-server && go vet ./... && CGO_ENABLED=1 go test -race ./...
cd gawk-app    && npm test && npm run lint && npm run build
```

### Containers & deployment

Both components build to images (`<component>/deploy/Dockerfile`) and ship
as Helm charts (`<component>/deploy/charts/<component>/`) published to GHCR by CI on
release — chart version, `appVersion` and image tag always match. Homelab
deployment is automated cluster-side: whenever a new version is released,
it is deployed to the homelab automatically (CI itself stays publish-only —
no cluster credentials in GitHub). Initial install runbook, GHCR pull-secret
setup and the release flow:
[`docs/05-resilience-deploy.md`](docs/05-resilience-deploy.md).

```sh
cd gawk-server && docker build -f deploy/Dockerfile -t gawk-server:dev .
cd gawk-app    && docker build -f deploy/Dockerfile -t gawk-app:dev .
```

## Status

Milestones (detail in [`docs/implementation-tasks.md`](docs/implementation-tasks.md)):

1. ✅ Local loopback (capture → encode → decode in one tab) — `docs/01`
2. ✅ WebTransport hello-world: TLS, dev certs, echo — `docs/02`
3. ✅ Single-client end-to-end: hub, publish/subscribe, frontend transport — `docs/03`
4. ✅ Fan-out hardening: multi-subscriber, restart-safe caches, `/statusz` — `docs/04`
5. ✅ Resilience + deployment: keepalive, viewer auto-reconnect, Docker,
   Helm charts, release-please CI — `docs/05` (v0.5.0 released to GHCR
   2026-07-12; homelab install + automated deploy-on-release and the manual
   end-to-end browser verify completed 2026-07-12)

What comes next (multi-broadcaster, hardening, quality pickers, production
UI, …) is laid out in [`ROADMAP.md`](ROADMAP.md).

## Important gotchas

Hard-won; each cost real debugging time. Details live in the linked docs.

**Environment**

- **WSL2: Chrome must run *inside* WSL2** alongside the server. Windows
  Chrome cannot complete a QUIC handshake to a server in WSL2 NAT mode
  (localhost forwarding is TCP-only; via the NAT IP Chrome idle-times-out
  even though CLI clients work). ([docs/02](docs/02-webtransport-hello.md))
- **`go test -race` needs `CGO_ENABLED=1`** — this environment defaults to 0.
- **npm may silently skip native bindings** (rolldown/vite 8, oxlint) due to
  [npm/cli#4828](https://github.com/npm/cli/issues/4828); build/lint/test
  then die with "Cannot find native binding" and the documented
  delete-and-reinstall remedy does not help. Workaround:
  `npm install @rolldown/binding-linux-x64-gnu @oxlint/binding-linux-x64-gnu --no-save`.
  ([docs/03](docs/03-single-client-e2e.md))
- **UDP buffers**: quic-go wants ~7 MiB receive buffers; Linux defaults to
  208 KiB. `sysctl -w net.core.rmem_max=7500000 net.core.wmem_max=7500000`
  for real streaming loads.

**Certificates (Chromium `serverCertificateHashes` rules)**

- Dev cert must be **ECDSA P-256** and its **total validity span ≤ 14 days,
  clock-skew backdate included** — a 14 d + 1 h cert is rejected with an
  opaque `CERTIFICATE_VERIFY_FAILED (certificate unknown)`. The Go probe
  won't catch this; only Chrome enforces it. ([docs/02](docs/02-webtransport-hello.md))
- The dev cert is **regenerated on every server start** — re-paste the fresh
  `cert_hash_hex` after each restart; a stale hash fails identically. This
  also means the viewer's **auto-reconnect can never heal a relay restart in
  `-dev-cert` mode** — test that with a file-based dev cert
  (`gawk-devcert -out certs`). ([docs/05](docs/05-resilience-deploy.md))

**webtransport-go (v0.11.x) — all fail at runtime, not compile time**

- ALPN must be added manually: wrap your TLS config in
  `http3.ConfigureTLSConfig(...)`, the library won't.
- WebTransport SETTINGS must be enabled manually:
  `webtransport.ConfigureHTTP3Server(s.H3)`, or clients reject with
  "server didn't enable WebTransport".
- Go *clients* need `EnableStreamResetPartialDelivery: true` in their
  `quic.Config`. ([docs/02](docs/02-webtransport-hello.md))

**Media pipeline**

- **Trust the actual `VideoFrame`, never `MediaStreamTrack.getSettings()`** —
  Chrome has been observed reporting one resolution while delivering frames
  at another. Configure encoders from the first real frame.
  ([docs/01](docs/01-loopback-test.md))
- H.264 needs **even dimensions**; `getDisplayMedia` can hand out odd sizes
  on some DPI combos — round down to even before configuring the encoder.
- Constrain only `width` in `getDisplayMedia`; constraining both dimensions
  makes Chrome pillarbox ultrawide sources.
- WebCodecs: the **first chunk after `VideoDecoder.configure()` must be a
  keyframe**, and decoder configure/decode calls must be strictly ordered
  (promise-chain them).
- **~1200-byte safe datagram payload** drives the chunking design — don't
  assume larger datagrams survive the path.
- **Raising the QUIC idle timeout does not keep idle viewers alive** — the
  effective timeout is the min of both endpoints' advertised values and
  browsers advertise ~30s. The server-side keepalive (`-keepalive-period`)
  is the mechanism. ([docs/05](docs/05-resilience-deploy.md))

**CI / deployment**

- **Tags created with `GITHUB_TOKEN` don't trigger workflows** — publish
  jobs must chain off release-please outputs in the same workflow, never
  `on: push: tags`. ([docs/05](docs/05-resilience-deploy.md))
- **release-please separate PRs self-conflict in a monorepo** (all release
  PRs edit the shared manifest, and the bot doesn't refresh the stranded
  one) — use one combined release PR (`"separate-pull-requests": false`);
  tags/releases/publishes stay per-component.
  ([docs/05](docs/05-resilience-deploy.md))
- **GHCR**: refs are lowercase-only (`tuhis`), and cluster pulls need a
  **classic** PAT with `read:packages` (fine-grained PATs don't cover GHCR).
- **The relay is HTTP/3-only (no TCP listener)** — kubelet probes must exec
  the bundled `gawk-echo` binary; `httpGet`/`tcpSocket` can never succeed.
- **That exec probe crash-loops the pod once `allowedOrigins` is set** —
  `gawk-echo` sends no `Origin` header, so `CheckOrigin` rejected its own
  probe as soon as production configured a real origin (dev's empty
  allowlist never hit it). `CheckOrigin` now exempts loopback `RemoteAddr`.
  ([docs/05](docs/05-resilience-deploy.md))
- **release-please `extra-files` paths are package-relative** — a repo-
  relative path silently leaves Chart.yaml unbumped.
