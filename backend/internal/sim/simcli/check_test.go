package simcli

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/trick77/nui/backend/internal/sim"
	"github.com/trick77/nui/backend/internal/store"
)

var anchor = time.Date(2026, 11, 22, 13, 30, 0, 0, time.UTC)

// history backfills the default sites with the named scenarios and returns the store and world.
func history(t testing.TB, scale float64, days int, scenarios string) (*store.Store, *sim.World) {
	t.Helper()
	ref, err := sim.OpenReference(filepath.Join(t.TempDir(), "ref"))
	if err != nil {
		t.Fatal(err)
	}
	sc, err := sim.Scenarios(scenarios)
	if err != nil {
		t.Fatal(err)
	}
	w, err := sim.NewWorld(sim.Options{Seed: 7, Anchor: anchor, Scale: scale, Scenarios: sc, Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "nui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := w.SyncSites(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	if _, err := sim.Backfill(t.Context(), st, w, sim.BackfillOptions{Days: days, Classify: true}); err != nil {
		t.Fatal(err)
	}
	return st, w
}

// The checks are not vacuous: a quiet history fails every story it did not play.
func TestExpectationsFailWithoutTheirStory(t *testing.T) {
	scale := 1.0
	if raceOn {
		scale = 0.3
	}
	st, w := history(t, scale, 3, "baseline")
	sc, _ := sim.Scenarios("all")
	w.Scenarios = sc
	fails, err := Check(t.Context(), st, w)
	if err != nil {
		t.Fatal(err)
	}
	failed := map[string]bool{}
	for _, f := range fails {
		failed[f.Expectation.Scenario] = true
	}
	for _, c := range sim.Catalogue() {
		if c.Name != "baseline" && !failed[c.Name] {
			t.Errorf("%s: its expectations hold without it", c.Name)
		}
	}
	if failed["baseline"] {
		t.Errorf("the baseline fails: %v", fails)
	}
}

// Every scenario, played with the baseline alone, shows on the real dashboard what it says it shows.
func TestScenarioExpectations(t *testing.T) {
	for _, sc := range sim.Catalogue() {
		t.Run(sc.Name, func(t *testing.T) {
			t.Parallel()
			days, scale := max(sc.MinDays, 9), 1.0
			if days > 30 {
				scale = 0.3
			}
			if raceOn {
				if days > 30 {
					t.Skip("half a year of history: checked without -race")
				}
				days, scale = max(sc.MinDays, 3), 0.3
			}
			st, w := history(t, scale, days, sc.Name)
			fails, err := Check(t.Context(), st, w)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range fails {
				t.Error(f)
			}
		})
	}
}
