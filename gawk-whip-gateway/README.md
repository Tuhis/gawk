# gawk-whip-gateway

WHIP ingest for gawk (R33, [docs/83](../docs/83-whip-gateway.md)). OBS Studio
30+ (or FFmpeg's `-f whip`, GStreamer's `whipsink`) streams to the gateway
over WebRTC, and the gateway republishes it to a gawk relay as an ordinary
publisher. Viewers watch it like any other broadcast; nothing on the relay or
viewer side changes.

The gateway is optional and off by default. A relay that no gateway publishes
to behaves exactly as before.

## How it fits

```
OBS ──POST /whip (SDP offer, Bearer gawk1_<ID>_<token>)──▶ gateway :8080 ──CONNECT /publish/{ID}?resume=<token>──▶ relay
    ◀──201 + SDP answer + Location────────────────────────        (Authorization: Bearer <publish-proxy token>,
    ══RTP H.264 + Opus (UDP or ICE-TCP, basePort+ordinal)═▶        Gawk-Publisher-Ip: <OBS's address>)
```

- **The stream key** is `gawk1_<ID>_<token>`: the broadcast code and the
  relay's resume token for it (D2). The gateway parses it, dials a reclaim of
  that ID, and holds no durable state of its own. A malformed key is `401`.
- **WebRTC** is [pion](https://github.com/pion/webrtc): ICE-lite, one UDP mux
  port per pod plus a passive ICE-TCP candidate on the same port number, host
  candidates rewritten to the shared public address (D10). The answer accepts
  one H.264 packetization-mode 1 layer whose profile-level-id viewers decode
  (Constrained Baseline, Main or High up to level 5.2) and Opus; HEVC, AV1 and simulcast are
  refused with `406` (D5). No trickle ICE.
- **The media adapter** rebuilds H.264 access units from RTP (single NAL,
  STAP-A, FU-A) in a reorder window of about one round trip (D6; a hole is
  NACKed once it has been missing for the reorder tolerance), drops a
  frame whose window closes and the deltas after it until the next IDR,
  refuses B-frames and streams it can't read, measures the keyframe interval
  (D7) and maps RTP/RTCP sender-report time onto the engine's clock, then
  feeds [`gawk-server/publisher`](../gawk-server/publisher), the shared Go
  publisher engine (chunking, keyframes on reliable streams, parity,
  TimeSync, auto-resume across relay drains).
- **Several replicas** (D10): a StatefulSet, the public route hashing on
  the `Authorization` header (a Gateway API HTTPRoute with Envoy Gateway's
  consistent hash, or an ingress-nginx Ingress), and an internal peer lookup for a key whose
  session or records sit on another pod after a rescale or a rollout.

### HTTP API (public listener)

| Route | What it does |
|---|---|
| `POST /whip` | SDP offer (`Content-Type: application/sdp`, `Authorization: Bearer <key>`) → `201` + answer + `Location: /whip/session/<id>` |
| `DELETE /whip/session/<id>` | OBS's Stop Streaming (same bearer key) → `200`; ends the broadcast (close 4014). OBS closes its PeerConnection first, so a session the sender closed waits 2 s for this DELETE before it counts as `lost` |
| `PATCH /whip/session/<id>` | `405`: no trickle ICE |
| `OPTIONS` on each route | WHIP/CORS preflight (`Accept-Post: application/sdp`) |
| `GET /whip/status` | D9's status for the presenting key (JSON, CORS for `-allowed-origins`); rate-limited per client IP (`429`) |
| `DELETE /whip/status` | "Retry now": clears D7's backoff and the reason that started it → `204` |
| `GET /healthz` | liveness |

`POST /whip` answers, in this order of checks: `401` malformed key, `415` not
SDP, `422` + `Retry-After` while D7's backoff runs for the key (checked first,
records nothing), `400` client IP unknown (D4), `429` + `Retry-After` the
gateway's own per-IP limit, `503` + `Retry-After: 30` at the pod's cap, `406`
codec or simulcast, then the relay's answer mapped by D8: `451` banned, `409`
reserved, `429` + `Retry-After` relay busy, `401`/`403` rejected, anything else
(unreachable, 5xx) `503` + `Retry-After`.

`GET /whip/status` (camelCase, unknown fields omitted, `warnings` always
present, never an IP):

```json
{"state":"live","codec":"H.264","resolution":{"width":1920,"height":1080},"fps":60,"gopMs":1000,
 "bitrateKbps":6000,"lossPct":0.1,"viewers":3,"warnings":[]}
{"state":"ended","codec":"H.264","gopMs":4000,"warnings":[],
 "lastError":{"reason":"gop_too_long","gopMs":4000},"retryAfterMs":48000}
```

