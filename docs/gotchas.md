# Gotchas

Hard-won lessons, each of which cost real debugging time. This file is the
consolidated list; the details live in the linked design docs, and the rule
in [CLAUDE.md](../CLAUDE.md) is that a gotcha has exactly one home — here —
rather than being restated in the doc that discovered it.

It lived in `README.md` until 2026-08-01, where it had grown to 937 of that
file's 1,143 lines and buried everything a newcomer needed. Nothing was
dropped in the move.

Add to it when a new gotcha lands in `docs/`.

---

**Environment**

- **WSL2: Chrome must run *inside* WSL2** alongside the server. Windows
  Chrome cannot complete a QUIC handshake to a server in WSL2 NAT mode
  (localhost forwarding is TCP-only; via the NAT IP Chrome idle-times-out
  even though CLI clients work). ([docs/02](02-webtransport-hello.md))
- **Point a browser at the relay by IP literal, not `localhost`, whenever the
  relay is in a container.** `localhost` resolves to `::1` first on macOS and
  most Linux desktops, Docker publishes a UDP port on IPv4 only, and a QUIC
  dial gets none of the happy-eyeballs fallback that hides this for the app's
  TCP port — the browser reports a bare "Opening handshake failed" and the
  relay logs nothing at all, because nothing reached it. The compose stack
  therefore renders `relayUrl: "https://127.0.0.1:4433"`.
  ([docs/41](41-local-dev-stack.md) §4.7)
- **Colima does not forward UDP with its default port forwarder**, so a
  published QUIC port silently swallows every packet (`docker port` shows the
  mapping; nothing arrives). Its forwarder is SSH, which is TCP-only. Fix:
  `colima start --port-forwarder grpc`. Docker Desktop's userland proxy
  handles UDP as-is. ([docs/41](41-local-dev-stack.md) §4.7)
- **The `alpine` base image has no `openssl` binary** (verified on 3.21) —
  only the libraries. `dev/config-gen.sh` computes the certificate's DER hash
  with busybox instead (PEM is base64-wrapped DER), because installing
  openssl would need the network in the one lane whose claim is that it works
  offline. ([docs/41](41-local-dev-stack.md) §4.7)
- **`go test -race` needs `CGO_ENABLED=1`** — this environment defaults to 0.
- **`gawk-telemetry` builds cgo-free by default and its SQL engine does not
  exist in that build.** `internal/sqlengine` sits behind `//go:build duckdb`;
  `go build ./...` on a fresh clone links no DuckDB and the console reports
  itself unavailable, while the shipped image is `CGO_ENABLED=1 -tags duckdb`
  on `distroless/cc-debian13`. So `-query-sql` being on (its default) is *not*
  the same claim as "queries work here", and CI builds both configurations.
  ([docs/36](36-telemetry-ui-history.md) §8 Q1)
- **A cgo image that BUILDS is not an image that runs**, and nothing catches
  the difference at build time. The telemetry image shipped twice-broken —
  `distroless/base` has no libstdc++ for a C++ dependency, and a builder whose
  glibc is newer than the runtime's produces an unrunnable binary — both
  surfacing only as dynamic-linker errors on first start. The builder and base
  are now pinned to the same Debian release, and CI starts every image it
  builds. ([docs/36](36-telemetry-ui-history.md) §9.3)
- **`go test ./gawk-telemetry/internal/dashboard/` is green and proves nothing
  without `npm run build` first.** Its no-external-fetch tests assert against
  `dist/`, which the Go job never builds, so they *skip by design*. That is the
  test guarding the bundled-chart decision — run it deliberately after touching
  `gawk-telemetry/ui/src/charts/`.
  ([docs/36](36-telemetry-ui-history.md) §0.4)
- **`npx tsc --noEmit` in `gawk-app` passes vacuously** — the root
  `tsconfig.json` is solution-style (references only), so it checks nothing.
  `npm run build` (`tsc -b`) is the real typecheck; vitest strips types and
  won't catch type errors either. ([CODE-REVIEW.md](../CODE-REVIEW.md))
