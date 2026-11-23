package dash

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trick77/stakeout/backend/internal/agg"
	"github.com/trick77/stakeout/backend/internal/store"
)

// build builds every day of the rig's store up to its end, as BuildThrough does for a history.
func (r *rig) build(t *testing.T) {
	t.Helper()
	if _, err := agg.BuildThrough(t.Context(), r.s.DB, r.end, agg.TopK); err != nil {
		t.Fatal(err)
	}
}

// pieceCount counts the pieces read from each source while f runs: from the aggregates, raw on the tables' grain
// (pieces the aggregates could have served), raw off it.
type pieceCount struct{ agg, raw, off int }

func countPieces(t *testing.T, f func()) pieceCount {
	t.Helper()
	var mu sync.Mutex
	var c pieceCount
	traceRead = func(fromAgg, aligned bool) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case fromAgg:
			c.agg++
		case aligned:
			c.raw++
		default:
			c.off++
		}
	}
	defer func() { traceRead = nil }()
	f()
	return c
}

// getFrom asks the endpoint with the server reading from src.
func (r *rig) getFrom(t *testing.T, src Source, endpoint string, params map[string]string) (int, map[string]any) {
	t.Helper()
	r.s.Source = src
	r.s.snaps = snapshots{}
	defer func() { r.s.Source, r.s.snaps = SourceAuto, snapshots{} }()
	return r.get(t, endpoint, params)
}

// distinctTol is how far a distinct count from sketches may be from the exact one: index collisions, rare in the
// sparse form.
func distinctTol(a, b float64) bool {
	return math.Abs(a-b) <= math.Max(1, 0.001*math.Max(math.Abs(a), math.Abs(b)))
}

// agree compares two answers field by field: equal, but inside a loose subtree (a distinct figure) numbers within
// distinctTol, and the change and verdict derived from them not at all.
func agree(t *testing.T, where string, raw, got any, loose func(path string, v any) bool) {
	t.Helper()
	var walk func(path string, a, b any, isLoose bool)
	walk = func(path string, a, b any, isLoose bool) {
		isLoose = isLoose || loose(path, a)
		switch x := a.(type) {
		case map[string]any:
			y, ok := b.(map[string]any)
			if !ok || len(x) != len(y) {
				t.Errorf("%s %s: %v vs %v", where, path, a, b)
				return
			}
			for k, v := range x {
				if isLoose && (k == "change" || k == "verdict") {
					continue
				}
				walk(path+"."+k, v, y[k], isLoose)
			}
		case []any:
			y, ok := b.([]any)
			if !ok || len(x) != len(y) {
				t.Errorf("%s %s: %d items vs %v", where, path, len(x), b)
				return
			}
			for i := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), x[i], y[i], isLoose)
			}
		case float64:
			y, ok := b.(float64)
			if !ok || (x != y && !(isLoose && distinctTol(x, y))) {
				t.Errorf("%s %s: raw %v, aggregates %v", where, path, a, b)
			}
		default:
			if fmt.Sprint(a) != fmt.Sprint(b) {
				t.Errorf("%s %s: raw %v, aggregates %v", where, path, a, b)
			}
		}
	}
	walk("", raw, got, false)
}

// looseTiles marks the distinct figures of the overview and the classes: the visitors tile, addresses per class.
func looseTiles(path string, v any) bool {
	if m, ok := v.(map[string]any); ok && m["key"] == "visitors" {
		return true
	}
	return strings.HasSuffix(path, ".addresses")
}

// The tiles, the classes and Noteworthy's trends read the same from the hourly aggregates as from raw, in every zone
// and comparison; the aggregates were read, and no piece on their grain was read raw.
func TestSources_Hourly(t *testing.T) {
	r := newRig(t, 20, 12, nil)
	for _, tz := range []string{"UTC", "Europe/Zurich", "Asia/Kolkata", "America/New_York"} {
		for _, rng := range []string{"24h", "7d", "30d", "90d", "12mo"} {
			if rng == "12mo" && tz != "UTC" { // a year of pieces: once is enough
				continue
			}
			for _, c := range []string{"prev", "yoy"} {
				auds := []string{"all"}
				if rng == "30d" {
					auds = []string{"all", "humans", "bots"}
				}
				for _, aud := range auds {
					params := p(feedSite, "range", rng, "cmp", c, "aud", aud, "tz", tz)
					where := fmt.Sprint(params)
					for _, ep := range []string{"overview", "classes"} {
						_, raw := r.getFrom(t, SourceRaw, ep, params)
						var got map[string]any
						n := countPieces(t, func() { _, got = r.getFrom(t, SourceAgg, ep, params) })
						delete(raw, "live")
						delete(got, "live")
						agree(t, ep+" "+where, raw, got, looseTiles)
						// Kolkata's 24h and 7d hours are on the half hour: read raw, under every source.
						offGrain := tz == "Asia/Kolkata" && (rng == "24h" || rng == "7d")
						if (n.agg == 0 && !offGrain) || n.raw != 0 || (n.off != 0 && !offGrain) {
							t.Errorf("%s %s: pieces %+v", ep, where, n)
						}
					}
				}
			}
		}
	}
}

