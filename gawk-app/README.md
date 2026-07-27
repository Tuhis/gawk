# gawk-app

React SPA frontend for the gawk game stream: screen capture, WebCodecs
encode/decode, and WebTransport datagram transport to the relay. Project
overview, quickstart and gotchas: [root README](../README.md).

## Pages (hash-routed)

Production surfaces (R6):

- `#/` — landing page; join-by-code entry, broadcaster/viewer chrome
- `#/broadcast` — capture the screen, encode, publish chunked datagrams
- `#/view/<id>` — subscribe to a broadcast ID, reassemble datagrams, decode,
  paint to canvas
- `#/terms` — usage terms (R23)

The pre-R6 pages are frozen, undecorated diagnostics under `#/debug/*` (not
shared components with the production surfaces above):

- `#/debug/broadcast`, `#/debug/view`, `#/debug/loopback` (capture → encode →
  decode in one tab, no network — a pipeline diagnostic)

## Structure

| Path | What |
|------|------|
| `src/media/` | Capture (`getDisplayMedia` + MSTP/rVFC fallback), encoder/decoder wrappers, loopback pipeline |
| `src/transport/` | Wire format mirror (`wire.ts`, golden-tested against the Go source of truth), packetizer, reassembler, WebTransport connection, broadcaster/viewer pipelines |
| `src/features/` | Pages and components (landing, broadcaster, viewer, terms, debug) |
| `src/state/` | Zustand stores (pipeline state, persisted server settings) |
| `src/ui/` | Shared design-system primitives (monochrome tokens, R6) |
| `src/device-probe/` | Standalone on-device capability harnesses (e.g. the R22 iOS MSE/AAC probe), never imported by the app bundle |
| `src/lib/` | Small shared utilities (broadcast ID formatting, diagnostics JSON, feature gates, fullscreen/hotkey/wake-lock hooks, telemetry, R9/R28) |
| `src/e2e/` | Standalone entry points the R20 CI harness injects into headless Chrome (e.g. the R22 muxer-check driver), never part of the app bundle |
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
