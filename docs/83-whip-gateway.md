# R33 — WHIP ingest for OBS through `gawk-whip-gateway` (docs/83)

**Status**: designed 2026-11-01 from a code survey of the relay, the viewer
and OBS Studio's WHIP output (§3); owner decisions OD1–OD8 below are **open**.
Chunks **WG0–WG10** (`WG` = WHIP gateway; unused). Nothing is implemented.

Supersedes the 2026-07-29 sketch in `ROADMAP.md` §R33, which put WHIP inside
`gawk-server`. That sketch predates the relay fleet's scale-out targets, R73's
reliable uplink, R74's PoP relays, R78's stop semantics and the removal of the
Go Linux broadcaster, and it left the stream key's UX open.

Touches a new seventh deployable `gawk-whip-gateway` (OD1), `gawk-server`
(a mint-only publish, a trusted publish proxy, and the publisher engine
promoted to a public package), `gawk-app` (an "OBS" panel on the broadcaster
screen) and the dev stack. It does **not** touch the wire, the hub's ingest
surface or any viewer.

## 1. Purpose

Let a broadcaster use OBS Studio, or any other WHIP sender (FFmpeg's
`-f whip`, GStreamer's `whipsink`), instead of the browser or native
broadcasters. OBS brings scenes, overlays, several sources and its own audio
mixer, which `getDisplayMedia` and the native apps don't. People who already
stream to other platforms with OBS shouldn't have to give that up.

WHIP (RFC 9725, the WebRTC-HTTP Ingestion Protocol) is how OBS 30+ pushes
WebRTC without a plugin. The sender `POST`s an SDP offer with
`Authorization: Bearer <token>` and gets back `201 Created`, an SDP answer
and a `Location` for the session. Media then flows as ordinary WebRTC
(ICE, DTLS-SRTP, RTP), and a `DELETE` on the `Location` ends it. In OBS the
user sets **Settings → Stream → Service: WHIP**, a server URL and a bearer
token.

This is an **ingest** path only. WebRTC playback (WHEP) stays out of scope,
and viewers stay on WebTransport + WebCodecs. The CLAUDE.md "WebRTC + SFU,
set aside" decision is about the viewer pipeline and is not reopened.

## 2. Shape

```
OBS ──WHIP POST (HTTPS)──▶ gawk-whip-gateway ──CONNECT /publish/{id}?resume=…──▶ relay fleet ──▶ viewers
    ◀──SDP answer, 201────        │  pion: ICE-lite, DTLS-SRTP, RTP           (WebTransport, unchanged)
    ══RTP H.264 + Opus (UDP)═════▶│  RTP → Annex-B access units → publisher engine
```

The gateway is a **publisher like any other** from the relay's point of view.
It terminates WebRTC with `pion/webrtc`, rebuilds whole H.264 access units and
Opus frames from RTP, and feeds them to the same Go publisher engine that
`gawk-pubsim` runs today (`gawk-server/internal/pubsim/engine`, §3.4). That
engine already does gawk's chunking within the ~1200-byte limit, keyframes on
reliable streams, R29 forward parity, TimeSync, auto-resume with the resume
token, and room attach. The relay, hub, wire and viewers don't change.

The gateway absorbs relay-side churn. A relay drain (4002) makes the engine
resume on another pod while OBS keeps sending;
OBS never sees it.

## 3. What the survey found

### 3.1 The stream key already exists: broadcast ID + resume token

- A broadcast's **resume token** is `HMAC-SHA256(key, normalizedID)`
  truncated to 16 bytes. It is deterministic and has no expiry. How long it
  lives depends on where the key comes from
  (`gawk-server/internal/transport/resume.go`):
  - **Explicit `-resume-token-key`:** the token lives until the key is
    rotated, which revokes every token at once. The key never leaves the
    relay, so the token is a real per-broadcast ownership proof.
  - **Derived from `-publish-secret`** (HKDF, the chart default when
    `resumeTokenKey` is empty): rotating the secret revokes every token, and
    **anyone holding the publish secret can compute every ID's token
    offline**. The package comment says fleets should set the explicit key.
  - **Neither:** a random key per relay process, so every token dies on each
    relay restart, which on the reference deployment means every release.
- `CONNECT /publish/{id}?resume=<token>` with a valid token for an
  **unknown** ID **creates** the broadcast (`hub/hub.go`, `docs/22`). After
  R78's stop (close 4014) a reclaim recreates it, and viewers who stayed on
  the code pick it back up (`docs/79`, `TestPublisherStopThenReclaimRecreates`).
- So, **with an explicit key (D12), ID + token is already a lasting,
  Twitch-style stream key**: the same code every session, so viewers' links
  keep working. The gateway needs no
  new relay credential to publish one, and no state of its own.
- No HTTP API mints an ID today. IDs are minted only by a live
  `CONNECT /publish`, which also creates the broadcast and fires
  `broadcast.started`. gawk-admin's `/api/v1/broadcasts` lists and kills only.
- The desktop app persists its token on disk. The web app keeps its token in
  memory only (`BroadcasterScreen.tsx`, `resumeTokenRef`).
- R67's desktop handoff (`docs/69` D3) deliberately never carries a token or
  secret. Showing the user a key is new policy, decided in OD3.

### 3.2 The GOP: what a 1–2 s keyframe interval costs

gawk publishers use a **500 ms GOP** (`gawk-app/src/media/types.ts`
`keyframeIntervalMs`, the desktop engine's `GOP_MS`, `docs/08`). Nothing in
the relay or viewer *checks* the interval, but several behaviours scale with
it:

| Behaviour | 500 ms (today) | 1 s | 2 s | Where |
|---|---|---|---|---|
| Live join: frozen cached keyframe until the next one | ≤ 0.5 s | ≤ 1 s | ≤ 2 s | `hub.go` (no delta ring), `reorder-buffer.ts` |
| Live loss: freeze until next keyframe | ≤ 0.5 s | ≤ 1 s | ≤ 2 s | `docs/12`, `docs/gotchas.md` |
| R29 parity: GOPs with an unrecovered loss (allowance is per GOP) | ~0.3 % | higher | ~4 % (estimate, ~85 deltas/GOP vs ~20) | `resilient.ts`, `docs/34` |
| Resilient carrier: one timed-out write kills the rest of the GOP | ≤ 0.5 s | ≤ 1 s | ≤ 2 s | `hub.go` `CarrierWriteTimeout` ("one GOP") |
| Deep-buffer DVR history (3 s window, evicted by whole GOP) | ~2.5–3 s | ~2–3 s | can drop to ~1 s, the `MinDVRBufferMs` floor | `dvr.go`, `docs/26` D4 |
| Edge / PoP / origin re-prime without a forced IDR | ≤ 0.5 s | ≤ 1 s | ≤ 2 s | `docs/22`, `hub.go` `InvalidatePrimes` |
| Stuck-viewer eviction (10 keyframes) | ~5 s | ~10 s | ~20 s; the viewer's own 8 s watchdog fires first | `hub.go`, `viewer.ts` |
| **Viewer keyframe watchdog** `KEYFRAME_STALL_MS = 8000` | fine | fine | fine; **≥ 8 s GOP reconnects viewers in a loop** | `viewer.ts` |
| Sampler (one sample per 5 s, reuses an unchanged keyframe) | fine | fine | fine; > 5 s repeats samples | `docs/71` |
| R73 carrier keyframe gate (~1 RTT per GOP) | baseline | cheaper | cheaper | `docs/75` (the gateway doesn't use carriers anyway, §3.4) |

**Verdict:** a 1 s GOP works and roughly doubles the worst-case freeze on
join and on loss. A 2 s GOP works with visibly worse recovery and a weaker
Deep-buffer promise. Above 2.2 s the gateway refuses (D7): OBS's
"0 = auto" can mean x264's default `keyint` of 250 frames (~4 s at 60 fps),
and approaching 8 s breaks viewers.

### 3.3 OBS's WHIP output, read from source

Read from `obsproject/obs-studio` master on 2026-11-01 (`plugins/obs-webrtc`,
`plugins/obs-x264`, `plugins/obs-nvenc`, `libobs/obs-encoder.c`). WG0
re-checks it against the OBS release it tests.

- **It ignores PLI/FIR.** The video chain is `RtcpSrReporter` +
  `RtcpNackResponder` (+ a `PacingHandler` when a bitrate is set). There is no
  PLI handler, and libobs has no public "force a keyframe" call for an output
  to make. **A receiver cannot get a keyframe from OBS on demand.**
- **The keyframe interval is whole seconds.** `keyint_sec` is an integer
  property: x264 0–20, NVENC 0–10, where 0 means encoder default. The
  smallest GOP in the UI is therefore **1 s**. Sub-second needs the encoders'
  custom-options field: x264 `keyint=30` (frames), NVENC `gopLength=` /
  `idrPeriod=`. WG0 confirms which spelling gives IDR-only GOPs on each
  encoder.
- **It forces B-frames off and repeats headers** for WHIP
  (`WHIPService::ApplyEncoderSettings` sets `bf = 0`, `repeat_headers = true`),
  which matches gawk's encoder rules (`docs/19`: no B-frames, every keyframe
  an IDR, SPS/PPS before every IDR). The gateway still validates both, for
  older OBS and other WHIP senders.
- **Codecs:** video H.264, HEVC (when built in) or AV1, one per session (the
  encoder's). Audio Opus only. gawk viewers decode H.264/VP9/VP8, so the
  gateway accepts **H.264 only** (D5).
- **Simulcast:** one layer per encoder, with RIDs when there are two or more.
  OBS fails the connect if the answer rejects a layer.
- **No trickle ICE.** OBS gathers once after the answer and takes STUN/TURN
  servers from `Link` headers. It needs a reachable candidate in the answer.
- **NACK:** a 4000-packet retransmit buffer (~3 s at 8.5 Mbps). No congestion
  control or bitrate adaptation; the bitrate is fixed at connect.
- **Reconnect:** on ICE disconnect OBS signals `OBS_OUTPUT_DISCONNECTED`, and
  libobs's generic auto-reconnect re-`POST`s with the same token. A non-201
  answer is `OBS_OUTPUT_INVALID_STREAM`, shown as a generic error.
  **OBS never shows the server's error text**, so the reason has to be shown
  somewhere else (D9).

### 3.4 What the gateway can reuse

- **The publisher engine.** `gawk-server/internal/pubsim/engine` takes whole
  Annex-B `AccessUnit{Data, Keyframe, TimestampUs, EncoderRestarted}`s from a
  `MediaSource` (`Options.MediaFactory`) and handles everything on the
  WebTransport side. It derives the decoder config from the first SPS (H.264
  only, which is all the gateway accepts). It has **no R73 carriers or BBR**;
  those live in the Rust engine only. On resume it forces no keyframe.
- It is `internal/`, so a module other than `gawk-server` can't import it.
  OD1 decides between promoting it and building the gateway inside the
  `gawk-server` module.
- **The template** for a small Go deployable that is a WebTransport client of
  the relay with a public listener is `gawk-mumble-bridge` (module, chart with
  deployment/service/networkpolicy/servicemonitor/pdb, `CGO_ENABLED=0` image).

## 4. Decisions

- **D1 — A separate deployable, `gawk-whip-gateway`, never code in the relay
  binary.** The owner's call (2026-11-01). It keeps pion's ICE/DTLS stack, a
  second UDP exposure and a public HTTP endpoint off the relay pods. It lets an
  operator run the relay without it. It makes the gateway a client of the
  relay's existing publish gates instead of a new way around them. It is
  **default off everywhere**, like gawk-admin and gawk-sampler.

- **D2 — The WHIP bearer token is the stream key `gawk1_<ID>_<token>`.**
  `<ID>` is the 6-character broadcast code and `<token>` the 32-hex resume
  token. One WHIP URL serves everyone (`https://<gateway>/whip`), and the key
  says which broadcast. This is the server-URL-plus-stream-key pattern OBS
  users know from every other platform. The gateway parses the key and dials
  `CONNECT /publish/{ID}?resume=<token>`. It holds **no state** and **no
  token-minting key**: a key is only as powerful as the publish it already
  allows. The `gawk1_` prefix versions the format and makes a leaked key
  findable by secret scanners.

- **D3 — The web app mints keys; the relay gets a mint-only publish.**
  `CONNECT /publish?mint=1` passes the same gates as a mint (draining,
  per-IP rate limit, `?secret=`, IP ban, `CheckPublishNew`). It announces the
  ID and the resume token as today, then closes the session with a new
  close code meaning "minted, nothing to publish", **without creating a
  broadcast**: no
  `broadcast.started`, nothing in `/statusz` or the admin portal, nothing
  counted against `max-broadcasts`. OBS's first `POST` then creates the
  broadcast through the existing token reclaim. Minting by opening and
  stopping a real broadcast was rejected, because it fires a started/stopped
  event pair and a webhook for a broadcast that never had media. The new
  close code is allocated in `wire.go` and mirrored in TS and Rust with
  golden vectors (CONTRIBUTING's three-copy rule).

- **D4 — The gateway is a trusted publish proxy, so bans and rate limits
  see the OBS IP.** Without this, every OBS user looks like the gateway's
  pod IP to the relay. One abusive key would get the gateway IP-banned, and
  the per-IP rate limit (3/s, burst 10) would throttle everyone. A new relay
  knob `-publish-proxy-token` / `GAWK_PUBLISH_PROXY_TOKEN` / Helm
  `publishProxy.token`, plumbed through `registryOptions` in
  `internal/serve/options.go`, lets a `CONNECT /publish…` that presents it
  (`Authorization: Bearer`, which a Go WebTransport client can set) supply
  `Gawk-Publisher-Ip`. The relay then applies the per-IP rate limit, IP ban
  and the admin portal's publisher-IP fields to that address. The same token
  exempts the gateway from the Origin allow-list. The publisher IP still
  never leaves through events or webhooks (CLAUDE.md). The gateway also
  rate-limits `POST /whip` per client IP itself, before dialing the relay.

- **D5 — H.264 + Opus only, one layer.** The SDP answer accepts H.264
  (packetization-mode 1, any profile the viewers' list can decode; Constrained
  Baseline / Main / High up to level 5.2) and Opus. It **rejects** HEVC, AV1
  and simulcast offers with `406`. OBS then fails the connect, and D9 tells
  the user why. VP8/VP9 from other WHIP senders are a later addition if anyone
  needs them; the engine's config derivation is H.264-only today.

- **D6 — The adapter drops, it doesn't wait.** RTP is reassembled into whole
  access units in a short reorder window (default **20 ms**, a gateway knob).
  The gateway sends NACKs as standard WebRTC. A retransmit that arrives inside
  the window is used. A frame still incomplete when the window closes is
  **dropped**, and the deltas after it are dropped until the next IDR, exactly
  as a viewer handles loss today. This applies "favour dropped frames over
  stalled playback" at the new hop. A longer window costs every frame its
  length in latency to hide losses that a 1 s GOP already bounds. WG0 measures
  the loss rate OBS → gateway on the reference path before the default is
  final.

- **D7 — The GOP is configured at the source, measured at the gateway,
  bounded by the gateway.** OBS can't be asked for keyframes (§3.3), so:
  - The guide (and the web app's OBS panel) tells the user to set **Keyframe
    Interval = 1 s**, the smallest the UI allows. It gives the custom-options
    spelling for 0.5 s on x264 and NVENC as an advanced option, once WG0 has
    verified it.
  - The gateway measures the IDR spacing from the **RTP timestamps of IDR
    starts**: the first packet of an access unit whose NAL (or FU-A / STAP-A
    inner) type is 5. It records them as packets arrive, before D6 can drop the
    frame, so a lost or late keyframe doesn't look like a long GOP. It judges
    the **median of the last 5 spacings**: **≤ 1.1 s**: fine. **≤ 2.2 s**:
    accepted, with a warning in the status (D9). **> 2.2 s**: the gateway ends
    the session with reason `gop_too_long`. Before 5 spacings exist, it ends
    early only after **two consecutive gaps of more than 4.5 s** with no IDR
    start (OBS's "auto" GOP), which one lost IDR start can't produce at a GOP
    the bounds allow. While the same key reconnects within 10 minutes it answers
    `POST`s with `422` and that reason, so OBS's auto-reconnect doesn't loop
    viewers through freezes. The thresholds are gateway knobs.
  - **The gateway sends PLI anyway, on two events only**: on session start
    and after the engine resumes on another relay pod. Both are moments when
    the relay's cache is empty, not moments of loss.
    Stock OBS ignores it (§3.3), but GStreamer's and FFmpeg's WHIP senders and
    any future OBS that honours it get the fast re-prime the native publishers
    get from `force_idr` (`docs/22`). The gateway **never sends a PLI for a
    dropped frame and never sends periodic PLI**. Drops (D6) happen mostly
    under loss and congestion, which is exactly where `docs/15` D6 found a
    keyframe request makes things worse: the bottleneck is delivering the
    keyframe, not how often one comes. A dropped frame waits for the next
    natural IDR, as it does for viewers today.
    A PLI every 500 ms would just be a forced 500 ms GOP; asking the user for
    a short GOP gets the same result without the requests.
  - This doesn't conflict with `docs/15` D6, which rejected a
    **viewer → server** back-channel and the relay talking back to the
    broadcaster. Here the gateway is the WebRTC receiver of its own leg. No
    viewer signal reaches OBS, and the relay still never talks back.

- **D8 — Session lifecycle maps onto existing semantics.**

  | Event | Gateway does | Relay sees |
  |---|---|---|
  | `POST` with a valid key | reclaim `/publish/{ID}?resume=` | broadcast created or resumed |
  | `DELETE` on the `Location` | engine `StopBroadcast()` (added in WG1) | close **4014**, broadcast ends (R78), viewers wait on the code |
  | ICE/consent failure (OBS gone) | engine `Stop()` (closes with code 0, as today) | publisher gone, **grace** (`-broadcast-grace`, 5 min); OBS's auto-reconnect reclaims the same ID |
  | Second OBS with the same key | new session reclaims | **4004** to the first; the gateway tears down the first PeerConnection |
  | Relay 4002 (drain) | engine auto-resumes, sends PLI | resume on another pod |
  | Relay 4006 (operator kill) / ID ban | ends the session; `POST`s answer `403` | — |
  | Relay `409` (ID reserved for a PoP) | `409` to OBS | — |
  | Relay `429` (caps, rate limit) | `429` + `Retry-After` | — |
  | Relay `401`/`403` (bad secret or token) | `401`/`403` | — |

- **D9 — The web app is where the user sees the gateway's verdict.** OBS
  shows only "could not connect" (§3.3), so the gateway exposes
  `GET /whip/status` (same bearer key, CORS-allowed for the configured
  `gawk-app` origins). It returns `{state, codec, resolution, fps, gopMs,
  bitrateKbps, lossPct, warnings[], lastError}` for the key's current or
  last session. The OBS panel (§5) polls it every 2 s while open. The status
  holds no IPs and no frame data.

- **D10 — ICE: ICE-lite on one UDP port, one replica.** pion runs ICE-lite
  with a single UDP mux port and a 1:1 NAT mapping
  (`SettingEngine.SetNAT1To1IPs`) to the gateway's public address, so the
  answer carries one host candidate. It sits behind its own UDP
  `LoadBalancer` Service, with an ICE-TCP passive candidate on the same port
  number as the fallback. **One replica** (`Recreate`): ICE binds a session to
  the pod that answered the `POST`, and kube-proxy may send another pod the
  UDP flow behind one LB address. Load is bounded by concurrent broadcasts
  (`max-broadcasts`, 5 by default), not viewers. A restart is an ICE failure,
  so OBS reconnects and the key reclaims the same ID. Scaling past one pod
  (a port per pod, or a TURN front) is out of scope until a deployment needs
  it.

- **D11 — No R73 carrier in v1.** The Go engine has none, and the gateway's
  relay leg is normally a short, clean in-cluster or same-site hop. The lossy
  long path is OBS → gateway, which is WebRTC's (NACK, D6). A gateway placed
  far from its relay would want the carrier; that waits for the Go engine
  growing one.

- **D12 — A relay that a gateway publishes to must have an explicit
  `-resume-token-key`.** Without it, D2's key either dies on every relay
  restart (random key) or can be computed for every broadcast by anything
  holding the publish secret, which the gateway holds (derived key, §3.1).
  The relay refuses to start with `-publish-proxy-token` (D4) set and no
  explicit `-resume-token-key`, and the relay chart refuses to render
  `publishProxy.token` without `resumeTokenKey`. A gateway is useless without
  D4 on a fleet with more than one publisher, so this ties the two together.

### Rejected

- **WHIP inside `gawk-server`** (the 2026-07-29 sketch). See D1.
- **An OBS output plugin** (idea #19 in the mumisija ideas list): a native
  release unit per OBS version and platform, installed by hand, and one more
  client of the wire format. It would get R73's carrier and parity for free,
  but WHIP gets parity at the gateway, and OBS users already accept OBS's
  encoder and pacing.
- **Periodic PLI to force a 500 ms GOP.** Ignored by OBS. For senders that
  honour it, it's a forced GOP with the congestion cost `docs/15` D6
  measured. See D7.
- **Transcoding at the gateway** to impose gawk's GOP: a decode and re-encode
  per broadcast, adding latency and quality loss, GPU or heavy CPU per
  stream, and a licensing question. A config field in OBS does the same job.
- **The gateway holds `-resume-token-key` and mints keys itself.** That would
  let one compromised pod compute the token for every broadcast on the fleet
  and supersede any of them (4004). With D3 and D12 it holds nothing that a
  stolen key doesn't already give. (It does hold the publish secret, which is
  why D12 requires the explicit key: with a derived key, the secret alone
  would give it every token.)
- **Mint by starting and immediately stopping a broadcast.** See D3.
- **Raising `KEYFRAME_STALL_MS` or the DVR window for long GOPs.** That
  weakens every broadcast for the sake of a misconfigured one. The gateway
  bounds the GOP instead (D7).

## 5. Stream-key UX in `gawk-app`

The OBS panel sits on the broadcaster screen's pre-start card, as a sibling
of "Open in the desktop app" (`BroadcasterScreen.tsx`, the R67 handoff link).
It opens a modal shaped like `DesktopHandoffModal`, and it appears only when
the selected server has a gateway (OD4).

1. **No key yet:** a short line ("Stream from OBS Studio or another WHIP
   app") and a **Create stream key** button. It mints through D3 with the
   server's saved publish secret, prompting for it the way Start does when
   it's required.
2. **Key ready:** two copyable fields, **Server** (the WHIP URL) and
   **Bearer token** (the key, masked, with Show and Copy), using OBS's own
   labels. A checklist of the OBS settings that matter: *Service: WHIP*,
   *Keyframe Interval: 1 s*, *Encoder: H.264 (NVENC/AMF/QSV or x264)*, *Audio:
   Opus*, *Simulcast off*. The join link and code for viewers, with the
   existing copy and QR affordances.
3. **Live status** from D9: *Waiting for OBS…* → *Live — 1920×1080 60 fps
   H.264, keyframe every 1.0 s*, plus warnings ("Keyframe interval is 2 s;
   set it to 1 s for faster recovery") and the last error in plain words
   ("OBS is sending AV1; choose an H.264 encoder"). An **Open viewer** link
   gives a preview in another tab.
4. **Reset key** mints a new code, with a confirmation that the old link
   stops being the one people should use. An old key can't be revoked
   (§3.1); a leaked key is closed by an operator ID ban in gawk-admin, and the
   panel says so.

The key is stored per server in `localStorage`, next to that server's saved
publish secret (OD3), so the same key and join code come back next time.
**Forget key** removes it.

**Rooms** (later, WG9): the panel's room picker attaches the OBS broadcast to
a room with the ID and token it already holds (`docs/44` D8: attach is keyed
by ID + token, not by the publisher session). WG9 verifies that this works for
a broadcast whose publisher is the gateway.

## 6. Where it lives

- **Module (OD1):** recommended: a **seventh top-level Go module**,
  `gawk-whip-gateway/`, its own release unit (release-please package, chart
  `oci://ghcr.io/tuhis/charts/gawk-whip-gateway`, image
  `ghcr.io/tuhis/gawk-whip-gateway`, chart version == appVersion == image
  tag), with `replace github.com/Tuhis/gawk/gawk-server => ../gawk-server`
  like `gawk-mumble-bridge`. This needs the publisher engine promoted from
  `internal/pubsim/engine` to a **public** `gawk-server/publisher` package.
  Same rule as `wire`: reuse, never mirror. A semantic change to it then needs
  a `gawk-whip-gateway`-scoped commit in the same PR, a rule CONTRIBUTING
  gains with WG1. Pion stays out of `gawk-server`'s `go.mod`.
  Alternative: a `gawk-server/cmd/gawk-whip-gateway` like `gawk-relay`. That
  needs no promotion, but pion enters the relay module's dependency graph and
  the gateway releases with every relay release.
- **Chart:** copied from `gawk-mumble-bridge`'s: Deployment (1 replica,
  `Recreate`), an HTTP Service for the WHIP endpoint behind the public
  Ingress (TLS by the Ingress), a UDP+TCP `LoadBalancer` Service for media,
  a NetworkPolicy (egress to the relay's UDP port only, ingress from the
  Ingress and the media LB), a ServiceMonitor and a PDB. `values.yaml` holds
  `relay.url`, `relay.publishSecret` (existing secret ref),
  `relay.publishProxyToken` (D4), `publicAddress` (D10), `allowedOrigins`
  (D9 CORS) and the D6/D7 knobs. Every value has a flag and a `GAWK_WHIP_*`
  env (the `registryOptions` rule, applied to the gateway's own config).
- **Frontend config:** `whipGatewayUrl` in `window.__GAWK_CONFIG__`
  (`gawk-app/src/config.ts`), set by the frontend chart's values. When it's
  unset the panel doesn't appear.
- **Dev stack:** a `whip` compose profile with the gateway and a GStreamer
  `whipsink` (or `ffmpeg -f whip`) container publishing a test pattern with a
  1 s GOP, so the OBS panel shows a live stream with nothing to install.
- **Telemetry:** Prometheus metrics only in v1: sessions, `POST` outcomes by
  status, frames dropped by the reorder window, measured GOP, NACKs sent.
  R28 per-session telemetry is a later addition.

## 7. Latency budget

Estimates, not measurements, until WG0 replaces them with numbers measured
the same way as the native ≈50 ms figure.

| Stage | Tuned OBS (NVENC, 1 s GOP) | Default-ish OBS (x264) |
|---|---|---|
| Capture + OBS compositing | ~17–33 ms | ~17–50 ms |
| Encode | ~5–15 ms | ~15–50 ms |
| Packetize + pace | ~1–5 ms | ~5–20 ms |
| OBS → gateway | RTT/2 | RTT/2 |
| Gateway reorder window (D6) | ≤ 20 ms (usually ~1 frame of reassembly) | same |
| Gateway → relay → viewer | as today | as today |
| **Glass-to-glass, LAN** | **~60–120 ms** | **~100–200 ms** |

## 8. Chunks and acceptance criteria

| Chunk | Scope | Accepted when |
|---|---|---|
| **WG0** | Lab: OBS (latest release, Windows NVENC and x264, 1 s and 2 s GOP) → a minimal pion WHIP receiver → the pubsim engine → the compose relay → the shipping viewer. Re-check §3.3 on that release (PLI ignored, `bf=0`, keyint granularity), and find the sub-second custom-options spelling per encoder. | Glass-to-glass p50/p95 on LAN and with 1 % / 40 ms netem, recorded in §7. §3.3 confirmed or corrected. Loss rate OBS → receiver on the reference path recorded, and D6's window default confirmed or changed. Owner sign-off on OD1–OD8. |
| **WG1** | Promote the engine to `gawk-server/publisher` (OD1), and add `StopBroadcast()`, which closes the session with **4014** (`Stop()` closes with code 0 today, so the relay can't tell a stop from a drop). `gawk-pubsim`, `e2e`, `e2e-cluster` and the iOS viewer tests keep using `Stop()` unchanged. CONTRIBUTING gains the release-coupling line. | `go test ./...` and the e2e lanes green. No `internal/pubsim/engine` import left outside the package. A test: `StopBroadcast()` ends the broadcast at once (R78), while `Stop()` leaves it in grace. |
| **WG2** | Relay: `CONNECT /publish?mint=1` (D3) and the new close code in all three wire copies with golden vectors. | Tests: a mint announces ID + token, creates no broadcast, emits no event, isn't counted in `max-broadcasts`, and passes every publish gate (secret, rate limit, IP ban, draining). A reclaim with the minted token creates the broadcast. Vectors byte-identical in Go, TS and Rust. |
| **WG3** | Relay: `-publish-proxy-token` (D4) through `registryOptions`, flag + env + Helm, and D12's guard. | Tests: with the token, `Gawk-Publisher-Ip` drives rate limit, IP ban and the admin publisher-IP fields, and the Origin check is skipped. Without it the header is ignored. A test asserts no event or webhook carries the IP. The Helm value renders into the relay args. The relay refuses to start, and the chart to render, with the proxy token and no explicit `resumeTokenKey`. |
| **WG4** | Gateway skeleton: module, config (flags + `GAWK_WHIP_*` + chart values), `/healthz`, metrics, Dockerfile, chart (default off), release-please package, a CI path filter on `gawk-whip-gateway/` and `gawk-server/`. | CI builds and tests it. The chart renders with defaults and does nothing until enabled. A release produces image + chart at the same version. |
| **WG5** | WHIP endpoint: `POST`/`DELETE`/`OPTIONS`, key parsing (D2), SDP answer (D5), ICE-lite + UDP mux + NAT 1:1 + ICE-TCP (D10), the D8 status mapping, per-IP `POST` rate limit. | A pion WHIP client test in CI: H.264+Opus accepted (`201` + `Location`); HEVC, AV1 and simulcast refused `406`; bad key `401`; every row of D8's table covered against a test relay. |
| **WG6** | Media adapter: RTP → Annex-B AUs with the reorder window (D6), IDR/SPS/B-frame validation, GOP measurement and bounds (D7), Opus → the engine's audio, RTP/RTCP-SR timestamps → engine timestamps, event PLIs. | Unit tests with recorded OBS RTP: a dropped packet drops that frame and the deltas until the next IDR; a B-frame stream is refused with reason; GOP thresholds warn and end as specified, and a stream with a 2 s GOP that loses one whole IDR (and one IDR's first packet) is neither warned about more than it would be without the loss nor ended; A/V offset within 20 ms of the source; PLI sent on start and on resume only: never on a dropped frame, never periodically. |
| **WG7** | `GET /whip/status` (D9) with CORS. | Tests: returns the D9 fields for the key's session, `401` for another key, and nothing containing an IP. |
| **WG8** | gawk-app OBS panel (§5), `whipGatewayUrl` config, key storage (OD3), status polling. | Vitest covers mint, show/copy/forget/reset, the publish-secret prompt, status states and warnings. The panel is hidden without `whipGatewayUrl`. |
| **WG9** | Dev stack `whip` profile with a GStreamer/FFmpeg WHIP sender. Rooms attach from the panel. `docs/self-hosting.md` section, an OBS setup guide, `docs/gotchas.md` entries. | `docker compose --profile whip up` shows a live test pattern in the viewer with no copy-paste. A `dev-stack` CI job round-trips a frame through the gateway. Attach puts the OBS broadcast in a room. |
| **WG10** | Owner pass on the reference deployment with real OBS on the gaming PC. | Key created in the web app, OBS streams, viewers watch. A drain during the stream doesn't reach OBS. OBS's auto-reconnect keeps the join code. Glass-to-glass recorded next to the native figure. |

## 9. Security considerations

- **A key is a full publish credential for its code until the relay's
  explicit `-resume-token-key` rotates** (§3.1, D12). It's shown masked,
  stored only in the user's browser, and sent only to the gateway over HTTPS.
  A leaked key can be used until the operator bans the ID or rotates the key,
  which invalidates every key. The panel says so, and Reset key gives the
  user a fresh code.
- The gateway holds the relay's publish secret and the publish-proxy token.
  The proxy token lets its holder claim any publisher IP, so it lives only in
  the gateway's Secret, never in the frontend, and the relay accepts it only
  on publish routes.
- `POST /whip` is public by design. It's rate-limited per IP at the gateway
  (WG5), then gated again by the relay through D4.
- The status endpoint answers only for the presenting key and holds no IPs or
  frame data. Content screening (R40) works unchanged, since it reads the
  relay's cached keyframe.

## 10. Owner decisions

| # | Question | Recommendation |
|---|---|---|
| **OD1** | Own module (`gawk-whip-gateway/`) with the engine promoted to `gawk-server/publisher`, or a `cmd/` inside `gawk-server`? | Own module (§6) |
| **OD2** | A mint-only publish on the relay (D3), or mint by starting and stopping a broadcast and accept the spurious events? | Mint-only |
| **OD3** | Keep the key in `localStorage` per server, or show it once and make the user keep it? | `localStorage`, with Forget |
| **OD4** | Where the panel learns a server's gateway: only `whipGatewayUrl` in `config.js` (official and self-hosted SPAs), or also an optional per-server field in the R37 server picker? | `config.js` only in v1 |
| **OD5** | GOP bounds: warn above 1.1 s, refuse above 2.2 s (D7)? | Yes, as knobs |
| **OD6** | D6's reorder window: 20 ms default, drop rather than wait? | Yes, pending WG0's loss numbers |
| **OD7** | One replica, ICE-lite behind its own UDP LB (D10)? | Yes |
| **OD8** | Publish the gateway's WHIP endpoint on the official deployment (`whip.gawk.ioio.fi`) once WG10 passes? | Yes |

## 11. Non-goals

- WHEP or any WebRTC viewer path.
- Accepting HEVC, AV1, VP8/VP9 or simulcast (D5).
- A WHIP gateway inside the PoP relay `gawk-relay` (a LAN party with OBS can
  use the home gateway; bundling one in `gawk-relay` is a later milestone).
- Transcoding, scaling, or any change to OBS's encoded bitstream beyond
  re-chunking.
- R73 carriers on the gateway → relay leg (D11).
