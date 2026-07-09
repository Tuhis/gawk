# `gawk-server` Implementation Plan — Go WebTransport Relay

## Progress (updated 2026-07-09)

| Chunk | Status | Notes |
|-------|--------|-------|
| A1 scaffold/config/main | ✅ done | |
| A2 tlsutil | ✅ done | hash fixtures verified against openssl |
| A3 gawk-devcert CLI | ✅ done | |
| A4 transport /echo + E2E | ✅ done | 3 library gotchas — see `02-webtransport-hello.md` |
| A5 manual browser verify | ⬜ pending | blocked in WSL2 (UDP/Chrome issue — see `02-webtransport-hello.md`); retry after move to native dev |
| B1 wire format | ✅ done | done early (no deps); golden vectors in `wire_test.go` |
| B2 hub | ⬜ next | |
| B3 publish/subscribe routes | ⬜ | |
| B4 frontend transport (TS) | ⬜ | |
| C1–C3 fan-out | ⬜ | |
| D1–D3 resilience + deploy | ⬜ | |

Note for implementers: webtransport-go v0.11.1 bumped the `go` directive to
1.25 (toolchain auto-downloads). The A4 chunk hit three runtime-only library
pitfalls (manual ALPN config, `ConfigureHTTP3Server`, client
`EnableStreamResetPartialDelivery`) — read the gotchas section of
`02-webtransport-hello.md` before touching `internal/transport`.

## Context

Build-order step 2 (WebTransport hello-world, nail TLS) is next; no server code exists yet. This plan scaffolds `gawk-server/` so it grows into the production relay (steps 3–5) with nothing thrown away, and decomposes the work into small, isolated, fully-specified chunks that can be delegated independently. All architectural decisions (package layout, interfaces, wire format) are made **here** so each chunk is mechanical to implement.

TLS decisions already made: prod = cert-manager/Let's Encrypt cert files mounted into the pod (with lightweight reload); local dev = self-signed ECDSA cert ≤14 days + Chrome flags / `serverCertificateHashes`.

## Locked decisions

- **Module**: `github.com/Tuhis/gawk/gawk-server`, Go 1.23.
- **Dependencies**: `github.com/quic-go/webtransport-go` (+ transitive) only. Stdlib for everything else: `flag` + env fallback (no viper), `log/slog`, `crypto/*`, `testing` (no testify).
- **Relay is a byte forwarder**: server parses datagram headers only to observe (cache keyframes/config, count); it forwards publisher datagrams verbatim. One wire format on both legs → exactly one TS mirror later.
- **Package boundaries**: bytes in `wire`, pub/sub + caches in `hub`, networking in `transport`, certs in `tlsutil`. Each chunk touches one package.

## Directory layout

```
gawk-server/
  go.mod, go.sum, README.md
  cmd/
    gawk-server/main.go            # flags → config → wiring → run, signal handling
    gawk-devcert/main.go           # dev cert generator CLI
  internal/
    config/config.go               # Config struct, ParseFlags(args, getenv) — pure, testable
    tlsutil/devcert.go, reload.go
    wire/wire.go, wire_test.go     # pure encode/parse + golden vectors
    hub/hub.go, hub_test.go        # Hub, Publisher, Subscriber, caches, drop policy
    transport/server.go, session.go
  deploy/                          # Milestone D only: Dockerfile, k8s/
```

## Config, logging, shutdown

```go
type Config struct {
    Addr           string     // ":4433" (UDP)
    CertFile       string     // "" in dev-cert mode
    KeyFile        string
    DevCert        bool       // generate in-memory ephemeral cert, log hashes
    DevCertHosts   string     // "localhost,127.0.0.1"
    LogLevel       slog.Level
    LogFormat      string     // "text"|"json", default text
    MaxSubscribers int        // default 15
    AllowedOrigins []string   // empty = allow all (dev); checked on CONNECT
}
func ParseFlags(args []string, getenv func(string) string) (Config, error)
```

Every flag has an env fallback (`GAWK_ADDR`, `GAWK_CERT_FILE`, …) via a 5-line `envOr` helper. `main.go`: `signal.NotifyContext(..., os.Interrupt, syscall.SIGTERM)` → `transport.Server.Run(ctx)`; on cancel, close sessions and exit 0.

