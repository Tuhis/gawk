# Known bugs

Confirmed, not-yet-fixed defects. Each entry says how it was found, what the
impact is, and where a fix would start. Remove entries when fixed (and move
anything durable they taught us into the relevant `docs/NN-*.md` gotchas).

## Viewer fullscreen button is a silent no-op on iPhone

- **Found**: 2026-07-15, iOS viewer field report ("the full screen button
  doesn't do anything").
- **Impact**: every iPhone viewer (all iOS browsers are WebKit) — the
  button and menu item do nothing, with no error or feedback.
- **Cause** (confirmed): `useFullscreen.ts` calls
  `ref.current?.requestFullscreen?.()`; iPhone Safari has no
  `Element.requestFullscreen` at all, so the optional chaining silently
  swallows the call. Not fixable by prefixing — iPhone's only native
  fullscreen is `HTMLVideoElement.webkitEnterFullscreen()`, and the viewer
  renders to a canvas.
- **Fix designed**: R16 ([`docs/21-ios-video-fullscreen.md`](docs/21-ios-video-fullscreen.md)) —
  canvas tee → `VideoTrackGenerator` → hidden `<video>` +
  `webkitEnterFullscreen()`, with CSS pseudo-fullscreen as the fallback
  tier (chunk U1 alone already removes the silent no-op). Not started.

(The Chrome 152 `WebTransport.getStats()` entry was resolved 2026-07-14: not
a gawk defect — Chromium removed the API entirely; see the gotcha in
`docs/13-observability.md` D7 and the README gotcha list.)
