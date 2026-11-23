package agg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trick77/stakeout/backend/internal/store"
)

func TestLastDue(t *testing.T) {
	for _, c := range []struct {
		now  string
		want string
	}{
		{"2026-11-22T13:31:00Z", "2026-11-20"}, // days up to the 20th are frozen
		{"2026-11-23T01:59:00Z", "2026-11-20"}, // the 21st waits for 02:00
		{"2026-11-23T02:00:00Z", "2026-11-21"},
	} {
		now, _ := time.Parse(time.RFC3339, c.now)
		if got := time.UnixMilli(lastDue(now) * dayMS).UTC().Format(time.DateOnly); got != c.want {
			t.Errorf("lastDue(%s) = %s, want %s", c.now, got, c.want)
		}
	}
}

// dump is every aggregate row, built_at and builder left out: what two builds of the same raw must agree on.
func dump(t *testing.T, st *store.Store) string {
	t.Helper()
	return dumpOf(t, st, `SELECT * FROM agg_addr ORDER BY site_id, day, ip_key, part`)
}

// dumpOf is agg_day's state, the hours and lists, and the rows of more.
func dumpOf(t *testing.T, st *store.Store, more ...string) string {
	t.Helper()
	var b strings.Builder
	for _, q := range append([]string{
		`SELECT site_id, day, frozen, dirty, raw, events FROM agg_day ORDER BY 1, 2`,
		`SELECT * FROM agg_hour ORDER BY site_id, hour, cls`,
		`SELECT * FROM agg_list ORDER BY site_id, dim, day, part, other, parent, key, sub`,
	}, more...) {
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
			fmt.Fprintln(&b, vals...)
		}
		_ = rows.Close()
	}
	return b.String()
}

