# R33 — WHIP ingest for OBS through `gawk-whip-gateway` (docs/83)

**Status**: designed 2026-11-01 from a code survey of the relay, the viewer
and OBS Studio's WHIP output (§3); owner decisions OD1–OD8 **taken
2026-11-01** (§10: OD6 and OD7 differ from the first draft). The web app
(§5) was redesigned in a Claude Design pass on 2026-11-02
([canvas](https://claude.ai/artifact/TtFyDhdhEv5x13oSmjds2m), §5.1), and its
owner decisions OD9 and OD10 were **taken 2026-11-02**: the way in is a
footnote under the broadcaster card, and OBS gets a page of its own.
Chunks **WG0–WG10** (`WG` = WHIP gateway; unused). **WG1–WG9 implemented
2026-11-02** (§12 records what the build settled); WG0's lab pass with real
OBS, WG10's owner pass and the OBS page's Room row (§12.4) are open.

Supersedes the 2026-07-29 sketch in `ROADMAP.md` §R33, which put WHIP inside
`gawk-server`. That sketch predates the relay fleet's scale-out targets, R73's
reliable uplink, R74's PoP relays, R78's stop semantics and the removal of the
Go Linux broadcaster, and it left the stream key's UX open.

Touches a new seventh deployable `gawk-whip-gateway` (OD1), `gawk-server`
(a mint-only publish, a trusted publish proxy, and the publisher engine
promoted to a public package), `gawk-app` (an OBS page at `#/broadcast/obs`
and a way in from the broadcaster screen) and the dev stack. It does **not** touch the wire, the hub's ingest
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
resume on another pod while OBS keeps sending; OBS never sees it.

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
  keep working. The gateway needs no new relay credential to publish one,
  and no durable state of its own.
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
  `CONNECT /publish/{ID}?resume=<token>`. It holds **no durable state** (only
  in-memory per-key records: sessions, D7's backoff and D9's refusals,
  normally on the pod the key hashes to, though they can sit on another pod
  after a rescale or rolling release, where D10's peer lookup finds them; a
  restart ends that pod's sessions, which OBS reconnects, and loses its
  records, which only shortens a backoff) and **no token-minting key**: a key
  is only as powerful as the publish it already allows. The `gawk1_` prefix
  versions the format and makes a leaked key findable by secret scanners.

- **D3 — The web app mints keys; the relay gets a mint-only publish.**
  `CONNECT /publish?mint=1` passes the same gates as a mint (draining, per-IP
  rate limit, `?secret=`, IP ban, `CheckPublishNew`). It announces the ID and
  the resume token as today, then closes the session with a new close code
  meaning "minted, nothing to publish", **without creating a broadcast**: no
  `broadcast.started`, nothing in `/statusz` or the admin portal, nothing
  counted against `max-broadcasts`. OBS's first `POST` then creates the
  broadcast through the existing token reclaim. Minting by opening and
  stopping a real broadcast was rejected, because it fires a started/stopped
  event pair and a webhook for a broadcast that never had media. The new close
  code is allocated in `wire.go` and mirrored in TS and Rust with golden
  vectors (CONTRIBUTING's three-copy rule).

- **D4 — The gateway is a trusted publish proxy, so bans and rate limits
  see the OBS IP.** Without this, every OBS user looks like the gateway's
  pod IP to the relay. One abusive key would get the gateway IP-banned, and
  the per-IP rate limit (3/s, burst 10) would throttle everyone. A new relay
  knob `-publish-proxy-token` / `GAWK_PUBLISH_PROXY_TOKEN` / Helm
  `publishProxy.token`, plumbed through `registryOptions` in
  `internal/serve/options.go`, lets a `CONNECT /publish…` that presents it
  (`Authorization: Bearer`, which a Go WebTransport client can set) supply
  `Gawk-Publisher-Ip`. The relay verifies the token **before**
  `rateLimited` runs (today's handler order changes), and with a valid token
  every per-IP decision uses the canonicalised header address and never the
  peer's: the loopback exemption, the `trustedCidrs` exemption, the rate-limit
  bucket, the IP ban and the admin portal's publisher-IP fields. Otherwise the
  gateway, a pod inside the `trustedCidrs` the relay chart tells operators to
  set, would skip the rate limit entirely. The relay canonicalises the header
  the way `remoteIP()` does (`moderation.CanonicalAddr`). With a valid token
  it refuses an unparsable **or missing** header with `400`, and never falls
  back to the peer address, which would bring the `trustedCidrs` bypass back
  for a buggy or older gateway. The same token exempts the gateway from the
  Origin allow-list. The publisher IP still never leaves through events or
  webhooks (CLAUDE.md). The gateway also rate-limits `POST /whip` per
  client IP itself, before dialing the relay.

  **Where the gateway gets the client IP.** `POST /whip` arrives through the
  public Ingress (§6), so `r.RemoteAddr` is the ingress controller for every
  OBS user. Keyed on it, both the gateway's limit and `Gawk-Publisher-Ip`
  would collapse into one shared bucket, the failure `gawk-telemetry`'s
  ingest already hit (`internal/ingest/ingest.go`). And `X-Forwarded-For` is
  attacker-controlled unless a trusted-proxy list says otherwise
  (`oidcauth/http.go`). So the gateway takes the client IP from
  `X-Forwarded-For` **only when `RemoteAddr` is inside `trustedProxyCidrs`**,
  by these rules:
  - `trustedProxyCidrs` lists **only the addresses of the proxy that writes
    the header**: the ingress controller's pod range, or, when it runs
    `hostNetwork`, the exact node addresses (`/32`, `/128`), never a subnet
    that clients share. On a homelab the node subnet is usually the LAN the
    broadcaster sits on. **Never** node CIDRs that sit in front of the proxy
    as a kube-proxy SNAT hop: in append mode (`compute-full-forwarded-for`) a
    trusted SNAT hop would make the walk skip past it to a forged entry.
    Dual-stack clusters list both address families. The chart default is
    `[]`.
  - The ingress controller must see real client IPs (its Service on
    `externalTrafficPolicy: Local`, or PROXY protocol). Otherwise the header's
    last entry is a node IP and every user shares a few buckets. §6's chart
    notes and the self-hosting guide say so.
  - All `X-Forwarded-For` header lines are joined in order, comma-split and
    trimmed. Each entry is parsed with `netip` (an optional `[v6]:port` or
    `v4:port` form is accepted) and canonicalised with
    `moderation.CanonicalAddr` (zone stripped, v4-mapped unmapped) **before**
    the CIDR match and before it's sent as `Gawk-Publisher-Ip`. Otherwise
    `::ffff:203.0.113.7` and `203.0.113.7` would be two buckets and a ban for
    one would miss the other.
  - The walk goes from the right and takes the first entry not in
    `trustedProxyCidrs`. If it hits an unparsable entry it **stops** rather
    than skipping it, since skipping would let an attacker shift the walk with
    junk. If **every** entry is trusted, it takes the **left-most** entry. A
    client outside the trusted range can't produce an all-trusted chain,
    because the proxy appends the real address it saw, so only a client
    inside the range can choose a value, and only another in-range one. This
    keeps a LAN broadcaster working if the list does cover clients. If the
    walk stops on junk, or the header is missing, the `POST` gets `400`
    (`client_ip_unknown`) and a metric counts it: a misconfigured Ingress
    fails loudly instead of collapsing into one shared bucket.
  - From a `RemoteAddr` outside `trustedProxyCidrs` the header is ignored and
    `RemoteAddr` (canonicalised) is the client IP. Empty `trustedProxyCidrs`
    means `RemoteAddr` only and never `400`, so a deployment without an
    Ingress (the compose stack, WG9's `whipsink`) works as is. The chart's
    notes warn that behind an Ingress this is one bucket. The silent version
    of that misconfiguration, a non-empty list that doesn't contain the
    ingress's real address (e.g. a dual-stack cluster listing only v4), is
    caught by a metric and a rate-limited warning log for "`X-Forwarded-For`
    from an untrusted peer while `trustedProxyCidrs` is set".
  - IPv6 is keyed per address (/128), the same as the relay's limiter. One
    host on a /64 can rotate addresses past both. That's the relay's existing
    behaviour, and D4 doesn't change it.

  The ICE remote address isn't used: it isn't known until after the `POST` is
  answered, and the relay dial and the rate limit both come first.

- **D5 — H.264 + Opus only, one layer.** The SDP answer accepts H.264
  (packetization-mode 1, any profile the viewers' list can decode; Constrained
  Baseline / Main / High up to level 5.2) and Opus. It **rejects** HEVC, AV1
  and simulcast offers with `406`. OBS then fails the connect, and D9 tells
  the user why. VP8/VP9 from other WHIP senders are a later addition if anyone
  needs them; the engine's config derivation is H.264-only today.

- **D6 — The adapter waits about one round trip for a retransmit, then
  drops** (OD6, owner 2026-11-01). RTP is reassembled into whole access units
  in a reorder window sized to the OBS → gateway path, so a NACKed packet
  usually gets one chance to arrive:
  - The gateway sends NACKs as standard WebRTC and measures the path's RTT
    from them: the time from a NACK to the retransmitted packet's arrival.
    As in RFC 6298, the **first sample sets the RTT directly**; later samples
    are smoothed (EWMA, α = 1/8, like TCP's SRTT). Before the first sample it
    assumes 50 ms (a knob).
  - The window is `clamp(1.25 × RTT + 5 ms, 20 ms, 150 ms)`; the floor, the
    cap, the multiplier and the starting RTT are gateway knobs. Before any
    sample that is 67.5 ms, so a session's first loss can wait that long. From
    the first recovered loss on, a LAN is at the 20 ms floor and a 60 ms
    internet path at ~80 ms.
  - A frame still incomplete when its window closes is **dropped**, and the
    deltas after it are dropped until the next IDR, exactly as a viewer
    handles loss today. The window delays frames only while the frame in
    front of them is missing a packet; a frame that arrives whole with no
    incomplete frame ahead of it is passed on at once and never waits for
    the window.
  - The latency cost is therefore paid **on loss**, not on every frame: a
    lost packet's frame (and the frames queued behind it) are held up to one
    window, then either recovered or dropped. The cap keeps "favour dropped
    frames over stalled playback" true on a bad path: no frame waits more
    than 150 ms.
  - WG0 measures the loss rate, the share of losses one retransmit recovers,
    and the added latency on loss on the reference path, and confirms or
    changes the floor, cap and multiplier.

- **D7 — The GOP is configured at the source, measured at the gateway,
  bounded by the gateway.** OBS can't be asked for keyframes (§3.3), so:
  - The guide (and the web app's OBS page, §5.4) tells the user to set
    **Keyframe Interval = 1 s**, the smallest the UI allows. The guide gives
    the custom-options spelling for 0.5 s on x264 and NVENC as an advanced
    option, once WG0 has verified it.
  - The gateway measures the IDR spacing from the **RTP timestamps of IDR
    starts**: the first packet of an access unit whose NAL (or FU-A / STAP-A
    inner) type is 5. It records them as packets arrive, before D6 can drop the
    frame, so a lost or late keyframe doesn't look like a long GOP. It judges
    the **median of the last 5 spacings**: **≤ 1.1 s**: fine. **≤ 2.2 s**:
    accepted, with a warning in the status (D9). **> 2.2 s**: the gateway ends
    the session with reason `gop_too_long`. Before 5 spacings exist, it ends
    early only after **two consecutive gaps of more than 4.5 s** with no IDR
    start (OBS's "auto" GOP at ≲ 55 fps; at 60 fps "auto" is ~4.2 s and the
    median rule ends it, after about 20 s of ~4 s freezes), which one lost IDR
    start can't produce at a GOP the bounds allow. The 4.5 s threshold can't
    come down without breaking that safeguard. The thresholds are gateway
    knobs.
  - **Backoff after a session the gateway ends itself.** For `gop_too_long`
    and every other reason the gateway ends a session (B-frames, a stream
    that isn't valid H.264), it answers that key's `POST`s with `422` and the
    reason for a backoff that starts at 30 s and doubles on each repeat up to
    10 minutes. That stops OBS's auto-reconnect from looping viewers through
    freezes. The gateway can't check a GOP before answering, so it can't tell
    a reconnect with the same bad settings from one where the user fixed OBS.
    The OBS page's **Retry now** (§5.4) clears the backoff for its key with
    `DELETE /whip/status` (same bearer key), so a user who has fixed OBS
    never waits it out and never has to reset the key.
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
  | ICE/consent failure (OBS gone) | engine `Stop()` (closes with code 0, as today), records `lost` | publisher gone, **grace** (`-broadcast-grace`, 5 min); OBS's auto-reconnect reclaims the same ID |
  | Second OBS with the same key | new session reclaims (not counted against `maxSessions` when the first session is live on this pod or a peer) | **4004** to the first; the gateway tears down the first PeerConnection |
  | Gateway at `maxSessions` (a new key) | `503` + `Retry-After: 30`, no relay dial | — |
  | Relay 4002 (drain) | engine auto-resumes, sends PLI | resume on another pod |
  | The gateway ends the session itself (D7: `gop_too_long`, B-frames, invalid H.264) | engine `StopBroadcast()`, records the reason, starts D7's backoff | close **4014**, broadcast ends; viewers wait on the code |
  | Relay 4006 (operator kill) | ends the session, records `killed` | — |
  | Relay `451` (ID or IP ban) | `451` to OBS, recorded as `banned` | — |
  | Relay `409` (ID reserved for a PoP) | `409` to OBS | — |
  | Relay `429` (caps, rate limit) | `429` + `Retry-After` | — |
  | Relay `401`/`403` (bad secret or token) | `401`/`403`, recorded as `rejected` (never `banned`; the relay keeps `451` for that on purpose, `server.go` `rejectBanned`) | — |

- **D9 — The web app is where the user sees the gateway's verdict.** OBS
  shows only "could not connect" (§3.3), so the gateway exposes
  `GET /whip/status` (same bearer key, CORS-allowed for the configured
  `gawk-app` origins). It returns `{state, codec, resolution, fps, gopMs,
  bitrateKbps, lossPct, viewers, warnings[], lastError, retryAfterMs}` for
  the key's current or last session. `state` is `idle`, `live` or `ended`.
  `viewers` is the relay's "N watching" count, which the engine already
  receives (`Callbacks.OnViewerCount`). `retryAfterMs` is the time left on
  D7's backoff, absent when none runs. `warnings[]` and `lastError.reason`
  come from a fixed set that WG7 defines and §5.6 gives copy for. A `POST`
  refused before any session exists (`503` at `maxSessions`, `400
  client_ip_unknown`, `406` codec or simulcast, relay refusals including
  `451 banned`) is recorded for 10 minutes and reported as `lastError`, so
  the OBS page can show the reason OBS can't. A `POST` refused with `422`
  during D7's backoff, such as OBS's automatic reconnect right after the
  end, **records nothing new**: `lastError` stays the reason that started
  the backoff, with `retryAfterMs`, so the page keeps S4's cause and fix.
  The backoff is checked **first**, before the cap and the SDP checks, so a
  `503` or `406` can't replace its reason while it runs. **A session that
  goes live clears the key's `lastError`**, so a stop after a good session
  reads as an end (§5.4 S5), never as an older refusal or an earlier `lost`.
  Records are keyed by a hash of the **full key**, never by the ID alone, so
  a code doesn't let anyone set another broadcaster's `lastError`. The table
  is bounded (an LRU, 4096 entries by default), because a `503` or `400`
  comes before the relay dial and the gateway can't verify a key it never
  dialed with. So `GET /whip/status` answers `401` only for a key that
  doesn't parse. For a well-formed key it doesn't know, it answers an empty
  `idle` status. `DELETE /whip/status` clears D7's backoff for the
  presenting key **and the `lastError` that started it**, which, since a
  `422` records nothing new, is the only one the backoff produced. The OBS
  page (§5.4) polls it every 2 s while its tab is visible, and the
  broadcaster page reads it once on load when a key is saved (§5.3). The
  status holds no IPs and no frame data.

- **D10 — Several replicas from the start** (OD7, owner 2026-11-01). ICE
  binds a WebRTC session to the pod that answered the `POST`, so both the
  media path and every HTTP request for a key must reach that pod:
  - **Media: one UDP port per pod.** The gateway is a StatefulSet. Each pod
    runs pion ICE-lite on its own UDP mux port, `media.basePort + ordinal`,
    with an ICE-TCP passive candidate on the same port number as the
    fallback. A per-pod `LoadBalancer` Service selects
    `statefulset.kubernetes.io/pod-name`, and all of them share one address
    (MetalLB's `allow-shared-ip`, the reference deployment's LB). Each pod
    maps that address 1:1 (`SettingEngine.SetNAT1To1IPs`) and answers with a
    host candidate carrying **its own** port, so the UDP flow can only reach
    the pod that holds the session. The chart renders one Service per
    replica, so changing `replicas` is a `helm upgrade`, and the firewall
    opens the port range.
  - **HTTP: the Ingress routes by key.** `POST`, `DELETE` and
    `GET`/`DELETE /whip/status` all carry the bearer key, and the Ingress
    hashes on it (ingress-nginx
    `nginx.ingress.kubernetes.io/upstream-hash-by: "$http_authorization"`).
    In the steady state every request for a key, and all of D7's and D9's
    per-key state, lands on one pod, and nothing is shared. `OPTIONS`
    preflights carry no key, and they're stateless. An Ingress that can't hash
    on a header isn't supported in v1; the chart notes and the self-hosting
    guide say so.
  - **Scaling and restarts move some keys.** On a **scale-up**, some keys
    hash to a new pod. A live session keeps running on its old pod until it
    ends; OBS's next `POST` reaches the new pod, which reclaims the ID, and
    the relay closes the old pod's session with 4004 (D8's second-OBS row).
    The old pod keeps the key's D7 backoff and D9 records, where the lookup
    below finds them. A **pod restart** is an ICE failure for its sessions:
    OBS reconnects and the key reclaims the same ID, but that pod's D7 backoff
    and D9 records are lost, which only shortens a backoff. A **scale-down**
    is a restart of the removed ordinals: the StatefulSet deletes the highest
    ones and the chart removes their media Services, so their sessions get an
    ICE failure, their records are gone, and their OBS clients reconnect to
    the remaining pods as new keys, counted against those pods' caps like
    any restart. The `RollingUpdate` update strategy restarts one pod at a
    time on a release, and a `PodDisruptionBudget` limits node drains to one
    pod at a time.
  - **A pod without a key's live session or running backoff asks its
    peers.** A key can be hashed to a pod other than the one holding its
    session or its records: after a rescale, and on every rolling release,
    because the Ingress maps a pod's keys elsewhere while it's down and back
    when it's Ready again, while a session that reconnected meanwhile stays
    where it is, and D7's backoff or D9's refusal records are written
    wherever that session ends. A local refusal record or `lost` settles
    nothing, because a peer can hold something newer. So any request whose
    pod holds **neither the key's live session nor its running D7 backoff**
    asks the other pods (found through the StatefulSet's headless Service, on
    an internal port that only gateway pods can reach under the
    NetworkPolicy, authenticated with a shared `internalToken`) for the key's
    session and records. A live session wins over any record wherever it
    is; then a running backoff; otherwise the newest record wins (wall-clock
    timestamps, compared within the cluster):
    - A `DELETE` on a `Location`, or a `GET`/`DELETE /whip/status`, is
      **forwarded** to the pod holding the winner. Otherwise OBS's `DELETE`
      would never reach the session: D8's `StopBroadcast()` wouldn't run, the
      broadcast would fall into the 5-minute grace instead of ending, the OBS
      page would show `idle` for a live stream or miss the last reason, and
      a **Retry now** would clear a local record while a peer's backoff kept
      refusing.
    - A `POST` is **never forwarded**; it reclaims on the pod it lands on,
      and the relay closes a peer's live session with 4004. But it uses what
      the lookup returns: a running D7 backoff on a peer refuses it with
      `422`, and the receiving pod **takes over everything the peer holds
      for the key**: the backoff, its `lastError` and the ended session,
      which the peer then drops. So the ladder isn't bypassed, there is only
      ever one copy for D9's **Retry now** to clear, and a clear leaves the
      ended session where it now is, so the page shows S5 as it would on one
      pod. A live session on a peer exempts it from the cap, like a same-pod
      reconnect.
    - So an old refusal left on one pod never hides a stream that is live
      on another, never lets a `POST` past a peer's backoff, and never costs
      a reconnect its cap exemption.
    - **A takeover is acknowledged.** The peer drops its records only when
      the receiving pod confirms it holds them. A peer that doesn't answer
      within 200 ms counts as having nothing, and keeps its records if it
      answers later: a stale copy, bounded by its backoff or the 10-minute
      record lifetime, which is no worse than a restart.
    - With a handful of pods the lookup is one small request per peer. It
      runs on every request for a key whose live session and backoff are
      elsewhere, so an S2 page polling an idle key asks every peer each 2 s,
      which is fine at this scale.
  - **Load and the cap.** Load is bounded by concurrent OBS sessions, not
    viewers. `max-broadcasts` doesn't size it: that cap counts relay
    broadcasts (per pod, and in cluster mode fleet-wide at Lease create; 5 for
    `gawk-server`, 50 for `gawk-relay` in `internal/config/relay.go`), not a
    gateway pod's CPU and memory, and gateway sessions count against it
    alongside every other broadcaster's. The relay's `429` when the fleet is
    full reaches OBS through D8. So each gateway pod has its own cap, which
    also keeps WHIP from using up the fleet's broadcast budget: `maxSessions`
    (knob, **per pod**, default 20). A `POST` over it gets `503` with
    `Retry-After: 30`. A `POST` for a key whose session is live, or still
    being torn down, on that pod **or on a peer** (the lookup above) replaces
    that session and **isn't counted against the cap**. That covers a
    second OBS, a reconnect after an ICE blip, and a key that moved after a
    rescale or rollout. A same-pod replacement leaves the pod's count
    unchanged, but a session taken over from a peer adds one here (the peer
    drops one when the relay sends it 4004), so the exemption alone would let
    a pod grow without bound, e.g. when keys hash back to a pod after a
    rollout. A **hard per-pod ceiling**, `maxSessionsHard` (knob, default
    `1.25 × maxSessions`), bounds it: past it even an exempt `POST` gets
    `503`. The pod's CPU and memory are sized for `maxSessionsHard`, not
    `maxSessions`. The exemption **doesn't** cover the reconnects after a pod
    restart: that pod's sessions are gone, so its OBS clients `POST` as new
    keys to the surviving pods, and a full one answers `503`. OBS retries
    with its own backoff and the broadcasts wait in the relay's 5-minute
    grace, so viewers keep the code; but size for it. Hashing spreads keys
    unevenly, so the capacity is `replicas × maxSessions` only on average,
    and only `(replicas − 1) × maxSessions` is safe through a restart; the
    chart notes say so. WG0 measures CPU and memory per 1080p60 session, and
    WG4 sizes the defaults and the pod's resource requests (for
    `maxSessionsHard`) from that number.

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

- **A fixed 20 ms reorder window** (the first draft's D6). The owner chose
  recovering retransmits over the lowest latency on loss (OD6). The window
  costs nothing for frames that arrive whole, and the 150 ms cap keeps a bad
  path from stalling playback.
- **One replica** (the first draft's D10). The owner chose to scale out from
  the start (OD7). A shared state store was not needed: hashing on the key at
  the Ingress puts a key's requests and per-key state on one pod in the steady
  state, and the peer lookup (D10) covers a session that has moved.

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
- **Mint by starting and immediately stopping a broadcast.** See D3 (OD2).
- **Raising `KEYFRAME_STALL_MS` or the DVR window for long GOPs.** That
  weakens every broadcast for the sake of a misconfigured one. The gateway
  bounds the GOP instead (D7).

## 5. The web app: the OBS page and its way in

OBS is a power-user path. Most people who open the broadcaster page should
press **Start a stream** and never think about it, and nothing on the page
should suggest they're missing a step. But someone who already streams with
OBS should find the way in on their own, without reading a guide first. The
design has two parts: a **way in** on the broadcaster page, kept outside the
main flow (§5.3), and an **OBS page** of its own, `#/broadcast/obs`, that
holds the key, the OBS settings and the live status (§5.4).

### 5.1 The design pass

Drawn in Claude Design on 2026-11-02 on the web app's real tokens
(`gawk-app/src/styles/global.css`) and the broadcaster's real markup: 11
boards in three rows.

- **Canvas**: <https://claude.ai/artifact/TtFyDhdhEv5x13oSmjds2m>, owned by
  the maintainer. It is **private**: ask the maintainer for access or an
  export. Its absence is not "no design exists". The boards are linked for
  Play: each way in opens the OBS page, **Create stream key** moves to the
  setup, **Show** reveals the key, **Retry now** and the settings
  disclosure work.

Where this document and the canvas disagree, this document wins.

| Board | What it shows |
|---|---|
| **A** A third line under Start | Option A of §5.3 |
| **B** Inside Sharing tips | Option B, the tips open |
| **C** In Settings | Option C, the Settings sheet open |
| **D** A footnote under the card | Option D, chosen (OD9) |
| **D, returning user** | D once this browser holds a key and OBS is live (§5.3) |
| **OBS page · first visit** | S1: no key yet (§5.4) |
| **OBS page · key ready** | S2: the three steps, waiting for OBS |
| **OBS page · live** | S3 |
| **OBS page · keyframe warning** | S3 with a D7 warning |
| **OBS page · gawk ended the stream** | S4: D7 ended the session, the backoff runs, **Retry now** |
| **OBS page · reset key** | The **Reset key** confirmation |

Not drawn, so this text is the spec: the page at phone width (§5.4), S5
after a stream ends (§5.4), the **Paste a key** field (§5.7), the page on a
server without a gateway (U3),
and the publish-secret prompt, which is the broadcaster's existing modal.

### 5.2 Goals and acceptance

| # | Goal | Verified by |
|---|---|---|
| U1 | **New users aren't drawn in.** The pre-start card is unchanged: **Start a stream** stays the only primary action, and the R67 offer stays its only secondary line (`docs/69` G7). The way in sits outside the card with the **Advanced** chip (§5.5). | Component test: the card's buttons and links are those of today; the way in is outside the `GlassPanel` and carries the chip |
| U2 | **OBS isn't hidden knowledge.** On a desktop where the server takes OBS, the way in is visible without opening anything. The page has a stable address, `#/broadcast/obs`, which the OBS guide, `docs/self-hosting.md` and the about site link to. | Component test; WG9's guide and self-hosting section link the route |
| U3 | **No way in where it can't work.** None without `whipGatewayUrl`, none when the selected server isn't the deployment's default (the gateway publishes only to its own relay, OD4), and none on the devices where R67 hides its offer (Android, iOS, iPadOS, ChromeOS: `isDesktopForHandoff`), since OBS doesn't run there. Opened directly without a gateway, or with another server selected, the route says "This server doesn't take streams from OBS" with a link back. A phone or tablet opening it directly gets the full page: no way in leads there, but a shared link still works, and the code and status can be checked from it. | Component tests per case, including the full page under a phone user agent |
| U4 | **The page explains what OBS won't.** Every D9 state, warning and `lastError.reason` maps to a sentence that says what happened and what to change in OBS, by OBS's own menu names (§5.6). No raw status code or reason string reaches the screen. | A Vitest table test over the full reason set: each has copy, a `422` reported with each backoff reason gets that reason's copy, and an unknown reason gets the generic line |
| U5 | **The key is treated as a password.** Masked by default. **Copy** copies the whole key without revealing it. It never enters a URL, a log line, telemetry or the diagnostics copy. | Unit tests; a test that the route's URL and the diagnostics text never contain the token |
| U6 | **A returning OBS user gets straight back.** With a saved key, the way in names the code; when OBS is live on it, it says so (board **D, returning user**). | Component test with a mocked `GET /whip/status` |
| U7 | **Polling stays cheap.** The OBS page polls D9 every 2 s only while the tab is visible, and stops on **Forget key**. The broadcaster page reads it once on load, and only when a key is saved. | Fake-timer tests over `visibilitychange` |

### 5.3 Where the way in lives

Four placements were drawn (row 1 of the canvas). Every one leads to the same
OBS page and uses the same advanced cues (§5.5); they differ in how visible
the way in is and what it costs the pre-start card.

| | Placement | What a new user sees | What an OBS user does | Cost |
|---|---|---|---|---|
| **A** | A third quiet line inside the card, under **Open in the desktop app**: "Stream from OBS Studio", smaller than the R67 line, with the chip | Three ways to start under one heading | One click | Breaks U1: the card gets a second alternative, where R67 deliberately kept one quiet line (`docs/69` G7) |
| **B** | The last item in the collapsed **Sharing tips**: "Already stream with OBS Studio? Its scenes, overlays and audio mixer work here too", **Set up OBS** | Nothing unless they open the tips | Two clicks, and only if they think to open tips | Leans towards hidden (U2) |
| **C** | A group at the bottom of the **Settings** sheet: "Stream from another app", **Set up OBS…** | Nothing unless they open Settings | Two clicks, and only if they guess Settings | The least discoverable: Settings is about this tab's stream |
| **D** | A footnote **under** the card, outside the glass, where the landing page keeps its server chip: "Already stream with OBS Studio? **Stream from OBS**" with the chip | One quiet line that plainly isn't part of the card | One click | None to the card |

**Decided: D** (OD9, owner 2026-11-02, as recommended). A, B and C stay on
the canvas as the rejected alternatives. D meets U1 and U2 together. The card
stays what it is, and the position does the explaining: the card is for everyone, the line
under it is for people who already know what OBS is. The copy picks its own
audience. Someone without OBS reads "Already stream with OBS Studio?" as "not
me", and someone with it finds the line on their first visit.

**For a returning user**, D's line earns a little weight. With a key saved for
this server it reads "Your OBS key for **K7Q2XM** is saved · **Open**". When
the one D9 read on load (U7) says OBS is live, it becomes a glass pill with
the LIVE badge: "**LIVE** OBS is streaming on **K7Q2XM** · **Open**". Live,
it is the most useful thing on the page, and the browser broadcast is still
one click away.

### 5.4 The OBS page, `#/broadcast/obs`

A page, not a modal (OD10, owner 2026-11-02, as recommended). The first
draft of this section opened a modal
like `DesktopHandoffModal` from the pre-start card. A modal suits a moment;
this is where an OBS broadcaster watches their status for the whole stream.
As a page, the tab can stay open beside OBS, a guide can link straight to
it (U2), and it never sits over a **Start a stream** button that would start
a second, browser broadcast. It is a new route with the broadcaster's query
(`?relay=`), parsed in `routing.ts`.

The look is the broadcaster's: the same drifting background and glass card,
wider (at most 600 px) and left-aligned, because it holds fields and steps
rather than one call to action. The header is the brand, the kicker
"BROADCAST · OBS STUDIO" and the **Advanced** chip. The footer has **← Back**
(to `#/broadcast`, keeping the query), **Reset key**, **Forget key** and
**OBS setup guide ↗** (WG9's guide). At phone width the label/value grids
stack and the footer wraps. On phones and tablets only the way in is hidden:
the route itself shows the full page (U3).

**S1 · First visit, no key.** At the top, a quiet strip: "New to streaming?
The browser takes one click and needs no other app." with **Start a stream in
the browser**. Then the title "Stream from OBS Studio", one paragraph on what
it is for ("your scenes, overlays and OBS's audio mixer come with you"), and
a **You need** box: OBS Studio 30 or later (or another WHIP app, such as
FFmpeg or GStreamer), an H.264 video encoder (NVENC, AMF, QuickSync or x264),
and a few minutes in OBS's Settings. Only then the primary **Create stream
key**, which mints through D3 with the server's saved publish secret and
asks for the secret with the broadcaster's existing modal when the server
requires one. Beside it, quietly, **Already have a key? Paste it** (§5.7).

**S2 · Key ready, waiting for OBS.** Three numbered steps, each named by
where it happens in OBS:

1. **In OBS, open Settings → Stream.** *Service* `WHIP`; *Server* (the
   WHIP URL, with **Copy**); *Bearer Token* (the key, masked as
   `gawk1_K7Q2XM_••••`, with **Show** and **Copy**); *Simulcast* `Off`, "if
   your OBS shows it". Under them, with a lock icon: "The token works like a
   password: anyone who has it can stream on your code. Keep it off screen."
   (An OBS user's screen is often being captured.)
2. **Then Settings → Output.** *Output Mode* `Advanced` (Keyframe Interval
   isn't in Simple); *Video Encoder* `H.264` (NVENC, AMF, QuickSync or x264);
   *Keyframe Interval* `1 s`, "not 0 (auto), which is often 4 s or more";
   *Audio Encoder* `Opus`. WG0's custom-options spelling for 0.5 s (D7) goes
   in the OBS guide, not here.
3. **Press Start Streaming in OBS.** A status box with a pulsing accent
   dot: "Waiting for OBS…", "This page updates on its own when OBS connects."

Then **Your viewers join on**: the code in the six join-code boxes and
**Copy link** (`buildViewLink`). The first draft mentioned "the existing copy
and QR affordances"; `gawk-app` has no QR code anywhere, so there is none
here. Labels and values are OBS's own words in monospace; WG0 checks each
label against the OBS release it tests.

**S3 · Live.** The steps fold into a closed **OBS settings for this key**
disclosure. On top, the live topbar's pieces: the LIVE badge, the code pill
with copy, and "N watching" (D9's new `viewers`). The title "You're live from
OBS". A four-cell grid from D9: *Video* (`1920×1080 · 60 fps`), *Codec* (`H.264`
✓), *Keyframes* (`every 1.0 s` ✓), *Upload* (`6.0 Mb/s · 0.1 % lost`). The
checks mean the gateway measured it: the frames, not the settings, as
CLAUDE.md asks. Then the primary **Copy link** (as on every live screen,
`docs/60` D3), **Watch as a viewer ↗** (a viewer tab, which counts as a
viewer), and "Stop Streaming in OBS ends the broadcast. Next time, your code
is K7Q2XM again." The page has no Stop of its own: OBS's `DELETE` is the stop
(D8), and the page holds no session.

**S3 with a warning.** A D7 warning is an amber banner above the grid, the
same weight as the broadcaster's live hints, and the cell it is about turns
amber. Example: "**OBS sends a keyframe every 2.0 s.** At 1 s, viewers who
join or hit a glitch see a moving picture again twice as fast." and "In OBS:
Settings → Output → Keyframe Interval → `1 s`. It applies the next time you
start streaming."

**S4 · gawk ended the stream.** When the gateway ends a session itself (D7)
the title is "gawk ended your stream", with "Viewers on K7Q2XM are waiting;
they pick the stream up again when you're back." A red box gives the measured
cause and the fix (§5.6). Under it, while the backoff runs: "If OBS says it
failed to connect, that is gawk turning it away for 0:48 more, so viewers
don't freeze in a loop." and the primary **Retry now** ("once you've changed
the setting"), which sends `DELETE /whip/status`. That clears the backoff
and its reason (D9), so the page turns into S5: "Your stream has ended",
"Press Start Streaming in OBS to go live again on K7Q2XM.", also after a
rescale moved the key to another pod, since D10 moves the ended session with
the backoff. The countdown
comes from D9's new `retryAfterMs`. A refusal with no backoff (a codec, the
gateway full, a ban) uses the same red box without the countdown or the
button.

**S5 · Ended.** After OBS's **Stop Streaming** (D8's `DELETE`, close 4014),
or **Retry now** after S4, the page shows "Your stream has ended" and "Press Start Streaming in OBS to
go live again on K7Q2XM.", with the S2 steps folded as in S3. Viewers who
stayed on the code pick the stream up again (R78). If OBS vanished instead
(D8's ICE failure, recorded as `lost`), it says "OBS disconnected" and "If
OBS is still open, it reconnects on its own. If not, press Start Streaming
in OBS again; your code stays K7Q2XM." It promises no reconnect and no
waiting time: `lost` also covers OBS closed, crashed or asleep, OBS's
automatic reconnect can be off or run out of retries, and the relay's grace
period isn't known to the app (as in `docs/60` D11). Neither is red: an end
the user chose isn't an error. A gateway pod restart loses that pod's
records (D10), so its sessions never show `lost`: the page shows S2's
"Waiting for OBS…" until OBS reconnects, which is accepted.

The status box, the warning and the ended box are live regions: `status` for
waiting, live, warnings and S5, `alert` for S4 and refusals.

### 5.5 The advanced cues

The same five everywhere, so the signal is consistent rather than loud:

1. **The ADVANCED chip.** A neutral outline pill (`--border` outline,
   `--muted` text, uppercase, letter-spaced like the kicker), never amber:
   in gawk, amber means something is wrong (`--warning` hints and badges).
   It is on the way in and in the page's header.
2. **Copy that names its audience.** "Already stream with OBS Studio?" and
   "For people who already stream with OBS". It never calls OBS "pro" or
   "better", and never sells it.
3. **Position.** Outside the card (D), after everything a first broadcast
   needs.
4. **A way back before any button.** S1 opens with the browser alternative
   and lists what you need before it offers **Create stream key**.
5. **OBS's own words, in monospace.** Menus and fields read as OBS shows them
   (*Settings → Output*, *Keyframe Interval*), which tells an OBS user they're
   in the right place and tells everyone else they aren't.

### 5.6 What each state says

The page maps D9's `state`, `warnings[]` and `lastError.reason` to these
lines (U4). `reason` is a fixed set the gateway defines in WG7; the wording
is the app's and can change without a gateway release. A `422` during a
backoff has no row of its own: it reports the backoff's reason (D9).

| D9 | Where | Copy |
|---|---|---|
| `state: idle` | S2 step 3 | "Waiting for OBS…" / "This page updates on its own when OBS connects." |
| `state: live` | S3 | "You're live from OBS" |
| `state: ended`, no `lastError` (OBS's Stop Streaming, or **Retry now** after S4) | S5 | "Your stream has ended" / "Press Start Streaming in OBS to go live again on {code}." |
| `state: ended`, `lost` (OBS vanished, the broadcast in grace) | S5 | "OBS disconnected" / "If OBS is still open, it reconnects on its own. If not, press Start Streaming in OBS again; your code stays {code}." |
| warning `gop_long` (1.1–2.2 s) | S3 amber banner | "OBS sends a keyframe every {g} s. At 1 s, viewers who join or hit a glitch see a moving picture again sooner." / "In OBS: Settings → Output → Keyframe Interval → 1 s. It applies the next time you start streaming." |
| `gop_too_long` (backoff) | S4 | "OBS sent a keyframe only every {g} s. Viewers who joined, or hit a glitch, saw a frozen picture for seconds at a time." / "In OBS: Settings → Output → Keyframe Interval → 1 s. If it says 0 (auto), that is the cause." |
| `bframes` (backoff) | S4 | "Your encoder sends B-frames, which gawk can't play live." / "Set B-frames to 0 in the encoder's settings (Settings → Output), then retry." |
| `invalid_h264` (backoff) | S4 | "gawk couldn't read the video OBS sent." / "Choose another H.264 encoder in Settings → Output, then retry." |
| `codec` (`406`) | refusal | "OBS is sending {AV1 / HEVC}. gawk takes H.264." / "In OBS: Settings → Output → Video Encoder → an H.264 one." |
| `simulcast` (`406`) | refusal | "OBS is sending several layers (simulcast)." / "Turn Simulcast off in Settings → Stream." |
| `gateway_full` (`503`) | refusal | "This server is streaming as many OBS broadcasts as it can." / "Wait a minute, then press Start Streaming in OBS again." |
| `relay_busy` (`429`) | refusal | "This server has no room for another stream right now." / "Try again in a few minutes." |
| `rejected` (`401`/`403`) | refusal | "The server didn't accept this key. It may have been reset by whoever runs it." / "Reset the key here, then paste the new one into OBS." |
| `banned` (`451`) | refusal | "The server's operator has blocked this stream." |
| `reserved` (`409`) | refusal | "This code is in use on another gawk server." / "Reset the key to get a new code." |
| `client_ip_unknown` (`400`) | refusal | "This server is set up wrong: it can't see where your stream comes from." / "Tell whoever runs it." |
| `killed` (relay 4006) | refusal | "The server's operator ended this stream." |
| any other | refusal | "OBS couldn't stream to gawk." / "Check the three steps above, then press Start Streaming again." |

### 5.7 The key in the browser

- **Storage** (OD3): per server in `localStorage`, next to that server's
  saved publish secret, so the same key and code come back next time.
- **Forget key** asks first: "Forget this key? OBS keeps streaming with it;
  this browser just stops showing its status. Copy it from OBS (Settings →
  Stream) to see it here again." It removes the key and stops polling.
- **Reset key** asks first (board **reset key**): "You get a new key and a
  new code. Paste the new key into OBS before you stream again, and share
  the new link." and "The old key keeps working on K7Q2XM; gawk can't switch
  it off. If it leaked, ask whoever runs this server to ban K7Q2XM." **Reset
  key** is the danger button, **Cancel** the other. It mints through D3.
- **Paste a key** (S1): a field that accepts a `gawk1_…` key, for the same
  OBS on a second computer or after **Forget key**. The page checks only the
  format (D2), saves it and starts polling. The gateway's answer for a key
  it doesn't know is `idle` (D9), so a typo looks like "Waiting for OBS…"
  until OBS connects with it.

**Rooms** (later, WG9): a **Room** row under **Your viewers join on**, with
the broadcaster's Room panel, attaches the OBS broadcast to a room with the
ID and token the page already holds (`docs/44` D8: attach is keyed by ID +
token, not by the publisher session). WG9 verifies that this works for a
broadcast whose publisher is the gateway.

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
- **Chart:** based on `gawk-mumble-bridge`'s, with a StatefulSet (D10,
  `replicas` default 2), an HTTP Service for the WHIP endpoint behind the
  public Ingress (TLS by the Ingress, `upstream-hash-by` on the bearer key),
  one UDP+TCP `LoadBalancer` Service per replica on a shared address (port
  `media.basePort + ordinal`), a headless Service for D10's peer lookup, a
  NetworkPolicy (egress to the relay's UDP port and to the other gateway pods'
  internal port; ingress from the Ingress, the media LBs and the other gateway
  pods on the internal port only), a ServiceMonitor and a PDB. `values.yaml`
  holds `relay.url`, `relay.publishSecret` (existing secret ref),
  `relay.publishProxyToken` (D4), `publicAddress`, `replicas`,
  `media.basePort` and `internalToken` (an existing secret ref) (D10),
  `allowedOrigins` (D9 CORS), `trustedProxyCidrs` (D4, default `[]`),
  `maxSessions` and `maxSessionsHard` (D10) and the D6/D7 knobs. Every value
  has a flag and a `GAWK_WHIP_*` env (the `registryOptions` rule, applied to
  the gateway's own config). The chart's notes carry D4's requirements:
  `trustedProxyCidrs` lists only the header-writing proxy (exact node
  addresses under `hostNetwork`, both address families on dual-stack), the
  ingress controller must see real client IPs (`externalTrafficPolicy: Local`
  or PROXY protocol), and an empty list behind an Ingress means one shared
  bucket. They also carry D10's: the Ingress must hash on the `Authorization`
  header, and the firewall must open `media.basePort` to `media.basePort +
  replicas - 1`, and only `(replicas − 1) × maxSessions` sessions are safe
  through a pod restart.
- **Frontend config:** `whipGatewayUrl` in `window.__GAWK_CONFIG__`
  (`gawk-app/src/config.ts`), set by the frontend chart's values. When it's
  unset there is no way in, and `#/broadcast/obs` says the server doesn't
  take streams from OBS (U3).
- **Dev stack:** a `whip` compose profile with the gateway and a GStreamer
  `whipsink` (or `ffmpeg -f whip`) container publishing a test pattern with a
  1 s GOP, so the OBS page shows a live stream with nothing to install.
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
| Gateway reorder window (D6) | 0 ms for a whole frame with no incomplete frame ahead of it; on loss (for the lost packet's frame and the frames queued behind it) up to `clamp(1.25 × RTT + 5, 20, 150)` ms (67.5 ms before the first RTT sample; then 20 ms on a LAN) | same |
| Gateway → relay → viewer | as today | as today |
| **Glass-to-glass, LAN** | **~60–120 ms** | **~100–200 ms** |

## 8. Chunks and acceptance criteria

| Chunk | Scope | Accepted when |
|---|---|---|
| **WG0** | Lab: OBS (latest release, Windows NVENC and x264, 1 s and 2 s GOP) → a minimal pion WHIP receiver → the pubsim engine → the compose relay → the shipping viewer. Re-check §3.3 on that release (PLI ignored, `bf=0`, keyint granularity), and find the sub-second custom-options spelling per encoder. | Glass-to-glass p50/p95 on LAN and with 1 % / 40 ms netem, recorded in §7. §3.3 confirmed or corrected. Loss rate OBS → receiver on the reference path, the share of losses one retransmit recovers and the latency added on loss recorded, and D6's floor, cap and multiplier confirmed or changed. CPU and memory per 1080p60 session recorded for D10's `maxSessions`. |
| **WG1** | Promote the engine to `gawk-server/publisher` (OD1), and add `StopBroadcast()`, which closes the session with **4014** (`Stop()` closes with code 0 today, so the relay can't tell a stop from a drop). `gawk-pubsim`, `e2e`, `e2e-cluster` and the iOS viewer tests keep using `Stop()` unchanged. CONTRIBUTING gains the release-coupling line. | `go test ./...` and the e2e lanes green. No `internal/pubsim/engine` import left outside the package. A test: `StopBroadcast()` ends the broadcast at once (R78), while `Stop()` leaves it in grace. |
| **WG2** | Relay: `CONNECT /publish?mint=1` (D3) and the new close code in all three wire copies with golden vectors. | Tests: a mint announces ID + token, creates no broadcast, emits no event, isn't counted in `max-broadcasts`, and passes every publish gate (secret, rate limit, IP ban, draining). A reclaim with the minted token creates the broadcast. Vectors byte-identical in Go, TS and Rust. |
| **WG3** | Relay: `-publish-proxy-token` (D4) through `registryOptions`, flag + env + Helm, and D12's guard. | Tests: with the token, `Gawk-Publisher-Ip` drives rate limit, IP ban and the admin publisher-IP fields, and the Origin check is skipped. Without it the header is ignored. A test asserts no event or webhook carries the IP. The Helm value renders into the relay args. The relay refuses to start, and the chart to render, with the proxy token and no explicit `resumeTokenKey`. A gateway peer inside `trustedCidrs` (and one on loopback) presenting the token with a proxied IP is still rate-limited on that IP. `::ffff:203.0.113.7` and `203.0.113.7` in the header hit the same bucket and the same ban. An unparsable or missing header with a valid token gets `400`, never the peer address. The release notes and §9 carry D12's one-time token migration. |
| **WG4** | Gateway skeleton: module, config (flags + `GAWK_WHIP_*` + chart values), `/healthz`, metrics, Dockerfile, chart (default off), release-please package, a CI path filter on `gawk-whip-gateway/` and `gawk-server/`. | CI builds and tests it. The chart renders with defaults and does nothing until enabled. `maxSessions`, `maxSessionsHard` and the resource requests (sized for `maxSessionsHard`) are set from WG0's per-session measurement. With `replicas: 3` the chart renders a 3-pod StatefulSet, three media Services on one shared address with ports `basePort`…`basePort+2`, and the Ingress hash annotation. A release produces image + chart at the same version. |
| **WG5** | WHIP endpoint: `POST`/`DELETE`/`OPTIONS`, key parsing (D2), SDP answer (D5), ICE-lite + per-pod UDP mux port + NAT 1:1 + ICE-TCP (D10), the D6 reorder window, the D8 status mapping, per-IP `POST` rate limit. | A pion WHIP client test in CI: H.264+Opus accepted (`201` + `Location`); HEVC, AV1 and simulcast refused `406`; bad key `401`; every row of D8's table covered against a test relay. Each pod answers with a candidate on its own `basePort + ordinal`. Two gateway instances behind a hashing proxy: a key's `POST`, `DELETE` and status reach the same instance, and the same key's `POST` landing on the other instance (as after a rescale) reclaims the ID while the relay closes the first with 4004. With the hash moved away from the instance holding a live session (as after a rescale or while a pod restarts during a rollout), a `DELETE` on its `Location` reaching the other instance still ends the broadcast with 4014; when the instance holding the key goes away (a scale-down), OBS's reconnect to the remaining one is admitted as a new key within its cap and reclaims the same ID; and `/whip/status` there still reports it live; the peer lookup is refused without `internalToken`. D6: with netem loss, a NACKed packet that returns within the window recovers its frame, one past it drops the frame and the deltas to the next IDR, a whole frame with no incomplete frame ahead of it is never delayed, a frame queued behind a loss waits at most one window, and the window follows the measured RTT within its clamp. Client IP: from a `RemoteAddr` inside `trustedProxyCidrs` the right-most untrusted `X-Forwarded-For` address is used, from outside it a forged `X-Forwarded-For` is ignored, and the chosen address is what reaches the rate limit and `Gawk-Publisher-Ip`. An append-mode chain `forged, <client>` arriving from an ingress pod `RemoteAddr`, with only the ingress pod range trusted, yields `<client>` and never `forged`. Several header lines are joined in order. An unparsable entry stops the walk and gets `400`, and so does a missing header. An all-trusted chain yields its left-most entry. With an empty `trustedProxyCidrs` a forged header is ignored and no request gets `400`. A header from an untrusted peer while the list is set increments the misconfiguration metric. A v4-mapped entry is canonicalised. At `maxSessions`, a new key gets `503` + `Retry-After` while the same key's reconnect succeeds, both on the same instance and when its session is live on the other instance; at `maxSessionsHard` even that reconnect gets `503` when its session is on the other instance. A key whose session ended on the other instance with a running D7 backoff gets `422` on this one, which then holds everything the other instance held for the key (the backoff, its `lastError` and the ended session), and the other instance holds nothing for it (a status read there is forwarded or `idle`); a **Retry now** (`DELETE /whip/status`) on this instance then lets the next `POST` through on either. A `GET /whip/status` reaching an instance without the key's records reports the other instance's `lastError`, and one reaching an instance that holds an old refusal reports the key's session live on the other instance. A `POST` reaching an instance that holds an old refusal still gets `422` from the other instance's running backoff, and is exempt from the cap when the key's session is live there; a **Retry now** on that instance clears the other instance's backoff. A takeover the receiving instance doesn't confirm leaves the other instance's records in place. During a backoff, a `POST` that would also be over the cap or refused `406` gets `422` with the backoff's reason. A peer that doesn't answer within 200 ms is treated as having nothing. |
| **WG6** | Media adapter: RTP → Annex-B AUs with the reorder window (D6), IDR/SPS/B-frame validation, GOP measurement and bounds (D7), Opus → the engine's audio, RTP/RTCP-SR timestamps → engine timestamps, event PLIs. | Unit tests with recorded OBS RTP: a dropped packet drops that frame and the deltas until the next IDR; a B-frame stream is refused with reason; GOP thresholds warn and end as specified (the session ends with 4014 through `StopBroadcast()`), the backoff runs 30 s → 60 s → … → 10 min on repeats and a B-frame refusal starts it too, and a stream with a 2 s GOP that loses one whole IDR (and one IDR's first packet) is neither warned about more than it would be without the loss nor ended; A/V offset within 20 ms of the source; PLI sent on start and on resume only: never on a dropped frame, never periodically. |
| **WG7** | `GET` and `DELETE /whip/status` (D9) with CORS, and the bounded refusal table. The fixed `warnings[]` and `lastError.reason` sets (§5.6). | Tests: returns the D9 fields for the key's session, including `viewers` from the engine's viewer count and `retryAfterMs` while a backoff runs (absent otherwise); every warning and refusal carries a reason from the fixed set; a session that goes live clears an earlier `lastError` (a `503` refusal, an earlier `lost`); a `422`'d `POST` during a backoff leaves `lastError` as the backoff's own reason; after at least one such `422`, `DELETE` clears the backoff and its `lastError` together, so the next `POST` is answered and the next `GET` is `ended` with no `lastError`, also after a D10 takeover; how a session's end is reported: a `DELETE` gives `ended` with no `lastError`, an ICE failure `ended` with `lost`, a D7 end `ended` with its reason and `retryAfterMs`, and a relay 4006 `ended` with `killed`; reports a `503`/`400`/`406`/`451` refusal of a `POST` that never made a session as `lastError`; a different key with the same ID neither sees nor sets that record; a malformed key gets `401` and an unknown well-formed key gets `idle`; the table evicts beyond its bound; nothing in a response contains an IP. |
| **WG8** | gawk-app: the `#/broadcast/obs` route and page (§5.4, OD10), the way in on the broadcaster page (§5.3, OD9) with its returning-user state, `whipGatewayUrl` config, key storage, paste, forget and reset (§5.7, OD3), status polling and the copy for every state (§5.6). | U1–U7 (§5.2), each by its named test. Vitest also covers mint, the publish-secret prompt, show and copy (copy works while masked), and both confirmations. Built against the canvas at desktop and phone width with the `gawk-app:verify` skill. |
| **WG9** | Dev stack `whip` profile with a GStreamer/FFmpeg WHIP sender. Rooms attach from the OBS page (§5.7). `docs/self-hosting.md` section, an OBS setup guide, `docs/gotchas.md` entries. | `docker compose --profile whip up` shows a live test pattern in the viewer with no copy-paste. A `dev-stack` CI job round-trips a frame through the gateway. Attach puts the OBS broadcast in a room. |
| **WG10** | Owner pass on the reference deployment with real OBS on the gaming PC. | Key created in the web app, OBS streams, viewers watch. A drain during the stream doesn't reach OBS. OBS's auto-reconnect keeps the join code. With 2 replicas, restarting the pod holding the session ends in OBS reconnecting on the same code, with the surviving pod below its cap. Glass-to-glass recorded next to the native figure. |

## 9. Security considerations

- **A key is a full publish credential for its code until the relay's
  explicit `-resume-token-key` rotates** (§3.1, D12). It's shown masked,
  stored only in the user's browser, and sent only to the gateway over HTTPS.
  A leaked key can be used until the operator bans the ID or rotates the key,
  which invalidates every key. The OBS page says so (§5.7), and Reset key
  gives the user a fresh code.
- **Turning D12 on is a one-time migration** on a relay that runs on the
  secret-derived key (the chart default, `resumeTokenKey: ""`). Setting an
  explicit key revokes every resume token in use, including the ones the
  desktop apps keep on disk. Their next reclaim fails and falls back to
  minting a new ID, so those broadcasters' viewers lose them once. Do it
  between broadcasts and say so in the release notes, unless the deployment
  already sets an explicit key.
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

OD1–OD8 decided by the owner on 2026-11-01, OD9 and OD10 on 2026-11-02 after
the design pass (§5.1,
[canvas](https://claude.ai/artifact/TtFyDhdhEv5x13oSmjds2m)).

| # | Question | Decision |
|---|---|---|
| **OD1** | Own module (`gawk-whip-gateway/`) with the engine promoted to `gawk-server/publisher`, or a `cmd/` inside `gawk-server`? | **Own module** (§6), as recommended |
| **OD2** | A mint-only publish on the relay (D3), or mint by starting and stopping a broadcast and accept the spurious events? | **Mint-only** (D3), as recommended |
| **OD3** | Keep the key in `localStorage` per server, or show it once and make the user keep it? | **`localStorage`, with Forget** (§5), as recommended |
| **OD4** | Where the panel learns a server's gateway: only `whipGatewayUrl` in `config.js`, or also a per-server field in the R37 server picker? | **`config.js` only** in v1, as recommended |
| **OD5** | GOP bounds: warn above 1.1 s, refuse above 2.2 s (D7)? | **Yes, as knobs**, as recommended |
| **OD6** | D6's reorder window: a fixed 20 ms that drops on a miss, or about one round trip so a retransmit can arrive? | **About one RTT** (D6), clamped to 20–150 ms. Recommended was the fixed 20 ms; the owner chose recovering retransmits over the lowest latency on loss |
| **OD7** | One replica with its own `maxSessions` cap, or several replicas from the start? | **Several replicas** (D10): a StatefulSet with a UDP port per pod and Ingress routing by key. Recommended was one replica |
| **OD8** | Publish the gateway's WHIP endpoint on the official deployment (`whip.gawk.ioio.fi`) once WG10 passes? | **Yes, after WG10**, as recommended |
| **OD9** | Where the way in lives on the broadcaster page: **A** a third line in the card, **B** in Sharing tips, **C** in Settings, or **D** a footnote under the card (§5.3)? | **D**, with the returning-user state, as recommended |
| **OD10** | A page of its own at `#/broadcast/obs`, instead of the first draft's modal on the pre-start card (§5.4)? | **The page**, as recommended |

## 11. Non-goals

- WHEP or any WebRTC viewer path.
- Accepting HEVC, AV1, VP8/VP9 or simulcast (D5).
- A WHIP gateway inside the PoP relay `gawk-relay` (a LAN party with OBS can
  use the home gateway; bundling one in `gawk-relay` is a later milestone).
- Transcoding, scaling, or any change to OBS's encoded bitstream beyond
  re-chunking.
- R73 carriers on the gateway → relay leg (D11).

## 12. Implementation contract (2026-11-02)

WG1–WG9 were built against this section. It records what §4–§8 left to the
chunks and where the build departs from them. Where the two disagree, this
section is what the code does. WG0 (the lab pass with real OBS) has not run,
so every number it was to set (D6's floor, cap and multiplier, D10's
`maxSessions`, the pod's resources) is the provisional value from §4, marked
so in the code and the chart.

### 12.1 Relay (WG1–WG3)

- **WG1.** The engine is `gawk-server/publisher`. `StopBroadcast()` closes
  with 4014, `Stop()` with 0. A terminal relay close reaches `OnError` as a
  typed `*publisher.CloseError{Code}`, which the gateway maps (D8) instead
  of matching text. `Config.Header` rides every publish dial, first and
  resumed, and never overrides Origin.
- **The engine owns its QUIC connection.** webtransport-go's
  `Transport.Dial` closes the connection the instant the session's context
  ends, which is when `CloseWithError` has queued its close capsule, not
  when the capsule has left. On loopback about half of the 4014s then read
  as drops and the broadcast sat in grace. The engine dials QUIC itself and
  lets the connection linger until the relay closes it, at most 2 s
  (`publisher/relay.go`). R78's PoP dialer had the same fix.
- **4017 `CloseCodePublishMinted`** closes a mint-only session, noticed
  in-band first (`011700000fb1`), in all three wire copies. Terminal.
- **Mint-only gate order:** draining 503, the proxy header check (400), the
  rate limit 429, `?mint` validity (400: a value other than `1`, or on a
  `/publish/{id}` claim), the secret 401, the IP ban 451, `CheckPublishNew`
  429, the upgrade, the ban re-check (4006), then BroadcastAnnounce and
  ResumeToken on their own uni streams as on any mint, SessionClosing(4017),
  a 250 ms settle and the close. The connection counter's new outcome is
  `minted`; nothing else is counted.
- **No reservation for a minted ID.** The token is the only way to use it
  later, and a reclaim of an unknown ID creates the broadcast. The draw only
  skips IDs that are live, graced or reserved on this pod, held by an origin
  Lease anywhere in the fleet (asked of the API server, not the informer
  cache), or ID-banned. A PoP relay answers a mint-only `404`: its mint
  gate clears IDs with the home fleet, which a mint-only would skip, and the
  gateway publishes only to its home relay (OD4). The cost of no
  reservation: an OBS key is saved for weeks, so an ordinary mint that later
  draws the same ID while the key is idle is superseded (4004) by the key's
  next Start. The odds are the ordinary mint's one in ~31^6 per mint, the
  same as for any long-lived resume token.
- **A stop during auto-resume still ends the broadcast.** If
  `StopBroadcast()` lands while a reclaim dial is in flight, the session that
  dial returns is closed with 4014 instead of attached. If it lands between
  attempts, with no live session, the engine makes one bounded reclaim and
  closes it with 4014 at once, as the desktop engine does when paused. It
  never does that after a terminal close: a deposed engine (4004) that
  reclaimed would supersede the newer publisher and end its broadcast.
- **Proxy (D4).** A bearer that doesn't match the token is ignored, not
  refused: browsers can't send headers on a WebTransport CONNECT and the
  native apps send none, so ignoring it leaves the sender with what it had.
  The relay logs a mismatch after the rate limit. `Gawk-Publisher-Ip` must
  be exactly one bare address; a port, a CIDR, a list, `::` or a repeated
  header is a 400. The token must differ from `-publish-secret`, which is
  handed to broadcasters. The local-relay offer also matches on the
  proxied address. The bad-header 400 is counted as `bad_request`.
- **D12 on `gawk-relay`.** `gawk-server` refuses the proxy token without an
  explicit `-resume-token-key` when parsing flags. `gawk-relay` keeps a
  durable key in its state directory, which satisfies D12, so the shared
  backstop in `serve.Run` checks the key it actually got.

### 12.2 Gateway (WG4–WG7)

- **Checks on `POST /whip`, in order:** a malformed key 401, not SDP 415,
  D7's backoff 422 (records nothing), the client IP unknown 400, the
  gateway's own per-IP limit 429 (1/s, burst 5), the peer lookup (a peer's
  backoff is a 422 and a takeover), the cap 503, the SDP checks 406, the
  relay dial (D8's mapping), the answer.
- **Not recorded for D9:** the gateway's own 429, a 415 and a malformed
  offer's 400, which have no reason in the fixed set. A relay that can't be
  reached, a 5xx, 404 or 400 from it and a failed resume are all `503` with
  `relay_busy`, the closest reason in the set.
- **NACKs are the gateway's own**, not pion's generator: one NACK per hole,
  sent at once, and only once-NACKed packets are sampled for D6's RTT. A
  packet that was only reordered gives a short sample, which the 20 ms floor
  absorbs. pion v4.2 deprecates `SetNAT1To1IPs`; the gateway uses the
  equivalent `SetICEAddressRewriteRules`.
- **B-frames** are detected from the slice types the engine's own
  `publisher.HasBSlices` reads. Audio is 48 kHz Opus in 20 ms frames, which
  the engine's first-packet check requires; a sender with other Opus
  settings keeps its video and loses audio.
- **Backoff level.** The doc doesn't say when D7's ladder resets: a key's
  record lives one record lifetime (10 minutes) past the end of its backoff,
  and the level goes with it. **Retry now** clears the backoff but keeps the
  level, so a user who retries with the same bad settings still climbs.
- **"Live"** for D9 and D10 means the PeerConnection is connected. A session
  that has been answered but isn't connected yet still exempts a reconnect
  from the cap; one that doesn't connect within 30 s ends as `lost`. A
  same-pod replacement is exempt even at `maxSessionsHard`, since the
  count doesn't grow.
- **Peer routes** (internal listener, bearer `internalToken`, keyed by the
  key's hash, never the key): `GET`/`DELETE /internal/v1/keys/{hash}` (the
  `DELETE` is the takeover acknowledgement), `GET`/`DELETE
  /internal/v1/keys/{hash}/status`, `DELETE
  /internal/v1/sessions/{id}?key={hash}`. Peers come from
  `-peer-template` + `-replicas` or an explicit `-peers` list.
- **No broadcast ID in the gateway's logs**: the engine's `broadcast_id`
  attribute is stripped, and relay dial errors are redacted, since an ID is
  a join capability (CLAUDE.md).

### 12.3 Web app (WG8)

- **The key is stored under its own `localStorage` entry,
  `gawk.obsKey:<relay origin>`,** not inside the server record beside the
  publish secret (OD3's wording). Clearing the default server's secret
  deletes that record, which would have deleted the key with it.
- **Browsers see no HTTP status on a refused CONNECT,** so a failed mint
  can't tell a missing secret from a ban or a rate limit. A refused dial
  opens the broadcaster's publish-secret prompt with its existing "may
  require a publish secret" note and the error.
- **Create stream key goes through the one-time terms acknowledgement**,
  like Start a stream: a key is a licence to broadcast.
- Copy §5.6 doesn't give: a malformed key (`401`): "The server can't read
  this key." / "Forget it here, then paste the key from OBS (Settings →
  Stream) again, or reset it."; the confirmation titles "Forget this key?"
  and "Reset this key?". A refusal keeps S2's layout with the red box in
  step 3, since its fallback copy points at the steps above.

### 12.4 Dev stack (WG9) and what is open

- **The dev sender is FFmpeg's `whip` muxer** (Alpine FFmpeg 8.1), sending a
  test pattern with a 1 s GOP. `gawk-whip-gateway/cmd/gawk-whipsim` does
  what FFmpeg can't: it mints the key over `/publish?mint=1`, attaches the
  broadcast to `devroom`, and checks that a frame comes back, which is the
  `dev-stack` CI job's round trip. A pion sender was not used: the
  gateway's synthetic test media isn't displayable and pubsim's fixture is
  internal to `gawk-server` (`docs/41`).
- **Every dev lane's relay carries a dev proxy token and an explicit resume
  key**, because a compose profile can't change an existing service.
- **The OBS page has no Room row yet.** The relay side works: attach checks
  only ID + token, and the dev stack attaches a gateway broadcast to a room.
  But §5.7 assumed more than holds. OBS's Stop Streaming ends the broadcast
  with 4014, which removes the attachment, so every Stop and Start needs a
  re-attach driven by the status poll. A room the page creates ends when the
  page leaves it. The broadcaster's join/create sheet is inline in
  `BroadcasterScreen.tsx` and would have to be extracted first. It needs
  its own small design.
- **The gateway takes no relay CA or server name**, only `-relay-insecure`:
  `relay.url` must be a name on the relay's certificate, normally its
  public URL.
- **WG0 is not done**, so the reorder window's floor, cap, multiplier and
  NACK delay (3 ms), `maxSessions` (20), `maxSessionsHard` (25) and the
  pod's requests are the §4 estimates. The review before landing changed
  three behaviours WG0 should look at: a hole is NACKed only after it has
  been missing 3 ms, so a reordered packet never sets the RTT; a session
  the sender closed waits 2 s for OBS's DELETE before it counts as `lost`,
  because OBS closes its PeerConnection first; and offers are refused `406`
  unless their `profile-level-id` is Baseline, Main or High at level 5.2 or
  below.
