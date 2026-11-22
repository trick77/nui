package simcli

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/trick77/nui/backend/internal/sim"
	"github.com/trick77/nui/backend/internal/store"
)

var anchor = time.Date(2026, 11, 22, 13, 30, 0, 0, time.UTC)

// history backfills the default sites with the named scenarios and returns the store and world.
func history(t testing.TB, scale float64, days int, scenarios string) (*store.Store, *sim.World) {
	t.Helper()
	return seededHistory(t, 7, scale, days, scenarios)
}

func seededHistory(t testing.TB, seed uint64, scale float64, days int, scenarios string) (*store.Store, *sim.World) {
	t.Helper()
	ref, err := sim.OpenReference(filepath.Join(t.TempDir(), "ref"))
	if err != nil {
		t.Fatal(err)
	}
	sc, err := sim.Scenarios(scenarios)
	if err != nil {
		t.Fatal(err)
	}
	// A half year (growth) on the first site alone, which its expectation reads: a third of the work, so the gate
	// affords it under -race.
	var ss []*sim.Site
	o := sim.BackfillOptions{Days: days, Classify: true}
	if days > 30 {
		ss, o.ClassifyLast = sim.DefaultSites()[:1], 9
	}
	w, err := sim.NewWorld(sim.Options{Seed: seed, Anchor: anchor, Scale: scale, Sites: ss, Scenarios: sc, Ref: ref})
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
	if _, err := sim.Backfill(t.Context(), st, w, o); err != nil {
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

// Played together, the scenarios still show what each says: Noteworthy's findings past the five it shows are
// checked too.
func TestScenariosTogether(t *testing.T) {
	t.Parallel()
	var names []string
	for _, c := range sim.Catalogue() {
		if c.MinDays <= 30 {
			names = append(names, c.Name)
		}
	}
	days, scale := 9, 1.0
	if raceOn {
		days, scale = 7, 0.3
	}
	st, w := history(t, scale, days, strings.Join(names, ","))
	fails, err := Check(t.Context(), st, w)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fails {
		t.Error(f)
	}
}

// Growth shows whatever the seed: its visitors rise far past Noteworthy's 20 % (by 47 to 73 % over eight seeds at
// this scale), not at the threshold.
func TestGrowthAnySeed(t *testing.T) {
	t.Parallel()
	seeds := []uint64{1, 2, 3, 4}
	if raceOn {
		seeds = seeds[:2] // the scenario test runs another
	}
	for _, seed := range seeds {
		t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			st, w := seededHistory(t, seed, 0.3, 180, "growth")
			fails, err := Check(t.Context(), st, w)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range fails {
				t.Error(f)
			}
			code, b := dashboardOf(t, st, w).Get(t.Context(), "overview", map[string]string{
				"site": "lens.example.com", "range": "90d", "tz": "UTC"})
			var body map[string]any
			if err := json.Unmarshal(b, &body); code != 200 || err != nil {
				t.Fatalf("overview: %d %v", code, err)
			}
			for _, x := range body["tiles"].([]any) {
				if m := x.(map[string]any); m["key"] == "visitors" {
					if pct, _ := m["change"].(map[string]any)["pct"].(float64); pct < 40 {
						t.Errorf("visitors up %v %%, too near the threshold", pct)
					}
				}
			}
		})
	}
}

// dashboardOf is NewDashboard in a test.
func dashboardOf(t *testing.T, st *store.Store, w *sim.World) *Dashboard {
	t.Helper()
	d, err := NewDashboard(t.Context(), st, w)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Every scenario, played with the baseline alone, shows on the real dashboard what it says it shows.
func TestScenarioExpectations(t *testing.T) {
	for _, sc := range sim.Catalogue() {
		t.Run(sc.Name, func(t *testing.T) {
			t.Parallel()
			// Half a year (growth) at 0.3, under -race too: the gate runs every scenario.
			days, scale := max(sc.MinDays, 9), 1.0
			if days > 30 {
				scale = 0.3
			}
			if raceOn {
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
