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
cached keyframe).

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
| `-allowed-origins` | `GAWK_ALLOWED_ORIGINS` | (empty = allow all) |
| `-max-idle-timeout` | `GAWK_MAX_IDLE_TIMEOUT` | `30s` |
| `-keepalive-period` | `GAWK_KEEPALIVE_PERIOD` | `10s` (`0` disables) |

The keepalive is what keeps idle viewers connected while the broadcaster is
away: QUIC PINGs reset both endpoints' idle timers. Raising
`-max-idle-timeout` alone does not help — the effective idle timeout is the
minimum of both endpoints' advertised values, and browsers advertise ~30s.

The server exits cleanly on SIGINT/SIGTERM.

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
