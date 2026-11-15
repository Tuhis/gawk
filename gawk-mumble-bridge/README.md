# gawk-mumble-bridge

Room voice for gawk through a Mumble server (R79, [docs/80](../docs/80-mumble-bridge.md)).
A participant clicks **Join voice** in a room and talks to the people in the
room's Mumble channel from the browser. Each gawk user appears in Mumble as
their own user, `Alice [web]`; the room shows who is in the channel, native
Mumble users included.

The bridge is optional and off by default. Nothing changes for a relay that
is not configured to use it.

## How it fits

```
browser ── WebTransport :4443 (VoiceHello + ticket, VoiceFrame datagrams) ──▶ bridge pod
relay home pod ── /internal/observe :4444 (observer link) ───────────────────▶ bridge pod
bridge pod ── /internal/voice/{code} (session link) ─────────────────────────▶ relay
bridge pod ── one TLS connection per gawk user (plus one observer per room ───▶ murmur
              in observer mode)
bridge pod ── Ice :6502, getUsers/getChannels and one callback registration ─▶ murmur  (Ice mode)
murmur ── Ice callbacks, pushed to each pod's :6503 ─────────────────────────▶ bridge pod (Ice mode)
```

The roster has two modes, chosen by configuration (R85,
[docs/89](../docs/89-mumble-ice-roster.md)):

- **Ice mode** (`-mumble-ice-addr` set, recommended): the roster comes from
  murmur's Ice administration interface. Every pod keeps one
  `ServerCallback` registration and a mirror of the server's users and
  channels that murmur's pushed events keep current, and re-reads it every
  `-mumble-ice-reconcile`. No Mumble user other than gawk's voice users
  exists. A dynamic room's temporary channel is created by its first gawk
  voice user. On murmur 1.6.870, whose callback handling crashes, the
  bridge never registers and the roster runs on the re-read alone.
- **Observer mode** (no `-mumble-ice-addr`): R79's behaviour, a visible
  observer user per voice-live room.

- **Public listener** (`-listen`, QUIC, a UDP LoadBalancer): `CONNECT /voice`.
  The browser sends a `VoiceHello` with a ticket its room's home pod minted;
  the bridge verifies it offline (HMAC, `-ticket-secret`), dials a session
  link to the relay, registers, and on `Accepted` logs the user in to murmur
  and joins the room's channel. Opus passes through untouched in both
  directions; each bridged connection receives its own audio, so Mumble's
  ACLs and whispers apply as for a native client.
- **Internal listener** (`-internal-listen`, QUIC, ClusterIP only):
  `CONNECT /internal/observe?psk=`. The room's home relay pod opens one
  observer link per voice-live room. The bridge pod that accepts it reports
  the channel's users back: in Ice mode from its mirror, in observer mode
  through the room's observer, a Mumble user named `gawkBridgeBot` that sits
  in the channel, creating a dynamic room's temporary channel and keeping it
  alive.
- **Ice callback listener** (`-mumble-ice-callback-listen`, TCP, Ice mode
  only, never public): murmur dials it to push roster events. It serves one
  random identity per process.
- **Ops listener** (`-ops-listen`, TCP, never public): `/healthz`, `/readyz`,
  `/metrics`, `/statusz`.

Close codes a browser sees, each announced in-band first (`SessionClosing`):
**4015** removed from voice (a gawk kill or ban, a Mumble kick or ban, a newer
session of the same participant; terminal for voice), **4016** voice
unavailable (murmur down, autoban, server full, a channel it may not enter,
the per-pod cap, an expired ticket; try again), **4002** the bridge pod is
draining or the room link dropped (rejoin at once with a fresh ticket).

The bridge never writes, logs or exposes voice audio, tickets, raw room codes
or client addresses (docs/80 D24); logs and `/statusz` key rooms by
`HMAC(-stats-key, code)`. `cmd/gawk-mumble-bridge/d24_test.go` enforces the
audio half statically.

## Flags

