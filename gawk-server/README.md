# gawk-server

Go WebTransport relay for the gawk game stream. One publisher (the
broadcaster's browser) sends encoded video as QUIC datagrams; the server fans
them out to up to 15 subscribers, caching the latest keyframe and decoder
config to prime late joiners.

Design, wire format and task breakdown: [`../docs/implementation-tasks.md`](../docs/implementation-tasks.md).

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

The server exits cleanly on SIGINT/SIGTERM.
