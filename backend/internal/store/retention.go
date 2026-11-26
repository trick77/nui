package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/trick77/ship/kit/logging"
	"github.com/trick77/ship/kit/run"

	"github.com/trick77/trakk/backend/internal/alert"
)

const (
	// retentionChunk is the rows one delete statement removes: a short write, so the batch writer never waits long
	// behind it (about 2,000 rows with their two index entries take milliseconds).
	retentionChunk = 2000
	// vacuumStep is the free pages one incremental_vacuum hands back: 1 MiB at 4 KiB pages, another short write.
	vacuumStep = 256
	// retentionYield is the pause between two deletes or vacuum steps: the writer's turn at the lock.
	retentionYield = 20 * time.Millisecond
)

// UsedBytes is the database's size without its free pages: what the rows take. The file itself only shrinks when
// incremental_vacuum hands free pages back, so the file size is the wrong gauge for a cap.
func (s *Store) UsedBytes(ctx context.Context) (int64, error) {
	var pages, free, size int64
	if err := s.DB.QueryRowContext(ctx, `SELECT page_count, freelist_count, page_size
		FROM pragma_page_count(), pragma_freelist_count(), pragma_page_size()`).Scan(&pages, &free, &size); err != nil {
		return 0, err
	}
	return (pages - free) * size, nil
}

// Prune runs one round of RunRetention at now: age, the raw purge, the cap, then hands the freed pages back. It
// returns the events and aggregate rows deleted. Every delete is its own short statement.
func (s *Store) Prune(ctx context.Context, o RetentionOptions, now time.Time) (int64, error) {
	total, err := s.PruneAge(ctx, now.AddDate(0, -o.Months, 0))
	if err != nil {
		return total, err
	}
	n, _, err := s.PurgeRaw(ctx, o, now)
	total += n
	if err != nil {
		return total, err
	}
	n, err = s.EnforceCap(ctx, o, now)
	total += n
	if err != nil {
		return total, err
	}
	return total, s.Vacuum(ctx)
}

// delAggAddrBefore deletes a chunk of a site's per-address rows older than a day (?1 site, ?2 day, ?3 chunk).
const delAggAddrBefore = `DELETE FROM agg_addr WHERE site_id = ?1 AND (day, ip_key, part) IN
	(SELECT day, ip_key, part FROM agg_addr WHERE site_id = ?1 AND day < ?2 LIMIT ?3)`

// AgeFloor is the oldest UTC day the aggregate job builds at now: the day after the one holding the months cutoff,
// 0 without one. The cutoff's own day may hold part of its raw only (0.1 pruned to the millisecond) and age retention
// deletes it within a day.
func AgeFloor(now time.Time, months int) int64 {
	if months <= 0 {
		return 0
	}
	return now.AddDate(0, -months, 0).UnixMilli()/dayMS + 1
}

// The watch's script hosts not seen since before a time, a chunk of a site's (?1 site, ?2 unix ms, ?3 chunk).
const delHostsBefore = `DELETE FROM script_host WHERE site_id = ?1 AND host IN
	(SELECT host FROM script_host WHERE site_id = ?1 AND last_seen < ?2 LIMIT ?3)`

// delClassesBefore deletes a chunk of a site's actor classes older than a day (?1 site, ?2 day, ?3 chunk):
// actor_class_day finds them, a day holds hundreds of thousands of address keys on a busy site.
const delClassesBefore = `DELETE FROM actor_class WHERE site_id = ?1 AND (ip_key, day) IN
	(SELECT ip_key, day FROM actor_class WHERE site_id = ?1 AND day < ?2 LIMIT ?3)`

// delUsageBefore deletes a chunk of a site's model usage older than a day (?1 site, ?2 day, ?3 chunk).
const delUsageBefore = `DELETE FROM llm_usage WHERE site_id = ?1 AND day IN
	(SELECT day FROM llm_usage WHERE site_id = ?1 AND day < ?2 LIMIT ?3)`

