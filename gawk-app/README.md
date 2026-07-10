# gawk-app

React SPA frontend for the gawk game stream: screen capture, WebCodecs
encode/decode, and WebTransport datagram transport to the relay. Project
overview, quickstart and gotchas: [root README](../README.md).

## Pages (hash-routed)

- `#/view` (default) — subscribe to the relay, reassemble datagrams, decode,
  paint to canvas
- `#/broadcast` — capture the screen, encode, publish chunked datagrams
- `#/loopback` — capture → encode → decode in one tab, no network; kept as a
  pipeline diagnostic

## Structure

| Path | What |
|------|------|
| `src/media/` | Capture (`getDisplayMedia` + MSTP/rVFC fallback), encoder/decoder wrappers, loopback pipeline |
| `src/transport/` | Wire format mirror (`wire.ts`, golden-tested against the Go source of truth), packetizer, reassembler, WebTransport connection, broadcaster/viewer pipelines |
| `src/features/` | Pages and components |
| `src/state/` | Zustand stores (pipeline state, persisted server settings) |

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
`gawk-server/internal/wire`. The golden hex vectors in `wire.test.ts` are
copied verbatim from `wire_test.go` — never regenerate them from code; if
they fail, the wire format drifted and that's the bug.
