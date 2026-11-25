# R88 — Real client addresses for bridged Mumble users (docs/92)

**Status**: designed 2026-11-26 from the PX0 spike against real murmur
1.6.1001 (§3). The owner made decisions OD1–OD9 on 2026-11-25 and
2026-11-26 (§2). No question is open. Not started. `PX` = proxy.

This adds an **eighth top-level Go module and deployable, `gawk-mmproxy`**,
which runs as a container in the operator's murmur pod. It also changes
`gawk-mumble-bridge`: one knob, the classification of one failure, the
name registry's view of the server in Ice mode (D12), its chart and its
tests. The voice e2e, the dev voice stack and the docs
change with it. The relay, the wire, the public `gawk-server/voice`
package and the web app don't change.

---

## 1. Why

Every bridged web user (R79) is a Mumble connection from a bridge pod, so
murmur sees each one at that pod's address (docs/80 §11.5). Three things
follow:

- **A Mumble ban of one web user bans every web user on that pod.**
  Murmur bans by address and by certificate hash. The certificate is fresh
  and never stored (docs/80 §11.5), so only the address part of the ban
  sticks, and it is the pod's.
- **Autoban counts a whole pod as one client.** Murmur bans an address
  after `autobanAttempts` connections in `autobanTimeframe`, so the
  operator has to raise it well above a pod's join rate. The reference
  deployment runs `autobanAttempts = 200`, and its ini comment explains
  why. That also weakens the throttle on password guessing from any one
  address.
- **Mumble admins can't tell web users apart by address**, as they can
  native users.

The bridge already has the browser's real address. Its public Service
defaults to `externalTrafficPolicy: Local`, and the bridge passes the
address to the relay for gawk's own IP bans (docs/80 D17,
`voice.TypeRegister`). Only murmur never sees it.

Murmur takes a client's address from the TCP socket and nothing else. It
has no PROXY protocol support: mumble-voip/mumble#4769 (2021) is still
open, with no pull request, and the latest release is 1.5.939. So the
address has to be put on the socket itself. The bridge's connections
make that possible: they are TCP only, and audio rides the TCP tunnel
(`CryptSetup` is ignored, docs/80 §11.5), so there is no UDP address to
carry.

The technique is Cloudflare's mmproxy pattern. A small proxy in murmur's
network namespace takes a PROXY protocol header from the bridge, then
dials murmur over loopback **with the browser's address as its own
source** (`IP_TRANSPARENT`). A policy route sends murmur's replies on
loopback back to the proxy instead of out to the internet.

---

## 2. Owner decisions (2026-11-25 and 2026-11-26)

| # | Question | Decision |
|---|---|---|
| OD1 | Sidecar, murmur patch or both | **The sidecar only.** No PROXY protocol patch to murmur, upstream or carried: murmur is the operator's own (docs/80 OD9). |
| OD2 | First-party or go-mmproxy | **First-party Go.** go-mmproxy (BSD-3-Clause, on the allowlist) works but was last pushed in 2024-05 and has no official image. Ours fails closed in the ways §5 needs, and has ops endpoints and the least privilege of D5. |
| OD3 | How the proxy knows a header comes from a bridge pod | **A Kubernetes NetworkPolicy.** Only bridge pods may reach the proxy's port, and the proxy itself trusts any peer that reaches it. No shared secret, no CIDR list (§4). |
| OD4 | The proxy configured but unreachable or refusing | **Fail closed with 4016.** The voice join fails as "voice unavailable" with its reason (docs/80 D18). The bridge never falls back to dialling murmur directly from the pod's address. |
| OD5 | Packaging | **Its own module and image**: an eighth top-level Go module, released on its own. |
| OD6 | Mumble admins seeing web users' addresses | **Acceptable, the same as native users.** The self-hosting guide and the bridge README say so plainly. No in-app notice. |
| OD7 | The reference deployment | **Out of R88.** R88 ends at the code, the docs and CI. Rolling it out on the reference deployment, and any change to its `autobanAttempts`, is a separate task. |
| OD8 | A Helm chart | **None, as a deliberate exception** to "one chart per component" (CLAUDE.md). The module only ever runs inside someone else's murmur pod, and gawk ships no murmur (docs/80 OD9). `deploy/` holds the Dockerfile and tested snippets instead (D11). |
| OD9 | Name | **`gawk-mmproxy`**: image `ghcr.io/tuhis/gawk-mmproxy`, tags `gawk-mmproxy/vX.Y.Z`. |

---

## 3. What the spike found (PX0, 2026-11-26)

Topology, as a Kubernetes pod lays it out: a pause container owns the
network namespace, and murmur 1.6.1001 (`host=` blank, the default) and a
prototype proxy (about 150 lines of Go) join it. The driver used the
bridge's own Mumble client (`internal/mumble`), with a `DialContext` that
writes a PROXY v2 header, and dialled from the host. A "native" client
was the same client dialling murmur directly from the host's address
on the docker network (`172.29.88.1`). The prototype is in the PR that
lands this doc's first chunk. Nothing of it is committed before then.

