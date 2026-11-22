package store

import (
	"context"
	"strconv"
	"time"
)

// retentionChunk is the rows one delete transaction removes: small, so the batch writer never waits long behind it.
const retentionChunk = 5000

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
// until they take less than nine tenths of it. Counters, classes and usage older than before go too. It returns the
// events deleted. Every delete is its own short transaction.
func (s *Store) Prune(ctx context.Context, before time.Time, maxBytes int64) (int64, error) {
	cut := before.UnixMilli()
	var total int64
	// Per site: event_site_ts finds the old rows; ts alone has no index and would scan the table.
	sites, err := s.Sites(ctx)
	if err != nil {
		return 0, err
	}
	for _, st := range sites {
		n, err := s.deleteEvents(ctx, `site_id = ? AND ts < ?`, st.ID, cut)
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
	if maxBytes > 0 {
		for {
			used, err := s.UsedBytes(ctx)
			if err != nil || used <= maxBytes*9/10 {
				break
			}
			var oldest int64
			if err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(MIN(id), 0) FROM event`).Scan(&oldest); err != nil ||
				oldest == 0 {
				break
			}
			res, err := s.DB.ExecContext(ctx, `DELETE FROM event WHERE id < ?`, oldest+retentionChunk)
			if err != nil {
				return total, err
			}
			n, _ := res.RowsAffected()
			total += n
		}
	}
	// Hand the freed pages back to the file system, so the volume shrinks too.
	_, err = s.DB.ExecContext(ctx, `PRAGMA incremental_vacuum`)
	return total, err
}

// deleteEvents deletes the events matching where in chunks.
func (s *Store) deleteEvents(ctx context.Context, where string, args ...any) (int64, error) {
	var total int64
	for {
		res, err := s.DB.ExecContext(ctx, `DELETE FROM event WHERE id IN (SELECT id FROM event WHERE `+where+
			` LIMIT `+strconv.Itoa(retentionChunk)+`)`, args...)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < retentionChunk || ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
}
