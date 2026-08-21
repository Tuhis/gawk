# gawk-server flag reference

Every flag has a `GAWK_*` environment fallback; precedence is
flag > env > default. All of them are also plumbed through the Helm chart's
`values.yaml` (see the comments there for the chart-side names).

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
| `-quiet-probe-logs` | `GAWK_QUIET_PROBE_LOGS` | `false` |
| `-live-edge-audio-on-reliable-stream` | `GAWK_LIVE_EDGE_AUDIO_ON_RELIABLE_STREAM` | `false` |
| `-parity-default` | `GAWK_PARITY_DEFAULT` | `2` (forward-parity symbols per delta frame; `0` disables fleet-wide) |
| `-striped-delivery` | `GAWK_STRIPED_DELIVERY` | `true` (off is byte-identical to the pre-interleaving relay) |
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

## Notes on the non-obvious ones

**`-keepalive-period` is what keeps idle viewers connected** while the
broadcaster is away: QUIC PINGs reset both endpoints' idle timers. Raising
`-max-idle-timeout` alone does not help — the effective idle timeout is the
minimum of both endpoints' advertised values, and browsers advertise ~30 s.

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

**`-moderation-source`** selects where R39 bans come from
([docs/42](../../docs/42-admin-moderation-portal.md) §4.3). `off` — the
default — constructs nothing: the ban set stays empty, every publish-path
check is a cheap miss, and the relay behaves exactly as it did before R39.
`k8s` watches `Ban` custom resources in `POD_NAMESPACE` and is *independent
of `-cluster-mode`* (enforcement is not a federation feature); it needs the
CRD installed and read-only RBAC on `bans`. `file:<path>` reads a JSON array
of ban records and reloads it when the file changes or the process gets
SIGHUP — the dev/compose lane. A banned publisher is rejected with HTTP
**451** before the WebTransport upgrade, counted as
`gawk_connections_total{route="publish",outcome="banned"}`; ban reasons are
logged at Debug only.

The Helm chart carries it as `moderation.source`, gated on
`moderation.enabled` — which is also what renders the `Ban` CRD, the
read-only RBAC on `bans` and the pod's `POD_NAME`/`POD_NAMESPACE` identity.
`file:<path>` through the chart means mounting that file yourself; `k8s` is
the deployed shape.

**The `-admin-*` flags** gate the relay's read-only admin API —
`GET /internal/admin/broadcasts` and `GET /internal/admin/config` on the
**ops listener** (docs/42 §4.5), the machine surface `gawk-admin` uses to
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

**The cluster flags** (`-cluster-mode` and the shared keys/PSK) enable the
multi-pod federation — per-broadcast Kubernetes origin Leases, pod-to-pod
edge pulls, and resume tokens. Off (the default) the server never touches
the Kubernetes API and behaves exactly like the single-pod relay; see
[docs/22](../../docs/22-relay-scale-out.md) for the design and
[docs/05](../../docs/05-resilience-deploy.md) for the multi-replica
runbook.