| # | Finding |
|---|---|
| F1 | **Murmur logs and acts on the spoofed address.** A user proxied as `203.0.113.7` appears as `New connection: 203.0.113.7:<port>`. The port is the proxy's ephemeral one, not the browser's. |
| F2 | **A native client and a proxied user from the same address work side by side.** The native client connected from `172.29.88.1`, and a proxied user spoofed `172.29.88.1` at the same time. Each saw the other, and 200 of 200 audio frames went each way. Replies separate by the local address the connection arrived on (pod address versus loopback), not by protocol or by client address. |
| F3 | **Native UDP is unaffected.** A Mumble UDP ping to murmur's port was answered with the policy routes in place. |
| F4 | **Bans are per real address.** A SuperUser kickban of a user proxied as `198.51.100.9` disconnected that user only: a user proxied as `198.51.100.10` through the same proxy stayed connected. A reconnect from `198.51.100.9` with a fresh certificate was refused (`Ignoring connection: 198.51.100.9 … (Server ban)`), and a new address connected. |
| F5 | **Autoban is per real address.** With `autobanAttempts = 8`, the 9th connection in a minute from `192.0.2.50` was refused, and `192.0.2.51` connected straight after. |
| F6 | **IPv6 works.** A user proxied as `2001:db8::7` was dialled from `[::1]` and murmur logged `[2001:db8::7]`. It needs `::1` on the namespace's loopback and the IPv6 pair of rules. |
| F7 | **A murmur restart needs nothing from the proxy.** The routes live in the namespace, not in either process, so after `docker restart` of murmur alone the next proxied user was spoofed as before. |
| F8 | **Murmur rejects a banned address with a TCP reset.** A direct connection from an autobanned address got `read: connection reset by peer` during the TLS handshake. |
| F9 | **Through a proxy, "murmur is down" looked exactly like "you are banned".** With murmur stopped, the proxy's dial was refused and it closed the bridge's side. Today's bridge reports any close before the TLS handshake as `autoban` (`mumble.ErrRefused`). Fixed by D6: the proxy passes murmur's reset through as a reset (`SO_LINGER 0`) and closes with a FIN when it can't reach murmur. The bridge then saw `connection reset by peer` for the ban and `EOF` for murmur down. That EOF was clean only because the prototype's buffered read had already consumed the ClientHello; D6 makes it deliberate. |
| F10 | **Latency cost: about 0.06 ms.** One sender, 2000 frames at 10 ms, a direct and a proxied listener receiving the same frames. Proxied minus direct arrival: p50 91–93 µs, p99 345–390 µs. Two direct listeners, as the baseline: p50 35–41 µs, p99 209–243 µs. That is noise next to the 22–26 ms browser pipeline (docs/80 §12.1). |
| F11 | **`IP_TRANSPARENT` needs only `CAP_NET_RAW`.** The proxy spoofed v4 and v6 with every capability dropped but `NET_RAW` (`CapEff 0x2000`), once the routes were in place. Only installing the routes needs `CAP_NET_ADMIN`. |
| F12 | **`ip rule add` is not idempotent.** Installing the routes a second time added a second identical rule. |
| F13 | **The proxy can see murmur's listening socket without connecting.** `/proc/net/tcp` and `/proc/net/tcp6` are per-namespace, and murmur's `:::64738` shows in state `0A` (LISTEN). Connecting to murmur instead would log a `New connection` each time and count towards autoban. |

What the spike didn't cover: Kubernetes itself (a NetworkPolicy, a
ClusterIP Service in front of the proxy, an init container), murmur
1.5.939, and the bridge's own code path. PX3 covers Kubernetes, PX1 murmur 1.5.939, and PX2 and PX4 the bridge.

---

## 4. Options considered

| Option | What it does | Taken? |
|---|---|---|
| A. PROXY protocol v2 in murmur | Murmur parses a header from trusted proxies. The cleanest fix. | No (OD1): upstream or a carried image, and murmur is the operator's own. D2 keeps the bridge's side compatible if murmur ever ships it. |
| B. A spoofing sidecar in murmur's namespace | §1's mmproxy pattern. Works with stock murmur. | **Yes.** |
| B1. …go-mmproxy | An existing BSD-3 implementation. | No (OD2). |
| B2. …a first-party proxy | Fails closed in the ways §5 needs, with D5's least privilege and an ops listener. | **Yes (OD2).** |
| Spoof from the bridge pod itself | No sidecar. | Impossible: murmur's replies would go to the real client, not back to the bridge. |
| Trust by shared secret in a PROXY v2 TLV, or by source CIDR | The proxy checks who sent the header. | No (OD3). CIDR alone would mean trusting the whole pod CIDR, because bridge pods move. NetworkPolicy is the one place that knows which pods are bridges. |
| Fall back to a direct dial | Voice stays up when the proxy is down, at the pod's address. | No (OD4): the ban property would silently come and go. |
| A custom acknowledgement byte from the proxy | Unambiguous error signalling. | No: it breaks plain PROXY v2, and D6's reset-or-FIN rule is enough (F8, F9). |

