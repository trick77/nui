package sim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
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
	for range 2 {
		w := testWorld(t, 0.5, "all")
		st := testStore(t, w)
		s, err := Backfill(t.Context(), st, w, BackfillOptions{Days: 3, Classify: true})
		if err != nil || s.Events == 0 {
			t.Fatalf("%v, %v", s, err)
		}
		digests = append(digests, dump(t, st))
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
	w = testWorld(t, 2, "baseline")
	st := testStore(t, w)
	s, err := Backfill(t.Context(), st, w, BackfillOptions{Days: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Refused) != 0 || s.Events < 1000 {
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
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("a day at scale 1 took %v", d)
	}
	t.Log(s)
}
