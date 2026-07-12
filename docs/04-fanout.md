# v0.4 — Fan-out: multi-subscriber hardening, restart-safe caches, /statusz (Milestone C)

Status (2026-07-12): **Milestone C complete.** Implemented and fully covered
by automated tests (unit + network integration, `-race` clean); manual
multi-viewer browser verify passed 2026-07-12 (Chrome inside WSL2): 3+
viewer tabs primed instantly and stayed in sync, viewer churn left peers
unaffected, broadcaster restart recovered all viewers at the new session's
first keyframe with no stale-frame flash, and `/statusz` tracked it all
live — subscriber count followed tabs, cache invalidation on restart was
visible (`hasConfig` false / keyframe fields zeroed, then `cachedKeyframeId`
restarting near 0), with zero dropped and zero bad datagrams throughout.

## What was built

### C1 — hub hardening (`gawk-server/internal/hub`)

The B2 hub already carried the C1 machinery (`ErrFull` on `Subscribe`, the
`Full()` pre-upgrade check, per-subscriber atomic drop counters), so C1
landed as acceptance-scale tests rather than new runtime code:

- **16th subscriber rejected** at the default `MaxSubscribers` of 15.
- **Full house with one stuck peer**
  (`TestFifteenSubscribersOneBlocked`): 15 subscribers, one blocked
  mid-send, receiving a ~60fps chunked synthetic stream for 5 seconds
  (~940 datagrams incl. config re-emission before every keyframe). Every
  unblocked subscriber received **100% of datagrams, in exact order, zero
  drops**; the blocked one dropped everything beyond its queue plus the one
  in-flight send, and its drops surface in `Stats()`. `-short` runs a 300ms
  variant.
- **Churn**: the concurrent join/leave test now runs with `MaxSubscribers`
  below the churner count, so the `ErrFull` path races against `Close` and
  the publisher under `-race`.

### C2 — restart-safe caches + config-before-keyframe

The config-before-keyframe re-emission itself shipped with B2. C2 adds the
restart semantics and pins both behaviors with tests:

- **A new publisher session invalidates both caches** (in `StartPublish`,
  under the hub lock). Rationale: frameIDs restart at 0 per session and the
  config may differ (codec renegotiation), so a keyframe or config cached
  from an older session must never prime a joiner once a newer session
  exists. The caches still **persist while no publisher is active** — the
  "viewer joins while the broadcaster is away and sees the last picture"
  behavior is unchanged.
- The window where this matters — new session started but its first
  keyframe not yet complete — primes joiners with *nothing*; they render at
  the session's next config+keyframe (which the broadcaster emits together).
  That beats decoding new deltas against a stale keyframe.
- No frontend change needed: the viewer's reassembler already tolerates
  frameID resets (keyframes are always emitted, see `docs/03`).

Tests:

- `hub`: `TestPublisherRestartResetsCaches` (away-priming still works →
  restart clears both caches → early joiner gets live-only, later joiner
  primed with new-session data, all exact-order);
  `TestConfigPrecedesEveryKeyframe` (the *current* config immediately
  precedes chunk 0 of every keyframe, including after a mid-stream config
  replacement).
- `transport` (real QUIC over loopback):
  `TestPublisherRestartPrimesWithNewConfig` — publisher streams codec A with
  frameID 7, disconnects, reconnects with codec B and frameID 0; a late
  joiner's first primed config is codec B and the primed keyframe is the new
  session's; **any** chunk of frame 7 arriving fails the test.

### C3 — `hub.Stats()` + `GET /statusz` (`internal/transport`)

- `hub.Stats` now carries json tags (lowerCamel) — the struct is the
  response shape, so the Go and any future TS reader can share field names.
- `GET /statusz` (HTTP/3, same mux as `/healthz`) returns the snapshot:
  `publisherActive`, `subscribers`, `framesRelayed`, `datagramsRelayed`,
  `datagramsDropped` (live + accumulated from closed subscribers),
  `badDatagrams`, `hasConfig`, `cachedKeyframeId`, `cachedKeyframeChunks`,
  `cachedKeyframeBytes`.
- `TestStatuszReachableAndNumbersMove` fetches it with an `http3.Transport`
  client: zeroed before any activity; after a subscriber + publisher +
  keyframe/delta traffic it shows the correct counts (decoded back into
  `hub.Stats`, which also proves the tag round-trip).

Handy during manual testing — `/statusz` is HTTP/3-only (the server has no
TCP listener), and distro curl (incl. Ubuntu's) is built without HTTP/3.
What actually works:

```sh
# h3-capable curl (e.g. a static build from github.com/stunnel/static-curl):
watch -n1 'curl3 -s --http3-only --insecure https://localhost:4433/statusz'
```

or a ~25-line quic-go probe (what the close-out verify used): an
`http3.Transport` with `InsecureSkipVerify` + `http.Client.Get` — see
`TestStatuszReachableAndNumbersMove` in `internal/transport/server_test.go`
for the pattern. A browser `fetch` does *not* work against the dev cert
(`serverCertificateHashes` only applies to WebTransport, not fetch).

## Gotchas hit in this milestone

Nothing new — no library surprises this time. The two standing ones that
apply here: `-race` needs `CGO_ENABLED=1`, and tests that assert "healthy
subscriber gets 100%" must pace the publisher (see `docs/03`).

## Manual verification (closed milestone C — passed 2026-07-12)

Same setup as milestone B (`docs/03` — Chrome **inside** WSL2, dev-cert hash
pasted into the app):

1. `go run ./cmd/gawk-server -dev-cert`, `npm run dev`, start a broadcast
   (`#/broadcast`).
2. Open **several** `#/view` tabs (3+; different windows/profiles count as
   different subscribers). All should show the picture immediately (each is
   primed independently) and stay in sync.
3. Close a viewer tab mid-stream — the others must be unaffected; the server
   log line for the ended session reports its drop count.
4. Stop the broadcast and start it again (same tab or a fresh one) while
   viewers stay open — viewers should recover at the new session's first
   config+keyframe. A viewer that *joins* between restart and first keyframe
   should show black/nothing, then the picture at the first keyframe (that's
   the C2 cache invalidation working — no stale frame flash).
5. Watch `/statusz` while doing all of the above: `subscribers` tracks tabs,
   `framesRelayed`/`datagramsRelayed` climb, `cachedKeyframeId` resets with
   the new session, `datagramsDropped` stays ~0 on loopback.