- **npm may silently skip native bindings** (rolldown/vite 8, oxlint) due to
  [npm/cli#4828](https://github.com/npm/cli/issues/4828); build/lint/test
  then die with "Cannot find native binding" and the documented
  delete-and-reinstall remedy does not help. Workaround:
  `npm install @rolldown/binding-linux-x64-gnu @oxlint/binding-linux-x64-gnu --no-save`.
  ([docs/03](03-single-client-e2e.md))
- **UDP buffers**: quic-go wants ~7 MiB receive buffers; Linux defaults to
  208 KiB. `sysctl -w net.core.rmem_max=7500000 net.core.wmem_max=7500000`
  for real streaming loads.
- **The production viewer (`#/view/<id>`) has no cert-hash field** — it is
  deliberately chrome-free (fullscreen + leave only). The local `docker
  compose` stack solves this by *configuring* the hash: `dev/config-gen.sh`
  renders `devCertHashHex` into `/config.js`, so the route works in a fresh
  profile, in incognito and on a second machine (R38, `docs/41` D4). Without
  that stack, set the hash once in the broadcaster's ⚙ panel — it persists to
  same-origin `localStorage`, which is why a fresh profile used to fail — or
  use `#/debug/view`, which keeps the full settings form.
  ([docs/10](10-production-ui.md), [docs/41](41-local-dev-stack.md))
- **The debug viewer uses `#/debug/view/<id>`, not `#/view/<id>`** — R6 gave
  `#/view/<id>` to the production viewer, and `ViewPage` syncs its code into the
  hash, so it must stay in its own namespace or clicking **Watch** bounces you
  into the production UI. Any "frozen" page that writes `location.hash` is not
  actually route-frozen. ([docs/10](10-production-ui.md))
- **A codec the viewer's browser can't decode is a *terminal* error, not a
  reconnect** — an unsupported codec (e.g. Chrome can't decode a Firefox
  broadcaster's `avc1.640034` High-5.2) fails every attempt identically, so the
  viewer shows a "can't play this stream" card and stops instead of looping the
  auto-reconnect. Decoder/codec failures are tagged `fatal` in
  `ViewerPipeline`; `ViewerSession` reconnects only on transport drops. (H.264
  `description` handling also matters: an Annex-B extradata blob must *not* be
  passed as the AVCC `description` — `viewer.ts` sniffs `avcC`'s `0x01` prefix.)
- **A host-target rustflag makes a cross build stop sharing with a host
  build**, and it looks like noise rather than a regression. Cargo does not
  apply `target.<triple>.rustflags` to host artifacts while cross-compiling,
  but does apply them when building *for* the host — so setting one
  workflow-wide in `gawk-broadcast-windows` CI made `cargo xwin clippy
  --target msvc` build all 107 proc-macro and build-script units bare and
  `cargo clippy` rebuild every one of them (367 units → 481, the step 2:02 →
  2:58). Scope such flags to the job that needs them.
  ([docs/38](38-windows-native-broadcaster.md) D18)
- **`producer | grep -q` under `set -o pipefail` fails on success** — and in CI
  shell that reads as the opposite of what happened. `grep -q` exits at its
  first match, the producer's next write gets EPIPE, and `pipefail` hands the
  pipeline the producer's failure. `tar -tzf … | grep -qx "$want"` therefore
  reported the file it had just *found* as missing from the release tarball,
  and published `gawk-broadcast-v1.11.0` with no assets; the same shape in an
  `if` (`echo "$out" | grep -q`) silently reads as "no match" instead. Use a
  here-string (`grep -q … <<<"$var"`) — no pipeline, no SIGPIPE. Producers that
  are read to the end (`grep -c`, `grep -B/-A`) are not exposed to it.

**Certificates (`serverCertificateHashes` rules — Chromium *and* Firefox)**

- Dev cert must be **ECDSA P-256** and its **total validity span ≤ 14 days,
  clock-skew backdate included** — a 14 d + 1 h cert is rejected with an
  opaque `CERTIFICATE_VERIFY_FAILED (certificate unknown)`. The Go probe
  won't catch this; only Chrome enforces it. ([docs/02](02-webtransport-hello.md))
- `-dev-cert` **alone** regenerates the certificate on every server start —
  the hash has to be re-pasted after each restart, and a stale one fails
  identically. It also means the viewer's **auto-reconnect can never heal a
  relay restart** in that mode. Since R38 the fix is a flag combination
  rather than a separate tool: `-dev-cert` **with** `-cert-file`/`-key-file`
  generates the pair once into those paths and loads it every time after,
  which is what `docker compose up` uses and what the host-binary inner loop
  should use too. (`gawk-devcert -out certs` still writes a pair by hand.)
  ([docs/05](05-resilience-deploy.md), [docs/41](41-local-dev-stack.md) §4.2.1)
- **`--ignore-certificate-errors` does not apply to WebTransport**, and
  neither does `--ignore-certificate-errors-spki-list`: Chrome's QUIC
  handshake still fails with `QUIC_TLS_CERTIFICATE_UNKNOWN`. The only two
  ways into a browser are `serverCertificateHashes` (ECDSA P-256, ≤ 14 d —
  **Firefox implements it too**, not just Chromium) and a **publicly**
  trusted certificate. ([docs/41](41-local-dev-stack.md) §6)
- **Installing a local CA does not help — in any browser.** `mkcert` and
  friends give you a certificate the browser trusts for ordinary TLS, so the
  page loads over HTTPS with no warning, and then the WebTransport dial to the
  relay is refused: Chrome fails with `ERR_QUIC_CERT_ROOT_NOT_KNOWN` (its QUIC
  proof verifier requires `is_issued_by_known_root`), and Firefox verifies the
  certificate successfully and closes the session anyway
  (`Http3Session::Authenticated … hasThirdPartyRoots=1,
  servCertHashesSucceeded=0` → `NS_ERROR_NET_RESET`). The symptom is a working
  UI that cannot connect, which reads like a relay bug and is not one. Measured
  in both engines on 2026-08-17; it is why R38 has no mkcert lane.
  ([docs/41](41-local-dev-stack.md) §6)

**webtransport-go (v0.11.x) — all fail at runtime, not compile time**

- ALPN must be added manually: wrap your TLS config in
  `http3.ConfigureTLSConfig(...)`, the library won't.
- WebTransport SETTINGS must be enabled manually:
  `webtransport.ConfigureHTTP3Server(s.H3)`, or clients reject with
  "server didn't enable WebTransport".
- Go *clients* need `EnableStreamResetPartialDelivery: true` in their
  `quic.Config`. ([docs/02](02-webtransport-hello.md))
- **Session close code hiding on datagram reads**: In both Go and JS, when a session is closed with a custom error code, reading from the datagram queue/channel returns only a generic `EOF` or channel-closed status. To retrieve the actual close code (e.g. `4000`), the client must listen to `wt.closed` (JS) or block on `AcceptStream` / `AcceptUniStream` (Go). ([docs/06](06-multi-broadcaster.md))
- **…and `wt.closed` can lose the settle-order race**: on a server close, the
  datagram read loop and the `wt.closed` promise settle in unspecified,
  browser-dependent order. If close semantics (like 4000 = "broadcast ended,
  don't reconnect") ride on `wt.closed` alone, a read-loop-first settle
  silently degrades them to an anonymous drop. On read-loop termination,
  race `wt.closed` with a short timeout before deciding
  (`transport/viewer-transport.ts`, `handleReadLoopEnd`). ([docs/06](06-multi-broadcaster.md))

**Observability (R9)**

- **`GAWK_METRICS_ADDR` cannot be disabled with an empty value** — the env
  helper treats empty as unset and falls back to the default `:2112`. The
  literal `off` disables; the Helm chart passes it when
  `metrics.enabled=false`. ([docs/13](13-observability.md))
- **client_golang rejects one metric family with two label sets** — you
  can't emit `gawk_x_total{broadcast=…}` and an unlabeled `gawk_x_total`
  from the same registry (gather fails with "inconsistent label
  dimensions"). Hence the `gawk_broadcast_*` / `gawk_relay_*` split.
  ([docs/13](13-observability.md))
- **`WebTransport.getStats()` currently exists in NO shipping browser** —
  Chromium removed its pre-spec implementation (absent in 150/151/152;
  spec-conformant rewrite "in development",
  [chromestatus](https://chromestatus.com/feature/5194440034746368)) and
  Firefox never had it, so the overlays' `connection.*` rows read `—`
  everywhere and per-leg attribution leans on relay-side counters. Every
  consumer must feature-detect and treat all fields as nullable
  (`transport/net-stats.ts` — spec-aligned, lights up on re-ship). The
  self-owned rows are immune: `RTT (time-sync)` and, since 2026-07-15,
  "Video bitrate (recv)"/"(sent)" — clients count their own video bytes
  (`videoBytesReceived`/`bytesSent`) instead of asking the transport.
  ([docs/13](13-observability.md))

**Cross-browser (Firefox, `docs/11`)**

- **Firefox negotiates a 1024-byte WebTransport datagram MTU, not 1200** — so
  the packetizer reads `wt.datagrams.maxDatagramSize` at runtime rather than
  assume the ~1200-byte ceiling; a datagram over the negotiated limit is
  silently dropped. ([docs/11](11-cross-browser-compatibility.md))
- **The browser's incoming datagram queue drops the OLDEST datagram, and it is
  shallow by default** — so a frame's back-to-back burst (its ~9-11 chunks,
  then its two parity symbols) loses its *head*, not its tail, before the read
  loop is ever scheduled. No reader can win that race, and it is invisible:
  nothing counts it on either side. A live Firefox 154 session lost **10.5 % of
  delta chunks and 0.05 % of parity** on one FIFO over one connection — which
  no network can do, and which is exactly the correlated multi-chunk loss a
  per-frame k=2 code cannot repair. `LocalViewerTransport` now raises
  `incomingMaxBufferedDatagrams` (Firefox ships only the older
  `incomingHighWaterMark`; Chromium is removing it, so both are tried) to 256
  and reports the result in the **`DatagramReceiveBuffer`** feature gate.
  Set it in the realm that owns the `WebTransport` — on the worker path the
  main thread has no handle on it. **The write is not the fix on Firefox, and
  this is measured, not inferred**: `incomingMaxBufferedDatagrams` is ABSENT
  there and only the legacy `incomingHighWaterMark` exists, defaulting to
  **1 datagram**. A paired A/B against a live broadcast
  (`e2e/firefox-datagram-buffer.mjs` — two subscribers, same seconds, hwm 1 vs
  8192, reader stalled to stress the queue) moves delivery by less than its own
  A/A noise floor, with the repeats disagreeing on direction: the attribute
  stores and reads back while dropping continues unchanged, so a readback proves
  storage, never effect. The same runs confirm the mechanism — stalling the
  reader changes loss by 0.01 points. The drop is not in the page at all — see
  the next entry.
  ([docs/34](34-live-edge-forward-parity.md))
- **Datagram loss is a function of ENCODED FRAME SIZE, not of the network being
  "bad"** — measured with `e2e/datagram-loss-profile.mjs`, which reconstructs
  each frame's arrival set from `chunkIndex`/`chunkCount` in the headers and so
  needs no source anchor. On a real link: frames of **≤ 8 chunks lost 0.00 %**
  (986 frames, not one datagram), while loss climbs monotonically past that —
  3.8 % at 11 chunks, 8.5 % at 18. The loss lands on the HEAD of each frame's
  burst (index 2 worst at 8.7 %, **zero from index 10 on**), which is why
  parity — written last — loses 0.00 %, and why the last chunk of a frame is
  effectively never lost. It is a path buffer ~8 packets deep evicting
  oldest-first, **below anything JavaScript can reach**: leg A is clean, the
  relay drops nothing, and neither the reader's speed nor the WebTransport
  buffer attribute moves it. Practical consequence: "lower the rung or the
  bitrate" would move frames under the threshold — but capping quality and
  pacing (which costs latency) are both **rejected**; the answer is R30's
  connection interleaving, the zero-latency way to shorten a burst. The
  bottleneck is **per-connection**: 4x the aggregate traffic cost ~10% more
  per-connection loss, so splitting a frame across transports puts every one of
  them under the threshold. Instruments:
  `e2e/datagram-loss-profile.mjs` and `e2e/datagram-connection-scaling.mjs`.
  ([docs/34](34-live-edge-forward-parity.md), [ROADMAP](../ROADMAP.md))
- **Firefox's `VideoEncoder` emits a malformed AVCC record** (bad reserved
  bits + a duplicated NALU-type byte); the viewer repairs it in
  `normalizeAvccExtradata` before configuring the decoder.
  ([docs/11](11-cross-browser-compatibility.md))
- **System audio is Chromium-only, and even there it needs the picker's
  checkbox** — the broadcaster always *requests* system audio (R15 graduated it
  to always-on), but the OS only shares it when the user ticks "Share audio" /
  "Share tab audio" in the share picker; there is no system-audio source on
  macOS/Linux screen shares (a **tab** share carries audio everywhere via
  internal mirroring — the reliable fallback), and Firefox lacks WebCodecs
  `AudioEncoder`/MSTP so it streams video-only. R24 surfaces this **browser-aware
  by feature detection** (`audioLaneSupported()`, never UA sniffing) as
  dismissible pre-start tips + reactive notes, and never gates the start path.
  A note **fires only where audio is achievable** — Firefox is told video-only
  up front, never nagged at runtime. ([docs/30](30-broadcaster-capture-audio-guidance.md))
- **A single-window share is the classic "frozen rectangle"** — it stops
  updating (or was never capturable) when the game goes exclusive-fullscreen or
  is alt-tabbed. Whole-screen is the recommended default for games; R24 nudges
  toward it whenever a `window` capture surface is detected
  (`displaySurface`, an advisory category only — never pipeline config).
  ([docs/30](30-broadcaster-capture-audio-guidance.md))

**Frontend UI**

- **A modal overlay must be portalled to `<body>`, never mounted where it is
  triggered.** `position: fixed` resolves against the nearest ancestor with a
  `transform`, `filter`, `backdrop-filter`, `perspective`, `contain` or
  `container-type` — not the viewport. The R37 server picker was opened from
  the landing chip, whose row is `position: absolute; transform:
  translateX(-50%)`, so the picker's full-screen scrim and centring layer
  collapsed to the chip's own ~78px box: a sliver of a panel pinned to the
  bottom of the page. Nothing in the panel's own CSS was wrong, and the two
  session-screen call sites rendered it correctly, which is what made it look
  like a panel bug rather than a mount-point bug. Portalled to `<body>` it also
  leaves every in-app stacking context, so it needs a z-index above all of them
  (the viewer's `.pseudoFullscreen` reaches 100), and the host must be
  `document.fullscreenElement ?? document.body` — the Fullscreen API paints
  only the fullscreen element's own subtree, and the viewer goes fullscreen on
  its root, so a `<body>` child would be invisible over a fullscreen stream.
  ([docs/40](40-relay-server-picker.md) §4.3)
- **A full-screen centring layer stacked over a scrim eats every
  "click outside".** The dismiss handler sat on the scrim, but the transparent
  centring layer above it covered the same area and had none, so no click ever
  reached the scrim. One element that both dims and centres — closing only when
  the click's `target` is the element itself — has no such gap. Pair it with an
  Escape handler; neither substitutes for the other.
  ([docs/40](40-relay-server-picker.md) §4.3)

- **A bare element rule in `global.css` outranks every CSS-module `:hover`.**
  `button:hover:not(:disabled)` scores (0,2,1) — higher than a module's
  `.row:hover` at (0,2,0) — so the legacy base-button background bled into
  every production surface whose hover rule did not restate `background`:
  server-list rows, the landing "Start a stream" link, the broadcaster's
  "Sharing tips", the viewer's unmute prompt. Each rendered near-white text on
  the light-blue accent, unreadable. The `<Button>` primitive looked fine
  purely by accident — its rules also use `:not(:disabled)`, so they tie-break
  above the global one, which is why the damage looked like scattered
  one-off bugs instead of one rule. Fix: wrap a legacy global element rule in
  `:where()` so it carries zero specificity and any single class beats it.
  Don't chase it by adding `background` to each module hover.
  ([docs/10](10-production-ui.md))
- **An exit-animation class must come *after* the base rule that animates the
  same element**, not merely exist. `.panel` and `.panelOut` both score
  (0,1,0), so source order alone picks the winner: declared above `.panel`,
  the picker's `.panelOut` lost and the panel vanished instantly while the
  backdrop faded out behind it. Nothing in the DOM looked wrong — the class
  was applied, the `closing` flag flipped, the element unmounted on schedule.
  The only tell is `getComputedStyle(el).animationName` still naming the
  *enter* animation mid-exit, which is worth sampling whenever an exit
  animation "does not play". ([docs/40](40-relay-server-picker.md) §4.3)

**Media pipeline**

- **A canvas player gets no free display wake lock — take one explicitly.**
  Browsers keep the screen awake only while an `HTMLMediaElement` plays video.
  Both gawk surfaces paint canvases instead (the viewer's WebCodecs → WebGL
  sink; the broadcaster's screen capture), so the OS sees an idle page and runs
  its normal idle timer: on macOS the display dims a few minutes in and then
  sleeps, mid-stream, **fullscreen included** — fullscreen has never controlled
  display sleep, so there is no fullscreen flag to look for. Both screens hold a
  `navigator.wakeLock` screen lock while live (`lib/useWakeLock.ts`). The trap
  when touching it: the UA **auto-releases the lock whenever the document is
  hidden and never re-acquires it**, so it must be re-requested on
  `visibilitychange` or one tab switch loses it for the rest of the stream.
- **Trust the actual `VideoFrame`, never `MediaStreamTrack.getSettings()`** —
  Chrome has been observed reporting one resolution while delivering frames
  at another. Configure encoders from the first real frame.
  ([docs/01](01-loopback-test.md))
- H.264 needs **even dimensions**; `getDisplayMedia` can hand out odd sizes
  on some DPI combos — round down to even before configuring the encoder.
- Constrain only `width` in `getDisplayMedia`; constraining both dimensions
  makes Chrome pillarbox ultrawide sources.
- WebCodecs: the **first chunk after `VideoDecoder.configure()` must be a
  keyframe**, and decoder configure/decode calls must be strictly ordered
  (promise-chain them).
- **A lost frame corrupts everything until the next keyframe — so the viewer
  freezes on a frameId gap.** Inter-frame coding means a delta whose reference
  was dropped decodes to visible garbage. Since R8 this freeze-on-gap policy
  lives in **one place** — the pure `transport/reorder-buffer.ts`, which merges
  reliable stream keyframes with datagram deltas by frameId and, on a
  non-contiguous *delta*, holds the last good frame (waits for the next
  keyframe) instead of decoding corruption. Consequences: the **500 ms GOP**
  (`keyframeIntervalMs`, cut from 2000) is what keeps that freeze short, and gap
  discards surface on the **"Awaiting keyframe"** / gap-resync stats — so those
  counters rising on Chrome during heavy motion is the fix working, not a
  decoder falling behind.
  ([docs/03](03-single-client-e2e.md), [docs/12](12-worker-and-reliable-keyframes.md))
- **Keyframes arrive hundreds of ms behind their own deltas — the reorder
  wait must cover *transfer* latency, not just reordering jitter (R10).**
  A ~236 KB keyframe is store-and-forwarded as one stream while its trailing
  deltas arrive instantly as datagrams; to a congested peer it was measured
  landing > 500 ms late. With `KEYFRAME_WAIT_MS` at 200 ms the held deltas
  expired first, so every GOP became keyframe-only ~2 fps playback — the
  telltale signature is **one gap resync per keyframe with zero reassembler
  drops**. The wait is now 1000 ms (bounded by `MAX_BUFFERED_FRAMES`).
  Relatedly, a subscriber whose keyframe stream *opens* fail persistently
  (exhausted stream credit — a zombie session that stopped reading streams)
  is now **evicted** after 10 consecutive failures with non-terminal close
  code 4001, so it reconnects if live and stops costing fan-out if dead.
  ([docs/14](14-viewer-render-performance.md))
- **An adaptive buffer can only be as deep as its estimator can measure.**
  R19's resilient playout controller clamps to `[150, 2000] ms`, but the
  arrival-jitter signal driving it came from a **fixed-range histogram**
  (500 ms, values past it clamp into the top bin), so the offset could never
  exceed **~534 ms** — the advertised upper half was dead, and the design doc
  had explicitly reasoned that the tracker "needs no changes". It survived
  design, implementation and a manual verification pass because every test
  fed the *controller* a synthetic jitter number instead of measuring one.
  The histogram's range and window are now part of `PlayoutProfile`, with a
  test asserting `quantileRangeMs >= maxMs` per profile. When a control law
  and its estimator are tuned in different files, check the estimator's
  dynamic range covers the law's envelope.
  ([docs/24](24-viewer-network-resilience.md))
- **A right-click menu is not a UI on a phone** — and a button that opens a
  popup needs two things jsdom will never tell you about. R19's "Resilient
  mode (mobile networks)" toggle lived only in the viewer's context menu,
  i.e. the feature built for phones was unreachable on phones; the fix is a
  visible "⋮" button in the control bar opening the *same* menu. Building it
  surfaced two browser-only traps. (1) **The opener is inside the
  outside-click handler's world**: the menu closes on any outside
  `pointerdown`, including the button's own, so the ensuing `click` re-opens
  what the pointerdown just closed and the button can never dismiss its own
  menu. A "was it open at pointerdown?" guard *passes in jsdom* — `fireEvent`
  runs inside `act`, which defers the flush — and fails in Chrome, which
  runs a microtask checkpoint between listeners. The menu now takes an
  `anchorRef` and excludes it from "outside": no race to time. (2)
  **Measuring an element mid-animation measures the animation**:
  `getBoundingClientRect()` on the menu returned its `scale(0.97)` opening
  frame, 3 % small, enough to drift it onto the button — and a fixed
  element's shrink-to-fit width depends on where you put it, so measuring at
  the requested position returns a squeezed size that then feeds the
  placement math. Measure the layout box (`offsetWidth`/`offsetHeight`) from
  a neutral corner, hidden until placed.
  ([docs/24](24-viewer-network-resilience.md))
- **A menu with no `max-height` grows off the bottom of the screen, and
  clamping its *position* does not save it.** Every milestone from R12 on
  added its viewer control to one flat context menu until the worst case was
  17 rows (~740 px at the touch row height) — past a phone's landscape
  viewport, with `Copy link`, `Terms of use` and `Leave` simply unreachable.
  The placement math looked correct and was: it floors `top` at the 8 px pad,
  which keeps the *head* on screen and says nothing about the tail. A menu
  needs a height bound and `overflow-y` as well as a position, and the bound
  has to feed the placement math — an unclamped `offsetHeight` sends a
  bottom-right anchor far off the top instead. Also: **a '✓' glued into a
  label string is not a state**, it is a changing accessible name; use
  `aria-checked` with the mark drawn from it in CSS, and keep the row's
  cost/reason line as `aria-describedby` so the name stays the label alone.
  ([docs/37](37-viewer-playback-presets.md))
- **A silently-dead publisher session holds its slot for up to the QUIC idle
  timeout (~30 s), and rejecting the broadcaster's own reclaim in that window
  killed live broadcasts.** The reclaim got 409 from its own zombie session,
  clients fell back to minting a new ID (the browser can't tell 409 from
  404), and the old broadcast — with every viewer — was GC'd mid-stream.
  Since 2026-07-18 a resume-token-bearing claim that completes its upgrade
  **supersedes** the incumbent instead ("newest publisher wins" — the
  same-pod counterpart of R17's lease force-take; deposed session gets close
  code 4004, `CloseCodePublisherSuperseded`); a malformed request that fails
  the upgrade still deposes nothing.
  ([docs/06](06-multi-broadcaster.md))
- **frameIds are uint32 serial numbers — every comparison must be wrap- and
  restart-aware, and moving keyframes off datagrams (R8) silently broke the
  restart path (R10).** The reassembler drops "late" deltas against a
  watermark that only keyframes could reset — but since R8 keyframes bypass
  it on streams, so a broadcaster restart (frameIds reset to 0, viewers stay
  connected) dropped *every* new-session delta as late for ~36 min: a 2 fps
  keyframe slideshow. Signature: "Dropped (late)" climbing at full frame
  rate, `keyframeWaitDrops` flat. Now: stream keyframes sync the watermark
  (`noteStreamKeyframe`), frameId comparisons use serial arithmetic
  (`wire.frameIdAhead`, handles uint32 rollover), and a serially-backwards
  keyframe triggers an immediate reorder resync (a grace wait there costs an
  extra GOP of freeze). ([docs/14](14-viewer-render-performance.md))
- **Keyframes go over a reliable uni stream; deltas stay on datagrams (R8).**
  A keyframe split into ~1200-byte datagrams is ruined by a single lost packet,
  and heavy-motion keyframes are large — so the broadcaster sends each keyframe
  (config prepended) over `createUnidirectionalStream()` and the relay
  **stores-and-forwards** it: it reads the whole keyframe into a bounded buffer
  (capped by `MaxKeyframeBytes`) *before* fanning out one uni stream per
  subscriber, which is what structurally decouples the publisher from any slow
  subscriber. A subscriber that stalls past `KeyframeWriteTimeout` is
  `CancelWrite`-ed and superseded by the newest keyframe (**≤1 in-flight per
  subscriber**) — "drops over stalls" at stream granularity. Only keyframes are
  reliable; making deltas reliable would reintroduce head-of-line blocking.
  ([docs/12](12-worker-and-reliable-keyframes.md))
- **`transferControlToOffscreen()` is one-shot and irreversible — so the viewer
  worker must outlive React's StrictMode remount (R8).** The production viewer
  runs its pipeline in a Web Worker and hands it the `<canvas>` via
  `transferControlToOffscreen()`; a second call on the same element throws, and
  a transferred canvas can't be reclaimed for a main-thread 2D context. So the
  worker + transfer are created **once for the life of the view** (reconnects
  happen *inside* the worker, reusing `ViewerSession`), and teardown is
  **deferred a macrotask** so StrictMode's synchronous unmount→remount reuses
  the same worker instead of trying to transfer twice. Worker support is probed
  with a **boot handshake before the transfer**, so an unsupported worker
  (e.g. no worker WebCodecs) falls back to the main-thread pipeline with the
  canvas still attached. ([docs/12](12-worker-and-reliable-keyframes.md))
- **Firefox's 2D-canvas `drawImage(VideoFrame)` is a synchronous CPU
  conversion — and on a shared viewer worker thread it starves everything
  else (R10).** When one thread runs the datagram reader, decoder feeding
  *and* rendering, a slow per-frame draw overflows the browser's small
  incoming-datagram queue (silent drops → "Dropped (incomplete)") and backs
  up decode (decoded fps < received fps). Hence two structural fixes: the
  worker renders through `createRenderSink()` — a **WebGL** textured-quad
  sink (2D fallback) wrapped in a **coalescing** sink that draws at most
  once per rAF tick, latest-frame-wins — and the WebTransport read loops run
  in a **nested transport worker** (`transport.worker.ts`, one per pipeline
  attempt behind the `ViewerTransport` seam) posting transferable buffers,
  so decode/render pressure can't reach the datagram queue at all.
  Consequence: "Rendered fps" reads ≈min(decoded fps, display Hz) — rendered
  *below* decoded under load is coalescing working, not frame loss. Every
  fallback here degrades silently, so the overlay shows the actual placement:
  "Renderer" / "Pipeline" / "Transport" read **WebGL / Worker / Worker** on
  the fast path (— / Main thread / In-process on the no-worker fallback).
  ([docs/14](14-viewer-render-performance.md))
- **Every worker has its own `performance.timeOrigin` (its creation moment) —
  a `performance.now()` reading is meaningless in another worker.** Field bug
  (2026-07-19): the TimeSync offset is measured inside the *nested transport
  worker*, but capture→render latency applied it to the *viewer worker's*
  `now()`. Identical origins on first connect masked it; any mid-view
  reconnect (resilient-mode toggle, 4002 rollout drain, auto-reconnect)
  spawns a fresh transport worker minutes after the viewer worker, and the
  latency row inflated by exactly that age gap (~3 min after ~3 min of
  watching). `TimeSyncStats` now carries `timeOriginMs` and consumers rebase
  onto the sample's clock domain before applying the offset. When timing data
  crosses a worker boundary, ship its `timeOrigin` with it.
  ([docs/15](15-viewer-live-edge.md))
- **Transferring a `MediaStreamTrack` detaches it — the broadcast worker gets
  a *clone*, the original stays for the preview (R11).** The broadcaster
  pipeline runs in a Web Worker fed by a transferred `track.clone()` (MSTP is
  created *in the worker* — transferring `processor.readable` instead would
  keep piping every frame through the main-thread realm, defeating the
  split). `getDisplayMedia` itself must stay on main (window scope + user
  gesture), and transferability is probed **before** committing to the worker
  path by postMessage-ing a dummy `canvas.captureStream()` track —
  non-transferable types throw `DataCloneError` *synchronously on the
  sender*, no roundtrip needed. Firefox lands on the untouched main-thread
  pipeline via the same boot-handshake fallback as the viewer; the overlay's
  broadcaster "Pipeline" row shows which path is live.
  ([docs/16](16-broadcaster-worker-offload.md))
- **Chrome on macOS exposes MSTP on `Window` only — worker scope has no
  `MediaStreamTrackProcessor`, no `MediaStreamTrackGenerator`, and no
  `MediaStreamTrack` at all**, so the broadcaster's worker offload can never
  engage there and `probeTrackTransfer` throws `DataCloneError` for the same
  underlying reason (the whole breakout-box + transferable-track surface is
  off). Measured on Chrome 150 / macOS, 2026-08-20, in both module and classic
  dedicated workers; `VideoEncoder`, `VideoDecoder`, `WebTransport` and
  `OffscreenCanvas` are all present there, so a "worker capability" check that
  stops at those reads healthy and tells you nothing. This is the exact
  inverse of what the spec assumes, so **probe the capability, don't infer it
  from the spec or from another platform**. The *viewer's* offload is
  unaffected — it needs only `OffscreenCanvas` +
  `transferControlToOffscreen` + a worker-scope `VideoDecoder`, all present.
  ([docs/16](16-broadcaster-worker-offload.md), [BUGS.md](../BUGS.md))
- **`isConfigSupported({hardwareAcceleration: 'prefer-hardware'})` answering
  `supported: true` is Chromium's hardware commitment** — the spec has no
  "require hardware", but Chromium returns false when it can't do HW, which
  is what makes the R13 probe matrix (and its "hardware only" mode)
  meaningful. The probe stays *advisory*: the live `configure()` result wins,
  and the overlay's "Encode mode" row shows the runtime truth — picker
  badges are predictions, not guarantees.
  ([docs/18](18-advanced-broadcaster-settings.md))
- **`isConfigSupported` is not free on Chrome — never fire probes
  unbounded.** Every *pending* call holds a real encoder instance (software
  probes at 4K allocate full encoder contexts), so requesting the
  broadcaster surface's ~170 matrix combos in parallel OOM-crashed the tab.
  `EncoderSupportProber` gates all probes through a fixed slot pool
  (`MAX_CONCURRENT_PROBES`, 4) and the per-codec matrices only probe once
  the settings panel opens — route any new probe callers through the
  prober, never `VideoEncoder.isConfigSupported` directly in a loop.
  ([docs/18](18-advanced-broadcaster-settings.md))
- **Track-clone constraints are per-track and must be applied worker-side
  (R13).** Capture alignment (`track.applyConstraints` on the sticky
  resolution/fps target) lands on whichever track feeds MSTP: the
  transferred clone inside the broadcast worker, or the original on the
  main-thread path. Constraining the main-thread original does nothing for
  a worker session — clones hold independent constraints (that's also why
  the preview stays native). And the probe matrix refines from real frame
  dims **upward only**: re-probing at our own constrained dims would feed
  the ceiling its own output (constrain → smaller frames → lower ceiling →
  constrain…). ([docs/18](18-advanced-broadcaster-settings.md))
- **An AVCC length prefix can be byte-identical to an Annex-B start code** —
  a 4-byte length prefix for any 256–511-byte NAL is `00 00 01 xx`, so a
  per-frame "does it start with a start code?" sniff will misparse real AVCC
  frames (the muxer fixture's frame 16 is exactly that). Decide the wire
  format at keyframes — avcC-bearing config ⇒ AVCC, leading start code with
  in-band SPS/PPS ⇒ Annex-B — and keep it sticky for the deltas.
  ([docs/27](27-ios-mse-fullscreen.md))
- **An fMP4 sample's declared duration must be the interval to its SUCCESSOR,
  not from its predecessor** — otherwise every cadence increase leaves a hole
  exactly that big in the buffered timeline, and a live stream's cadence is never
  constant (a reorder-gap resync discards a GOP and jumps ~33 ms → ~500 ms). It
  costs one frame of lookahead. Constant-cadence fixtures cannot catch this: the
  forward and backward intervals are equal there, which is why it shipped twice.
  ([docs/27](27-ios-mse-fullscreen.md))
- **An MSE SourceBuffer that never receives its init segment can block the whole
  MediaSource** — Chromium's demuxer waits for every added buffer's init segment
  before reporting metadata, so video appended alongside an unfed audio buffer
  never becomes playable and `play()` returns a promise that can never resolve
  (this hung a CI run for six minutes; `play()` resolves only when playback
  *begins*, so always race it against a timeout in a test harness). Other
  implementations treat a track-less buffer as absent and play video regardless —
  so a watchdog for this must act on the observable symptom (video appended, still
  no metadata and no buffered range), never on the proxy "the other track is
  quiet", or it destroys working presentations on the forgiving implementations.
  ([docs/27](27-ios-mse-fullscreen.md))
- **All of an MSE MediaSource's SourceBuffers must be added before the first init
  segment is appended** — Chromium's demuxer accepts new IDs only while
  initializing and throws `QuotaExceededError` ("reached the limit of SourceBuffer
  objects") afterwards. So create the buffer from its **mime** as soon as the
  track is known, and withhold only the init *segment* until you have bytes for
  it; a track's buffer existing and a track's init being appended are two
  different things worth tracking separately.
  ([docs/27](27-ios-mse-fullscreen.md))
- **`ManagedMediaSource.streaming` must never gate the *first* appends** — MMS
  sets it when the element needs more data, and an element that has never been
  given an init segment needs nothing (`buffered` empty, `readyState` 0). Park
  priming behind it and neither side moves: the source stays open, no append is
  ever attempted, no error is ever raised, and on iPhone the native fullscreen
  tier silently disappears for the whole session because `webkitEnterFullscreen`
  requires `HAVE_METADATA`. Prime through the park; pace only a fed element.
  ([docs/27](27-ios-mse-fullscreen.md))
- **A segment stream whose init segment is emitted once per session cannot
  tolerate a sink that comes and goes** — the R22 worker muxer survives
  reconnects and never re-emits its init, so unregistering the main-thread sink
  on a status change (a reconnect) can lose that one segment, after which every
  media segment is dropped for want of it. Silently, permanently. Tie such a
  sink's lifetime to its consumer, not to connection state.
  ([docs/27](27-ios-mse-fullscreen.md))
- **iOS refuses Opus in MP4 through ManagedMediaSource** — measured on iOS 18.7 /
  Safari 26.5.2: `isTypeSupported('audio/mp4; codecs="opus"')` is false, even
  though WebKit 17 added Opus-in-MP4 for file playback and Chrome's MSE accepts
  it. AAC-LC (`mp4a.40.2`), the codec Apple's own HLS mandates, is the fallback —
  which for a WebCodecs pipeline means transcoding the decoded PCM.
  ([docs/27](27-ios-mse-fullscreen.md))
- **A live MSE presentation must set `duration = Infinity`** — otherwise MSE
  keeps raising duration to the newest appended end timestamp, and two things
  break: the native player draws a finite scrub bar instead of the LIVE badge,
  and "the playhead reached the buffered end" becomes indistinguishable from
  "the media ended". **WebKit resolves that by pausing and firing `ended` on an
  MSE buffer underrun, where Chromium stalls and resumes** — so on iPhone every
  hiccup killed playback until the user tapped play, and no Chrome-based CI
  could see it. Set it in the `sourceopen` handler: the setter throws unless
  readyState is `open` with no SourceBuffer updating.
  ([docs/27](27-ios-mse-fullscreen.md))
- **`HTMLMediaElement.buffered` is the INTERSECTION of the SourceBuffers'
  ranges** — so with a muxed audio track, a hole in audio is a hole in playback
  and stalls the *video* the native player is showing. Hence one packet of
  lookahead in the audio muxer (a sample's declared duration must be the real
  interval to its successor, which also lets a lost datagram be absorbed by
  stretching the preceding sample over the gap), and hence the audio
  SourceBuffer is created on the first audio *sample*, never when its config
  arrives — an audio track with no samples empties the intersection.
  ([docs/27](27-ios-mse-fullscreen.md))
- **WebCodecs is `[SecureContext]`, and `about:blank` is not one** — an e2e page
  loaded via `about:blank` has `AudioEncoder`/`VideoEncoder` undefined (while
  non-gated APIs like `AudioData` and `MediaSource` are present, so it looks
  like a codec-support problem rather than an origin problem). Serve the page
  from a `localhost` origin — the `--muxer-check` harness fulfills one
  intercepted request in memory rather than starting a server.
  ([docs/27](27-ios-mse-fullscreen.md))
- **iPhone has no Element Fullscreen API** — `Element.requestFullscreen`
  ships on iPadOS (16.4+) but not iPhone, in every iOS browser (all WebKit),
  so an optional-chained call is a silent no-op there. The only native
  fullscreen is `HTMLVideoElement.webkitEnterFullscreen()`: it needs a user
  gesture and a video with loaded media (hence the pre-armed hidden
  `<video>` — since R22 fed by a worker-side fMP4 muxer through
  `ManagedMediaSource`, after R16's MediaStream tee proved the native player
  renders locally generated tracks black), it does **not** fire
  `fullscreenchange` (track `webkitbeginfullscreen`/`webkitendfullscreen`
  instead), and **`display: none` on the video breaks it** — hide by
  size/position. ([docs/21](21-ios-video-fullscreen.md))
- **One pipeline, one pacing schedule.** The reorder buffer's release gate and
  the paced sink's display target must derive from the *same* baseline. R15
  moved the display target onto the audio playhead and left the release gate
  on the arrival baseline; the difference (audio's jitter buffer + output
  latency) landed every frame in `PacedPresentationSink` long before its slot,
  and the sink drops the **oldest** past `MAX_HELD_FRAMES` — so it discarded
  each frame just as it came due. Total video freeze, the instant audio
  started. Video is now the master outright: both ends read the arrival
  baseline, av-sync exports no video-side lever at all, and audio aligns to
  video. ([docs/20](20-system-audio.md) field findings 2 + 4)
- **Audio alignment is a start-time decision, not a buffering one.** The
  AudioWorklet consumes exactly `sampleRate` samples per second at 1×, so once
  playback has begun, holding chunks longer changes queue *depth* and nothing
  else — it cannot change when a given sample is heard. Lip sync is therefore
  set by *when playback starts* (hold the first chunk until the video
  presentation schedule says it is due), and everything after it is a **rate**
  problem: drift is absorbed by a sub-audible ±0.4% playback trim, never a
  step or a skip. ([docs/20](20-system-audio.md) field finding 4)
- **A control loop's metric must measure where the user is, and stop reading
  when it stops measuring.** Because alignment is a start-time decision (above)
  and the drift trim integrates away from it, `avSkewMs` is not a diagnostic —
  it is the *only* thing deciding where audio ends up on a long session. Two
  ways that bit. (1) It compared video against the AudioWorklet's playhead,
  which is the sample being *written*; the speaker plays it `outputLatency`
  later, so a perfectly synced stream read `−outputLatency` and the trim
  dutifully drove that to zero — walking audio 100 ms late on a 120 ms device
  and 280 ms on a 300 ms one, with the overlay showing ~0 the whole way. Measure
  at the listener (`getOutputTimestamp()` gives the audible position *and* when
  it is audible, as one measurement). (2) The skew is written only where a frame
  is *presented* but read on the stats tick, so a throttled tab (`renderedFps: 0`
  while audio plays on) fed the trim a frozen number open-loop; a stale sample
  now reads null instead. ([docs/20](20-system-audio.md) field finding 13)
- **A metric that means two things means nothing.** `avSkewMs` was read in the
  thousands on long sessions while audio sounded fine, and three findings argued
  over the number, because one subtraction was being asked to report both "audio
  is offset from video" (lip sync, actionable) and "the audio timeline is losing
  ground" (starvation debt, which collapses the moment audio recovers). The
  arithmetic is identical; only the companion signal separates them. The viewer
  now reports **Playhead advance** — how far the audio playhead moved against
  the wall clock over the last second — beside the skew: `1.000×` with a skew is
  lip sync, `0.934×` with a skew is debt still being accrued (and 0.934×
  reproduces the field capture's 1986 ms over 30 s exactly). Deliberately
  reported, never used to suppress the skew — when audio really has fallen
  behind, that reading is true. ([docs/20](20-system-audio.md) field
  finding 12)
- **Don't smooth a measurement you were handed exactly.** The same metric's
  mapping low-passed each 4 Hz playhead report toward its own prediction at
  20 ms/s. A playhead that moves in jumps — a buffer skipping a hole, a re-prime
  resuming at live — outruns that cap, so the smoothing traded bounded input
  jitter for an unbounded one-directional bias, and a snap threshold had already
  been bolted on to paper over the worst of it. Anchor on the measurement;
  smooth the *output* if anything. Related: bound how far an estimator will
  extrapolate — at 1500 ms against a 4 Hz reporter this one could invent 1.5 s
  of skew out of an assumption. ([docs/20](20-system-audio.md) field
  finding 12)
- **Stamp both media at the same pipeline stage.** Video is stamped at capture
  arrival, before encode; audio's shared-clock anchor was pinned in the *encoder
  output* callback, so MSTP delivery + Opus algorithmic delay + queueing + the
  one-shot `configure()` init were written into every audio timestamp for the
  session. Nothing downstream can see it — those timestamps *are* the reference
  the two media are compared against, so `avSkewMs` reads a clean zero while lip
  sync is wrong. ([docs/20](20-system-audio.md) field finding 13)
- **A drop policy and a concealment policy can cancel each other out.** R15's
  audio buffer dropped an incoming chunk on overflow *without* advancing its
  expected-timestamp cursor, so the run of drops came back as a hole — which
  the gap branch then filled with exactly as much synthesized silence,
  re-adding the depth the drop existed to shed. Overflow-dropping could
  therefore never lower the depth, only convert audio into silence: on Safari
  the buffer latched at the ceiling and served **74 % overflow drops against
  zero packet loss** (received == decoded), ~25 % real audio and ~75 % silence.
  Two symptoms that cannot both be true — overflow drops climbing *and*
  underruns climbing — are the signature. ([docs/20](20-system-audio.md)
  field finding 8)
- **A depth estimate credited down at 4 Hz is stale by 250 ms** — more than the
  200 ms of overflow slack it was being compared against, so a healthy
  real-time producer feeding a real-time sink cleared the ceiling near the end
  of every report window and dropped audio ~46×/s. Extrapolate a known-rate
  drain between reports. ([docs/20](20-system-audio.md) field finding 8)
- **Don't shadow a queue you don't own — make its owner report it.** R15's
  audio buffer maintained its own count of what the AudioWorklet held, from
  deliveries and drain deltas. A shadow cannot audit itself, and it drifted for
  three different reasons across two findings (undelivered chunks while the node
  booted; an assumed context sample rate; a suspended context that stopped
  reporting), each time latching the buffer above its overflow ceiling with no
  way back. The worklet now reports its own depth in **content ms** (each
  chunk's `frameCount / sampleRate`, so the context's rate cannot enter the
  accounting), and a cumulative counter on each side reconciles the chunks
  in flight when the report was generated.
  ([docs/20](20-system-audio.md) field findings 7 + 8)
- **An AudioContext does not have to run at the rate you asked for** — macOS
  hands back the device rate (44.1 kHz is routine) for 48 kHz content, and a
  browser may reject the `sampleRate` option outright. Playing one source sample
  per output frame there is 8.8 % slow and a semitone low, *and* under-drains
  the queue by 8 %/s. Always read `ctx.sampleRate` back; the worklet's read rate
  is `content ÷ context`, with the drift trim multiplying it (the fractional
  resampler is already there for the trim — the bug is hardcoding its base
  to 1). ([docs/20](20-system-audio.md) field finding 8)
- **A jitter-buffer target that is only a ceiling is not a jitter buffer** —
  R15's audio buffer enforced its 40–150 ms target on overflow but forwarded
  every chunk to the worklet on arrival, so the sink played at ~0 ms depth and
  any jitter ran it dry (zero is a reflecting barrier: underruns discard time
  that can never be won back). The target must be a **floor**: prime to it
  before playing, rebuild it after a genuine dry spell.
  ([docs/20](20-system-audio.md) field finding 3)
- **`getDisplayMedia` audio is all-or-nothing, not best-effort** — if the
  browser cannot start a system-audio source it rejects the **whole**
  request with `NotReadableError: Could not start audio source`, taking video
  down with it; it does *not* degrade to a video-only grant. Two independent
  failure classes, and the exception is identical for both: **platform** (no
  loopback path at all — Linux and macOS screen/window shares) and **device
  state** (Windows *can* capture system audio, and still fails when the
  default output endpoint won't open for loopback — exclusive-mode holders,
  disconnected/asleep endpoints, some virtual devices). **Tab audio uses
  Chrome's internal mirroring path, not OS loopback, so it works where system
  audio doesn't** — it's the discriminator when triaging, and the fallback
  when capturing. `capture.ts` retries once without audio, but that retry
  needs its own transient activation, which the seconds spent in the picker
  have usually already spent — so a failed first start is the common landing
  spot. Since audio became unconditional (2026-07-23) the refusal is
  **remembered for the rest of the page session**, so simply starting again
  broadcasts video-only; a reload retries audio, because the device-state
  class of failure is transient.
  ([docs/20](20-system-audio.md) field finding 1 + Graduation)
- **~1200-byte safe datagram payload** drives the chunking design — don't
  assume larger datagrams survive the path.
- **Raising the QUIC idle timeout does not keep idle viewers alive** — the
  effective timeout is the min of both endpoints' advertised values and
  browsers advertise ~30s. The server-side keepalive (`-keepalive-period`)
  is the mechanism. ([docs/05](05-resilience-deploy.md))
- **Hardware encoders don't surface backpressure via `encodeQueueSize`** —
  they drain frames without the queue growing past R4's `> 2` threshold, so
  the encode-queue rejection signal (auto-fallback's sole trigger) under-fires
  on the hardware path the gaming PC uses. Low fps there is usually
  source-limited, not encoder strain (encoder queue stays 0–2, `Dropped
  (source)` flat). Reproduce real backpressure with DevTools CPU throttling,
  which forces the software encode path.
  ([docs/09](09-automatic-fallback.md))

**Native Linux broadcaster (R14)**

- **The browser cannot hardware-encode on Linux — don't go flag-hunting.**
  WebCodecs `VideoEncoder` HW encode ships on Windows/macOS/Android only
  (Linux gets HW *decode*), Chromium's own VA-API doc disclaims Linux, and
  on NVIDIA it's impossible in principle: Chromium's Linux encode path is
  VA-API only and `nvidia-vaapi-driver` is decode-only by design. That gap
  is the entire reason `gawk-broadcast/` exists.
  ([docs/19](19-linux-native-broadcaster.md))
- **`gawk-broadcast` is the *only* Annex-B publisher — and the only thing
  exercising the viewer's Annex-B branch.** It emits raw Annex-B with
  **empty DecoderConfig extradata** and builds no avcC record; the viewer's
  `isAnnexB` start-code sniff (`viewer.ts`) routes it into the branch that
  ignores extradata. The browser broadcaster always sends AVCC, so a
  regression there breaks native broadcasts while browser ones stay green.
  ([docs/19](19-linux-native-broadcaster.md))
- **`h264parse config-interval=-1` is load-bearing, not cosmetic.** Empty
  extradata means a late joiner primed with the relay's cached keyframe can
  only decode if SPS/PPS travel *inside* the keyframe AU. Drop the flag and
  late joiners see nothing while everyone already watching is fine.
  ([docs/19](19-linux-native-broadcaster.md))
- **Don't gate the native broadcaster on Wayland** — the XDG ScreenCast
  portal works on X11 GNOME sessions too. Gate on the portal call
  succeeding, never on the session type.
  ([docs/19](19-linux-native-broadcaster.md))
- **A Gio widget that animates on a state it never leaves free-runs the
  window.** `gioui.org/x`'s `component.TextField` executed an
  `op.InvalidateCmd` on *every* frame a field held text, so the native
  broadcaster's GUI redrew at the compositor's rate from launch and burned
  20–30 % CPU sitting idle. It hid twice over: the fields are pre-filled from
  the saved config (a fresh config doesn't reproduce it) and the settings panel
  is laid out disabled while live, where a zero `input.Source` swallows the
  invalidate — so it vanished exactly when someone was watching for it. The GUI
  now draws its own text fields and the dependency is gone. Guard the property,
  not the symptom: assert an idle window schedules no already-due wakeups
  (`cmd/gawk-broadcast-gui/main_test.go`), never a CPU threshold.
  ([docs/19](19-linux-native-broadcaster.md))
- **`pipewiregrab` is NOT in mainline FFmpeg** — it's an unmerged patchset
  carried downstream by Jami; mainline ffmpeg has no PipeWire input at all.
  This is why capture is a GStreamer subprocess. Don't re-propose it
  without verifying it actually merged.
  ([docs/19](19-linux-native-broadcaster.md))
- **The Opus-in-MPEG-TS control header begins `7F E0`, not `FF E0`.** The
  mapping spec's 11-bit prefix holds the value `0x3FF`, which is *ten* ones —
  so an 11-bit field carrying it is one zero bit followed by ten ones, and the
  first byte is `0x7F`. A demuxer that syncs on `0xFF` finds nothing, ever.
  Measured on real muxer output, and pinned by the committed fixture in
  `gawk-broadcast/internal/mpegts/testdata/`. Also: GStreamer writes one Opus
  access unit per PES, ffmpeg batches five — the format permits both, so parse
  a loop. ([docs/28](28-native-broadcaster-audio.md))
- **A muxer's audio cadence follows the *declared* caps framerate, not actual
  frame arrival.** `mpegtsmux` is `GstAggregator`-based and aggregates against
  a clock deadline derived from the pipeline's declared latency. Starve the
  video pad for 4.6 s with 30 fps caps and audio keeps flowing every 20 ms;
  declare `framerate=1/5` and the same starvation clumps audio into 5-second
  bursts, because one video frame interval *is* the pipeline's latency. This
  is a second, non-obvious reason `BuildPipeline` pins `framerate=<fps>/1` on
  the encoder caps — docs/19 added it for rate control, and audio smoothness
  on a motionless screen rides on it too.
  ([docs/28](28-native-broadcaster-audio.md))
- **One PTS anchor maps both media in the native broadcaster, and splitting it
  is an invisible lip-sync bug.** Audio and video come out of one `mpegtsmux`
  on one pipeline running time, so one `ptsAnchor` maps both with one affine
  function and the relative A/V skew is zero *by construction*. Give audio its
  own anchor — which looks tidier — and it ships a constant
  (video path latency − audio path latency) offset that no viewer instrument
  can detect or remove. `TestOneAnchorStampsBothMedia` exists to fail that
  refactor; note that it has to script the clock, because a fixture delivered
  in bulk makes arrival effectively constant and collapses both media onto it,
  which would let a split anchor pass.
  ([docs/28](28-native-broadcaster-audio.md))
- **Notifications must be critical urgency or the broadcaster never sees
  them** — KDE's portal inhibits normal notifications *while screen
  casting*, so the act of broadcasting suppresses exactly the notifications
  a fullscreen broadcaster needs. Failures use critical; going live doesn't.
  ([docs/19](19-linux-native-broadcaster.md))
- **The viewer count is relay-pushed and eventually-consistent (~1–2 s)** —
  since R18 (2026-07-18) both broadcasters and every viewer show a live
  "N watching" figure over a new relay-originated `ViewerCount` datagram
  (0x0B). It is only ever trusted from a relay peer: a broadcaster-sent
  count is dropped (spoof guard), and in cluster mode edges report their
  local count up while the origin fans the global total down — edge
  sessions themselves are never counted.
  ([docs/23](23-live-viewer-count.md))
- **`pipewiresrc` can die with `stream error: unhandled format` — that's
  capture, not the encoder.** The compositor's chosen screencast format
  sometimes can't be mapped onto the downstream caps (DMA-BUF modifier/DRM-caps
  skew, 10-bit HDR desktops). The live start walks a three-rung capture ladder
  per encoder (rate-capped zero-copy — `max-framerate` asked of the
  compositor — then free negotiation, then system-memory pinned
  `video/x-raw`) before advancing the cascade, and an all-pipewiresrc failure
  reports as `ErrCaptureFormat`, never "no hardware encoder". Diagnose with
  `GST_DEBUG=pipewire*:5` (the child inherits env).
  ([docs/19](19-linux-native-broadcaster.md))
- **The same death *mid-broadcast* rebuilds the pipeline instead of ending
  the broadcast** — a window's screencast format is renegotiated when it
  resizes, changes output or hands over its surface, and until 2026-08-01
  that killed a working stream outright (the ladder only ever ran at start).
  A rebuild reuses the portal grant (no second picker), keeps the relay
  session, frameId space and the frame/audio channels, and walks the whole
  ladder from the encoder that was working — so a stack whose DMA-BUF
  negotiation has gone bad converges on system-memory capture by itself. The
  bound is a *rate* — 60 rebuilds per 30 s — because the thing worth refusing
  is a hot loop, not a stream that recovers now and then for hours; and it is
  counted (`captureRestarts`), since the viewer's only symptom is a freeze
  ending on the next keyframe.
  ([docs/39](39-linux-app-sharing.md) D2)
- **`gawk-broadcast` is its own Go module and its CI job needs cgo + Gio
  headers** (`libwayland-dev`, `libvulkan-dev`, …) and `CGO_ENABLED=1` —
  with cgo off, the GUI fails as "build constraints exclude all Go files in
  `gioui.org/internal/vk`", which says nothing about cgo. The engine and CLI
  need no headers: `go test ./internal/...` works bare. The relay's CI job
  must stay header-free (Decision 1).
  ([docs/19](19-linux-native-broadcaster.md))
- **`mpegts.AU.Data` aliases the demuxer's buffer — clone before it outlives
  the callback.** A frame handed to the channel un-cloned is rewritten by
  the AUs demuxed behind it: in the field this meant clean debug dumps (both
  taps read the bytes while still valid) but a black viewer — the SPS parse
  ran on recycled bytes, so no DecoderConfig was ever derived, and an
  unconfigured `VideoDecoder` swallows chunks with zero errors. Motion
  scrambled queued frames to reference soup; a static screen (drained
  channel) stayed sharp. `-race` catches it outright.
  ([docs/19](19-linux-native-broadcaster.md))

**Native Windows broadcaster (R34)**

- **Stock `wtransport` 0.7.1 cannot interop with quic-go** — four defects,
  every one invisible to unit tests: rejected-CONNECT status discarded,
  HashMap headers randomly ordering pseudo-headers after fields (a
  coin-flip H3_MESSAGE_ERROR), unknown H3 frames treated as violations
  (GOAWAY killed shutdown), and close-code capsules split across DATA
  frames silently skipped. Vendored + patched in-repo; every site marked
  `GAWK PATCH`. ([docs/38](38-windows-native-broadcaster.md) §11)
- **The hosted Windows runner's WARP has no D3D11 video support** — device
  creation with `D3D11_CREATE_DEVICE_VIDEO_SUPPORT` fails
  `DXGI_ERROR_UNSUPPORTED`, so GPU-conversion tests must self-skip there
  and run on real hardware. ([docs/38](38-windows-native-broadcaster.md) F-6)
- **The system `GraphicsCapturePicker` is structurally unusable** for
  per-app audio: it returns an item with no HWND and no PID, and process-
  loopback capture needs the PID — hence the in-app picker.
  ([docs/38](38-windows-native-broadcaster.md) D6)
- **Vendored libopus needs `CMAKE_POLICY_VERSION_MINIMUM=3.5`** under
  CMake 4, or the build script dies before compiling a line.
  ([docs/38](38-windows-native-broadcaster.md) F-7)

**Single-application audio on Linux (R35)**

- **`application.process.binary` is not in PipeWire's registry global
  properties.** A stream node's globals carry `client.id` and no application
  identity; the owning Client's globals stop at `application.name`. The binary
  — which is what per-application capture must match on across a game restart —
  appears only in the properties a **bound** object reports through its info
  event. A helper that trusted globals would show an application list it could
  not link, and would report "nothing is playing audio" while a game was.
  Binding also means **two** registry round-trips before that list is
  trustworthy: the first delivers the globals and triggers the binds, and the
  info events land after its `done`.
  ([docs/39](39-linux-app-sharing.md) F1)
- **Never recreate the capture sink mid-broadcast.** The GStreamer child
  addresses it by `object.serial`, so destroying and recreating it — the
  obvious reaction to a default-device layout change — moves the target out
  from under a running `pipewiresrc` and kills the audio branch. Re-link into
  the existing sink instead. ([docs/39](39-linux-app-sharing.md) F2)
- **An application's stream node is an adapter, and its output ports speak the
  layout it negotiated toward the default sink.** A 5.1 game playing to stereo
  speakers has *stereo* output ports — the downmix already happened. So a
  surround machine is modelled by surround speakers, not by a surround
  application, and reading the application's own ports is the direct way to ask
  what layout a capture sink must have.
  ([docs/39](39-linux-app-sharing.md) F3)
- **The portal never says which application owns the window you picked** — no
  PID, no name, by privacy design. Windows' one-picker "share this app, get its
  audio" flow is structurally impossible on Linux; the shipping flow asks, and
  says why. ([docs/39](39-linux-app-sharing.md) §1)
- **WirePlumber needs a session bus, and a default sink takes a moment after
  the sink node exists.** A headless PipeWire test harness that starts an
  emitter too early gets `stream error: no target node available`, which reads
  like a bug in the code under test rather than a race in the harness.
  ([docs/39](39-linux-app-sharing.md) F6)
- **The stock WirePlumber config cannot run on a container runner**: it enables
  the Bluetooth monitor, which loads the logind plugin, which fails fatally
  with no `/run/systemd` and no system bus — and a dead session manager means
  nothing routes, so no application stream ever grows ports. Invisible on a
  developer's machine, which has logind. A headless harness must supply its own
  config with the linking half and no hardware monitors.
  ([docs/39](39-linux-app-sharing.md) F6)

**Relay fleet (R17)**

- **Sessions created as a group must carry their group identity from
  birth — a later association is unreconstructable.** R30's stripe legs
  were ordinary subscribers "sharing nothing" with their primary by design,
  so when the relay evicted a primary out from under a viewer that never
  reacted, its legs held subscriber slots for the life of the broadcast —
  and the QUIC idle timeout can never fire on a leg, because the relay's own
  delta flow keeps the peer's stack acking while the app is gone. The one
  live capture read `subscribers: 4 / viewersGlobal: 0`: invisible on
  exactly the surface an operator watches, since legs are excluded from the
  viewer count. Fixed by a viewer-minted `?owner=` token on every dial of an
  attempt (required on legs; an unowned primary cannot stripe), an ownership
  reap in `Subscriber.Close`, and a 20 s per-leg liveness lease renewed by a
  1 Hz heartbeat — the cross-pod backstop, since legs may land on pods that
  never see the primary. Watch `stripeLegsReaped` /
  `gawk_stripe_legs_reaped_total`: zero on a healthy fleet.
  ([docs/35](35-connection-interleaving.md) §14)
- **`reason: "context canceled"` in a session-ended log meant "we don't
  know"** — and it is the *normal* reason for any abrupt death, so an idle
  timeout, a stateless reset and a peer's CONNECTION_CLOSE all looked
  identical. quic-go tears streams down inside `handleCloseError` but only
  cancels the connection context in a `defer` that runs after `Conn.run`
  returns, and http3 links the request context to the *stream* with a plain
  `context.AfterFunc(str.Context(), cancel)` — which discards the cause. The
  relay now keeps the connection's context via `http3.Server.ConnContext` and
  reports `context.Cause` from it (`internal/transport/endreason.go`); the
  wait in there is load-bearing, because the stream always wins that race.
  A real cause (`EOF`, `timeout: no recent network activity`) is never
  overwritten. ([docs/19](19-linux-native-broadcaster.md))
- **A native broadcaster reclaims its own broadcast now** (2026-07-27): a
  dropped transport is retried for up to 5 minutes rather than ending the
  session, and `OnEnded` means the broadcast is genuinely over. Close codes
  **4000 and 4004 are never resumed** — "newest publisher wins" only
  converges because the deposed publisher stays down, so an engine that
  reclaimed after a 4004 would flap forever against the one that deposed it.
  ([docs/19](19-linux-native-broadcaster.md))
- **Every `/publish/{id}` claim needs a resume token** (wire 0x09, the
  `resume` query param) — including reclaims that used to work bare. An old
  client's tokenless reclaim gets 403 and falls back to minting; that's the
  designed hijack fix, not a bug. The native broadcaster persists the token
  as `lastResumeToken` beside `lastBroadcastId`.
  ([docs/22](22-relay-scale-out.md))
- **Server uni-stream accept order is NOT open order.** The relay sends the
  announce (0x03) and the resume token (0x09) on two uni streams;
  webtransport-go delivers them to `AcceptUniStream` in nondeterministic
  order — the native engine saw the token first in ~half of dials, and its
  old "read the first stream, strict-parse as announce" turned that into
  "no broadcast code". Dispatch server messages by wire type, never by
  arrival order. ([docs/22](22-relay-scale-out.md) finding 9)
- **The hijack fix needs an explicit `resumeTokenKey` to hold between
  broadcasters** — a token key derived from the broadcaster-distributed
  publish secret is computable by every broadcaster, so it only stops
  non-secret-holders. Set the independent server-side key (it wins over the
  secret derivation; `resume_token_key_mode=explicit-key` in the startup
  log confirms). ([docs/22](22-relay-scale-out.md))
- **Fleet-shared Secrets are load-bearing**: without a shared resume-token
  key (or publish secret), re-homing 403s on every pod except the minting
  one; without a shared `statsKey`, one broadcast has N metric identities;
  without the shared `StatelessResetKey`, abrupt pod deaths cost the ~30 s
  idle timeout instead of ~1 RTT. Check `resume_token_key_mode` in the
  startup log. ([docs/22](22-relay-scale-out.md))
- **Drain is close-first-while-Ready, never "unready then linger"** —
  kube-proxy flushes UDP conntrack on endpoint removal, so an
  established flow can be re-DNAT'd mid-stream at an unspecified time; the
  4002 close must go out while the conntrack entries still point at the
  draining pod. Don't re-derive this. ([docs/22](22-relay-scale-out.md))
- **`replicas > 1` requires `config.clusterMode: true`** — the chart
  refuses to render otherwise (two pods without federation split the
  publisher from the subscribers). ([docs/22](22-relay-scale-out.md))
- **The ClockMapping is rewritten per hop, never forwarded verbatim** across
  an edge pull — each pod's monotonic clock has an arbitrary epoch, so a
  forwarded mapping would corrupt viewers' capture→render latency by that
  epoch difference. ([docs/22](22-relay-scale-out.md))

**Moderation and the admin portal (R39)**

- **client-go ≥ 1.35 defaults the `WatchListClient` feature gate ON, which
  makes a `SharedIndexInformer` fed by a `watch.FakeWatcher` never sync and
  never call `List`.** The reflector opens a streaming watch-list and waits
  forever for an `initial-events-end` bookmark the fake never sends. It fails
  as a silent 5-second timeout with **nothing logged below klog `-v=3`**, so
  the informer simply appears to do nothing. Fix in informer tests:
  `clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.WatchListClient, false)`.
  Production is unaffected — a real API server sends the bookmark.
  ([docs/42](42-admin-moderation-portal.md) §4.2)
- **A Helm `pre-install` hook runs before the chart's own manifests**, so a
  migration Job cannot read a Secret the same chart creates. A chart that
  rendered its own CloudNativePG `Cluster` therefore deadlocked on every first
  install: Helm waiting for the Job, the Job's pod waiting for the DSN Secret
  Helm would not create until the Job finished. Moving the hook to
  `post-install` dodges it, but the real fix is upstream of Helm — **don't put
  the stateful database inside the stateless chart**. `gawk-admin` takes a
  connection to a Postgres that already exists (`postgres.dsn` /
  `postgres.dsnSecretRef`), which makes the database a *prerequisite* rather
  than a dependency of the release, and `pre-install` legal again: the schema
  is migrated before the Deployment rolls on a first install too, not only on
  upgrades. ([docs/42](42-admin-moderation-portal.md) §4.15)
- **Helm never upgrades a chart's `crds/` directory** — it installs those once
  and then leaves them alone forever, so a schema change in a later release
  silently does not apply. Ship CRDs from `templates/` (with
  `helm.sh/resource-policy: keep`, so uninstall does not cascade-delete every
  live custom resource) when the schema is expected to evolve.
  ([docs/42](42-admin-moderation-portal.md) D14)
- **CloudNativePG's generated application Secret is `<cluster-name>-app`**, and
  its `uri` key is a complete connection URI. That naming is the operator's
  contract, not a choice any chart makes — which is what lets a chart support
  CNPG without templating a line of it: `dsnSecretRef: {name: <cluster>-app,
  key: uri}` is the entire integration.
- **Postgres-backed Go tests that `t.Skip` without a DSN report as PASSES**, so
  a CI job that forgets to set the environment variable is green and proves
  nothing. The database is where the interesting invariants live (partial
  unique indexes, `FOR UPDATE SKIP LOCKED`, advisory locks), so the job has to
  assert the tests actually ran, not just that they did not fail.
  ([docs/42](42-admin-moderation-portal.md) AP4/AP8)
- **A raw broadcast ID is a join capability**, which is why it may appear in
  exactly three places — the credential-gated relay admin endpoints, the
  OIDC-gated portal, and Postgres — and never in a webhook payload, `/statusz`
  or a log line at Info+. Webhooks carry the HMAC'd key and a portal link
  instead. Ban *reasons*, by contrast, do ride webhooks: they are operator text
  and the receiver sees them.
  ([docs/42](42-admin-moderation-portal.md) D8)

**CI / deployment**

- **`cmd | grep -q` inverts a "this must fail" test under `set -o pipefail`**
  (which is GitHub Actions' default shell): the pipeline reports the *failing*
  command's status, so `if helm template … | grep -q 'expected error'` takes
  the else branch even when the message matched. Assign the output first
  (`if out=$(cmd 2>&1); then …`), then grep the variable.

- **Tags created with `GITHUB_TOKEN` don't trigger workflows** — publish
  jobs must chain off release-please outputs in the same workflow, never
  `on: push: tags`. ([docs/05](05-resilience-deploy.md))
- **release-please separate PRs self-conflict in a monorepo** (all release
  PRs edit the shared manifest, and the bot doesn't refresh the stranded
  one) — use one combined release PR (`"separate-pull-requests": false`);
  tags/releases/publishes stay per-component.
  ([docs/05](05-resilience-deploy.md))
- **GHCR**: refs are lowercase-only (`tuhis`). The packages are public, so
  pulls need no credentials.
- **The relay is HTTP/3-only (no TCP listener)** — kubelet probes must exec
  the bundled `gawk-echo` binary; `httpGet`/`tcpSocket` can never succeed.
- **That exec probe crash-loops the pod once `allowedOrigins` is set** —
  `gawk-echo` sends no `Origin` header, so `CheckOrigin` rejected its own
  probe as soon as production configured a real origin (dev's empty
  allowlist never hit it). `CheckOrigin` now exempts loopback `RemoteAddr`.
  ([docs/05](05-resilience-deploy.md))
- **release-please `extra-files` paths are package-relative** — a repo-
  relative path silently leaves Chart.yaml unbumped.
- **A git worktree inside the main checkout defeats Go's `-buildvcs`** — the
  go tool walks up for the nearest `.git`, and `.claude/worktrees/*` are
  overtaken by the outer repo's real `.git` directory, so a broadcaster built
  there stamps the *main checkout's* commit and cleanliness. The version badge
  can therefore read clean while the worktree is filthy. Right in CI and in a
  normal clone. ([docs/19](19-linux-native-broadcaster.md) V9)
- **Edge pods verify the internal origin dial against the system root
  pool** — the R17 edge dialer deliberately never sets
  `InsecureSkipVerify`, so on a cluster without cert-manager (the R20 kind
  E2E, any self-signed install) the origin→edge pull fails TLS unless the
  pods trust the cert: set `SSL_CERT_FILE=/tls/tls.crt` via the chart's
  `extraEnv` (Go then uses the mounted self-signed cert as its root pool;
  its SANs must cover `config.internalServerName`).
  ([docs/25](25-e2e-testing-in-ci.md))
- **`gawk-loadgen`'s `gaps` counter has a structural baseline** — keyframes
  ride reliable streams (R8), so the datagram frameID sequence skips one ID
  per GOP: a healthy stream reads (keyframes/s × viewers) gaps/s. Only
  growth beyond that baseline is loss/reorder.
  ([docs/25](25-e2e-testing-in-ci.md))
- **A `needs:` naming a job that doesn't exist doesn't fail the job — the
  whole workflow silently doesn't load.** No jobs, no logs, no annotation,
  and the run is titled with the *file path* instead of the workflow name,
  which is the tell. Two individually-green PRs can produce it (one renames
  a job, the other adds a reference to the old name), so "require branches
  to be up to date before merging" is what actually prevents it.
  ([docs/19](19-linux-native-broadcaster.md))
- **A GitHub re-run replays the workflow file as of that commit** — so it is
  no help when the file itself was what broke, and a release whose build
  never ran cannot be repaired by re-running or by any later push (the
  attach gate compares the tag's commit to the one being built, correctly).
  Recovery is `workflow_dispatch` with `attach_to`.
  ([docs/19](19-linux-native-broadcaster.md))
- **A client that grants no incoming uni streams cannot even complete the
  WebTransport handshake** — HTTP/3 itself needs uni credit for the peer's
  control/QPACK streams, so a `MaxIncomingUniStreams: -1` quic-go client
  blocks forever inside `Dial` waiting for SETTINGS. You cannot simulate a
  "no uni credit" peer at that layer; test the observable half (streams
  unread) and make the no-wedge property structural (send in a goroutine).
  ([docs/40](40-relay-server-picker.md) SP5)
- **"AcceptUniStream blocks until session close" stops being true the moment
  a route grows a real server-initiated stream** — the R17 drain test used
  that idiom to await the close code, and R37's `/echo` RelayIdentity stream
  started satisfying the accept instead; loop until error. Any test using
  AcceptUniStream as a close-barrier inherits this trap when a new 0x-type
  ships. ([docs/40](40-relay-server-picker.md) SP5)
- **A server-message read cap sized to yesterday's largest message silently
  truncates tomorrow's** — both native engines capped relay uni-stream reads
  at 258 bytes (BroadcastAnnounce-era); a full-length TelemetryEndpoint
  (517 bytes) parsed as malformed. Both were caught test-first in R37; size
  read limits from the wire package's max constants, never from the current
  biggest message. ([docs/40](40-relay-server-picker.md) SP8/SP9)