// PruneAge deletes what is older than before (BACKEND_RETENTION_MONTHS): events, and the per-address aggregates with
// the actor classes and the model's usage (they name addresses as the events do), and the watch state idle since.
// The other aggregates and the collector counters stay; the size cap never cuts the watch state (it is bounded per
// site, and a host it forgot would come back as new). Events go by whole UTC days (the cutoff floored to midnight):
// an aggregate day is built from all of its raw or none of it. It returns the events and rows deleted.
func (s *Store) PruneAge(ctx context.Context, before time.Time) (int64, error) {
	cut := before.UnixMilli() / dayMS * dayMS
	var total int64
	// Per site, inactive ones too (sites are never deleted): event_site_ts finds the old rows, the other tables'
	// site-led keys theirs; ts or day alone has no index and would scan the table in one long write.
	sites, err := s.Sites(ctx)
	if err != nil {
		return 0, err
	}
	for _, st := range sites {
		n, err := s.deleteChunks(ctx, `site_id = ? AND ts < ?`, st.ID, cut)
		total += n
		if err != nil {
			return total, err
		}
		for _, del := range []struct {
			q   string
			cut int64
		}{{delAggAddrBefore, cut / dayMS}, {delClassesBefore, cut / dayMS}, {delUsageBefore, cut / dayMS},
			{delHostsBefore, cut}} {
			for {
				n, err := s.execRows(ctx, del.q, st.ID, del.cut, retentionChunk)
				total += n
				if err != nil {
					return total, err
				}
				if n < retentionChunk {
					break
				}
				if !run.Sleep(ctx, retentionYield) {
					return total, ctx.Err()
				}
			}
		}
	}
	// Findings only: a host row's first_seen is the host's merged first sight, a long-known host's open-day row stays
	// until the pass replaces it.
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM watch_open WHERE kind <> 'host' AND first_seen < ?`,
		cut); err != nil {
		return total, err
	}
	return total, nil
}

// PurgeGrace is how long after its first start (ArmRawPurge) a release that purges raw deletes nothing: time to
// compare the figures with what they showed before, raw still whole.
const PurgeGrace = 7 * 24 * time.Hour

// metaArmed is agg_meta's key for the purge's first start, unix ms.
const metaArmed = "raw_purge_armed_at"

// ArmRawPurge records now as the purge's first start unless a start is recorded already, and returns the one
// recorded. The grace runs from it, across restarts.
func (s *Store) ArmRawPurge(ctx context.Context, now time.Time) (time.Time, error) {
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO agg_meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO NOTHING`,
		metaArmed, strconv.FormatInt(now.UnixMilli(), 10)); err != nil {
		return time.Time{}, err
	}
	at, _, err := s.RawPurgeArmed(ctx)
	return at, err
}

