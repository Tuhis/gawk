// Package fixture holds the committed H.264 MPEG-TS test stream (see
// README.md for its provenance and the properties tests depend on).
//
// It is embedded rather than read from testdata so gawk-pubsim (R20 Z1,
// docs/25 Decision 3) is a self-contained binary — the manual scale drill
// copies one file, and the CI harness never depends on a working directory.
// The package deliberately imports nothing, so any test package (including
// white-box ones like mpegts's) can use it without an import cycle.
package fixture

import _ "embed"

// TS is the raw MPEG-TS fixture: 320x240 @ 30 fps, 60 frames, GOP 15
// (= 500 ms at 30 fps), no B-frames, SPS/PPS before every IDR. Treat it as
// read-only — it is shared by every test in the process.
//
//go:embed sample.ts
var TS []byte

// TSLarge is the R30 large-frame fixture (docs/35 §12): same container and
// encoder invariants as TS, but 1280x720 temporal noise at ~5 Mbps, so every
// DELTA frame exceeds the ~8-chunk burst threshold (docs/34 finding 4) and a
// striped viewer actually engages. The default fixture's ~2–4-chunk deltas
// sit under one stripe share, which is exactly why the striped e2e pass
// needs this one — and why TestLargeFixtureDeltasExceedBurstThreshold pins
// the property: a regenerated clip that compresses under the threshold would
// silently turn that pass into the designed no-op.
//
//go:embed sample-large.ts
var TSLarge []byte
