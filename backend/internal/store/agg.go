package store

import (
	"context"
	"database/sql"
	"errors"
	"iter"
	"time"

	"github.com/trick77/ship/kit/run"
)

// aggChunk bounds the rows one aggregate transaction deletes and inserts: a short write, as retention's chunks.
const aggChunk = retentionChunk

// agg_day.dirty. The dashboard reads a frozen day's rows only while AggClean.
const (
	AggClean      = 0
	AggRawChanged = 1 // raw changed since the rows were written: complete, but stale
	AggRequeued   = 2 // the day before changed the sessions this day carries: complete
	AggWriting    = 3 // a write under way or cut short: the rows may be partial
)

// Queryer is a pool or a transaction: a build reads a day and its state in one read transaction's snapshot.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// AggDays returns a site's agg_day rows for the days from first to last, keyed by day.
func (s *Store) AggDays(ctx context.Context, siteID, first, last int64) (map[int64]AggDay, error) {
	return AggDaysIn(ctx, s.DB, siteID, first, last)
}

// AggDaysIn is AggDays read through q.
func AggDaysIn(ctx context.Context, q Queryer, siteID, first, last int64) (map[int64]AggDay, error) {
	rows, err := q.QueryContext(ctx, `SELECT site_id, day, frozen, dirty, version, builder, raw, events, built_at
		FROM agg_day WHERE site_id = ? AND day >= ? AND day <= ?`, siteID, first, last)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]AggDay{}
	for rows.Next() {
		var d AggDay
		if err := rows.Scan(&d.SiteID, &d.Day, &d.Frozen, &d.Dirty, &d.Version, &d.Builder, &d.Raw, &d.Events,
			&d.BuiltAt); err != nil {
			return nil, err
		}
		out[d.Day] = d
	}
	return out, rows.Err()
}

// OldestEventDay is the unix day of a site's oldest event, ok false when it has none: where its raw begins.
func (s *Store) OldestEventDay(ctx context.Context, siteID int64) (int64, bool, error) {
	var ts sql.NullInt64
	if err := s.DB.QueryRowContext(ctx, `SELECT MIN(ts) FROM event WHERE site_id = ?`, siteID).Scan(&ts); err != nil {
		return 0, false, err
	}
	return ts.Int64 / dayMS, ts.Valid, nil
}

// BeginAggDay makes sure a site-day has its row before a build reads it: from then on every raw change to the day
// bumps the version the build's read sees.
func (s *Store) BeginAggDay(ctx context.Context, siteID, day int64) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO agg_day (site_id, day) VALUES (?, ?)
		ON CONFLICT (site_id, day) DO NOTHING`, siteID, day)
	return err
}

// ClearAggDirty clears a site-day's dirty flag when its raw is gone: nothing can rebuild it, its complete rows
// stand. A day left mid-write (AggWriting) stays out of reading.
func (s *Store) ClearAggDirty(ctx context.Context, siteID, day int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE agg_day SET dirty = 0 WHERE site_id = ? AND day = ? AND raw = 0
		AND dirty IN (1, 2)`, siteID, day)
	return err
}

// ClearAggDirtyBefore is ClearAggDirty for every day of a site before day: those before its oldest raw, which a
// build finishing after retention cut them left dirty. A year of agg_day rows per statement (a row a day, a few
// pages), from the site's first.
func (s *Store) ClearAggDirtyBefore(ctx context.Context, siteID, day int64) error {
	var first sql.NullInt64
	if err := s.DB.QueryRowContext(ctx, `SELECT MIN(day) FROM agg_day WHERE site_id = ?`, siteID).
		Scan(&first); err != nil || !first.Valid {
		return err
	}
	for lo := first.Int64; lo < day; lo += 366 {
		if _, err := s.DB.ExecContext(ctx, `UPDATE agg_day SET dirty = 0 WHERE site_id = ? AND day >= ? AND day < ?
			AND raw = 0 AND dirty IN (1, 2)`, siteID, lo, min(lo+366, day)); err != nil {
			return err
		}
	}
	return nil
}