// The source of each day: auto reads 24h and 7d raw, every clean day of a longer range from the aggregates, a dirty
// or unbuilt day raw, and nothing before the first event; SourceAgg fails on a day with raw but no clean build.
func TestSources_Plan(t *testing.T) {
	r, insert := exactRig(t)
	insert(pageLoad(at("2026-11-02T10:00:00Z"), "a", "192.0.2.1", "CH", "Mozilla/5.0", 0)...)
	insert(pageLoad(at("2026-11-12T10:00:00Z"), "b", "192.0.2.2", "CH", "Mozilla/5.0", 0)...)
	r.build(t)
	visitors := func(src Source, rng string) (float64, pieceCount) {
		t.Helper()
		var body map[string]any
		var code int
		n := countPieces(t, func() { code, body = r.getFrom(t, src, "overview", p(feedSite, "range", rng, "tz", "UTC")) })
		if code != http.StatusOK {
			t.Fatalf("%v %s: %d %v", src, rng, code, body)
		}
		return num64(tiles(t, body)["visitors"]["value"]), n
	}
	if v, n := visitors(SourceAuto, "7d"); v != 0 || n.agg != 0 || n.raw == 0 {
		t.Errorf("auto 7d: %v visitors, pieces %+v", v, n)
	}
	v, n := visitors(SourceAuto, "30d")
	// The 21 built days from 2 to 22 November; 24 October to 1 November, before the first event, are not read.
	if v != 2 || n.agg != 21 || n.raw != 0 {
		t.Errorf("auto 30d: %v visitors, pieces %+v", v, n)
	}
	// A write to a built day shows at once, read raw, and again once rebuilt.
	insert(pageLoad(at("2026-11-12T11:00:00Z"), "c", "192.0.2.3", "CH", "Mozilla/5.0", 0)...)
	if v, n := visitors(SourceAuto, "30d"); v != 3 || n.raw != 1 {
		t.Errorf("after the write: %v visitors, pieces %+v", v, n)
	}
	if code, body := r.getFrom(t, SourceAgg, "overview", p(feedSite, "range", "30d", "tz", "UTC")); code != 500 {
		t.Errorf("SourceAgg on a dirty day: %d %v", code, body)
	}
	r.build(t)
	if v, n := visitors(SourceAuto, "30d"); v != 3 || n.raw != 0 {
		t.Errorf("after the rebuild: %v visitors, pieces %+v", v, n)
	}
	// A day whose raw retention cut (raw = 0) is read from its build, or as nothing when it has none.
	site, _ := r.s.Sites.Lookup(feedSite)
	if _, err := r.s.DB.DB.Exec(`UPDATE agg_day SET raw = 0 WHERE site_id = ? AND day = ?`, site.ID,
		at("2026-11-12T00:00:00Z")/dayMS); err != nil {
		t.Fatal(err)
	}
	if v, _ := visitors(SourceRaw, "30d"); v != 3 {
		t.Errorf("raw source: %v", v)
	}
	if _, err := r.s.DB.DB.Exec(`UPDATE agg_day SET frozen = 0 WHERE site_id = ? AND day = ?`, site.ID,
		at("2026-11-12T00:00:00Z")/dayMS); err != nil {
		t.Fatal(err)
	}
	if v, _ := visitors(SourceAuto, "30d"); v != 1 {
		t.Errorf("a cut day without a build: %v visitors, want only a's", v)
	}
	if v, _ := visitors(SourceAgg, "30d"); v != 1 {
		t.Errorf("SourceAgg on a cut day without a build: %v", v)
	}
}

// A rebuild under way (AggWriting between its first and last transaction) is not read: the statement's own snapshot
// sees the day out of reading and the piece goes raw.
func TestSources_RebuildWhileRead(t *testing.T) {
	r, insert := exactRig(t)
	insert(pageLoad(at("2026-11-12T10:00:00Z"), "a", "192.0.2.1", "CH", "Mozilla/5.0", 0)...)
	r.build(t)
	site, _ := r.s.Sites.Lookup(feedSite)
	day := at("2026-11-12T00:00:00Z") / dayMS
	q := &query{site: site, aud: "all", p: newPeriod("30d", "off", r.end, time.UTC)}
	pl, err := r.s.planOf(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	// The plan says built and clean; the job takes the day out and deletes its rows before the read.
	if _, err := r.s.DB.DB.Exec(`UPDATE agg_day SET dirty = ? WHERE site_id = ? AND day = ?`, store.AggWriting,
		site.ID, day); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.DB.DB.Exec(`DELETE FROM agg_hour WHERE site_id = ?`, site.ID); err != nil {
		t.Fatal(err)
	}
	b, err := r.s.bucket(t.Context(), pl, "all", time.UnixMilli(day*dayMS), time.UnixMilli((day+1)*dayMS), false)
	if err != nil || b.views != 1 || b.visitors.count() != 1 {
		t.Errorf("a day rebuilt while read: %+v, %v", b, err)
	}
	pl.src = SourceAgg
	if _, err := r.s.bucket(t.Context(), pl, "all", time.UnixMilli(day*dayMS), time.UnixMilli((day+1)*dayMS),
		false); !errors.Is(err, ErrNoAggregates) {
		t.Errorf("SourceAgg on a day rebuilt while read: %v", err)
	}
}

// uniq is exact while it holds ids only, and merges a stored sketch.
func TestUniq(t *testing.T) {
	var u uniq
	for i := range 100 {
		u.add(fmt.Sprint("v", i%60))
	}
	if u.count() != 60 || u.sk != nil {
		t.Errorf("exact: %d", u.count())
	}
	s := agg.NewSketch()
	for i := 40; i < 140; i++ {
		s.AddString(fmt.Sprint("v", i))
	}
	if err := u.addSketch(s.Marshal()); err != nil || u.count() != 140 {
		t.Errorf("with a sketch: %d, %v", u.count(), err)
	}
	var w uniq
	w.add("v1")
	w.merge(&u)
	if w.count() != 140 || w.sk == nil {
		t.Errorf("merged: %d", w.count())
	}
	if err := w.addSketch([]byte{9}); err == nil {
		t.Error("a bad sketch merged")
	}
}