// RawPurgeArmed is the purge's first start, ok false before one is recorded. A value that is no unix ms is an error:
// the purge then deletes nothing.
func (s *Store) RawPurgeArmed(ctx context.Context) (time.Time, bool, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM agg_meta WHERE key = ?`, metaArmed).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("agg_meta %s: %q is no unix ms", metaArmed, v)
	}
	return time.UnixMilli(ms).UTC(), true, nil
}

// RawCutDay is the oldest UTC day raw kept for days holds at now: the purge deletes the days before it.
func RawCutDay(now time.Time, days int) int64 {
	return (now.UnixMilli() - int64(days)*dayMS) / dayMS
}

// coverage is whether a site-day's raw may go: what the dashboard reads for the day must not need it.
type coverage int

const (
	covered coverage = iota
	// notBuilt: not built clean by this builder or a newer one, or the next day not yet built against its raw (a
	// first build of a day whose day before has no raw counts no session carried over midnight). The job's turn.
	notBuilt
	// cutNotClean: raw cut already and the day's rows missing or partial: nothing will build it.
	cutNotClean
)

// dayCoverage reads, in the deleting transaction, whether a site-day's raw may go. A day with its raw whole: built
// (AggDay.BuiltBy), and the next day frozen and clean too (its carried sessions counted against this raw; a rebuild
// of it then keeps them). A day whose raw is cut already (a cap or a purge stopped part-way): its rows complete,
// frozen and clean by any builder (nothing rebuilds a day without its raw, so a builder bump never reaches it), and
// what is left of its raw goes as detail: the dashboard never reads a cut day's raw for a figure. Complete rows
// still marked dirty wait for the job to clear the mark; missing or partial rows block for good. anyCut (the cap):
// a cut day passes whatever its rows, for the same reason.
func dayCoverage(ctx context.Context, tx *sql.Tx, site, day, builder int64, anyCut bool) (coverage, error) {
	days, err := AggDaysIn(ctx, tx, site, day, day+1)
	if err != nil {
		return notBuilt, err
	}
	d, ok := days[day]
	next, nextOK := days[day+1]
	switch {
	case !ok:
		return notBuilt, nil
	case !d.Raw && (anyCut || d.Frozen && d.Dirty == AggClean):
		return covered, nil
	case !d.Raw && d.Frozen && (d.Dirty == AggRawChanged || d.Dirty == AggRequeued):
		return notBuilt, nil
	case !d.Raw:
		return cutNotClean, nil
	case d.BuiltBy(builder) && nextOK && (!next.Raw || next.Frozen && next.Dirty == AggClean):
		return covered, nil
	}
	return notBuilt, nil
}

// dayEvents is a site-day's events oldest first (?: site, the day's first ms, the next day's): the time index walks
// them in order, a chunk at a time.
const dayEvents = `site_id = ? AND ts >= ? AND ts < ? ORDER BY ts`

// purgeChunk deletes a chunk of a site-day's events if dayCoverage passes, in one transaction with the check: a raw
// write or a build's first write committed before it fails the check, one after it finds the day without raw.
func (s *Store) purgeChunk(ctx context.Context, site, day, builder int64, anyCut bool) (int64, coverage, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, notBuilt, err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := dayCoverage(ctx, tx, site, day, builder, anyCut)
	if err != nil || c != covered {
		return 0, c, err
	}
	n, err := deleteEventsTx(ctx, tx, dayEvents, site, day*dayMS, (day+1)*dayMS)
	if err != nil {
		return 0, c, err
	}
	return n, c, tx.Commit()
}

// PurgeWait is a site whose purge stopped at a day it may not delete yet.
type PurgeWait struct {
	Site, Day int64
	// Cut: the day's raw is cut already and its aggregate rows are missing or partial; waiting does not help.
	Cut bool
}

// purgeProgress spaces the progress lines of a long purge.
const purgeProgress = 30 * time.Second

// afterPurgeChunk, when set (tests), runs after each chunk the purge committed: a write between two chunks.
var afterPurgeChunk func(site, day int64)

// PurgeRaw deletes the raw events older than o.RawDays (0: none) once the grace after ArmRawPurge's first start has
// passed: per site, whole UTC days, oldest first, each only while dayCoverage passes, in chunks with yields. Raw
// before AgeFloor (the day holding the months cutoff and older: age retention's, outside every range, never built)
// goes first and whatever its coverage: left, an upgraded database's cutoff day would hold the oldest raw for up to
// two days and hide where the purged raw ends. From the floor on it stops a site at the first day it may not delete,
// which it returns: the job builds it, a later round goes on. It returns the events deleted.
func (s *Store) PurgeRaw(ctx context.Context, o RetentionOptions, now time.Time) (int64, []PurgeWait, error) {
	if o.RawDays <= 0 {
		return 0, nil, nil
	}
	armed, ok, err := s.RawPurgeArmed(ctx)
	if err != nil || !ok || now.Before(armed.Add(PurgeGrace)) {
		return 0, nil, err
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	cut := min(RawCutDay(now, o.RawDays), now.UnixMilli()/dayMS-(CapTail-1))
	floor := AgeFloor(now, o.Months)
	sites, err := s.Sites(ctx)
	if err != nil {
		return 0, nil, err
	}
	ids := make([]int64, 0, len(sites))
	for _, st := range sites {
		ids = append(ids, st.ID)
	}
	slices.Sort(ids)
	var total int64
	var waits []PurgeWait
	logged := time.Now()
	for _, site := range ids {
		if floor > 0 {
			n, err := s.deleteChunks(ctx, `site_id = ? AND ts < ?`, site, floor*dayMS)
			if total += n; err != nil {
				return total, waits, err
			}
		}
		for last := int64(-1); ; {
			var ts sql.NullInt64
			if err := s.DB.QueryRowContext(ctx, `SELECT MIN(ts) FROM event WHERE site_id = ? AND ts >= ?`, site,
				floor*dayMS).Scan(&ts); err != nil {
				return total, waits, err
			}
			day := ts.Int64 / dayMS
			// last: a day deleted to the end that still has rows (a write raced in) waits for the next round.
			if !ts.Valid || day >= cut || day == last {
				break
			}
			last = day
			for {
				n, c, err := s.purgeChunk(ctx, site, day, o.Builder, false)
				total += n
				if err != nil {
					return total, waits, err
				}
				if c != covered {
					waits = append(waits, PurgeWait{Site: site, Day: day, Cut: c == cutNotClean})
					break
				}
				if afterPurgeChunk != nil {
					afterPurgeChunk(site, day)
				}
				if time.Since(logged) >= purgeProgress {
					logged = time.Now()
					log.Info("Raw purge deleting", "deleted", total, "site", site,
						"day", time.UnixMilli(day*dayMS).UTC().Format(time.DateOnly))
				}
				if !run.Sleep(ctx, retentionYield) {
					return total, waits, ctx.Err()
				}
				if n < retentionChunk {
					break
				}
			}
			if len(waits) > 0 && waits[len(waits)-1].Site == site {
				break
			}
		}
	}
	return total, waits, nil
}

// execRows runs one statement and returns the rows it changed.
func (s *Store) execRows(ctx context.Context, q string, args ...any) (int64, error) {
	res, err := s.DB.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CapTail is the UTC days the cap and the purge never cut: today, yesterday and the day before, which the aggregate
// job has not built yet (a day is built after 02:00 two days on). Cutting them, as the cap once did when the
// aggregates filled it, deleted every raw event and left those days without figures for good. The only days raw is
// still written to (the collector's back-dating, the classifier's restamps) are among them.
const CapTail = 3

// capRun is one EnforceCap call's settings.
type capRun struct {
	o RetentionOptions
	// floor is AgeFloor: older raw is age retention's, deleted within a day and never built.
	floor int64
}

// capStep is one kind of row the cap deletes: oldest finds a site's oldest day before the tail that still has such
// rows (at orders it against the other sites', dim is a list's dimension), del deletes a chunk of them.
type capStep struct {
	what   string
	oldest func(ctx context.Context, s *Store, site, tail int64) (day, at int64, dim string, ok bool, err error)
	del    func(ctx context.Context, s *Store, c *capRun, site, day int64, dim string) (int64, error)
}

// capSteps are what the cap deletes, in order: raw before the aggregates (the figures outlive it, as age retention
// has it), then the per-address rows, the lists, the hours: the finest grain first.
var capSteps = []capStep{
	{what: "raw events", oldest: func(ctx context.Context, s *Store, site, tail int64) (int64, int64, string, bool, error) {
		var ts sql.NullInt64
		err := s.DB.QueryRowContext(ctx, `SELECT MIN(ts) FROM event WHERE site_id = ?`, site).Scan(&ts)
		return ts.Int64 / dayMS, ts.Int64, "", ts.Valid && ts.Int64 < tail*dayMS, err
	}, del: capRaw},
	// A day's per-address rows go together, the day's other figures stay readable: its address answers are empty,
	// as after age retention, never a few addresses read as the day's.
	{what: "per-address aggregates", oldest: minDay(`SELECT MIN(day) FROM agg_addr WHERE site_id = ?`, 1),
		del: func(ctx context.Context, s *Store, _ *capRun, site, day int64, _ string) (int64, error) {
			return s.cutAddrs(ctx, site, day)
		}},
	{what: "list aggregates", oldest: oldestList,
		del: func(ctx context.Context, s *Store, _ *capRun, site, day int64, dim string) (int64, error) {
			return s.capDelete(ctx, site, day, delAggList, site, dim, day, retentionChunk)
		}},
	{what: "hourly aggregates", oldest: minDay(`SELECT MIN(hour) FROM agg_hour WHERE site_id = ?`, 24),
		del: func(ctx context.Context, s *Store, _ *capRun, site, day int64, _ string) (int64, error) {
			return s.capDelete(ctx, site, day, delAggHour, site, day*24, day*24+24, retentionChunk)
		}},
}

// ErrNotDue is Cover's answer for a day the aggregate job does not build yet but will: the cap waits for it.
var ErrNotDue = errors.New("day not due for building yet")

// capStalled is the cap's raw step at a day it may not delete: not built, and Cover did not build it. wait: Cover
// answered ErrNotDue for it or the day after; else no Cover, or a write raced the build.
type capStalled struct {
	site, day int64
	wait      bool
}

func (e capStalled) Error() string {
	return fmt.Sprintf("site %d, %s not built", e.site, time.UnixMilli(e.day*dayMS).UTC().Format(time.DateOnly))
}

// capRaw deletes a chunk of a site-day's raw for the cap. A day before the age floor is age retention's, never built:
// it goes as it would within a day. A later day goes once dayCoverage passes (a cut day always); else Cover builds
// it and the day after, and it goes if it passes then. A day that still does not is capStalled: the site's raw step
// ends there for this round. From 00:00 to 02:00 UTC the day after the oldest one the cap may cut is the tail's
// first, not due yet (ErrNotDue): a wait that ends by itself, as the job or the next round's Cover builds it then.
func capRaw(ctx context.Context, s *Store, c *capRun, site, day int64, _ string) (int64, error) {
	if day < c.floor {
		return s.deleteEvents(ctx, dayEvents, site, day*dayMS, (day+1)*dayMS)
	}
	n, cv, err := s.purgeChunk(ctx, site, day, c.o.Builder, true)
	if err != nil || cv == covered {
		return n, err
	}
	wait := false
	if c.o.Cover != nil {
		for _, d := range []int64{day, day + 1} {
			if err := c.o.Cover(ctx, site, d); errors.Is(err, ErrNotDue) {
				wait = true
			} else if err != nil {
				return 0, err
			}
		}
		if n, cv, err = s.purgeChunk(ctx, site, day, c.o.Builder, true); err != nil || cv == covered {
			return n, err
		}
	}
	return 0, capStalled{site, day, wait}
}

// cutAddrs deletes a site-day's per-address rows, all of them in one transaction with the day's mark (addrs_cut):
// a reader sees the day's addresses whole or none, and a shutdown cannot leave part of them, which the visitors
// table and its visits' stitching would take for the day's whole set (a day without rows counts no address and no
// session). One day at a time: the largest day of a 23 M-event, 25-month test history held 501 rows; the chunks are
// the days.
func (s *Store) cutAddrs(ctx context.Context, site, day int64) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE agg_day SET addrs_cut = 1 WHERE site_id = ? AND day = ?`, site,
		day); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM agg_addr WHERE site_id = ? AND day = ?`, site, day)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// capDelete deletes a chunk of a day's list or hourly rows and, in the same transaction, marks the day unbuilt with
// partial rows (frozen 0, AggWriting): without raw (the cap deletes aggregates only of days before the site's oldest
// raw), the state of a day nothing can be read for, so every figure of it is missing alike, never a zero from the
// rows left. Nothing rebuilds or clears it; a build writing the day meanwhile leaves it so (finishAggDay).
func (s *Store) capDelete(ctx context.Context, site, day int64, q string, args ...any) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE agg_day SET frozen = 0, dirty = ? WHERE site_id = ? AND day = ?`,
		AggWriting, site, day); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// Per row, what the bytes of its text leave out but every row stores at least: an event's record header (23
