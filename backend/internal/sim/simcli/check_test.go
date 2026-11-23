package simcli

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trick77/stakeout/backend/internal/sim"
	"github.com/trick77/stakeout/backend/internal/store"
)

var anchor = time.Date(2026, 11, 22, 13, 30, 0, 0, time.UTC)

// skipRace skips a test -race makes too slow for CI's ten minutes a package: the gate runs it whole without (ship.yaml).
func skipRace(t *testing.T) {
	t.Helper()
	if raceOn {
		t.Skip("run without -race (ship.yaml's step)")
	}
}

// history is the default sites' history with the named scenarios: its store and world.
func history(t testing.TB, scale float64, days int, scenarios string) (*store.Store, *sim.World) {
	t.Helper()
	return seededHistory(t, 7, scale, days, scenarios)
}

// seededHistory is a history built once for every test that asks for it (the growth scenario's half year is read by
// two tests, the baseline's days by two under -race): outside any one test's directory and context, so the first
// test to end closes nothing the others read, and removed when the package's tests end (TestMain). Its world is
// shared too: a test changes a copy.
func seededHistory(t testing.TB, seed uint64, scale float64, days int, scenarios string) (*store.Store, *sim.World) {
	t.Helper()
	classifyLast := 0
	if days > 30 {
		classifyLast = 9
	}
	return historyOf(t, seed, scale, days, scenarios, classifyLast)
}

// classifiedHistory is seededHistory with the classifier's pass after every day, however long: as a live server
// stamps each day. Without it the older days keep the writer's instant classes, and a session's rows fall into both
// parts (people and automated), which the aggregates count once each.
func classifiedHistory(t testing.TB, seed uint64, scale float64, days int, scenarios string) (*store.Store,
	*sim.World) {
	t.Helper()
	return historyOf(t, seed, scale, days, scenarios, 0)
}

func historyOf(t testing.TB, seed uint64, scale float64, days int, scenarios string, classifyLast int) (*store.Store,
	*sim.World) {
	t.Helper()
	key := fmt.Sprint(seed, scale, days, scenarios, classifyLast)
	shared.Lock()
	h, ok := shared.histories[key]
	if !ok {
		h = &sharedHistory{}
		shared.histories[key] = h
	}
	shared.Unlock()
	h.once.Do(func() { h.err = h.build(seed, scale, days, scenarios, classifyLast) })
	if h.err != nil {
		t.Fatal(h.err)
	}
	return h.st, h.w
}

var shared = struct {
	sync.Mutex
	histories map[string]*sharedHistory
}{histories: map[string]*sharedHistory{}}

type sharedHistory struct {
	once sync.Once
	st   *store.Store
	w    *sim.World
	dir  string
	err  error
}

func TestMain(m *testing.M) {
	// Two ranges keep an export short (the long ones read a year a day at a time; the command exports all five), one
	// under -race. Set before any test runs, not by the export's test: that one runs beside the others.
	exportRanges = []string{"24h", "7d"}
	if raceOn {
		exportRanges = exportRanges[:1]
	}
	code := m.Run()
	for _, h := range shared.histories {
		if h.st != nil {
			_ = h.st.Close()
		}
		if h.dir != "" {
			_ = os.RemoveAll(h.dir)
		}
	}
	os.Exit(code)
}

func (h *sharedHistory) build(seed uint64, scale float64, days int, scenarios string, classifyLast int) error {
	ctx := context.Background()
	var err error
	if h.dir, err = os.MkdirTemp("", "stakeout-simcli-test-"); err != nil {
		return err
	}
	ref, err := sim.OpenReference(filepath.Join(h.dir, "ref"))
	if err != nil {
		return err
	}
	sc, err := sim.Scenarios(scenarios)
	if err != nil {
		return err
	}
	// A half year (growth) on the first site alone, which its expectation reads: a third of the work.
	var ss []*sim.Site
	o := sim.BackfillOptions{Days: days, Classify: true, ClassifyLast: classifyLast}
	if days > 30 {
		ss = sim.DefaultSites()[:1]
	}
	if h.w, err = sim.NewWorld(sim.Options{Seed: seed, Anchor: anchor, Scale: scale, Sites: ss, Scenarios: sc,
		Ref: ref}); err != nil {
		return err
	}
	if h.st, err = store.Open(ctx, filepath.Join(h.dir, "app.db")); err != nil {
		return err
	}
	if _, err := h.w.SyncSites(ctx, h.st); err != nil {
		return err
	}
	if _, err = sim.Backfill(ctx, h.st, h.w, o); err != nil {
		return err
	}
	// Built here, before any test reads it: Check's own build then writes nothing beside the other tests' reads.
	return buildAggregates(ctx, h.st, h.w)
}

