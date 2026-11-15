# R85 — A Mumble roster without a visible bot: murmur's Ice (docs/89)

**Status**: designed 2026-11-15 from the MI0 spike against real murmur
1.5.939 and 1.6.870 (§3), and the callback crash checked against 1.6.1001
(RC2) the same day. The owner made decisions OD1–OD6 on 2026-11-15 (§2).
No question is open (§2.1). MI1–MI5 not started. `MI` =
Mumble Ice.

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
audio packets. So no option here changes how fast speaking shows up.

---

## 2. Owner decisions (2026-11-15)

| # | Question | Decision |
|---|---|---|
| OD1 | Where the roster comes from | **Murmur's Ice interface** (option 5 of §4). No Mumble user is needed to see a channel. |
| OD2 | Who keeps a dynamic room's temporary channel | **Nobody: the first gawk user to join voice creates it** (option 3 of §4). Murmur removes it once everyone has left, as before. |
| OD3 | Ice rights | **Read-write is acceptable.** The design needs only read (§3 F4, D9), so it asks operators for the read secret and accepts either. |
| OD4 | A murmur without Ice | **The visible observer stays as the fallback**, exactly as R79 ships it. |
| OD5 | Tests | **Both modes are covered**, by unit tests, real-murmur integration tests and the voice e2e. |
| OD6 | Ice failing at runtime, with Ice configured | **The roster reports unavailable, voice keeps working, and no visible bot appears** (D11). OD4's fallback means "when Ice isn't configured", not "whenever Ice fails": a bot that came and went with Ice's health, and dynamic rooms switching channel-keeping rules mid-call, would be worse than a short gap in the gawk roster. |

The implementation follows mumisija's Ice client
(`github.com/Tuhis/mumisija/ice`, the owner's own MIT module, already in
production against the reference murmur) for how to speak Ice. Gawk
implements **only the minimum it needs** in its own package rather than
importing it (owner, 2026-11-15; D7).

### 2.1 Open

None. OQ1 (Ice failing at runtime) became OD6. OQ2 (reporting the murmur
crash upstream) closed: upstream had already fixed it (§3 F3), so there
is nothing to report.

---

## 3. What the MI0 spike found (2026-11-15)