// columns), its ts in the row and in both indexes (6 bytes each at today's unix ms) and the indexes' headers; an
// actor class's record header, its updated_at and its two indexes' headers.
const (
	eventFloor = 24 + 3*6 + 4 + 5
	classFloor = 7 + 6 + 5 + 4
)

// capFloor is a lower bound of the bytes the aggregate steps can never free: each site's raw from rawFrom (the tail,
// or the day its raw step stopped at) on and the actor classes, which no step touches. Text bytes plus the floors
// above: no page, cell or integer bytes beyond them, so the real size is larger and a cap below the bound is out of
// reach for certain; between the bound and the real size the steps still delete in vain. A raw day per statement,
// actor classes a month (dash's classDays).
func (s *Store) capFloor(ctx context.Context, sites map[string]Site, rawFrom map[int64]int64, today int64) (int64, error) {
	var total int64
	for _, st := range sites {
		for d := rawFrom[st.ID]; d <= today; d++ {
			var n, text int64
			if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(octet_length(source) + octet_length(kind)
				+ octet_length(name) + octet_length(visitor) + octet_length(sid) + octet_length(ip) + 2 * octet_length(ip_key)
				+ octet_length(ua) + octet_length(path) + octet_length(template) + octet_length(url) + octet_length(ref)
				+ octet_length(ref_host) + octet_length(method) + octet_length(country) + octet_length(account)
				+ octet_length(op) + octet_length(err) + octet_length(props)), 0) FROM event WHERE site_id = ? AND ts >= ? AND ts < ?`,
				st.ID, d*dayMS, (d+1)*dayMS).Scan(&n, &text); err != nil {
				return 0, err
			}
			total += text + n*eventFloor
		}
		var first sql.NullInt64
		if err := s.DB.QueryRowContext(ctx, `SELECT MIN(day) FROM actor_class WHERE site_id = ?`, st.ID).
			Scan(&first); err != nil {
			return 0, err
		}
		for lo := first.Int64; first.Valid && lo <= today; lo += 31 {
			var n, text int64
			if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(2 * octet_length(ip_key)
				+ octet_length(class) + octet_length(reasons)), 0) FROM actor_class WHERE site_id = ? AND day >= ? AND day < ?`,
				st.ID, lo, lo+31).Scan(&n, &text); err != nil {
				return 0, err
			}
			total += text + n*classFloor
		}
	}
	return total, nil
}