---

## 5. Design

| # | Decision | Why |
|---|---|---|
| D1 | **`gawk-mmproxy` is a new top-level Go module and deployable, default off everywhere.** One static binary (`CGO_ENABLED=0`) with two subcommands, `routes` (D4) and `serve` (D5). It depends on the standard library and `golang.org/x/sys` only (BSD-3-Clause), using `x/sys/unix` for netlink and the socket options. It doesn't import `gawk-server`: it shares no gawk types, only the PROXY v2 standard (D2). Image `ghcr.io/tuhis/gawk-mmproxy`, its own release-please component and tags, no chart (OD5, OD8, OD9). | The module boundary keeps murmur's pod free of gawk's relay code and gives the operator one small image to pin. |
| D2 | **The header is standard PROXY protocol v2**: the `PROXY` command, `TCP over IPv4` or `TCP over IPv6`. The source is the browser's address and port, the destination the unspecified address of the same family with port 0, and there are no TLVs. The proxy also accepts and skips TLVs it receives. It refuses v1, `LOCAL`, `UNSPEC`, UDP and UNIX families, a header that doesn't arrive in full within `-header-timeout` (default 2 s), and a source that is unspecified, multicast or broadcast. Each side implements its own half: the bridge an encoder, the proxy a decoder. Both are tested against the spec's byte layouts and against each other (PX2 criterion 5). | A standard header means the bridge's side works unchanged against any PROXY v2 listener, including murmur itself if #4769 ever ships. It isn't a gawk wire type, so the "never mirror `wire`" rule doesn't apply. |
| D3 | **The spoofed dial.** For each accepted header the proxy dials `127.0.0.1:<murmur-port>`, or `[::1]:<murmur-port>` for an IPv6 source, with `IP_TRANSPARENT` (`IPV6_TRANSPARENT`) set and the local address bound to the header's source address (unmapped) and port 0. It then copies both ways: first any bytes it read past the header, then a plain `io.Copy` each way, which splices on Linux. `-murmur-port` defaults to 64738. Murmur must listen on loopback, which it does with `host=` blank (its default, and the reference deployment's). | F1, F2, F6. The source port is the proxy's ephemeral one: murmur only acts on the address. Loopback is what makes replies return to the proxy. Spoofing towards the pod address would send them out to the internet. |
| D4 | **The routes, installed by `gawk-mmproxy routes`, as an init container with `CAP_NET_ADMIN`.** It **always exits 0**: when it can't install them (no capability, a policy refusing it) it logs why and lets the pod start, so a broken proxy setup never keeps murmur down; `serve` then fails closed (D5). Over netlink it installs `from 127.0.0.0/8 iif lo lookup <table>` and `local 0.0.0.0/0 dev lo table <table>`. When the namespace has `::1` it also installs `from ::1/128 iif lo lookup <table>` and `local ::/0 dev lo table <table>`; without it, it logs that IPv6 sources will fail. `-route-table` defaults to 64738. **It checks before adding** and exits 0 when the routes are already there. It never removes them: they live and die with the pod's namespace. | F12: re-running `ip rule add` duplicates rules. F7: routes outlive container restarts, so a one-shot init container is enough. Only the init container needs `NET_ADMIN` (F11). |
| D5 | **`gawk-mmproxy serve` runs with `CAP_NET_RAW` only.** Every other capability is dropped, the root filesystem is read-only, and it runs as uid 0. *(Kubernetes adds capabilities only to root processes, and a non-root uid with file capabilities is PX1's measurement to make. If it works, the snippet switches to it and this line records the change.)* At start it reads the rules and routes over netlink, which needs no capability. **Without them it doesn't exit**: it keeps serving, closes every connection with a FIN (D6), and reports `gawk_mmproxy_routes_ok 0` and not-ready on `/readyz`, until the routes appear. `-require-routes=false` skips the check; it is only for the e2e, where every source is a loopback address (PX4). `serve` never exits on a runtime condition (D15). Listeners: `-listen` (default `:64739`) and `-ops-listen` (default `:2114`), with `/healthz`, `/readyz` and `/metrics`. `-max-conns` (default 1024) caps concurrent connections, and a connection over the cap is closed with a FIN (D6). | F11: the long-running process holds the smallest privilege that works. Failing closed without the routes turns a forgotten init container into a visible, unambiguous refusal, not silently misrouted replies. Not exiting keeps murmur's pod Ready (D15). |
| D6 | **Error signalling, without a custom byte.** When murmur resets the proxy's loopback connection, the proxy resets the bridge's connection (`SO_LINGER 0`, then close). When the proxy can't reach murmur, refuses a header, is at `-max-conns` or has no routes, it closes the bridge's connection with a FIN before sending any byte. **A plain `close(2)` is not a FIN here**: Linux resets a socket whose receive queue still holds unread data, and the bridge's ClientHello usually follows the header at once. So every FIN path is `CloseWrite()` (SHUT_WR), then reads drained until EOF or a 1 s deadline, then close. Otherwise each half-close is passed on as a half-close. **In proxy mode the bridge classifies an EOF before the TLS handshake completes as `murmur_unreachable`** (a `DialError`, 4016), and a reset as `mumble.ErrRefused` (`autoban`, 4016), as today. Direct mode is unchanged. | F8, F9. Murmur bans with a reset, so a reset is "your address is refused" and a FIN is "nothing is there". Both end the join with 4016 and the reason in `/statusz` (D18), but an operator reading `autoban` while murmur is down would look in the wrong place. The reason names are the bridge's existing labels (`ReasonUnreachable = "murmur_unreachable"`, `ReasonAutoban = "autoban"`). |
| D7 | **Readiness without touching murmur.** `/readyz` is ready when the routes are in place and murmur has a LISTEN socket on `-murmur-port` **whose local address D3 can dial, checked per family**. IPv4 is reachable on `0.0.0.0` or `127.0.0.0/8` in `/proc/net/tcp`, or on a dual-stack `::` in `/proc/net/tcp6` (murmur's `:::64738` is dual-stack: the spike's IPv4 dials to `127.0.0.1` reached it, F1, F13). IPv6 is reachable on `::` or `::1`. Ready needs IPv4, plus IPv6 when D4 installed the IPv6 routes. A socket bound only to the pod's address, or only to `::1`, leaves IPv4 not ready. `/healthz` reports that the process is serving. Neither is a Kubernetes probe of the pod (D15). | F13: a probe that connected to murmur would log a connection every period and count towards autoban for `127.0.0.1`. A port-only check would call a murmur with `host=<pod IP>` ready while every loopback dial is refused. |
| D8 | **The bridge's knob: `-mumble-proxy-addr host:port`** (`GAWK_BRIDGE_MUMBLE_PROXY_ADDR`, chart `mumble.proxy.addr`), default unset. When set, **every voice user's Mumble connection** dials it and writes D2's header before TLS. The source is the voice session's remote address (`voiceSession.remote`, the address it already sends in `Register`), canonicalised with `moderation.CanonicalAddr`: zone stripped, v4-mapped unmapped. If that address can't be parsed, the join fails with 4016 and reason `proxy_source`; it never falls back to a direct dial. TLS is unchanged end to end: the verification name and SNI stay the host of `-mumble-addr`, and `-mumble-cert-sha256` and `-mumble-ca-file` apply as before. The observer connection (observer mode, docs/89 D1) and Ice keep dialling murmur directly: they aren't people, and murmur sees them at the pod's address as today. The option goes in the bridge's options struct under its carry-all test (CLAUDE.md's `registryOptions` rule, docs/80 D23). The chart's NetworkPolicy egress gains the proxy's port when it is set. | OD4. The certificate a proxy-mode bridge verifies is still murmur's, so a misdirected proxy can't impersonate it. Keeping the observer direct means a ban of the bot's address never covers a person. |
| D9 | **Trust is a NetworkPolicy** (OD3). The proxy accepts a header from any peer that reaches its port. The operator's murmur pod needs an ingress policy that admits the proxy's port **only from the bridge's pods** (a `podSelector` and `namespaceSelector` for `app.kubernetes.io/name: gawk-mumble-bridge`), and keeps murmur's public port open as before. The proxy's port is exposed only on a **ClusterIP** Service, never a LoadBalancer or NodePort. `deploy/` ships the policy (D11), and the self-hosting guide states the prerequisite: a CNI that enforces NetworkPolicy. | Whoever can reach the port can make murmur see any address. They could dodge a Mumble IP ban, or worse, connect as someone else's address until murmur bans *them*. The policy is the one place that knows which pods are bridges. The reference cluster's Calico enforces it, and `gawk-whip-gateway` already ships one in the same namespace. |
| D10 | **What murmur now knows, and what gawk still never keeps** (OD6). Murmur sees each web user's real address in its log, in a ban, in an admin's user information and in Ice's `getUsers`, exactly as for a native user. The bridge still never logs the address (`session.go`), and still discards the address from Ice (docs/89 D10). **`gawk-mmproxy` never logs an address and never uses one as a metric label**, and a source-walk test enforces it, as docs/80 D24 does for voice audio. | The address goes to the operator's own Mumble server on purpose, and nowhere else. |
| D11 | **`deploy/` without a chart** (OD8): the Dockerfile; `deploy/k8s/murmur-pod-snippet.yaml` (the init container and the sidecar container with its capabilities and ports, **no readiness or liveness probe**, D15); `deploy/k8s/networkpolicy.yaml`; `deploy/k8s/service.yaml` (the ClusterIP Service); and `deploy/compose/murmur.yml` (`network_mode: "service:murmur"`, `cap_add`). CI applies the Kubernetes snippets with a stock murmur in kind and runs a proxied connection through them (PX3). The snippets carry the image tag, and release-please bumps them as extra files, so "the deploy files name the image they were tested with" stands in for "chart version == image tag". | A snippet that is never applied drifts: the R2 review's lesson, applied to deploy files. |
| D12 | **Names and ghosts.** Murmur ghosts a second login with the same name *from the same address*. That used to happen only within one pod. Now two pods can ghost each other for one person (or one NAT) with one nickname in voice twice. Today's registry (`names.go` `claim`) only sees names this pod holds and the user lists of this pod's own live connections, so a pod with none, such as the first voice user on it, dials blind. **In Ice mode, `claim` also consults the pod's Ice mirror** (docs/89 D2), which every Ice-mode pod keeps, so the server's whole user list is seen and only a race between two simultaneous joins is left. In observer mode a pod with no live connection stays blind: accepted, because it can only hit one nickname from one address. Either way the ghosted session ends with 4016 `replaced`, and its Try again joins under a suffixed name. | The ghost rule now keys on the person's address, not the pod's. The Ice mirror is the one fleet-wide user list a pod already has. |
| D13 | **Autoban advice changes, the reference values don't** (OD7). Self-hosting §15 says that with the proxy, autoban and bans count per real address, so the "far above a pod's join rate" advice no longer applies. Instead, size autoban for one person's reconnects. R88 doesn't measure that number. | The 200 in the reference ini exists only because of the shared address. Lowering it there is OD7's separate task. |
| D14 | **Outside Kubernetes.** Docker Compose: the proxy joins murmur's namespace with `network_mode: "service:murmur"`, the routes run as a one-shot service with `NET_ADMIN`, and the proxy gets `NET_RAW`. On a bare host the routes apply to the **whole host's loopback**. The guide says when that breaks (§8). | Self-hosting is first-class (CLAUDE.md), and the bare-host case has one real trap. |
| D15 | **The proxy never takes murmur's pod out of service.** It shares murmur's pod, and a pod is Ready only while every container is. A readiness probe on the proxy, a liveness restart or an exit would drop the whole pod from murmur's public Service endpoints, and kube-proxy flushes UDP conntrack when an endpoint goes (CLAUDE.md, docs/22), cutting native users' voice. So the snippet gives the proxy **no readiness and no liveness probe**, `routes` always exits 0 (D4), and `serve` never exits on a runtime condition (D5). `/readyz` and `gawk_mmproxy_routes_ok` are for operators and alerts, not for the kubelet. A crash of `serve` itself (a bug) still restarts the container, briefly unreadies the pod and costs native users that blip (§8). | §6 and G3 promise native users carry on when the proxy fails, and only this makes that true. |
| D16 | **The trust boundary outside Kubernetes.** With no NetworkPolicy, the operator must make the proxy's port reachable only from the bridge: bind `-listen` to an address only the bridge can reach (a private interface, or a Compose network that only murmur and the bridge share), never publish the port, and on a bare host add a firewall rule admitting it only from the bridge's address. `deploy/compose/murmur.yml` does this with a dedicated internal network and no `ports:` entry for 64739. | D9's harm (dodging a ban, or connecting as someone else's address) is the same on any host where the port is open. |

### 5.1 Knobs

**`gawk-mmproxy`** (flags and `GAWK_MMPROXY_*` envs; no chart):

| Flag | Default | Meaning |
|---|---|---|
| `routes -route-table` | `64738` | the policy-routing table D4 installs into |
| `serve -listen` | `:64739` | where bridges send headers; ClusterIP only (D9), or an address only the bridge reaches outside Kubernetes (D16) |
| `serve -ops-listen` | `:2114` | `/healthz`, `/readyz`, `/metrics`; never routed publicly |
| `serve -murmur-port` | `64738` | murmur's port on loopback (D3) |
| `serve -route-table` | `64738` | the table whose rules `serve` checks at start (D5) |
| `serve -require-routes` | `true` | fail closed without D4's routes (D5); `false` only where every source is a loopback address (the e2e) |
| `serve -header-timeout` | `2s` | the deadline for a complete PROXY v2 header (D2) |
| `serve -max-conns` | `1024` | concurrent connections; over it, a FIN (D6) |

**`gawk-mumble-bridge`** (flag, `GAWK_BRIDGE_*` env and chart):

| Flag | Default | Meaning |
|---|---|---|
| `-mumble-proxy-addr` | — | `gawk-mmproxy`'s `host:port`. Set: voice users' Mumble connections go through it with the browser's address (D8). |

Metrics, all unlabelled by address (D10):
- `gawk_mmproxy_connections_total{result}`, where `result` is `proxied`,
  `bad_header`, `header_timeout`, `refused_source`, `murmur_unreachable`
  or `over_cap`.
- `gawk_mmproxy_resets_total`: murmur resets passed through.
- `gawk_mmproxy_active`: connections being proxied.
- `gawk_mmproxy_routes_ok`: the D5 check, as 1 or 0.

The bridge's existing `gawk_bridge_mumble_failures_total{reason}` gains
`proxy_source`.

---

## 6. What users see

- **Web users:** nothing changes until a Mumble admin acts. A Mumble ban
  or autoban then covers only the person it was meant for, together with
  any native client of theirs on the same address. Another web user on
  the same bridge pod is unaffected.
- **Mumble admins:** web users show their real addresses, like native
  users (OD6). Banning `Alice [web]` bans Alice's address.
- **Operators:** one more container and an init container in murmur's
  pod, a ClusterIP Service, a NetworkPolicy, and `mumble.proxy.addr` on
  the bridge. If the proxy is down, web voice joins fail with 4016
  `murmur_unreachable`, and existing web voice sessions end with 4016
  `connection_lost`: each user sees Voice unavailable with Try again,
  which fails until the proxy is back (D6,
  OD4), while native users carry on.

---

## 7. Chunks and acceptance criteria

### Milestone goals

| # | Goal | Verified by |
|---|---|---|
| G1 | Murmur sees each bridged user at the browser's own address | PX2 criterion 3, PX4 criterion 3 |
| G2 | A Mumble ban or autoban of one bridged user leaves another bridged user on the same pod connected and able to join | PX2 criterion 3, PX4 criterion 3 |
| G3 | Native clients, TCP and UDP, are unaffected, including one on the same address as a bridged user | PX1 criterion 4, PX2 criterion 3 |
| G4 | With the proxy configured, no voice user's Mumble connection ever reaches murmur from the pod's address | PX2 criteria 2 and 4 |
| G5 | "Murmur down" and "address banned" stay distinguishable through the proxy | PX1 criterion 4, PX2 criterion 4 |
| G6 | No gawk component logs a bridged user's address or labels a metric with it | PX1 criterion 3, PX2 criterion 6 |
| G7 | With the knob unset, the bridge behaves exactly as before | Every existing bridge test and e2e pass, green with their assertions unedited |

### PX0 — Spike (done 2026-11-26)

| # | Criterion | Result |
|---|---|---|
| 1 | Spoofing over loopback works against real murmur, with native clients beside it | Yes (F1–F3) |
| 2 | Bans and autoban apply per real address | Yes (F4, F5) |
| 3 | IPv6, murmur restart | Yes (F6, F7) |
| 4 | Failure signalling through a proxy | Ambiguous as built; fixed by D6 (F8, F9) |
| 5 | Latency cost | About 0.06 ms at p50 (F10) |
| 6 | Least privilege | `NET_RAW` to serve, `NET_ADMIN` only for the routes (F11, F12) |

### PX1 — The `gawk-mmproxy` module

| # | Criterion |
|---|---|
| 1 | D1–D7 and D10 implemented: `routes` and `serve`, the ops listener, §5.1's flags and envs, and the metrics. A config carry-all test covers every flag. |
| 2 | Unit tests: the v2 decoder against the spec's byte layouts for both families, TLVs skipped, every refusal of D2 (v1, `LOCAL`, `UNSPEC`, UDP, UNIX, short and slow headers, unspecified and multicast sources), and a fuzz target. D6 against a fake upstream, with a client that sends the header **and then trailing bytes** (a ClientHello) before reading: reset passed through as a reset; a dial failure, a bad header, `-max-conns` and missing routes each seen by the client as EOF, never a reset; half-closes passed on. `-max-conns`. D7 with fixture `/proc/net/tcp{,6}` files: murmur on `::`, on `0.0.0.0`, on `127.0.0.1` (ready), bound only to a pod address (not ready), and only to `::1` (IPv4 not ready). |
| 3 | The D10 source-walk test: no log call and no metric label takes an address. |
| 4 | Integration tests in Docker, skipped unless `GAWK_TEST_MMPROXY=1`, against real murmur 1.5.939 and 1.6.1001 in a pause-container namespace (PX0's topology). `routes` runs twice and leaves exactly one copy of each rule and route (F12). Without them `serve` stays up, closes every connection with a FIN and reports `routes_ok 0`, then serves normally once they appear (D5). F1–F9 become tests: spoofed v4 and v6 logged by murmur; a native client and a proxied user on the same address talking both ways; a native UDP ping answered; a kickban and an autoban each covering only their address; a murmur restart; a reset for a banned address and a FIN for murmur down. `serve` runs with `--cap-drop ALL --cap-add NET_RAW`. If file capabilities let it run as a non-root uid, a test proves it and D5 is updated. |

### PX2 — The bridge's proxy mode

| # | Criterion |
|---|---|
| 1 | D8, D6's classification and D12 implemented. The `-mumble-proxy-addr` flag, env and chart value; the chart's NetworkPolicy egress; `helm template` tests with and without it. The options carry-all test covers it. |
| 2 | Unit tests with the fake murmur (`internal/mumble/mumbletest`) behind a fake proxy: the header carries the session's canonical address (a v4-mapped remote arrives unmapped); an unparseable remote fails the join with 4016 `proxy_source` and makes no dial at all; in proxy mode an EOF before TLS is `murmur_unreachable` and a reset is `autoban`, while direct mode still reports both as `autoban`; the observer dials `-mumble-addr` directly. |
| 3 | Real-murmur integration tests. `testdata/murmur.sh` gains a fourth server, `proxied`: murmur 1.6.1001 in a pause-container namespace with `gawk-mmproxy`, exporting `GAWK_TEST_MURMUR_PROXY` and the address a native client from the host appears as. With voice sessions given distinct remote addresses, the tests check: murmur logs each address; a SuperUser kickban of one user ends that session with 4015, its rejoin is refused with 4016 `autoban`, and a second user on the same pod stays connected and can rejoin; a native client on the same address as a bridged user hears it and is heard. |
| 4 | Fail closed (OD4): with the proxy stopped, a join fails with 4016 `murmur_unreachable` within the dial timeout, and murmur's log shows no connection from the bridge's address. With murmur stopped behind a running proxy, the same, reported as `murmur_unreachable`, not `autoban`. |
| 5 | The bridge's encoder and `gawk-mmproxy`'s decoder are tested against each other: the bridge's header bytes for v4 and v6 sources, fed to the decoder from PX1, give the same addresses. |
| 6 | The bridge still never logs the remote address. The D24 source-walk test (`cmd/gawk-mumble-bridge/d24_test.go`) gains the remote address's variable names and the `Remote` field, so the address can't reach a logging, formatting or error-building call on the proxy dial path or anywhere else. |
| 7 | D12, in Ice mode: a session already in voice through one pod, then a second session with the same nickname and address joining through another pod that has no live Mumble connection; the second gets the suffixed name and the first stays connected. And the race: two such sessions joining on two pods at once end as one connected user and one 4016 `replaced`, whose Try again joins under a suffixed name. |

### PX3 — Release, CI and deploy files

| # | Criterion |
|---|---|
| 1 | release-please: a `gawk-mmproxy` component in the combined release PR, with D11's snippets as extra files. The release workflow builds and pushes `ghcr.io/tuhis/gawk-mmproxy:<version>` and `:latest`. Renovate covers its `go.mod` and Dockerfile. |
| 2 | CI: a change filter for `gawk-mmproxy/**`, `go test -race`, the linters, the licence check (`tools/licenses`), `THIRD-PARTY-NOTICES.md`, and a coverage floor in `coverage-floors.json`. A Docker job runs PX1 criterion 4 and PX2 criterion 3. It needs `--cap-add NET_ADMIN` from the runner's Docker daemon, which the `ioio-k8s` runners' privileged dind provides (checked 2026-11-26). |
| 3 | A kind job applies `deploy/k8s/*.yaml` beside a stock murmur Deployment, with a CNI that enforces NetworkPolicy, and checks four things. A proxied connection from a pod labelled as the bridge reaches murmur with its header's address. A pod without the label can't reach the proxy's port at all. With the init container denied `NET_ADMIN`, murmur's pod still starts, is Ready, and the proxy refuses with a FIN. With `serve` failing closed (no routes, murmur's port closed, over `-max-conns`), murmur's pod stays Ready and in its Service's endpoints (D15). Killing `serve`'s process leaves the pod briefly NotReady, then Ready again after the container restarts, as D15 and §8 say. |
| 4 | CLAUDE.md's repository layout gains `gawk-mmproxy` (eighth module, default off, no chart and why, D9's prohibition: its port is never routed publicly and never reachable from anything but bridge pods). CONTRIBUTING gains its build-and-test line and joins the module lists there. |

### PX4 — Dev stack and the voice e2e

| # | Criterion |
|---|---|
| 1 | `dev/compose.voice.yml`: `gawk-mmproxy` in murmur's namespace with its routes, and the bridge on `GAWK_BRIDGE_MUMBLE_PROXY_ADDR`, on by default and one variable away from off (`VOICE_PROXY=off`). |
| 2 | `e2e/udp-lb.mjs` gives each new flow its own loopback source (`127.0.0.N`), as a LoadBalancer with `externalTrafficPolicy: Local` preserves each client's address. The bridges then see distinct browser addresses. `gawk-mmproxy` runs beside the Ice-mode murmur on the host network with `-require-routes=false`: every source is a local loopback address, so no route is needed, and none is installed in the runner's namespace. |
| 3 | A new pass in `e2e/voice.mjs`, before pass 6 (the gawk ban), which needs alice in voice. Alice and bob in voice; murmur's log shows each at its own `127.0.0.N`; the SuperUser kickbans **bob**. Bob's voice ends with 4015 and his rejoin is refused with 4016 `autoban`. Alice stays in voice and still hears the native user, and the native user still hears alice. |
| 4 | Passes 1–5, 7 and 8 run unchanged with the proxy on. **Pass 6 changes one value**: its gawk ban widens from `127.0.0.1/32` to `127.0.0.0/8`, because with criterion 2 alice's address is `127.0.0.N`, not necessarily `.1`. It runs the same in proxy and direct mode, since the stand-in's distinct sources don't depend on the proxy. |

### PX5 — Docs

| # | Criterion |
|---|---|
| 1 | `docs/self-hosting.md` §15: a "Real addresses in Mumble" subsection covering what it does and why, the prerequisites (murmur on loopback, a NetworkPolicy-enforcing CNI, IPv6 only with `::1`), D11's snippets, D13's autoban advice, OD6's privacy statement, D15's no-probes rule, D16's trust boundary for Compose and bare hosts, the bare-host caveat of §8, and §15.4 "Is it working?" (`/readyz`, `gawk_mmproxy_routes_ok`, murmur's log showing browser addresses). |
| 2 | `gawk-mmproxy/README.md`, and the bridge README's knob table and privacy line. |
| 3 | `docs/gotchas.md` entries for F8, F9, F11, F12 and F13. docs/80 §11.5's "a Mumble ban bans the bridge pod's address" gains a dated note pointing here. |

---

## 8. Risks

- **No NetworkPolicy, or a CNI that ignores it.** Then anything in the
  cluster can make murmur see any address (D9). Nothing in the proxy can
  detect that. The guide makes the policy a prerequisite, not a
  suggestion, and PX3 criterion 3 proves the shipped policy blocks an
  unlabelled pod.
- **Source NAT in front of the proxy.** With kube-proxy's
  `masquerade-all`, or a hairpin, the proxy sees a node address and the
  policy's `podSelector` doesn't match. That fails closed (4016
  `murmur_unreachable`), which is safe, but it is confusing. The guide names it.
- **Something else in murmur's namespace talking to murmur over loopback**
  (another TCP proxy, a sidecar reaching murmur at `127.0.0.1`). Murmur's
  replies to it also leave from loopback and hit the policy route. On a
  bare host (D14) that covers every loopback service on the host. Dialling
  murmur at the pod's own address avoids it. The guide says so.
- **Murmur bound to a specific address** (`host=` set to the pod or host
  IP only). The proxy's loopback dial is refused. That fails closed, and
  `/readyz` shows it, because D7 looks at the LISTEN socket's local
  address, not only its port.
- **The proxy is a single point for web voice.** If it fails closed,
  every web user's join fails and existing web sessions end with 4016
  `connection_lost` (Voice unavailable, Try again; no automatic rejoin),
  while native users are unaffected (D15). If `serve` itself crashes, the
  container restart briefly unreadies murmur's pod, so murmur's Service
  drops the endpoint and native users' UDP conntrack is flushed: a short
  cut for everyone, as a murmur restart already is.
- **The port open outside Kubernetes.** Compose and bare hosts have no
  NetworkPolicy, so D16's bind address, unpublished port or firewall rule
  is the only boundary. The guide makes it a prerequisite.
- **Root with `NET_RAW`.** D5 runs uid 0 with one capability, and PX1
  measures whether file capabilities allow a non-root uid.
- **Murmur changes how it rejects banned addresses** (a FIN instead of a
  reset). Then a ban through the proxy reads as `murmur_unreachable`. PX1's
  integration matrix covers each murmur version gawk supports.

---

## 9. Out of scope

- A PROXY protocol patch to murmur (OD1).
- The reference deployment's rollout and its `autobanAttempts` (OD7).
- Native clients' own addresses. Those depend on murmur's own Service
  using `externalTrafficPolicy: Local`, which the reference deployment
  already does.
- UDP for bridged users, which never set it up (docs/80 §11.5).
- Any trust check in the proxy beyond the NetworkPolicy (OD3).
- Any change to the relay, the wire, the `voice` package or the web app.
