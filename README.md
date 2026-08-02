# gawk

Self-hosted, low-latency live game streaming for the browser. A broadcaster
shares their screen; viewers join with a 6-character code and watch at
**sub-500 ms glass-to-glass latency** (≈50 ms measured on a hardware
encode/decode path) — no accounts, no plugins, no native app.

Built on **WebTransport datagrams + WebCodecs** with a custom Go relay that
**scales horizontally**: a self-federating fleet of relay pods carries
hundreds of concurrent broadcasts and ~1,000 viewers on a single hot
broadcast behind one UDP load balancer.

**[gawk.ioio.fi](https://gawk.ioio.fi)** is gawk — start a stream, share the
code, done. This repository is that same stack, open-sourced, so you can run
it yourself: two Helm charts, two images, and a
**[self-hosting guide](docs/self-hosting.md)** that covers DNS, TLS, and
single-node through multi-pod cluster mode.

WebTransport + WebCodecs is a deliberate choice over the mature WebRTC/SFU
route (OvenMediaEngine et al.): a lower-level path that trades some ready-made
robustness for a leaner, self-owned pipeline and a real exploration of newer
browser transport and codec APIs.

## Quick links

| | |
|---|---|
| **Run it yourself** | [`docs/self-hosting.md`](docs/self-hosting.md) — prerequisites, TLS, single-node and cluster mode, verification, upgrades |
| **Local dev in 5 minutes** | [Quickstart](#quickstart-local-dev) below |
| **How it works** | [below](#how-it-works), then [`docs/`](docs/README.md) — 40 design docs, one per milestone |
| **What's built, what's next** | [`ROADMAP.md`](ROADMAP.md) |
| **Known bugs** | [`BUGS.md`](BUGS.md) |
| **Gotchas that cost real debugging time** | [`docs/gotchas.md`](docs/gotchas.md) |
| **Contributing** | [`CONTRIBUTING.md`](CONTRIBUTING.md) · [`CODE-REVIEW.md`](CODE-REVIEW.md) |
| **Security** | [`SECURITY.md`](SECURITY.md) |

## How it works

```
Broadcaster (Chrome)                relay (Go)                Viewers (Chrome, many)
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
  message, big-endian) implemented four times — Go (`gawk-server/wire`),
  TypeScript (`gawk-app/src/transport/wire.ts`), and the two native
  broadcasters — kept byte-compatible by shared golden test vectors.
- Codec is **negotiated**, not fixed: H.264 hardware realtime is the happy
  path, with VP9/VP8 fallbacks probed through a cascade of encoder configs.
- **Audio** rides the same transport as Opus (48 kHz stereo, one packet per
  datagram), synchronised video-master off the shared capture clock.

Beyond the basics: the relay federates across pods so one broadcast's
audience can span nodes ([`docs/22`](docs/22-relay-scale-out.md)), viewers on
lossy links can opt into reliable carrier streams and a relay-side DVR ring
([`docs/24`](docs/24-viewer-network-resilience.md),
[`docs/26`](docs/26-relay-dvr-buffer.md)), and forward parity protects
datagram deltas against loss
([`docs/34`](docs/34-live-edge-forward-parity.md)).

## Components

| Path | What it is | State |
|---|---|---|
| [`gawk-server/`](gawk-server/) | The Go relay — WebTransport endpoint, pub/sub hub, cluster-mode federation. Its `wire/` package is public so other components import rather than mirror it. Image + Helm chart. | Production; runs the reference deployment |
| [`gawk-app/`](gawk-app/) | React SPA — landing/join, broadcaster, viewer. Diagnostics live frozen under `#/debug/*`. Image + Helm chart. | Production; runs the reference deployment |
| [`gawk-broadcast/`](gawk-broadcast/) | Native **Linux** broadcaster (Go): Gio GUI + CLI, hardware encode via XDG portal + GStreamer — which the browser structurally cannot do on Linux. A binary you run, not a deployed component. | Working; verified on real hardware |
| [`gawk-broadcast-windows/`](gawk-broadcast-windows/) | Native **Windows** broadcaster (Rust): Windows.Graphics.Capture + Media Foundation + Slint, single static EXE. | Implemented; **on-hardware acceptance pass outstanding** |
| [`gawk-telemetry/`](gawk-telemetry/) | Optional per-session diagnostics service — ingest, 30-day history, a diagnosis UI and an MCP surface. **Off by default everywhere.** Image + Helm chart. | Working; optional |
| [`e2e/`](e2e/) | Browser E2E harness: headless Chrome decoding real relayed frames, plus the kind cluster tier. | Green on every PR |
| [`docs/`](docs/README.md) | 40 numbered design docs, one per milestone — decisions, rejected alternatives, acceptance criteria. | — |
| [`tools/`](tools/) | Repo tooling (third-party licence notice generation). | — |

**Maturity, honestly.** The relay and web app run a real deployment daily and
are the most exercised parts. Several later milestones are implemented with
automated gates green but a manual on-hardware pass still outstanding —
[`ROADMAP.md`](ROADMAP.md) marks each one, and [`BUGS.md`](BUGS.md) lists
confirmed open defects rather than hiding them. Browser support is
Chromium-first: Firefox works through documented fallbacks, and iPhone
playback goes through an fMP4 + `ManagedMediaSource` path
([`docs/27`](docs/27-ios-mse-fullscreen.md)).

## Run your own

Two Helm charts, two images, published to GHCR on every release with chart
version, `appVersion` and image tag always in lockstep.

```sh
helm upgrade --install gawk-server oci://ghcr.io/tuhis/charts/gawk-server \
  --version <X.Y.Z> -n gawk -f relay-values.yaml
helm upgrade --install gawk-app oci://ghcr.io/tuhis/charts/gawk-app \
  --version <X.Y.Z> -n gawk -f app-values.yaml
```

That is the shape of it, but the parts that actually decide whether it works
are DNS, a publicly-trusted certificate on a UDP-exposed relay, and telling
the frontend where its relay lives.
**[`docs/self-hosting.md`](docs/self-hosting.md)** walks through all of it:
prerequisites, the values you must change, TLS requirements and why
WebTransport is stricter than a normal HTTPS service, single-node versus
cluster mode, verification commands, and upgrades.

## Quickstart (local dev)

Prerequisites: Go ≥ 1.26 (the toolchain auto-downloads what the module
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

Open `http://localhost:5173/#/broadcast`, open the **⚙ settings** panel,
paste the cert hash into the "Dev cert hash" field, **Start a stream**, pick a
screen — you get a 6-char code. The cert hash persists to `localStorage`, so
in another tab just open the join link (or type the code on the landing page
`#/`) and it connects. No Chrome flags needed — the app passes the hash via
`serverCertificateHashes`. (The old stats-heavy pages remain at
`#/debug/broadcast` and `#/debug/view` for diagnostics.)

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

Browser E2E (R20): a real headless-Chrome viewer decoding real relayed
frames runs on every PR (`e2e` job), and a 2-replica cluster-mode variant on
kind runs on release PRs (`e2e-cluster`). Locally: see
[`e2e/README.md`](e2e/README.md).

Every module's own README covers building and running just that piece:
[relay](gawk-server/README.md) ·
[app](gawk-app/README.md) ·
[Linux broadcaster](gawk-broadcast/README.md) ·
[Windows broadcaster](gawk-broadcast-windows/README.md) ·
[telemetry](gawk-telemetry/README.md).

## Gotchas that will bite you

The full list — 112 entries, each of which cost real debugging time — lives in
**[`docs/gotchas.md`](docs/gotchas.md)**. The ones most likely to catch you
first:

- **`go test -race` needs `CGO_ENABLED=1`**; many environments default to `0`.
- **Browser hardware encode does not exist on Linux.** WebCodecs HW encode
  ships on Windows/macOS/Android only. That is why the native broadcasters
  exist — don't go flag-hunting.
  ([`docs/19`](docs/19-linux-native-broadcaster.md))
- **Raising `-max-idle-timeout` alone is a no-op.** The effective timeout is
  the minimum of both endpoints' and browsers advertise ~30 s;
  `-keepalive-period` is the mechanism that keeps idle viewers connected.
- **Trust the actual `VideoFrame`, not `MediaStreamTrack.getSettings()`** —
  Chrome's `getSettings()` has been observed to disagree with the frames it
  actually delivers. ([`docs/01`](docs/01-loopback-test.md))
- **`npx tsc --noEmit` in `gawk-app` passes vacuously** — the root
  `tsconfig.json` is solution-style. `npm run build` is the real typecheck.
- **`gawk-telemetry` builds cgo-free by default** and its SQL engine does not
  exist in that build (`//go:build duckdb`).
- **WSL2: Chrome must run *inside* WSL2** — Windows Chrome cannot complete a
  QUIC handshake to a server in WSL2 NAT mode.
  ([`docs/02`](docs/02-webtransport-hello.md))

## Design docs

Forty numbered docs, one per milestone, each carrying the decisions, the
alternatives that were rejected and why, and explicit acceptance criteria.
They are the reason this repository is worth reading rather than just running:
[**`docs/README.md`**](docs/README.md) is the index.

Start with [`docs/03`](docs/03-single-client-e2e.md) (the end-to-end path),
[`docs/12`](docs/12-worker-and-reliable-keyframes.md) (reliable keyframes +
worker offload), and [`docs/22`](docs/22-relay-scale-out.md) (how one
broadcast's audience spans pods).
## License

**[Apache-2.0](LICENSE)** — copyright (c) 2026 Juho Kuusisto.

Apache-2.0 rather than MIT for one reason worth stating: this is a codec and
transport project — H.264 profile negotiation, WebCodecs, QUIC, GF(256)
forward parity — and Apache-2.0 carries an explicit patent grant from every
contributor, plus a retaliation clause. MIT carries neither.

Contributions are welcome under the DCO (a `git commit -s` sign-off, no CLA) —
see [CONTRIBUTING.md](CONTRIBUTING.md).

### Third-party code

Every component ships a generated `THIRD-PARTY-NOTICES.md` listing what is
actually linked into that artifact, with licenses and copyright holders:
[relay](gawk-server/THIRD-PARTY-NOTICES.md) ·
[app](gawk-app/THIRD-PARTY-NOTICES.md) ·
[Linux broadcaster](gawk-broadcast/THIRD-PARTY-NOTICES.md) ·
[Windows broadcaster](gawk-broadcast-windows/THIRD-PARTY-NOTICES.md) ·
[telemetry](gawk-telemetry/THIRD-PARTY-NOTICES.md) ·
[telemetry UI](gawk-telemetry/ui/THIRD-PARTY-NOTICES.md).
Regenerate them with `python3 tools/licenses/gen-notices.py`; CI's `licenses`
job independently gates every dependency against a permissive allowlist.

Everything gawk links is permissive (MIT, BSD, Apache-2.0, ISC and friends)
with one deliberate exception: the native Windows broadcaster's GUI uses
**Slint** under its **Royalty-free Desktop License v2.0**, one of the three
licenses Slint offers. Note also that the Linux broadcaster *runs*
`gst-launch-1.0` as a separate, user-installed process — no GStreamer code is
linked or redistributed here.

Slint's royalty-free license asks for attribution, and this badge is it — §2(b)
of that license accepts it on a public page where the binaries are downloaded,
which is this README and the releases page. The asset is committed rather than
hot-linked so the attribution cannot quietly break if an upstream URL moves.

<a href="https://slint.dev">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/MadeWithSlint-logo-dark.svg">
    <img alt="Made with Slint" width="106" src="docs/assets/MadeWithSlint-logo-light.svg">
  </picture>
</a>
