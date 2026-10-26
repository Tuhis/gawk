# gawk-server flag reference

Every flag has a `GAWK_*` environment fallback; precedence is
flag > env > default. All of them are also plumbed through the Helm chart's
`values.yaml` (see the comments there for the chart-side names), with one
deliberate exception: `-eventbus-insecure` has no chart value, because a
deployment must not be able to turn TLS verification off by setting one
(docs/51 D6).

| Flag | Env | Default |
|------|-----|---------|
| `-addr` | `GAWK_ADDR` | `:4433` |
| `-cert-file` | `GAWK_CERT_FILE` | (empty) |
| `-key-file` | `GAWK_KEY_FILE` | (empty) |
| `-dev-cert` | `GAWK_DEV_CERT` | `false` |
| `-dev-cert-hosts` | `GAWK_DEV_CERT_HOSTS` | `localhost,127.0.0.1` |
| `-log-level` | `GAWK_LOG_LEVEL` | `info` |
| `-log-format` | `GAWK_LOG_FORMAT` | `text` |
| `-max-subscribers` | `GAWK_MAX_SUBSCRIBERS` | `15` (per broadcast) |
| `-max-broadcasts` | `GAWK_MAX_BROADCASTS` | `5` |
| `-max-total-subscribers` | `GAWK_MAX_TOTAL_SUBSCRIBERS` | `50` (per node) |
| `-publish-secret` | `GAWK_PUBLISH_SECRET` | (empty) |
| `-conn-rate-limit` | `GAWK_CONN_RATE_LIMIT` | `3.0` |
| `-conn-burst-limit` | `GAWK_CONN_BURST_LIMIT` | `10` |
| `-max-bandwidth` | `GAWK_MAX_BANDWIDTH` | `0` (unlimited) |
| `-max-keyframe-bytes` | `GAWK_MAX_KEYFRAME_BYTES` | `8388608` (8 MiB) |
| `-keyframe-write-timeout` | `GAWK_KEYFRAME_WRITE_TIMEOUT` | `1s` |
| `-dvr-window` | `GAWK_DVR_WINDOW` | `3s` (DVR ring depth per broadcast) |
| `-dvr-max-bytes` | `GAWK_DVR_MAX_BYTES` | `25165824` (24 MiB) |
| `-dvr-max-catchup` | `GAWK_DVR_MAX_CATCHUP` | `4` (per-subscriber catch-up multiplier cap) |
| `-dvr-audio` | `GAWK_DVR_AUDIO` | `true` (include audio in the DVR ring) |
| `-metrics-addr` | `GAWK_METRICS_ADDR` | `:2112` (`off` disables) |
| `-allowed-origins` | `GAWK_ALLOWED_ORIGINS` | (empty = allow all) |
| `-max-idle-timeout` | `GAWK_MAX_IDLE_TIMEOUT` | `30s` |
| `-keepalive-period` | `GAWK_KEEPALIVE_PERIOD` | `10s` (`0` disables) |
| `-broadcast-grace` | `GAWK_BROADCAST_GRACE` | `5m` |
| `-publisher-stall-timeout` | `GAWK_PUBLISHER_STALL_TIMEOUT` | `90s` (a connected publisher that sends no datagram at all for this long reads as away; `0` disables; keep ≥ 90s for hidden-tab throttling) |
| `-publisher-stall-ends` | `GAWK_PUBLISHER_STALL_ENDS` | `false` (end a broadcast stalled for the grace with the terminal 4000 and free its slot) |
| `-quiet-probe-logs` | `GAWK_QUIET_PROBE_LOGS` | `false` |
| `-live-edge-audio-on-reliable-stream` | `GAWK_LIVE_EDGE_AUDIO_ON_RELIABLE_STREAM` | `false` |
| `-parity-default` | `GAWK_PARITY_DEFAULT` | `2` (forward-parity symbols per delta frame; `0` disables fleet-wide) |
| `-striped-delivery` | `GAWK_STRIPED_DELIVERY` | `true` (off is byte-identical to the pre-interleaving relay) |
| `-uplink-carriers` | `GAWK_UPLINK_CARRIERS` | `true` (accept broadcasters' reliable uplink carriers, [docs/75](../../docs/75-reliable-uplink.md) D2; off is byte-identical to the pre-R73 relay) |
| `-uplink-max-bitrate` | `GAWK_UPLINK_MAX_BITRATE` | `50mbps` (sizes the initial QUIC receive windows, 2 MB per stream / 3 MB per connection at the default, docs/75 D10; `0` keeps quic-go's defaults) |
| `-stateless-reset-key` | `GAWK_STATELESS_RESET_KEY` | (empty = disabled; 64 hex chars, shared fleet-wide) |
| `-resume-token-key` | `GAWK_RESUME_TOKEN_KEY` | (empty; wins over the publish-secret derivation — recommended for fleets) |
| `-stats-key` | `GAWK_STATS_KEY` | (empty = per-process random; 64 hex chars) |
| `-telemetry-key` | `GAWK_TELEMETRY_KEY` | (empty = telemetry disabled; 64 hex chars, shared with gawk-telemetry) |
| `-telemetry-report-interval` | `GAWK_TELEMETRY_REPORT_INTERVAL` | `2s` (500ms–60s) |
| `-telemetry-advertise-url` | `GAWK_TELEMETRY_ADVERTISE_URL` | (empty = advertise nothing; absolute https URL of this fleet's telemetry ingest) |
| `-server-name` | `GAWK_SERVER_NAME` | (empty = the relay stays unnamed) |
| `-cluster-mode` | `GAWK_CLUSTER_MODE` | `false` |
| `-internal-psk` | `GAWK_INTERNAL_PSK` | (empty; required with `-cluster-mode`) |
| `-internal-server-name` | `GAWK_INTERNAL_SERVER_NAME` | (empty; required with `-cluster-mode`) |
| `-trusted-cidrs` | `GAWK_TRUSTED_CIDRS` | (empty) |
| `-moderation-source` | `GAWK_MODERATION_SOURCE` | `off` (`off` \| `k8s` \| `file:<path>`) |
| `-admin-api-token` | `GAWK_ADMIN_API_TOKEN` | (empty; with no OIDC issuer either, `/internal/admin/*` is never registered — 404) |
| `-admin-oidc-issuer` | `GAWK_ADMIN_OIDC_ISSUER` | (empty; must be set together with `-admin-oidc-audience`) |
| `-admin-oidc-audience` | `GAWK_ADMIN_OIDC_AUDIENCE` | (empty; must be set together with `-admin-oidc-issuer`) |
| `-admin-oidc-roles-claim` | `GAWK_ADMIN_OIDC_ROLES_CLAIM` | `resource_access.{audience}.roles` (`{audience}` substituted; may not be blank once OIDC is configured) |
| `-admin-oidc-role` | `GAWK_ADMIN_OIDC_ROLE` | `operator` (may not be blank once OIDC is configured) |
| `-screening-token` | `GAWK_SCREENING_TOKEN` | (empty; `/internal/screening/*` is never registered — 404. Must differ from `-admin-api-token`) |
| `-screening-notice` | `GAWK_SCREENING_NOTICE` | `false` (tell every broadcaster this server screens streams for explicit content) |
| `-eventbus-url` | `GAWK_EVENTBUS_URL` | (empty = the R50 event bus is off entirely) |
| `-eventbus-creds-file` | `GAWK_EVENTBUS_CREDS_FILE` | (empty; path to a NATS `.creds` file) |
| `-eventbus-tls-cert` / `-eventbus-tls-key` | `GAWK_EVENTBUS_TLS_CERT` / `_KEY` | (empty; a client certificate — with `verify_and_map` its subject DN is the NATS user) |
| `-eventbus-ca-file` | `GAWK_EVENTBUS_CA_FILE` | (empty = platform trust store; set for a bus on a private CA) |
| `-eventbus-subject-prefix` | `GAWK_EVENTBUS_SUBJECT_PREFIX` | `gawk` |
| `-eventbus-viewer-interval` | `GAWK_EVENTBUS_VIEWER_INTERVAL` | `5s` |
| `-eventbus-insecure` | `GAWK_EVENTBUS_INSECURE` | `false` (local development only; warns at startup) |
| `-rooms` | `GAWK_ROOMS` | `false` |
| `-room-empty-grace` | `GAWK_ROOM_EMPTY_GRACE` | `60s` |
| `-max-rooms` | `GAWK_MAX_ROOMS` | `10` |
| `-max-room-broadcasts` | `GAWK_MAX_ROOM_BROADCASTS` | `4` |
| `-max-room-participants` | `GAWK_MAX_ROOM_PARTICIPANTS` | `50` |
| `-room-create-secret` | `GAWK_ROOM_CREATE_SECRET` | (empty = anyone with a live broadcast may mint a dynamic room) |
| `-rooms-file` | `GAWK_ROOMS_FILE` | (empty; JSON array of static rooms for non-Kubernetes deployments) |
| `-voice-bridge-addr` | `GAWK_VOICE_BRIDGE_ADDR` | (empty; the bridge's internal QUIC `host:port` for observer links) |
| `-voice-bridge-public-url` | `GAWK_VOICE_BRIDGE_PUBLIC_URL` | (empty; the bridge's https URL sent to browsers in every voice ticket) |
| `-voice-bridge-psk` | `GAWK_VOICE_BRIDGE_PSK` | (empty; gates the observer and session links) |
| `-voice-ticket-secret` | `GAWK_VOICE_TICKET_SECRET` | (empty; ticket HMAC key shared with the bridge, ≥ 16 bytes) |
| `-voice-bridge-server-name` | `GAWK_VOICE_BRIDGE_SERVER_NAME` | (empty = the host of `-voice-bridge-addr`) |
| `-voice-bridge-ca-file` | `GAWK_VOICE_BRIDGE_CA_FILE` | (empty = system roots; PEM roots for the observer dial) |
| `-voice-max-rooms` | `GAWK_VOICE_MAX_ROOMS` | `10` (voice-live dynamic rooms; per pod, fleet-wide in cluster mode) |
| `-voice-max-users-per-room` | `GAWK_VOICE_MAX_USERS_PER_ROOM` | `25` |
| `-pops` | `GAWK_POPS` | `false` (R74 PoP relays; needs `-cluster-mode` or `-pops-file`) |
| `-pops-file` | `GAWK_POPS_FILE` | (empty; YAML or JSON list of PoPs for non-Kubernetes deployments; no chart value) |
| `-pop-uplink-linger` | `GAWK_POP_UPLINK_LINGER` | `30s` |
| `-pop-offer-defer` | `GAWK_POP_OFFER_DEFER` | `2s` |
| `-pop-demand-timeout` | `GAWK_POP_DEMAND_TIMEOUT` | `5s` |

## Notes on the non-obvious ones

**`-rooms` is off by default and off means byte-identical** to a relay
without R42 ([docs/44](../../docs/44-rooms.md) D17): the `/room/` routes
are not registered, no registry is built, `/statusz` has no `rooms`
section and no `gawk_room*` series exist. With it on, a dynamic room is
minted by a live broadcaster (`CONNECT /room/new`, gated by
`-room-create-secret` when set and by `-max-rooms`), joined by its
six-character code (`CONNECT /room/{code}`), and ends after
`-room-empty-grace` with nobody in it — keep that grace longer than a
client reconnect interval, or a pod drain ends every room it hosted.
Static rooms come from `Room` CRs in cluster mode (`rooms.enabled` in the
chart) or from `-rooms-file` elsewhere: a JSON array of
`{"code": "TuhisRoom", "displayName": "...", "attachSecret": "...",
"maxBroadcasts": 4, "maxParticipants": 50}` objects, reloaded on change and on SIGHUP exactly like
`-moderation-source=file`. Room codes are joinable secrets like broadcast
IDs and are keyed by the same `-stats-key` HMAC everywhere they appear.

**Room voice (R79, [docs/80](../../docs/80-mumble-bridge.md)) is on only
when `-voice-bridge-addr`, `-voice-bridge-public-url`, `-voice-bridge-psk`
and `-voice-ticket-secret` are all set**, and needs `-rooms`; a partial set
refuses to start. Without them nothing changes: no `/internal/voice/` route,
no `voice` room capability, no observer link, no `gawk_*voice*` series. With
them, a static room with a `mumble` mapping (`spec.integrations.mumble` on
its CR, or `"mumble": {"channel": "Friends/Gaming"}` in `-rooms-file`) is
voice-live — homed and observed even before anyone joins, in cluster mode by
whichever pod claims its lease — and `CONNECT /room/new?voice=1` mints a
dynamic room with a temporary channel while `-voice-max-rooms` has room
(over it, the room is minted without voice). The home pod dials the bridge's
`/internal/observe` per voice-live room and serves the bridge's session
links on `CONNECT /internal/voice/{code}` (PSK-gated, internal only; a
non-home pod proxies them home).

**`-keepalive-period` is what keeps idle viewers connected** while the
broadcaster is away: QUIC PINGs reset both endpoints' idle timers. Raising
`-max-idle-timeout` alone does not help — the effective idle timeout is the
minimum of both endpoints' advertised values, and browsers advertise ~30 s.

**`-dev-cert` alone is ephemeral; with `-cert-file`/`-key-file` it
persists.** Alone it generates a fresh in-memory certificate on every start,
which invalidates the hash any browser was already given. Combined, it means
"generate into these paths if absent, otherwise load them", and the pair is
then watched by the same reloader production uses — so a restart keeps the
hash and a join link keeps working. `-key-file` is required with that
combination. Truth table:
[`docs/41`](../../docs/41-local-dev-stack.md) §4.2.1.

**`-metrics-addr` takes the literal value `off` to disable** (not the empty
string: an empty environment variable reads as unset and silently falls
back to the default — the Helm chart passes `off` when
`metrics.enabled=false`).

**`-quiet-probe-logs`** suppresses the `session started`/`session ended`
INFO logs for `/echo` sessions dialed from loopback — i.e. the k8s exec
probe, which otherwise logs on every probe period, forever. The plain
binary defaults to `false`; the Helm chart sets it `true`
(`config.quietProbeLogs`). Real, off-pod echo diagnostic use is never
affected — only loopback traffic is quieted.

**`-live-edge-audio-on-reliable-stream`** — measure before flipping; see
the chart values for the discussion.

**`-server-name`** is the display name a server picker shows next to your
relay's host once it has probed `/echo`. Unset, the relay is listed by
version alone.

**`-telemetry-advertise-url`** tells every session which telemetry ingest to
report to. Without it a viewer who arrived from someone else's frontend
reports to *that* deployment's collector, where its token is rejected and
the diagnostics are lost; with it, those sessions land in yours. An invalid
URL fails startup rather than silently advertising nothing. See
[`docs/self-hosting.md`](../../docs/self-hosting.md) §8.

**`-eventbus-url`** turns on the R50 event bus
([docs/51](../../docs/51-relay-event-bus.md)): this pod publishes broadcast and
room lifecycle events to an operator-provided NATS JetStream. Empty — the
default — builds no client, starts no goroutine and leaves the relay
byte-identical to one predating R50. It never affects the media path: a hook
does one non-blocking channel send, and a full queue, an unreachable server or
a missing stream are counted in `gawk_eventbus_dropped_total{reason}` rather
than waited on. A configured bus is never abandoned: the connection is
supervised and re-dialled every 60s, including after a NATS that refused this
pod's credential learns about it, so a late grant needs no restart (docs/51
D11). Subjects carry the fleet's HMAC'd keys; payloads carry raw
broadcast IDs and room codes, so the bus is internal-tier and must never be
routed publicly. `-eventbus-insecure` skips NATS TLS verification and exists
for the docs/41 compose lane only; it has no chart value and warns at every
start. Setup: [`docs/self-hosting.md`](../../docs/self-hosting.md) §12.

**`-moderation-source`** selects where R39 bans come from
([docs/42](../../docs/42-admin-moderation-portal.md) §4.3). `off` — the
default — constructs nothing: the ban set stays empty, every publish-path
check is a cheap miss, and the relay behaves exactly as it did before R39.
`k8s` watches `Ban` custom resources in `POD_NAMESPACE` and is *independent
of `-cluster-mode`* (enforcement is not a federation feature); it needs the
CRD installed and read-only RBAC on `bans`, and resolves credentials
in-cluster first, then by the ordinary kubeconfig rules (`KUBECONFIG`) — the
out-of-pod fallback the docs/41 compose lane relies on. `file:<path>` reads a
JSON array of ban records and reloads it when the file changes or the process
gets SIGHUP — the no-Kubernetes lane (docs/self-hosting.md §9). A banned publisher is rejected with HTTP
**451** before the WebTransport upgrade, counted as
`gawk_connections_total{route="publish",outcome="banned"}`; ban reasons are
logged at Debug only.

The Helm chart carries it as `moderation.source`, gated on
`moderation.enabled` — which is also what renders the `Ban` CRD, the
read-only RBAC on `bans` and the pod's `POD_NAME`/`POD_NAMESPACE` identity.
`file:<path>` through the chart means mounting that file yourself; `k8s` is
the deployed shape.

**The `-admin-*` flags** gate the relay's read-only admin API —
`GET /internal/admin/broadcasts`, `GET /internal/admin/config` and, since
R49, `GET /internal/admin/rooms` (the live roster of every room this pod is
home for; an empty list with `-rooms` off, docs/50 D2) on the **ops
listener** (docs/42 §4.5), the machine surface `gawk-admin` uses to
enumerate a fleet.

*The surface stays dark until a credential exists*: with `-admin-api-token`
empty **and** no `-admin-oidc-issuer`, those routes are never registered and
answer 404 rather than 401. Setting either one is what brings them into
being. The two are alternatives for the same routes — a static bearer token
for the admin service (deliberately not `-internal-psk`: a different trust
domain, independently rotatable), or an OIDC JWT verified offline against a
cached JWKS so a human can use the identity the portal already issued.

Half-configured OIDC is rejected at startup, never silently widened:
`-admin-oidc-issuer` and `-admin-oidc-audience` must both be set or both be
empty (issuer-without-audience would mean "accept any audience"), and once
OIDC is configured neither `-admin-oidc-roles-claim` nor `-admin-oidc-role`
may be blank (blanking either would turn authorization off). The claim
default is Keycloak's client-roles path, with `{audience}` substituted by the
configured audience; override it for an IdP that puts roles elsewhere.

These responses carry **raw broadcast IDs** — the one scoped relaxation of
the never-expose-raw-IDs invariant (docs/42 D8), acceptable only because the
ops listener is never routed publicly and the routes are credential-gated.
`/statusz` is unchanged and still HMAC-only. Chart-side these are
`moderation.adminApi.token` / `.tokenRef` and `moderation.adminApi.oidc.*`,
and the chart refuses to render any of them with `metrics.enabled: false` —
that would switch off the very listener they ride.

**`-screening-token`** gates R40's keyframe route,
`GET /internal/screening/broadcasts/{id}/keyframe`, on the same ops listener
([docs/71](../../docs/71-content-flagging.md) D1, §9.2): the one read
`gawk-sampler` makes of the relay. It returns the broadcast's cached keyframe
— the StreamFrame bytes verbatim — with `X-Gawk-Codec`,
`X-Gawk-Publisher-Epoch` and an `ETag` of publisher session and frame ID; a
matching `If-None-Match` is a 304, so an unchanged frame costs nothing. 404
means there is nothing to score now: no such broadcast, no keyframe yet, or a
publisher in its away grace (a frozen frame is never scored). Like the admin
routes, the surface stays dark — 404, not 401 — until the token is set. It is
**its own credential**: the admin token and admin JWTs are refused here, the
screening token is refused on `/internal/admin/*`, and the relay refuses to
start when the two tokens are equal, because the admin surface reads
publisher IPs and the sampler must never be able to. Chart-side:
`screening.token` / `.tokenSecretRef`, also refused with
`metrics.enabled: false`.

**`-screening-notice`** advertises the screening to every broadcaster
(docs/71 D13): the `RelayIdentity` notices bit on `/echo`, and — only while
it is on — one `RelayIdentity` stream on every publish session, which the
web, desktop and iOS broadcasters show as one line on the broadcast screen.
Off, publish sessions are byte-identical to a relay without the feature. It
is a separate switch from the token because the relay cannot see whether a
sampler actually runs; turn both on together. Chart-side: `screening.notice`.

**The cluster flags** (`-cluster-mode` and the shared keys/PSK) enable the
multi-pod federation — per-broadcast Kubernetes origin Leases, pod-to-pod
edge pulls, and resume tokens. Off (the default) the server never touches
the Kubernetes API and behaves exactly like the single-pod relay; see
[docs/22](../../docs/22-relay-scale-out.md) for the design and
[docs/05](../../docs/05-resilience-deploy.md) for the multi-replica
runbook.

**`-pops` is off by default.** With it on, the relay serves the `/pop/*`
routes PoP relays dial ([docs/76](../../docs/76-pop-relays.md) §9). A PoP
dials with Origin `gawk-relay://pop`, which the relay accepts on `/pop/*`
only, so it never needs adding to `-allowed-origins`. `-pop-uplink-linger`
is how long an uplink leg stays open after its last outside viewer leaves,
`-pop-offer-defer` how long a guest's session that announced it can switch
(`lr=1`) waits for the switch before home pulls the stream up anyway, and
`-pop-demand-timeout` how long an outside viewer waits for a PoP to start
forwarding before the broadcast reads as away. All five appear in
`/internal/admin/config`. Offers need real client addresses:
[docs/self-hosting.md](../../docs/self-hosting.md#14-pop-relays-r74) §14.

## gawk-relay

`cmd/gawk-relay` takes the same flags and `GAWK_*` envs with LAN-shaped
defaults ([docs/76](../../docs/76-pop-relays.md) §6.1 item 2): `-max-subscribers`
100, `-max-broadcasts` 50, `-max-total-subscribers` 500, `-metrics-addr`
`127.0.0.1:2112` and rooms on, in memory. With no certificate flags it
serves a rolling self-signed certificate from its state directory; an
explicit `-cert-file`/`-key-file` pair overrides that, and `-dev-cert` is
ignored with a warning. These flags are the relay's alone; `gawk-server`
does not register them:

| Flag | Env | Default |
|------|-----|---------|
| `-state-dir` | `GAWK_STATE_DIR` | (empty = the per-OS default) |
| `-console-addr` | `GAWK_CONSOLE_ADDR` | `127.0.0.1:4480` (loopback only; a non-loopback address refuses to start) |
| `-ui-addr` | `GAWK_UI_ADDR` | `:4433` (the HTTPS web app, named tier only) |
| `-lan-address` | `GAWK_LAN_ADDRESS` | `auto` (or an IP literal) |
| `-pair` | `GAWK_PAIR` | (empty; a `gawk-pair:…` or `gawk-cred:…` string) |
| `-pop-leg-delivery` | `GAWK_POP_LEG_DELIVERY` | `carrier` (or `datagram`; measured in docs/76 §11.2) |
| `-uplink-budget` / `-downlink-budget` | `GAWK_UPLINK_BUDGET` / `GAWK_DOWNLINK_BUDGET` | (empty = the home fleet's ceiling; `Mbps[/streams]`, lower only) |