The spike was a throwaway Ice client of about 440 lines: protocol 1.0,
encoding 1.1, twoway requests and a minimal callback endpoint. It was
written against mumisija's client and run against
`mumblevoip/mumble-server:v1.6.870` (the reference deployment's version)
and `v1.5.939`, with Ice on TCP and both secrets set. Mumble users were
the bridge's own `internal/mumble` client, all on loopback.

| # | Finding | Measured |
|---|---|---|
| F1 | **Murmur 1.6.870 segfaults when the most recently registered Ice callback is dead and any server event fires.** | 3 runs of 3, exit 139: three callbacks registered, the last one's endpoint closed, one user connects. 1.5.939 and 1.6.1001 (RC2) survived the same case. |
| F2 | **On 1.6.870 a dead callback removes a *live* one.** When callback *a* fails, murmur logs "Removed" for the callback after it, *b*, and keeps calling the dead *a*. | 1 dead of 3 (the first): the live second callback got no further events. 1.5.939 and 1.6.1001 removed the right one. |
| F3 | **Already fixed upstream; only 1.6.870 (RC1) is affected.** The cause: each event handler looped over a *reference* to the live callback list and removed a failed callback from it inside the loop (`badServerProxy` → `removeAll`), so removal shifted elements under the iteration. Mumble's `a780cc9259` ("Remove failed Ice callbacks only after notifying all callbacks", #7402, merged 2026-10-04) collects failures and removes them after the loop, at every notification site. Its message names `c80ece5bb` (`foreach` over a copy replaced by range-based `for`) as where the bug came in. | Fixed in `v1.6.1001` (1.6.x RC2, tag `v1.6.0-alpha2`, 2026-10-25) and `master`: neither F1 nor F2 reproduced on RC2 (3 runs). 1.5.939 has neither the fix nor the bug in practice. The reference deployment runs 1.6.870. |
| F4 | The read secret (`icesecretread`) allows `getBootedServers`, `getUsers` and `getChannels`. **`addCallback` needs the write secret.** No secret gets `InvalidSecretException` on every call. | Both versions. |
| F5 | A callback is dialled lazily, on the first event after `addCallback`. Murmur removes callbacks by identity, so a stale registration with the same identity as a new one takes the new one with it when it fails. | 1.6.870. |
| F6 | Callbacks are fast: a self-mute toggle arrives over Ice in **0.23 ms median (1.1 ms max)**, against **0.31 ms (1.3 ms)** for a Mumble client watching the same user. | 40 toggles, 1.6.870, loopback. |
| F7 | **Polling is cheap.** `getUsers` with 50 users connected takes **1.0 ms median, 4.2 ms max** on 1.6.870 and **0.73 ms / 4.4 ms** on 1.5.939. | 200 calls each. |
| F8 | **1.6 numbers its default virtual server `0`, 1.5 numbers it `1`.** `Meta.getBootedServers` gives the right identity on both (`s/0`, `s/1`). | Both versions. |
| F9 | Exception base types differ by version: 1.6's `InvalidSecretException` derives from `::MumbleServer::ServerException`, 1.5's from `::MumbleServer::MurmurException`. Match on the most-derived type ID only. | Both versions. |
| F10 | **Option 3 works.** Two token-group users who both create the same temporary channel: the loser gets `Permission denied: ChannelName`, finds the channel by name and joins it, and they end up in one channel. The channel survives its creator leaving while others stay. Murmur removes it when the last user leaves. Ice sees it with `temporary = true` throughout. | Both versions, with the docs/80 §5 ACL on a fresh parent. The bridge's `enterTemp` already handles the race this way (`internal/bridge/mumbleconn.go`). |
| F11 | `getUsers` returns every user's IP address, comment, identity and plugin context, as well as the fields a roster needs. | Both versions (and the reference ini's own comment). |

**What it settles:** callbacks are out. On 1.6.870, which the reference
deployment and anyone else on RC1 still run, a bridge pod killed without
`removeCallback` (SIGKILL, OOM, a lost node) would crash the operator's
whole Mumble server (F1). Even where F3's fix is in, callbacks would need
the write secret (F4) and an endpoint in every pod that murmur can dial.
Polling with the read secret is the design (D2): it is cheap (F7), needs the lesser secret (F4), and
mumisija's serveradmin already polls the same server every 3 s
(mumisija `docs/serveradmin-control-plane.md`, "Presence").

**To reproduce:** start murmur with
`MUMBLE_CONFIG_ICE='tcp -h 127.0.0.1 -p 6599'`,
`MUMBLE_CONFIG_ICESECRETWRITE` and `MUMBLE_CONFIG_ICESECRETREAD`, then
`addCallback` three proxies to three TCP listeners, close the third, and
connect a Mumble user. 1.6.870 crashes; 1.6.1001 doesn't. MI1 criterion 5
keeps the bridge from ever registering a callback.

---

## 4. Options considered

| Option | What it does | Taken? |
|---|---|---|
| 1. Park the observer in its own channel | The bot leaves the talking channels but still shows up in the tree. Doesn't keep temporary channels. | No: still visible. |
| 2. One observer per bridge pod | One bot per pod instead of one per room (docs/80 §10). Still visible, and temporary channels need another keeper. | No. |
| 3. The first gawk user creates the temporary channel | Removes the keeper role. A native user can't wait in a dynamic room's channel before a gawk user is in voice. | **Yes (OD2).** |
| 4. Roster from the voice users' own connections | No extra connection while someone from gawk is in voice. Still needs something for the gap before anyone joins (OD28). | No. A degraded mode for Ice outages was considered and not taken (OD6). |
| 5. Murmur's Ice | No Mumble user at all. Needs Ice reachable and a secret, and a first-party Ice client. | **Yes (OD1).** |
| 5a. …with callbacks | Push, about as fast as a Mumble client (F6). | **No: crashes murmur 1.6.870 (F1, F2; fixed in 1.6.1001, F3)**, and needs the write secret (F4) plus an endpoint murmur can reach in each pod. |
| 5b. …by polling | One `getUsers` and one `getChannels` per pod per tick. Roster lag up to one interval. | **Yes (D2).** |
| Import `mumisija/ice` | A tested, dependency-free client. | No: the owner asked for gawk's own minimum (D7). The licence would allow it. |

