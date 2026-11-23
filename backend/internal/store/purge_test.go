package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trick77/stakeout/backend/internal/app"
)

// purgeDays fills a site with n events on each of the days from first to last and builds each day clean by builder
// 1, as the job does: a day of raw with its figures standing.
func purgeDays(t *testing.T, st *Store, id, first, last int64, n int) {
	t.Helper()
	var evs []Event
	for d := first; d <= last; d++ {
		for i := range n {
			evs = append(evs, Event{SiteID: id, TS: d*dayMS + int64(i)*1000, Source: "browser", Kind: "page_view",
				IP: "192.0.2.1", IPKey: "192.0.2.1", SID: fmt.Sprint("s", d), Props: `{"pad":"` + strings.Repeat("x", 200) + `"}`})
		}
	}
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	for d := first; d <= last; d++ {
		write(t, st, AggBuild{Site: id, Day: d, Builder: 1, Events: int64(n), Dims: []string{"pages"},
			Hours: []AggHour{{Hour: d * 24, Cls: 1, Events: int64(n)}}, Lists: []AggList{{Dim: "pages", Part: 1, Key: "/"}},
			Addrs: slices.Values(aggAddrs(id, d, 2))})
	}
}

// rawDays is how many events each of a site's days holds, keyed by day.
func rawDays(t *testing.T, st *Store, id int64) map[int64]int {
	t.Helper()
	rows, err := st.DB.Query(`SELECT ts / 86400000, COUNT(*) FROM event WHERE site_id = ? GROUP BY 1`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]int{}
	for rows.Next() {
		var d int64
		var n int
		if err := rows.Scan(&d, &n); err != nil {
			t.Fatal(err)
		}
		out[d] = n
	}
	return out
}

