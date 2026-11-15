# R85 — A Mumble roster without a visible bot: murmur's Ice (docs/89)

**Status**: designed 2026-11-15 from the MI0 spike against real murmur
1.5.939 and 1.6.870 (§3), first with polling. **Redesigned around Ice
callbacks the same day** (OD7), once the reference murmur moved to
1.6.1001 (1.6.x RC2), which has the upstream fix for 1.6.870's callback
crash. The callback behaviour was measured on 1.6.1001 and 1.5.939
(MI0b, §3.2). The owner made decisions OD1–OD7 on 2026-11-15 (§2). No
question is open. MI1–MI5 not started. `MI` = Mumble Ice.

This touches only `gawk-mumble-bridge`: its Mumble side, its config, the
chart and its tests, plus the voice e2e and the dev stack. The relay, the
wire, the public `gawk-server/voice` package and the web app don't change.
The observer link and its `Roster` messages stay exactly as R79 left them.
Only where the bridge gets the roster from changes.

---

## 1. Why

R79's observer (docs/80 D4, OD33) is a Mumble user named `gawkBridgeBot`.
It sits in every voice room's channel, so the bridge can show the room
who is in Mumble before any gawk user joins voice (docs/80 OD28). It is
one user per voice-live room, so a server with several voice rooms shows
`gawkBridgeBot`, `gawkBridgeBot 2` and so on, in channels where people
talk. docs/80 §10 left that clutter as an open question. The owner's
verdict: it would be a better experience if Mumble users didn't see it.

The observer has two jobs:

1. **The roster.** It reports a channel's users, with mute and deafen
   state, to the room's home pod. This doesn't need a user in the
   channel: `rosterUsers` (`internal/bridge/observer.go`) reads the
   client's copy of the server's user list, which murmur sends to every
   client wherever it sits.
2. **Keeping temporary channels.** A dynamic room's channel is a murmur
   temporary channel. Murmur removes it once it's empty, so the observer
   creates it and stays in it (docs/80 D11). This job does need a user in
   the channel.

Murmur has no invisible users. The only way to see a server's users
without being one is murmur's administration RPC, ZeroC Ice. This
milestone moves job 1 to Ice and drops job 2: the first gawk user to join
a dynamic room's voice creates its channel.

**Speaking indicators are unaffected.** They are worked out in the
browser from the `VoiceFrame`s each voice user's own connection receives
(docs/80 D16), and never touch the observer, the relay or Ice. Neither
the Mumble protocol nor Ice has a talking state: talking exists only as
audio packets.

---

## 2. Owner decisions (2026-11-15)

| # | Question | Decision |
|---|---|---|
| OD1 | Where the roster comes from | **Murmur's Ice interface** (option 5 of §4). No Mumble user is needed to see a channel. |
| OD2 | Who keeps a dynamic room's temporary channel | **Nobody: the first gawk user to join voice creates it** (option 3 of §4). Murmur removes it once everyone has left, as before. |
| OD3 | Ice rights | **Read-write is acceptable.** Callbacks need the write secret (§3 F4), so that is what the bridge takes (D9). |
| OD4 | A murmur without Ice | **The visible observer stays as the fallback**, exactly as R79 ships it. |
| OD5 | Tests | **Both modes are covered**, by unit tests, real-murmur integration tests and the voice e2e. |
| OD6 | Ice failing at runtime, with Ice configured | **The roster reports unavailable, voice keeps working, and no visible bot appears** (D11). OD4's fallback means "when Ice isn't configured", not "whenever Ice fails": a bot that came and went with Ice's health, and dynamic rooms switching channel-keeping rules mid-call, would be worse than a short gap in the gawk roster. |
| OD7 | Polling or callbacks | **Callbacks** (option 5a of §4), decided once the reference murmur was upgraded from 1.6.870 to 1.6.1001. Polling survives only as the slow reconciliation that repairs what callbacks can silently lose (D15). |

The implementation follows mumisija's Ice client
(`github.com/Tuhis/mumisija/ice`, the owner's own MIT module, already in
production against the reference murmur) for how to speak Ice. Gawk
implements **only the minimum it needs** in its own package rather than
importing it (owner, 2026-11-15; D7). mumisija's client has no server
side, so the callback endpoint (D14) is new code either way.

OQ1 (Ice failing at runtime) became OD6. OQ2 (reporting 1.6.870's crash
upstream) closed with nothing to report: upstream had already fixed it
(§3 F3).

---

## 3. What the spikes found (2026-11-15)

The spike was a throwaway Ice client of about 440 lines: protocol 1.0,
encoding 1.1, twoway requests and a minimal callback endpoint. It was
written against mumisija's client and run against the
`mumblevoip/mumble-server` images `v1.6.870`, `v1.6.1001` and `v1.5.939`,
with Ice on TCP and both secrets set. Mumble users were the bridge's own
`internal/mumble` client, all on loopback.

### 3.1 MI0: Ice, polling and the 1.6.870 crash

