## Domain notes (curated)

- One row in `rollups` is one finished session of one participant. `role` is
  `viewer` or `broadcaster`. A broadcast is identified by `broadcastKey`; a
  viewer's row carries the key of the broadcast it watched. `reconnects`
  counts the reconnects within that session.
- `browser` holds a browser family and its major version, as the web app's
  `describeClient` reduces the user agent: `Edge`, `Opera`, `Firefox`,
  `Chrome` or `Safari`, then the number (`Firefox 159`), or `unknown`.
  Native clients put their distribution name there instead:
  `gawk-broadcast-windows`, `gawk-broadcast-macos`, `gawk-broadcast-linux`,
  `gawk-ios`, and the removed Go Linux broadcaster's `gawk-broadcast`. Use
  `split_part(browser, ' ', 1)` for a browser family.
- `os` is the platform family: `Windows`, `Linux`, `macOS`, `iOS`,
  `Android`, `ChromeOS` or `unknown`. iPads report as `macOS`.
- Times are epoch milliseconds (BIGINT): `startedAt`, `endedAt` in rollups,
  `receivedAtMs` in sessions, `atMs` in relay. Use
  `to_timestamp(x / 1000)` to get a timestamp and
  `epoch_ms(now() - INTERVAL 7 DAY)` to compare. `durationMs` is the session
  length.
- In `sessions`, `tMs` is milliseconds since the session started, not an
  epoch: plot it as `tMs / 1000.0` seconds. `receivedAtMs` is when the
  sample's batch arrived, so many samples share it; never use it as a
  sample's time axis. `startedAtMs` is set only on `kind = 'meta'` lines,
  never on samples.
- `sessions` and `relay` are large; always filter their `date` partition
  column (a DATE) so DuckDB prunes files. `rollups` is small and can be
  scanned whole.
- `series.<field>` holds per-session percentiles of a gauge (n, min, p05,
  median, p95, max). `counters.<field>` holds the session's final value of a
  cumulative counter.
- Viewer experience signals: `stalls`, `totalStallMs`, `longestStallMs`
  (playback freezes); `series.receivedFps` / `series.renderedFps`; latency
  `series.capToRenderMs` (glass-to-glass, capture to render, ms); frame loss
  `counters.framesDroppedIncomplete` (frames lost to datagram loss). A loss
  ratio is lost / (lost + `counters.decodedFrames`): the denominator always
  includes the numerator, so the ratio stays within 0..1; `reconnects`;
  `verdict.healthy` and
  `verdict.findings[].id` (the automatic diagnosis).
- Broadcaster uplink loss: `counters.uplinkPacketsLost` /
  `counters.uplinkPacketsSent` (native broadcasters only).
  `series.viewerCount` is the viewer count the relay reported to the
  broadcaster during its session.
- In `relay`, `broadcast.viewersGlobal` is the live viewer count of a
  broadcast across the fleet at that scrape.
- `verdict.findings` is a LIST of structs; use `unnest` to count findings.
- Avoid reserved words as aliases (`day`, `share`, `value`, `from`, `scope`).