// The purge deletes nothing before its grace has passed: not before the first start is recorded, not a minute
// before the week is up, and the recorded start survives a restart (a later start does not move it). Past the
// grace, the days before the raw days go, whole, marked without raw and their figures kept; the rest stays.
func TestPurgeRaw_Grace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	st, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	id := seed(t, st)
	start := time.Now().Truncate(time.Hour)
	today := start.UnixMilli() / dayMS
	purgeDays(t, st, id, today-20, today, 3)
	o := RetentionOptions{RawDays: 10, Months: 25, Builder: 1}
	purge := func(at time.Time) int64 {
		t.Helper()
		n, waits, err := st.PurgeRaw(t.Context(), o, at)
		if err != nil || len(waits) != 0 {
			t.Fatalf("purge at %v: %d, waits %v, %v", at, n, waits, err)
		}
		return n
	}
	if n := purge(start.Add(30 * 24 * time.Hour)); n != 0 {
		t.Errorf("purged %d before any start was recorded", n)
	}
	if armed, err := st.ArmRawPurge(t.Context(), start); err != nil || !armed.Equal(start) {
		t.Fatalf("armed %v, %v", armed, err)
	}
	if n := purge(start.Add(PurgeGrace - time.Minute)); n != 0 {
		t.Errorf("purged %d within the grace", n)
	}
	// A restart: the first start stands.
	_ = st.Close()
	if st, err = Open(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if armed, err := st.ArmRawPurge(t.Context(), start.Add(3*24*time.Hour)); err != nil || !armed.Equal(start) {
		t.Fatalf("after a restart: armed %v, %v; want the first start %v", armed, err, start)
	}
	if n := purge(start.Add(PurgeGrace - time.Minute)); n != 0 {
		t.Errorf("purged %d within the grace after a restart", n)
	}
	// Past it, at the start's own time of day a week on: raw kept 10 days.
	at := start.Add(PurgeGrace)
	cut := RawCutDay(at, 10)
	if n := purge(at); n == 0 {
		t.Fatal("nothing purged past the grace")
	}
	left := rawDays(t, st, id)
	for d := today - 20; d <= today; d++ {
		want := 3
		if d < cut {
			want = 0
		}
		if left[d] != want {
			t.Errorf("day %d (cut %d): %d events left, want %d", d, cut, left[d], want)
		}
		if s := dayState(t, st, id, d); s.Raw != (d >= cut) || !s.Frozen || s.Dirty != AggClean {
			t.Errorf("day %d: %+v", d, s)
		}
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM agg_hour`); n != 21 {
		t.Errorf("hours %d, want every day's", n)
	}
	// A second round finds nothing more.
	if n := purge(at.Add(time.Hour)); n != 0 {
		t.Errorf("second round purged %d", n)
	}
	// A recorded start that is no unix ms stops the purge with an error, deleting nothing.
	if _, err := st.DB.Exec(`UPDATE agg_meta SET value = 'soon'`); err != nil {
		t.Fatal(err)
	}
	if n, _, err := st.PurgeRaw(t.Context(), o, at.Add(48*time.Hour)); err == nil || n != 0 {
		t.Errorf("unreadable start: %d, %v", n, err)
	}
}

// An upgraded database as 0.1 and age retention leave it: its oldest raw day holds the months cutoff, below the
// job's floor and never built. Past the grace the purge deletes it with everything before the floor, unbuilt (age
// retention's within a day anyway, outside every range), then the covered days down to the raw days: the oldest
// raw left is the raw days' first, no stray old day before it.
func TestPurgeRaw_CutoffDay(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	now := time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC)
	floor := AgeFloor(now, 1)
	purgeDays(t, st, id, floor, now.UnixMilli()/dayMS, 2)
	// The cutoff's day: its raw from the cutoff on (as 0.1 pruned it), unbuilt.
	if err := st.InsertEvents(t.Context(), []Event{{SiteID: id, TS: now.AddDate(0, -1, 0).UnixMilli() + 1000,
		Source: "server", Kind: "request", IP: "192.0.2.1", IPKey: "192.0.2.1"}}); err != nil {
		t.Fatal(err)
	}
	o := RetentionOptions{Months: 1, RawDays: 10, Builder: 1}
	// Within the grace it stays.
	if _, err := st.ArmRawPurge(t.Context(), now.Add(-PurgeGrace+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, _, err := st.PurgeRaw(t.Context(), o, now); err != nil || n != 0 || rawDays(t, st, id)[floor-1] != 1 {
		t.Fatalf("within the grace: purged %d, %v", n, err)
	}
	if _, err := st.DB.Exec(`UPDATE agg_meta SET value = ?`, strconv.FormatInt(now.Add(-PurgeGrace).UnixMilli(),
		10)); err != nil {
		t.Fatal(err)
	}
	if _, waits, err := st.PurgeRaw(t.Context(), o, now); err != nil || len(waits) != 0 {
		t.Fatalf("waits %v, %v", waits, err)
	}
	var oldest int64
	if err := st.DB.QueryRow(`SELECT MIN(ts) FROM event`).Scan(&oldest); err != nil ||
		oldest/dayMS != RawCutDay(now, 10) {
		t.Errorf("oldest raw on day %d, want %d (%v)", oldest/dayMS, RawCutDay(now, 10), err)
	}
	if s := dayState(t, st, id, floor-1); s.Raw || s.Frozen {
		t.Errorf("cutoff day %+v: marked without raw, never built", s)
	}
}

// A purge that cannot read its first start fails its round, and the size cap still runs.
func TestRunRetention_PurgeErrorKeepsCap(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	today := time.Now().UnixMilli() / dayMS
	var evs []Event
	for i := range 6000 {
		evs = append(evs, Event{SiteID: id, TS: (today-10)*dayMS + int64(i), Source: "browser", Kind: "page_view",
			IP: "192.0.2.1", IPKey: "192.0.2.1", Props: `{"pad":"` + strings.Repeat("x", 200) + `"}`})
	}
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`INSERT INTO agg_meta (key, value) VALUES ('raw_purge_armed_at', 'soon')`); err != nil {
		t.Fatal(err)
	}
	used, _ := st.UsedBytes(t.Context())
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
		st.RunRetention(ctx, coverOpts(st, RetentionOptions{Months: 25, RawDays: 3, MaxBytes: used / 2,
			CapEvery: 20 * time.Millisecond, Log: log}))
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if u, _ := st.UsedBytes(t.Context()); u <= used/2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cap not enforced after a purge error")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(logs.String(), "no unix ms") {
		t.Errorf("the purge's error not logged: %s", logs.String())
	}
}

// A first start that cannot be recorded (here: its table gone) is retried every round, warned once: the purge does
// not stay off until a restart.
func TestRunRetention_ArmRetry(t *testing.T) {
	st := open(t)
	if _, err := st.DB.Exec(`ALTER TABLE agg_meta RENAME TO agg_meta_away`); err != nil {
		t.Fatal(err)
	}
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
		st.RunRetention(ctx, RetentionOptions{Months: 25, RawDays: 30, CapEvery: 10 * time.Millisecond, Log: log})
		close(done)
	}()
	defer func() { cancel(); <-done }()
	time.Sleep(100 * time.Millisecond)
	if _, err := st.DB.Exec(`ALTER TABLE agg_meta_away RENAME TO agg_meta`); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok, err := st.RawPurgeArmed(t.Context()); err == nil && ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first start never recorded after the failure")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if n := strings.Count(logs.String(), "Raw purge not armed"); n != 1 {
		t.Errorf("warned %d times:\n%s", n, logs.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// A fresh install: the first start is recorded, nothing is older than the raw days, nothing is deleted or waited
// for, past the grace too; RawDays 0 purges nothing at all.
func TestPurgeRaw_Fresh(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	now := time.Now()
	today := now.UnixMilli() / dayMS
	if n, waits, err := st.PurgeRaw(t.Context(), RetentionOptions{RawDays: 90, Builder: 1}, now); n != 0 || waits != nil ||
		err != nil {
		t.Errorf("empty database: %d %v %v", n, waits, err)
	}
	purgeDays(t, st, id, today-5, today, 2)
	if _, err := st.ArmRawPurge(t.Context(), now.Add(-PurgeGrace)); err != nil {
		t.Fatal(err)
	}
	for _, o := range []RetentionOptions{{RawDays: 90, Months: 25, Builder: 1}, {Months: 25, Builder: 1}} {
		if n, waits, err := st.PurgeRaw(t.Context(), o, now); n != 0 || len(waits) != 0 || err != nil {
			t.Errorf("%+v: purged %d, waits %v, %v", o, n, waits, err)
		}
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM event`); n != 12 {
		t.Errorf("%d events left", n)
	}
}

