package store

import (
	"path/filepath"
	"testing"
	"time"
)

// Open opens a fresh database in t's temp dir.
func open(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.Context(), filepath.Join(t.TempDir(), "nui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestSyncSites(t *testing.T) {
	st := open(t)
	sites, err := st.SyncSites(t.Context(), []Site{{Domain: "a.example.com", Kind: "site", PathRules: "[]"},
		{Domain: "b.example.com", Kind: "site", FeedKeyHash: "ab", PathRules: "[]"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 2 || !sites["a.example.com"].Active || sites["b.example.com"].FeedKeyHash != "ab" {
		t.Fatalf("sites = %+v", sites)
	}
	id := sites["a.example.com"].ID
	// Dropping b deactivates it; a keeps its id.
	sites, err = st.SyncSites(t.Context(), []Site{{Domain: "a.example.com", Kind: "site", Product: "P",
		PathRules: "[]"}})
	if err != nil {
		t.Fatal(err)
	}
	if sites["a.example.com"].ID != id || sites["a.example.com"].Product != "P" || sites["b.example.com"].Active {
		t.Fatalf("after resync = %+v", sites)
	}
}

func seed(t *testing.T, st *Store) int64 {
	t.Helper()
	sites, err := st.SyncSites(t.Context(), []Site{{Domain: "a.example.com", Kind: "site", PathRules: "[]"}})
	if err != nil {
		t.Fatal(err)
	}
	return sites["a.example.com"].ID
}

func TestEventsAndActorClass(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	day := time.Now().UnixMilli() / dayMS
	ts := day*dayMS + 1000
	evs := []Event{
		{SiteID: id, TS: ts, Source: "browser", Kind: "page_view", IP: "192.0.2.1", IPKey: "192.0.2.1"},
		{SiteID: id, TS: ts + 1, Source: "server", Kind: "request", IP: "192.0.2.1", IPKey: "192.0.2.1"},
		{SiteID: id, TS: ts + 2, Source: "server", Kind: "request", IP: "192.0.2.2", IPKey: "192.0.2.2"},
	}
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertEvents(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SetActorClass(t.Context(), ActorClass{SiteID: id, IPKey: "192.0.2.1", Day: day, Class: "unknown",
		Reasons: []string{"no script beacon"}}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE cls = ?`, ClsUnknown).Scan(&n); err != nil || n != 2 {
		t.Fatalf("classified events = %d, %v", n, err)
	}
	got, err := st.ActorClasses(t.Context(), id, day)
	if err != nil || got["192.0.2.1"].Class != "unknown" || got["192.0.2.1"].Reasons[0] != "no script beacon" {
		t.Fatalf("classes = %+v, %v", got, err)
	}
	if ClassCode("bot") != ClsBot || ClassCode("x") != ClsPending {
		t.Error("ClassCode")
	}
}

func TestStatsAndLLM(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	for range 2 {
		if err := st.AddStats(t.Context(), []IngestStat{{SiteID: id, Hour: 5, Accepted: 3, Dropped: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	var acc, drop int
	if err := st.DB.QueryRow(`SELECT accepted, dropped FROM ingest_stat WHERE site_id = ? AND hour = 5`, id).
		Scan(&acc, &drop); err != nil || acc != 6 || drop != 2 {
		t.Fatalf("stats %d %d %v", acc, drop, err)
	}
	if err := st.AddStats(t.Context(), nil); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := st.Briefing(t.Context(), id, "7d"); ok || err != nil {
		t.Fatalf("empty briefing: %v %v", ok, err)
	}
	b := Briefing{SiteID: id, Range: "7d", FactsHash: "h", AsOf: 1, Body: "{}", CreatedAt: 2}
	if err := st.PutBriefing(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	b.FactsHash = "h2"
	if err := st.PutBriefing(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := st.Briefing(t.Context(), id, "7d"); !ok || err != nil || got.FactsHash != "h2" {
		t.Fatalf("briefing %+v %v %v", got, ok, err)
	}

	if err := st.PutReading(t.Context(), Reading{Signature: "s", Body: "{}", BasisFrom: 1, BasisTo: 2}); err != nil {
		t.Fatal(err)
	}
	rs, err := st.Readings(t.Context(), []string{"s", "t"})
	if err != nil || len(rs) != 1 || rs["s"].BasisTo != 2 {
		t.Fatalf("readings %+v %v", rs, err)
	}
	if rs, err := st.Readings(t.Context(), nil); err != nil || len(rs) != 0 {
		t.Fatal("empty readings")
	}

	if err := st.PutLabel(t.Context(), "path", "/.env", "looking for leaked secrets"); err != nil {
		t.Fatal(err)
	}
	ls, err := st.Labels(t.Context(), "path", []string{"/.env", "/x"})
	if err != nil || ls["/.env"] != "looking for leaked secrets" || len(ls) != 1 {
		t.Fatalf("labels %v %v", ls, err)
	}
	if ls, err := st.Labels(t.Context(), "path", nil); err != nil || len(ls) != 0 {
		t.Fatal("empty labels")
	}

	for range 2 {
		if err := st.AddUsage(t.Context(), Usage{SiteID: id, Day: 9, Calls: 1, InputTokens: 10, OutputTokens: 5}); err != nil {
			t.Fatal(err)
		}
	}
	u, err := st.UsageOn(t.Context(), id, 9)
	if err != nil || u.Calls != 2 || u.InputTokens != 20 {
		t.Fatalf("usage %+v %v", u, err)
	}
	if u, err := st.UsageOn(t.Context(), id, 10); err != nil || u.Calls != 0 {
		t.Fatal("no usage")
	}
}

func TestPrune(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	now := time.Now()
	old := now.Add(-800 * 24 * time.Hour).UnixMilli()
	var evs []Event
	for i := range 7000 {
		ts := old
		if i%2 == 0 {
			ts = now.UnixMilli()
		}
		evs = append(evs, Event{SiteID: id, TS: ts, Source: "browser", Kind: "page_view", IP: "192.0.2.1",
			IPKey: "192.0.2.1", Props: `{"pad":"` + string(make([]byte, 0)) + `xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}`})
	}
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	n, err := st.Prune(t.Context(), now.AddDate(0, -25, 0), 0)
	if err != nil || n != 3500 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	used, err := st.UsedBytes(t.Context())
	if err != nil || used <= 0 {
		t.Fatalf("used %d %v", used, err)
	}
	// A cap below what is left deletes the oldest rows until the rest fits.
	n, err = st.Prune(t.Context(), now.AddDate(0, -25, 0), used/4)
	if err != nil || n == 0 {
		t.Fatalf("capped prune %d, %v", n, err)
	}
	if after, _ := st.UsedBytes(t.Context()); after > used/4 {
		t.Errorf("after cap %d > %d", after, used/4)
	}
	if err := st.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Set before the first table: incremental_vacuum has nothing to do otherwise.
	var av int
	if err := st.DB.QueryRow(`PRAGMA auto_vacuum`).Scan(&av); err != nil || av != 2 {
		t.Errorf("auto_vacuum = %d, %v; want 2 (incremental)", av, err)
	}
	// Temp tables and sorters stay in memory: the container's file system is read-only outside /data.
	var ts int
	if err := st.DB.QueryRow(`PRAGMA temp_store`).Scan(&ts); err != nil || ts != 2 {
		t.Errorf("temp_store = %d, %v; want 2 (memory)", ts, err)
	}
}