`state` is `idle`, `live` or `ended`. The reason strings are a fixed set
defined once in [`internal/status/reasons.go`](internal/status/reasons.go):
warning `gop_long`; lastError `lost`, `gop_too_long`, `bframes`,
`invalid_h264`, `codec` (with `codec: "HEVC"|"AV1"` when known), `simulcast`,
`gateway_full`, `relay_busy`, `rejected`, `banned`, `reserved`,
`client_ip_unknown`, `killed`. An unknown well-formed key is
`{"state":"idle","warnings":[]}`.

### Ports

| Port | Default | Exposure |
|---|---|---|
| Public HTTP (`-listen`) | `:8080` | behind the public HTTPRoute or Ingress (TLS there), hashed on `Authorization` |
| Media UDP + ICE-TCP (`-media-base-port` + ordinal) | `8189` + N | one LoadBalancer Service per pod, shared address |
| Internal peer listener (`-internal-listen`) | `:8081` | other gateway pods only; never routed publicly |
| Ops (`-ops-listen`): `/healthz`, `/readyz`, `/metrics` | `:9090` | cluster only (ServiceMonitor) |

Internal peer routes (`Authorization: Bearer <internal token>`, `{hash}` the
hex SHA-256 of the full key): `GET`/`DELETE /internal/v1/keys/{hash}`,
`GET`/`DELETE /internal/v1/keys/{hash}/status`,
`DELETE /internal/v1/sessions/{id}?key={hash}`.

## Flags

Every flag has a `GAWK_WHIP_*` environment twin (`-relay-url` is
`GAWK_WHIP_RELAY_URL`); a flag wins over its env. Defaults marked *WG0* are
docs/83's design numbers, provisional until the lab measurement with real
OBS (WG0).

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `:8080` | public HTTP listener |
| `-internal-listen` | `:8081` | D10 peer listener |
| `-ops-listen` | `:9090` | `/healthz`, `/readyz`, `/metrics` |
| `-tls-cert-file`, `-tls-key-file` | — | serve the public listener over TLS directly (no Gateway or Ingress) |
| `-relay-url` | **required** | the relay, `https://host:port` |
| `-relay-publish-secret` | — | the relay's `-publish-secret` (prefer the env) |
| `-relay-publish-proxy-token` | — | the relay's `-publish-proxy-token` (D4; prefer the env) |
| `-relay-origin` | `gawk-broadcast://native` | Origin on the publish dial |
| `-relay-insecure` | `false` | skip relay TLS verification (dev certs) |
| `-relay-dial-timeout` | `10s` | how long a POST waits on the relay |
| `-public-address` | — | IP in every host candidate (the media LB's shared address) |
| `-media-bind` | all | host the media mux binds |
| `-media-base-port` | `8189` | media port of ordinal 0; a pod uses base + ordinal |
| `-pod-name` | — | the pod's name; its trailing `-N` is the ordinal (set from `metadata.name`) |
| `-ice-disconnected-timeout` | `5s` | *WG0* |
| `-ice-failed-timeout` | `15s` | silence until a session is `lost` (*WG0*) |
| `-ice-keepalive` | `2s` | |
| `-allowed-origins` | any | Origins allowed to read `/whip/status` cross-origin |
| `-trusted-proxy-cidrs` | — | the proxy that writes `X-Forwarded-For` (D4) |
| `-post-rate`, `-post-burst` | `1`, `5` | `POST /whip` per client IP: per second sustained, at once |
| `-status-rate`, `-status-burst` | `2`, `10` | `GET`/`DELETE /whip/status` per client IP (`429` + `Retry-After` past it; `0` disables) |
| `-max-sessions` | `20` | per pod, for a new key (*WG0*) |
| `-max-sessions-hard` | `ceil(1.25 × max)` | per pod, even for an exempt reconnect |
| `-full-retry-after` | `30s` | `Retry-After` of a `503` at the cap |
| `-relay-busy-retry-after` | `30s` | `Retry-After` for a relay `429` or an unreachable relay |
| `-reorder-initial-rtt` | `50ms` | D6 (*WG0*) |
| `-reorder-window-floor`, `-reorder-window-cap` | `60ms`, `150ms` | D6 (*WG0*; the floor was `20ms` until docs/83 §13.4) |
| `-reorder-window-multiplier`, `-reorder-window-offset` | `1.25`, `5ms` | window = m × RTT + offset (*WG0*) |
| `-reorder-nack-delay` | `3ms` | reorder tolerance: a hole is NACKed only if still missing this long, so a merely reordered packet is never NACKed or RTT-sampled; below the window floor (*WG0*) |
| `-reorder-max-packets` | `4096` | buffer bound, and the largest head-to-newest span; past it the head frame drops at once, or the window resyncs on the new packet (a loss: the next frame needs an IDR) |
| `-gop-warn`, `-gop-max` | `1.1s`, `2.2s` | D7 bounds (OD5) |
| `-gop-early-gap`, `-gop-samples` | `4.5s`, `5` | D7's early rule and median width |
| `-backoff-initial`, `-backoff-max` | `30s`, `10m` | D7's backoff ladder |
| `-record-ttl`, `-max-records` | `10m`, `4096` | D9's records |
| `-peers` | — | other pods' internal URLs (explicit list) |
| `-peer-template`, `-replicas` | —, `1` | `http://gawk-whip-gateway-{ordinal}.<headless>:8081`, every ordinal but this pod's |
| `-internal-token` | — | shared peer token; required with peers (prefer the env) |
| `-peer-timeout` | `200ms` | a slower peer counts as holding nothing |
| `-telemetry-url` | — | R28 ingest each OBS session's broadcaster telemetry is posted to (WG11, docs/83 §13), e.g. `http://gawk-telemetry/api/telemetry/v1/ingest`; empty reports only where the relay advertises an endpoint |
| `-log-level`, `-log-format` | `info`, `text` | |

The per-IP POST limit (1/s, burst 5) sits in front of the relay's own (3/s,
burst 10): OBS's automatic reconnect retries every few seconds, a user
stopping and starting a stream a few times in a row stays inside the burst,
and a LAN with several OBS machines behind one address still fits, while a
script probing keys or burning SDP-parse CPU is held to one attempt a second.