// Only covered days go, oldest first: a day not built clean by the current builder, a dirty one, or the day before
// one whose figures are not built against its raw yet stops the site's purge there and is returned; once the job has
// built it, the next round goes on. A day whose raw is cut and whose figures are no clean build stops it for good.
func TestPurgeRaw_Coverage(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	now := time.Now()
	today := now.UnixMilli() / dayMS
	purgeDays(t, st, id, today-20, today, 2)
	if _, err := st.ArmRawPurge(t.Context(), now.Add(-PurgeGrace)); err != nil {
		t.Fatal(err)
	}
	o := RetentionOptions{RawDays: 5, Months: 25, Builder: 1}
	// round purges and checks where it stopped (0: nowhere, the cut): every day before it gone, every day from it on
	// kept.
	round := func(stop int64, cut bool) {
		t.Helper()
		_, waits, err := st.PurgeRaw(t.Context(), o, now)
		if err != nil {
			t.Fatal(err)
		}
		want, end := []PurgeWait{{Site: id, Day: stop, Cut: cut}}, stop
		if stop == 0 {
			want, end = nil, RawCutDay(now, 5)
		}
		if fmt.Sprint(waits) != fmt.Sprint(want) {
			t.Errorf("waits %v, want %v", waits, want)
		}
		left := rawDays(t, st, id)
		for d := today - 20; d <= today; d++ {
			if (left[d] == 0) != (d < end) {
				t.Errorf("stop %d: day %d has %d events", stop-today, d-today, left[d])
			}
		}
	}
	// A raw write makes today-16 dirty: today-17 (its figures' next day not clean) is where it stops.
	if err := st.InsertEvents(t.Context(), []Event{{SiteID: id, TS: (today-16)*dayMS + 50_000, Source: "server",
		Kind: "request", IP: "192.0.2.9", IPKey: "192.0.2.9"}}); err != nil {
		t.Fatal(err)
	}
	round(today-17, false)
	round(today-17, false) // waiting changes nothing
	// The job rebuilds it with the write: the purge goes on to the next gap, an unbuilt day.
	write(t, st, AggBuild{Site: id, Day: today - 16, Builder: 1, Events: 3})
	if _, err := st.DB.Exec(`DELETE FROM agg_day WHERE site_id = ? AND day = ?`, id, today-13); err != nil {
		t.Fatal(err)
	}
	round(today-14, false)
	// Built by an older builder: the job rebuilds it first.
	write(t, st, AggBuild{Site: id, Day: today - 13, Builder: 0, Events: 2})
	round(today-13, false)
	write(t, st, AggBuild{Site: id, Day: today - 13, Builder: 1, Events: 2})
	// Cut already (raw = 0) and not clean: nothing builds it, the purge says so and stays there.
	if _, err := st.DB.Exec(`UPDATE agg_day SET raw = 0, frozen = 0, dirty = ? WHERE site_id = ? AND day = ?`,
		AggWriting, id, today-11); err != nil {
		t.Fatal(err)
	}
	round(today-11, true)
	// Cleared by hand, the rest goes: up to the cut, the newer days kept.
	if _, err := st.DB.Exec(`UPDATE agg_day SET frozen = 1, dirty = 0, builder = 1 WHERE site_id = ? AND day = ?`, id,
		today-11); err != nil {
		t.Fatal(err)
	}
	round(0, false)
}

