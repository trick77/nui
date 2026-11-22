package store

import (
	"context"
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

// Prune deletes events older than before and, while the rows take more than maxBytes (0: no cap), the oldest events
// until they take less than nine tenths of it, then hands the freed pages back. Counters, classes and usage older
// than before go too. It returns the events deleted. Every delete is its own short statement.
func (s *Store) Prune(ctx context.Context, before time.Time, maxBytes int64) (int64, error) {
	total, err := s.PruneAge(ctx, before)
	if err != nil {
		return total, err
	}
	n, err := s.EnforceCap(ctx, maxBytes)
	total += n
	if err != nil {
		return total, err
	}
	return total, s.Vacuum(ctx)
}

// PruneAge deletes what is older than before.
func (s *Store) PruneAge(ctx context.Context, before time.Time) (int64, error) {
	cut := before.UnixMilli()
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

// EnforceCap deletes the oldest events, by time over every site, while the rows take more than maxBytes, until they
// take nine tenths of it. 0 is no cap.
func (s *Store) EnforceCap(ctx context.Context, maxBytes int64) (int64, error) {
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
	var total int64
	for used > maxBytes*9/10 {
		// The site holding the oldest event (one index lookup per site), then its oldest chunk.
		var site, oldest int64 = 0, -1
		for _, st := range sites {
			var ts *int64
			if err := s.DB.QueryRowContext(ctx, `SELECT MIN(ts) FROM event WHERE site_id = ?`, st.ID).Scan(&ts); err != nil {
				return total, err
			}
			if ts != nil && (oldest < 0 || *ts < oldest) {
				site, oldest = st.ID, *ts
			}
		}
		if oldest < 0 {
			break
		}
		res, err := s.DB.ExecContext(ctx, `DELETE FROM event WHERE id IN (SELECT id FROM event WHERE site_id = ?
			ORDER BY ts LIMIT `+strconv.Itoa(retentionChunk)+`)`, site)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if !run.Sleep(ctx, retentionYield) {
			return total, ctx.Err()
		}
		if used, err = s.UsedBytes(ctx); err != nil {
			return total, err
		}
	}
	return total, nil
}

// Vacuum hands the free pages back to the file system a step at a time, so the volume shrinks too without one long
// lock.
func (s *Store) Vacuum(ctx context.Context) error {
	for {
		var free int64
		if err := s.DB.QueryRowContext(ctx, `SELECT freelist_count FROM pragma_freelist_count()`).Scan(&free); err != nil ||
			free == 0 {
			return err
		}
		if _, err := s.DB.ExecContext(ctx, `PRAGMA incremental_vacuum(`+strconv.Itoa(vacuumStep)+`)`); err != nil {
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
		//nolint:gosec // where is this file's own constant; values are bound
		res, err := s.DB.ExecContext(ctx, `DELETE FROM event WHERE id IN (SELECT id FROM event WHERE `+where+
			` LIMIT `+strconv.Itoa(retentionChunk)+`)`, args...)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < retentionChunk {
			return total, nil
		}
		if !run.Sleep(ctx, retentionYield) {
			return total, ctx.Err()
		}
	}
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
			n, err = s.EnforceCap(ctx, o.MaxBytes)
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