// AggCarriedIn returns a site-day's stored carried sessions per address key and part, read through q: a rebuild
// keeps them when the day before has no raw left to compare with.
func AggCarriedIn(ctx context.Context, q Queryer, siteID, day int64) (map[AggAddrKey]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT ip_key, part, carried FROM agg_addr WHERE site_id = ? AND day = ?`,
		siteID, day)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[AggAddrKey]int64{}
	for rows.Next() {
		var k AggAddrKey
		var n int64
		if err := rows.Scan(&k.IPKey, &k.Part, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// The deletes take at most a chunk's rows each, by primary key: agg_list's starts with (site_id, dim, day), so it
// is cleared a dimension at a time.
const (
	delAggHour = `DELETE FROM agg_hour WHERE site_id = ?1 AND hour >= ?2 AND hour < ?3 AND (hour, cls) IN
		(SELECT hour, cls FROM agg_hour WHERE site_id = ?1 AND hour >= ?2 AND hour < ?3 LIMIT ?4)`
	delAggList = `DELETE FROM agg_list WHERE site_id = ?1 AND dim = ?2 AND day = ?3 AND (part, other, parent, key, sub)
		IN (SELECT part, other, parent, key, sub FROM agg_list WHERE site_id = ?1 AND dim = ?2 AND day = ?3 LIMIT ?4)`
	delAggAddr = `DELETE FROM agg_addr WHERE site_id = ?1 AND day = ?2 AND (ip_key, part) IN
		(SELECT ip_key, part FROM agg_addr WHERE site_id = ?1 AND day = ?2 LIMIT ?3)`

	insAggHour = `INSERT INTO agg_hour (site_id, hour, cls, events, views, actions, starts, requests, refused, errors,
		browser_ids, addrs, server_addrs) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	insAggList = `INSERT INTO agg_list (site_id, dim, day, part, other, parent, key, sub, a, b, n, addr_n, ids, addrs,
		entries, keys) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	insAggAddr = `INSERT INTO agg_addr (site_id, day, ip_key, part, events, views, actions, requests, refused,
		sessions, carried, browser, human, first_ts, last_ts, ua, asn, country, cls)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
)

// chunks runs writes in transactions of at most aggChunk rows, yielding between them: the collector's writer gets
// the lock in the gaps.
type chunks struct {
	s       *Store
	tx      *sql.Tx
	n       int
	commits int
	ctx     context.Context
}

// room is how many rows the open transaction may still take, after committing a full one.
func (c *chunks) room() (int, error) {
	if c.tx != nil && c.n >= aggChunk {
		if err := c.commit(); err != nil {
			return 0, err
		}
		if !run.Sleep(c.ctx, retentionYield) {
			return 0, c.ctx.Err()
		}
	}
	if c.tx == nil {
		tx, err := c.s.DB.BeginTx(c.ctx, nil)
		if err != nil {
			return 0, err
		}
		c.tx, c.n = tx, 0
	}
	return aggChunk - c.n, nil
}

func (c *chunks) commit() error {
	if c.tx == nil {
		return nil
	}
	err := c.tx.Commit()
	c.tx = nil
	c.commits++
	return err
}

func (c *chunks) rollback() {
	if c.tx != nil {
		_ = c.tx.Rollback()
	}
}

// del repeats a chunked delete until it takes fewer rows than it was allowed.
func (c *chunks) del(q string, args ...any) error {
	for {
		room, err := c.room()
		if err != nil {
			return err
		}
		res, err := c.tx.ExecContext(c.ctx, q, append(args, room)...)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		c.n += int(n)
		if int(n) < room {
			return nil
		}
	}
}

func (c *chunks) ins(q string, args ...any) error {
	if _, err := c.room(); err != nil {
		return err
	}
	if _, err := c.tx.ExecContext(c.ctx, q, args...); err != nil {
		return err
	}
	c.n++
	return nil
}

// AggBuild is one site-day as a build made it from the raw its read saw at Version.
type AggBuild struct {
	Site, Day int64
	Version   int64 // agg_day.version in the read's snapshot
	Builder   int64
	Events    int64
	// RequeueNext: the build may change the next day's carried sessions.
	RequeueNext bool
	Dims        []string // the list dimensions the day's old rows are cleared of
	Hours       []AggHour
	Lists       []AggList
	// Addrs come streamed from the build's sums: a busy day has hundreds of thousands, a second copy doubles the peak.
	Addrs iter.Seq[AggAddr]
}