// A purge stopped after one chunk of a day (a shutdown), then a release with a newer builder, which rebuilds every
// day with raw but not the cut one (nothing rebuilds a day without its raw): the cut day's rows are complete, an
// older builder's, and the purge takes the rest of it and goes on to the raw days.
func TestPurgeRaw_PartCutThenBump(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	now := time.Now()
	today := now.UnixMilli() / dayMS
	d := today - 8
	var evs []Event
	for i := range 3 * retentionChunk {
		evs = append(evs, Event{SiteID: id, TS: d*dayMS + int64(i), Source: "browser", Kind: "page_view",
			IP: "192.0.2.1", IPKey: "192.0.2.1"})
	}
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	purgeDays(t, st, id, d, today, 2)
	if _, err := st.ArmRawPurge(t.Context(), now.Add(-PurgeGrace)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	afterPurgeChunk = func(_, _ int64) { cancel() }
	t.Cleanup(func() { afterPurgeChunk = nil })
	if _, _, err := st.PurgeRaw(ctx, RetentionOptions{RawDays: 3, Builder: 1}, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("purge not cut short: %v", err)
	}
	afterPurgeChunk = nil
	if s, left := dayState(t, st, id, d), rawDays(t, st, id)[d]; s.Raw || left == 0 || left == 3*retentionChunk+2 {
		t.Fatalf("after the shutdown: %+v, %d events left", s, left)
	}
	// The new release: builder 2 rebuilds every day with raw.
	for x := d + 1; x <= today; x++ {
		write(t, st, AggBuild{Site: id, Day: x, Builder: 2, Events: 2})
	}
	if _, waits, err := st.PurgeRaw(t.Context(), RetentionOptions{RawDays: 3, Builder: 2}, now); err != nil ||
		len(waits) != 0 {
		t.Fatalf("waits %v, %v", waits, err)
	}
	left := rawDays(t, st, id)
	for x := d; x <= today; x++ {
		if want := x >= RawCutDay(now, 3); (left[x] != 0) != want {
			t.Errorf("day %d (cut %d): %d events left", x-today, RawCutDay(now, 3)-today, left[x])
		}
	}
	if s := dayState(t, st, id, d); s.Raw || !s.Frozen || s.Dirty != AggClean || s.Builder != 1 {
		t.Errorf("cut day after the purge: %+v", s)
	}
}

// A site whose oldest raw day Cover cannot build (from 00:00 to 02:00 UTC the next day is not due) ends only its own
// raw step: the other site's raw before the tail goes, the aggregate steps run, and neither touches the stalled
// site's days from the stalled one on. The stall is warned once across rounds.
func TestEnforceCap_StallOneSite(t *testing.T) {
	st := open(t)
	sites, err := st.SyncSites(t.Context(), []Site{{Domain: "a.example.com", Kind: "site", PathRules: "[]"},
		{Domain: "b.example.com", Kind: "site", PathRules: "[]"}})
	if err != nil {
		t.Fatal(err)
	}
	a, b := sites["a.example.com"].ID, sites["b.example.com"].ID
	now := time.Now()
	today := now.UnixMilli() / dayMS
	purgeDays(t, st, a, today-8, today, 2)
	purgeDays(t, st, b, today-8, today, 60)
	// The day after site a's stuck day is not built: the stuck day waits for it (its carried sessions).
	stuck := today - 6
	if _, err := st.DB.Exec(`UPDATE agg_day SET dirty = ? WHERE site_id = ? AND day = ?`, AggRawChanged, a,
		stuck+1); err != nil {
		t.Fatal(err)
	}
	addrs := func(site, from int64) int {
		return countRows(t, st, `SELECT COUNT(*) FROM agg_addr WHERE site_id = ? AND day >= ?`, site, from)
	}
	keptA := addrs(a, stuck)
	var logs bytes.Buffer
	o := coverOpts(st, RetentionOptions{Months: 25, Builder: 1, Log: slog.New(slog.NewTextHandler(&logs, nil)),
		capStalls: map[int64]int64{}})
	cover := o.Cover
	o.Cover = func(ctx context.Context, site, day int64) error {
		if site == a {
			return nil // not due
		}
		return cover(ctx, site, day)
	}
	used, _ := st.UsedBytes(t.Context())
	o.MaxBytes = used / 3
	for range 2 {
		if _, err := st.EnforceCap(t.Context(), o, now); err != nil {
			t.Fatal(err)
		}
	}
	left := map[int64]map[int64]int{a: rawDays(t, st, a), b: rawDays(t, st, b)}
	for d := today - 8; d <= today; d++ {
		if wantA := d >= stuck; (left[a][d] != 0) != wantA {
			t.Errorf("site a, day %d: %d events", d-today, left[a][d])
		}
		if wantB := d > today-CapTail; (left[b][d] != 0) != wantB {
			t.Errorf("site b, day %d: %d events", d-today, left[b][d])
		}
	}
	if addrs(a, stuck) != keptA || addrs(b, 0) == 9*2 {
		t.Errorf("per-address rows: site a from the stalled day %d (want %d), site b %d", addrs(a, stuck), keptA,
			addrs(b, 0))
	}
	if n := strings.Count(logs.String(), "its raw from there stays"); n != 1 {
		t.Errorf("stall warned %d times:\n%s", n, logs.String())
	}
}

// The cap's per-address cut of a day is all or nothing, past a chunk's rows too: a shutdown as it deletes leaves the
// day's rows whole and unmarked or none and marked cut, never a part the visitors table would read as the day's.
func TestEnforceCap_AddrsCutWhole(t *testing.T) {
	const n = retentionChunk + 500
	for _, shutdown := range []bool{true, false} {
		st := open(t)
		id := seed(t, st)
		now := time.Now()
		d := now.UnixMilli()/dayMS - 10
		write(t, st, AggBuild{Site: id, Day: d, Builder: 1, Addrs: slices.Values(aggAddrs(id, d, n))})
		used, _ := st.UsedBytes(t.Context())
		ctx, cancel := context.WithCancel(t.Context())
		var once sync.Once
		onStmt(t, st, func(q string) {
			if shutdown && strings.HasPrefix(q, "DELETE FROM agg_addr") {
				once.Do(func() { time.AfterFunc(5*time.Millisecond, cancel) }) // as the first rows go
			}
		}, func() {
			_, _ = st.EnforceCap(ctx, RetentionOptions{Months: 25, MaxBytes: used / 2, Builder: 1,
				Log: slog.New(slog.DiscardHandler)}, now)
		})
		cancel()
		rows, s := countRows(t, st, `SELECT COUNT(*) FROM agg_addr WHERE day = ?`, d), dayState(t, st, id, d)
		whole, none := rows == n && !s.AddrsCut, rows == 0 && s.AddrsCut
		if !whole && !none || !shutdown && rows != 0 {
			t.Errorf("shutdown %v: %d of %d rows left, day %+v", shutdown, rows, n, s)
		}
	}
}

// A raw write between two chunks of a day being purged: into that day (its raw cut already) it changes nothing the
// figures read and goes with the day, never counted (a loss this test pins); into the next day it makes that day
// dirty, and the purge stops there instead of taking the raw its rebuild needs. The purged day ends clean with its
// figures, never rebuilt from half its raw.
//
// The product cannot write there: the collector dates an event at most an hour back (the feed) or half an hour
// (the browser's clock), so it writes today and yesterday only (ingest's TestBackdate_WithinRawTail); the classifier
// restamps the days of the events written since its last pass, those same days; and the purge never reaches the
// last CapTail days, whatever the raw days (the shortest allowed is far more).
func TestPurgeRaw_RacingWrite(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	now := time.Now()
	today := now.UnixMilli() / dayMS
	// The bounds together: the shortest raw retention is past the tail, and a purge keeping a single day still
	// leaves the tail's days, built or not.
	if app.MinRawRetentionDays <= CapTail {
		t.Fatalf("shortest raw retention %d days, the tail %d", app.MinRawRetentionDays, CapTail)
	}
	purgeDays(t, st, id, today-CapTail+1, today, 2)
	if _, err := st.ArmRawPurge(t.Context(), now.Add(-PurgeGrace)); err != nil {
		t.Fatal(err)
	}
	if n, _, err := st.PurgeRaw(t.Context(), RetentionOptions{RawDays: 1, Builder: 1}, now); err != nil || n != 0 {
		t.Fatalf("a purge keeping one day cut %d events of the tail, %v", n, err)
	}
	d := today - 10
	purgeDays(t, st, id, d, d+1, 3*retentionChunk)
	if _, err := st.ArmRawPurge(t.Context(), now.Add(-PurgeGrace)); err != nil {
		t.Fatal(err)
	}
	before := dayState(t, st, id, d)
	chunks := 0
	afterPurgeChunk = func(_, _ int64) {
		if chunks++; chunks != 1 {
			return
		}
		if err := st.InsertEvents(context.Background(), []Event{
			{SiteID: id, TS: d*dayMS + 7, Source: "server", Kind: "request", IP: "192.0.2.7", IPKey: "192.0.2.7"},
			{SiteID: id, TS: (d+1)*dayMS + 7, Source: "server", Kind: "request", IP: "192.0.2.7", IPKey: "192.0.2.7"},
		}); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterPurgeChunk = nil })
	_, waits, err := st.PurgeRaw(t.Context(), RetentionOptions{RawDays: 3, Builder: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(waits) != fmt.Sprint([]PurgeWait{{Site: id, Day: d + 1}}) {
		t.Errorf("waits %v, want the next day", waits)
	}
	left := rawDays(t, st, id)
	if left[d] != 0 || left[d+1] != 3*retentionChunk+1 {
		t.Errorf("left %v", left)
	}
	after := dayState(t, st, id, d)
	if after.Raw || !after.Frozen || after.Dirty != AggClean || after.Events != before.Events ||
		after.Version <= before.Version {
		t.Errorf("purged day %+v, before %+v", after, before)
	}
	if n := countRows(t, st, `SELECT COALESCE(SUM(events), 0) FROM agg_hour WHERE hour >= ? AND hour < ?`, d*24,
		d*24+24); n != 3*retentionChunk {
		t.Errorf("purged day's hours count %d events", n)
	}
	if s := dayState(t, st, id, d+1); !s.Raw || s.Dirty != AggRawChanged {
		t.Errorf("next day %+v", s)
	}
}

// Age retention after the purge: the months still take the per-address rows, actor classes and model usage; the
// tiles' hours and lists, agg_day and the collector counters stay, however old.
func TestPruneAge_KeepsFigures(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	now := time.Now()
	today := now.UnixMilli() / dayMS
	old, young := today-800, today-30
	for _, d := range []int64{old, young} {
		write(t, st, AggBuild{Site: id, Day: d, Builder: 1, Dims: []string{"pages"},
			Hours: []AggHour{{Hour: d * 24, Cls: 1}}, Lists: []AggList{{Dim: "pages", Part: 1, Key: "/"}},
			Addrs: slices.Values(aggAddrs(id, d, 3))})
		if err := st.SetActorClass(t.Context(), ActorClass{SiteID: id, IPKey: "192.0.2.1", Day: d, Class: "bot"},
			nil); err != nil {
			t.Fatal(err)
		}
		if err := st.AddUsage(t.Context(), Usage{SiteID: id, Day: d, Calls: 1}); err != nil {
			t.Fatal(err)
		}
		if err := st.AddStats(t.Context(), []IngestStat{{SiteID: id, Hour: d * 24, Accepted: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Prune(t.Context(), RetentionOptions{Months: 25, RawDays: 90, Builder: 1}, now); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM agg_addr WHERE day = ?`:          0,
		`SELECT COUNT(*) FROM actor_class WHERE day = ?`:       0,
		`SELECT COUNT(*) FROM llm_usage WHERE day = ?`:         0,
		`SELECT COUNT(*) FROM agg_hour WHERE hour = ? * 24`:    1,
		`SELECT COUNT(*) FROM agg_list WHERE day = ?`:          1,
		`SELECT COUNT(*) FROM agg_day WHERE day = ?`:           1,
		`SELECT COUNT(*) FROM ingest_stat WHERE hour = ? * 24`: 1,
	} {
		if n := countRows(t, st, q, old); n != want {
			t.Errorf("%s: %d, want %d", q, n, want)
		}
	}
	for _, q := range []string{`SELECT COUNT(*) FROM agg_addr WHERE day = ?`, `SELECT COUNT(*) FROM actor_class WHERE day = ?`,
		`SELECT COUNT(*) FROM llm_usage WHERE day = ?`} {
		if n := countRows(t, st, q, young); n == 0 {
			t.Errorf("%s: the young day's rows went", q)
		}
	}
}

// The cap deletes raw only from a day whose figures stand without it: each day it reaches is built first through
// Cover (and the day after it), while its raw is whole, then deleted; never a day of the last three. Without Cover
// it deletes no unbuilt raw and no aggregate, and says why. A day before the age floor (never built) goes as age
// retention would take it.
func TestEnforceCap_Cover(t *testing.T) {
	st := open(t)
	id := seed(t, st)
	now := time.Now()
	today := now.UnixMilli() / dayMS
	var evs []Event
	for d := today - 8; d <= today; d++ {
		for i := range 1000 {
			evs = append(evs, Event{SiteID: id, TS: d*dayMS + int64(i)*1000, Source: "browser", Kind: "page_view",
				IP: "192.0.2.1", IPKey: "192.0.2.1", Props: `{"pad":"` + strings.Repeat("x", 300) + `"}`})
		}
	}
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	used, _ := st.UsedBytes(t.Context())
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	// Without Cover: nothing goes.
	if n, err := st.EnforceCap(t.Context(), RetentionOptions{Months: 25, MaxBytes: used / 4, Log: log}, now); err != nil ||
		n != 0 {
		t.Fatalf("cap without Cover deleted %d, %v", n, err)
	}
	if !strings.Contains(logs.String(), "not built") {
		t.Errorf("warnings: %s", logs.String())
	}
	o := coverOpts(st, RetentionOptions{Months: 25, MaxBytes: used / 4, Builder: 1, Log: log})
	build := o.Cover
	var order []string
	o.Cover = func(ctx context.Context, site, day int64) error {
		order = append(order, fmt.Sprintf("cover %d (%d events)", day-today, rawDays(t, st, id)[day]))
		return build(ctx, site, day)
	}
	n, err := st.EnforceCap(t.Context(), o, now)
	if err != nil || n == 0 {
		t.Fatalf("cap deleted %d, %v", n, err)
	}
	left := rawDays(t, st, id)
	for d := today - 2; d <= today; d++ {
		if left[d] != 1000 {
			t.Errorf("tail day %d: %d events", d-today, left[d])
		}
	}
	// Every day reached was covered while whole, the day after it too, before any of its raw went.
	var gone []string
	for d := today - 8; d < today-2; d++ {
		if left[d] < 1000 {
			gone = append(gone, fmt.Sprint(d-today))
			for _, w := range []string{fmt.Sprintf("cover %d (1000 events)", d-today),
				fmt.Sprintf("cover %d (1000 events)", d+1-today)} {
				if !slices.Contains(order, w) {
					t.Errorf("day %d cut without %q: %v", d-today, w, order)
				}
			}
		}
	}
	if len(gone) == 0 || order[0] != "cover -8 (1000 events)" {
		t.Errorf("days cut %v, covers %v", gone, order)
	}
	// A day before the age floor goes without a build.
	floor := AgeFloor(now, 1)
	if err := st.InsertEvents(t.Context(), []Event{{SiteID: id, TS: (floor-1)*dayMS + 5, Source: "server",
		Kind: "request", IP: "192.0.2.1", IPKey: "192.0.2.1"}}); err != nil {
		t.Fatal(err)
	}
	order = nil
	used, _ = st.UsedBytes(t.Context())
	o.Months, o.MaxBytes = 1, used-1
	if _, err := st.EnforceCap(t.Context(), o, now); err != nil {
		t.Fatal(err)
	}
	if left := rawDays(t, st, id); left[floor-1] != 0 || slices.Contains(order, fmt.Sprintf("cover %d (1 events)",
		floor-1-today)) {
		t.Errorf("pre-floor day: %d events left, covers %v", left[floor-1], order)
	}
}
