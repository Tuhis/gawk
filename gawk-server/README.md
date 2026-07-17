# gawk-server

Go WebTransport relay for the gawk game stream. One publisher (the
broadcaster's browser) sends encoded video as QUIC datagrams; the server fans
them out to up to 15 subscribers, caching the latest keyframe and decoder
config to prime late joiners.

Design, wire format and task breakdown: [`../docs/implementation-tasks.md`](../docs/implementation-tasks.md).

Routes: `CONNECT /publish` (single publisher; 409 while taken; a new
publisher session invalidates the priming caches), `CONNECT /subscribe`
(429 when full; primed with cached decoder config + last keyframe on join),
`CONNECT /echo` (connectivity diagnostic), `GET /healthz`,
`GET /statusz` (JSON stats: subscribers, frames/datagrams relayed, drops,
cached keyframe, per-subscriber detail).

A separate plain-TCP **ops endpoint** (R9, [`../docs/13-observability.md`](../docs/13-observability.md))
serves `GET /metrics` (Prometheus), `GET /healthz` and a mirror of
`GET /statusz` on `-metrics-addr` (default `:2112`) — the main server is
HTTP/3-over-UDP only, which Prometheus (and plain curl) can't reach. Never
expose this port publicly; the Helm chart exposes it via a ClusterIP Service
+ optional ServiceMonitor only.

## Build & test

```sh
go build ./...
go vet ./...
go test -race ./...
```

## Run

```sh
# Local dev (ephemeral in-memory cert; hashes for Chrome are logged at startup):
go run ./cmd/gawk-server -dev-cert

# Production (cert-manager-provisioned files, reloaded automatically on renewal):
go run ./cmd/gawk-server -cert-file /tls/tls.crt -key-file /tls/tls.key
```

To verify connectivity from the CLI (`-cert-hash` value is logged by the
server at startup; omit it to skip cert verification):

```sh
go run ./cmd/gawk-echo -cert-hash <cert_hash_hex>
```

Against a deployment with `-allowed-origins` set (e.g. production), also
pass `-origin` with one of the allowed values — a real CA-issued cert needs
no `-cert-hash`:

```sh
go run ./cmd/gawk-echo -url https://api.gawk.ioio.fi:4433/echo -origin https://gawk.ioio.fi
```

Every flag has a `GAWK_*` environment fallback (flag > env > default):

| Flag | Env | Default |
|------|-----|---------|
| `-addr` | `GAWK_ADDR` | `:4433` |
| `-cert-file` | `GAWK_CERT_FILE` | (empty) |
| `-key-file` | `GAWK_KEY_FILE` | (empty) |
| `-dev-cert` | `GAWK_DEV_CERT` | `false` |
| `-dev-cert-hosts` | `GAWK_DEV_CERT_HOSTS` | `localhost,127.0.0.1` |
| `-log-level` | `GAWK_LOG_LEVEL` | `info` |
| `-log-format` | `GAWK_LOG_FORMAT` | `text` |
| `-max-subscribers` | `GAWK_MAX_SUBSCRIBERS` | `15` |
| `-max-broadcasts` | `GAWK_MAX_BROADCASTS` | `5` |
| `-max-total-subscribers` | `GAWK_MAX_TOTAL_SUBSCRIBERS` | `50` |
| `-publish-secret` | `GAWK_PUBLISH_SECRET` | (empty) |
| `-conn-rate-limit` | `GAWK_CONN_RATE_LIMIT` | `3.0` |
| `-conn-burst-limit` | `GAWK_CONN_BURST_LIMIT` | `10` |
| `-max-bandwidth` | `GAWK_MAX_BANDWIDTH` | `0` (unlimited) |
| `-max-keyframe-bytes` | `GAWK_MAX_KEYFRAME_BYTES` | `8388608` (8 MiB) |
| `-keyframe-write-timeout` | `GAWK_KEYFRAME_WRITE_TIMEOUT` | `1s` |
| `-metrics-addr` | `GAWK_METRICS_ADDR` | `:2112` (`off` disables) |
| `-allowed-origins` | `GAWK_ALLOWED_ORIGINS` | (empty = allow all) |
| `-max-idle-timeout` | `GAWK_MAX_IDLE_TIMEOUT` | `30s` |
| `-keepalive-period` | `GAWK_KEEPALIVE_PERIOD` | `10s` (`0` disables) |
| `-broadcast-grace` | `GAWK_BROADCAST_GRACE` | `5m` |
| `-quiet-probe-logs` | `GAWK_QUIET_PROBE_LOGS` | `false` |
| `-stateless-reset-key` | `GAWK_STATELESS_RESET_KEY` | (empty = disabled; 64 hex chars, shared fleet-wide) |
| `-resume-token-key` | `GAWK_RESUME_TOKEN_KEY` | (empty; wins over the publish-secret derivation when set — recommended for fleets, R17) |
| `-stats-key` | `GAWK_STATS_KEY` | (empty = per-process random; 64 hex chars — R17) |
| `-cluster-mode` | `GAWK_CLUSTER_MODE` | `false` |
| `-internal-psk` | `GAWK_INTERNAL_PSK` | (empty; required with `-cluster-mode`) |
| `-internal-server-name` | `GAWK_INTERNAL_SERVER_NAME` | (empty; required with `-cluster-mode`) |
| `-trusted-cidrs` | `GAWK_TRUSTED_CIDRS` | (empty) |

The keepalive is what keeps idle viewers connected while the broadcaster is
away: QUIC PINGs reset both endpoints' idle timers. Raising
`-max-idle-timeout` alone does not help — the effective idle timeout is the
minimum of both endpoints' advertised values, and browsers advertise ~30s.

`-metrics-addr` takes the literal value `off` to disable (not the empty
string: an empty environment variable reads as unset and silently falls back
to the default — the Helm chart passes `off` when `metrics.enabled=false`).

`-quiet-probe-logs` suppresses the `session started`/`session ended` INFO
logs for `/echo` sessions dialed from loopback — i.e. the k8s exec probe,
which otherwise logs on every startup/liveness/readiness period, forever.
It defaults to `false` here (plain binary / local dev logs everything as
usual); the Helm chart sets it `true` by default (`config.quietProbeLogs`).
Real, off-pod echo diagnostic use is never affected — only loopback traffic
is quieted.

The R17 flags (`-cluster-mode` and the shared keys/PSK) enable the
multi-pod federation — per-broadcast Kubernetes origin Leases, pod-to-pod
edge pulls, and resume tokens. Off (the default) the server never touches
the Kubernetes API and behaves exactly like the single-pod relay; see
[docs/22](../docs/22-relay-scale-out.md) for the design and
[docs/05](../docs/05-resilience-deploy.md) for the multi-replica runbook.

On SIGINT/SIGTERM the server drains before exiting (R17 W1): every open
session gets close code 4002 (clients reconnect immediately), cluster mode
releases this pod's Leases, and the process exits within ~1.5 s.

`cmd/gawk-loadgen` is the synthetic-viewer load tool (R17 W6): N subscribe
sessions against one broadcast, reporting frames/s, keyframes, frameID gaps
and aggregate bitrate:

```sh
go run ./cmd/gawk-loadgen -url https://api.gawk.ioio.fi:4433 -id K7XQ2M -viewers 200 -duration 60s
```

## Docker

```sh
docker build -f deploy/Dockerfile -t gawk-server:dev .
docker run --rm -d --name gawk -p 4433:4433/udp gawk-server:dev -dev-cert
docker logs gawk 2>&1 | grep cert_hash_hex   # → hash for gawk-echo / the app
go run ./cmd/gawk-echo -url https://localhost:4433/echo -cert-hash <hex>
```

Distroless static, non-root (uid 65532), ~16 MB. `gawk-echo` is included in
the image: the server is HTTP/3-only, so the Helm chart's k8s probes exec it
against localhost (kubelet can't speak h3), and it doubles as an
in-container diagnostic. There is no shell in the image — anything exec'd
must be a binary path in exec-form.

## Deploy (Helm)

The chart lives in `deploy/charts/gawk-server/` and is published by CI to
`oci://ghcr.io/tuhis/charts/gawk-server`, versioned in lockstep with the
image (chart `version` == `appVersion` == image tag). Values that must be
set per install: `certificate.dnsNames`, `config.allowedOrigins` (the
frontend's origin) and `imagePullSecrets` — see the comments in
[`deploy/charts/gawk-server/values.yaml`](deploy/charts/gawk-server/values.yaml). Full runbook incl.
the GHCR pull secret: [`../docs/05-resilience-deploy.md`](../docs/05-resilience-deploy.md).

```sh
helm upgrade --install gawk-server oci://ghcr.io/tuhis/charts/gawk-server \
  --version <X.Y.Z> -n gawk -f my-values.yaml
```

The Deployment is fixed at one replica (`Recreate`): the hub is an
in-memory single-publisher pub/sub, so overlapping pods would split the
publisher and the subscribers.
