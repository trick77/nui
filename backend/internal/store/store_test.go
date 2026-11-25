package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	// An order set on a known site, changed and removed again reaches its row each time; a new site starts with its.
	for _, want := range []int{2, 1, 0} {
		sites, err = st.SyncSites(t.Context(), []Site{{Domain: "a.example.com", Kind: "site", SortOrder: want,
			PathRules: "[]"}, {Domain: "new" + strconv.Itoa(want) + ".example.com", Kind: "site", SortOrder: want,
			PathRules: "[]"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := sites["a.example.com"].SortOrder; got != want {
			t.Errorf("known site's order = %d, want %d", got, want)
		}
		if got := sites["new"+strconv.Itoa(want)+".example.com"].SortOrder; got != want {
			t.Errorf("new site's order = %d, want %d", got, want)
		}
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

// coverOpts is o with a Cover that builds a site-day as the job's handshake does, without rows (store has no
// builder): frozen and clean by o.Builder unless it is built already (the job's own rule) or its raw is cut.
func coverOpts(st *Store, o RetentionOptions) RetentionOptions {
	o.Cover = func(ctx context.Context, site, day int64) error {
		if err := st.BeginAggDay(ctx, site, day); err != nil {
			return err
		}
		days, err := st.AggDays(ctx, site, day, day)
		if err != nil {
			return err
		}
		if d := days[day]; !d.Raw || d.BuiltBy(o.Builder) {
			return nil
		}
		_, err = st.WriteAggDay(ctx, AggBuild{Site: site, Day: day, Version: days[day].Version, Builder: o.Builder})
		return err
	}
	return o
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
	base := time.Now().Add(-10 * 24 * time.Hour).UnixMilli() // before the days the cap never cuts
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
	deleted, err := st.Prune(t.Context(), coverOpts(st, RetentionOptions{Months: 25, MaxBytes: used / 2}), time.Now())
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
	old := time.Now().AddDate(0, 0, -10).UnixMilli() // before the days the cap never cuts
	for i := range 6000 {
		evs = append(evs, Event{SiteID: id, TS: old - int64(i), Source: "browser",
			Kind: "page_view", IP: "192.0.2.1", IPKey: "192.0.2.1", Props: `{"pad":"` + strings.Repeat("x", 200) + `"}`})
	}
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	used, _ := st.UsedBytes(t.Context())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		st.RunRetention(ctx, coverOpts(st, RetentionOptions{Months: 25, MaxBytes: used / 2,
			CapEvery: 20 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}))
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

// Free pages left by anything (here a delete outside retention) are handed back a budget per round, and a round
// that leaves some is followed by the next right away, not after CapEvery: the cap and the purge run between
// budgets, the shrink still finishes.
func TestRunRetention_VacuumBudget(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	var evs []Event
	for i := range 6000 {
		evs = append(evs, Event{SiteID: id, TS: time.Now().UnixMilli() - int64(i), Source: "browser",
			Kind: "page_view", IP: "192.0.2.1", IPKey: "192.0.2.1", Props: `{"pad":"` + strings.Repeat("x", 1000) + `"}`})
	}
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`DELETE FROM event`); err != nil {
		t.Fatal(err)
	}
	var free int64
	if err := st.DB.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil || free < 4*vacuumStep {
		t.Fatalf("free pages %d, %v", free, err)
	}
	budget := vacuumBudget
	vacuumBudget = time.Millisecond // a step per round
	t.Cleanup(func() { vacuumBudget = budget })
	var logs bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logs.Write(p)
	}), nil))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	start := time.Now()
	const every = 500 * time.Millisecond
	go func() {
		st.RunRetention(ctx, RetentionOptions{Months: 25, CapEvery: every, Log: log})
		close(done)
	}()
	defer func() { cancel(); <-done }()
	for {
		mu.Lock()
		out := logs.String()
		mu.Unlock()
		if strings.Contains(out, "Vacuum done") {
			if !strings.Contains(out, "Vacuum continues over the next rounds") {
				t.Errorf("done in one round:\n%s", out)
			}
			break
		}
		// One CapEvery per budget would take a round per step: free/vacuumStep of them.
		if time.Since(start) > every*time.Duration(free/vacuumStep) {
			t.Fatalf("not shrunk a budget per prompt round:\n%s", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := st.DB.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil || free != 0 {
		t.Errorf("free pages left %d, %v", free, err)
	}
}

// A database that held events before incremental auto-vacuum was switched on keeps its mode: Vacuum returns at once
// (pages are reused, the file keeps its size), never loops on a freelist incremental_vacuum cannot empty, and the
// retention loop goes on pruning and capping. A step that hands nothing back ends Vacuum with ErrVacuumStalled.
func TestVacuum_WrongModeAndStall(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	var evs []Event
	old := time.Now().AddDate(0, 0, -10).UnixMilli() // before the days the cap never cuts
	for i := range 6000 {
		evs = append(evs, Event{SiteID: id, TS: old - int64(i), Source: "browser",
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
		st.RunRetention(rctx, coverOpts(st, RetentionOptions{Months: 25, MaxBytes: used / 2,
			CapEvery: 20 * time.Millisecond, Log: slog.New(slog.NewTextHandler(&logs, nil))}))
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

// seedOld is seed on a database an old version left: 0001's columns, not SyncSites, whose statement names later ones.
func seedOld(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	res, err := db.ExecContext(t.Context(), `INSERT INTO site (domain, kind, created_at)
		VALUES ('a.example.com', 'site', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestUpgrade_0002 opens a database that 0.1 left (0001 only, with events): Open adds the aggregate tables, empty,
// and every event stays.
func TestUpgrade_0002(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trakk.db")
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
	id := seedOld(t, db)
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
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&migrated); err != nil || migrated != 9 {
		t.Errorf("migrations = %d, %v", migrated, err)
	}
	// The site from before the upgrade has no order, and takes one under its id.
	sites, err := st.Sites(t.Context())
	if err != nil || sites["a.example.com"].ID != id || sites["a.example.com"].SortOrder != 0 {
		t.Fatalf("sites after upgrade = %+v, %v", sites, err)
	}
	sites, err = st.SyncSites(t.Context(), []Site{{Domain: "a.example.com", Kind: "site", SortOrder: 3, PathRules: "[]"}})
	if err != nil || sites["a.example.com"].ID != id || sites["a.example.com"].SortOrder != 3 {
		t.Errorf("site synced after upgrade = %+v, %v", sites, err)
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

// TestUpgrade_0004 opens a database that 0.2 left, with classes and hourly aggregates whose raw is gone: every row
// stays as it was, sketches included, and the new class is taken.
func TestUpgrade_0004(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	old := fstest.MapFS{}
	for _, f := range []string{"0001_init.sql", "0002_agg.sql", "0003_purge.sql"} {
		data, err := fs.ReadFile(migrations(), f)
		if err != nil {
			t.Fatal(err)
		}
		old[f] = &fstest.MapFile{Data: data}
	}
	db, err := sql.Open("sqlite3", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Up(t.Context(), db, old, migrate.Options{Dialect: migrate.SQLite}); err != nil {
		t.Fatal(err)
	}
	id := seedOld(t, db)
	// Every column its own value: a column copied into another would show.
	for i, class := range []string{"human", "bot", "monitor", "unknown"} {
		if _, err := db.Exec(`INSERT INTO actor_class VALUES (?, ?, ?, ?, ?, ?)`, id, fmt.Sprintf("192.0.2.%d", i+1),
			100+i, class, fmt.Sprintf(`["r%d"]`, i), 1000+i); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO agg_hour VALUES (?, ?, ?, 11, 12, 13, 14, 15, 16, 17, x'01', x'0203', ?)`,
			id, 2400+i, i+1, []byte{byte(i), 0xff}); err != nil {
			t.Fatal(err)
		}
	}
	// The columns 0004 copied: a later migration adds its own.
	dumpCols := map[string]string{"actor_class": "*", "agg_hour": `site_id, hour, cls, events, views, actions, starts,
		requests, refused, errors, browser_ids, addrs, server_addrs`}
	dump := func(db *sql.DB, table string) string {
		t.Helper()
		//nolint:gosec // the test's own table names
		rows, err := db.Query(`SELECT ` + dumpCols[table] + ` FROM ` + table + ` ORDER BY 1, 2, 3`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		out := fmt.Sprintln(cols)
		for rows.Next() {
			vals, ptrs := make([]any, len(cols)), make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out += fmt.Sprintln(vals...)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	tables := []string{"actor_class", "agg_hour"}
	before := map[string]string{}
	for _, tbl := range tables {
		before[tbl] = dump(db, tbl)
	}
	_ = db.Close()

	st, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, tbl := range tables {
		if got := dump(st.DB, tbl); got != before[tbl] {
			t.Errorf("%s after the upgrade:\n%s\nwant\n%s", tbl, got, before[tbl])
		}
	}
	if err := st.SetActorClass(t.Context(), ActorClass{SiteID: id, IPKey: "192.0.2.9", Day: 200, Class: "api"}, nil); err != nil {
		t.Errorf("class api: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO agg_hour (site_id, hour, cls) VALUES (?, 4800, ?)`, id, ClsAPI); err != nil {
		t.Errorf("hour of class api: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO agg_hour (site_id, hour, cls) VALUES (?, 4800, 6)`, id); err == nil {
		t.Error("a class code nobody knows was taken")
	}
	var index int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = 'actor_class_day'`).
		Scan(&index); err != nil || index != 1 {
		t.Errorf("actor_class_day: %d, %v", index, err)
	}
}

// TestUpgrade_0009 opens a database with the watch's path state (0007): its tables start empty in the script hosts'
// shape, path_state and its client addresses gone.
func TestUpgrade_0009(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	old := fstest.MapFS{}
	files, err := fs.Glob(migrations(), "000[1-8]_*.sql")
	if err != nil || len(files) != 8 {
		t.Fatalf("migrations %v, %v", files, err)
	}
	for _, f := range files {
		data, err := fs.ReadFile(migrations(), f)
		if err != nil {
			t.Fatal(err)
		}
		old[f] = &fstest.MapFile{Data: data}
	}
	db, err := sql.Open("sqlite3", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Up(t.Context(), db, old, migrate.Options{Dialect: migrate.SQLite}); err != nil {
		t.Fatal(err)
	}
	id := seedOld(t, db)
	for _, q := range []string{
		`INSERT INTO watch_site (site_id, from_day, done_day, routed_day, cut) VALUES (?, 1, 2, 2, 7)`,
		`INSERT INTO path_state (site_id, template, hit_first, hit_addr, found) VALUES (?, 'GET /x.php', 1, '192.0.2.1', 'hit')`,
		`INSERT INTO script_host (site_id, host, first_seen, last_seen, found_at) VALUES (?, 'cdn.example.org', 1, 2, 3)`,
		`INSERT INTO watch_open (site_id, kind, key, first_seen) VALUES (?, 'flip', 'GET /x.php', 1)`,
	} {
		if _, err := db.Exec(q, id); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()

	st, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE name LIKE 'path_state%'`).Scan(&n); err != nil ||
		n != 0 {
		t.Errorf("path_state: %d, %v", n, err)
	}
	for _, tbl := range []string{"watch_site", "script_host", "watch_open"} {
		//nolint:gosec // the test's own table names
		if err := st.DB.QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d rows, %v", tbl, n, err)
		}
	}
	var cols []string
	rows, err := st.DB.Query(`SELECT name FROM pragma_table_info('watch_site') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		cols = append(cols, c)
	}
	_ = rows.Close()
	if fmt.Sprint(cols) != "[site_id from_day done_day ready_at scripts_from cut]" {
		t.Errorf("watch_site %v", cols)
	}
	if _, err := st.DB.Exec(`INSERT INTO watch_open (site_id, kind, key, first_seen) VALUES (?, 'flip', 'x', 1)`,
		id); err == nil {
		t.Error("an open path finding was taken")
	}
	if _, err := st.DB.Exec(`INSERT INTO watch_open (site_id, kind, key, first_seen) VALUES (?, 'script', 'x', 1)`,
		id); err != nil {
		t.Errorf("an open script finding: %v", err)
	}
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = 'script_host_found'`).
		Scan(&n); err != nil || n != 1 {
		t.Errorf("script_host_found: %d, %v", n, err)
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
	// The cap takes the oldest chunks, here part of day+2 (a padded day of 5000 events, before the age floor: age
	// retention's, never built): that day has no whole raw left either.
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
	if n, err := st.EnforceCap(t.Context(), RetentionOptions{Months: 25, MaxBytes: used * 95 / 100}, time.Now()); err != nil ||
		n == 0 || n >= 5000 {
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
	n, err := st.Prune(t.Context(), RetentionOptions{Months: 25}, now)
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

// capFloor stays below what the tail's raw and the actor classes take: a store holding only them uses more than
// the bound says (the gap is where the cap still deletes aggregates in vain).
func TestCapFloor(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	empty, _ := st.UsedBytes(t.Context())
	now := time.Now()
	today := now.UnixMilli() / dayMS
	var evs []Event
	for i := range 6000 {
		ip := fmt.Sprintf("198.51.100.%d", i%200)
		evs = append(evs, Event{SiteID: id, TS: (today-int64(i%3))*dayMS + int64(i)*1000, Source: "browser",
			Kind: "page_view", Visitor: fmt.Sprint("v", i%500), SID: fmt.Sprint("s", i%900), IP: ip, IPKey: ip,
			UA:   "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Safari/605.1",
			Path: fmt.Sprint("/p/", i%40), Template: "/p/:id", URL: fmt.Sprint("https://example.com/p/", i%40),
			Country: "CH", Props: `{"w":1440}`})
	}
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		if err := st.SetActorClass(t.Context(), ActorClass{SiteID: id, IPKey: fmt.Sprintf("198.51.100.%d", i),
			Day: today - int64(i%40), Class: "human"}, map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	sites, _ := st.Sites(t.Context())
	floor, err := st.capFloor(t.Context(), sites, map[int64]int64{id: today - (CapTail - 1)}, today)
	used, _ := st.UsedBytes(t.Context())
	if err != nil || floor <= 0 || floor > used-empty {
		t.Fatalf("floor %d of %d bytes, %v", floor, used-empty, err)
	}
	t.Logf("floor %d of %d bytes (%.0f %%)", floor, used-empty, float64(floor)/float64(used-empty)*100)
}

// The cap's order: raw before the last three UTC days first, then the per-address rows, the lists, the hours, each
// oldest day first, each step warned once; a day whose per-address rows went (all of them) stays clean, one it cut
// lists or hours of is marked unbuilt and partial. No aggregate goes while the last three days' raw alone exceeds the
// cap; every agg_day row survives. Age retention takes the per-address rows with the events and leaves the other
// aggregates.
func TestEnforceCap_Order(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	now := time.Now()
	today := now.UnixMilli() / dayMS
	pad := strings.Repeat("x", 1000)
	// Ten built days, 20 to 11 days ago, with a few small raw events each and many large per-address rows; raw
	// events on each of the last three days.
	var evs []Event
	for d := today - 20; d <= today; d++ {
		if d > today-11 && d < today-2 {
			continue
		}
		for i := range 20 {
			evs = append(evs, Event{SiteID: id, TS: d*dayMS + int64(i)*1000, Source: "server", Kind: "request",
				IP: "192.0.2.1", IPKey: "192.0.2.1"})
		}
	}
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	for d := today - 20; d <= today-11; d++ {
		addrs := aggAddrs(id, d, 300)
		for i := range addrs {
			addrs[i].UA = pad
		}
		write(t, st, AggBuild{Site: id, Day: d, Dims: []string{"pages"}, Hours: []AggHour{{Hour: d * 24, Cls: 1}},
			Lists: []AggList{{Dim: "pages", Part: 1, Key: "/"}}, Addrs: slices.Values(addrs)})
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	count := func(q string, args ...any) int { t.Helper(); return countRows(t, st, q, args...) }

	// Half the size: the old raw (each day built first: the last built day's next one through Cover) and the oldest
	// per-address days go; lists and hours stay.
	used, _ := st.UsedBytes(t.Context())
	capOpts := func(max int64) RetentionOptions {
		return coverOpts(st, RetentionOptions{Months: 25, MaxBytes: max, Log: log})
	}
	if _, err := st.EnforceCap(t.Context(), capOpts(used/2), now); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT COUNT(*) FROM event WHERE ts < ?`, (today-2)*dayMS); n != 0 {
		t.Errorf("old raw left: %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM agg_addr WHERE day = ?`, today-20); n != 0 {
		t.Errorf("the oldest per-address day kept: %d rows", n)
	}
	if n := count(`SELECT COUNT(*) FROM agg_addr WHERE day = ?`, today-11); n != 300 {
		t.Errorf("the newest per-address day cut before older ones: %d rows", n)
	}
	if l, h := count(`SELECT COUNT(*) FROM agg_list`), count(`SELECT COUNT(*) FROM agg_hour`); l != 10 || h != 10 {
		t.Errorf("lists %d, hours %d cut before the per-address rows were", l, h)
	}
	first := logs.String()
	if !strings.Contains(first, "raw events") || !strings.Contains(first, "per-address") ||
		strings.Contains(first, "list aggregates") || strings.Count(first, "per-address") != 1 {
		t.Errorf("warnings:\n%s", first)
	}
	if i, j := strings.Index(first, "raw events"), strings.Index(first, "per-address"); i > j {
		t.Errorf("per-address rows cut before the raw:\n%s", first)
	}
	// A day whose per-address rows went whole keeps its other figures readable: frozen and clean, every built day.
	addrCut := count(`SELECT COUNT(*) FROM agg_day d WHERE day <= ? AND NOT EXISTS (SELECT 1 FROM agg_addr a
		WHERE a.site_id = d.site_id AND a.day = d.day)`, today-11)
	if addrCut == 0 || count(`SELECT COUNT(*) FROM agg_day WHERE frozen = 1 AND dirty = 0 AND day <= ?`, today-11) != 10 ||
		count(`SELECT COUNT(*) FROM (SELECT day FROM agg_addr GROUP BY day HAVING COUNT(*) != 300)`) != 0 {
		t.Errorf("after the cut: %d days without per-address rows, built days not all clean, or a day cut in part",
			addrCut)
	}

	// One byte: the last three days' raw alone exceeds it, so no aggregate goes; the tail's raw and agg_day stay.
	logs.Reset()
	rows := func() [3]int {
		return [3]int{count(`SELECT COUNT(*) FROM agg_addr`), count(`SELECT COUNT(*) FROM agg_list`),
			count(`SELECT COUNT(*) FROM agg_hour`)}
	}
	kept := rows()
	if _, err := st.EnforceCap(t.Context(), capOpts(1), now); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT COUNT(*) FROM event WHERE ts >= ?`, (today-2)*dayMS); n != 60 {
		t.Errorf("the last three days' raw: %d events, want 60", n)
	}
	if got := rows(); got != kept || kept[0] == 0 {
		t.Errorf("aggregate rows %v, before %v: deleted in vain", got, kept)
	}
	if second := logs.String(); !strings.Contains(second, "the aggregates stay") || strings.Contains(second, "deleting") {
		t.Errorf("warnings:\n%s", second)
	}

	// Nothing in the tail: one byte takes every aggregate row before it, lists before hours, every day marked.
	if _, err := st.DB.Exec(`DELETE FROM event`); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	if _, err := st.EnforceCap(t.Context(), capOpts(1), now); err != nil {
		t.Fatal(err)
	}
	if got := rows(); got != [3]int{} {
		t.Errorf("rows left: %v", got)
	}
	if n := count(`SELECT COUNT(*) FROM agg_day WHERE frozen = 0 AND dirty = ?`, AggWriting); n != 10 {
		t.Errorf("days marked: %d of 10", n)
	}
	third := logs.String()
	if i, j := strings.Index(third, "list aggregates"), strings.Index(third, "hourly"); i < 0 || j < i ||
		!strings.Contains(third, "alone exceed") {
		t.Errorf("warnings:\n%s", third)
	}

	// Age retention: the per-address rows before the cutoff go, lists and hours stay.
	st2 := open(t)
	id2 := seed(t, st2)
	for _, d := range []int64{today - 40, today - 30} {
		write(t, st2, AggBuild{Site: id2, Day: d, Dims: []string{"pages"}, Hours: []AggHour{{Hour: d * 24, Cls: 1}},
			Lists: []AggList{{Dim: "pages", Part: 1, Key: "/"}}, Addrs: slices.Values(aggAddrs(id2, d, 2500))})
	}
	if n, err := st2.PruneAge(t.Context(), time.UnixMilli((today-35)*dayMS)); err != nil || n != 2500 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if a, l, h := countRows(t, st2, `SELECT COUNT(*) FROM agg_addr`), countRows(t, st2, `SELECT COUNT(*) FROM agg_list`),
		countRows(t, st2, `SELECT COUNT(*) FROM agg_hour`); a != 2500 || l != 2 || h != 2 {
		t.Errorf("after age retention: %d addr, %d list, %d hour rows", a, l, h)
	}
}