// A backfill cut off midway resumes where it stopped and ends with the rows of an uninterrupted one; a second run
// builds nothing.
func TestBuildThrough_ResumeIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("simulator history")
	}
	whole := historyCopy(t)
	n, err := BuildThrough(t.Context(), whole, anchor, TopK)
	if err != nil || n != len(siteDays(t, whole)) {
		t.Fatalf("built %d of %d site-days, %v", n, len(siteDays(t, whole)), err)
	}
	want := dump(t, whole)

	st := historyCopy(t)
	ctx, cancel := context.WithCancel(t.Context())
	reads := 0
	j := &job{b: &Builder{Store: st, afterRead: func(int64, int64) {
		if reads++; reads == 5 {
			cancel()
		}
	}}, log: slog.New(slog.DiscardHandler)}
	n, err = j.passes(ctx, anchor.UnixMilli()/dayMS, 0)
	if !errors.Is(err, context.Canceled) || n != 4 {
		t.Fatalf("cancelled run: built %d, %v", n, err)
	}
	// The day cut off is not readable: never frozen and clean with half its rows.
	var clean int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM agg_day WHERE frozen = 1 AND dirty = 0`).Scan(&clean); err != nil ||
		clean != 4 {
		t.Errorf("clean days after the cut: %d, %v", clean, err)
	}
	if n, err := BuildThrough(t.Context(), st, anchor, TopK); err != nil || n != len(siteDays(t, st))-4 {
		t.Fatalf("resumed: built %d, %v", n, err)
	}
	if got := dump(t, st); got != want {
		t.Error("resumed build differs from an uninterrupted one")
	}
	if n, err := BuildThrough(t.Context(), st, anchor, TopK); err != nil || n != 0 {
		t.Errorf("second run built %d, %v", n, err)
	}
	if got := dump(t, st); got != want {
		t.Error("second run changed rows")
	}
}

// view is a person's page view by key in session sid at ts.
func view(id int64, key, sid string, ts int64) store.Event {
	return store.Event{SiteID: id, TS: ts, Source: "browser", Kind: "page_view", Visitor: "v-" + key, SID: sid,
		IP: key, IPKey: key, Template: "/", Cls: store.ClsHuman}
}

// A raw write to a built day rebuilds it and the day after it (its carried sessions), not the days beyond.
func TestBuildThrough_RebuildsNextDay(t *testing.T) {
	st := openStore(t)
	id := site(t, st)
	d := anchor.UnixMilli()/dayMS - 10
	insert(t, st, view(id, "192.0.2.1", "s1", d*dayMS+1000), view(id, "192.0.2.1", "s2", (d+1)*dayMS+1000),
		view(id, "192.0.2.1", "s3", (d+2)*dayMS+1000), view(id, "192.0.2.1", "s3", (d+3)*dayMS+1000))
	if n, err := BuildThrough(t.Context(), st, anchor, TopK); err != nil || n != 11 {
		t.Fatalf("built %d, %v", n, err)
	}
	carried := func(day int64) int64 {
		t.Helper()
		var n int64
		if err := st.DB.QueryRow(`SELECT carried FROM agg_addr WHERE site_id = ? AND day = ?`, id, day).
			Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if carried(d+1) != 0 || carried(d+3) != 1 {
		t.Fatalf("carried before: %d, %d", carried(d+1), carried(d+3))
	}
	// s2 also late on day d: day d+1 now carries it.
	insert(t, st, view(id, "192.0.2.1", "s2", d*dayMS+80_000_000))
	var rebuilt []int64
	j := &job{b: &Builder{Store: st, afterRead: func(_, day int64) { rebuilt = append(rebuilt, day) }},
		log: slog.New(slog.DiscardHandler)}
	if _, err := j.passes(t.Context(), anchor.UnixMilli()/dayMS, 0); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(rebuilt) != fmt.Sprint([]int64{d, d + 1}) {
		t.Errorf("rebuilt days %v, want %d and %d", rebuilt, d, d+1)
	}
	if carried(d+1) != 1 {
		t.Errorf("carried after the rebuild = %d", carried(d+1))
	}
}

// A day retention deleted from (raw = 0) is never built or rebuilt from what is left of it, and the day after it
// keeps the carried sessions counted against its whole raw.
func TestBuildThrough_PartlyDeletedDay(t *testing.T) {
	st := openStore(t)
	id := site(t, st)
	d := anchor.UnixMilli()/dayMS - 10
	insert(t, st, view(id, "192.0.2.1", "s1", d*dayMS+3_600_000), view(id, "192.0.2.1", "s1", d*dayMS+72_000_000),
		view(id, "192.0.2.2", "s9", d*dayMS+80_000_000), view(id, "192.0.2.1", "s1", (d+1)*dayMS+1000),
		view(id, "192.0.2.3", "s5", (d+5)*dayMS+1000))
	// Day d+5 lost part of its raw before its first build.
	if _, err := st.DB.Exec(`INSERT INTO agg_day (site_id, day, raw) VALUES (?, ?, 0)`, id, d+5); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildThrough(t.Context(), st, anchor, TopK); err != nil {
		t.Fatal(err)
	}
	days, _ := st.AggDays(t.Context(), id, d, d+5)
	var hours int
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM agg_hour WHERE site_id = ? AND hour >= ? AND hour < ?`, id, (d+5)*24,
		(d+6)*24).Scan(&hours)
	if days[d+5].Frozen || hours != 0 {
		t.Errorf("a day without its whole raw was built: %+v, %d hours", days[d+5], hours)
	}
	// Day d loses its s1 events, as a cap would delete them, after it was built.
	if _, err := st.DB.Exec(`DELETE FROM event WHERE site_id = ? AND sid = 's1' AND ts < ?`, id, (d+1)*dayMS); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`UPDATE agg_day SET raw = 0 WHERE site_id = ? AND day = ?`, id, d); err != nil {
		t.Fatal(err)
	}
	// Writes to both days: d stays as built, d+1 is rebuilt (and d+2 after it) and keeps carrying s1.
	insert(t, st, view(id, "192.0.2.4", "s7", d*dayMS+5000), view(id, "192.0.2.4", "s8", (d+1)*dayMS+5000))
	if n, err := BuildThrough(t.Context(), st, anchor, TopK); err != nil || n != 2 {
		t.Fatalf("rebuilt %d, %v", n, err)
	}
	days, _ = st.AggDays(t.Context(), id, d, d+1)
	if days[d].Events != 3 || days[d].Dirty != 0 || days[d+1].Events != 2 {
		t.Errorf("after the writes: %+v", days)
	}
	var carried int64
	if err := st.DB.QueryRow(`SELECT carried FROM agg_addr WHERE site_id = ? AND day = ? AND ip_key = '192.0.2.1'`,
		id, d+1).Scan(&carried); err != nil || carried != 1 {
		t.Errorf("carried after the rebuild = %d, %v", carried, err)
	}
}

