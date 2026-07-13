# R2 — Hardening: Design + Implementation Plan

Design doc for roadmap item [R2](../ROADMAP.md#r2--hardening).
This document details the goals, proposed changes, and verification plan for adding robust limits, access control, defensive parsing, and resource bounds to the relay server and the broadcaster client.

## Goals

1. **Configurable Limits**:
   - `max-broadcasts`: Limit the number of concurrent broadcasts active on the server (default: 5).
   - `max-total-subscribers`: Limit the total number of subscribers across all active broadcasts (default: 50).
   - `conn-rate-limit` / `conn-burst-limit`: Rate limit connection attempts per client IP using a thread-safe token bucket (default: 3.0 attempts/sec, burst 10). Bypass rate limiting for loopback addresses (k8s probes).
2. **Access Control**:
   - `publish-secret`: If configured server-side, publishers must present this secret in the query string (`?secret=<key>`) of their CONNECT request to `/publish` or `/publish/{id}`. Non-matching attempts are rejected with `401 Unauthorized` pre-upgrade.
3. **Resource Bounds & Defensive Parsing**:
   - Cap the maximum chunk count (`ChunkCount`) in a single video keyframe to `1000` (approx 1.2MB, far higher than any typical keyframe) during header parsing to prevent memory inflation attacks.
   - Reject any incoming datagram larger than `wire.MaxDatagramSize` (1200 bytes) in the hub handler.
4. **Bandwidth Limiting**:
   - `max-bandwidth`: Enforce a global egress bandwidth limit on total datagram bytes written to all subscribers (e.g. `10mbps`, `50mbps`, or raw bytes/sec).
   - Drop datagrams gracefully at the subscriber's network drain phase when the limit is exceeded (respecting `gawk`'s loss-tolerant philosophy).
5. **Observability**:
   - Obfuscate/mask broadcast IDs in `/statusz` by exposing their truncated SHA-256 hashes instead of raw IDs, ensuring unauthorized viewers cannot scrape `/statusz` to join active broadcasts.
   - Expose bandwidth drop metrics in `/statusz`.

---

## Status

Implemented 2026-07-13 (single change, plus the post-implementation review
fixes below). Acceptance per goal:

| Goal | Where | Verified by |
|------|-------|-------------|
| `max-broadcasts` / `max-total-subscribers` limits | `hub.Registry` (`StartPublish`, `CheckPublishNew`, `Subscribe`/`CheckSubscribe`) | `TestMaxBroadcastsLimit`, `TestMaxTotalSubscribersLimit`, `TestMintRejected429PreUpgradeAtBroadcastLimit` |
| Connection rate limiting + loopback bypass | `transport.rateLimited` / `ipRateLimiter` | `TestIPRateLimiter`, `TestConnRateLimit429OverNetwork`, `TestConnRateLimitLoopbackBypass` |
| Publish secret (401 pre-upgrade) | `transport.handlePublish` (constant-time compare) | `TestPublishSecret`; frontend URL tests in `broadcaster.test.ts` |
| Defensive parsing (chunk-count cap, size cap) | `wire.ParseVideoChunk`, `hub.HandleDatagram`; mirrored in `wire.ts` | `TestVideoChunkRoundTrip`, `TestOversizedDatagramDroppedAndCounted`, `wire.test.ts` |
| Bandwidth limit + drop accounting | `hub.bandwidthLimiter`, drain-phase drops | `TestBandwidthLimiting`, `TestBandwidthLimitingE2E`, `TestBandwidthDropsSurvive*` |
| `/statusz` ID obfuscation | `Registry.ObfuscateID` (per-process HMAC) | `TestObfuscatedStatsKeysArePerRegistry`, raw-ID assertion in `TestStatuszReachableAndNumbersMove` |
| Config plumbing (flags/env/Helm/prod wiring) | `config.go`, charts, `cmd/gawk-server/registryOptions` | `TestHardeningConfig`, `TestRegistryOptionsCarryAllLimits` |

## Decisions (reviewed and locked)

- **Publish Secret UX**: Broadcasters enter the secret once in the Settings panel (stored in `localStorage` alongside the Server URL and Dev Cert Hash). It travels as a `?secret=` query param because the WebTransport JS API cannot set request headers.
- **Default limits**:
  - `max-broadcasts`: Default 5.
  - `max-total-subscribers`: Default 50.
  - Egress bandwidth limit: Default `0` (unlimited).
  - Connection rate limiter: Default 3.0 attempts/sec (burst 10).
