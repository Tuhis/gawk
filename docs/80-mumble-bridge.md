# R79 — Room voice through a Mumble bridge

**Status**: designed 2026-10-25; **MV0–MV7 implemented 2026-10-26** (§11 is
the implementation contract, §12 the results); **MV8's first pass on the
reference deployment 2026-10-26** (§12.5). Open: MV0's owner-device rows
(Safari on macOS and iOS, Chrome on Android, echo cancellation) and the
owner's sign-off on the latency target, then the rest of MV8 (phones, a
kick from each side, a bridge rollout mid-call, latency). Owner decisions taken in the design interview on 2026-10-25 (§2).

**Relationship to earlier work**: docs/44 §4.11 reserved the hooks for
this: a `voice` room capability, `spec.integrations.mumble` on the `Room`
CR, the `0x50–0x5F` sub-range of `RoomCommand`/`RoomEvent`, and the
`speaking` participant flag. R49 (docs/50) and R60 (docs/62) gave the
separate Mumble bot, which lives in its own repository, read access to
rooms and the right to create and delete static rooms. This milestone
builds the bridge itself. The bot keeps running beside it (OD3).

---

## 1. Why, and what "done" means

The common use of a room is a friend group in a voice channel while one or
more of them stream (docs/44 §1). Today the voice channel lives in Mumble
and the streams live in gawk, so everyone needs a desktop Mumble client
open next to the browser tab. A participant on a phone, or a friend who
was sent a link, has no way into the conversation.

With this milestone a gawk participant clicks **Join voice** in the room
and is talking to the people in the room's Mumble channel, the way a Meet
or Zoom call works, without a webcam and without knowing that Mumble is
involved. People on native Mumble clients see the web users as ordinary
Mumble users marked `[web]`, and the gawk room shows the people in Mumble.

### Milestone acceptance criteria