// minDay is an oldest finder from one MIN statement on a table's primary key (one index probe), its unit per day.
func minDay(q string, perDay int64) func(context.Context, *Store, int64, int64) (int64, int64, string, bool, error) {
	return func(ctx context.Context, s *Store, site, tail int64) (int64, int64, string, bool, error) {
		var v sql.NullInt64
		err := s.DB.QueryRowContext(ctx, q, site).Scan(&v)
		day := v.Int64 / perDay
		return day, day, "", v.Valid && day < tail, err
	}
}

// oldestList is the oldest day of a site's list rows: agg_list's key starts with (site_id, dim, day), so one MIN per
// dimension, the dimensions found by skipping along the key.
func oldestList(ctx context.Context, s *Store, site, tail int64) (int64, int64, string, bool, error) {
	var best int64
	var bestDim string
	found := false
	for dim := ""; ; {
		err := s.DB.QueryRowContext(ctx, `SELECT dim FROM agg_list WHERE site_id = ? AND dim > ? ORDER BY dim LIMIT 1`,
			site, dim).Scan(&dim)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return 0, 0, "", false, err
		}
		var day int64
		if err := s.DB.QueryRowContext(ctx, `SELECT MIN(day) FROM agg_list WHERE site_id = ? AND dim = ?`, site, dim).
			Scan(&day); err != nil {
			return 0, 0, "", false, err
		}
		if !found || day < best {
			best, bestDim, found = day, dim, true
		}
	}
	return best, best, bestDim, found && best < tail, nil
}