---

## 5. Design

| # | Decision | Why |
|---|---|---|
| D1 | **Two roster modes, chosen by configuration.** `-mumble-ice-addr` set: **Ice mode**. Unset: **observer mode**, R79's behaviour byte for byte. The bridge never switches mode at runtime. | OD1, OD4. One mode per process keeps the channel rules (D5) stable for the life of a call. |
| D2 | **Ice mode polls.** One poller per bridge pod calls `getUsers` and `getChannels` every `-mumble-ice-poll` (default **250 ms**), and only while the pod holds at least one observer link. Every observer on the pod reads the same snapshot. | F1–F2 rule out callbacks. F7: `getUsers` costs about 1 ms per tick for 50 users. `getChannels` wasn't timed, but it returns a much smaller list. A roster change shows in gawk within one interval plus the link hop. Today's observer takes about 0.3 ms plus the 50 ms coalesce, so the cost is up to 250 ms on joins, leaves and mute changes, never on speaking (§1). |
| D3 | **The observer link is unchanged.** The bridge pod that accepts a room's link runs an *Ice watcher* in place of a Mumble connection. It resolves the room's channel (D4) in each snapshot and sends a `Roster` when the channel's users differ from the last one sent. Message shape, `Available`/`Reason`, the home pod's merge with session links, redial and drain handover all stay as docs/80 D2–D4 and §11.5 define them. | No relay, wire or `voice` package change. The relay's `/statusz` `voiceObserver` keeps its meaning. |
| D4 | **Channel resolution from Ice's channel list.** A static room's `/`-separated path is walked from the root (ID 0) by name, with the same rules as `voice.ChannelPath` and `mumble.Client.FindPath`. A dynamic room's channel is the child of `-mumble-temp-parent` named by `tempChannel()` (docs/80 D11, unchanged). The IDs are re-resolved on every snapshot, because channels come and go. | One naming rule for both modes, so the key-based temporary channel name stays fleet-stable. |
| D5 | **Temporary channels in Ice mode: the first voice user creates it.** A voice user's pod calls `enterTemp` with no wait (it never waits for an observer), creating the channel or joining it if it exists, with the existing `ChannelName` race handling (F10). Until then the room's roster is `Available` and empty. Once everyone has left, murmur removes the channel as before. In observer mode nothing changes. | OD2. The cost: a native Mumble user can't wait in a dynamic room's channel before a gawk user joins voice. That narrows docs/80 OD28 for dynamic rooms only. Static rooms keep their roster before anyone joins. |
| D6 | **`-max-temp-channels` in Ice mode** counts the temporary channels under `-mumble-temp-parent` in the **voice user's own Mumble connection's channel tree**, just before `enterTemp` would create one, and refuses the *create*, never a join, with `temp_channel_cap` once the count reaches the cap. That connection has the server's whole tree as soon as it's logged in, on whichever pod it runs, so the cap doesn't depend on the Ice poller: a voice session can land on a pod with no observer link and no snapshot (docs/80 OD22). The count covers the whole server, unlike observer mode's count of a pod's own observers. Two pods can race past the cap by one each. The cap is an abuse guard, not an exact quota. | docs/80 OD19 and D23 keep a cap. Without a keeper there's nothing per pod to count. |
| D7 | **A minimal first-party Ice client, `internal/murmurice`.** Ice protocol 1.0 over TCP, encoding 1.1, twoway requests multiplexed on one connection, validate and close messages, one reconnect-and-retry for idempotent calls, and a deadline on every call. Operations: `Meta.getBootedServers`, `Server.getUsers` and `Server.getChannels`, nothing else. No object adapter, callbacks, classes, oneway or batch requests, compression, TLS endpoints or locators. Modelled on `mumisija/ice` (`protocol.go`, `conn.go`, `encoding.go`). Each struct decoder must use up exactly its bytes, and a `getUsers` reply must decode to its end, so a murmur whose `User` layout changes fails loudly with `ice_protocol` instead of producing a wrong roster. Murmur 1.5 or later (the `User.version2` field). | Owner: only the minimum, in gawk. The spike was about 440 lines for this scope plus a callback endpoint, which isn't needed. `CGO_ENABLED=0` and the Go licence allowlist are unaffected. |
| D8 | **Which virtual server.** `-mumble-ice-server-id` if set. Otherwise `getBootedServers`: exactly one booted server is used. More than one is a configuration error (`ice_server_ambiguous`). | F8: 1.6 and 1.5 number the default server differently, so a hard-coded `1` breaks 1.6. |
| D9 | **The secret.** `-mumble-ice-secret` (`GAWK_BRIDGE_MUMBLE_ICE_SECRET`, chart `mumble.ice.secretRef`), sent as the `secret` request context on every call. Operators are asked for murmur's **read** secret. The write secret works too (OD3). It is logged only as set or unset, like every bridge secret. Ice sends it in clear text, so the Ice port must stay on the private network. The chart's `networkPolicy` egress gains the Ice port. | F4: polling needs only read. The least privilege that works. |
| D10 | **The bridge keeps nothing from Ice it doesn't need.** `getUsers` carries every user's IP address, comment, identity and plugin context (F11). The decoder skips those bytes without storing them: `murmurice.User` has only session, user ID, name, channel and the mute, deafen and suppress flags. A test walks the package's types and fails if a field named `Address`, `IP`, `Comment`, `Identity` or `Context` appears, or if any code logs an Ice payload. This joins docs/80 D24's prohibitions. | The CLAUDE.md rule that no event ever carries a publisher IP, applied in spirit to native users' IPs: the bridge never had them before and shouldn't start keeping them now. |
| D11 | **Ice failing in Ice mode** (unreachable, wrong secret, protocol error, ambiguous server): every observer on the pod reports `Available: false` with the reason (`ice_unavailable`, `ice_secret`, `ice_protocol`, `ice_server_ambiguous`, all counted in `gawk_bridge_mumble_failures_total{reason}`). Voice sessions keep running, because they are their own Mumble connections. The poller retries with the existing redial backoff. **No visible observer appears** (OD6). | Ice can't take voice down with it. A bot appearing only when Ice fails would confuse the Mumble side more than a short gap in the gawk roster. |
| D12 | **Status and metrics.** The bridge's `/statusz` gains `roster: "ice" \| "observer"` and, in Ice mode, `ice: {available, reason, serverId, lastPollAgeMs}`. New metrics: `gawk_bridge_ice_polls_total{result}` and the histogram `gawk_bridge_ice_poll_seconds`. `gawk_bridge_observers` keeps counting observer links in both modes. | docs/self-hosting.md §15.4 "Is it working?" needs an answer in both modes. |
| D13 | **Names.** In Ice mode no Mumble user named `-mumble-observer-name` ever exists. The flag applies only in observer mode, and the observer name ladder (docs/80 §11.5, 1000 names) only there. | OD33 becomes observer mode's rule. |

