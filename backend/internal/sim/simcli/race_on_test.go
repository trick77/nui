//go:build race

package simcli

// raceOn: SQLite runs in wazero, which the race detector slows by two orders of magnitude; the histories shrink.
const raceOn = true