// EnforceCap deletes, while the rows take more than o.MaxBytes and until they take nine tenths of it, the capSteps
// in order, each oldest first over every site, in chunks, never a day of the last CapTail. 0 is no cap. Raw goes
// only from days whose figures stand without it (capRaw builds them first); an aggregate step starts only while
// deleting aggregates can still reach the target (capFloor): figures deleted in vain are lost for good. A site whose
// raw step stops at a day it may not delete (capStalled) keeps its raw from there on for this round, and no aggregate
// of its from that day on goes (the job builds those days); the other sites and the aggregate steps go on, unless a
// site's day only waits to fall due (capStalled.wait): then the round deletes no aggregate, since within two hours
// that day's raw can go instead (whether it is enough is not checked), and the database stays over the cap
// meanwhile. agg_day stays: it says which days were built, and
// which the cap cut. Each step warns once when it first deletes, a stalled or waiting site once per day it stops at
// (o.capStalls across rounds); it returns the events and rows deleted.
func (s *Store) EnforceCap(ctx context.Context, o RetentionOptions, now time.Time) (int64, error) {
	maxBytes, log := o.MaxBytes, o.Log
	if log == nil {
		log = slog.Default()
	}
	if maxBytes <= 0 {
		return 0, nil
	}
	used, err := s.UsedBytes(ctx)
	if err == nil {
		alert.Note(o.Report, alert.Condition{Kind: alert.KindSizeCap, Open: used > maxBytes,
			Text: fmt.Sprintf("%d MiB used, the cap is %d MiB", used>>20, maxBytes>>20)})
	}
	if err != nil || used <= maxBytes {
		return 0, err
	}
	sites, err := s.Sites(ctx)
	if err != nil {
		return 0, err
	}
	today := now.UnixMilli() / dayMS
	tail, target := today-(CapTail-1), maxBytes*9/10
	c := &capRun{o: o, floor: AgeFloor(now, o.Months)}
	stalls := o.capStalls
	if stalls == nil {
		stalls = map[int64]int64{}
	}
	// rawFrom is each site's oldest day whose raw stays this round: the tail, or the day its raw step stopped at.
	rawFrom := map[int64]int64{}
	for _, st := range sites {
		rawFrom[st.ID] = tail
	}
	var total int64
	waiting := false
	for i, step := range capSteps {
		if i == 1 {
			for _, st := range sites {
				if _, ok := stalls[st.ID]; ok && rawFrom[st.ID] == tail {
					delete(stalls, st.ID) // not stalled this round
				}
			}
			if waiting && used > target {
				return total, nil
			}
		}
		if i > 0 && used > target {
			floor, err := s.capFloor(ctx, sites, rawFrom, today)
			if err != nil {
				return total, err
			}
			if floor > target {
				log.Warn("Size cap reached: the last days' raw and the actor classes alone exceed it, the aggregates stay",
					"days", CapTail, "at_least_mb", floor>>20, "max_mb", maxBytes>>20)
				return total, nil
			}
		}
		warned := false
		for used > target {
			// The site holding the oldest such day (an index lookup or a few per site), then a chunk of it.
			var site, day, at int64
			var dim string
			found := false
			for _, st := range sites {
				if i == 0 && rawFrom[st.ID] < tail {
					continue // stalled
				}
				d, a, dm, ok, err := step.oldest(ctx, s, st.ID, rawFrom[st.ID])
				if err != nil {
					return total, err
				}
				if ok && (!found || a < at) {
					site, day, at, dim, found = st.ID, d, a, dm, true
				}
			}
			if !found {
				break
			}
			n, err := step.del(ctx, s, c, site, day, dim)
			total += n
			var stalled capStalled
			if errors.As(err, &stalled) {
				rawFrom[stalled.site] = stalled.day
				waiting = waiting || stalled.wait
				// Once per site, day and reason: a day that waited and then stalls says so too.
				seen := stalled.day
				if stalled.wait {
					seen = ^stalled.day
				}
				if last, ok := stalls[stalled.site]; !ok || last != seen {
					stalls[stalled.site] = seen
					msg := "Size cap reached: the site's oldest raw day's figures are not built, its raw from there " +
						"stays until they are"
					if stalled.wait {
						msg = "Size cap waits: the day after the site's oldest raw day is not due for building yet, no " +
							"aggregate goes and the database stays over the cap until it is"
					}
					log.Warn(msg, "site", stalled.site, "day",
						time.UnixMilli(stalled.day*dayMS).UTC().Format(time.DateOnly), "max_mb", maxBytes>>20)
				}
				continue
			}
			if err != nil {
				return total, err
			}
			if !warned {
				warned = true
				log.Warn("Size cap reached: deleting the oldest "+step.what, "from",
					time.UnixMilli(day*dayMS).UTC().Format(time.DateOnly), "max_mb", maxBytes>>20)
			}
			if !run.Sleep(ctx, retentionYield) {
				return total, ctx.Err()
			}
			if used, err = s.UsedBytes(ctx); err != nil {
				return total, err
			}
		}
	}
	if used > target {
		log.Warn("Size cap reached: the last days alone exceed it, they stay", "days", CapTail, "max_mb", maxBytes>>20)
	}
	return total, nil
}

// ErrVacuumStalled is Vacuum giving up: steps that hand no free page back.
var ErrVacuumStalled = errors.New("incremental vacuum made no progress")

// vacuumStmt is one vacuum step (a test replaces it with one that does nothing).
var vacuumStmt = `PRAGMA incremental_vacuum(` + strconv.Itoa(vacuumStep) + `)` //nolint:gosec // a constant, no input

// maxStalledSteps is how many steps in a row may leave the free pages as many as before (the writer may free a few
// meanwhile) before Vacuum stops.
const maxStalledSteps = 3

// Incremental reports whether the database is in incremental auto-vacuum mode: only then can Vacuum shrink the
// file. Open switches an empty database; one that held events first stays in its mode.
func (s *Store) Incremental(ctx context.Context) (bool, error) {
	var mode int
	err := s.DB.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode)
	return mode == 2, err
}

// Vacuum hands the free pages back to the file system a step at a time, so the volume shrinks too without one long
// lock. In another mode than incremental it does nothing (the free pages are reused by new rows, the file keeps its
// size); when steps stop handing pages back it returns ErrVacuumStalled instead of trying forever.
func (s *Store) Vacuum(ctx context.Context) error {
	_, err := s.vacuum(ctx, 0)
	return err
}

// vacuumBudget is how long one retention round hands free pages back (RunRetention); a round that leaves free pages
// is followed by the next one right away, so the cap and the purge are checked between budgets. Measured at 13 GB
// after the first purge: 18k-50k pages a minute, each step holding the write lock 0.3-1 s; its 2.8 M free pages
// took 58 min in one go, no cap check and no purge round meanwhile. A variable: tests shorten it.
var vacuumBudget = 30 * time.Second

