package dash

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Source is where the dashboard reads the figures the aggregate tables can serve.
type Source int

const (
	// SourceAuto reads 24h and 7d (and their prior period) raw while it exists, every other day from the aggregates
	// once they are built and clean, raw before that.
	SourceAuto Source = iota
	// SourceRaw reads every figure from the raw events.
	SourceRaw
	// SourceAgg reads every figure the aggregates can serve from them, and fails on a day that has raw events but no
	// clean aggregates: the tests' way to see both sources answer alike.
	SourceAgg
)

const hourMS = int64(time.Hour / time.Millisecond)

// ErrNoAggregates is SourceAgg on a day the aggregates do not cover.
var ErrNoAggregates = errors.New("no aggregates for a day with raw events")

// how is the source one piece is read from.
type how int

const (
	readNone how = iota // nothing to read: before the first event, or raw gone with nothing built
	readRaw
	readAgg
)

// plan is one request's view of a site's coverage: which days are built and clean, where its raw begins. Every
// aggregate read checks its day again in the same statement, so a day rebuilt meanwhile falls back to raw.
type plan struct {
	s       *Server
	src     Source
	site    int64
	now     time.Time
	oldest  int64 // the oldest raw event's day
	hasRaw  bool
	// oldestTS is the oldest raw event's time (unix ms): the trail, the log and a reading reach back to it.
	oldestTS int64
	days    map[int64]dayState
	lo, hi  int64 // the days loaded, inclusive
	loaded  bool
}

// planOf is the request's plan, made on first use for the whole of its period.
func (s *Server) planOf(ctx context.Context, q *query) (*plan, error) {
	if q.plan != nil {
		return q.plan, nil
	}
	pl := &plan{s: s, src: s.Source, site: q.site.ID, now: q.p.Now, days: map[int64]dayState{}}
	var oldest sql.NullInt64
	if err := s.row(ctx, `SELECT MIN(ts) FROM event WHERE site_id = ?`, q.site.ID).Scan(&oldest); err != nil {
		return nil, err
	}
	pl.oldestTS, pl.oldest, pl.hasRaw = oldest.Int64, oldest.Int64/dayMS, oldest.Valid
	first := minTime(q.p.Bounds[0], q.p.win.From)
	if q.p.Prev != nil {
		first = minTime(first, q.p.Prev[0])
	}
	if q.p.prevWin != nil {
		first = minTime(first, q.p.prevWin.From)
	}
	if err := pl.ensure(ctx, ms(first)/dayMS, ms(q.p.Now)/dayMS+1); err != nil {
		return nil, err
	}
	q.plan = pl
	return pl, nil
}

// ensure loads the agg_day rows of [first, last] it does not hold yet.
func (pl *plan) ensure(ctx context.Context, first, last int64) error {
	if pl.src == SourceRaw {
		return nil
	}
	load := func(a, b int64) error {
		if a > b {
			return nil
		}
		rows, err := pl.s.query(ctx, `SELECT day, frozen, dirty, raw FROM agg_day WHERE site_id = ? AND day >= ?
			AND day <= ?`, pl.site, a, b)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var d, dirty int64
			var st dayState
			if err := rows.Scan(&d, &st.frozen, &dirty, &st.raw); err != nil {
				return err
			}
			st.clean = dirty == 0
			pl.days[d] = st
		}
		return rows.Err()
	}
	if !pl.loaded {
		pl.lo, pl.hi, pl.loaded = first, last, true
		return load(first, last)
	}
	if err := load(first, pl.lo-1); err != nil {
		return err
	}
	if err := load(pl.hi+1, last); err != nil {
		return err
	}
	pl.lo, pl.hi = min(pl.lo, first), max(pl.hi, last)
	return nil
}

// rawFrom is from, or the oldest raw event when that is later: where a raw-only figure of a span begins.
func (pl *plan) rawFrom(from time.Time) time.Time {
	if pl.hasRaw && pl.oldestTS > ms(from) {
		return time.UnixMilli(pl.oldestTS)
	}
	return from
}

// detailSince is the API's detail_since: the oldest raw event, null without any.
func (pl *plan) detailSince() any {
	if !pl.hasRaw {
		return nil
	}
	return iso(time.UnixMilli(pl.oldestTS))
}