## TLS (`internal/tlsutil`)

**Prod reload** — per-handshake mtime check with 30s stat throttle, no fsnotify:

```go
func NewReloader(certPath, keyPath string, log *slog.Logger) (*Reloader, error) // fails fast on bad initial load
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
```

On mtime change: `tls.LoadX509KeyPair` and swap; keep old cert on reload error (log warning). Handles cert-manager ~60-day renewals with no pod restart.

**Dev cert** (Chromium hard requirements: ECDSA P-256, validity ≤ 14 days):

```go
func GenerateDevCert(hosts []string, validity time.Duration) (tls.Certificate, error) // caps at 14*24h
func SPKIFingerprint(cert *x509.Certificate) string  // base64(SHA-256(SPKI)) → --ignore-certificate-errors-spki-list
func CertHashHex(cert *x509.Certificate) string      // hex(SHA-256(cert DER)) → serverCertificateHashes
```

`cmd/gawk-devcert`: `-out dir -hosts ... -days 13`; writes `cert.pem`/`key.pem`, prints SPKI hash, cert hash, ready-to-paste Chrome launch line, and a JS `serverCertificateHashes` snippet. Server's `-dev-cert` mode reuses `GenerateDevCert` in-memory and logs the same hashes.

## Wire format (`internal/wire`) — frozen contract

All integers **big-endian** (natural for TS `DataView`). Common 2-byte prefix: `byte0 version=0x01` (unknown → drop+count), `byte1 type` (0x01 VideoChunk, 0x02 DecoderConfig).

**0x01 VideoChunk** (20-byte header, then payload ≤ `MaxChunkPayload`):

```
byte 2      uint8   flags        bit0 = keyframe
byte 3      uint8   reserved
bytes 4-7   uint32  frameID      monotonic per publisher session, starts 0
bytes 8-9   uint16  chunkIndex   0-based
bytes 10-11 uint16  chunkCount   explicit count (enables prealloc + completeness under reorder/loss)
bytes 12-19 uint64  timestampUs  EncodedVideoChunk.timestamp (decoder needs it; carries E2E latency)
```

**0x02 DecoderConfig** (always fits one datagram): `byte2 reserved`, `byte3 codecLen`, codec string ASCII, then extradata (AVCC bytes; may be empty for VP8/VP9).

Constants: `MaxDatagramSize = 1200`, `VideoChunkHeaderSize = 20`, `MaxChunkPayload = 1180`. Sizing: keyframes ~50–150 KB → ~45–130 chunks, uint16 has huge headroom.

**Loss-tolerant config delivery** (the late-joiner story): broadcaster sends DecoderConfig immediately before every keyframe. Server caches latest config and independently re-emits it before each relayed keyframe and on subscriber join. Viewers treat duplicate configs as idempotent (reconfigure only if bytes differ). No ACKs, no reliable streams.

API (pure functions):

```go
func PeekType(dgram []byte) (version, msgType uint8, err error)
func AppendVideoChunk(dst []byte, h VideoChunkHeader, payload []byte) ([]byte, error)
func ParseVideoChunk(dgram []byte) (h VideoChunkHeader, payload []byte, err error) // payload aliases dgram
func AppendDecoderConfig(dst []byte, c DecoderConfig) ([]byte, error)
func ParseDecoderConfig(dgram []byte) (DecoderConfig, error)
```

Tests include **golden hex vectors** committed in `wire_test.go`, later copy-pasted into the TS mirror's tests — the portability guarantee.

## Hub (`internal/hub`)

```go
type DatagramSender interface { SendDatagram(payload []byte) error } // all hub needs from a session

type Options struct { MaxSubscribers int; QueueDepth int /* 256 */ }

func New(log *slog.Logger, opts Options) *Hub
func (h *Hub) StartPublish() (*Publisher, error)               // ErrPublisherActive
func (h *Hub) Subscribe(s DatagramSender) (*Subscriber, error) // ErrFull
func (h *Hub) Stats() Stats
func (p *Publisher) HandleDatagram(dgram []byte)  // parse → cache → fan out verbatim
func (p *Publisher) Close()                        // frees slot; caches persist
func (s *Subscriber) Close()
```