// A raw write between a build's read and its last transaction leaves the day dirty; the next pass builds it with
// the write.
func TestBuildDay_RacingWrite(t *testing.T) {
	st := openStore(t)
	id := site(t, st)
	d := anchor.UnixMilli()/dayMS - 10
	insert(t, st, view(id, "192.0.2.1", "s1", d*dayMS+1000))
	once := sync.Once{}
	b := &Builder{Store: st, afterRead: func(int64, int64) {
		once.Do(func() { insert(t, st, view(id, "192.0.2.2", "s2", d*dayMS+2000)) })
	}}
	if clean, err := b.BuildDay(t.Context(), id, d); err != nil || clean {
		t.Fatalf("build with a racing write: clean %v, %v", clean, err)
	}
	days, err := st.AggDays(t.Context(), id, d, d)
	if err != nil || !days[d].Frozen || days[d].Dirty != 1 || days[d].Events != 1 {
		t.Fatalf("after the race: %+v, %v", days[d], err)
	}
	j := &job{b: b, log: slog.New(slog.DiscardHandler)}
	if n, err := j.passes(t.Context(), d, 0); err != nil || n != 1 {
		t.Fatalf("next pass built %d, %v", n, err)
	}
	days, _ = st.AggDays(t.Context(), id, d, d)
	if days[d].Dirty != 0 || days[d].Events != 2 {
		t.Errorf("rebuilt: %+v", days[d])
	}
}

// A new builder version rebuilds every day whose raw still exists, to the same rows here; a day without raw keeps
// its rows and its old builder.
func TestBuildThrough_BuilderBump(t *testing.T) {
	st := openStore(t)
	id := site(t, st)
	d := anchor.UnixMilli()/dayMS - 3
	insert(t, st, view(id, "192.0.2.1", "s1", d*dayMS+1000), view(id, "192.0.2.1", "s1", (d+1)*dayMS+1000))
	if _, err := BuildThrough(t.Context(), st, anchor, TopK); err != nil {
		t.Fatal(err)
	}
	before := dump(t, st)
	if _, err := st.DB.Exec(`UPDATE agg_day SET raw = 0 WHERE site_id = ? AND day = ?`, id, d); err != nil {
		t.Fatal(err)
	}
	j := &job{b: &Builder{Store: st, version: Version + 1}, log: slog.New(slog.DiscardHandler)}
	n, err := j.passes(t.Context(), anchor.UnixMilli()/dayMS, 0)
	if err != nil || n != 3 {
		t.Fatalf("bump rebuilt %d, %v", n, err)
	}
	days, _ := st.AggDays(t.Context(), id, d, d+3)
	if days[d].Builder != Version || days[d+1].Builder != Version+1 || days[d+3].Builder != Version+1 {
		t.Errorf("builders: %+v", days)
	}
	if _, err := st.DB.Exec(`UPDATE agg_day SET raw = 1 WHERE site_id = ? AND day = ?`, id, d); err != nil {
		t.Fatal(err)
	}
	if after := dump(t, st); after != before {
		t.Errorf("rows changed:\n%s\nwant\n%s", after, before)
	}
}

// Run wakes at start, builds what is due and nothing newer, nor the day holding the age cutoff, and says so.
func TestRun(t *testing.T) {
	st := openStore(t)
	id := site(t, st)
	d := anchor.UnixMilli()/dayMS - 3
	for i := int64(0); i < 4; i++ {
		insert(t, st, view(id, "192.0.2.1", fmt.Sprint("s", i), (d+i)*dayMS+1000))
	}
	// As 0.1 left it: raw pruned to the millisecond a month back (13:30), the rest of that day and the next whole.
	cut := anchor.AddDate(0, -1, 0)
	cutDay := cut.UnixMilli() / dayMS
	insert(t, st, view(id, "192.0.2.2", "c1", cut.UnixMilli()+1000), view(id, "192.0.2.2", "c2", (cutDay+1)*dayMS+1000))
	var logs bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logs.Write(p)
	}), nil))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		Run(ctx, st, Options{Every: time.Hour, Months: 1, Now: func() time.Time { return anchor }, Log: log})
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		out := logs.String()
		mu.Unlock()
		if strings.Contains(out, "Aggregates built") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no build logged: %s", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	days, err := st.AggDays(t.Context(), id, d, d+4)
	if err != nil {
		t.Fatal(err)
	}
	// The anchor is the 22nd 13:30: the 20th (d+1) is the last due day.
	if len(days) != 2 || !days[d].Frozen || !days[d+1].Frozen {
		t.Errorf("days built: %+v", days)
	}
	// The cutoff's day, partial and deleted within a day, stays unbuilt; the day after is built.
	if days, err = st.AggDays(t.Context(), id, cutDay, cutDay+1); err != nil || len(days) != 1 || !days[cutDay+1].Frozen {
		t.Errorf("around the age cutoff: %+v, %v", days, err)
	}
}

