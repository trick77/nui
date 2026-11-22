package sim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// The simulator marks what it writes, so nothing it leaves behind passes for real data. OwnerFile in a data
// directory: the simulator wrote its history (sim backfill, server -demo, server -sim). RefMarkerFile in a reference
// directory: the reference data there is the world's made-up one, which a normal server discards.
const (
	OwnerFile     = "nui-sim.json"
	RefMarkerFile = "nui-sim-ref.json"
)

// mark writes a marker file saying who wrote the directory and why it matters.
func mark(path, what string) error {
	b, err := json.MarshalIndent(map[string]string{"written_by": "nui sim", "what": what}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// Owned reports whether the simulator wrote the data directory dir.
func Owned(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, OwnerFile))
	return err == nil
}

// Mark marks dir as the simulator's: its events are synthetic.
func Mark(dir string) error {
	return mark(filepath.Join(dir, OwnerFile), "synthetic traffic: this directory's events are simulated")
}

// Claim makes dir a data directory the simulator may write a history into: an absent or empty one, or one it wrote
// before and that holds no history yet. A directory with anything else in it is refused, never written to; wipe
// empties one the simulator wrote first (its history refilled), and is refused for any other. The directory is
// marked as the simulator's.
func Claim(dir string, wipe bool) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	case err != nil:
		return err
	case len(entries) == 0:
	case !Owned(dir):
		return fmt.Errorf("%s is not empty and was not written by the simulator (no %s): the simulator writes only "+
			"into an empty directory or one it wrote, never over real data", dir, OwnerFile)
	case wipe:
		for _, e := range entries {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	default:
		if _, err := os.Stat(filepath.Join(dir, "nui.db")); err == nil {
			return fmt.Errorf("%s holds a simulated history already (nui.db): another one would double it; -wipe "+
				"empties the directory and writes it anew", dir)
		}
	}
	return Mark(dir)
}

// SimulatedReference reports whether the reference directory dir holds the world's made-up data.
func SimulatedReference(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, RefMarkerFile))
	return err == nil
}

// DiscardReference removes the world's made-up reference data from dir (a normal server's cache), so the real data
// is fetched in its place. A directory without the marker is left alone.
func DiscardReference(dir string) error {
	if !SimulatedReference(dir) {
		return nil
	}
	for _, f := range []string{crawlersFile, monitorFile, networksFile, countriesFile, RefMarkerFile} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
