package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3"
	sqlite3driver "github.com/ncruces/go-sqlite3/driver"

	"github.com/trick77/stakeout/backend/internal/plancheck"
)

func aggAddrs(site, day int64, n int) []AggAddr {
	out := make([]AggAddr, n)
	for i := range out {
		out[i] = AggAddr{SiteID: site, Day: day, IPKey: fmt.Sprintf("key-%d", i),
			Part: 1 + i%2, Events: 1, FirstTS: day * dayMS, LastTS: day * dayMS}
	}
	return out
}

func countRows(t *testing.T, st *Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// write builds a site-day as the job does: its row made, the rows written at the version it has now.
func write(t *testing.T, st *Store, b AggBuild) bool {
	t.Helper()
	if err := st.BeginAggDay(t.Context(), b.Site, b.Day); err != nil {
		t.Fatal(err)
	}
	b.Version = dayState(t, st, b.Site, b.Day).Version
	clean, err := st.WriteAggDay(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	return clean
}

func dayState(t *testing.T, st *Store, site, day int64) AggDay {
	t.Helper()
	days, err := st.AggDays(t.Context(), site, day, day)
	if err != nil {
		t.Fatal(err)
	}
	return days[day]
}

// A day's rows go in transactions of at most aggChunk rows, deletes counted; a rewrite replaces every row. An Other
// row per sub is its own row.
func TestWriteAggDay(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	day := int64(20_000)
	b := AggBuild{Site: id, Day: day, Dims: []string{"pages", "events"},
		Hours: []AggHour{{Hour: day * 24, Cls: 1, Events: 3}, {Hour: day*24 + 23, Cls: 4, Events: 1}},
		Lists: []AggList{{Dim: "pages", Part: 1, Key: "/", A: 3}, {Dim: "pages", Part: 2, Other: true, Entries: 2},
			{Dim: "events", Part: 2, Other: true, Sub: "auto", Entries: 1, AddrN: 4},
			{Dim: "events", Part: 2, Other: true, Sub: "custom", Entries: 2, AddrN: 5}},
		Addrs: slices.Values(aggAddrs(id, day, 4500)),
	}
	// Neighbours stay.
	write(t, st, AggBuild{Site: id, Day: day + 1, Dims: []string{"pages"},
		Hours: []AggHour{{Hour: (day + 1) * 24, Cls: 1}}, Lists: []AggList{{Dim: "pages", Part: 1, Key: "/"}},
		Addrs: slices.Values(aggAddrs(id, day+1, 1))})
	if !write(t, st, b) {
		t.Fatal("a quiet day not clean")
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM agg_addr WHERE day = ?`, day); n != 4500 {
		t.Fatalf("addr rows = %d", n)
	}
	if n := countRows(t, st, `SELECT SUM(addr_n) FROM agg_list WHERE day = ? AND dim = 'events'`, day); n != 9 {
		t.Errorf("events' Other rows by sub: addr_n %d", n)
	}
	b.Addrs = slices.Values(aggAddrs(id, day, 10))
	b.Lists = b.Lists[:1]
	write(t, st, b)
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM agg_addr WHERE day = ?`:                               10,
		`SELECT COUNT(*) FROM agg_list WHERE day = ?`:                               1,
		`SELECT COUNT(*) FROM agg_hour WHERE hour >= ? * 24 AND hour < ? * 24 + 24`: 2,
		`SELECT COUNT(*) FROM agg_addr WHERE day = ? + 1`:                           1,
		`SELECT COUNT(*) FROM agg_list WHERE day = ? + 1`:                           1,
		`SELECT COUNT(*) FROM agg_hour WHERE hour = (? + 1) * 24`:                   1,
	} {
		args := []any{day}
		if strings.Count(q, "?") == 2 {
			args = append(args, day)
		}
		if n := countRows(t, st, q, args...); n != want {
			t.Errorf("%s: %d, want %d", q, n, want)
		}
	}

	// The chunks themselves: 4500 inserts are three transactions.
	c := &chunks{s: st, ctx: t.Context()}
	for _, a := range aggAddrs(id, day+2, 4500) {
		if err := c.ins(insAggAddr, id, a.Day, a.IPKey, a.Part, 0, 0, 0, 0, 0, 0, 0, false, false, 0, 0, "", 0, "",
			0); err != nil {
			t.Fatal(err)
		}
	}
	if c.n > aggChunk || c.commits != 2 {
		t.Errorf("open chunk %d rows after %d commits", c.n, c.commits)
	}
	if err := c.commit(); err != nil {
		t.Fatal(err)
	}
	c = &chunks{s: st, ctx: t.Context()}
	if err := c.del(delAggAddr, id, day+2); err != nil {
		t.Fatal(err)
	}
	if err := c.commit(); err != nil || c.commits != 3 {
		t.Fatalf("delete of 4500 rows: %d commits, %v", c.commits, err)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM agg_addr WHERE day = ?`, day+2); n != 0 {
		t.Errorf("after the chunked delete: %d rows", n)
	}
}

// The build's states: a write freezes a day clean unless its version moved or its raw went since the build's read,
// and requeues the next day. A write cut short leaves the day AggWriting: never read, never cleared without a
// rebuild, kept by a raw write; the complete rows of a day whose raw is gone stand once cleared.
func TestAggDayStates(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	day := int64(20_000)
	if _, err := st.WriteAggDay(t.Context(), AggBuild{Site: id, Day: day}); !errors.Is(err, ErrNoAggDay) {
		t.Errorf("write without begin: %v", err)
	}
	for _, d := range []int64{day, day + 1} {
		if !write(t, st, AggBuild{Site: id, Day: d, Builder: 1, Events: 5, RequeueNext: true}) {
			t.Fatalf("day %d not clean", d)
		}
	}
	// A raw write after the read: the rows go in, the day stays dirty, the next day is requeued.
	pre := dayState(t, st, id, day)
	if err := st.InsertEvents(t.Context(), []Event{{SiteID: id, TS: day*dayMS + 1000, Source: "server",
		Kind: "request", IP: "192.0.2.1", IPKey: "192.0.2.1"}}); err != nil {
		t.Fatal(err)
	}
	clean, err := st.WriteAggDay(t.Context(), AggBuild{Site: id, Day: day, Version: pre.Version, Builder: 2,
		Events: 7, RequeueNext: true})
	if err != nil || clean {
		t.Fatalf("write after a raw write: clean %v, %v", clean, err)
	}
	if d := dayState(t, st, id, day); !d.Frozen || d.Dirty != AggRawChanged || d.Builder != 2 || d.Events != 7 ||
		d.Version != pre.Version+1 {
		t.Errorf("day after a racing write: %+v", d)
	}
	if d := dayState(t, st, id, day+1); d.Dirty != AggRequeued {
		t.Errorf("next day not requeued: %+v", d)
	}

	// Retention cuts day+1 after a build read it: the build's rows are its whole raw, written but not clean; with the
	// raw gone the job clears them to stand.
	if err := st.InsertEvents(t.Context(), []Event{{SiteID: id, TS: (day+1)*dayMS + 1000, Source: "server",
		Kind: "request", IP: "192.0.2.1", IPKey: "192.0.2.1"}}); err != nil {
		t.Fatal(err)
	}
	pre = dayState(t, st, id, day+1)
	if _, err := st.deleteEvents(t.Context(), `site_id = ? AND ts < ?`, id, (day+2)*dayMS); err != nil {
		t.Fatal(err)
	}
	if d := dayState(t, st, id, day+1); d.Raw || d.Version != pre.Version+1 {
		t.Errorf("a cut day's version did not move: %+v", d)
	}
	if clean, err := st.WriteAggDay(t.Context(), AggBuild{Site: id, Day: day + 1, Version: pre.Version,
		Hours: []AggHour{{Hour: (day + 1) * 24, Cls: 1, Events: 9}}}); err != nil || clean {
		t.Fatalf("write after a cut: clean %v, %v", clean, err)
	}
	if err := st.ClearAggDirty(t.Context(), id, day+1); err != nil {
		t.Fatal(err)
	}
	if d := dayState(t, st, id, day+1); d.Dirty != AggClean || !d.Frozen {
		t.Errorf("a cut day's complete rows do not stand: %+v", d)
	}

	// A write cut short (a shutdown) after its first chunk: AggWriting, part of its rows.
	if err := st.BeginAggDay(t.Context(), id, day+2); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	n := 0
	_, err = st.WriteAggDay(ctx, AggBuild{Site: id, Day: day + 2, Addrs: func(yield func(AggAddr) bool) {
		for _, a := range aggAddrs(id, day+2, 4000) {
			if n++; n == aggChunk+100 {
				cancel()
			}
			if !yield(a) {
				return
			}
		}
	}})
	if err == nil {
		t.Fatal("a cancelled write succeeded")
	}
	written := countRows(t, st, `SELECT COUNT(*) FROM agg_addr WHERE day = ?`, day+2)
	if d := dayState(t, st, id, day+2); d.Dirty != AggWriting || written == 0 || written >= 4000 {
		t.Fatalf("a write cut short: %+v, %d rows", d, written)
	}
	// A raw write keeps the mark; a cut then makes the day unrebuildable, and the clear leaves it out of reading.
	if err := st.InsertEvents(t.Context(), []Event{{SiteID: id, TS: (day+2)*dayMS + 1000, Source: "server",
		Kind: "request", IP: "192.0.2.1", IPKey: "192.0.2.1"}}); err != nil {
		t.Fatal(err)
	}
	if d := dayState(t, st, id, day+2); d.Dirty != AggWriting {
		t.Errorf("a raw write cleared the write mark: %+v", d)
	}
	if _, err := st.deleteEvents(t.Context(), `site_id = ? AND ts < ?`, id, (day+3)*dayMS); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearAggDirty(t.Context(), id, day+2); err != nil {
		t.Fatal(err)
	}
	if d := dayState(t, st, id, day+2); d.Dirty != AggWriting || d.Raw {
		t.Errorf("partial rows cleared to clean: %+v", d)
	}
	if _, ok, err := st.OldestEventDay(t.Context(), id); ok || err != nil {
		t.Errorf("oldest day without events: %v, %v", ok, err)
	}
}

// traced runs fn with every statement the store sends recorded, its values expanded: one connection, SQLite's own
// statement trace, so the plans checked are the SQL the code runs, not copies of it.
func traced(t *testing.T, st *Store, fn func()) []string {
	t.Helper()
	var mu sync.Mutex
	var out []string
	onStmt(t, st, func(q string) {
		mu.Lock()
		out = append(out, q)
		mu.Unlock()
	}, fn)
	return out
}

// onStmt runs fn with see called on every statement the store sends, its values expanded, as it starts.
func onStmt(t *testing.T, st *Store, see func(q string), fn func()) {
	t.Helper()
	st.DB.SetMaxOpenConns(1)
	setTrace := func(on bool) {
		conn, err := st.DB.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		if err := conn.Raw(func(dc any) error {
			c := dc.(sqlite3driver.Conn).Raw()
			if !on {
				return c.Trace(0, nil)
			}
			return c.Trace(sqlite3.TRACE_STMT, func(_ sqlite3.TraceEvent, a1, _ any) error {
				if s, ok := a1.(*sqlite3.Stmt); ok {
					see(s.ExpandedSQL())
				}
				return nil
			})
		}); err != nil {
			t.Fatal(err)
		}
	}
	setTrace(true)
	fn()
	setTrace(false)
}

// The statements the store runs on the event and aggregate tables (writes, retention's deletes and the cap's every
// step, the build's handshake) search a key that bounds them: none walks a table or a whole site.
func TestAggPlans(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	now := time.Now()
	day := now.UnixMilli()/dayMS - 10
	ev := func(d int64) Event {
		return Event{SiteID: id, TS: d*dayMS + 5000, Source: "server", Kind: "request", IP: "192.0.2.1",
			IPKey: "192.0.2.1", UA: "a", Cls: 4}
	}
	stmts := traced(t, st, func() {
		ctx := t.Context()
		if err := st.InsertEvents(ctx, []Event{ev(day - 1), ev(day), ev(day + 1), ev(day + 9)}); err != nil {
			t.Fatal(err)
		}
		for _, d := range []int64{day - 1, day, day + 1} {
			b := AggBuild{Site: id, Day: d, Dims: []string{"pages"}, RequeueNext: true,
				Hours: []AggHour{{Hour: d * 24, Cls: 4, Events: 1}}, Lists: []AggList{{Dim: "pages", Part: 2, Key: "/"}},
				Addrs: slices.Values(aggAddrs(id, d, 3))}
			if d == day+1 { // a rewrite keeping its carried sessions: saved first
				b.Carried = map[AggAddrKey]int64{{IPKey: "192.0.2.1", Part: 2}: 1}
			}
			write(t, st, b)
		}
		if _, err := AggCarriedIn(ctx, st.DB, id, day); err != nil {
			t.Fatal(err)
		}
		if _, _, err := AggSavedCarriedIn(ctx, st.DB, id, day); err != nil {
			t.Fatal(err)
		}
		if err := st.InsertEvents(ctx, []Event{ev(day)}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetActorClass(ctx, ActorClass{SiteID: id, IPKey: "192.0.2.1", Day: day, Class: "bot"},
			map[string]string{"a": "bot"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.OldestEventDay(ctx, id); err != nil {
			t.Fatal(err)
		}
		if _, err := st.PruneAge(ctx, time.UnixMilli(day*dayMS)); err != nil {
			t.Fatal(err)
		}
		if err := st.ClearAggDirty(ctx, id, day-1); err != nil {
			t.Fatal(err)
		}
		if err := st.ClearAggDirtyBefore(ctx, id, day); err != nil {
			t.Fatal(err)
		}
		// The purge past its grace: it finds the oldest raw day (dirty) and waits.
		if _, err := st.ArmRawPurge(ctx, now.Add(-PurgeGrace-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, waits, err := st.PurgeRaw(ctx, RetentionOptions{RawDays: 3, Months: 25}, now); err != nil ||
			len(waits) != 1 {
			t.Fatalf("purge waits %v, %v", waits, err)
		}
		// Half the size: within reach of the aggregate steps (one byte is not: the tail's raw alone exceeds it). The
		// raw step builds the dirty day first (Cover), then deletes it.
		used, err := st.UsedBytes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		o := coverOpts(st, RetentionOptions{MaxBytes: used / 2, Months: 25,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if _, err := st.EnforceCap(ctx, o, now); err != nil {
			t.Fatal(err)
		}
	})
	seen := map[string]bool{}
	for _, q := range stmts {
		if !strings.Contains(q, "event") && !strings.Contains(q, "agg_") {
			continue
		}
		norm := strings.Join(strings.Fields(q), " ")
		if seen[norm] {
			continue
		}
		seen[norm] = true
		plan, err := plancheck.Explain(t.Context(), st.DB, q)
		if err != nil {
			t.Fatalf("explain %s: %v", norm, err)
		}
		if p := plancheck.Problem(q, plan); p != "" {
			t.Errorf("%s: %s\nplan: %v", p, norm, plan)
		}
	}
	// The statements that must have been among them.
	for _, want := range []string{
		"UPDATE agg_day SET version = version + 1, dirty = CASE",         // markAggDirty
		"DELETE FROM event WHERE id IN",                                  // deleteEvents
		"INSERT INTO agg_day (site_id, day, raw, version)",               // its mark
		"INSERT INTO agg_day (site_id, day) VALUES",                      // BeginAggDay
		"UPDATE agg_day SET dirty = 3",                                   // the write's mark
		"UPDATE agg_day SET dirty = 0 WHERE site_id = 1 AND day >=",      // ClearAggDirtyBefore
		"UPDATE event SET cls",                                           // SetActorClass
		"DELETE FROM agg_addr WHERE site_id = 1 AND (day, ip_key, part)", // age retention
		"DELETE FROM agg_addr WHERE site_id = 1 AND day =",               // a chunk of a day: write, cap
		"DELETE FROM agg_list WHERE", "DELETE FROM agg_hour WHERE", "SELECT MIN(day) FROM agg_addr",
		"SELECT MIN(hour) FROM agg_hour", "SELECT dim FROM agg_list WHERE", "SELECT MIN(ts) FROM event",
		"SELECT MIN(ts) FROM event WHERE site_id = 1 AND ts >=",                           // the purge's next day
		"DELETE FROM event WHERE id IN (SELECT id FROM event WHERE site_id = 1 AND ts >=", // a day's chunk
		"INSERT INTO agg_meta", "SELECT value FROM agg_meta",
		"INSERT INTO agg_carried", "DELETE FROM agg_carried", "SELECT ip_key, part, carried FROM agg_carried",
		"UPDATE agg_day SET addrs_cut = 1", // the cap's per-address cut of a day
	} {
		found := false
		for q := range seen {
			found = found || strings.Contains(q, want)
		}
		if !found {
			t.Errorf("not traced: %s", want)
		}
	}
}