### 5.1 Knobs (bridge only; flags, `GAWK_BRIDGE_*` envs and the chart)

| Flag | Default | Meaning |
|---|---|---|
| `-mumble-ice-addr` | — | murmur's Ice endpoint, `host:port` over TCP. Set: Ice mode (D1). |
| `-mumble-ice-secret` | — | murmur's `icesecretread` (or `icesecretwrite`). Prefer the env or `secretRef`. |
| `-mumble-ice-server-id` | discovered | the virtual server's ID (D8) |
| `-mumble-ice-poll` | `250ms` | poll interval (D2). Bounded to 50 ms–5 s. |
| `-mumble-ice-timeout` | `2s` | the deadline on each Ice call, and the dial timeout |

These go in the bridge's options struct under its carry-all test, the
same rule as the relay's `registryOptions` (CLAUDE.md, docs/80 D23).
Setting `-mumble-ice-secret` or the server ID without `-mumble-ice-addr`
refuses to start.

---

## 6. What users see

- **In Mumble (Ice mode):** no `gawkBridgeBot` anywhere. A gawk voice user
  appears as `Alice [web]`, as today. A dynamic room's channel appears
  when its first gawk user joins voice and goes once everyone has left.
- **In gawk:** the same voice roster as before. A native user joining,
  leaving or muting shows within a poll interval (about 250 ms) instead of
  at once. Speaking indicators are exactly as fast as before. A dynamic
  room shows no Mumble users until someone from gawk is in voice, because
  until then there's no channel to be in.
