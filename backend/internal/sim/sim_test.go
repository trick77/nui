package sim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/trick77/nui/backend/internal/ingest"
	"github.com/trick77/nui/backend/internal/store"
)

var anchor = time.Date(2026, 11, 22, 13, 30, 0, 0, time.UTC)

// testWorld is a world over the default sites with the named scenarios, its reference data in a temporary dir.
func testWorld(t testing.TB, scale float64, scenarios string) *World {
	t.Helper()
	return seededWorld(t, 7, scale, scenarios)
}

func seededWorld(t testing.TB, seed uint64, scale float64, scenarios string) *World {
	t.Helper()
	ref, err := OpenReference(filepath.Join(t.TempDir(), "ref"))
	if err != nil {
		t.Fatal(err)
	}
	sc, err := Scenarios(scenarios)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWorld(Options{Seed: seed, Anchor: anchor, Scale: scale, Scenarios: sc, Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func testStore(t testing.TB, w *World) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "nui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := w.SyncSites(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	return st
}

// dump is a canonical digest of what a backfill stored: every event column, the hourly counters, the classes
// (their write time aside, the wall clock's).
func dump(t testing.TB, st *store.Store) string {
	t.Helper()
	h := sha256.New()
	for _, q := range []string{
		`SELECT id, site_id, ts, source, kind, name, visitor, sid, ip, ip_key, ua, path, template, url, ref, ref_host,
			method, status, dur_ms, asn, country, account, cls, props FROM event ORDER BY id`,
		`SELECT site_id, hour, accepted, accepted_server, rate_limited, rejected, dropped, unknown_domain
			FROM ingest_stat ORDER BY site_id, hour`,
		`SELECT site_id, ip_key, day, class, reasons FROM actor_class ORDER BY site_id, ip_key, day`,
	} {
		rows, err := st.DB.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			_, _ = fmt.Fprintln(h, vals...)
		}
		_ = rows.Close()
	}
	return hex.EncodeToString(h.Sum(nil))
}

// The same seed and anchor store the same history, byte for byte as dumped.
func TestBackfillDeterministic(t *testing.T) {
	var digests []string
	days, scale, scenarios := 3, 0.5, "all"
	if raceOn {
		days, scale, scenarios = 1, 0.2, "api-walker,scan-wave,headless,cred-prober,deploy-error"
	}
	for range 2 {
		w := testWorld(t, scale, scenarios)
		st := testStore(t, w)
		s, err := Backfill(t.Context(), st, w, BackfillOptions{Days: days, Classify: true})
		if err != nil || s.Events == 0 {
			t.Fatalf("%v, %v", s, err)
		}
		digests = append(digests, dump(t, st))
		// Every class, every kind of event the tracker and the feed send, nothing in the future.
		var classes, kinds int
		var newest int64
		_ = st.DB.QueryRow(`SELECT COUNT(DISTINCT class) FROM actor_class`).Scan(&classes)
		_ = st.DB.QueryRow(`SELECT COUNT(DISTINCT kind) FROM event`).Scan(&kinds)
		_ = st.DB.QueryRow(`SELECT MAX(ts) FROM event`).Scan(&newest)
		minKinds := 10
		if raceOn {
			minKinds = 8 // a day of a small world may miss a rare kind
		}
		if classes != 4 || kinds < minKinds || newest >= anchor.UnixMilli() {
			t.Errorf("classes %d, kinds %d, newest %v", classes, kinds, time.UnixMilli(newest))
		}
	}
	if digests[0] != digests[1] {
		t.Error("two backfills of one seed differ")
	}
	a := testWorld(t, 0.5, "all").Requests(anchor.Add(-day), anchor)
	b := seededWorld(t, 8, 0.5, "all").Requests(anchor.Add(-day), anchor)
	if string(a[0].Body(nil)) == string(b[0].Body(nil)) {
		t.Error("another seed, the same first request")
	}
}

// The requests of a span are the same whether the world is asked for it at once or a minute at a time.
func TestRequestsIndependentOfTicks(t *testing.T) {
	from, to := anchor.Add(-26*time.Hour), anchor.Add(-20*time.Hour)
	once := testWorld(t, 1, "all").Requests(from, to)
	w := testWorld(t, 1, "all")
	var ticked []*Request
	for at := from; at.Before(to); at = at.Add(time.Minute) {
		ticked = append(ticked, w.Requests(at, at.Add(time.Minute))...)
	}
	if len(once) == 0 || len(once) != len(ticked) {
		t.Fatalf("%d requests at once, %d ticked", len(once), len(ticked))
	}
	for i := range once {
		if !once[i].At.Equal(ticked[i].At) || string(once[i].Body(nil)) != string(ticked[i].Body(nil)) {
			t.Fatalf("request %d differs:\n%s\n%s", i, once[i].Body(nil), ticked[i].Body(nil))
		}
	}
}

// Every request keeps within the collector's limits, and the baseline's clients are all accepted.
func TestRequestsWithinLimits(t *testing.T) {
	w := testWorld(t, 2, "all")
	for _, q := range w.Requests(anchor.Add(-2*day), anchor) {
		body := q.Body(nil)
		switch {
		case q.Feed && (len(body) > ingest.MaxFeedBody || q.Events() > ingest.MaxFeedEvents):
			t.Fatalf("feed batch of %d events, %d bytes", q.Events(), len(body))
		case !q.Feed && (len(body) > ingest.MaxBrowserBody || q.Events() > ingest.MaxBrowserEvents):
			t.Fatalf("tracker batch of %d events, %d bytes", q.Events(), len(body))
		}
	}
	scale := 2.0
	if raceOn {
		scale = 0.3
	}
	w = testWorld(t, scale, "baseline")
	st := testStore(t, w)
	s, err := Backfill(t.Context(), st, w, BackfillOptions{Days: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Refused) != 0 || s.Events < 500 {
		t.Errorf("baseline: %v", s)
	}
	// The collector's counters agree with the events, site by site. (Not hour by hour: an event is dated when it
	// happened, counted when its batch arrived, seconds later.)
	var mismatched int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM (SELECT site_id, COUNT(*) AS n, SUM(source = 'server') AS ns
		FROM event GROUP BY site_id) e LEFT JOIN (SELECT site_id, SUM(accepted) AS a, SUM(accepted_server) AS s
		FROM ingest_stat GROUP BY site_id) c ON c.site_id = e.site_id WHERE c.a IS NOT e.n OR c.s IS NOT e.ns`).
		Scan(&mismatched); err != nil || mismatched != 0 {
		t.Errorf("%d sites whose counters disagree with the events, %v", mismatched, err)
	}
}

// BenchmarkBackfill reports the backfill's rate at a larger scale, without the classifier (go test -bench).
func BenchmarkBackfill(b *testing.B) {
	for b.Loop() {
		w := testWorld(b, 40, "all")
		st := testStore(b, w)
		s, err := Backfill(b.Context(), st, w, BackfillOptions{Days: 2})
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(s.Rate(), "events/s")
		b.ReportMetric(float64(s.Events), "events")
	}
}

// The day plans plan keeps (maxPlans neighbouring days) fit in memory at the largest scale: a day's plan grows with
// the scale, measured here at a small one, day by day over the history's last days (the referrer wave's the largest)
// and the live week after the anchor, at its level.
func TestPlanMemory(t *testing.T) {
	const scale = 4
	w := testWorld(t, scale, DefaultScenarios)
	for d := range int64(4) {
		w.plan(dayOf(anchor) - 3 + d)
	}
	if held := len(w.plans); held > maxPlans {
		t.Errorf("%d day plans held, at most %d", held, maxPlans)
	}
	var sizes []float64 // a day's plan per unit of scale
	for d := dayOf(anchor) - 2; d <= dayOf(anchor)+5; d++ {
		w := testWorld(t, scale, DefaultScenarios)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		w.plan(d)
		runtime.GC()
		runtime.ReadMemStats(&after)
		sizes = append(sizes, (float64(after.HeapAlloc)-float64(before.HeapAlloc))/scale)
		runtime.KeepAlive(w)
	}
	var held, largest float64
	for i, s := range sizes {
		largest = max(largest, s)
		if i > 0 {
			held = max(held, sizes[i-1]+s)
		}
	}
	atMax := held * ScaleLimit.Max()
	t.Logf("a day's plan per unit of scale: %.0f kB at most, %.0f kB on a live weekday; %d days held: %.0f MB at "+
		"scale %v", largest/1024, sizes[3]/1024, maxPlans, atMax/(1<<20), ScaleLimit.Max())
	if atMax > planBudget {
		t.Errorf("%.0f MB of day plans at the largest scale, budget %d MB", atMax/(1<<20), planBudget>>20)
	}
}

// Growth is the history's: live time past the anchor keeps the anchor's level, so a day's plan stays within the
// scale's bound however long a live run goes on (doubling on, a day plan at scale 1 took 897 MB at 900 days).
func TestGrowthStopsAtTheAnchor(t *testing.T) {
	w := testWorld(t, 2, "growth")
	people := func(from int64) int {
		n := 0
		for d := from; d < from+7; d++ {
			for _, b := range w.plan(d).batches {
				if b.actor == "person" {
					n += len(b.events)
				}
			}
		}
		return n
	}
	// A week each, the same weekdays: the week after the anchor, one a year on, one a quarter before.
	first, later, before := people(dayOf(anchor)), people(dayOf(anchor)+364), people(dayOf(anchor)-91)
	t.Logf("people's events a week: %d after the anchor, %d a year on, %d a quarter before", first, later, before)
	if first == 0 || float64(later) > 1.3*float64(first) {
		t.Errorf("a year past the anchor %d events a week, %d the week after it", later, first)
	}
	// The history still grows into the anchor.
	if float64(before) > 0.8*float64(first) {
		t.Errorf("a quarter before the anchor %d events a week, %d after it", before, first)
	}
}

// A small scale is a handful of events, fast.
func TestSmallScale(t *testing.T) {
	start := time.Now()
	w := testWorld(t, 0.02, "baseline")
	st := testStore(t, w)
	s, err := Backfill(t.Context(), st, w, BackfillOptions{Days: 1, Classify: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.Events == 0 || s.Events > 300 {
		t.Errorf("scale 0.02: %v", s)
	}
	w = testWorld(t, 1, "baseline")
	st = testStore(t, w)
	if s, err = Backfill(t.Context(), st, w, BackfillOptions{Days: 1, Classify: true}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second && !raceOn {
		t.Errorf("a day at scale 1 took %v", d)
	}
	t.Log(s)
}
