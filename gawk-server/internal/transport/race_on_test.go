//go:build race

package transport

// raceEnabled reports a -race build, where sync.Pool drops a share of what
// is put back on purpose.
const raceEnabled = true
