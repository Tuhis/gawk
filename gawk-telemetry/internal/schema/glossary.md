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
- `config.pipelineContext` says whether a browser broadcaster ran its
  pipeline in a worker or on the main thread, and
  `config.pipelineFallbackReason` says why it ran on the main thread (a
  sentence naming the missing capability; absent when it ran in the worker).
  Group main-thread broadcaster rows by it to see what kept them off the
  worker.
- In `relay`, `broadcast.viewersGlobal` is the live viewer count of a
  broadcast across the fleet at that scrape, and every pod carrying the
  broadcast reports it. For the fleet's viewers at a moment, take one value
  per `broadcastKey` in that moment (`max`, not `sum`, across pods), then
  sum across broadcasts: one broadcast's peak is not the fleet's.
- In `relay`, a striped viewer is one primary subscriber plus up to four
  stripe leg sessions, possibly on other pods. A leg is never a `subscriber`
  row (`kind = 'subscriber'` lines are viewers only); legs appear inside
  `broadcast.subscriberDetails[]` with `broadcast.subscriberDetails[].stripeLeg`
  true and no `sessionId`. A viewer's primary entry and its legs carry the
  same `broadcast.subscriberDetails[].stripeOwner`, on any pod. Count viewers
  from entries whose `broadcast.subscriberDetails[].stripeLeg` and
  `broadcast.subscriberDetails[].internal` are both not true.
- `verdict.findings` is a LIST of structs; use `unnest` to count findings.
- Avoid reserved words as aliases (`day`, `share`, `value`, `from`, `scope`).
