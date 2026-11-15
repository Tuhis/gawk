//go:build race

package bridge

// raceEnabled skips the freeze-timing tests: the race detector puts
// 100–200 ms of noise on a single Mumble move, the size of the freezes they
// measure. CI runs them in a step of their own without it.
const raceEnabled = true