// recorder is a job whose builder records every day it starts a build of (BeginAggDay, also when it then finds the
// raw gone).
func recorder(st *store.Store) (*job, *[]int64) {
	var days []int64
	return &job{b: &Builder{Store: st, beforeRead: func(_, day int64) { days = append(days, day) }},
		log: slog.New(slog.DiscardHandler)}, &days
}

// A day the size cap cut aggregate rows of (frozen 0, AggWriting) has no raw: the job never starts a build of it nor
// clears it, pass after pass, whether its site still has raw on later days or none at all.
func TestBuildThrough_CapCutDay(t *testing.T) {
	st := openStore(t)
	rows, err := st.SyncSites(t.Context(), []store.Site{{Domain: "a.example.com", Kind: "site", PathRules: "[]"},
		{Domain: "b.example.com", Kind: "site", PathRules: "[]"}})
	if err != nil {
		t.Fatal(err)
	}
	a, b := rows["a.example.com"].ID, rows["b.example.com"].ID
	d, today := anchor.UnixMilli()/dayMS-10, anchor.UnixMilli()/dayMS
	pad := strings.Repeat("x", 1000)
	var evs []store.Event
	for _, id := range []int64{a, b} {
		for i := range 300 {
			ev := view(id, fmt.Sprintf("198.51.100.%d-%d", i%256, i/256), fmt.Sprint("s", i), d*dayMS+int64(i)*1000)
			ev.UA = pad
			evs = append(evs, ev)
		}
	}
	// Site a keeps raw in the cap's tail; site b has none left after the cap.
	insert(t, st, append(evs, view(a, "192.0.2.1", "t1", today*dayMS+1000))...)
	if _, err := BuildThrough(t.Context(), st, anchor, TopK); err != nil {
		t.Fatal(err)
	}
	used, err := st.UsedBytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnforceCap(t.Context(), used/10, anchor, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	cut := func(when string) {
		t.Helper()
		for _, id := range []int64{a, b} {
			if day, _ := dayEvents(t, st, id, d); day.Frozen || day.Dirty != store.AggWriting || day.Raw {
				t.Errorf("%s: site %d's cut day %+v, want unbuilt, AggWriting, without raw", when, id, day)
			}
		}
	}
	cut("after the cap")
	j, built := recorder(st)
	for pass := range 2 {
		if n, err := j.passes(t.Context(), today, 0); err != nil || n != 0 || len(*built) != 0 {
			t.Errorf("pass %d: built %d, started %v, %v", pass, n, *built, err)
		}
		cut(fmt.Sprint("after pass ", pass))
	}
}

// A cut day that still has its raw (the cap cuts aggregates only once no raw before its tail is left, so only a cap
// stopped mid-way or a raw write could leave one: set by hand) is rebuilt once, to the rows it had, and clean.
func TestBuildThrough_CapCutDayWithRaw(t *testing.T) {
	st := openStore(t)
	id := site(t, st)
	d := anchor.UnixMilli()/dayMS - 10
	insert(t, st, view(id, "192.0.2.1", "s1", d*dayMS+1000), view(id, "192.0.2.2", "s2", d*dayMS+2000),
		view(id, "192.0.2.2", "s2", (d+1)*dayMS+1000))
	if _, err := BuildThrough(t.Context(), st, anchor, TopK); err != nil {
		t.Fatal(err)
	}
	want := dump(t, st)
	for _, q := range []string{`UPDATE agg_day SET frozen = 0, dirty = 3 WHERE day = ?`,
		`DELETE FROM agg_addr WHERE day = ? AND ip_key = '192.0.2.1'`} {
		if _, err := st.DB.Exec(q, d); err != nil {
			t.Fatal(err)
		}
	}
	j, built := recorder(st)
	if _, err := j.passes(t.Context(), anchor.UnixMilli()/dayMS, 0); err != nil || len(*built) == 0 ||
		(*built)[0] != d || slices.Contains((*built)[1:], d) {
		t.Fatalf("builds started %v, %v; want day %d once", *built, err, d)
	}
	if day, _ := dayEvents(t, st, id, d); !day.Frozen || day.Dirty != store.AggClean {
		t.Errorf("rebuilt: %+v", day)
	}
	if got := dump(t, st); got != want {
		t.Errorf("rows:\n%s\nwant\n%s", got, want)
	}
	*built = nil
	if n, err := j.passes(t.Context(), anchor.UnixMilli()/dayMS, 0); err != nil || n != 0 || len(*built) != 0 {
		t.Errorf("next pass built %d, started %v, %v", n, *built, err)
	}
}

// A write cut short (a shutdown) leaves the day frozen and AggWriting with part of its rows; with its raw still there
// the next pass rebuilds it to the rows of an uninterrupted build, clean.
func TestBuildThrough_CrashRebuilt(t *testing.T) {
	st, id, d := cutDay(t, 3000)
	if _, err := BuildThrough(t.Context(), st, anchor, TopK); err != nil {
		t.Fatal(err)
	}
	want := dump(t, st)
	ctx, cancel := context.WithCancel(t.Context())
	b := &Builder{Store: st, beforeWrite: func(w *store.AggBuild) {
		addrs := w.Addrs
		w.Addrs = func(yield func(store.AggAddr) bool) {
			i := 0
			for a := range addrs {
				if i++; i == 2100 {
					cancel()
				}
				if !yield(a) {
					return
				}
			}
		}
	}}
	if _, err := b.BuildDay(ctx, id, d); err == nil {
		t.Fatal("a cancelled rebuild succeeded")
	}
	if day, _ := dayEvents(t, st, id, d); !day.Frozen || day.Dirty != store.AggWriting || !day.Raw {
		t.Fatalf("after the shutdown: %+v", day)
	}
	j, built := recorder(st)
	if _, err := j.passes(t.Context(), anchor.UnixMilli()/dayMS, 0); err != nil || len(*built) == 0 || (*built)[0] != d {
		t.Fatalf("builds started %v, %v; want day %d first", *built, err, d)
	}
	if got := dump(t, st); got != want {
		t.Error("rebuilt rows differ from an uninterrupted build's")
	}
	*built = nil
	if n, err := j.passes(t.Context(), anchor.UnixMilli()/dayMS, 0); err != nil || n != 0 || len(*built) != 0 {
		t.Errorf("next pass built %d, started %v, %v", n, *built, err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// cutDay fills a day with n page views of distinct addresses, padded so that one retention chunk is part of it.
func cutDay(t *testing.T, n int) (*store.Store, int64, int64) {
	t.Helper()
	st := openStore(t)
	id := site(t, st)
	d := anchor.UnixMilli()/dayMS - 10
	evs := make([]store.Event, n)
	for i := range evs {
		evs[i] = view(id, fmt.Sprintf("198.51.100.%d-%d", i%256, i/256), fmt.Sprint("s", i), d*dayMS+int64(i)*1000)
		evs[i].Props = `{"pad":"` + strings.Repeat("x", 200) + `"}`
	}
	insert(t, st, evs...)
	return st, id, d
}

// dayEvents is what a site-day's rows count: agg_day's events and the sum of its hours.
func dayEvents(t *testing.T, st *store.Store, id, d int64) (store.AggDay, int64) {
	t.Helper()
	days, err := st.AggDays(t.Context(), id, d, d)
	if err != nil {
		t.Fatal(err)
	}
	var hours int64
	if err := st.DB.QueryRow(`SELECT COALESCE(SUM(events), 0) FROM agg_hour WHERE site_id = ? AND hour >= ? AND
		hour < ?`, id, d*24, d*24+24).Scan(&hours); err != nil {
		t.Fatal(err)
	}
	return days[d], hours
}

// Retention cutting a day while it is built (the whole day by age, or one chunk of it by the cap) never leaves it
// read clean from part of its raw. Cut before the build's read: nothing is written, the rows built before from all
// of it stand. Cut after the read: the rows written are the whole raw the read saw, the day not clean until the job,
// finding the raw gone, lets them stand.
func TestBuildDay_CutDuringBuild(t *testing.T) {
	const n = 5000
	discard := slog.New(slog.DiscardHandler)
	cuts := map[string]func(st *store.Store, d int64) error{
		"whole day": func(st *store.Store, d int64) error {
			_, err := st.PruneAge(t.Context(), time.UnixMilli((d+1)*dayMS))
			return err
		},
		"one cap chunk": func(st *store.Store, _ int64) error {
			used, err := st.UsedBytes(t.Context())
			if err == nil {
				_, err = st.EnforceCap(t.Context(), used*97/100, anchor, discard)
			}
			return err
		},
	}
	for name, cut := range cuts {
		t.Run(name+" before the read", func(t *testing.T) {
			st, id, d := cutDay(t, n)
			if _, err := BuildThrough(t.Context(), st, anchor, TopK); err != nil {
				t.Fatal(err)
			}
			// Age retention takes the per-address rows with the raw: the other rows are what must stand.
			before := dumpOf(t, st)
			b := &Builder{Store: st, version: Version + 1, beforeRead: func(int64, int64) {
				if err := cut(st, d); err != nil {
					t.Fatal(err)
				}
			}}
			if _, err := b.BuildDay(t.Context(), id, d); err != nil {
				t.Fatal(err)
			}
			var left int
			_ = st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE site_id = ?`, id).Scan(&left)
			if left == n {
				t.Fatal("nothing cut")
			}
			if day, hours := dayEvents(t, st, id, d); day.Builder != Version || hours != n || day.Dirty != 0 {
				t.Errorf("rows built from the whole day replaced: %+v, %d events in its hours", day, hours)
			}
			if _, err := st.DB.Exec(`UPDATE agg_day SET raw = 1, version = 0`); err != nil {
				t.Fatal(err)
			}
			if after := dumpOf(t, st); after != before {
				t.Errorf("rows changed:\n%s\nwant\n%s", after, before)
			}
		})
		t.Run(name+" after the read", func(t *testing.T) {
			st, id, d := cutDay(t, n)
			b := &Builder{Store: st, afterRead: func(int64, int64) {
				if err := cut(st, d); err != nil {
					t.Fatal(err)
				}
			}}
			clean, err := b.BuildDay(t.Context(), id, d)
			if err != nil || clean {
				t.Fatalf("build cut after its read: clean %v, %v", clean, err)
			}
			if day, hours := dayEvents(t, st, id, d); !day.Frozen || day.Dirty != store.AggRawChanged ||
				day.Events != n || hours != n {
				t.Errorf("after the cut: %+v, %d events in its hours", day, hours)
			}
			j := &job{b: b, log: discard}
			if _, err := j.passes(t.Context(), d, 0); err != nil {
				t.Fatal(err)
			}
			if day, hours := dayEvents(t, st, id, d); day.Dirty != 0 || hours != n {
				t.Errorf("the whole raw's rows do not stand: %+v, %d", day, hours)
			}
		})
	}
}

// A rebuild cut short mid-write (a shutdown) and then cut by retention: the day's rows are partial and nothing can
// rebuild them, so it stays out of reading (AggWriting) instead of being cleared to clean.
func TestBuildDay_CrashThenCut(t *testing.T) {
	st, id, d := cutDay(t, 3000)
	if _, err := BuildThrough(t.Context(), st, anchor, TopK); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	b := &Builder{Store: st, version: Version + 1, beforeWrite: func(w *store.AggBuild) {
		addrs := w.Addrs
		w.Addrs = func(yield func(store.AggAddr) bool) {
			i := 0
			for a := range addrs {
				if i++; i == 2100 {
					cancel()
				}
				if !yield(a) {
					return
				}
			}
		}
	}}
	if _, err := b.BuildDay(ctx, id, d); err == nil {
		t.Fatal("a cancelled rebuild succeeded")
	}
	var rows int
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM agg_addr WHERE site_id = ? AND day = ?`, id, d).Scan(&rows)
	if day, _ := dayEvents(t, st, id, d); day.Dirty != store.AggWriting || rows >= 3000 {
		t.Fatalf("after the shutdown: %+v, %d address rows", day, rows)
	}
	if _, err := st.PruneAge(t.Context(), time.UnixMilli((d+1)*dayMS)); err != nil {
		t.Fatal(err)
	}
	j := &job{b: &Builder{Store: st, version: Version + 1}, log: slog.New(slog.DiscardHandler)}
	if _, err := j.passes(t.Context(), anchor.UnixMilli()/dayMS, 0); err != nil {
		t.Fatal(err)
	}
	if clean, err := j.b.BuildDay(t.Context(), id, d); err != nil || clean {
		t.Errorf("build of the cut day: clean %v, %v", clean, err)
	}
	if day, _ := dayEvents(t, st, id, d); day.Dirty != store.AggWriting || day.Raw {
		t.Errorf("partial rows cleared to clean: %+v", day)
	}
}