Semantics (spec'd so implementers invent nothing):
- `HandleDatagram` owns the slice; fan-out shares the same immutable slice across subscribers.
- **Config cache**: any 0x02 datagram replaces `cachedConfig`.
- **Keyframe cache**: keyframe chunk with new frameID starts assembly (bitset + ordered `[][]byte`); on completion, atomic swap into `cachedKeyframe`. Incomplete assemblies abandoned when a newer frame starts; previous complete keyframe stays.
- **Drop policy**: each Subscriber owns a `chan []byte` (cap QueueDepth) drained by one goroutine calling `SendDatagram`. Non-blocking enqueue; full queue → drop + increment `sub.dropped`. Slow subscriber never blocks publisher or peers.
- **Priming**: `Subscribe`, under hub lock, enqueues `cachedConfig` then all `cachedKeyframe` chunks in order *before* adding the sub to the live map.
- **Config-before-keyframe**: when relaying chunk 0 of a keyframe, enqueue `cachedConfig` first for every subscriber.

Tests use a `fakeSender` (records slices; optional blocking channel to simulate a stuck peer) — no network.

## Transport (`internal/transport`)

```go
func New(cfg config.Config, h *hub.Hub, getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error), log *slog.Logger) *Server
func (s *Server) Run(ctx context.Context) error
```

- Routes (`http.ServeMux` on the h3 server):
  - `CONNECT /publish` — `StartPublish()`; `ErrPublisherActive` → 409 before Upgrade. Loop `ReceiveDatagram` → `HandleDatagram`; session end → `publisher.Close()`.
  - `CONNECT /subscribe` — `Subscribe(session)`; `ErrFull` → 429. Inbound datagrams read-and-discarded (reserved for future control messages).
  - `CONNECT /echo` — Milestone A diagnostic; kept permanently for connectivity triage.
  - `GET /healthz` — 200 "ok".
- `quic.Config{EnableDatagrams: true, MaxIdleTimeout: 30s}`; `tls.Config{GetCertificate: getCert}`.
- Origin check via webtransport-go `CheckOrigin` when `AllowedOrigins` non-empty.
- Auth out of scope; later it's a token query param on the CONNECT URL (JS can't set headers) — routing already accommodates this.
- `webtransport.Session` satisfies `hub.DatagramSender` structurally.

## Milestones → delegable chunks

Each chunk = one package, PR-sized, with the spec above as its contract.

### Milestone A — build step 2: hello-world (TLS nailed)

| # | Chunk | Deps | Acceptance criteria |
|---|-------|------|---------------------|
| A1 | Scaffold: `go.mod`, `internal/config`, `cmd/gawk-server` skeleton (config→slog→wait for signal→clean exit), README | — | `go build ./... && go vet ./...` clean; config tests cover flag/env precedence + bad log-level; binary exits 0 on SIGINT |
| A2 | `tlsutil`: `GenerateDevCert`, `SPKIFingerprint`, `CertHashHex`, `Reloader` | A1 | Tests: cert is ECDSA P-256, ≤14d, hosts in SANs; hashes match openssl-derived fixtures; Reloader swaps v1→v2 on mtime change (inject `now` clock), keeps v1 on corrupt reload |
| A3 | `cmd/gawk-devcert` CLI | A2 | Produces loadable pem pair; stdout has SPKI b64, cert-hash hex, full Chrome flag line, JS snippet |
| A4 | `transport` minimal: `/healthz` + `/echo`, `Run(ctx)` lifecycle, dev-cert + file-cert paths wired in main | A1, A2 | **Automated Go E2E, no browser**: test starts server on random port with in-memory dev cert, `webtransport.Dial`s `/echo`, datagram round-trips within 1s; clean shutdown on ctx cancel |
| A5 | Manual browser verify + `docs/02-webtransport-hello.md` | A3, A4 | Chrome with printed flags (or devtools `new WebTransport(url, {serverCertificateHashes})`) connects, echo round-trips; gotchas recorded |

Nothing in A is throwaway.

### Milestone B — build step 3: single-client end-to-end