// ErrNoAggDay is WriteAggDay on a day BeginAggDay did not start.
var ErrNoAggDay = errors.New("no agg_day row")

// WriteAggDay replaces a site-day's aggregate rows with b's and freezes the day, in transactions of at most aggChunk
// rows: one transaction of a busy day's rows would hold the collector's writer off for seconds. The first one marks
// the day AggWriting before it deletes anything, so neither a reader nor the job after a crash takes partial rows for
// complete ones. The last one sets the state from what it finds then: clean only while the version is still
// b.Version and the raw all there; else AggRawChanged, the rows complete and built from the whole raw the read saw
// (the job rebuilds the day, or, its raw gone, lets them stand). It returns whether the day came out clean.
func (s *Store) WriteAggDay(ctx context.Context, b AggBuild) (bool, error) {
	c := &chunks{s: s, ctx: ctx}
	defer c.rollback()
	if _, err := c.room(); err != nil {
		return false, err
	}
	res, err := c.tx.ExecContext(ctx, `UPDATE agg_day SET dirty = 3 WHERE site_id = ? AND day = ?`, b.Site, b.Day)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, ErrNoAggDay
	}
	c.n++
	hour := b.Day * 24
	if err := c.del(delAggHour, b.Site, hour, hour+24); err != nil {
		return false, err
	}
	for _, dim := range b.Dims {
		if err := c.del(delAggList, b.Site, dim, b.Day); err != nil {
			return false, err
		}
	}
	if err := c.del(delAggAddr, b.Site, b.Day); err != nil {
		return false, err
	}
	for _, h := range b.Hours {
		if err := c.ins(insAggHour, b.Site, h.Hour, h.Cls, h.Events, h.Views, h.Actions, h.Starts, h.Requests,
			h.Refused, h.Errors, h.BrowserIDs, h.Addrs, h.ServerAddrs); err != nil {
			return false, err
		}
	}
	for _, l := range b.Lists {
		if err := c.ins(insAggList, b.Site, l.Dim, b.Day, l.Part, l.Other, l.Parent, l.Key, l.Sub, l.A, l.B, l.N,
			l.AddrN, l.IDs, l.Addrs, l.Entries, l.Keys); err != nil {
			return false, err
		}
	}
	if b.Addrs != nil {
		for a := range b.Addrs {
			if err = c.ins(insAggAddr, b.Site, b.Day, a.IPKey, a.Part, a.Events, a.Views, a.Actions, a.Requests,
				a.Refused, a.Sessions, a.Carried, a.Browser, a.Human, a.FirstTS, a.LastTS, a.UA, a.ASN, a.Country,
				a.Cls); err != nil {
				return false, err
			}
		}
	}
	if _, err := c.room(); err != nil {
		return false, err
	}
	clean, err := finishAggDay(ctx, c.tx, b)
	if err != nil {
		return false, err
	}
	return clean, c.commit()
}

// finishAggDay freezes a written day in the write's last transaction. RequeueNext takes the next day, when frozen
// and clean, out of reading for a rebuild: its carried sessions were counted against this day's raw, which changed.
func finishAggDay(ctx context.Context, tx *sql.Tx, b AggBuild) (bool, error) {
	var version int64
	var raw bool
	if err := tx.QueryRowContext(ctx, `SELECT version, raw FROM agg_day WHERE site_id = ? AND day = ?`, b.Site,
		b.Day).Scan(&version, &raw); err != nil {
		return false, err
	}
	clean := version == b.Version && raw
	dirty := AggClean
	if !clean {
		dirty = AggRawChanged
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agg_day SET frozen = 1, dirty = ?, builder = ?, events = ?, built_at = ?
		WHERE site_id = ? AND day = ?`, dirty, b.Builder, b.Events, time.Now().UnixMilli(), b.Site, b.Day); err != nil {
		return false, err
	}
	if b.RequeueNext {
		if _, err := tx.ExecContext(ctx, `UPDATE agg_day SET dirty = 2
			WHERE site_id = ? AND day = ? AND frozen = 1 AND dirty = 0 AND raw = 1`, b.Site, b.Day+1); err != nil {
			return false, err
		}
	}
	return clean, nil
}