- **Rate Limiting Status Code**: `429 Too Many Requests` on rate-limited connection attempts, rejected pre-upgrade. The broadcast-capacity rejection on the mint path is also pre-upgrade 429 (post-review), matching the subscribe path.
- **ID obfuscation must be keyed** (post-review): broadcast IDs are only ~31^6 strong, so `/statusz` keys are a per-process HMAC-SHA256 of the ID — an unkeyed (even truncated) hash can be brute-forced offline back to joinable IDs. Keys change across server restarts; that's fine, they're only compared within one run.

---

## Proposed Changes

### `gawk-server`

#### [MODIFY] gawk-server/cmd/gawk-server/main.go
- Pass the new limits into `hub.NewRegistry` via a `registryOptions(cfg)`
  helper (unit-tested), and log them at startup. *(Added by the
  post-implementation review — the original plan omitted this file, and the
  implementation faithfully reproduced the omission: every limit parsed but
  never reached the production registry.)*

#### [MODIFY] gawk-server/internal/config/config.go
- Add new fields to `Config` struct:
  ```go
  MaxBroadcasts       int
  MaxTotalSubscribers int
  PublishSecret       string
  ConnRateLimit       float64
  ConnBurstLimit      int
  MaxBandwidthBytes   int64
  ```
- Plumb corresponding command-line flags and environment variables:
  - `-max-broadcasts` / `GAWK_MAX_BROADCASTS` (default: 5)
  - `-max-total-subscribers` / `GAWK_MAX_TOTAL_SUBSCRIBERS` (default: 50)
  - `-publish-secret` / `GAWK_PUBLISH_SECRET` (default: "")
  - `-conn-rate-limit` / `GAWK_CONN_RATE_LIMIT` (default: 3.0)
  - `-conn-burst-limit` / `GAWK_CONN_BURST_LIMIT` (default: 10)
  - `-max-bandwidth` / `GAWK_MAX_BANDWIDTH` (default: "0", support unit suffixes: `mbps`, `kbps`, `m`, `k`)
- Validate inputs during `ParseFlags`. Add helper `parseBandwidth` to handle bandwidth suffix strings.

#### [MODIFY] gawk-server/internal/wire/wire.go
- Add constant `MaxChunkCount = 1000`.
- Update `ParseVideoChunk` to reject `h.ChunkCount > MaxChunkCount` with `ErrBadChunkCount`.

#### [MODIFY] gawk-server/internal/hub/hub.go
- Add sentinel errors:
  ```go
  ErrMaxBroadcasts = errors.New("hub: max concurrent broadcasts reached")
  ErrTotalSubscribers = errors.New("hub: total subscriber limit reached")
  ```
- Update `Options` to include `MaxBroadcasts`, `MaxTotalSubscribers`, and `MaxBandwidthBytes`.
- Implement a thread-safe token bucket for bandwidth limiting:
  ```go
  type bandwidthLimiter struct {
      mu     sync.Mutex
      rate   float64
      burst  float64
      tokens float64
      last   time.Time
  }
  ```
- Registry tracks bandwidth drops:
  ```go
  totalBandwidthDroppedDatagrams uint64
  totalBandwidthDroppedBytes     uint64
  ```
- Update `StartPublish` to check `MaxBroadcasts` when minting a new broadcast.
- Update `Subscribe` and `CheckSubscribe` to check `MaxTotalSubscribers` across all active hubs.
- Update `Stats` and `RegistryStats` to include bandwidth drop stats:
  ```go
  BandwidthDroppedDatagrams uint64 `json:"bandwidthDroppedDatagrams"`
  BandwidthDroppedBytes     uint64 `json:"bandwidthDroppedBytes"`
  ```
- Hash-mask broadcast IDs in `Registry.Stats()` output keys to prevent ID leaking:
  ```go
  // Obfuscate ID as truncated SHA-256 hash (12 hex characters)
  func obfuscateID(id string) string {
      h := sha256.Sum256([]byte(id))
      return hex.EncodeToString(h[:6])
  }
  ```
- Update `HandleDatagram` to check `len(dgram) > wire.MaxDatagramSize` and count as bad.