| Goal | Verified by |
|---|---|
| A gawk participant joins a room's voice from Chrome, Firefox and Safari on desktop, and from Chrome on Android and Safari on iOS, without any Mumble software | MV6 browser tests; MV8 owner pass on all five |
| Native Mumble users and gawk users hear each other both ways in the mapped channel | MV7 e2e (a scripted gumble client as the native user, against murmur in compose); MV8 with the desktop Mumble client |
| Each gawk user appears in Mumble as their own user, `Alice [web]`, and a name clash gets a numeric suffix | MV5 unit tests; MV7 e2e |
| The gawk room shows who is in the Mumble channel, also while nobody from gawk is in voice | MV3/MV5 tests; MV7 e2e with an empty gawk room |
| Mouth-to-ear latency from a gawk speaker to a native Mumble listener and back stays within the target MV0 sets (owner's target: ≤100 ms, 10 ms frames) | MV0 measurement; MV8 repeat on the reference deployment |
| A gawk ban or kill drops the person's voice; a Mumble kick or server mute is enforced on the gawk side | MV3/MV5 tests; MV7 e2e |
| Dynamic rooms get a temporary channel under the operator's parent channel, which disappears when the room ends | MV5 tests against murmur; MV7 e2e |
| Bridge pods scale out behind one UDP LoadBalancer; a pod drain moves users to another pod with a short gap | MV4 tests; MV7 two-pod e2e |
| Nothing changes when the bridge is not installed: no capability, no UI, no new relay behaviour | MV3 off-path test (the docs/44 RM2 shape); MV6 test |
| No voice audio is ever written to disk, logged or recorded by gawk | MV4 source-walk test; review |

---

## 2. Owner decisions (2026-10-25)

| # | Question | Decision |
|---|---|---|
| OD1 | Who is in the Mumble channel? | **Both**: native Mumble users and gawk users share the channel. The Mumble side's experience is a requirement, not an afterthought. |
| OD2 | How do rooms map to channels? | **A static room maps to an existing channel. A dynamic room gets a temporary channel** that the bridge creates and murmur removes when it empties. |
| OD3 | The existing Mumble bot (R49/R60)? | **Stays in its own repository and runs beside the bridge.** The bridge only carries voice and the voice roster. |
| OD4 | How general is the room's integrations block? | **Mumble only.** Keep the fixed `integrations.mumble` field; no list of integration kinds. |
| OD5 | Who turns voice on for a room? | **The operator** (CR, rooms file, `gawk-admin`), **the bot** (`rooms-manager`), and **the creator of a dynamic room** in the app. |
| OD6 | How do gawk users appear in Mumble? | **One Mumble user per gawk user**, marked as bridged (`Alice [web]`). |
| OD7 | Does the gawk roster show native Mumble users? | **Yes**, with mute state. Live speaking indicators only for people in voice (OD14). |
| OD8 | Voice audible in the stream? | **Accepted.** A broadcaster capturing system audio will also capture the voice channel; viewers hear it. |
| OD9 | Where does the Mumble server come from? | **The operator brings their own.** gawk ships no murmur. |
| OD10 | Controls in v1 | **Listen-only, voice activity, push-to-talk, mute, deafen, per-person volume.** |
| OD11 | Clients in v1 | **The web app, desktop and phone.** The native broadcasters and the iOS app come later. |
| OD12 | Moderation | **A gawk ban or kill drops voice. A Mumble kick, ban or server mute is reflected in gawk. Mumble ACLs apply** to bridged users as usual. |
| OD13 | Bridged names | **Marked as bridged; a clash gets a suffix** (`Alice [web] 2`). |
| OD14 | Who sees speaking indicators? | **Only people in voice**, worked out locally from the audio they receive. |
| OD15 | Scale | **Community scale**: dozens per channel, many voice rooms at once. The bridge scales horizontally. |
| OD16 | Transport | **Our own stack**: Opus through WebCodecs over WebTransport, as the rest of gawk. No WebRTC. |
| OD17 | Echo without a headset | **Accepted**, solved in-house later if it turns out to be bad. |
| OD18 | Murmur's limits (autoban, `maxusers`) | **A documented operator prerequisite.** The bridge detects and reports a violation. |
| OD19 | Abuse guard for dynamic rooms | **Caps** (temporary channels, voice users) **and the parent channel's ACL.** Installing the bridge is the opt-in; there is no separate switch for dynamic rooms. |
| OD20 | Audio path | **Voice goes from the browser straight to the bridge**; control stays on the room's existing session. |
| OD21 | Speaking Mumble | **A thin Tuhis fork of gumble** (D6). |
| OD22 | Placing browser sessions | **Any bridge pod behind one UDP LoadBalancer**; no room affinity for voice sessions. |
| OD23 | Authorising a voice session | **A short-lived ticket minted by the room's home pod**, HMAC-signed with a secret shared with the bridge, verified offline. |
| OD24 | Bridge ↔ room control | **A direct internal link**, not the event bus (D3 explains why the bus can't carry it). |
| OD25 | Who receives audio | **Each bridged connection receives its own audio**, so per-user ACLs and whispers work as for a native client. |
| OD26 | Mixing | **The browser mixes**; per-speaker Opus passes through the bridge untouched. |
| OD27 | Room discovery and ownership | **The relay dials the bridge**: holding the link is the ownership. Works with or without Kubernetes. |
| OD28 | Idle static rooms | **Always watched**: the voice roster is visible before anyone opens the room. |
| OD29 | Murmur authentication | **Server password plus an operator-chosen access token** for every bridged connection; bridged users stay unregistered. |
| OD30 | Pod drain | **A short gap is fine**: Mumble users see a leave and a rejoin. |
| OD31 | Latency and frame size | **≤100 ms mouth-to-ear, 10 ms Opus frames**, subject to MV0. |
| OD32 | Temporary channel names | **An operator template**; the default contains no room code. |
| OD33 | The observer in Mumble | **A visible user named `gawkBridgeBot`, with no comment.** A comment is visible to everyone on the server, not only the channel. *(Renamed from `gawk` 2026-10-27: the name says what the user is to someone reading the Mumble channel list.)* |

---

## 3. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | **A new top-level Go module and deployable, `gawk-mumble-bridge`, default off everywhere.** Image `ghcr.io/tuhis/gawk-mumble-bridge`, chart `oci://ghcr.io/tuhis/charts/gawk-mumble-bridge`, its own release-please component. It imports `gawk-server/wire` (public, never mirrored). It has two listeners: a **public** QUIC listener for browsers (UDP LoadBalancer), and an **internal** one for relay links, metrics and `/statusz`, never routed publicly. | OD20, OD22. The two-listener split is the gawk-telemetry posture: the public side carries only ticket-gated media. |
| D2 | **The relay is the authority; bridge pods are spokes.** Every fact about who may be in voice lives on the room's home pod. Two kinds of links connect them, both PSK-gated (`-voice-bridge-psk`): the **observer link**, which the home pod dials to the bridge's internal Service when a voice room becomes live (OD27), and a **session link**, which a bridge pod dials to the relay's `/internal/voice/{code}` when it accepts a browser's voice session. A non-home relay pod proxies a session link to the home, like `/internal/room` (docs/44 §4.5). | One authority, as in the rest of the room model. Kicks reach exactly the pod hosting the user, because that pod holds a link to the home. No bridge-to-bridge coordination and no Kubernetes lease. |
| D3 | **Not the event bus.** The bus drops on overflow (docs/51 D1), the relay only publishes and never consumes, NATS is off by default (docs/51 D6), and nothing orders events across pods (docs/51 D9). | A dropped `participant_left` would leave a kicked user talking in Mumble. OD24. |
| D4 | **The observer.** The bridge pod that accepts a room's observer link opens one Mumble connection for the room, named `gawkBridgeBot` (a suffix on a clash, D9), with no comment (OD33). It joins the mapped channel, or creates the temporary channel (D11), and reports the channel's user list, with mute and deafen state, to the home pod as Mumble changes it. If the link drops, the home pod redials after a backoff; whichever pod accepts takes over. The observer link exists for as long as the room is voice-live (D12). | OD27, OD28. The observer is what lets the room show who is in Mumble before any gawk user joins voice. |
| D5 | **A voice session.** The browser asks for voice over the room control session (`RoomCommand` `VoiceJoin`). The home pod answers with a `VoiceTicket` event: the bridge's public URL and a ticket (D7). The browser opens a WebTransport session to that URL and sends the ticket first. The bridge pod that accepts it verifies the ticket, dials its session link to the home, and registers the participant with the ticket's nonce. **The home checks the nonce first**: it keeps every nonce it has accepted until that ticket expires and refuses a repeat. **Then the newest session wins**: a fresh registration for a participant who already has a voice session closes the older session (with 4015), the way a reload replaces the old tab. The pod then opens this user's own Mumble connection (OD6) and joins the room's channel. | OD20, OD22, OD23. Newest-wins lets a user rejoin from a reloaded tab or a drained pod without waiting for the old session to time out. The nonce check is what makes a ticket single-use: without it, a copied ticket presented inside its 30 s life would pass the offline HMAC and expiry checks and then, by newest-wins, close the legitimate session. One voice session per participant, and the relay knows where it is. |
| D6 | **A thin fork of gumble, `github.com/tuhis/gumble`.** It adds (a) a raw audio listener that hands over each Opus packet with the sender session, sequence number, target and end-of-transmission bit, without decoding; (b) a delivery path that never blocks the connection's read loop (a bounded queue per listener, dropping the oldest packet on overflow); (c) a configurable reported client version. Sending uses the exported `Conn.WriteAudio` (`gumble/conn.go:65`) with the browser's Opus bytes. The bridge links no libopus and builds with `CGO_ENABLED=0`. | Stock gumble decodes every packet (`handlers.go:134`), throws away the sequence number and the end-of-transmission bit, and sends from the read loop into an unbuffered channel, so a slow consumer stalls the connection. A pass-through codec shim could work around the decode, but it would smuggle bytes through `[]int16` and still lose the end-of-speech bit. The upkeep of a small fork is cheaper than that. |
| D7 | **The voice ticket.** Fields: room code, participant ID, nickname, a nonce, the minting home's **epoch** (the room lease generation in cluster mode, a random per-process value in single-pod mode) and an expiry 30 s after minting. HMAC-SHA256 under `-voice-ticket-secret`, which only the relay and the bridge hold. The bridge verifies it without a network call. The ticket only opens the session; the session link (D2) is what keeps it authorised. The home refuses a ticket from another epoch at registration, so a home move can't be used to replay a ticket whose nonce only the old home remembered. | OD23. A ticket does not have to outlive a session: a kick works through the link, not through expiry. |
| D8 | **Each bridged connection receives its own audio** (OD25). The bridge pod forwards every Opus packet it receives on user A's connection to A's browser as a `VoiceFrame` datagram tagged with the speaker's Mumble session ID. Murmur already leaves out A's own voice, applies ACLs and whispers, and stops sending to a deafened user. | Mumble semantics stay exact without re-implementing them in the bridge. The cost is murmur sending N copies inside the cluster, which a homelab LAN carries. |
| D9 | **Names.** A bridged user's Mumble name comes from `-mumble-name-template`, default `{nick} [web]`. If murmur refuses the name (in use or registered), the bridge retries with ` 2`, ` 3` and so on, up to 10 attempts. The gawk roster always shows the gawk nickname. The observer uses the same retry on `gawkBridgeBot`. | OD13, OD33. |
| D10 | **Murmur authentication**: every bridged connection, observer included, sends the server password (if configured) and the access token `-mumble-access-token`. Bridged users are unregistered. Channel ACLs can grant or deny bridged users as a group through `#<token>`. The temporary-channel parent must grant that group `MakeTempChannel`. | OD29. A token-based group needs no per-user certificate, and per-user certificates would mean nothing while nicknames are unauthenticated. |
| D11 | **Channels.** A static room's CR names an existing channel by path (`integrations.mumble.channel`, `/`-separated). A dynamic room with voice gets a temporary channel under `-mumble-temp-parent`, named from `-mumble-temp-channel-template`. The default is `gawk {key}`, where `{key}` is the first six base32 characters of HMAC-SHA256(`-voice-ticket-secret`, `"mumble-channel:" + code`). The bridge computes it. Every relay pod and every bridge pod holds that secret, so the name is the same whichever bridge pod runs the observer and whichever relay pod is home, and it contains no code. (The `/statusz` key was rejected: it falls back to a per-process key when `statsKey` is unset, the chart default, so a home move would rename the channel.) The template may use `{code}` if the operator chooses. The observer creates the channel and stays in it, so murmur removes it once the room ends and everyone has left. A redialled observer finds the existing channel by name, so a home move or an observer move mid-call keeps everyone, native users included, in the same channel. | OD2, OD32. A temporary channel lives only while someone is in it, which matches the room's life without a cleanup job. Room codes are joinable secrets (docs/44), so they stay out of names by default. |
| D12 | **Voice-live rooms.** A static room with `integrations.mumble` is **voice-live** whenever the bridge is configured. The relay homes it at startup and when its CR appears, as if the first join had happened (in cluster mode by the same lease claim). A dynamic room is voice-live from creation if its creator asked for voice. **Voice-live is not live**: it emits no `room.opened`, does not set `live` in the R49 API and does not count toward the room caps. | OD28 without changing what "live" means to every existing consumer. |
| D13 | **The `Room` CR's reserved field is reshaped while nothing reads it**: `integrations.mumble` becomes `{channel: string, temporary: bool}`. `server` and `channelPrefix` are removed. `-rooms-file` entries gain the same `mumble` object. A dynamic room created with voice gets `{temporary: true}`. One Mumble server per deployment, configured on the bridge (OD9). | OD4. docs/44 §4.11 reserved the field to make this a field change, not a version change. |
| D14 | **Who can enable voice.** Operator: CR, rooms file, `gawk-admin` room form. The bot: the `rooms-manager` create body gains the `mumble` object. A dynamic room's creator: a **Voice** toggle on create, sent in `RoomHello`. The relay ignores the request (and advertises no `voice` capability) when no bridge is configured. | OD5, OD19. |
| D15 | **The voice roster is separate from the participant roster.** `RoomState` gains a voice-roster section and a `RoomEvent` `VoiceRosterUpdated` (voice range). An entry is a Mumble session ID, a name, a kind (`gawk` with the participant ID, or `mumble`), and self/server mute and deafen flags. The home pod merges what the observer reports with what the session links register. The UI shows one list. | OD7. Native Mumble users never join the room, so they are not participants: no kick from gawk, no streaming, no counting against `maxParticipants`. |
| D16 | **Speaking indicators are local** (OD14). A browser in voice marks a speaker as speaking from the `VoiceFrame`s it receives, holding the indicator 250 ms after the last frame and clearing it at once on the end-of-transmission flag. Its own indicator comes from its own voice activity detection. The `speaking` participant flag stays false on the wire. | No high-rate event traffic on the control stream. |
| D17 | **Moderation.** A gawk kill, ban or room removal: the home pod tells the hosting pod over the session link, which closes the browser's voice session with **4015 `CloseCodeVoiceRemoved`** and disconnects the Mumble user. A Mumble kick or ban of a bridged user: the hosting pod closes the browser's voice session with 4015 and reports it to the home. A Mumble server mute or deafen: sent to the browser as `VoiceState`, which disables the mic or the playback control and says why. 4015 is terminal for voice (the browser does not rejoin voice by itself); it does not affect the room session. | OD12. |
| D18 | **Mumble unreachable.** If the bridge can't connect a user (murmur down, autobanned, `maxusers` reached, a channel it may not enter), it closes the voice session with **4016 `CloseCodeVoiceUnavailable`**, non-terminal, with the reason in the in-band notice (docs/59). It counts the reason in a metric and in `/statusz`, so an operator sees an autoban or a full server at once. | OD18. |
| D19 | **Drain.** A draining bridge pod sends `SessionClosing` with 4002 to its browsers while it is still Ready (the docs/22 lesson), closes its Mumble connections, and stops accepting. The browser rejoins voice through the LoadBalancer with a fresh ticket from the room session. An observer link closes with the drain, and the home pod redials, landing on another pod. | OD30. |
| D20 | **Data-plane wire.** New top-level types after `0x19`: `VoiceHello` (browser → bridge, the ticket), `VoiceFrame` (both directions: speaker session ID, sequence number, flags with end-of-transmission, Opus bytes; a datagram), `VoiceState` (bridge → browser: server mute/deafen, the browser's own Mumble session ID) and `VoiceControl` (browser → bridge: self-mute and self-deafen, passed on to Mumble). Mirrored in `wire.ts` and `crates/wire` with golden vectors, as every wire type is. Exact layouts and numbers are fixed in MV2. | CLAUDE.md conventions. A 10 ms Opus frame at 40 kbit/s is ~50 bytes, far under the 1200-byte datagram limit. |
| D21 | **Browser capture and encode.** `getUserMedia` with echo cancellation, noise suppression and auto gain on, then into `AudioEncoder`. Chromium and Safari reuse the broadcaster audio lane's MSTP pump (`audio-lane.ts`; the worker MSTP on Safari). Firefox has no capture path today: `audioLaneSupported()` needs MSTP, which Firefox lacks, so the broadcaster runs video-only there. Voice adds a new AudioWorklet capture node that builds `AudioData` from the worklet's PCM; MV6 builds it and MV0 checks that Firefox's `AudioEncoder` accepts that input (Opus, 48 kHz mono, `frameDuration` 10 ms, 40 kbit/s default). Voice activity detection runs on the captured PCM with a hangover; push-to-talk is a key while the tab has focus, and a hold button on phones. Nothing is sent while not talking; the last frame carries end-of-transmission. Listen-only never calls `getUserMedia`. | OD10, OD16, OD31. Mumble clients send nothing between talk spurts, so silence costs nothing. The §1 goal includes Firefox, so its capture is budgeted work, not reuse. |
| D22 | **Browser playback.** One `AudioDecoder` and jitter buffer (`transport/audio-buffer.ts`) per speaker, created on the first frame and released after 10 s of silence, into the room's shared `AudioContext` (docs/44 §4.7) through a gain node per speaker. Per-person volume persists per browser by name. Deafen mutes the voice bus and tells Mumble. | OD10, OD26. Usually two or three people talk at once, so decoder count stays small even in a big channel. |
| D23 | **Caps.** On the relay: `-voice-max-rooms` (voice-live dynamic rooms per fleet) and `-voice-max-users-per-room`. On the bridge: `-max-voice-sessions` per pod and `-max-temp-channels`. All plumbed through `roomOptions` and the bridge's options struct with the R2 carry-all test, plus `GAWK_*` envs and Helm values. | OD19, the CLAUDE.md invariant. |
| D24 | **Prohibitions that travel with the bridge.** No voice audio is ever written to disk, logged, recorded or exposed by gawk. The bridge never sees publisher IPs, and its `/statusz` keys rooms by the keyed hash, never the code. Its internal listener is never routed publicly. | The D8 rule of `gawk-sampler`, applied to voice; the CLAUDE.md broadcast-ID rule. |

---

## 4. Architecture

```
            room control session (exists)                      ┌──────────────┐
 Browser ─────────────────────────────────────▶ relay home pod │ Room CR /    │
   │   VoiceJoin ▶  ◀ VoiceTicket, voice roster      │  ▲       │ rooms file   │
   │                                                 │  │       └──────────────┘
   │ WebTransport (public UDP LB)        observer link│  │session link (PSK)
   │ VoiceHello(ticket), VoiceFrame ⇄                 ▼  │
   └──────────────────────────────────▶  gawk-mumble-bridge pods
                                          │ per-user conn │ observer conn
                                          ▼ (TLS, gumble fork)
                                       operator's murmur ◀── native Mumble clients
```

A voice frame from a gawk speaker: mic → `AudioEncoder` (10 ms) → `VoiceFrame`
datagram → bridge pod → `Conn.WriteAudio` on the speaker's Mumble
connection → murmur → every listener. In the other direction: murmur → each
gawk listener's own Mumble connection → raw listener (D6) → `VoiceFrame`
datagram → that browser's per-speaker decoder and jitter buffer → mix.

---

## 5. Operator prerequisites (murmur)

Documented in `docs/self-hosting.md` with MV7, and checked by the bridge at
runtime (D18):

- **`autobanAttempts=0`** (or a value above the bridge's connection rate).
  Murmur bans an IP after 10 connections in 2 minutes by default, and every
  bridged user is a connection from the bridge's few IPs.
- **`users`** (max users) sized for native plus bridged users.
- **`messagelimit` / `messageburst`**: the defaults (1/s, burst 5) are per
  connection, so they bind per bridged user, not the bridge as a whole.
- **An ACL** on the temporary-channel parent granting `#<access token>`
  `MakeTempChannel` and `Enter`, and on mapped static channels whatever
  the operator wants bridged users to have.

---

## 6. Security and privacy

- **The public listener accepts only a valid ticket**, from an allowed
  Origin (the relay's `allowedOrigins` list), and closes anything else
  after the first message. Tickets expire in 30 s and are useful once (D5).
- **Who can listen**: anyone who can join the room can join voice, as
  anyone in the room can watch its streams. A room code is the key, as
  before. Native Mumble users' names and mute state become visible to
  everyone in the room (D15). Operators who map a channel should know that.
- **Who can speak in Mumble as a bridged user** is limited by the access
  token's ACL group. The `[web]` suffix keeps a bridged user from passing
  as a registered one.
- **Mumble may record.** Murmur or a native client can record a channel;
  gawk can't prevent it. The voice panel says the channel is a Mumble
  channel shared with others.

---

## 7. Risks

- **The latency target is unmeasured.** Mic capture, `AudioEncoder` delay,
  whether Firefox and Safari accept a 10 ms `frameDuration`, the jitter
  buffer and the `AudioContext` output latency all add up in the browser.
  MV0 measures them before anything else is built; the target in §1 is
  updated from its numbers.
- **Firefox capture is new code.** Nothing in `gawk-app` captures audio on
  Firefox today. MV0 proves the AudioWorklet → `AudioEncoder` path before
  MV6 builds it; if it fails, Firefox is listen-only by owner decision.
- **Echo cancellation.** Browsers' echo cancellation has historically
  covered only audio played through WebRTC. Users without a headset may be
  heard twice. Accepted (OD17); MV0 records what each browser actually
  does.
- **Murmur's per-connection cost at community scale.** Hundreds of TLS
  connections and N copies of each voice frame inside the cluster. MV7
  measures one pod's ceiling.
- **The gumble fork** is a dependency we own. Upstream has been quiet
  since 2022, so there is little to merge.

---

## 8. Out of scope

Text chat (docs/44 §4.11 keeps its range); voice on the native broadcasters
and in the iOS app (OD11); more than one Mumble server per deployment;
mixing on the bridge; recording; a voice path through PoP relays (a PoP
guest reaches the bridge over the internet like anyone else; the ticket
arrives over the room session as usual); stronger identity (the
participant `identity` field stays empty).

---

## 9. Chunks

| Chunk | What | Acceptance |
|---|---|---|
| **MV0** | **Measurement spike, before any design is locked.** (a) Browser voice pipeline on Chrome, Firefox and Safari (desktop) and on Chrome Android and Safari iOS: `getUserMedia` → capture (MSTP, or on Firefox a prototype AudioWorklet node building `AudioData`) → `AudioEncoder` with 10 ms and 20 ms `frameDuration` → loopback datagrams → `AudioDecoder` → jitter buffer → AudioWorklet output; record each stage's latency and whether 10 ms is accepted. (b) Echo cancellation with WebAudio playback, with and without a headset. (c) A gumble-fork prototype passing raw Opus both ways through murmur 1.5/1.6 in compose, with a native client. | A results table in this doc (§1 target confirmed or changed, frame size chosen per browser), raw data committed under `docs/assets/80/`. If Firefox's `AudioEncoder` can't take worklet `AudioData`, the owner decides at sign-off whether Firefox ships listen-only. Owner sign-off on the target before MV6 starts. |
| **MV1** | The gumble fork (D6) in `tuhis/gumble`. | Fork tests: raw listener delivers bytes, sequence and end-of-transmission unchanged; a stalled consumer does not stall the read loop; reported version configurable. An integration test against murmur in a container. |
| **MV2** | Wire: D20's data-plane types, close codes 4015 and 4016 (with notice-set membership), the voice `RoomCommand`/`RoomEvent` kinds (`VoiceJoin`, `VoiceTicket`, `VoiceRosterUpdated`), the `voice` capability and the `RoomState` voice roster. All three mirrors. | Round-trip and golden-vector tests byte-identical in Go, TS and Rust; existing room vectors unchanged. |
| **MV3** | Relay: D13's CR and rooms-file shape, D12 voice-live homing, D14 create flags, the observer link (dial, redial with backoff), `/internal/voice/{code}` session links with proxying, ticket minting, the merged voice roster, kicks over the link, D23 caps, all knobs through `roomOptions`/`registryOptions`, chart values. | `TestVoiceLiveStaticRoomIsHomedButNotLive`, `TestVoiceTicketVerifiesWithSharedSecret`, `TestSecondVoiceRegistrationClosesTheFirst` (newest wins), `TestReplayedVoiceTicketNonceIsRefused`, `TestVoiceTicketFromAnotherEpochIsRefused`, `TestKillClosesVoiceOverTheSessionLink`, `TestObserverLinkRedialsAfterDrop`, carry-all knob test, and the off-path test: with no bridge configured, the full existing suite is green and no voice capability is advertised. |
| **MV4** | Bridge service: the module, two listeners, ticket verification, session links, per-pod caps, drain (D19), `/healthz`, `/metrics`, `/statusz` (keyed), Dockerfile, Helm chart (UDP LoadBalancer, internal Service), CI workflow and release-please component. | Unit tests for every flag/env/value; a refused bad ticket; a drain sends 4002 while Ready; a source-walk test that no package writes audio to disk or logs; chart lint and template tests. |
| **MV5** | Bridge Mumble side: per-user connections, D9 naming, D10 auth, the observer and D11 temporary channels, roster reporting, D17/D18 moderation and failures. | Tests against murmur in a container: name clash suffix; temporary channel created under the parent and gone after the room ends; the channel name is the same from two bridge pods for one room code, and a home move mid-call (observer redialled to another pod) keeps everyone in the same channel; a Mumble kick closes the browser session with 4015; server mute reaches the browser as `VoiceState`; autoban is reported as 4016 with its reason in `/statusz`. |
| **MV6** | Web app: the voice panel (join, listen-only, voice activity, push-to-talk, mute, deafen, per-person volume, the merged voice roster, speaking indicators), D21 capture (the MSTP pump reused on Chromium and Safari, the new AudioWorklet capture node on Firefox) and D22 playback, the dynamic room's Voice toggle, phone layout. | Vitest for the voice session (ticket → connect → frames), the worklet capture node's PCM → `AudioData` framing, VAD and PTT state, per-speaker decoder lifecycle, roster merge; Playwright on Chromium with a fake bridge; screens at 360/390/430 px and desktop. No voice UI without the `voice` capability. |
| **MV7** | e2e and docs: the compose dev stack gains murmur and the bridge; an e2e pass with two browsers and a scripted native gumble client, including a two-pod drain; a one-pod load measurement; `docs/self-hosting.md` voice section with the §5 prerequisites; `docs/gotchas.md` entries. | e2e green in CI; load numbers recorded here; self-hosting recipe followed once from scratch. |
| **MV8** | **Owner pass on the reference deployment.** | A static room mapped to the friends' channel and a dynamic room with a temporary channel; desktop Mumble users and gawk users on desktop and phone talking both ways; a kick from each side; a bridge rollout mid-call; latency measured against the MV0 target. |

---

## 10. Open questions

- **Bridge URL discovery.** The relay hands out the bridge's public URL in
  `VoiceTicket` from its own config (`-voice-bridge-public-url`). A relay
  using a bridge run by someone else is not considered.
- **The observer's name across rooms.** Every voice room has its own
  observer, so a server with several voice rooms shows `gawkBridgeBot`,
  `gawkBridgeBot 2`, and so on, in different channels. If that reads badly
  in Mumble, the
  alternative is one observer per bridge pod watching every channel. That
  is cheaper, but temporary channels would then need a different keeper.

---

## 11. Implementation contract (2026-10-26)

MV2–MV7 were built against this section. It fixes what §3 left to the
chunks (layouts, routes, knobs) and records where the build departs from a
§3 decision and why. Where the two disagree, this section is what the code
does.

### 11.1 Departures from §3

| # | §3 said | Built | Why |
|---|---|---|---|
| C1 | D6, OD21: a thin fork of gumble in `tuhis/gumble` | **A first-party Mumble client** in `gawk-mumble-bridge/internal/mumble` (TLS control channel, the handful of protobuf messages the bridge needs encoded with `protowire`, the legacy UDPTunnel audio format). It keeps D6's three properties: raw Opus with sender session, sequence, target and end-of-transmission; a bounded, drop-oldest delivery queue that never blocks the read loop; a configurable reported client version. | gumble is MPL-2.0, which is not on the Go license allowlist (`tools/licenses/gen-notices.py`; adding it is a licensing decision, CONTRIBUTING.md), and the `tuhis/gumble` repository could not be created from this change. The client the bridge needs is small; a fork would have carried a decoder, a UDP stack and a plugin model the bridge does not use. |
| C2 | D15: `RoomState` gains a voice-roster section | `RoomState` is **unchanged**. The roster is a `VoiceRosterUpdated` event (0x51) the home sends right after every `RoomState` and on every change, only to participants whose `RoomHello` asked for `RoomCapVoice`. It is out of sequence: it carries the room's current `seq` without advancing it, like `CommandRejected`. | `RoomState`'s parser is strict (exact length), and the released desktop and iOS clients join rooms: a trailing section would make every voice room unreadable to them. Sending the roster only to clients that asked keeps every existing vector and every existing client byte-identical. |
| C3 | D14: the dynamic room's Voice toggle is sent in `RoomHello` | `CONNECT /room/new?voice=1`. | The room is minted before the upgrade, from the query, and `wantCaps` already means "send me voice events" for every voice-capable client; one bit cannot mean both. |
| C4 | D17 named kills and bans only | A participant's voice session also ends when its **room control session** ends, with **4002** (non-terminal): the browser rejoins voice with a fresh ticket once its room session is back. | A voice session is bound to a participant ID, and a reconnected room session is a new participant. |

### 11.2 Wire (MV2)

Layouts and golden vectors: `gawk-server/wire/voice.go` (VoiceHello 0x1A,
VoiceFrame 0x1B, VoiceState 0x1C, VoiceControl 0x1D, close codes 4015 and
4016) and `gawk-server/wire/room.go` (`RoomCommandVoiceJoin` 0x50,
`RoomEventVoiceTicket` 0x50, `RoomEventVoiceRosterUpdated` 0x51, the voice
roster entry). Mirrored in `wire.ts` and `crates/wire` with the same vectors.

The ticket, the temporary channel key and the two internal links are the
public package `gawk-server/voice` (the `screening` precedent: the bridge
compiles it in, nobody restates it). Link messages are JSON, framed as
`uint32 length ‖ JSON` on one bidirectional stream.

### 11.3 Topology and routes

| Where | Listener | Route | Who dials |
|---|---|---|---|
| bridge, public (UDP LoadBalancer) | QUIC `:4443` | `CONNECT /voice` | the browser, with the ticket in its first record |
| bridge, internal (ClusterIP, never public) | QUIC `:4444` | `CONNECT /internal/observe?psk=` | the room's home relay pod |
| bridge, internal | TCP `:2113` | `GET /healthz`, `/readyz`, `/metrics`, `/statusz` | probes, Prometheus, operators |
| relay | its QUIC listener | `CONNECT /internal/voice/{code}?psk=` | a bridge pod (via the relay's Service); in cluster mode a non-home pod proxies it to the home with the cluster PSK and the lease generation, as `/internal/room` |

`/internal/voice/{code}` is registered only when rooms are on and voice is
configured. Both links send Origin `gawk-server://native-internal-edge`.

### 11.4 Knobs

Relay (all through `roomOptions`, flags, `GAWK_*` envs and the chart; voice
is configured only when the first four are all set, a partial set refuses
to start, and voice needs `-rooms`):

| Flag | Default | Meaning |
|---|---|---|
| `-voice-bridge-addr` | — | the bridge's internal QUIC address the home dials for observer links (`host:port`) |
| `-voice-bridge-public-url` | — | the https URL a browser opens a voice session on, sent in `VoiceTicket` |
| `-voice-bridge-psk` | — | gates both links |
| `-voice-ticket-secret` | — | HMAC key for tickets and the temporary channel key; ≥16 bytes; the bridge holds the same |
| `-voice-bridge-server-name` | host of `-voice-bridge-addr` | TLS name verified on the observer dial |
| `-voice-bridge-ca-file` | system roots | extra roots for the observer dial (the dev stack's self-signed certificate) |
| `-voice-max-rooms` | 10 | voice-live dynamic rooms (per pod; fleet-wide in cluster mode, counted from CRs) |
| `-voice-max-users-per-room` | 25 | voice sessions per room |

Bridge (`gawk-mumble-bridge`, flags and `GAWK_*` envs and the chart):
`-listen`, `-internal-listen`, `-ops-listen`, `-cert-file`, `-key-file`,
`-dev-cert`, `-allowed-origins`, `-psk`, `-ticket-secret`, `-relay-url`,
`-relay-server-name`, `-relay-ca-file`, `-mumble-addr`, `-mumble-password`,
`-mumble-access-token`, `-mumble-ca-file`, `-mumble-cert-sha256`,
`-mumble-insecure-skip-verify`, `-mumble-name-template` (`{nick} [web]`),
`-mumble-observer-name` (`gawkBridgeBot`), `-mumble-temp-parent`,
`-mumble-temp-channel-template` (`gawk {key}`), `-mumble-client-version`
(`1.4.0`, so murmur speaks the legacy audio format on the TCP tunnel),
`-max-voice-sessions` (200), `-max-temp-channels` (50), `-drain-timeout`
(10s), `-stats-key`. The bridge's own README is the reference.

### 11.5 Behaviour the chunks share

- **Epoch** (D7): the room lease generation in cluster mode; a random
  per-process value on a single pod. The home keeps every nonce it accepted
  until its ticket expires and refuses a repeat.
- **Register at the home**: ticket MAC and expiry, epoch, nonce, the
  participant still in the room under that ID, the room still voice-live,
  `-voice-max-users-per-room`, and the voice client's address against the
  ban set. Then newest wins (4015 to the older session), then `Accepted`,
  which carries the channel (path or `temporary`) so the bridge pod hosting
  the user needs nothing from the observer's pod.
- **Ends of a voice session**, as the browser sees them: 4015 for a room
  ending, a gawk IP ban covering the voice client, a kill of a broadcast the
  participant attached, voice turned off for the room, a newer session of
  the same participant, or a Mumble kick or ban; 4016 for a refused or
  expired ticket's retryable cases and every Mumble-side failure (murmur
  unreachable, autoban, full, a channel it may not enter, the bridge cap);
  4002 for a bridge drain and for the participant's room session ending
  (C4). Every one is sent in-band first.
- **Names** (D9): `{nick} [web]`, then ` 2`… up to 10 on `UsernameInUse`.
  Measured against murmur 1.5.939 and 1.6.870: with no `username` setting
  both accept spaces and brackets, so the pretty form works out of the box;
  the rule murmur's sample ini shows does not. On `InvalidUsername` (that
  rule, or 1.6 refusing a non-ASCII nickname) the bridge retries once with
  every character outside the sample rule replaced by `_`, then suffixes
  `_2`…. Murmur does not refuse a second login of the same name from the
  same address: it disconnects the first as a ghost. Every bridged user of a
  pod shares its address, so the bridge keeps a per-pod name registry and
  checks the server's user list before it dials; a ghosted connection is
  closed with 4016.
  The **observers** share one name family (`gawkBridgeBot`, `gawkBridgeBot 2`, …) across every
  voice room on the server, so their ladder is not D9's ten but 1000; a
  ten-name ladder capped a server at ten voice rooms (found in review).
- **Certificates**: murmur refuses to create a temporary channel for a
  client without a certificate, so every bridged connection presents a
  fresh self-signed one that is never stored. A Mumble ban of a bridged
  user bans the bridge pod's address; `docs/self-hosting.md` says so.
- **Audio format**: murmur 1.5 and 1.6 still send the legacy audio format
  over the TCP tunnel to a client that reports 1.4.0 (measured, byte-
  identical), and the protobuf format to one that reports ≥ 1.5.0. The
  bridge refuses a configured version outside 1.2.4–1.4.x.
- **Speaking indicators** (D16) never touch the relay.
- **Privacy** (D24): no package of the bridge writes, logs or exposes voice
  audio; a source-walk test enforces it, as `gawk-sampler`'s D8 test does.

---

## 12. Results (2026-10-26)

### 12.1 MV0 — the browser pipeline, measured

Raw data, the harness and the full write-up: [`assets/80/RESULTS.md`](assets/80/RESULTS.md).
Headless Chromium 151, Firefox 153 and Playwright's WebKit 26.5 (not
Safari), on a PulseAudio null sink.

| | Chromium | Firefox | WebKit (not Safari) |
|---|---|---|---|
| 10 ms `frameDuration` accepted, real 10 ms packets | yes | yes | yes |
| Encode / decode p50, 10 ms | 0.5 / 0.3 ms | 0.4 / 0.9 ms | 1 / 1 ms (1 ms timer) |
| Capture node → playout-ready, 10 ms, p50 / p95 | 22.5 / 37.2 ms | 25.2 / 33.8 ms | 25.8 / 44.5 ms |
| Same at 20 ms, p50 | 31.8 ms | 35.8 ms | 43.8 ms |
| AudioWorklet capture → `AudioData` → `AudioEncoder` | works | works | works |

- **Firefox ships speaking, not listen-only**: its `AudioEncoder` takes the
  worklet's `AudioData` (§7's risk, retired).
- **Frame size: 10 ms on every browser** (provisional for Safari until
  MV8), ~9–11 ms less transit than 20 ms for under 1 ms of encode.
- **The ≤100 ms target is neither confirmed nor ruled out.** The measured
  browser part is 22–26 ms; with estimates for the microphone and echo
  processing, the network, the jitter buffer and the output device the
  total is ~70–155 ms. Plausible on a wired desktop on a LAN, unlikely on
  phones or Bluetooth. The owner decides at sign-off.
- Findings the implementation follows: `AudioEncoder.flush()` on Chromium
  emits an extra padded packet per call, so the web client flushes only at
  the end of a spurt; Firefox ignores the 40 kbit/s bitrate (~57 kbit/s);
  `AudioData` does not exist inside an AudioWorklet anywhere, so the
  capture node posts PCM to the main thread.
- **Not measured**: Safari (macOS, iOS), Chrome on Android, real microphone
  and output latency, echo cancellation with WebAudio playback. RESULTS.md
  has the owner's manual procedure for the last.

### 12.2 MV5 — what real murmur taught

Recorded in §11.5 and `docs/gotchas.md`: murmur 1.5 and 1.6 still send the
legacy audio format to a 1.4.0 client; a duplicate name from one address is
a ghost, not a reject; temporary channels need a client certificate; the
compiled-in username rule accepts `Alice [web]`.

### 12.3 MV7 — end to end

`e2e/voice.mjs` (CI job `e2e-voice`): the relay, two bridge pods behind a
UDP LoadBalancer stand-in, murmur 1.6, three Chromium participants on the
fake microphone and a scripted native Mumble user. All six passes green,
three runs in a row:

| Pass | Shows |
|---|---|
| Roster before voice | a room viewer who never joined voice sees the native Mumble user (OD28, D15) |
| Both directions | the native user receives the gawk speaker's frames; both gawk users see the native user speaking from frames they received |
| Mumble moderation | a server mute reaches the panel; a kick ends voice with 4015 |
| Pod drain | 4002 while the pod still serves, rejoin onto the other pod through the LoadBalancer, the observer follows, audio resumes |
| Temporary channel | a `?voice=1` room's channel exists under the parent, found by the fleet-stable name, and its native user is on the room's roster |
| gawk ban | an IP ban covering the browsers ends voice with 4015 |

Its first run found a bug no unit test had the topology for: the draining
pod takes the room's observer with it, so the immediate rejoin was told
`Unavailable` and the browser gave up. A rejoin now treats that as another
rejoin (`001972ba`, test-first).

### 12.4 MV7 — one pod under load

`e2e/voice-load.sh`, raw results in
[`assets/80/load/results.jsonl`](assets/80/load/results.jsonl): one relay,
**one** bridge pod and murmur 1.6 on one 8-core Ivy Bridge Xeon, N real voice
participants in one channel (room session, ticket, voice session and bridged
Mumble user each), three talking at once for 30 s.

| Users | Joined | Delivered / expected | Bridge CPU | Bridge RSS | murmur CPU |
|---|---|---|---|---|---|
| 10 | 10 | 100 % | 0.43 cores | 26 MB | 0.22 cores |
| 25 | 25 | 100 % | 0.92 cores | 36 MB | 0.45 cores |
| 50 | 50 | 80 % | 1.40 cores | 54 MB | 0.70 cores |
| 100 | 99 | 30 % | 1.06 cores | 89 MB | 0.6 cores |

- **Up to 25 people in one channel, nothing is lost** on this machine.
- At 50 the shortfall is **between murmur and the bridge**: the bridge
  forwarded to the participants what it received (351,658 vs 351,537 frames,
  2 dropped by its queues). Murmur sends each frame to each of N bridged
  TCP connections from one host (D8's "N copies"); the load generator,
  the bridge, the relay and murmur shared the 8 cores, so the run does not
  say how much of that is murmur's own ceiling. A real pod on its own node
  should do better; MV8 measures the deployment.
- At 100 murmur's default `users` cap (100, the observer is one) refused the
  last participant: the bridge closed it with **4016 "the Mumble server is
  full"**, the D18 path. Size `users` for native plus bridged users (§5).
- Community scale (OD15, dozens per channel) fits at the low end. More
  bridge pods spread the bridge's own CPU across nodes; they do not reduce
  murmur's fan-out, which is one copy per bridged user whichever pod holds
  it. A channel much bigger than this is murmur's limit before it is the
  bridge's, and §7's "per-connection cost" risk stands until MV8 measures
  murmur on its own host.

### 12.5 MV8 — the reference deployment, first pass (2026-10-26)

`gawk-mumble-bridge` 1.0.0 (two pods) and `gawk-server` 0.38.0 (three pods,
cluster mode) against the deployment's existing murmur 1.6.870, which also
carries its regular users and bots. The bridge reaches the relay through
its in-cluster Service and murmur through murmur's ClusterIP, with murmur's
self-signed certificate pinned (`certSha256`).

| MV8 row | Result |
|---|---|
| A static room mapped to an existing channel | ✅ The observer holds the channel from relay start (D12), before anyone opens the room. |
| Desktop browser ↔ desktop Mumble, both ways | ✅ Owner, desktop browser and desktop Mumble client in the same channel, heard each other. A tab reload rejoined as the same `Name [web]`. |
| A dynamic room with a temporary channel | ✅ A `?voice=1` room got `gawk <key>` under the operator's parent channel; the browser and the desktop client talked in it; when the room ended the observer left and murmur removed the channel. |
| A bridge rollout | ✅ Not mid-call: a rollout to pick up a rotated access token; the new pods' observer was back in its channel within seconds. |
| Phones, a kick from each side, a rollout mid-call, latency against §12.1 | Not yet run. |

Two observer names showed the D9 clash suffix working on a live server: the
static room's observer is `gawk`, the dynamic room's `gawk 2` (the names
before OD33's rename to `gawkBridgeBot`).

What the deployment taught, all operator-side (now in self-hosting §15.1
and `docs/gotchas.md`):

- **An invite-only channel needs its own `#<token>` entry.** The mapped
  channel denies `@all` Enter; a grant on the root did not reach it, and
  the observer was refused (`not allowed to Enter`) until the channel got
  its own entry, after the deny.
- **The access token is a credential.** The first token was the bridge's
  own name. Any desktop client that sends it gets the bridge's grants, past
  that deny included, and the observer's client version
  (`gawk-mumble-bridge 1.0.0`) advertises the name. It was rotated to a
  random value.
- **A rotated key needs a rollout**: both charts read `gawk-voice` into
  env. The rotation added the new ACL entries first, rolled the bridge
  through a pod annotation, then removed the old entries.
- **The documented Secret recipe lacked `mumbleToken`**, which the §15.2
  values read; fixed. `autobanAttempts` runs at 200, not 0: well above a
  pod's join rate, and it keeps murmur's per-address throttle on password
  guessing.
