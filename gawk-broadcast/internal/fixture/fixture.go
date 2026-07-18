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
