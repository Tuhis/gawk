//go:build !race

package wire

// raceEnabled reports a -race build. See race_on_test.go for why the parity
// sweep samples under -race and stays exhaustive here.
const raceEnabled = false
