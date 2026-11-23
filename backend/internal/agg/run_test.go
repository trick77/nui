package agg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
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

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
