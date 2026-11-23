package sim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trick77/stakeout/backend/internal/ingest"
	"github.com/trick77/stakeout/backend/internal/store"
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
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "app.db"))
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
			method, status, dur_ms, asn, country, account, op, err, unrouted, cls, props FROM event
			ORDER BY id`,
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
		if classes != len(store.ClassNames) || kinds < minKinds || newest >= anchor.UnixMilli() {
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

// Every plan reaches past its day's end within planReach, and a midnight crossed in one span or in many small ones
// gives the same requests, from two day plans at most: the day before's plan read only for as long as it reaches. At
// the largest scale the API's integration runs reach minutes past midnight (at small scales seconds), so a reach too
// short loses requests here; the midnight crossed is the week's a plan reaches furthest past.
func TestRequestsAcrossMidnight(t *testing.T) {
	scale := ScaleLimit.Max()
	ow := testWorld(t, scale, "baseline")
	var mid time.Time
	var furthest time.Duration
	spilled := 0
	for d := dayOf(anchor) - 7; d <= dayOf(anchor); d++ {
		p, end := ow.plan(d-1), dayStart(d)
		n := 0
		past := func(at time.Time) {
			if at.Before(end) {
				return
			}
			n++
			if at.Sub(end) > furthest {
				furthest, mid = at.Sub(end), end
			}
		}
		for _, b := range p.batches {
			past(b.at)
		}
		for _, rs := range p.records {
			for _, r := range rs {
				past(r.t)
			}
		}
		if mid.Equal(end) {
			spilled = n
		}
	}
	t.Logf("a plan reaches %v past its day at most, %d of its items past midnight %s", furthest, spilled,
		mid.Format(time.DateOnly))
	if furthest > planReach || furthest < time.Minute {
		t.Fatalf("a plan reaches %v past its day: planReach %v, the test wants minutes", furthest, planReach)
	}
	from, to := mid.Add(-time.Hour), mid.Add(maxVisit+time.Hour)
	once := ow.Requests(from, to)
	for _, tick := range []time.Duration{time.Hour, 7 * time.Minute, 13 * time.Second} {
		w := testWorld(t, scale, "baseline")
		var ticked []*Request
		for at := from; at.Before(to); at = at.Add(tick) {
			end := earlier(at.Add(tick), to)
			ticked = append(ticked, w.Requests(at, end)...)
			if len(w.plans) > maxPlans {
				t.Fatalf("ticks of %v: %d day plans held after the span %v to %v", tick, len(w.plans), at, end)
			}
		}
		if len(once) != len(ticked) {
			t.Fatalf("ticks of %v: %d requests at once, %d ticked", tick, len(once), len(ticked))
		}
		for i := range once {
			if !once[i].At.Equal(ticked[i].At) || string(once[i].Body(nil)) != string(ticked[i].Body(nil)) {
				t.Fatalf("ticks of %v: request %d differs:\n%s\n%s", tick, i, once[i].Body(nil), ticked[i].Body(nil))
			}
		}
	}
}

// No visit runs longer than maxVisit, and no day plan reaches past its day further than planReach: every session's
// first event to its last, every plan's batches and records, over a week of every scenario's days. Records may come a
// moment before the day (a document is requested before its page shows), within a feed window: they are sent after
// midnight. A visit drawn longer than the bound ends at it.
func TestVisitBound(t *testing.T) {
	w := testWorld(t, 2, "all")
	for d := dayOf(anchor) - 6; d <= dayOf(anchor)+1; d++ {
		p, d0 := w.plan(d), dayStart(d)
		reach := d0.Add(day + planReach)
		visits := map[string][2]time.Time{}
		for _, b := range p.batches {
			if b.at.Before(d0) || !b.at.Before(reach) {
				t.Fatalf("%s: a batch at %v", d0.Format(time.DateOnly), b.at)
			}
			for _, e := range b.events {
				v, ok := visits[b.sid]
				if !ok {
					v = [2]time.Time{e.t, e.t}
				}
				visits[b.sid] = [2]time.Time{earlier(v[0], e.t), later(v[1], e.t)}
			}
		}
		for _, v := range visits {
			if v[1].Sub(v[0]) > maxVisit {
				t.Fatalf("%s: a visit from %v to %v", d0.Format(time.DateOnly), v[0], v[1])
			}
		}
		for _, rs := range p.records {
			for _, r := range rs {
				if r.t.Before(d0.Add(-feedWindow)) || !r.t.Before(reach) {
					t.Fatalf("%s: a record at %v", d0.Format(time.DateOnly), r.t)
				}
			}
		}
	}
	start := anchor
	var pages []page
	for i := range 12 {
		at := start.Add(time.Duration(i) * 21 * time.Minute)
		pages = append(pages, page{at: at, leave: at.Add(20 * time.Minute)})
	}
	pages = endBy(pages, start.Add(maxVisit))
	last := pages[len(pages)-1]
	if len(pages) != 9 || last.leave.Sub(start) != maxVisit || !last.leave.After(last.at) {
		t.Errorf("a visit cut to %d pages, the last from %v to %v", len(pages), last.at.Sub(start), last.leave.Sub(start))
	}
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
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

// weekOfSpans asks w for a week of spans on each side of its anchor, as backfill and live ask it, each crossing every
// midnight in its own way: the history's last week as Backfill slices it (an hour at a time, a day's last span ending
// at midnight), the referrer wave's day among it, then the live week in ticks of 47 minutes. each runs after every
// span; the requests are dropped.
func weekOfSpans(w *World, each func(from, to time.Time)) {
	for at := w.Anchor.Add(-7 * day); at.Before(w.Anchor); {
		end := earlier(earlier(at.Add(time.Hour), dayStart(dayOf(at)+1)), w.Anchor)
		w.Requests(at, end)
		each(at, end)
		at = end
	}
	start := time.Unix(0, 0)
	l := NewLive(w, start, 60)
	for wall := 47 * time.Second; l.cursor.Before(w.Anchor.Add(7 * day)); wall += 47 * time.Second {
		from := l.cursor
		l.Tick(start.Add(wall))
		each(from, l.cursor)
	}
}

// The world holds two day plans at most at any time, however it is asked: a week of spans on each side of the anchor,
// every midnight crossed; one span of eleven days; the day-long spans LiveEvents asks, from an anchor an hour past
// midnight, so each reads three days. The day before a span's is held only in the hours a visit begun on it can reach
// past midnight.
func TestPlansHeld(t *testing.T) {
	w := testWorld(t, 0.5, DefaultScenarios)
	weekOfSpans(w, func(from, to time.Time) {
		if len(w.plans) > maxPlans {
			t.Fatalf("%d day plans held after the span %v to %v", len(w.plans), from, to)
		}
		late := from.Sub(dayStart(dayOf(from))) > maxVisit+time.Minute
		for d := range w.plans {
			if d > dayOf(to.Add(-time.Nanosecond)) || d < dayOf(from)-1 || d < dayOf(from) && late {
				t.Fatalf("the plan of %s held after the span %v to %v", dayStart(d).Format(time.DateOnly), from, to)
			}
		}
	})
	long := testWorld(t, 0.5, DefaultScenarios)
	if long.Requests(anchor.Add(-11*day), anchor); long.most > maxPlans {
		t.Errorf("a span of eleven days: %d day plans held at once", long.most)
	}
	live := testWorld(t, 0.5, DefaultScenarios)
	live.Anchor = dayStart(dayOf(anchor)).Add(time.Hour)
	if live.LiveEvents(); live.most > maxPlans {
		t.Errorf("LiveEvents: %d day plans held at once", live.most)
	}
	if w.most > maxPlans {
		t.Errorf("the week's spans: %d day plans held at once", w.most)
	}
}

// The day plans a world holds fit the budget at the largest scale: a world of the default scenarios asked for a week
// of spans on each side of the anchor, the referrer wave's day (the largest plan) and every midnight among them, its
// heap read whenever the plans it holds change (some 9 s under the race detector).
func TestPlanMemory(t *testing.T) {
	w := testWorld(t, ScaleLimit.Max(), DefaultScenarios)
	heap := func() float64 {
		var m runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m)
		return float64(m.HeapAlloc)
	}
	base := heap()
	var held, peakHeld string
	var peak float64
	weekOfSpans(w, func(_, _ time.Time) {
		days := slices.Sorted(maps.Keys(w.plans))
		var names []string
		for _, d := range days {
			names = append(names, dayStart(d).Format(time.DateOnly))
		}
		if now := strings.Join(names, " "); now != held {
			held = now
			if used := heap() - base; used > peak {
				peak, peakHeld = used, now
			}
		}
	})
	t.Logf("at most %.0f MB of day plans at scale %v, %s held", peak/(1<<20), ScaleLimit.Max(), peakHeld)
	if peak > planBudget {
		t.Errorf("%.0f MB of day plans at the largest scale, budget %d MB", peak/(1<<20), planBudget>>20)
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