| # | Finding | Measured |
|---|---|---|
| F1 | **Murmur 1.6.870 segfaults when the most recently registered Ice callback is dead and any server event fires.** | 3 runs of 3, exit 139: three callbacks registered, the last one's endpoint closed, one user connects. 1.5.939 and 1.6.1001 (RC2) survived the same case. |
| F2 | **On 1.6.870 a dead callback removes a *live* one.** When callback *a* fails, murmur logs "Removed" for the callback after it, *b*, and keeps calling the dead *a*. | 1 dead of 3 (the first): the live second callback got no further events. 1.5.939 and 1.6.1001 removed the right one. |
| F3 | **Fixed upstream; only 1.6.870 (RC1) is affected.** The cause: each event handler looped over a *reference* to the live callback list and removed a failed callback from it inside the loop (`badServerProxy` → `removeAll`), so removal shifted elements under the iteration. Mumble's `a780cc9259` ("Remove failed Ice callbacks only after notifying all callbacks", #7402, merged 2026-10-04) collects failures and removes them after the loop, at every notification site. Its message names `c80ece5bb` (`foreach` over a copy replaced by range-based `for`) as where the bug came in. | Fixed in `v1.6.1001` (1.6.x RC2, tag `v1.6.0-alpha2`, 2026-10-25) and `master`: neither F1 nor F2 reproduced on RC2 (3 runs). 1.5.939 still iterates with Qt's `foreach`, over a copy, so it was never affected. The reference deployment moved from 1.6.870 to 1.6.1001 on 2026-11-15. |
| F4 | The read secret (`icesecretread`) allows `getBootedServers`, `getUsers` and `getChannels`. **`addCallback` needs the write secret.** No secret gets `InvalidSecretException` on every call. | All three versions. |
| F5 | *(Corrected in MI0b.)* On 1.6.870 a stale registration seemed to take a new one with the same identity with it. That was F2, not identity matching: on fixed versions registrations are per proxy (F14). | 1.6.870 only. |
| F6 | Callbacks are fast: a self-mute toggle arrives over Ice in **0.23 ms median (1.1 ms max)**, against **0.31 ms (1.3 ms)** for a Mumble client watching the same user. | 40 toggles, 1.6.870, loopback. Re-measured in F16. |
| F7 | `getUsers` with 50 users connected takes **1.0 ms median, 4.2 ms max** on 1.6.870 and **0.73 ms / 4.4 ms** on 1.5.939. | 200 calls each. It sizes the reconciliation (D15). |
| F8 | **1.6 numbers its default virtual server `0`, 1.5 numbers it `1`.** `Meta.getBootedServers` gives the right identity on both (`s/0`, `s/1`). | All three versions. |
| F9 | Exception base types differ by version: 1.6's `InvalidSecretException` derives from `::MumbleServer::ServerException`, 1.5's from `::MumbleServer::MurmurException`. Match on the most-derived type ID only. | 1.6.870 and 1.5.939. |
| F10 | **Option 3 works.** Two token-group users who both create the same temporary channel: the loser gets `Permission denied: ChannelName`, finds the channel by name and joins it, and they end up in one channel. The channel survives its creator leaving while others stay. Murmur removes it when the last user leaves. Ice sees it with `temporary = true` throughout. | 1.6.870 and 1.5.939, with the docs/80 §5 ACL on a fresh parent. The bridge's `enterTemp` already handles the race this way (`internal/bridge/mumbleconn.go`). |
| F11 | `getUsers`, and every user event, carries every user's IP address, comment, identity and plugin context, as well as the fields a roster needs. | All versions (and the reference ini's own comment). |

### 3.2 MI0b: callbacks on fixed murmur

