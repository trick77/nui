//go:build race

package dash

// raceOn: SQLite runs in wazero, which the race detector slows by two orders of magnitude.
const raceOn = true