#### [NEW] gawk-server/internal/transport/limiter.go
- Implement `ipRateLimiter` using a thread-safe map of client IP to `tokenBucket`. Includes a background cleanup sweep or passive cleanup to prevent memory leaks from transient IPs.

#### [MODIFY] gawk-server/internal/transport/server.go
- Initialize `ipRateLimiter` in `New` if `cfg.ConnRateLimit > 0`.
- Apply rate limiting in `handlePublish` and `handleSubscribe` (and `/echo`) pre-upgrade:
  ```go
  if s.limiter != nil && !isLoopbackAddr(r.RemoteAddr) {
      if !s.limiter.Allow(r.RemoteAddr) {
          w.WriteHeader(http.StatusTooManyRequests)
          return
      }
  }
  ```
- Enforce `PublishSecret` in `handlePublish` pre-upgrade:
  ```go
  if s.cfg.PublishSecret != "" && r.URL.Query().Get("secret") != s.cfg.PublishSecret {
      w.WriteHeader(http.StatusUnauthorized)
      return
  }
  ```
- Enforce egress bandwidth limit in `Subscriber.drain()`:
  ```go
  for dgram := range s.queue {
      if !s.hub.registry.ConsumeBandwidth(len(dgram)) {
          s.dropped.Add(1)
          s.hub.countBandwidthDrop(len(dgram))
          continue
      }
      if err := s.sender.SendDatagram(dgram); err != nil {
          s.sendErrors.Add(1)
      }
  }
  ```

#### [MODIFY] gawk-server/deploy/charts/gawk-server/values.yaml
- Add new keys in `config` section:
  ```yaml
  maxBroadcasts: 5
  maxTotalSubscribers: 50
  publishSecret: ""
  connRateLimit: 3.0
  connBurstLimit: 10
  maxBandwidth: "0" # unlimited
  ```

#### [MODIFY] gawk-server/deploy/charts/gawk-server/templates/deployment.yaml
- Map Helm values to env variables:
  ```yaml
  - name: GAWK_MAX_BROADCASTS
    value: {{ .Values.config.maxBroadcasts | quote }}
  - name: GAWK_MAX_TOTAL_SUBSCRIBERS
    value: {{ .Values.config.maxTotalSubscribers | quote }}
  - name: GAWK_PUBLISH_SECRET
    value: {{ .Values.config.publishSecret | quote }}
  - name: GAWK_CONN_RATE_LIMIT
    value: {{ .Values.config.connRateLimit | quote }}
  - name: GAWK_CONN_BURST_LIMIT
    value: {{ .Values.config.connBurstLimit | quote }}
  - name: GAWK_MAX_BANDWIDTH
    value: {{ .Values.config.maxBandwidth | quote }}
  ```

---

### `gawk-app`

#### [MODIFY] gawk-app/src/state/transportStore.ts
- Add `publishSecret` state and `setPublishSecret` action. Persist to `LS_PUBLISH_SECRET = 'gawk.publishSecret'`.

#### [MODIFY] gawk-app/src/features/stream/ServerSettings.tsx
- Add a new input field for entering the "Publish Secret" (using `type="password"`).

#### [MODIFY] gawk-app/src/transport/connection.ts
- Extend `ConnectOptions` interface:
  ```typescript
  export interface ConnectOptions {
    certHashHex?: string;
    publishSecret?: string;
  }
  ```

#### [MODIFY] gawk-app/src/transport/broadcaster.ts
- Construct URL with `secret` query param if provided:
  ```typescript
  const path = this.broadcastId ? `/publish/${this.broadcastId}` : '/publish';
  const urlObj = new URL(path, this.serverUrl);
  if (this.connectOpts.publishSecret) {
    urlObj.searchParams.set('secret', this.connectOpts.publishSecret);
  }
  const url = urlObj.toString();
  ```

#### [MODIFY] gawk-app/src/features/stream/BroadcastPage.tsx
- Read `publishSecret` from the Zustand store and pass it in `ConnectOptions` during `BroadcastPipeline` initialization.

---

## Verification Plan

### Automated Tests

#### Config Tests
- Test flag/env configuration parsing, validation, and units conversions (e.g. `10mbps` -> `1250000` bytes/sec).

