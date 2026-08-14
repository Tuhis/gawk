# gawk-app

React SPA frontend for the gawk game stream: screen capture, WebCodecs
encode/decode, and WebTransport datagram transport to the relay. Requires
a Chromium-based browser for broadcasting; viewing works on Chromium and
Firefox. **WebKit — Safari everywhere, and every browser on iOS — currently
cannot join at all** (see [BUGS.md](../BUGS.md)); the app detects it on load
and warns before the user hits the failure. The iPhone Safari fallbacks are
still in the code and still documented, but nothing reaches them today.
Project overview, quickstart and gotchas: [root README](../README.md).

## Pages (hash-routed)

Production surfaces:

- `#/` — landing page; join-by-code entry, broadcaster/viewer chrome
- `#/broadcast` — capture the screen, encode, publish chunked datagrams
- `#/view/<id>` — subscribe to a broadcast ID, reassemble datagrams,
  decode, paint to canvas
- `#/terms` — usage terms

Frozen, undecorated diagnostics live under `#/debug/*` (not shared
components with the production surfaces above):

- `#/debug/broadcast`, `#/debug/view`, `#/debug/loopback` (capture →
  encode → decode in one tab, no network — a pipeline diagnostic)

## Structure

| Path | What |
|------|------|
| `src/media/` | Capture (`getDisplayMedia` + MSTP/rVFC fallback), encoder/decoder wrappers, loopback pipeline |
| `src/transport/` | Wire format mirror (`wire.ts`, golden-tested against the Go source of truth), packetizer, reassembler, WebTransport connection, broadcaster/viewer pipelines |
| `src/features/` | Pages and components (landing, broadcaster, viewer, terms, debug) |
| `src/state/` | Zustand stores (pipeline state, persisted server settings) |
| `src/ui/` | Shared design-system primitives (monochrome tokens) |
| `src/device-probe/` | Standalone on-device capability harnesses (e.g. the iOS MSE/AAC probe), never imported by the app bundle |
| `src/lib/` | Small shared utilities (broadcast ID formatting, diagnostics JSON, feature gates, fullscreen/hotkey/wake-lock hooks, telemetry) |
| `src/e2e/` | Standalone entry points the CI harness injects into headless Chrome, never part of the app bundle |
| `src/styles/` | Global CSS + design tokens |

## Commands

```sh
npm run dev      # Vite dev server on :5173
npm test         # vitest — wire golden vectors, packetize/reassemble policy
npm run lint     # oxlint
npm run build    # tsc -b + vite build
```

If any of these die with "Cannot find native binding", npm skipped the
platform-native optional deps ([npm/cli#4828](https://github.com/npm/cli/issues/4828)):

```sh
npm install @rolldown/binding-linux-x64-gnu @oxlint/binding-linux-x64-gnu --no-save
```

## Invariant worth knowing

`src/transport/wire.ts` must stay byte-compatible with
`gawk-server/wire`. The golden hex vectors in `wire.test.ts` are
copied verbatim from `wire_test.go` — never regenerate them from code; if
they fail, the wire format drifted and that's the bug.
