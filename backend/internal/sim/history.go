package sim

import (
	"context"
	"os"
	"time"

	"github.com/trick77/nui/backend/internal/store"
)

// HistoryOptions shape History.
type HistoryOptions struct {
	Seed uint64
	// Anchor is where the history ends; zero is now (not reproducible).
	Anchor time.Time
	Days   int
	// Scale is the volume: 1 is about 25 visitors a day per site.
	Scale float64
	// Scenarios is a comma-separated list; baseline is always played.
	Scenarios string
	// RefDir holds the reference data: the world's is written there when it holds none. Empty: a temporary
	// directory, removed once the history is written.
	RefDir string
	// Classify runs the classifier's pass after every day.
	Classify bool
}

// History backfills a seeded history into st on the sites configured there (or the default sites, synced, when st
// has none) and returns its world, whose Expectations say what the dashboard shows for it. For a test:
//
//	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "nui.db"))
//	w, _, err := sim.History(ctx, st, sim.HistoryOptions{Seed: 7, Anchor: end, Days: 14, Scale: 1,
//		Scenarios: "api-walker", Classify: true})
func History(ctx context.Context, st *store.Store, o HistoryOptions) (*World, Stats, error) {
	if o.RefDir == "" {
		dir, err := os.MkdirTemp("", "nui-sim-ref-")
		if err != nil {
			return nil, Stats{}, err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		o.RefDir = dir
	}
	ref, err := OpenReference(o.RefDir)
	if err != nil {
		return nil, Stats{}, err
	}
	sc, err := Scenarios(o.Scenarios)
	if err != nil {
		return nil, Stats{}, err
	}
	ss, err := SitesFrom(ctx, st)
	if err != nil {
		return nil, Stats{}, err
	}
	if len(ss) == 0 {
		ss = nil // the default sites
	}
	if o.Anchor.IsZero() {
		o.Anchor = time.Now()
	}
	w, err := NewWorld(Options{Seed: o.Seed, Anchor: o.Anchor, Scale: o.Scale, Sites: ss, Scenarios: sc, Ref: ref})
	if err != nil {
		return nil, Stats{}, err
	}
	if ss == nil {
		if _, err := w.SyncSites(ctx, st); err != nil {
			return nil, Stats{}, err
		}
	}
	s, err := Backfill(ctx, st, w, BackfillOptions{Days: o.Days, Classify: o.Classify})
	return w, s, err
}