// vacuum is Vacuum for at most budget (0: until done). It returns the free pages left: 0 when done, or in another
// mode than incremental.
func (s *Store) vacuum(ctx context.Context, budget time.Duration) (int64, error) {
	if inc, err := s.Incremental(ctx); err != nil || !inc {
		return 0, err
	}
	start := time.Now()
	prev, stalled := int64(-1), 0
	for {
		var free int64
		if err := s.DB.QueryRowContext(ctx, `SELECT freelist_count FROM pragma_freelist_count()`).Scan(&free); err != nil ||
			free == 0 {
			return 0, err
		}
		if prev >= 0 && free >= prev {
			if stalled++; stalled >= maxStalledSteps {
				return 0, fmt.Errorf("%w: %d free pages left", ErrVacuumStalled, free)
			}
		} else {
			stalled = 0
		}
		if budget > 0 && time.Since(start) >= budget {
			return free, nil
		}
		prev = free
		if _, err := s.DB.ExecContext(ctx, vacuumStmt); err != nil {
			return 0, err
		}
		if !run.Sleep(ctx, retentionYield) {
			return 0, ctx.Err()
		}
	}
}

// deleteChunks deletes the events matching where in chunks, yielding between them.
func (s *Store) deleteChunks(ctx context.Context, where string, args ...any) (int64, error) {
	var total int64
	for {
		n, err := s.deleteEvents(ctx, where, args...)
		total += n
		if err != nil {
			return total, err
		}
		if n < retentionChunk {
			return total, nil
		}
		if !run.Sleep(ctx, retentionYield) {
			return total, ctx.Err()
		}
	}
}

