//go:build race

package wire

// raceEnabled reports a -race build. The parity sweep in parity_test.go
// uses it to sample the erasure-pair space instead of walking all of it:
// the code under test is single-goroutine, so the race detector finds
// nothing there and only multiplies its cost (TestRecoverAllErasurePairs:
// ~3s without -race; with it ~100s locally and ~160s in CI, measured
// 2026-11-01). The full exhaustive sweep still runs in every non-race
// build, which includes CI's server-coverage job.
const raceEnabled = true
