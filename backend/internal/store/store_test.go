package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/trick77/ship/kit/migrate"
)

// Open opens a fresh database in t's temp dir.
func open(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.Context(), filepath.Join(t.TempDir(), "app.db"))
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
		{SiteID: id, TS: ts + 3, Source: "server", Kind: "request", IP: "192.0.2.1", IPKey: "192.0.2.1", UA: "curl/8"},
	}
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertEvents(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	// The actor's class on its events, one client's own class on that client's.
	if err := st.SetActorClass(t.Context(), ActorClass{SiteID: id, IPKey: "192.0.2.1", Day: day, Class: "unknown",
		Reasons: []string{"no script beacon"}}, map[string]string{"curl/8": "bot"}); err != nil {
		t.Fatal(err)
	}
	var n, b int
	if err := st.DB.QueryRow(`SELECT SUM(cls = ?), SUM(cls = ?) FROM event`, ClsUnknown, ClsBot).Scan(&n, &b); err != nil ||
		n != 2 || b != 1 {
		t.Fatalf("classified events = %d unknown, %d bot, %v", n, b, err)
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

	if err := st.PutReading(t.Context(), Reading{Signature: "s", Body: "{}", CreatedAt: 2}); err != nil {
		t.Fatal(err)
	}
	rs, err := st.Readings(t.Context(), []string{"s", "t"})
	if err != nil || len(rs) != 1 || rs["s"].CreatedAt != 2 {
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

// The cap deletes the oldest events by time, whatever their ids, over every site, and the freed pages go back.
func TestPrune_Cap(t *testing.T) {
	st := open(t)
	sites, err := st.SyncSites(t.Context(), []Site{{Domain: "a.example.com", Kind: "site", PathRules: "[]"},
		{Domain: "b.example.com", Kind: "site", PathRules: "[]"}})
	if err != nil {
		t.Fatal(err)
	}
	a, b := sites["a.example.com"].ID, sites["b.example.com"].ID
	const n = 12000
	base := time.Now().Add(-time.Hour).UnixMilli()
	// Written newest first: a higher id is an older event. The sites alternate.
	var evs []Event
	for i := range n {
		site := a
		if i%2 == 1 {
			site = b
		}
		evs = append(evs, Event{SiteID: site, TS: base + int64(n-i), Source: "browser", Kind: "page_view",
			IP: "192.0.2.1", IPKey: "192.0.2.1", Props: `{"pad":"` + strings.Repeat("x", 200) + `"}`})
	}
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	used, err := st.UsedBytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := st.Prune(t.Context(), time.Now().AddDate(0, -25, 0), used/2)
	if err != nil || deleted == 0 {
		t.Fatalf("capped prune %d, %v", deleted, err)
	}
	if after, _ := st.UsedBytes(t.Context()); after > used/2*9/10 {
		t.Errorf("after cap %d > %d", after, used/2*9/10)
	}
	// Exactly the newest events are left: their times are the top of the range, without a gap.
	var left, minTS, maxTS int64
	if err := st.DB.QueryRow(`SELECT COUNT(*), MIN(ts), MAX(ts) FROM event`).Scan(&left, &minTS, &maxTS); err != nil {
		t.Fatal(err)
	}
	if left != n-deleted || maxTS != base+n || minTS != base+n-left+1 {
		t.Errorf("left %d events from %d to %d, want the newest %d", left, minTS, maxTS, n-deleted)
	}
	var free int64
	if err := st.DB.QueryRow(`SELECT freelist_count FROM pragma_freelist_count()`).Scan(&free); err != nil || free != 0 {
		t.Errorf("free pages %d %v after the stepwise vacuum", free, err)
	}
}

// The cap is enforced on the retention loop's short cadence, not once a day.
func TestRunRetention_Cap(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	var evs []Event
	for i := range 6000 {
		evs = append(evs, Event{SiteID: id, TS: time.Now().UnixMilli() - int64(i), Source: "browser",
			Kind: "page_view", IP: "192.0.2.1", IPKey: "192.0.2.1", Props: `{"pad":"` + strings.Repeat("x", 200) + `"}`})
	}
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	used, _ := st.UsedBytes(t.Context())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		st.RunRetention(ctx, RetentionOptions{Months: 25, MaxBytes: used / 2, CapEvery: 20 * time.Millisecond,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if u, _ := st.UsedBytes(t.Context()); u <= used/2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cap not enforced")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
}

// A database that held events before incremental auto-vacuum was switched on keeps its mode: Vacuum returns at once
// (pages are reused, the file keeps its size), never loops on a freelist incremental_vacuum cannot empty, and the
// retention loop goes on pruning and capping. A step that hands nothing back ends Vacuum with ErrVacuumStalled.
func TestVacuum_WrongModeAndStall(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	var evs []Event
	for i := range 6000 {
		evs = append(evs, Event{SiteID: id, TS: time.Now().UnixMilli() - int64(i), Source: "browser",
			Kind: "page_view", IP: "192.0.2.1", IPKey: "192.0.2.1", Props: `{"pad":"` + strings.Repeat("x", 200) + `"}`})
	}
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	// Stalled: incremental mode, free pages, steps that do nothing.
	if _, err := st.DB.Exec(`DELETE FROM event WHERE id % 2 = 0`); err != nil {
		t.Fatal(err)
	}
	vacuumStmt = `SELECT 1`
	err := st.Vacuum(t.Context())
	vacuumStmt = `PRAGMA incremental_vacuum(` + strconv.Itoa(vacuumStep) + `)`
	if !errors.Is(err, ErrVacuumStalled) {
		t.Errorf("stalled vacuum: %v", err)
	}
	// The wrong mode: switched back to none, as a database created before the switch would be.
	conn, err := st.DB.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), `PRAGMA auto_vacuum = NONE`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), `VACUUM`); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if inc, err := st.Incremental(t.Context()); err != nil || inc {
		t.Fatalf("incremental %v %v after switching it off", inc, err)
	}
	if _, err := st.DB.Exec(`DELETE FROM event WHERE id % 3 = 0`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := st.Vacuum(ctx); err != nil {
		t.Errorf("vacuum in the wrong mode: %v", err)
	}
	// Retention keeps working: the cap still prunes, and the loop says once why the file does not shrink.
	used, _ := st.UsedBytes(t.Context())
	var logs bytes.Buffer
	rctx, rcancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		st.RunRetention(rctx, RetentionOptions{Months: 25, MaxBytes: used / 2, CapEvery: 20 * time.Millisecond,
			Log: slog.New(slog.NewTextHandler(&logs, nil))})
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if u, _ := st.UsedBytes(t.Context()); u <= used/2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cap not enforced in the wrong mode")
		}
		time.Sleep(20 * time.Millisecond)
	}
	rcancel()
	<-done
	if !strings.Contains(logs.String(), "not in incremental auto-vacuum mode") {
		t.Errorf("no word on the mode: %s", logs.String())
	}
}

// Raw writes to a built day mark it dirty and bump its version, in their own transaction; a day without a row, a
// day whose raw is gone and a class pass that restamps nothing leave agg_day alone.
func TestRawWritesMarkAggDirty(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	day := int64(20_000)
	for _, d := range []int64{day, day + 1, day + 2} {
		write(t, st, AggBuild{Site: id, Day: d, Builder: 1})
	}
	if _, err := st.DB.Exec(`UPDATE agg_day SET raw = 0 WHERE site_id = ? AND day = ?`, id, day+2); err != nil {
		t.Fatal(err)
	}
	state := func() map[int64]AggDay {
		t.Helper()
		days, err := st.AggDays(t.Context(), id, day-1, day+3)
		if err != nil {
			t.Fatal(err)
		}
		return days
	}
	ev := func(d int64, ua string, cls int) Event {
		return Event{SiteID: id, TS: d*dayMS + 5000, Source: "server", Kind: "request", IP: "192.0.2.1",
			IPKey: "192.0.2.1", UA: ua, Cls: cls}
	}
	// One batch over two built days, a day without a row and a purged one: each built day once.
	if err := st.InsertEvents(t.Context(), []Event{ev(day, "a", 4), ev(day, "b", 4), ev(day+1, "a", 4),
		ev(day-1, "a", 4), ev(day+2, "a", 4)}); err != nil {
		t.Fatal(err)
	}
	s := state()
	if s[day].Version != 1 || s[day].Dirty != 1 || s[day+1].Version != 1 || s[day+1].Dirty != 1 {
		t.Errorf("built days after an insert: %+v, %+v", s[day], s[day+1])
	}
	if _, ok := s[day-1]; ok {
		t.Errorf("a row appeared for an unbuilt day: %+v", s[day-1])
	}
	if s[day+2].Version != 0 || s[day+2].Dirty != 0 {
		t.Errorf("purged day marked: %+v", s[day+2])
	}

	// A pass that changes nothing leaves the day alone; one that restamps, or restamps one client, marks it.
	if _, err := st.DB.Exec(`UPDATE agg_day SET dirty = 0`); err != nil {
		t.Fatal(err)
	}
	set := func(class string, byUA map[string]string) {
		t.Helper()
		if err := st.SetActorClass(t.Context(), ActorClass{SiteID: id, IPKey: "192.0.2.1", Day: day, Class: class},
			byUA); err != nil {
			t.Fatal(err)
		}
	}
	set("unknown", map[string]string{"b": "unknown"})
	if s := state()[day]; s.Version != 1 || s.Dirty != 0 {
		t.Errorf("no restamp: %+v", s)
	}
	set("unknown", map[string]string{"b": "bot"})
	if s := state()[day]; s.Version != 2 || s.Dirty != 1 {
		t.Errorf("one client restamped: %+v", s)
	}
	set("unknown", map[string]string{"b": "bot"}) // the same pass again: nothing moves
	if s := state()[day]; s.Version != 2 {
		t.Errorf("a repeated pass marked the day: %+v", s)
	}
	var bots int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE ts >= ? AND ts < ? AND cls = 2`, day*dayMS,
		(day+1)*dayMS).Scan(&bots); err != nil || bots != 1 {
		t.Errorf("client's own class: %d rows, %v", bots, err)
	}
	set("bot", nil)
	if s := state()[day]; s.Version != 3 {
		t.Errorf("actor restamped: %+v", s)
	}
	if s := state()[day+1]; s.Version != 1 {
		t.Errorf("another day marked by the class pass: %+v", s)
	}
}

// TestUpgrade_0002 opens a database that 0.1 left (0001 only, with events): Open adds the aggregate tables, empty,
// and every event stays.
func TestUpgrade_0002(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nui.db")
	init, err := fs.ReadFile(migrations(), "0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Up(t.Context(), db, fstest.MapFS{"0001_init.sql": {Data: init}},
		migrate.Options{Dialect: migrate.SQLite}); err != nil {
		t.Fatal(err)
	}
	id := seed(t, &Store{DB: db})
	day := time.Now().UnixMilli()/dayMS - 30
	// As 0.1 wrote them: plain inserts, no aggregate table to touch.
	for i := range 50 {
		if _, err := db.Exec(`INSERT INTO event (site_id, ts, source, kind, ip, ip_key)
			VALUES (?, ?, 'browser', 'page_view', '192.0.2.1', '192.0.2.1')`, id, day*dayMS+int64(i)*60_000); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()

	st, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var events, migrated int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM event`).Scan(&events); err != nil || events != 50 {
		t.Errorf("events after upgrade = %d, %v", events, err)
	}
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&migrated); err != nil || migrated != 2 {
		t.Errorf("migrations = %d, %v", migrated, err)
	}
	for _, tbl := range []string{"agg_day", "agg_hour", "agg_list", "agg_addr"} {
		var n int
		//nolint:gosec // the test's own table names
		if err := st.DB.QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d rows, %v", tbl, n, err)
		}
	}
	// Writes go on as before.
	if err := st.InsertEvents(t.Context(), []Event{{SiteID: id, TS: day * dayMS, Source: "browser",
		Kind: "page_view", IP: "192.0.2.1", IPKey: "192.0.2.1"}}); err != nil {
		t.Fatal(err)
	}
}

