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
| `-cluster-mode` | `GAWK_CLUSTER_MODE` | `false` |
| `-internal-psk` | `GAWK_INTERNAL_PSK` | (empty; required with `-cluster-mode`) |
| `-internal-server-name` | `GAWK_INTERNAL_SERVER_NAME` | (empty; required with `-cluster-mode`) |
| `-trusted-cidrs` | `GAWK_TRUSTED_CIDRS` | (empty) |

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

**The cluster flags** (`-cluster-mode` and the shared keys/PSK) enable the
multi-pod federation — per-broadcast Kubernetes origin Leases, pod-to-pod
edge pulls, and resume tokens. Off (the default) the server never touches
the Kubernetes API and behaves exactly like the single-pod relay; see
[docs/22](../../docs/22-relay-scale-out.md) for the design and
[docs/05](../../docs/05-resilience-deploy.md) for the multi-replica
runbook.