#### Hub & Defensive Parsing Tests
- Verify `StartPublish` rejects if `MaxBroadcasts` is exceeded.
- Verify `Subscribe` and `CheckSubscribe` reject if `MaxTotalSubscribers` is reached.
- Verify keyframes with `ChunkCount > 1000` are rejected.
- Verify datagrams larger than `1200` bytes are counted as bad and dropped.
- Verify `/statusz` output hides/obfuscates actual broadcast IDs.

#### Connection Rate Limiting Tests
- Test connection attempts rate limiting using simulated multiple dial requests (expecting `429 Too Many Requests` after burst exceeds).
- Verify loopback requests bypass connection rate limits.

#### Publish Secret Access Control Tests
- Verify dial with missing/incorrect `secret` fails with `401 Unauthorized` (or HTTP CONNECT upgrade rejection).
- Verify dial with valid `secret` succeeds.

#### Bandwidth Limiting Tests
- Verify bandwidth drops occur once the set bandwidth limit is exceeded, and the `bandwidthDropped` stats increase.

### Manual Verification

1. Start `gawk-server` with `-publish-secret=supersecret`.
2. Open the broadcaster UI without specifying the secret; start broadcasting (expect `401 Unauthorized` or dial failure).
3. Enter the correct secret in settings, start broadcasting (expect successful dial and broadcast ID minted).
4. Share the join link; viewers should be able to watch without needing to enter any secret (frictionless watch).
5. Verify `/statusz` output hides the broadcast IDs under a hashed key.

---

## Post-implementation review (2026-07-13)

A full review against this doc and [CODE-REVIEW.md](../CODE-REVIEW.md) found
six issues in the initial implementation (all tests green). All were fixed
test-first — each fix's test was run red before the fix landed.

1. **Critical — limits never wired into production.** `main.go` still built
   the registry with only `MaxSubscribers` + `BroadcastGrace`:
   `-max-bandwidth` was a complete no-op, and `-max-broadcasts` /
   `-max-total-subscribers` overrides were silently ignored (the registry's
   internal defaults coincide with the flag defaults, masking it). Only the
   *test helper* had been wired, so every test passed. Root cause: this doc's
   file list omitted `main.go`. Fix: `registryOptions(cfg)` +
   `TestRegistryOptionsCarryAllLimits`; limits added to the startup log.
   Lesson recorded in CODE-REVIEW.md's "fully plumbed" rule: the plumbing
   includes the production call site, and a design doc's file list is not
   proof of completeness.
2. **High — brute-forceable `/statusz` obfuscation.** Truncated *unsalted*
   SHA-256 over a ~31^6 ID space is enumerable offline in seconds; anyone who
   could read `/statusz` could recover every joinable broadcast ID, defeating
   this doc's stated goal. Fix: per-process-keyed HMAC
   (`Registry.ObfuscateID`), `TestObfuscatedStatsKeysArePerRegistry`.
3. **Medium — bandwidth drops lost after owner teardown.** Drain-phase drops
   landed after `Subscriber.Close` had folded the subscriber's count (post-
   close backlog) or after `handleGraceExpiry` had folded and deleted the hub
   (post-GC drops) — violating "counters survive their owner's deletion".
   Fix: fold once, after drain finishes, with a fall-back to registry totals
   when the hub is gone; `TestBandwidthDropsSurviveSubscriberClose` /
   `TestBandwidthDropsSurviveHubGC`.
4. **Medium — promised tests missing.** The verification plan's oversized-
   datagram, network-level 429 + loopback-bypass, raw-ID-absent, and frontend
   secret-URL tests didn't exist. Added (the rate-limit test needed a
   `testHookRateLimitLoopback` seam — test dials come from 127.0.0.1, which
   production bypasses).
5. **Medium — `MaxChunkCount` existed only in Go.** `wire.ts` (the designated
   mirror) had no `MAX_CHUNK_COUNT`; a >1000-chunk keyframe would be silently
   eaten server-side while the broadcaster kept sending. Mirrored with
   encode+parse validation (encode throws loudly — the broadcaster is the
   side that produces chunk counts) and documented in docs/02.
6. **Low.** Secret compare made constant-time (`crypto/subtle`); mint-path
   capacity rejection moved pre-upgrade (HTTP 429, symmetric with subscribe;
   `CheckPublishNew`); gofmt dirt cleaned and `gofmt -l` added to the gates;
   README/CLAUDE.md status synced; this doc gained the Status/Decisions
   sections above.