// Retention keeps an aggregate day honest: age deletes whole UTC days, and every day it or the cap deleted from is
// marked without raw in the same transaction, a built day's row kept, a missing one made.
func TestPrune_DayCoverage(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	day := time.Now().UnixMilli()/dayMS - 800
	ev := func(d int64, hour int) Event {
		return Event{SiteID: id, TS: d*dayMS + int64(hour)*3_600_000, Source: "server", Kind: "request",
			IP: "192.0.2.1", IPKey: "192.0.2.1"}
	}
	var evs []Event
	for _, d := range []int64{day, day + 1, day + 2, day + 3} {
		evs = append(evs, ev(d, 2), ev(d, 20))
	}
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	// Day+1 was built before; day+2 and day+3 have no row.
	write(t, st, AggBuild{Site: id, Day: day + 1, Builder: 1, Events: 2})
	// A cutoff at noon of day+2: day and day+1 go, day+2 stays whole.
	n, err := st.PruneAge(t.Context(), time.UnixMilli((day+2)*dayMS+12*3_600_000))
	if err != nil || n != 4 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	days, err := st.AggDays(t.Context(), id, day, day+3)
	if err != nil {
		t.Fatal(err)
	}
	if d := days[day]; d.Raw || d.Frozen {
		t.Errorf("pruned unbuilt day: %+v", d)
	}
	// A build that read it before the cut finds its version moved.
	if d := days[day+1]; d.Raw || !d.Frozen || d.Events != 2 || d.Version != 1 {
		t.Errorf("pruned built day: %+v", d)
	}
	if _, ok := days[day+2]; ok {
		t.Errorf("a kept day marked: %+v", days[day+2])
	}
	// The cap takes the oldest chunks, here part of day+2 (a padded day of 5000 events): that day has no whole raw
	// left either.
	evs = nil
	for _, d := range []int64{day + 2, day + 3} {
		for i := range 5000 {
			e := ev(d, 3)
			e.TS += int64(i)
			e.Props = `{"pad":"` + strings.Repeat("x", 200) + `"}`
			evs = append(evs, e)
		}
	}
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	used, _ := st.UsedBytes(t.Context())
	if n, err := st.EnforceCap(t.Context(), used*95/100); err != nil || n == 0 || n >= 5000 {
		t.Fatalf("capped %d, %v", n, err)
	}
	days, _ = st.AggDays(t.Context(), id, day, day+3)
	if d, ok := days[day+2]; !ok || d.Raw {
		t.Errorf("half-deleted day: %+v", d)
	}
	if _, ok := days[day+3]; ok {
		t.Errorf("an untouched day marked: %+v", days[day+3])
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