| # | Finding | Measured |
|---|---|---|
| F12 | **Murmur dials a callback lazily and keeps one connection.** No connection arrives at `addCallback`, only at the first event, and that one TCP connection then carries every event. Murmur sends events **oneway**: it stores the proxy as `ice_oneway()->ice_connectionCached(false)` (`impl_Server_addCallback`), so it never waits for a reply. | 0 connections 1 s after `addCallback`, 1 connection after 21 events. 1.6.1001 and 1.5.939. |
| F13 | **One refused delivery removes a registration for good, and nothing tells the client.** With the endpoint back on the same port, events don't come back until the client calls `addCallback` again. Re-adding a proxy that is still registered is a no-op: no duplicate deliveries. | A closed endpoint, one event, the endpoint reopened: no delivery; after re-adding: delivered. 1.6.1001 and 1.5.939. |
| F14 | **Registrations are per proxy** (identity and endpoint). Two registrations with the same identity at different endpoints are independent: one failing leaves the other. | 1.6.1001 and 1.5.939. |
| F15 | **An endpoint that blackholes (a lost node, a deleted pod's IP) freezes murmur's event loop** until the connect times out, about twice the endpoint's timeout, because Ice retries once. Every user's actions on the whole server wait meanwhile. Then murmur drops the registration (F13), so it happens once per dead registration. A refused connection (RST) costs nothing. | A self-mute toggle seen by another client, with one registration pointing at `192.0.2.1:9`: endpoint timeout 100 ms → **0.22 s**; 250 ms → **0.51 s** (1.5.939: 0.53 s); 1 s → **2.0 s**; infinite (`-t -1`, which the spike first sent) → about **4.5 min**. Baseline 0.25 ms. 1.6.1001 unless noted. |
| F16 | Callbacks stay faster than a Mumble client: **0.22 ms median (1.1 ms max)** against **0.29 ms (1.3 ms)** on 1.6.1001, **0.25 ms / 0.35 ms** on 1.5.939. | 40 toggles each, loopback. |
| F17 | `Meta.getVersion` tells the versions apart: `1.6.870`, `1.6.1001`, `1.5.939`. | All three versions. |

**What it settles:**
- Callbacks are safe on 1.5.x and on 1.6.1001 and later (F3). They aren't
  safe on 1.6.870, so the bridge never registers there (D16).
- Callbacks need the write secret (F4) and a way for murmur to dial each
  bridge pod (D14).
- Murmur can drop a registration without telling us (F13). So a slow
  reconciliation re-registers and re-reads the state (D15).
- A dead pod's registration can freeze murmur for twice the endpoint
  timeout (F15). So the bridge sends a short timeout, 100 ms by default,
  and removes its registration before it exits (D14, D17).

**To reproduce F1:** start murmur with
`MUMBLE_CONFIG_ICE='tcp -h 127.0.0.1 -p 6599'`,
`MUMBLE_CONFIG_ICESECRETWRITE` and `MUMBLE_CONFIG_ICESECRETREAD`, then
`addCallback` three proxies to three TCP listeners, close the third, and
connect a Mumble user. 1.6.870 crashes; 1.6.1001 doesn't. **F15:**
`addCallback` a proxy whose endpoint is `-h 192.0.2.1 -p 9 -t <ms>` and
time another client's view of a mute toggle.

---

## 4. Options considered

| Option | What it does | Taken? |
|---|---|---|
| 1. Park the observer in its own channel | The bot leaves the talking channels but still shows up in the tree. Doesn't keep temporary channels. | No: still visible. |
| 2. One observer per bridge pod | One bot per pod instead of one per room (docs/80 §10). Still visible, and temporary channels need another keeper. | No. |
| 3. The first gawk user creates the temporary channel | Removes the keeper role. A native user can't wait in a dynamic room's channel before a gawk user is in voice. | **Yes (OD2).** |
| 4. Roster from the voice users' own connections | No extra connection while someone from gawk is in voice. Still needs something for the gap before anyone joins (OD28). | No. A degraded mode for Ice outages was considered and not taken (OD6). |
| 5. Murmur's Ice | No Mumble user at all. Needs Ice reachable and a secret, and a first-party Ice client. | **Yes (OD1).** |
| 5a. …with callbacks | Push, faster than a Mumble client (F16). Needs the write secret, an endpoint murmur can dial in every pod, and care with dead registrations (F13, F15). | **Yes (OD7, D2).** Not on 1.6.870 (D16). |
| 5b. …by polling | `getUsers` and `getChannels` every 250 ms per pod. Roster lag up to one interval. Read secret only. | The first design, replaced by 5a (OD7). It survives as the 30 s reconciliation (D15). |
| Import `mumisija/ice` | A tested, dependency-free client. No server side. | No: the owner asked for gawk's own minimum (D7). The licence would allow it. |

---

## 5. Design

| # | Decision | Why |
|---|---|---|
| D1 | **Two roster modes, chosen by configuration.** `-mumble-ice-addr` set: **Ice mode**. Unset: **observer mode**, R79's behaviour byte for byte. The bridge never switches mode at runtime. | OD1, OD4. One mode per process keeps the channel rules (D5) stable for the life of a call. |
| D2 | **Ice mode is pushed by callbacks.** Each bridge process in Ice mode keeps one `ServerCallback` registration with murmur (D14), from start to exit, **whether or not it holds an observer link**, and a **mirror** of the server's users and channels. When it connects, it reads `getVersion` (D16), registers if D15's rule allows it, then reads `getUsers` and `getChannels` into the mirror, so no event falls in a gap. Events that arrive while that read is in flight are buffered and applied after it. That read, and the one after a reconnect (D11), are not reconciles in D15's sense. Then each event updates the mirror: `userConnected` and `userStateChanged` upsert the user by session, `userDisconnected` deletes it, the three `channel*` events do the same for channels, and `userTextMessage` is ignored. Every observer on the pod reads the same mirror. | OD7. A roster change reaches the pod in under a millisecond (F16), then the existing 50 ms coalesce and the link hop. Every event carries the user's or channel's full state, so applying one is idempotent. A buffered event older than the snapshot can briefly undo a newer state; the next event or D15 corrects it. Keeping the registration independent of observer links means rooms coming and going never register again, and a new link finds the mirror warm. |
| D3 | **The observer link is unchanged.** The bridge pod that accepts a room's link runs an *Ice watcher* in place of a Mumble connection. It resolves the room's channel (D4) in the mirror after each change, and sends a `Roster` when the channel's users differ from the last one sent. Message shape, `Available`/`Reason`, the home pod's merge with session links, redial and drain handover all stay as docs/80 D2–D4 and §11.5 define them. | No relay, wire or `voice` package change. The relay's `/statusz` `voiceObserver` keeps its meaning. |
| D4 | **Channel resolution from the mirror's channel list.** A static room's `/`-separated path is walked from the root (ID 0) by name, with the same rules as `voice.ChannelPath` and `mumble.Client.FindPath`. A dynamic room's channel is the child of `-mumble-temp-parent` named by `tempChannel()` (docs/80 D11, unchanged). IDs are re-resolved on every channel event, because channels come and go. | One naming rule for both modes, so the key-based temporary channel name stays fleet-stable. |
| D5 | **Temporary channels in Ice mode: the first voice user creates it.** A voice user's pod calls `enterTemp` with no wait (it never waits for an observer), creating the channel or joining it if it exists, with the existing `ChannelName` race handling (F10). Until then the room's roster is `Available` and empty. Once everyone has left, murmur removes the channel as before. In observer mode nothing changes. | OD2. The cost: a native Mumble user can't wait in a dynamic room's channel before a gawk user joins voice. That narrows docs/80 OD28 for dynamic rooms only. Static rooms keep their roster before anyone joins. |
| D6 | **`-max-temp-channels` in Ice mode** counts the temporary channels under `-mumble-temp-parent` in the **voice user's own Mumble connection's channel tree**, just before `enterTemp` would create one, and refuses the *create*, never a join, with `temp_channel_cap` once the count reaches the cap. That connection has the server's whole tree as soon as it's logged in, on whichever pod it runs, so the cap doesn't depend on the mirror: a voice session can land on a pod with no observer link (docs/80 OD22). The count covers the whole server, unlike observer mode's count of a pod's own observers. Two pods can race past the cap by one each. The cap is an abuse guard, not an exact quota. | docs/80 OD19 and D23 keep a cap. Without a keeper there's nothing per pod to count. |
| D7 | **A minimal first-party Ice client and callback endpoint, `internal/murmurice`.** Ice protocol 1.0 over TCP, encoding 1.1. **Client:** twoway requests multiplexed on one connection, validate and close messages, one reconnect-and-retry for idempotent calls, and a deadline on every call. It sends `Meta.getBootedServers`, `Meta.getVersion`, `Server.getUsers`, `Server.getChannels`, `Server.addCallback` and `Server.removeCallback`, nothing else. **Endpoint** (D14): accept, send validate connection, decode requests, and dispatch the seven `ServerCallback` operations plus `ice_ping` and `ice_isA` (true only for `::MumbleServer::ServerCallback`) to one servant. Anything else gets "operation does not exist", and a twoway request gets its reply. No classes, batch requests, compression, TLS endpoints or locators. Modelled on `mumisija/ice` (`protocol.go`, `conn.go`, `encoding.go`). Each struct decoder must use up exactly its bytes, and every reply and event must decode to its end, so a murmur whose `User` layout changes fails loudly with `ice_protocol` instead of producing a wrong roster. Murmur 1.5 or later (the `User.version2` field). | Owner: only the minimum, in gawk. The spike's version of this scope was about 440 lines. `CGO_ENABLED=0` and the Go licence allowlist are unaffected. |
| D8 | **Which virtual server.** `-mumble-ice-server-id` if set. Otherwise `getBootedServers`: exactly one booted server is used. More than one is a configuration error (`ice_server_ambiguous`). | F8: 1.6 and 1.5 number the default server differently, so a hard-coded `1` breaks 1.6. |
| D9 | **The secret.** `-mumble-ice-secret` (`GAWK_BRIDGE_MUMBLE_ICE_SECRET`, chart `mumble.ice.secretRef`) is murmur's **write** secret (`icesecretwrite`), sent as the `secret` request context on every call. A read secret fails `addCallback` with `ice_secret`, which the bridge reports as such rather than as an outage. It is logged only as set or unset, like every bridge secret. Ice sends it in clear text, so the Ice port must stay on the private network. The chart's `networkPolicy` egress gains the Ice port. | F4, OD3. The write secret grants full Ice administration, so it is handled like the access token: never logged, never in a URL or status. |
| D10 | **The bridge keeps nothing from Ice it doesn't need.** Every `User` murmur sends, in `getUsers` and in each event, carries the user's IP address, comment, identity and plugin context (F11). The decoder skips those bytes without storing them: `murmurice.User` has only session, user ID, name, channel and the mute, deafen and suppress flags. A test walks the package's types and fails if a field named `Address`, `IP`, `Comment`, `Identity` or `Context` appears, or if any code logs an Ice payload. This joins docs/80 D24's prohibitions. | The CLAUDE.md rule that no event ever carries a publisher IP, applied in spirit to native users' IPs: the bridge never had them before and shouldn't start keeping them now. |
| D11 | **Ice failing in Ice mode** (the client connection lost, a wrong secret, a protocol error, an ambiguous server): every observer on the pod reports `Available: false` with the reason (`ice_unavailable`, `ice_secret`, `ice_protocol`, `ice_server_ambiguous`, all counted in `gawk_bridge_mumble_failures_total{reason}`) until the client reconnects and re-reads the state. A murmur restart loses every registration, and it shows up as exactly this. On each reconnect the pod reads `getVersion` again (D16), then registers again only under D15's rule: a `registered` pod does if fewer than 2 registrations are unanswered, an `unreachable` one doesn't, a `probing` one ends its probe as not run, and an `unsafe` one never does. Voice sessions keep running, because they are their own Mumble connections. The client redials with the existing backoff. **No visible observer appears** (OD6). | Ice can't take voice down with it. Events that arrive while the client is down are applied but not trusted until the re-read. A flaky Ice connection must not add registrations beyond D15's count, nor let an unreachable pod skip its backoff. |
| D12 | **Status and metrics.** The bridge's `/statusz` gains `roster: "ice" \| "observer"` and, in Ice mode, `ice: {available, reason, serverId, murmurVersion, callbacks, unanswered, nextProbeInMs, lastEventAgeMs, lastReconcileAgeMs}`. `callbacks` is D15's state: `registered`, `unreachable`, `probing` or `unsafe` (D16); while the Ice client is down it keeps its last value and `available` says so. `unanswered` is D15's count. `nextProbeInMs` is set while `unreachable`. New metrics: `gawk_bridge_ice_events_total{op}`, `gawk_bridge_ice_reconciles_total{result}`, `gawk_bridge_ice_reconcile_drift_total`, `gawk_bridge_ice_registrations_total{result}` and the gauge `gawk_bridge_ice_registered`. `gawk_bridge_observers` keeps counting observer links in both modes. | docs/self-hosting.md §15.4 "Is it working?" needs an answer in both modes, including when the next probe is due. |
| D13 | **Names.** In Ice mode no Mumble user named `-mumble-observer-name` ever exists. The flag applies only in observer mode, and the observer name ladder (docs/80 §11.5, 1000 names) only there. | OD33 becomes observer mode's rule. |
| D14 | **The callback endpoint.** Each bridge pod listens on `-mumble-ice-callback-listen` (default `:6503`, TCP). It registers the proxy `<identity> -o:tcp -h <callback-host> -p <port> -t <callback-timeout>`: the host is `-mumble-ice-callback-host` (the chart sets the pod IP through the downward API) and the timeout `-mumble-ice-callback-timeout` (default **100 ms**). The identity is `gawk-bridge-<128 random bits, hex>`, new for each process. The endpoint serves only that identity; any other identity, such as an old process's registration arriving at a reused pod IP, gets "object does not exist". The listener is **never routed publicly**: the chart's NetworkPolicy lets in only murmur, from `networkPolicy.murmur` selectors. | F12: murmur dials each pod, so each pod needs an address murmur can reach. F15: the endpoint timeout bounds how long a dead pod can freeze murmur, at about twice its value, so it defaults low. An in-cluster connect takes well under a millisecond. The random identity means a reused IP can't inherit a dead process's events, and it makes injecting fake events harder. Murmur logs the proxy when it adds or removes it, so the identity is a capability against the network, not against murmur's log readers. |
| D15 | **Reconciliation, and when to register.** Every `-mumble-ice-reconcile` (default **30 s**) the pod re-reads `getUsers` and `getChannels`. Only this periodic re-read is a *reconcile*. If it differs from the mirror (*drift*), the pod replaces the mirror, sends any `Roster` that changed and counts `gawk_bridge_ice_reconcile_drift_total`. The pod counts **unanswered registrations**: `addCallback` calls since the last event it received. Receiving any event sets the count to 0. **One rule covers every `addCallback` call site** (start, reconnect, re-add): **call it only if the count is below 2, and increment the count when you do.** The only exception is a probe, described below. Per process, the state is one of: **`registered`** (the initial state, count 0): on every drifting reconcile the pod re-adds if the rule allows (a no-op if murmur still has it, a repair if it dropped it, F13). On a drifting reconcile with the count already at 2, it calls `removeCallback`, logs once and becomes `unreachable`. **`unreachable`**: no `addCallback` at all, and the roster runs on the reconciliation alone, as on 1.6.870. After `-mumble-ice-callback-probe` (default **10 min**) it becomes **`probing`**: exactly one `addCallback`, allowed whatever the count. An event ends the probe as passed: `registered`, count 0, backoff reset. The first drifting reconcile after the probe's `addCallback` with no event ends it as failed: `removeCallback`, `unreachable`, and the next probe waits twice as long, up to 2 h. A probe interval without drift stays pending. A client reconnect during a probe ends it as not run: `unreachable`, with the next probe one unchanged backoff away. **`unsafe`**: D16. The state, the count and the backoff live for the process; only an event clears them. | F13: murmur drops a registration after one failed delivery and doesn't say so, and a drifting re-read is the only sign of it the pod can see. **Why the bound holds:** each registration costs murmur at most one F15 freeze (only if the address blackholes and an event fires). The rule allows two registrations without an event between them, plus one per probe. So **an unreachable pod costs at most two freezes per process start, then at most one per probe.** Re-adding on every drift, not only drift before the first event, means a registration dropped later (a conntrack flush, a blip) is restored at the next drifting reconcile. Changes that revert within an interval show no drift, so they cause no re-adds and cost nothing, and the mirror they leave is already right. A quiet server that restarts murmur twice without a single event in between can push a healthy pod to `unreachable`; its first probe brings it back. At 30 s the reconciliation costs one `getUsers` per pod per 30 s (F7). |
| D16 | **No callbacks on 1.6.870.** On every connect and reconnect, the bridge reads `Meta.getVersion` (F17). On 1.6.0 up to and including 1.6.870 it never calls `addCallback`: `callbacks` is `unsafe`, it logs why once, and the roster runs on D15's reconciliation alone at `-mumble-ice-reconcile`. Every other version 1.5 or later registers, so a reconnect to an upgraded murmur leaves `unsafe` and starts as `registered` would. | F1, F3. A gawk pod that crashes must never crash the operator's Mumble server. A slow roster beats none, and operators see the reason in `/statusz`. |
| D17 | **Leaving cleanly.** On drain and at exit, the bridge calls `removeCallback` before it closes its listener, whatever D15's state is. An `InvalidCallbackException` (murmur already dropped it, F13) or a failed call counts as done, not as a D11 failure. A pod that dies without doing so (SIGKILL, OOM, a lost node) leaves a registration that murmur drops at its next event: at no cost if the address refuses, or one freeze of about twice the callback timeout if it blackholes (F15). | F15 is the only cost callbacks put on murmur's other users, so normal rollouts must never pay it. |

### 5.1 Knobs (bridge only; flags, `GAWK_BRIDGE_*` envs and the chart)

| Flag | Default | Meaning |
|---|---|---|
| `-mumble-ice-addr` | — | murmur's Ice endpoint, `host:port` over TCP. Set: Ice mode (D1). |
| `-mumble-ice-secret` | — | murmur's `icesecretwrite` (D9). Prefer the env or `secretRef`. |
| `-mumble-ice-server-id` | discovered | the virtual server's ID (D8) |
| `-mumble-ice-timeout` | `2s` | the deadline on each Ice call the bridge makes, and the dial timeout |
| `-mumble-ice-callback-listen` | `:6503` | the callback endpoint's listen address (D14) |
| `-mumble-ice-callback-host` | — | the address murmur dials, required in Ice mode; the chart sets the pod IP (D14) |
| `-mumble-ice-callback-timeout` | `100ms` | the timeout in the registered endpoint, bounded to 20 ms–1 s; it bounds F15's freeze (D14) |
| `-mumble-ice-reconcile` | `30s` | the reconciliation interval, bounded to 5 s–5 min (D15, D16) |
| `-mumble-ice-callback-probe` | `10m` | the first probe after a pod gives up on callbacks, doubling up to 2 h; bounded to 30 s–2 h (D15) |

These go in the bridge's options struct under its carry-all test, the
same rule as the relay's `registryOptions` (CLAUDE.md, docs/80 D23).
Setting any `-mumble-ice-*` flag without `-mumble-ice-addr`, or Ice mode
without `-mumble-ice-callback-host`, refuses to start.

---

## 6. What users see

- **In Mumble (Ice mode):** no `gawkBridgeBot` anywhere. A gawk voice user
  appears as `Alice [web]`, as today. A dynamic room's channel appears
  when its first gawk user joins voice and goes once everyone has left.
  If a bridge pod dies without cleaning up and its address blackholes,
  everyone on the server sees one pause of about 0.2 s at the next
  server event (F15, D14). A bridge pod murmur can't reach costs up to two such
  pauses each time it starts (so a rollout of N pods, up to 2N), then one
  per probe, every 10 min at first and every 2 h at most (D15).
- **In gawk:** the same voice roster as before, as fast as the observer's
  (F16). Speaking indicators are exactly as fast as before. A dynamic
  room shows no Mumble users until someone from gawk is in voice, because
  until then there's no channel to be in. On murmur 1.6.870 (D16), or when
  murmur can't reach the bridge (D15), roster changes show within 30 s.
- **For an operator:** Ice's address and write secret to configure, and
  murmur must be able to reach the bridge pods. If they don't configure
  Ice, today's bot stays.

---

## 7. Chunks and acceptance criteria

### Milestone goals

| # | Goal | Verified by |
|---|---|---|
| G1 | With Ice configured, no Mumble user created by the bridge exists except voice users | MI4 pass 1 and pass 5 check the SuperUser's user list. MI5 on the reference server. |
| G2 | A static room's roster shows its native users before anyone joins voice, in both modes | MI4 pass 1, run in each mode |
| G3 | A dynamic room's channel is created by its first voice user in Ice mode and kept by the observer in observer mode, and murmur removes it once it's empty in both | MI2 integration tests. MI4 pass 5 in each mode. |
| G4 | Voice keeps working through an Ice outage, the roster reports unavailable and then recovers, and no bot appears | MI2 integration test. MI4 pass 7. |
| G5 | The bridge keeps no IP, comment, identity or context from Ice, and logs no Ice payload | MI1's type-walk and log test |
| G6 | Observer mode is unchanged | Every existing bridge test and the existing MI4 passes, green in observer mode with no edits to their assertions |
| G7 | Gawk never crashes murmur, never freezes it for longer than about twice the callback timeout at a time, and, per bridge process start, freezes it at most twice before an unreachable pod stops registering, then at most once per probe: no registration on 1.6.870, `removeCallback` on every clean exit | MI1 criterion 6, MI2 criteria 2, 5, 8 and 9, MI4 pass 8 |
| G8 | A native user's join, leave and mute reach the gawk roster in callback time, and a silently dropped registration is repaired within one reconciliation | MI2 criteria 3 and 7 |

### MI0 — Spikes (done 2026-11-15)

| # | Criterion | Result |
|---|---|---|
| 1 | Callbacks vs polling on real murmur 1.5 and 1.6 | 1.6.870 crashes on a dead callback (F1–F3); fixed in 1.6.1001. Callbacks chosen once the reference moved to it (OD7). |
| 2 | Which secret each needs | Read for polling, write for callbacks (F4) |
| 3 | Option 3 works on real murmur, race included | Yes (F10) |
| 4 | What a dead or unreachable callback costs murmur (MI0b) | Nothing if refused; about 2 × the endpoint timeout if blackholed, once (F13, F15) |

### MI1 — `internal/murmurice`

| # | Criterion |
|---|---|
| 1 | D7's client: dial, validate, multiplexed twoway calls, close, reconnect with one retry for idempotent calls, a deadline on every call. D7's endpoint: validate on accept, oneway and twoway requests, the seven `ServerCallback` operations, `ice_ping` and `ice_isA`, "object does not exist" for any other identity. |
| 2 | Unit tests against an in-process fake Ice server and fake caller (mumisija's `icetest` shape): framing, sizes ≥ 255, strings, dictionaries, user exceptions matched on the most-derived type ID for both 1.5's and 1.6's hierarchy (F9), a reply or event that ends early or runs long (`ice_protocol`), reconnect after the server closes, a call outliving its deadline, an event for a foreign identity, the registered proxy's encoding (`-o`, host, port, timeout) |
| 3 | The D10 type-walk test, and a test that no Ice payload or secret reaches a log line |
| 4 | Integration tests against real murmur **1.5.939, 1.6.870 and 1.6.1001**. `testdata/murmur.sh` gains 1.6.1001 and enables Ice on its `main` server with both secrets. Murmur's container reaches the test's callback endpoint through `--add-host host.docker.internal:host-gateway`, the endpoint listening on the docker gateway address. It exports `GAWK_TEST_MURMUR_ICE`, `GAWK_TEST_MURMUR_ICE_SECRET` and `GAWK_TEST_MURMUR_CALLBACK_HOST`. The tests cover server discovery (`0` on 1.6, `1` on 1.5), `getVersion`, `getUsers` and `getChannels` decoding with 50 users (a name, mute, deafen and channel each checked), the wrong secret, no secret, a read secret refused at `addCallback`, and each of the seven events arriving and decoding |
| 5 | F13 and F14 kept as tests: a refused delivery removes the registration and re-adding restores it; re-adding while registered doesn't duplicate; two endpoints with one identity are independent |
| 6 | F15 kept as a test: with a registration pointing at an unroutable address and the default 100 ms timeout, another client's mute toggle takes under 0.5 s, and the next takes under 50 ms (G7) |

### MI2 — The bridge's Ice mode

| # | Criterion |
|---|---|
| 1 | D1–D17 implemented. Observer mode's code path is untouched apart from the mode switch. |
| 2 | **Both modes in the bridge's unit tests.** The fake-murmur harness (`internal/mumble/mumbletest`) gains a fake Ice endpoint and a fake callback caller fed from the same fake server state, and the observer and temporary-channel tests are table-driven over `{observer, ice}`. D2's buffering of events during the initial read has its own test. **D15's state machine has table tests**, each asserting the exact number of `addCallback` calls: a change that reverts within an interval (no drift, no call); a drifting reconcile after earlier events (one re-add); a second drifting reconcile with no event (no call, `removeCallback`, `unreachable`); in `unreachable`, N client reconnects and N observer links coming and going (zero calls); with the count at 1, a reconnect then a link cycle (at most one call); D2's and D11's initial reads drifting (no call, not a reconcile); a probe that gets an event (`registered`, count 0), that drifts (backoff doubled), that sees no drift (still pending), or that a reconnect interrupts (not run, backoff unchanged). |
| 3 | **Both modes in the real-murmur integration tests** (`internal/bridge/murmur_integration_test.go`), run as {1.5.939, 1.6.1001} × {observer, ice}: the roster before voice for a static room; a native user's join, leave, self-mute and server-mute reaching the roster within 200 ms in Ice mode (the 50 ms coalesce plus margin); a dynamic room's channel created by the first voice user and joined by a second user who raced it; the creator leaving while others stay; removal once it's empty; the D6 cap refusing a create but not a join, including for a voice user on a pod that holds no observer link; in Ice mode, a check that no user named the observer name ever appears |
| 4 | An Ice outage (a TCP proxy in front of Ice, cut and restored): observers go `ice_unavailable`, an established voice session keeps sending and receiving, the roster comes back after recovery, and no Mumble user is created during the outage. A murmur restart mid-test ends the same way, with the registration made again. |
| 5 | A drain hands a room's watcher to the surviving pod in Ice mode, as docs/80 §11.5 does for the observer, and the draining pod's `removeCallback` shows in murmur's log before its listener closes |
| 6 | The options carry-all test covers every new knob |
| 7 | **Silent loss repaired.** The pod first receives events normally. Then the test closes the bridge's callback listener for one event, so that one delivery is refused and murmur drops the registration, and reopens it. It changes a native user's mute: the roster has it within one reconciliation interval (set to 5 s in the test), `gawk_bridge_ice_reconcile_drift_total` counts it, and `gawk_bridge_ice_registrations_total` counts the re-add. The *next* mute then arrives in callback time (under 200 ms) and `callbacks` is `registered`. A callback host murmur can't reach ends in `callbacks: "unreachable"`, with the roster still updating at reconciliation pace. |
| 8 | **Version gate.** Against 1.6.870 the bridge never sends `addCallback` (no "Added Ice ServerCallback" in murmur's log), reports `callbacks: "unsafe"`, and its roster follows changes at reconciliation pace |
| 9 | **An unreachable pod stops costing murmur.** With the callback host set to an unroutable address, the reconciliation at 5 s and the probe at 30 s, a native client moves one step around a ring of 10 channels every 700 ms for at least 70 s, while a second client times each move. Seven or eight moves fall in each interval, never ten, so every interval ends in a different channel and every reconcile drifts. **Lower bounds**, so the test can't pass vacuously: at least one move takes 150 ms or more before `unreachable` (the address really blackholes; if the runner returns an immediate unreachable instead, the test fails with that message rather than passing); murmur's log shows the probe's "Added Ice ServerCallback" between 30 and 37 s after `unreachable`; `gawk_bridge_ice_registered` goes 1 → 0 → 1. **Upper bounds**: at most two moves take longer than 50 ms before `unreachable`, none between `unreachable` and the probe, and at most one in the probe's interval. |

### MI3 — Config, chart, docs

| # | Criterion |
|---|---|
| 1 | §5.1's flags, `GAWK_BRIDGE_*` envs and chart values (`mumble.ice.addr`, `mumble.ice.secretRef`, `mumble.ice.serverId`, `mumble.ice.timeout`, `mumble.ice.callbackPort`, `mumble.ice.callbackTimeout`, `mumble.ice.reconcile`, `mumble.ice.callbackProbe`). The callback host comes from `status.podIP`. The chart's NetworkPolicy egress allows the Ice port, and its ingress allows the callback port from `networkPolicy.murmur` only, both when `mumble.ice.addr` is set. `helm template` tests for both modes. |
| 2 | `docs/self-hosting.md` §15: the Ice prerequisites (`ice=`, `icesecretwrite`, murmur 1.5 or 1.6.1001 and later, murmur able to reach the bridge pods, all of it on the private network) as the recommended setup, observer mode as the fallback, and §15.4 in both modes, with `callbacks: "unreachable"` called out as a misconfiguration and a bridge restart as the way to skip the probe backoff once it's fixed |
| 3 | The bridge README's knob table. `docs/gotchas.md` entries for F1–F4, F8, F9, F13 and F15. |
| 4 | The dev voice stack (`dev/compose.voice.yml`) runs Ice mode by default, with murmur on 1.6.1001 and observer mode one variable away |

### MI4 — The voice e2e in both modes

| # | Criterion |
|---|---|
| 1 | `e2e/voice.mjs` takes `VOICE_ROSTER=ice\|observer`. The `e2e-voice` CI job gains a matrix axis `roster: [ice, observer]`, so both run on every PR that runs it today. Its murmur moves to 1.6.1001 and has Ice on in both runs (observer mode simply doesn't configure it), with the host-gateway mapping of MI1 criterion 4 so murmur can dial both bridges. |
| 2 | Passes 1–6 run in both modes. Their assertions are unchanged except where §6 says the behaviour differs. |
| 3 | Pass 1 in Ice mode also checks, through the SuperUser's user list, that no user named the observer name exists (G1) |
| 4 | Pass 5 in Ice mode: before anyone joins voice the channel doesn't exist and the roster is empty; alice's join creates it; the native user joins it by its fleet-stable name and appears in the roster; the SuperUser's user list has no user named the observer name, before or after alice's join (G1); the channel is gone once everyone has left |
| 5 | A new pass 7 (Ice mode only): the Ice proxy is cut mid-call, alice and the native user still hear each other, the relay reports the observer `unavailable`, the proxy is restored, and the roster is back with no bot ever seen |
| 6 | A new pass 8 (Ice mode only): the bridge pod watching the static room is SIGKILLed. The room's observer link moves to the other pod, the native user's next mute reaches the roster, murmur stays up, and its log shows the dead registration removed (G7) |

### MI5 — Owner pass on the reference deployment

| # | Criterion |
|---|---|
| 1 | Murmur on 1.6.1001 (done 2026-11-15). The bridge's values in `Tuhis/ioio` point at `mumble-ice` with the write secret, and murmur's NetworkPolicy, if any, lets it reach the bridge pods on the callback port. Every bridge pod's `/statusz` says `roster: "ice"`, `available` and `callbacks: "registered"`. |
| 2 | No `gawkBridgeBot` on the server. The friends' static room shows its Mumble users in gawk before anyone joins voice, and a native user's mute shows at once. |
| 3 | A `?voice=1` room: its channel appears with the first gawk voice user, a desktop Mumble user joins and talks both ways, and the channel goes once everyone has left |
| 4 | A bridge rollout: murmur's log shows each old pod's `removeCallback` and each new pod's `addCallback`, and nobody in Mumble notices |
| 5 | mumisija's serveradmin keeps working beside the bridge on the same Ice endpoint |

---

## 8. Risks

- **A dead pod freezing murmur** (F15). Bounded to about twice
  `-mumble-ice-callback-timeout` (0.2 s by default) once per registration
  left behind, and only when its address blackholes. Clean exits remove
  theirs (D17). An operator who raises the timeout raises this cost with
  it, which is why the flag is capped at 1 s.
- **Murmur can't reach the pods.** A murmur outside the cluster usually
  can't dial pod IPs, and a NetworkPolicy that drops packets has the same
  effect. If the address blackholes, each registration costs one F15
  freeze. D15 detects it after at most two per bridge process start (a
  rollout of N pods costs up to 2N, a crash-looping pod 2 per restart),
  removes the registration and probes only rarely, and the roster keeps
  working every 30 s. `callbacks: "unreachable"` is the signal to fix
  it, and restarting the bridge pod skips the probe backoff once it's
  fixed. Observer mode is the alternative for such setups.
- **The write secret.** It grants all of Ice's administration: kicking,
  banning, ACLs, registrations. D9 handles it like the access token, and
  Ice must stay on the private network, as it already must for
  mumisija.
- **Fake events.** Anyone who can reach a pod's callback port and knows
  its identity can feed it a false roster (not voice). The NetworkPolicy
  and the random identity (D14) make that hard. Murmur's log shows the
  identity, so operators with log access are trusted, as they are with
  the write secret.
- **Murmur changes its Slice.** D7's exact-length decoding turns a layout
  change into a loud `ice_protocol`, and the integration matrix runs
  every supported version. A new murmur version joins the matrix before
  the reference deployment upgrades.
- **Hosted murmur without Ice.** Many hosted Mumble services don't expose
  Ice. Observer mode is their path (OD4).

---

## 9. Out of scope

- Polling as the primary path. It is only D15's reconciliation and D16's
  1.6.870 fallback.
- Using Ice for anything but the roster: moderation, channel creation,
  ACLs, registration, `MetaCallback`, context-menu callbacks,
  authenticators. The voice users' own connections keep doing what they
  do today.
- Ice over TLS (`ssl` endpoints), more than one virtual server per
  bridge, and Ice through a locator or IceGrid.
- Any change on the relay, the wire, the `voice` package or the web app.