// The checks are not vacuous: a quiet history fails every story it did not play.
func TestExpectationsFailWithoutTheirStory(t *testing.T) {
	t.Parallel()
	scale := 1.0
	if raceOn {
		scale = 0.3
	}
	st, quiet := history(t, scale, 3, "baseline")
	w := *quiet // the history is shared: its world stays as it is
	w.Scenarios, _ = sim.Scenarios("all")
	fails, err := Check(t.Context(), st, &w)
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
	skipRace(t)
	var names []string
	for _, c := range sim.Catalogue() {
		if c.MinDays <= 30 {
			names = append(names, c.Name)
		}
	}
	st, w := history(t, 1, 9, strings.Join(names, ","))
	fails, err := Check(t.Context(), st, w)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fails {
		t.Error(f)
	}
}

// Growth shows whatever the seed: its visitors rise far past Noteworthy's 20 % (by 44 to 70 % over seeds 1 to 8 at
// this scale), not at the threshold. A half year's history is the package's longest build: the dashboard reads the
// scenario test's and more seeds', without -race; every seed is checked on the world's own visitors, which agree
// with the dashboard's where both are read (within 4 points).
func TestGrowthAnySeed(t *testing.T) {
	t.Parallel()
	dashboard := []uint64{7, 1, 2, 3, 4}
	if raceOn {
		dashboard = nil
	}
	ref, err := sim.OpenReference(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range dashboard {
		t.Run("dashboard/"+strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			st, w := seededHistory(t, seed, 0.3, 180, "growth")
			if seed != 7 { // the scenario test checks seed 7's expectations
				fails, err := Check(t.Context(), st, w)
				if err != nil {
					t.Fatal(err)
				}
				for _, f := range fails {
					t.Error(f)
				}
			}
			pct := visitorsUp(t, st, w)
			own := worldVisitorsUp(t, ref, seed)
			t.Logf("visitors up %.1f %% on the dashboard, %.1f %% in the world", pct, own)
			if pct < 40 {
				t.Errorf("visitors up %v %%, too near the threshold", pct)
			}
			if math.Abs(pct-own) > 10 {
				t.Errorf("the world's visitors up %.1f %%, the dashboard's %.1f %%", own, pct)
			}
		})
	}
	for seed := range uint64(8) {
		t.Run("world/"+strconv.FormatUint(seed+1, 10), func(t *testing.T) {
			t.Parallel()
			pct := worldVisitorsUp(t, ref, seed+1)
			t.Logf("visitors up %.1f %%", pct)
			if pct < 40 {
				t.Errorf("visitors up %.1f %%, too near the threshold", pct)
			}
		})
	}
}

// visitorsUp is the overview's change of visitors over 90 days against the 90 before.
func visitorsUp(t *testing.T, st *store.Store, w *sim.World) float64 {
	t.Helper()
	code, b := dashboardOf(t, st, w).Get(t.Context(), "overview", map[string]string{
		"site": "lens.example.com", "range": "90d", "tz": "UTC"})
	var body map[string]any
	if err := json.Unmarshal(b, &body); code != 200 || err != nil {
		t.Fatalf("overview: %d %v", code, err)
	}
	for _, x := range body["tiles"].([]any) {
		if m := x.(map[string]any); m["key"] == "visitors" {
			pct, _ := m["change"].(map[string]any)["pct"].(float64)
			return pct
		}
	}
	t.Fatal("no visitors tile")
	return 0
}

// worldVisitorsUp is the same change on the growth world's own clients, no history written: the distinct visitor
// ids the lookup site's tracker batches carry in the overview's 90 day buckets (the last one open at the anchor)
// against the 90 before, up to the anchor's hour.
func worldVisitorsUp(t *testing.T, ref *sim.Reference, seed uint64) float64 {
	t.Helper()
	sc, _ := sim.Scenarios("growth")
	w, err := sim.NewWorld(sim.Options{Seed: seed, Anchor: anchor, Scale: 0.3, Sites: sim.DefaultSites()[:1],
		Scenarios: sc, Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	visitors := func(from, to time.Time) float64 {
		ids := map[string]bool{}
		for d := from; d.Before(to); d = d.Add(24 * time.Hour) {
			end := d.Add(24 * time.Hour)
			if end.After(to) {
				end = to
			}
			for _, q := range w.Requests(d, end) {
				var b struct {
					VID string `json:"vid"`
				}
				if !q.Feed && json.Unmarshal(q.Body(nil), &b) == nil {
					ids[b.VID] = true
				}
			}
		}
		return float64(len(ids))
	}
	from := anchor.Truncate(24*time.Hour).AddDate(0, 0, -89)
	cur := visitors(from, anchor)
	prev := visitors(from.AddDate(0, 0, -90), anchor.Truncate(time.Hour).AddDate(0, 0, -90))
	return 100 * (cur - prev) / prev
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

// Every scenario, played with the baseline alone, shows on the real dashboard what it says it shows. The tests
// that set a package variable (checkWorld) or time the wall clock (live sends) are not parallel: they run before the
// parallel ones, alone. This one is: its subtests would hold the parallel ones back until they end.
func TestScenarioExpectations(t *testing.T) {
	t.Parallel()
	for _, sc := range sim.Catalogue() {
		t.Run(sc.Name, func(t *testing.T) {
			t.Parallel()
			// Half a year (growth) at 0.3, not under -race.
			days, scale := max(sc.MinDays, 9), 1.0
			if days > 30 {
				skipRace(t)
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