## Relay prerequisites

- An explicit `-resume-token-key` on the relay (D12): otherwise keys die on
  every relay restart, or anyone holding the publish secret (which the
  gateway does) can compute every broadcast's key.
- `-publish-proxy-token` on the relay, the same value as the gateway's
  `-relay-publish-proxy-token`, so the relay's bans and rate limits see OBS
  users' addresses instead of the gateway pod's (D4).
- Behind a proxy (a Gateway's Envoy pods, an ingress controller):
  `-trusted-proxy-cidrs` lists only the proxy's addresses (exact node
  addresses under `hostNetwork`, both families on dual-stack), and the proxy
  must see real client IPs (`externalTrafficPolicy: Local` or PROXY
  protocol). An empty list behind a proxy puts every user in one rate-limit
  bucket.

## Running it locally

```sh
go run ./cmd/gawk-whip-gateway -relay-url https://127.0.0.1:4433 -relay-insecure \
  -public-address 127.0.0.1
```

Then point OBS at **Settings → Stream → Service: WHIP**, server
`http://127.0.0.1:8080/whip`, bearer token a `gawk1_…` key (the web app's OBS
page mints one).

## Metrics

All on the ops listener, prefix `gawk_whip_`: `sessions` (gauge),
`posts_total{status}`, `sessions_ended_total{reason}`,
`frames_dropped_total{reason=reorder_window|awaiting_idr|engine_backpressure}`,
`frames_total`, `gop_seconds` (histogram of D7's median), `rtt_seconds`
(histogram of D6's estimate), `nacks_sent_total`,
`plis_sent_total{trigger=start|resume}`,
`xff_misconfigured_total{kind=untrusted_peer|client_ip_unknown}`,
`peer_requests_total{op,result}`, `build_info`.

## Per-session telemetry

With `-telemetry-url` set (or an ingest endpoint the relay advertises), each
session posts R28 broadcaster telemetry under the relay's telemetry identity,
so an OBS broadcast gets a broadcaster lane in `gawk-telemetry`. Each sample
is the publisher engine's stats (the gateway → relay leg) plus the gateway's
`whip*` fields (the OBS → gateway leg): packets expected and lost, NACKs and
what became of them (`whipRetransmitsRecovered`, `whipRetransmitsLate`), the
slowest retransmit since the last sample against the window D6 applied,
frames dropped by reason and the measured GOP. The sender is reported as a
class (`whipSender: "OBS 32.0"`), never its user-agent string, and nothing
carries the stream key or the raw broadcast ID ([docs/83 §13](../docs/83-whip-gateway.md#13-broadcaster-telemetry-for-obs-sessions-wg11-2026-11-03)).

## Tests

```sh
test -z "$(gofmt -l .)" && go vet ./... && CGO_ENABLED=1 go test -race ./...
CGO_ENABLED=0 go build ./...
```

The media adapter (`internal/media`) is pure and tested with synthetic H.264
(`internal/testmedia`, generated, no copied fixture). `internal/gateway`
drives a pion WHIP client in process against a fake relay seam
(`publisher.Options.Dial`), two instances behind a hashing proxy for D10, and
an end-to-end test that builds and runs the real `gawk-server` from this
checkout (skipped with `-short`).