// rawOK: the day's raw is all there. Retention marks a day it deleted from (raw = 0); before the oldest event
// there is none.
func (pl *plan) rawOK(day int64) bool {
	st, ok := pl.days[day]
	return pl.hasRaw && day >= pl.oldest && (!ok || st.raw)
}

// rawCovers: no day touching [from, to) lost raw to retention (a span before the first event never had any, which
// is all there). Under every source: the raw-only reads (a template's paths) ask it.
func (pl *plan) rawCovers(ctx context.Context, from, to time.Time) (bool, error) {
	var cut bool
	err := pl.s.row(ctx, `SELECT EXISTS (SELECT 1 FROM agg_day WHERE site_id = ? AND day >= ? AND day <= ? AND raw = 0)`,
		pl.site, ms(from)/dayMS, (ms(to)-1)/dayMS).Scan(&cut)
	return !cut, err
}

// pick is the source of the piece [lo, hi). unit is the table's grain (an hour, a day): a piece not on it is no
// aggregate's and is read raw. short marks a 24h or 7d range and its prior period: raw first.
func (pl *plan) pick(ctx context.Context, lo, hi time.Time, unit time.Duration, short bool) (how, error) {
	if pl.src == SourceRaw {
		return readRaw, nil
	}
	if !lo.Before(pl.now) { // the open bucket's future: nothing is there yet
		return readNone, nil
	}
	if !lo.Equal(lo.Truncate(unit)) || !hi.Equal(hi.Truncate(unit)) {
		return readRaw, nil
	}
	day := ms(lo) / dayMS
	if err := pl.ensure(ctx, day, day); err != nil {
		return readNone, err
	}
	st, ok := pl.days[day]
	aggOK, rawOK := ok && st.frozen && st.clean, pl.rawOK(day)
	switch {
	case pl.src == SourceAgg && aggOK:
		return readAgg, nil
	case pl.src == SourceAgg && rawOK:
		return readNone, fmt.Errorf("%w: site %d, %s", ErrNoAggregates, pl.site,
			time.UnixMilli(day*dayMS).UTC().Format(time.DateOnly))
	case pl.src == SourceAgg:
		return readNone, nil
	case short && rawOK:
		return readRaw, nil
	case aggOK:
		return readAgg, nil
	case rawOK:
		return readRaw, nil
	}
	return readNone, nil
}

// read reads the piece [lo, hi) from its source: fromAgg answers false when the day was no longer clean when it read
// (a rebuild under way), and the piece falls back as pick would without the aggregates.
func (pl *plan) read(ctx context.Context, lo, hi time.Time, unit time.Duration, short bool,
	fromAgg func() (bool, error), fromRaw func() error) error {
	h, err := pl.pick(ctx, lo, hi, unit, short)
	if err != nil {
		return err
	}
	if h == readAgg {
		ok, err := fromAgg()
		if err != nil || ok {
			pl.s.traced(true, true)
			return err
		}
		day := ms(lo) / dayMS
		switch {
		case pl.src == SourceAgg:
			return fmt.Errorf("%w: site %d, %s rebuilt while read", ErrNoAggregates, pl.site,
				time.UnixMilli(day*dayMS).UTC().Format(time.DateOnly))
		case !pl.rawOK(day):
			return nil
		}
		h = readRaw
	}
	if h != readRaw {
		return nil
	}
	pl.s.traced(false, lo.Equal(lo.Truncate(unit)) && hi.Equal(hi.Truncate(unit)))
	return fromRaw()
}

// aggFrom is the FROM clause of an aggregate read of one day: the day's agg_day row (?1 site, ?2 day), and the
// table's rows matching on (t, the further placeholders from ?3) left-joined to it. Every row carries ok, the day
// still frozen and clean in the statement's own snapshot: a rebuild's half-written rows are never merged. No row at
// all is a day without a row.
func aggFrom(table, on string) string {
	return ` FROM (SELECT frozen = 1 AND dirty = 0 AS ok FROM agg_day WHERE site_id = ?1 AND day = ?2) AS d
		LEFT JOIN ` + table + ` t ON t.site_id = ?1 AND ` + on
}