Every flag has a `GAWK_BRIDGE_*` environment twin (`-relay-url` is
`GAWK_BRIDGE_RELAY_URL`); a flag beats its env. Prefer the env (or the chart's
Secret refs) for the secrets.

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `:4443` | public QUIC listener for voice sessions |
| `-internal-listen` | `:4444` | internal QUIC listener for observer links |
| `-ops-listen` | `:2113` | `/healthz`, `/readyz`, `/metrics`, `/statusz` |
| `-cert-file`, `-key-file` | — | TLS pair for both QUIC listeners, reloaded on change (required unless `-dev-cert`) |
| `-dev-cert` | `false` | an in-memory self-signed certificate (13 days; its SHA-256 is logged for `serverCertificateHashes`) |
| `-allowed-origins` | — | comma list of Origins allowed on `/voice` (the relay's `allowedOrigins`); empty allows any |
| `-psk` | — | gates both internal links (the relay's `-voice-bridge-psk`) |
| `-ticket-secret` | — | ticket HMAC key and temporary-channel key, ≥ 16 bytes (the relay's `-voice-ticket-secret`) |
| `-relay-url` | — | `https://host:port` the session links dial |
| `-relay-server-name` | host of `-relay-url` | TLS name verified on that dial |
| `-relay-ca-file` | system roots | extra roots for that dial |
| `-mumble-addr` | — | murmur `host:port` |
| `-mumble-password` | — | murmur's server password |
| `-mumble-access-token` | — | access token every bridged connection sends (ACL group `#<token>`) |
| `-mumble-ca-file` | system roots | roots for murmur's certificate |
| `-mumble-cert-sha256` | — | pin murmur's certificate (SHA-256 of the DER, hex) |
| `-mumble-insecure-skip-verify` | `false` | accept any murmur certificate (lab only) |
| `-mumble-name-template` | `{nick} [web]` | a bridged user's Mumble name |
| `-mumble-observer-name` | `gawkBridgeBot` | the observer's Mumble name (observer mode only) |
| `-mumble-temp-parent` | root | channel path temporary channels go under |
| `-mumble-temp-channel-template` | `gawk {key}` | a dynamic room's channel; `{key}` keyed hash, `{code}` the code |
| `-mumble-client-version` | `1.4.0` | reported Mumble version; must be ≥ 1.2.4 and < 1.5.0 |
| `-max-voice-sessions` | `200` | voice sessions per pod; more get 4016 |
| `-mumble-ice-addr` | — | murmur's Ice `host:port` (TCP); set, Ice mode |
| `-mumble-ice-secret` | — | murmur's `icesecretwrite` (callbacks need the write secret) |
| `-mumble-ice-server-id` | discovered | the virtual server's Ice ID; default the one booted server |
| `-mumble-ice-timeout` | `2s` | deadline on each Ice call and the dial |
| `-mumble-ice-callback-listen` | `:6503` | the callback endpoint's TCP listen address |
| `-mumble-ice-callback-host` | — | the address murmur dials to reach this pod (the pod IP); required in Ice mode |
| `-mumble-ice-callback-timeout` | `100ms` | the timeout murmur uses to reach the pod, 20 ms–1 s; it bounds a dead pod's stall of murmur |
| `-mumble-ice-reconcile` | `30s` | the full re-read, 5 s–5 min |
| `-mumble-ice-callback-probe` | `10m` | after giving up on callbacks, the first retry, doubling to 2 h; 30 s–2 h |
| `-max-temp-channels` | `50` | temporary channels this pod's observers hold (observer mode); in Ice mode, those under the parent server-wide, checked before a create |
| `-drain-timeout` | `10s` | bound on the SIGTERM drain |
| `-stats-key` | random per process | key for room hashes in logs and `/statusz` |
| `-log-level`, `-log-format` | `info`, `text` | `debug`…`error`; `text` or `json` |

The three murmur trust flags are exclusive. The bridge refuses to start on
any invalid value and names the flag, and on any `-mumble-ice-*` flag set
without `-mumble-ice-addr`.

## Murmur prerequisites

The operator brings their own murmur (docs/80 OD9, §5). The bridge detects
and reports a violation (`gawk_bridge_mumble_failures_total{reason}`,
`/statusz` `mumbleFailures`), but cannot fix it:

- **`autobanAttempts=0`**, or a value above the bridge's connection rate.
  Every bridged user is a connection from the bridge's few addresses, and
  murmur's default bans an address after 10 connections in 2 minutes. A
  banned bridge is reported as reason `autoban`: murmur drops the
  connection before TLS without saying why.
- **`users`** sized for native plus bridged users plus one observer per
  voice room (reason `server_full`).
- **`messagelimit` / `messageburst`** bind per connection, so per bridged
  user; the defaults (1/s, burst 5) are enough.
- **The `username` rule** must allow the `{nick} [web]` form. murmur 1.5 and
  1.6 with no `username` setting accept spaces and brackets; the rule shown
  as the default in murmur's sample ini, `[-=\w\[\]\{\}\(\)\@\|\.]+`, does
  not. Murmur 1.6 also refuses non-ASCII letters. On `InvalidUsername` the
  bridge retries once with every other character replaced by `_`
  (`Alice_[web]`), then `_2`, `_3`…
- **An ACL on the temporary-channel parent** granting `#<access token>`
  `MakeTempChannel` and `Enter`, and on mapped static channels whatever
  bridged users should have.
- **For Ice mode**: murmur 1.5, or 1.6.1001 and later; `ice=` and
  `icesecretwrite` set, on the private network (Ice is clear text and the
  write secret administers the server); and murmur able to reach every
  bridge pod's IP on `-mumble-ice-callback-listen`'s port.

Two murmur behaviours the bridge works around, measured on 1.5.939 and
1.6.870:

- A second login with a name already in use **from the same address** is not
  refused: murmur disconnects the first one as a ghost. All of a pod's
  connections share its address, so the bridge skips names its own
  connections hold or can see on the server before it dials (`Alice [web] 2`).
  Two bridge pods that reach murmur through one SNAT address can still
  ghost each other's same-named user; that user sees 4016 and can rejoin.
- Murmur creates a temporary channel only for a user with a client
  certificate, so every bridged connection presents a fresh self-signed one
  (never stored, unregistered). A Mumble ban of a bridged user bans its
  certificate and **its address — the bridge pod's** — so prefer a kick.

## Running it locally

```sh
cd gawk-mumble-bridge
go run ./cmd/gawk-mumble-bridge -dev-cert \
  -psk dev-psk -ticket-secret dev-ticket-secret-0123 \
  -relay-url https://localhost:4433 -relay-ca-file <the relay's dev certificate> \
  -mumble-addr localhost:64738 -mumble-insecure-skip-verify
```

In the compose dev stack the bridge mounts the relay's generated certificate
(`-cert-file`/`-key-file`), so the relay's `-voice-bridge-ca-file` and the
browser trust it the same way they trust the relay; `-dev-cert` is for a
bridge run on its own.

`cmd/gawk-mumblesim` is the e2e stand-in for a native Mumble user: it joins a
channel (`-channel Friends/Gaming`, or `-temp-under Gawk -temp-code <code>
-ticket-secret …` for gawk's temporary channel), and `-echo`es what it hears
or `-play`s `testdata/tone-440hz-10ms.opus` (regenerated by
`testdata/gen-tone.sh`), printing one JSON line a second with the frames it
received per speaker. As SuperUser it can `-kick`, `-mute` and `-unmute` a
user and `-grant-temp <token>` on `-channel`.

## Tests

```sh
go test -race ./...                       # unit tests, a fake murmur in-process
eval "$(testdata/murmur.sh up 1.5)"       # or 1.6 (1.6.1001), 1.6.870: three real murmurs in docker
go test -race -p 1 ./...                  # now also the TestMurmur* integration tests
go test -run 'TestMurmurIceUnreachable|TestMurmurBlackholed' ./internal/...  # the freeze timings, without -race
testdata/murmur.sh down
```

`testdata/murmur.sh` starts a `main` murmur (the §5 prerequisites), a
`strict` one (the no-space username rule) and an `autoban` one (3
connections a minute), and prints `GAWK_TEST_MURMUR`,
`GAWK_TEST_MURMUR_STRICT`, `GAWK_TEST_MURMUR_AUTOBAN` and
`GAWK_TEST_MURMUR_SUPERUSER_PASSWORD`. `main` also has Ice on, runs on the
host's network so it can dial the tests' callback endpoints on loopback,
and adds `GAWK_TEST_MURMUR_ICE`, `GAWK_TEST_MURMUR_ICE_SECRET`,
`GAWK_TEST_MURMUR_ICE_READ_SECRET`, `GAWK_TEST_MURMUR_CALLBACK_HOST` and
`GAWK_TEST_MURMUR_CONTAINER` (its log is how the tests see murmur add and
remove callbacks). The Ice tests run in both roster modes; on 1.6.870 the
callback tests skip and the version gate runs. `GAWK_TEST_MURMUR_RESTART=1`
lets the outage test restart murmur, so run with `-p 1` then: another
package's tests would lose their murmur. Without the variables the
integration tests skip. `GAWK_MURMUR_PREFIX` and `GAWK_MURMUR_BASE_PORT` run a second set side
by side.

## Image and chart

```sh
docker build -f gawk-mumble-bridge/deploy/Dockerfile -t gawk-mumble-bridge:dev .   # from the repo root
docker build -f gawk-mumble-bridge/deploy/Dockerfile --target test .
helm lint gawk-mumble-bridge/deploy/charts/gawk-mumble-bridge
```

The chart (`deploy/charts/gawk-mumble-bridge`) renders a Deployment, the
public UDP LoadBalancer Service for `:4443`, an internal ClusterIP Service
for `:4444/udp` and `:2113/tcp`, a PodDisruptionBudget, and optionally a
cert-manager Certificate, a ServiceMonitor and a NetworkPolicy. Its
`values.yaml` documents every value; secrets take existing-Secret refs.