| # | Chunk | Deps | Acceptance criteria |
|---|-------|------|---------------------|
| B1 | `internal/wire` complete | A1 | Round-trip tests both types; golden hex vectors; error cases (short dgram, bad version, index ≥ count, oversized payload, codecLen overrun); fuzz test (`testing.F`) — parse never panics |
| B2 | `internal/hub` core: caches, priming, per-sub queue+drop | B1 | Fake-sender tests: verbatim forwarding; 2nd publisher → ErrPublisherActive; late sub gets [config, kf chunks 0..n-1] before live data; incomplete keyframe never cached; blocked sender drops while healthy peer gets 100%; `-race` clean |
| B3 | Wire `/publish` + `/subscribe` in transport; 409/429; session cleanup | B2, A4 | Go integration test: synthetic chunked frames relay pub→sub, ≥95% frames intact on loopback; 2nd publisher gets 409; disconnect frees slot |
| B4 | Frontend touchpoint (noted, not planned here): `gawk-app/src/transport/` TS mirror of `wire` (DataView big-endian, golden vectors from B1), `serverCertificateHashes` plumbing, Broadcaster/Viewer pipeline split | B3 | Out of server scope; §Wire format is the interface |

### Milestone C — build step 4: fan-out

| # | Chunk | Deps | Acceptance criteria |
|---|-------|------|---------------------|
| C1 | Hub hardening: MaxSubscribers → ErrFull, churn under load, per-sub drop counters | B2 | 16th subscribe rejected; 15 fakes with one blocked over 5s synthetic stream — unblocked subs get 100%; `-race` clean |
| C2 | Config-before-keyframe re-emission + cache replacement on publisher restart (new session → frameIDs reset) | C1 | Config datagram precedes kf chunk 0 for every keyframe; reconnect with new config primes joiners with the new config |
| C3 | `hub.Stats()` + `GET /statusz` JSON (sub count, drops, cached kf id/size, frames relayed) | C1 | Reachable in integration test; numbers move |

### Milestone D — build step 5: resilience + deployment

| # | Chunk | Deps | Acceptance criteria |
|---|-------|------|---------------------|
| D1 | Publisher-gone handling (subs stay connected, re-primed when publisher returns), idle timeout/keepalive tuning | B3 | Integration test: kill publisher, viewers stay; new publisher resumes flow |
| D2 | `deploy/Dockerfile`: multi-stage, CGO_ENABLED=0, distroless/static, non-root | A4 | `docker build` + `docker run -p 4433:4433/udp` passes the A4 echo test from host |
| D3 | `deploy/k8s/`: Deployment (cert Secret mounted, `GAWK_CERT_FILE=/tls/tls.crt`), Service (UDP), cert-manager Certificate | D2 | `kubectl apply --dry-run=server` clean; Reloader means renewals need no restart. Note: k8s HTTP probes can't speak h3 — use exec/startup probe |

Deployment lands after fan-out works locally (when a friend first tests). Each milestone close-out: write `docs/0N-*.md` per repo convention. Forced-keyframe cadence is broadcaster-side (`keyframeIntervalFrames` already exists in gawk-app); server-side resilience = D1 + C2 semantics.

## Verification

- **Unit** (no network): config, tlsutil (temp-dir certs, injected clock), wire (golden vectors + fuzz), hub (fake senders, always `-race`).
- **Automated E2E** (network, no browser): Go `webtransport.Dial` clients in transport tests — echo (A4), relay (B3), multi-sub slow-peer (C1). Exists from Milestone A onward.
- **Manual browser**: once per milestone — A5 echo via devtools + Chrome flags; B real broadcaster→viewer; C multiple viewer tabs.

## Risks / watch-items

- webtransport-go needs `EnableDatagrams` on quic.Config *and* h3 datagram negotiation — A4 proves it before any media code.
- Chrome ≤14-day/ECDSA constraints apply to `serverCertificateHashes`; `--ignore-certificate-errors-spki-list` is laxer — dev tooling emits both hashes so either flow works.
- Effective safe payload depends on path MTU; `MaxDatagramSize` is one constant in `wire` — tunable without a format change.
- `frameID` resets on publisher restart — C2 specifies cache replacement so stale keyframes never prime joiners.
