# v0.3 — Single-client end-to-end: hub + publish/subscribe relay (Milestone B)

Status (2026-07-10): **Milestone B complete.** Server-side relay is complete
and integration-tested; the frontend has broadcast and view pages wired to
`/publish` and `/subscribe`. Manual browser verification passed end-to-end
(Chrome inside WSL2): broadcast → relay → view shows live picture, with relay
priming on join. One bug surfaced and was fixed during verification — the
viewer decoder needed a HW→software fallback (see gotchas below).

## What was built

### B2 — `gawk-server/internal/hub`

Pub/sub core exactly per the spec in `implementation-tasks.md`:

- **Single publisher slot**: `StartPublish()` returns `ErrPublisherActive`
  while taken; `Publisher.Close()` frees it. Caches persist across publisher
  sessions so late joiners can still be primed while the broadcaster is away.
- **Byte forwarder**: `HandleDatagram` parses headers only to observe;
  datagrams are fanned out verbatim, sharing one immutable slice across all
  subscriber queues. Malformed/unknown datagrams are dropped and counted
  (`Stats().BadDatagrams`), never forwarded.
- **Caches**: any DecoderConfig datagram replaces `cachedConfig`. Keyframe
  chunks are reassembled (per-frame `[][]byte`, duplicate- and
  reorder-tolerant); only a *complete* keyframe is swapped into the cache.
  An incomplete assembly is abandoned when a keyframe with a different
  frameID starts; the previously cached complete keyframe stays.
- **Drop policy**: each subscriber owns a bounded `chan []byte` (default
  depth 256) drained by its own goroutine calling `SendDatagram`. Enqueue is
  non-blocking: full queue → drop + count. A stuck subscriber never blocks
  the publisher or other subscribers (tested with a blocking fake sender).
- **Priming**: `Subscribe` enqueues, under the hub lock, the cached config
  then the cached keyframe's chunks in order *before* adding the subscriber
  to the live fan-out — so the primed data always precedes live datagrams.
- **Config-before-keyframe**: chunk 0 of every relayed keyframe is preceded
  by a re-emission of the cached config to all subscribers. Viewers must
  treat duplicate configs as idempotent (reconfigure only if bytes differ).

Locking model: one hub mutex guards the subscriber map, caches and counters.
All enqueues happen under it, and `Subscriber.Close` removes + closes the
queue under the same lock, which is what makes enqueue-after-close (a panic)
impossible. `Close` then waits for the drain goroutine to flush and exit.

### B3 — `/publish` and `/subscribe` routes (`internal/transport`)

- `CONNECT /publish` — claims the publisher slot **before** upgrading, so a
  second publisher gets a plain HTTP/3 **409** response. Read loop feeds
  `ReceiveDatagram` → `HandleDatagram`; session end frees the slot.
- `CONNECT /subscribe` — rejects with **429** before upgrade when the hub is
  full (`hub.Full()` pre-check; `Subscribe` remains authoritative for the
  post-upgrade race). Inbound datagrams are read and discarded — reserved
  for future control messages; the loop exists to notice session end and
  `Close` the subscriber.
- `transport.New` now takes the `*hub.Hub` (wired in `cmd/gawk-server`).
- `*webtransport.Session` satisfies `hub.DatagramSender` structurally, as
  planned — no adapter needed.

Integration tests (real QUIC over loopback, no browser): 40 synthetic
3-chunk frames relay pub→sub with ≥95% completeness; late joiner receives
config + full cached keyframe with no publisher traffic after its join;
409/429 rejections; publisher disconnect frees the slot.

### B4 — frontend transport (`gawk-app/src/transport/`)

- **`wire.ts`** — DataView-based mirror of `internal/wire`: same constants,
  same validation rules, parse-never-copies (payload/extradata are
  `subarray` views). The golden hex vectors from `wire_test.go` are copied
  verbatim into `wire.test.ts` (vitest, `npm test`) — the cross-language
  byte-compatibility guarantee. `timestampUs` is a `bigint` (uint64 doesn't
  fit a JS number).
- **`packetizer.ts`** — pure: one encoded frame → ≤1180-byte VideoChunk
  datagrams (explicit chunkCount), plus the DecoderConfig datagram builder.
- **`reassembler.ts`** — pure: datagrams → complete frames + config events.
  Policy (favor drops over stalls): emit only complete frames; at most 8
  concurrent assemblies, oldest evicted under pressure; late-completing
  *delta* frames are dropped, but keyframes are always emitted — that both
  tolerates reorder and makes broadcaster restarts (frameIDs reset to 0)
  recover; duplicate configs deduplicated by byte equality. Unit-tested for
  loss/reorder/duplicate/forged-count cases without any network.
- **`connection.ts`** — WebTransport connect (with `serverCertificateHashes`
  from a pasted dev-cert hash, `requireUnreliable`, `congestionControl:
  'low-latency'`), a datagram read loop, and a write chain that serializes
  sends.
