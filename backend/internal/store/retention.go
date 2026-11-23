package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/trick77/ship/kit/logging"
	"github.com/trick77/ship/kit/run"
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

// Prune deletes what is older than before and, while the rows take more than maxBytes (0: no cap), what EnforceCap
// takes, then hands the freed pages back. It returns the events and aggregate rows deleted. Every delete is its own
// short statement.
func (s *Store) Prune(ctx context.Context, before time.Time, maxBytes int64) (int64, error) {
	total, err := s.PruneAge(ctx, before)
	if err != nil {
		return total, err
	}
	n, err := s.EnforceCap(ctx, maxBytes, time.Now(), slog.Default())
	total += n
	if err != nil {
		return total, err
	}
	return total, s.Vacuum(ctx)
}

// delAggAddrBefore deletes a chunk of a site's per-address rows older than a day (?1 site, ?2 day, ?3 chunk).
const delAggAddrBefore = `DELETE FROM agg_addr WHERE site_id = ?1 AND (day, ip_key, part) IN
	(SELECT day, ip_key, part FROM agg_addr WHERE site_id = ?1 AND day < ?2 LIMIT ?3)`

// PruneAge deletes what is older than before: events, and the per-address aggregates with the actor classes (they
// name addresses as the events do; the other aggregates stay). Events go by whole UTC days (the cutoff floored to
// midnight): an aggregate day is built from all of its raw or none of it. It returns the events and rows deleted.
func (s *Store) PruneAge(ctx context.Context, before time.Time) (int64, error) {
	cut := before.UnixMilli() / dayMS * dayMS
	var total int64
	// Per site: event_site_ts finds the old rows; ts alone has no index and would scan the table.
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
		for {
			n, err := s.execRows(ctx, delAggAddrBefore, st.ID, cut/dayMS, retentionChunk)
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
	for q, arg := range map[string]int64{
		`DELETE FROM ingest_stat WHERE hour < ?`: cut / int64(time.Hour/time.Millisecond),
		`DELETE FROM actor_class WHERE day < ?`:  cut / dayMS,
		`DELETE FROM llm_usage WHERE day < ?`:    cut / dayMS,
	} {
		if _, err := s.DB.ExecContext(ctx, q, arg); err != nil {
			return total, err
		}
	}
	return total, nil
}

// execRows runs one statement and returns the rows it changed.
func (s *Store) execRows(ctx context.Context, q string, args ...any) (int64, error) {
	res, err := s.DB.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// capTail is the UTC days the cap never cuts: today, yesterday and the day before, which the aggregate job has not
// built yet (a day is built after 02:00 two days on). Cutting them, as the cap once did when the aggregates filled
// it, deleted every raw event and left those days without figures for good.
const capTail = 3

// capStep is one kind of row the cap deletes: oldest finds a site's oldest day before the tail that still has such
// rows (at orders it against the other sites', dim is a list's dimension), del deletes a chunk of them.
type capStep struct {
	what   string
	oldest func(ctx context.Context, s *Store, site, tail int64) (day, at int64, dim string, ok bool, err error)
	del    func(ctx context.Context, s *Store, site, day int64, dim string, tail int64) (int64, error)
}

// capSteps are what the cap deletes, in order: raw before the aggregates (the figures outlive it, as age retention
// has it), then the per-address rows, the lists, the hours: the finest grain first.
var capSteps = []capStep{
	{what: "raw events", oldest: func(ctx context.Context, s *Store, site, tail int64) (int64, int64, string, bool, error) {
		var ts sql.NullInt64
		err := s.DB.QueryRowContext(ctx, `SELECT MIN(ts) FROM event WHERE site_id = ?`, site).Scan(&ts)
		return ts.Int64 / dayMS, ts.Int64, "", ts.Valid && ts.Int64 < tail*dayMS, err
	}, del: func(ctx context.Context, s *Store, site, _ int64, _ string, tail int64) (int64, error) {
		return s.deleteEvents(ctx, `site_id = ? AND ts < ? ORDER BY ts`, site, tail*dayMS)
	}},
	{what: "per-address aggregates", oldest: minDay(`SELECT MIN(day) FROM agg_addr WHERE site_id = ?`, 1),
		del: func(ctx context.Context, s *Store, site, day int64, _ string, _ int64) (int64, error) {
			return s.capDelete(ctx, site, day, delAggAddr, site, day, retentionChunk)
		}},
	{what: "list aggregates", oldest: oldestList,
		del: func(ctx context.Context, s *Store, site, day int64, dim string, _ int64) (int64, error) {
			return s.capDelete(ctx, site, day, delAggList, site, dim, day, retentionChunk)
		}},
	{what: "hourly aggregates", oldest: minDay(`SELECT MIN(hour) FROM agg_hour WHERE site_id = ?`, 24),
		del: func(ctx context.Context, s *Store, site, day int64, _ string, _ int64) (int64, error) {
			return s.capDelete(ctx, site, day, delAggHour, site, day*24, day*24+24, retentionChunk)
		}},
}

// capDelete deletes a chunk of a day's aggregate rows and, in the same transaction, marks the day unbuilt with
// partial rows (frozen 0, AggWriting): without raw (the cap deletes aggregates only once no raw before the tail is
// left), the state of a day nothing can be read for, so every figure of it is missing alike, never a zero from the
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

// capFloor is a lower bound of the bytes the aggregate steps can never free: the tail's raw and the actor classes,
// which no step touches. Text bytes plus the floors above: no page, cell or integer bytes beyond them, so the real
// size is larger and a cap below the bound is out of reach for certain; between the bound and the real size the
// steps still delete in vain. A tail day per statement, actor classes a month (dash's classDays).
func (s *Store) capFloor(ctx context.Context, sites map[string]Site, tail, today int64) (int64, error) {
	var total int64
	for _, st := range sites {
		for d := tail; d <= today; d++ {
			var n, text int64
			if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(octet_length(source) + octet_length(kind)
				+ octet_length(name) + octet_length(visitor) + octet_length(sid) + octet_length(ip) + 2 * octet_length(ip_key)
				+ octet_length(ua) + octet_length(path) + octet_length(template) + octet_length(url) + octet_length(ref)
				+ octet_length(ref_host) + octet_length(method) + octet_length(country) + octet_length(account)
				+ octet_length(props)), 0) FROM event WHERE site_id = ? AND ts >= ? AND ts < ?`,
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

// EnforceCap deletes, while the rows take more than maxBytes and until they take nine tenths of it, the capSteps in
// order, each oldest first over every site, in chunks, never a day of the last capTail. 0 is no cap. An aggregate
// step starts only while deleting aggregates can still reach the target (capFloor): figures deleted in vain are lost
// for good. agg_day stays: it says which days were built, and which the cap cut. Each step warns once when it
// starts; it returns the events and rows deleted.
func (s *Store) EnforceCap(ctx context.Context, maxBytes int64, now time.Time, log *slog.Logger) (int64, error) {
	if maxBytes <= 0 {
		return 0, nil
	}
	used, err := s.UsedBytes(ctx)
	if err != nil || used <= maxBytes {
		return 0, err
	}
	sites, err := s.Sites(ctx)
	if err != nil {
		return 0, err
	}
	today := now.UnixMilli() / dayMS
	tail, target := today-(capTail-1), maxBytes*9/10
	var total int64
	for i, step := range capSteps {
		if i > 0 && used > target {
			floor, err := s.capFloor(ctx, sites, tail, today)
			if err != nil {
				return total, err
			}
			if floor > target {
				log.Warn("Size cap reached: the last days' raw and the actor classes alone exceed it, the aggregates stay",
					"days", capTail, "at_least_mb", floor>>20, "max_mb", maxBytes>>20)
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
				d, a, dm, ok, err := step.oldest(ctx, s, st.ID, tail)
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
			if !warned {
				warned = true
				log.Warn("Size cap reached: deleting the oldest "+step.what, "from",
					time.UnixMilli(day*dayMS).UTC().Format(time.DateOnly), "max_mb", maxBytes>>20)
			}
			n, err := step.del(ctx, s, site, day, dim, tail)
			total += n
			if err != nil {
				return total, err
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
		log.Warn("Size cap reached: the last days alone exceed it, they stay", "days", capTail, "max_mb", maxBytes>>20)
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
	if inc, err := s.Incremental(ctx); err != nil || !inc {
		return err
	}
	prev, stalled := int64(-1), 0
	for {
		var free int64
		if err := s.DB.QueryRowContext(ctx, `SELECT freelist_count FROM pragma_freelist_count()`).Scan(&free); err != nil ||
			free == 0 {
			return err
		}
		if prev >= 0 && free >= prev {
			if stalled++; stalled >= maxStalledSteps {
				return fmt.Errorf("%w: %d free pages left", ErrVacuumStalled, free)
			}
		} else {
			stalled = 0
		}
		prev = free
		if _, err := s.DB.ExecContext(ctx, vacuumStmt); err != nil {
			return err
		}
		if !run.Sleep(ctx, retentionYield) {
			return ctx.Err()
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
	return n, tx.Commit()
}

// RetentionOptions configures RunRetention.
type RetentionOptions struct {
	// Months is how long events are kept.
	Months int
	// MaxBytes caps what the rows take (0: no cap).
	MaxBytes int64
	// CapEvery is how often the cap is checked (UsedBytes is three pragmas): five minutes by default, so the disk
	// bound is the cap plus minutes of intake, not a day of it. Age retention runs on the first check and daily.
	CapEvery time.Duration
	Now      func() time.Time
	Log      *slog.Logger
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
	// A database that held events before Stakeout switched incremental auto-vacuum on keeps its mode: switching needs a
	// full VACUUM, which locks the collector out for minutes and needs the file's size again in free disk. Retention
	// runs without it: deleted pages are reused by new rows (the cap counts the rows, not the file), the file does
	// not shrink.
	if inc, err := s.Incremental(ctx); err == nil && !inc {
		o.Log.Warn("Database not in incremental auto-vacuum mode: retention frees pages for reuse, the file does not " +
			"shrink (switch with PRAGMA auto_vacuum = INCREMENTAL; VACUUM; while Stakeout is stopped)")
	}
	var lastAge time.Time
	// The first check a minute after start (sooner with a shorter cadence), then every CapEvery.
	for wait := min(o.CapEvery, time.Minute); run.Sleep(ctx, wait); wait = o.CapEvery {
		var deleted int64
		var err error
		if now := o.Now(); now.Sub(lastAge) >= 24*time.Hour {
			lastAge = now
			deleted, err = s.PruneAge(ctx, now.AddDate(0, -o.Months, 0))
		}
		if err == nil {
			var n int64
			n, err = s.EnforceCap(ctx, o.MaxBytes, o.Now(), o.Log)
			deleted += n
		}
		if err == nil && deleted > 0 {
			err = s.Vacuum(ctx)
		}
		switch {
		case err != nil && ctx.Err() == nil:
			o.Log.Warn("Retention failed", logging.Err(err))
		case deleted > 0:
			o.Log.Info("Retention done", "deleted", deleted)
		}
	}
}