- **For an operator:** one more endpoint and secret to configure. If they
  don't configure it, today's bot stays.

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
| G7 | No murmur crash from gawk: the bridge never calls `addCallback` | MI1 criterion 5 |

### MI0 — Spike (done 2026-11-15)

| # | Criterion | Result |
|---|---|---|
| 1 | Callbacks vs polling decided on real murmur 1.5 and 1.6 | Polling (§3 F1–F7) |
| 2 | Which secret polling needs | Read (F4) |
| 3 | Option 3 works on real murmur, race included | Yes (F10) |

### MI1 — `internal/murmurice`

| # | Criterion |
|---|---|
| 1 | The D7 scope: dial, validate, multiplexed twoway calls, close, reconnect with one retry for idempotent calls, a deadline on every call |
| 2 | Unit tests against an in-process fake Ice server (mumisija's `icetest` shape): framing, sizes ≥ 255, strings, dictionaries, user exceptions matched on the most-derived type ID for both 1.5's and 1.6's hierarchy (F9), a reply that ends early or runs long (`ice_protocol`), reconnect after the server closes, a call outliving its deadline |
| 3 | The D10 type-walk test, and a test that no Ice payload or secret reaches a log line |
| 4 | Integration tests against real murmur 1.5 and 1.6 (the existing CI matrix). `testdata/murmur.sh` gains 1.6.1001 (RC2) alongside 1.5.939 and 1.6.870, and enables Ice on its `main` server with both secrets and exports `GAWK_TEST_MURMUR_ICE` and `GAWK_TEST_MURMUR_ICE_SECRET`. The tests cover server discovery (`0` on 1.6, `1` on 1.5), `getUsers` and `getChannels` decoding with 50 users (a name, mute, deafen and channel each checked), the wrong secret, and no secret |
| 5 | A test that the package exposes no way to register a callback: `addCallback` isn't an operation it can send (G7) |

### MI2 — The bridge's Ice mode

| # | Criterion |
|---|---|
| 1 | D1–D6 and D11–D13 implemented. Observer mode's code path is untouched apart from the mode switch. |
| 2 | **Both modes in the bridge's unit tests.** The fake-murmur harness (`internal/mumble/mumbletest`) gains a fake Ice endpoint fed from the same fake server state, and the observer and temporary-channel tests are table-driven over `{observer, ice}` |
| 3 | **Both modes in the real-murmur integration tests** (`internal/bridge/murmur_integration_test.go`), run as {1.5, 1.6} × {observer, ice}: the roster before voice for a static room; a native user's join, leave, self-mute and server-mute reaching the roster within one poll interval plus 1 s; a dynamic room's channel created by the first voice user and joined by a second user who raced it; the creator leaving while others stay; removal once it's empty; the D6 cap refusing a create but not a join, including for a voice user on a pod that holds no observer link; in Ice mode, a check that no user named the observer name ever appears |
| 4 | An Ice outage (a TCP proxy in front of Ice, cut and restored): observers go `ice_unavailable`, an established voice session keeps sending and receiving, the roster comes back after recovery, and no Mumble user is created during the outage |
| 5 | A drain hands a room's watcher to the surviving pod in Ice mode, as docs/80 §11.5 does for the observer |
| 6 | The options carry-all test covers every new knob |

### MI3 — Config, chart, docs

| # | Criterion |
|---|---|
| 1 | §5.1's flags, `GAWK_BRIDGE_*` envs and chart values (`mumble.ice.addr`, `mumble.ice.secretRef`, `mumble.ice.serverId`, `mumble.ice.poll`, `mumble.ice.timeout`); the chart's NetworkPolicy egress allows the Ice port when `mumble.ice.addr` is set; `helm template` tests for both modes |
| 2 | `docs/self-hosting.md` §15: the Ice prerequisite (`ice=`, `icesecretread`, keep it private) as the recommended setup, observer mode as the fallback, and §15.4 in both modes |
| 3 | The bridge README's knob table; `docs/gotchas.md` entries for F1–F3, F4, F8 and F9 |
| 4 | The dev voice stack (`dev/compose.voice.yml`) runs Ice mode by default, with observer mode one variable away |

### MI4 — The voice e2e in both modes

| # | Criterion |
|---|---|
| 1 | `e2e/voice.mjs` takes `VOICE_ROSTER=ice\|observer`. The `e2e-voice` CI job gains a matrix axis `roster: [ice, observer]`, so both run on every PR that runs it today. The murmur that `testdata/murmur.sh` starts has Ice on in both runs; observer mode simply doesn't configure it. |
| 2 | Passes 1–6 run in both modes. Their assertions are unchanged except where §6 says the behaviour differs. |
| 3 | Pass 1 in Ice mode also checks, through the SuperUser's user list, that no user named the observer name exists (G1) |
| 4 | Pass 5 in Ice mode: before anyone joins voice the channel doesn't exist and the roster is empty; alice's join creates it; the native user joins it by its fleet-stable name and appears in the roster; the SuperUser's user list has no user named the observer name, before or after alice's join (G1); the channel is gone once everyone has left |
| 5 | A new pass 7 (Ice mode only): the Ice proxy is cut mid-call, alice and the native user still hear each other, the relay reports the observer `unavailable`, the proxy is restored, and the roster is back with no bot ever seen |

### MI5 — Owner pass on the reference deployment

| # | Criterion |
|---|---|
| 1 | The bridge's values in `Tuhis/ioio` point at `mumble-ice` with the read secret. Every bridge pod's `/statusz` says `roster: "ice"` and `available`. |
| 2 | No `gawkBridgeBot` on the server. The friends' static room shows its Mumble users in gawk before anyone joins voice. |
| 3 | A `?voice=1` room: its channel appears with the first gawk voice user, a desktop Mumble user joins and talks both ways, and the channel goes once everyone has left |
| 4 | mumisija's serveradmin keeps working beside the bridge on the same Ice endpoint |

---

## 8. Risks

- **Murmur changes its Slice.** D7's exact-length decoding turns a layout
  change into a loud `ice_protocol`, and the integration matrix runs
  every supported version. A new murmur version joins the matrix before
  the reference deployment upgrades.
- **Big servers.** `getUsers` is answered from memory and costs about
  1 ms for 50 users (F7). A server with hundreds of users makes each poll
  a few milliseconds and some tens of KB per pod. `-mumble-ice-poll`
  trades lag for load, and the poller only runs while the pod holds
  observer links.
- **Hosted murmur without Ice.** Many hosted Mumble services don't expose
  Ice. Observer mode is their path (OD4).
- **The secret in clear text.** Ice has no transport security as murmur
  is usually configured. D9 keeps it on the private network, and the
  read secret limits the damage. It still reads every user's IP, which
  is why D10 exists.
- **A slower roster.** Up to one poll interval on join, leave and mute
  changes (D2). Speaking is unaffected (§1).

---

## 9. Out of scope

- Ice callbacks (§3 F1). Revisit only once every supported murmur has
  F3's upstream fix (1.6.1001 and later) and 1.6.870 is out of the
  support matrix, and only if polling's lag (D2) turns out to matter.
- Using Ice for anything but reading: moderation, channel creation, ACLs,
  registration. The voice users' own connections keep doing what they do
  today.
- Ice over TLS (`ssl` endpoints), more than one virtual server per
  bridge, and Ice through a locator or IceGrid.
- Any change on the relay, the wire, the `voice` package or the web app.