- **`broadcaster.ts` / `viewer.ts`** — the loopback pipeline split in half
  with the network in the middle. The broadcaster connects *before*
  prompting for screen capture (a 409 shouldn't show the share picker),
  re-sends the config datagram before every keyframe, and packetizes with a
  session-monotonic frameID. The viewer chains decoder ops so configure
  completes before decodes, waits for a keyframe after every (re)configure
  (WebCodecs requirement), and treats the relay's priming (config + cached
  keyframe on join) as ordinary traffic — first picture without waiting for
  the next keyframe.
  **Update (2026-07-14, freeze-on-gap — CLAUDE.md build-order item 11):** the
  viewer also tracks the last frameId and, when a *delta* arrives with a
  non-contiguous frameId (an intervening frame was lost/dropped-incomplete
  upstream), waits for the next keyframe instead of decoding it. Decoding a
  delta whose reference never arrived renders visible corruption until the
  next keyframe; holding the last good frame turns that corruption into a
  brief freeze (kept short by the 500 ms GOP — see [docs/08](08-resolution-framerate-picker.md)).
  Gap-induced discards surface on the existing "Awaiting keyframe" stat.
- **UI**: hash-routed pages — `#/broadcast`, `#/view` (default), `#/loopback`
  (kept as a diagnostic). Server URL + dev-cert hash inputs persist in
  localStorage (`src/state/transportStore.ts`).

## Gotchas hit in this milestone

- **`go test -race` needs cgo**: this environment defaults `CGO_ENABLED=0`;
  run `CGO_ENABLED=1 go test -race ./...`.
- **A hot publisher outruns even a healthy drain goroutine.** With a small
  queue depth, enqueueing in a tight loop drops datagrams for a *healthy*
  subscriber too — the drain goroutine simply hasn't been scheduled. Not a
  bug (60fps pacing is glacial by comparison; default depth 256 is plenty),
  but tests asserting "healthy peer gets 100%" must pace the publisher.
  Corollary: `QueueDepth` must exceed a keyframe's chunk count (~45–130)
  or priming itself would drop chunks.
- **npm optional-dependency bug (WSL2)**: `npm install` silently skips the
  platform-native bindings for rolldown (vite 8) and oxlint
  ([npm/cli#4828](https://github.com/npm/cli/issues/4828)); build/lint/test
  then fail with "Cannot find native binding", and the documented remedy
  (delete `node_modules` + lockfile, reinstall) does **not** fix it here.
  Workaround: `npm install @rolldown/binding-linux-x64-gnu @oxlint/binding-linux-x64-gnu --no-save`.
  May need repeating after a fresh clone/install.
- **The decoder needs the same HW→software fallback the encoder has.** The
  viewer's `VideoDecoder` originally forced `hardwareAcceleration:
  'prefer-hardware'` and threw on the first `isConfigSupported` miss. Inside
  WSL2 (software Chrome, no GPU) there is no hardware H.264 decoder, so it
  failed with `Decoder config unsupported: avc1.42E02A @ undefinedxundefined`
  while the broadcaster's encoder worked fine — the encoder already cascades
  down to software (see `media/encoder.ts`). `media/decoder.ts` now probes
  prefer-hardware first, then falls back to no hint (software). The
  `undefinedxundefined` was a red herring: the wire `DecoderConfig` carries no
  dimensions (Chrome derives them from the AVCC `description`), so those fields
  are always absent.
- **quic-go's outgoing datagram queue is finite**: blasting datagrams
  faster than the connection sends them can surface as send errors/loss
  even on loopback. The relay integration test paces frames ~2ms apart
  (still far faster than real 60fps traffic).

## Manual verification (closes milestone B)

Chrome must run **inside WSL2** alongside the server (Windows Chrome → WSL2
NAT UDP is broken; see `02-webtransport-hello.md`).

1. `cd gawk-server && go run ./cmd/gawk-server -dev-cert` — copy the
   `cert_hash_hex` value from the startup log.
2. `cd gawk-app && npm run dev` — open `http://localhost:5173/#/broadcast`
   (localhost is a secure context, so plain http is fine for the app; the
   WebTransport connection itself is QUIC/TLS to `:4433`).
3. Paste the cert hash into "Dev cert hash", Start Broadcast, pick a screen.
   No Chrome flags needed — the app passes the hash via
   `serverCertificateHashes`.
4. Open `http://localhost:5173/#/view` in another tab (or another Chrome
   profile/machine), same cert hash, Start Watching. Expect the picture to
   appear immediately (relay priming) and the viewer stats to move.
5. Worth poking at: stop/restart the broadcaster mid-view (viewer should
   recover at the next config+keyframe), a second broadcaster tab (should
   fail — 409), and viewer join long after broadcast start (primed keyframe).

Note: the dev-cert hash changes on every server restart — re-paste it after
restarting `gawk-server`. The inputs persist in localStorage otherwise.