// deleteEvents deletes one chunk of the events matching where and, in the same transaction, marks every site-day it
// deleted from as without raw (raw = 0, the row made if missing) and bumps its version: the aggregate job never
// builds such a day, a build that read it before finds its version moved, so a half-deleted day cannot replace
// figures built from all of it, and the dashboard stops reading its raw.
func (s *Store) deleteEvents(ctx context.Context, where string, args ...any) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	n, err := deleteEventsTx(ctx, tx, where, args...)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// deleteEventsTx is deleteEvents in the caller's transaction, uncommitted.
func deleteEventsTx(ctx context.Context, tx *sql.Tx, where string, args ...any) (int64, error) {
	//nolint:gosec // where is this file's own constant; values are bound
	rows, err := tx.QueryContext(ctx, `DELETE FROM event WHERE id IN (SELECT id FROM event WHERE `+where+
		` LIMIT `+strconv.Itoa(retentionChunk)+`) RETURNING site_id, ts`, args...)
	if err != nil {
		return 0, err
	}
	var n int64
	days := map[[2]int64]bool{}
	for rows.Next() {
		var site, ts int64
		if err := rows.Scan(&site, &ts); err != nil {
			_ = rows.Close()
			return 0, err
		}
		n++
		days[[2]int64{site, ts / dayMS}] = true
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for d := range days {
		if _, err := tx.ExecContext(ctx, `INSERT INTO agg_day (site_id, day, raw, version) VALUES (?, ?, 0, 1)
			ON CONFLICT (site_id, day) DO UPDATE SET raw = 0, version = version + 1`, d[0], d[1]); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// RetentionOptions configures RunRetention.
type RetentionOptions struct {
	// Months is how long the per-address aggregates, actor classes and model usage are kept, and raw at most.
	Months int
	// RawDays is how long raw events are kept once the purge's grace has passed (PurgeRaw); 0 keeps them Months.
	RawDays int
	// Builder is the aggregate build code's version (agg.Version): only a day built clean by it lets its raw go.
	Builder int64
	// Cover builds a site-day's aggregates when the aggregate job would (due, not before its age floor), so the cap
	// can delete its raw; store does not import agg. A day not due yet: ErrNotDue, and the cap waits for it. nil: the
	// cap deletes no raw it finds unbuilt.
	Cover func(ctx context.Context, site, day int64) error
	// MaxBytes caps what the rows take (0: no cap).
	MaxBytes int64
	// CapEvery is how often the cap is checked (UsedBytes is three pragmas): five minutes by default, so the disk
	// bound is the cap plus minutes of intake, not a day of it (two hours of it from 00:00 to 02:00 UTC, while a
	// round waits). Age retention runs on the first check and daily.
	CapEvery time.Duration
	Now      func() time.Time
	Log      *slog.Logger
	// Report takes each round's outcome (job.failed: retention, purge) and whether the rows are over the cap
	// (db.size_cap).
	Report alert.Reporter
	// capStalls is, per site, the day the cap's raw step last stopped at, complemented while it only waits
	// (RunRetention keeps it across rounds): its warning once per reason, not every few minutes.
	capStalls map[int64]int64
}

// logWaits says once per site and day where the purge stopped: waiting for the job (info), or at a day nothing will
// build (warn: its raw stays until age retention takes it). A site that goes on again is forgotten.
func logWaits(log *slog.Logger, last map[int64]PurgeWait, waits []PurgeWait) {
	now := map[int64]PurgeWait{}
	for _, w := range waits {
		now[w.Site] = w
		if last[w.Site] == w {
			continue
		}
		day := time.UnixMilli(w.Day * dayMS).UTC().Format(time.DateOnly)
		if w.Cut {
			log.Warn("Raw purge stopped: the day's raw is cut and its figures are partial, nothing builds it; "+
				"its raw and the newer stay until age retention", "site", w.Site, "day", day)
		} else {
			log.Info("Raw purge waits for the aggregate job", "site", w.Site, "day", day)
		}
	}
	clear(last)
	maps.Copy(last, now)
}

// RunRetention keeps the database within its age and size until ctx ends.
func (s *Store) RunRetention(ctx context.Context, o RetentionOptions) {
	if o.CapEvery <= 0 {
		o.CapEvery = 5 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	o.capStalls = map[int64]int64{}
	// A database that held events before Trakk switched incremental auto-vacuum on keeps its mode: switching needs a
	// full VACUUM, which locks the collector out for minutes and needs the file's size again in free disk. Retention
	// runs without it: deleted pages are reused by new rows (the cap counts the rows, not the file), the file does
	// not shrink.
	if inc, err := s.Incremental(ctx); err == nil && !inc {
		o.Log.Warn("Database not in incremental auto-vacuum mode: retention frees pages for reuse, the file does not " +
			"shrink (switch with PRAGMA auto_vacuum = INCREMENTAL; VACUUM; while Trakk is stopped)")
	}
	// The purge's first start, kept across restarts: the grace runs from it. Until it is recorded the purge deletes
	// nothing and the footer claims no raw days; a start that failed is retried every round.
	armed, armWarned := false, false
	arm := func() {
		if armed {
			return
		}
		at, err := s.ArmRawPurge(ctx, o.Now())
		if err != nil {
			if !armWarned && ctx.Err() == nil {
				armWarned = true
				o.Log.Warn("Raw purge not armed, no raw is purged until it is", logging.Err(err))
			}
			return
		}
		armed = true
		if from := at.Add(PurgeGrace); o.RawDays > 0 && o.Now().Before(from) {
			o.Log.Info("Raw purge in its grace: raw events older than raw_days are deleted from", "raw_days", o.RawDays,
				"from", from.Format(time.RFC3339))
		}
	}
	arm()
	var lastAge, shrinking time.Time
	waiting := map[int64]PurgeWait{}
	// The first check a minute after start (sooner with a shorter cadence), then every CapEvery; right after a round
	// whose vacuum budget ran out with free pages left.
	var next time.Duration
	for wait := min(o.CapEvery, time.Minute); run.Sleep(ctx, wait); wait = next {
		arm()
		// Each step runs whatever the one before it did: a purge that cannot read its first start must not switch the
		// size cap off.
		var deleted int64
		var errs []error
		if now := o.Now(); now.Sub(lastAge) >= 24*time.Hour {
			lastAge = now
			n, err := s.PruneAge(ctx, now.AddDate(0, -o.Months, 0))
			deleted, errs = deleted+n, append(errs, err)
			n, err = s.PruneAlerts(ctx, now.AddDate(0, 0, -AlertHistoryDays))
			deleted, errs = deleted+n, append(errs, err)
		}
		// Every check: a site waiting for the job goes on minutes after it built the day, not a day later.
		start := time.Now()
		n, waits, purgeErr := s.PurgeRaw(ctx, o, o.Now())
		deleted = deleted + n
		alert.Fail(o.Report, alert.JobPurge, purgeErr)
		if n > 0 {
			o.Log.Info("Raw purge done", "deleted", n, "took", time.Since(start).Round(time.Millisecond).String())
		}
		logWaits(o.Log, waiting, waits)
		n, err := s.EnforceCap(ctx, o, o.Now())
		deleted, errs = deleted+n, append(errs, err)
		// Every round while free pages are left, a budget at a time (a stall or another mode: none left to try).
		var vacuumed time.Duration
		next = o.CapEvery
		if ctx.Err() == nil {
			began := time.Now()
			left, verr := s.vacuum(ctx, vacuumBudget)
			vacuumed, errs = time.Since(began), append(errs, verr)
			switch {
			case left > 0:
				next = retentionYield
				if shrinking.IsZero() {
					shrinking = began
					o.Log.Info("Vacuum continues over the next rounds", "free_pages", left)
				}
			case !shrinking.IsZero():
				o.Log.Info("Vacuum done", "took", time.Since(shrinking).Round(time.Second).String(), "stalled", verr != nil)
				shrinking = time.Time{}
			}
		}
		// The purge reports apart: its waits for the aggregate job are no failure, its errors are.
		alert.Fail(o.Report, alert.JobRetention, errors.Join(errs...))
		err = errors.Join(append(errs, purgeErr)...)
		switch {
		case err != nil && ctx.Err() == nil:
			o.Log.Warn("Retention failed", logging.Err(err))
		case deleted > 0:
			o.Log.Info("Retention done", "deleted", deleted, "vacuum_took", vacuumed.Round(time.Millisecond).String())
		}
	}
}
