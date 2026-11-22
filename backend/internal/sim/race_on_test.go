//go:build race

package sim

// raceOn: SQLite runs in wazero, which the race detector slows by two orders of magnitude; the backfills shrink.
const raceOn = true
