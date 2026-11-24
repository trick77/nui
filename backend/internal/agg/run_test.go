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
	"sync/atomic"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/store"
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
	// Below what the schema's pages take: every aggregate row goes, the lists and hours too.
	if _, err := st.EnforceCap(t.Context(), store.RetentionOptions{MaxBytes: used / 100, Builder: Version,
		Log: slog.New(slog.DiscardHandler)}, anchor); err != nil {
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

// Retention cutting a day while it is rebuilt (the whole day by age, a chunk of it by the cap or the raw purge)
// never leaves it read clean from part of its raw. Cut before the build's read: nothing is written, the rows built
// before from all of it stand. Cut after the read: the rows written are the whole raw the read saw, the day not clean
// until the job, finding the raw gone, lets them stand.
func TestBuildDay_CutDuringBuild(t *testing.T) {
	const n = 5000
	discard := slog.New(slog.DiscardHandler)
	cuts := map[string]func(st *store.Store, d int64) error{
		"whole day": func(st *store.Store, d int64) error {
			_, err := st.PruneAge(t.Context(), time.UnixMilli((d+1)*dayMS))
			return err
		},
		"cap chunks": func(st *store.Store, _ int64) error {
			used, err := st.UsedBytes(t.Context())
			if err == nil {
				_, err = st.EnforceCap(t.Context(), store.RetentionOptions{MaxBytes: used * 97 / 100, Builder: Version,
					Log: discard}, anchor)
			}
			return err
		},
		// The raw purge, the day built clean by this builder before the rebuild started.
		"purge": func(st *store.Store, _ int64) error {
			if _, err := st.ArmRawPurge(t.Context(), anchor.Add(-store.PurgeGrace)); err != nil {
				return err
			}
			_, _, err := st.PurgeRaw(t.Context(), store.RetentionOptions{RawDays: 3, Builder: Version, Log: discard},
				anchor)
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
			// As a build of this builder leaves the day and the next one (no rows needed: the rebuild replaces them),
			// so the cap and the purge may cut it.
			for _, day := range []int64{d, d + 1} {
				if err := st.BeginAggDay(t.Context(), id, day); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.DB.Exec(`UPDATE agg_day SET frozen = 1, builder = ?`, Version); err != nil {
				t.Fatal(err)
			}
			b := &Builder{Store: st, version: Version + 1, afterRead: func(int64, int64) {
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

// The purge never deletes a day before the day after it is built against its raw: a first build of that day would
// find no sessions to carry over midnight and count them twice. Purged afterwards, a rebuild of the next day keeps
// the carried count it had.
func TestPurge_NextDayCarried(t *testing.T) {
	st := openStore(t)
	id := site(t, st)
	d := anchor.UnixMilli()/dayMS - 10
	insert(t, st, view(id, "192.0.2.1", "s1", (d+1)*dayMS-600_000), view(id, "192.0.2.1", "s1", (d+1)*dayMS+600_000),
		view(id, "192.0.2.2", "s2", (d+2)*dayMS+1000))
	if _, err := st.ArmRawPurge(t.Context(), anchor.Add(-store.PurgeGrace)); err != nil {
		t.Fatal(err)
	}
	discard := slog.New(slog.DiscardHandler)
	o := store.RetentionOptions{RawDays: 3, Builder: Version, Log: discard}
	purge := func() []store.PurgeWait {
		t.Helper()
		_, waits, err := st.PurgeRaw(t.Context(), o, anchor)
		if err != nil {
			t.Fatal(err)
		}
		return waits
	}
	carried := func() (n int64) {
		t.Helper()
		if err := st.DB.QueryRow(`SELECT carried FROM agg_addr WHERE site_id = ? AND day = ? AND ip_key = '192.0.2.1'`,
			id, d+1).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Day d built, the next day not yet: d waits.
	if _, err := (&Builder{Store: st}).BuildDay(t.Context(), id, d); err != nil {
		t.Fatal(err)
	}
	if w := purge(); len(w) != 1 || w[0].Day != d {
		t.Fatalf("waits %v, want day %d", w, d)
	}
	if days, _ := st.AggDays(t.Context(), id, d, d); !days[d].Raw {
		t.Fatal("day purged before the next day was built")
	}
	// Every day built; a write into d+2 keeps d+1's raw (its next day dirty) while d goes.
	if _, err := BuildThrough(t.Context(), st, anchor, TopK); err != nil {
		t.Fatal(err)
	}
	insert(t, st, view(id, "192.0.2.3", "s3", (d+2)*dayMS+2000))
	if w := purge(); len(w) != 1 || w[0].Day != d+1 {
		t.Fatalf("waits %v, want day %d", w, d+1)
	}
	// d+1 rebuilt with d's raw gone (a new builder): s1 is still carried, one visit over the two days.
	if _, err := (&Builder{Store: st, version: Version + 1}).BuildDay(t.Context(), id, d+1); err != nil {
		t.Fatal(err)
	}
	if n := carried(); n != 1 {
		t.Errorf("carried after the purge and a rebuild = %d, want 1", n)
	}
}

// Cover builds only what the job would build now: not a day before the age floor, not one not yet due; and a day
// built clean by this builder not again. The job and Cover never build at once (one lock per store).
func TestCover(t *testing.T) {
	st := openStore(t)
	id := site(t, st)
	today := anchor.UnixMilli() / dayMS
	floor := ageFloor(anchor, 1)
	for _, d := range []int64{floor - 1, floor, today - 2, today - 1} {
		insert(t, st, view(id, "192.0.2.1", fmt.Sprint("s", d), d*dayMS+1000))
	}
	cover := Cover(st, Options{Months: 1, Now: func() time.Time { return anchor }})
	for _, d := range []int64{floor - 1, floor, today - 2, today - 1} {
		// Not due yet is the cap's to wait for; before the floor is never built, no wait.
		err := cover(t.Context(), id, d)
		if notDue := d == today-1; errors.Is(err, store.ErrNotDue) != notDue || !notDue && err != nil {
			t.Fatalf("day %d: %v", d-today, err)
		}
	}
	days, err := st.AggDays(t.Context(), id, floor-1, today)
	if err != nil {
		t.Fatal(err)
	}
	// The anchor is the 22nd 13:30: the 20th (today-2) is due, the 21st not.
	built := func(d int64) bool { r, ok := days[d]; return ok && r.Frozen && r.Dirty == store.AggClean }
	if built(floor-1) || !built(floor) || !built(today-2) || built(today-1) {
		t.Errorf("covered days %+v", days)
	}
	// Built clean already: neither Cover nor the job builds it again, whatever a stale list of the job's says.
	if err := cover(t.Context(), id, floor); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.AggDays(t.Context(), id, floor, floor); again[floor].BuiltAt != days[floor].BuiltAt {
		t.Errorf("Cover built a clean day again: %+v", again[floor])
	}
	j, started := recorder(st)
	if did, err := j.b.buildIfNeeded(t.Context(), id, floor); err != nil || did || len(*started) != 0 {
		t.Errorf("the job built a clean day again: %v %v %v", did, *started, err)
	}
}

// The first raw day after a purged one keeps its carried sessions (they cannot be counted again: the day before's
// raw is gone) through rewrites cut short, twice: each retry takes them from the copy saved before the first
// delete, not from the partial rows, and the finished day holds them all, the copy gone.
func TestBuildDay_KeepAfterCrash(t *testing.T) {
	st := openStore(t)
	id := site(t, st)
	d := anchor.UnixMilli()/dayMS - 10
	const n = 3000 // past a chunk: the copy is saved in more than one transaction
	var evs []store.Event
	for i := range n {
		k := fmt.Sprintf("2001:db8::%x", i)
		evs = append(evs, view(id, k, fmt.Sprint("s", i), d*dayMS-60_000), view(id, k, fmt.Sprint("s", i), d*dayMS+60_000))
	}
	insert(t, st, evs...)
	if _, err := BuildThrough(t.Context(), st, anchor, TopK); err != nil {
		t.Fatal(err)
	}
	carried := func() (n int64) {
		t.Helper()
		if err := st.DB.QueryRow(`SELECT COALESCE(SUM(carried), 0) FROM agg_addr WHERE site_id = ? AND day = ?`, id,
			d).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if c := carried(); c != n {
		t.Fatalf("carried before %d", c)
	}
	if _, err := st.ArmRawPurge(t.Context(), anchor.Add(-store.PurgeGrace)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.PurgeRaw(t.Context(), store.RetentionOptions{Months: 25, RawDays: 10, Builder: Version,
		Log: slog.New(slog.DiscardHandler)}, anchor); err != nil {
		t.Fatal(err)
	}
	if days, _ := st.AggDays(t.Context(), id, d-1, d); days[d-1].Raw || !days[d].Raw {
		t.Fatalf("days %+v", days)
	}
	// A rewrite for a new builder, cut short after at rows written.
	crash := func(at int) {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		b := &Builder{Store: st, version: Version + 1, beforeWrite: func(w *store.AggBuild) {
			addrs := w.Addrs
			w.Addrs = func(yield func(store.AggAddr) bool) {
				i := 0
				for a := range addrs {
					if i++; i == at {
						cancel()
					}
					if !yield(a) {
						return
					}
				}
			}
		}}
		if _, err := b.BuildDay(ctx, id, d); err == nil {
			t.Fatal("rewrite cut short succeeded")
		}
		if days, _ := st.AggDays(t.Context(), id, d, d); days[d].Dirty != store.AggWriting || carried() == n {
			t.Fatalf("after the cut-short rewrite: %+v, carried %d", days[d], carried())
		}
	}
	crash(2100)
	crash(500)
	j := &job{b: &Builder{Store: st, version: Version + 1}, log: slog.New(slog.DiscardHandler)}
	if _, err := j.passes(t.Context(), lastDue(anchor), 0); err != nil {
		t.Fatal(err)
	}
	days, _ := st.AggDays(t.Context(), id, d, d)
	if c := carried(); c != n || !days[d].BuiltBy(Version+1) {
		t.Errorf("after the rebuild: %+v, carried %d, want %d", days[d], c, n)
	}
	var copies int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM agg_carried`).Scan(&copies); err != nil || copies != 0 {
		t.Errorf("%d saved rows left, %v", copies, err)
	}
}

// padded is per events on each day from first to last, each a session of its own, a long user agent each: rows the
// size cap has to work through.
func padded(t *testing.T, st *store.Store, id, first, last int64, per int) {
	t.Helper()
	var evs []store.Event
	for d := first; d <= last; d++ {
		for i := range per {
			e := view(id, fmt.Sprint("192.0.2.", i%200), fmt.Sprint("s", d, i), d*dayMS+int64(i)*1000)
			e.UA = strings.Repeat("u", 400)
			evs = append(evs, e)
		}
	}
	insert(t, st, evs...)
}

// After a downgrade every day is a newer builder's: the older release neither rebuilds them (newer rows stand) nor
// stalls on them. The purge and the size cap count them as built, the real Cover included, and delete their raw.
func TestRetention_Downgrade(t *testing.T) {
	st := openStore(t)
	id := site(t, st)
	today := anchor.UnixMilli() / dayMS
	padded(t, st, id, today-12, today, 400)
	newer := &job{b: &Builder{Store: st, version: Version + 1}, log: slog.New(slog.DiscardHandler)}
	if _, err := newer.passes(t.Context(), today, 0); err != nil {
		t.Fatal(err)
	}
	older := &job{b: &Builder{Store: st}, log: slog.New(slog.DiscardHandler)}
	if n, err := older.passes(t.Context(), lastDue(anchor), 3); err != nil || n != 0 {
		t.Fatalf("the older job rebuilt %d days, %v", n, err)
	}
	if _, err := st.ArmRawPurge(t.Context(), anchor.Add(-store.PurgeGrace)); err != nil {
		t.Fatal(err)
	}
	o := store.RetentionOptions{Months: 25, RawDays: 6, Builder: Version, Log: slog.New(slog.DiscardHandler),
		Cover: Cover(st, Options{Months: 25, Now: func() time.Time { return anchor }})}
	if n, waits, err := st.PurgeRaw(t.Context(), o, anchor); err != nil || n == 0 || len(waits) != 0 {
		t.Fatalf("purge after a downgrade: %d, waits %v, %v", n, waits, err)
	}
	used, _ := st.UsedBytes(t.Context())
	o.MaxBytes = used / 2
	if _, err := st.EnforceCap(t.Context(), o, anchor); err != nil {
		t.Fatal(err)
	}
	if after, _ := st.UsedBytes(t.Context()); after > o.MaxBytes {
		t.Errorf("cap not enforced after a downgrade: %d > %d", after, o.MaxBytes)
	}
}

// The size cap stops part-way through a day (its raw cut), then a release with a newer builder rebuilds every day
// with raw, not the cut one: the purge takes the rest of the cut day (its rows complete, an older builder's) and goes
// on to the raw days.
func TestPurge_CapCutThenBump(t *testing.T) {
	st := openStore(t)
	id := site(t, st)
	today := anchor.UnixMilli() / dayMS
	padded(t, st, id, today-6, today-1, 5000)
	if _, err := BuildThrough(t.Context(), st, anchor, TopK); err != nil {
		t.Fatal(err)
	}
	used, _ := st.UsedBytes(t.Context())
	if _, err := st.EnforceCap(t.Context(), store.RetentionOptions{Months: 25, Builder: Version, MaxBytes: used - 1,
		Cover: Cover(st, Options{Months: 25, Now: func() time.Time { return anchor }}),
		Log:   slog.New(slog.DiscardHandler)}, anchor); err != nil {
		t.Fatal(err)
	}
	var cut, left int64
	if err := st.DB.QueryRow(`SELECT MIN(ts) / ?, COUNT(*) FROM event WHERE ts < (SELECT MIN(ts) / ? * ? + ? FROM event)`,
		dayMS, dayMS, dayMS, dayMS).Scan(&cut, &left); err != nil {
		t.Fatal(err)
	}
	if day, _ := dayEvents(t, st, id, cut); day.Raw || left == 0 || left == 5000 {
		t.Fatalf("the cap left no day cut part-way: oldest raw day %+v with %d events", day, left)
	}
	newer := &job{b: &Builder{Store: st, version: Version + 1}, log: slog.New(slog.DiscardHandler)}
	if _, err := newer.passes(t.Context(), lastDue(anchor), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ArmRawPurge(t.Context(), anchor.Add(-store.PurgeGrace)); err != nil {
		t.Fatal(err)
	}
	if _, waits, err := st.PurgeRaw(t.Context(), store.RetentionOptions{Months: 25, RawDays: 3, Builder: Version + 1,
		Log: slog.New(slog.DiscardHandler)}, anchor); err != nil || len(waits) != 0 {
		t.Fatalf("waits %v, %v", waits, err)
	}
	var old int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE ts < ?`, store.RawCutDay(anchor, 3)*dayMS).
		Scan(&old); err != nil || old != 0 {
		t.Errorf("%d raw events older than the raw days stay (%v)", old, err)
	}
}

// The cap's wait for a day not due ends by itself: the cap asks Cover for days before its tail and the day after
// each, and at every minute of a day lastDue is at least the day before the tail, from 02:00 UTC the tail's first.
// Only the tail's first day is ever not due to the cap, and only from 00:00 to 02:00.
func TestCover_WaitBounded(t *testing.T) {
	midnight := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	for m := range 24 * 60 {
		now := midnight.Add(time.Duration(m) * time.Minute)
		tail := now.UnixMilli()/dayMS - (store.CapTail - 1)
		if due := lastDue(now); due < tail-1 || (due >= tail) != (now.Hour() >= 2) {
			t.Fatalf("%s: last due day %d, the tail from %d", now.Format(time.TimeOnly), due, tail)
		}
	}
}

// From 00:00 to 02:00 UTC the cap's oldest raw day waits for the day after it, not due yet: the rounds delete no
// aggregate (that day's raw can go two hours later; here it is enough) and warn once. At 02:01 the next round's Cover builds
// the day after and the raw goes, every aggregate row kept. The real Cover, through RunRetention, no job running.
func TestRetention_CapWaitsForDue(t *testing.T) {
	st := openStore(t)
	id := site(t, st)
	discard := slog.New(slog.DiscardHandler)
	night := time.Date(2026, 11, 23, 1, 0, 0, 0, time.UTC)
	today := night.UnixMilli() / dayMS
	wait := today - store.CapTail // the cap's oldest day: built, the day after it due at 02:00
	padded(t, st, id, today-12, today, 1000)
	if _, err := (&job{b: &Builder{Store: st}, log: discard}).passes(t.Context(), lastDue(night), 0); err != nil {
		t.Fatal(err)
	}
	// The older days' raw purged: their aggregates alone stand for them.
	if _, err := st.ArmRawPurge(t.Context(), night.Add(-store.PurgeGrace)); err != nil {
		t.Fatal(err)
	}
	if _, waits, err := st.PurgeRaw(t.Context(), store.RetentionOptions{Months: 25, RawDays: store.CapTail,
		Builder: Version, Log: discard}, night); err != nil || len(waits) != 0 {
		t.Fatalf("purge: waits %v, %v", waits, err)
	}
	raw := func(d int64) (n int) {
		_ = st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE ts >= ? AND ts < ?`, d*dayMS, (d+1)*dayMS).Scan(&n)
		return n
	}
	// The aggregate rows up to that day (02:01's Cover builds the next).
	aggs := func() (n [3]int) {
		_ = st.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM agg_addr WHERE day <= ?1),
			(SELECT COUNT(*) FROM agg_list WHERE day <= ?1), (SELECT COUNT(*) FROM agg_hour WHERE hour < (?1 + 1) * 24)`,
			wait).Scan(&n[0], &n[1], &n[2])
		return n
	}
	if raw(wait-1) != 0 || raw(wait) != 1000 || aggs()[0] == 0 {
		t.Fatalf("fixture: raw %d before the day, %d on it, aggregates %v", raw(wait-1), raw(wait), aggs())
	}
	kept := aggs()
	if err := st.Vacuum(t.Context()); err != nil { // the purge's free pages back: a round's vacuum would shrink used
		t.Fatal(err)
	}
	used, _ := st.UsedBytes(t.Context())
	var clock atomic.Int64
	clock.Store(night.UnixMilli())
	now := func() time.Time { return time.UnixMilli(clock.Load()).UTC() }
	cover := Cover(st, Options{Months: 25, Now: now})
	var asked atomic.Int64 // the rounds that asked for the day after, not due
	var logs bytes.Buffer
	var mu sync.Mutex
	o := store.RetentionOptions{Months: 25, Builder: Version, MaxBytes: used - 1, CapEvery: 10 * time.Millisecond,
		Now: now, Log: slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			return logs.Write(p)
		}), nil)),
		Cover: func(ctx context.Context, site, day int64) error {
			if day == wait+1 && clock.Load() == night.UnixMilli() {
				asked.Add(1)
			}
			return cover(ctx, site, day)
		}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		st.RunRetention(ctx, o)
		close(done)
	}()
	defer func() { cancel(); <-done }()
	until := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				mu.Lock()
				defer mu.Unlock()
				t.Fatalf("timed out waiting for %s:\n%s", what, logs.String())
			}
		}
	}
	until("three rounds at 01:00", func() bool { return asked.Load() >= 3 || aggs() != kept })
	if got := aggs(); got != kept || raw(wait) != 1000 {
		t.Errorf("at 01:00: aggregates %v, before %v; the day's raw %d of 1000", got, kept, raw(wait))
	}
	clock.Store(night.Add(61 * time.Minute).UnixMilli())
	until("the day's raw to go at 02:01", func() bool { return raw(wait) == 0 })
	cancel()
	<-done
	if got := aggs(); got != kept {
		t.Errorf("at 02:01: aggregates %v, before %v", got, kept)
	}
	for d := wait + 1; d <= today; d++ {
		if raw(d) != 1000 {
			t.Errorf("tail day %d: %d events", d-today, raw(d))
		}
	}
	if after, _ := st.UsedBytes(t.Context()); after > o.MaxBytes*9/10 {
		t.Errorf("used %d after 02:01, target %d", after, o.MaxBytes*9/10)
	}
	mu.Lock()
	defer mu.Unlock()
	out := logs.String()
	if strings.Count(out, "Size cap waits") != 1 || strings.Count(out, "deleting the oldest raw events") != 1 ||
		strings.Contains(out, "aggregates stay") || strings.Contains(out, "alone exceed") {
		t.Errorf("warnings:\n%s", out)
	}
}
