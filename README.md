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
| `gawk-app/` | React SPA (Vite + TypeScript + Zustand). Pages: `#/view` (default), `#/broadcast`, `#/loopback` (no-network diagnostic) |
| `gawk-server/` | Go relay: WebTransport endpoint, pub/sub hub, dev-cert tooling. See its [README](gawk-server/README.md) |
| `docs/` | Per-milestone design notes and gotchas (`01`–`03`), plus [`implementation-tasks.md`](docs/implementation-tasks.md) — the server design + task breakdown and current progress |

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

## Status

Milestones (detail in [`docs/implementation-tasks.md`](docs/implementation-tasks.md)):

1. ✅ Local loopback (capture → encode → decode in one tab) — `docs/01`
2. ✅ WebTransport hello-world: TLS, dev certs, echo — `docs/02`
3. ✅ Single-client end-to-end: hub, publish/subscribe, frontend transport — `docs/03`
4. ⬜ Fan-out hardening (multi-subscriber, stats endpoint)
5. ⬜ Resilience + deployment (Docker, k8s, cert-manager)

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
  `cert_hash_hex` after each restart; a stale hash fails identically.

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
