package dash

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/trick77/stakeout/backend/internal/store"
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
	s      *Server
	src    Source
	site   int64
	now    time.Time
	oldest int64 // the oldest raw event's day
	hasRaw bool
	// oldestTS is the oldest raw event's time (unix ms): the trail, the log and a reading reach back to it.
	oldestTS int64
	days     map[int64]dayState
	lo, hi   int64 // the days loaded, inclusive
	loaded   bool
	// gaps counts the pieces read as nothing although they held events: raw only (off the aggregates' grain) and
	// their day's raw cut by retention. A figure whose reads added one is short; a change against it is unknown.
	gaps int
	// older counts the gaps that are days an older builder made without the figure read (need), their raw gone.
	older int
	// need is the builder a day's aggregates must come from for what is being read (0: any): a figure an older build
	// does not hold would read as nothing.
	need int64
	api  bool // the site is shown as an API
}

// planOf is the request's plan, made on first use for the whole of its period.
func (s *Server) planOf(ctx context.Context, q *query) (*plan, error) {
	if q.plan != nil {
		return q.plan, nil
	}
	pl := &plan{s: s, src: s.Source, site: q.site.ID, now: q.p.Now, days: map[int64]dayState{}, api: q.api()}
	oldest, ok, err := s.rawStart(ctx, q.site.ID)
	if err != nil {
		return nil, err
	}
	pl.oldestTS, pl.oldest, pl.hasRaw = oldest, oldest/dayMS, ok
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
		rows, err := pl.s.query(ctx, `SELECT day, frozen, dirty, raw, builder FROM agg_day WHERE site_id = ? AND day >= ?
			AND day <= ?`, pl.site, a, b)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var d, dirty int64
			var st dayState
			if err := rows.Scan(&d, &st.frozen, &dirty, &st.raw, &st.builder); err != nil {
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

// rawStart is where a site's raw begins for the dashboard (ok false: none): its first event after the newest day
// retention deleted raw from (agg_day.raw = 0), the oldest event when none was cut. Leftovers before that day (a
// cut stopped part-way, an older day the purge has not reached) are no detail anyone can read a span of: the oldest
// stray event would move the horizon back over the purged gap. The cut day is looked for back from today, over the
// months (the raw days' rows, a few pages, on the primary key).
func (s *Server) rawStart(ctx context.Context, site int64) (int64, bool, error) {
	now := s.now()
	var cut sql.NullInt64
	if err := s.row(ctx, `SELECT day FROM agg_day WHERE site_id = ? AND day >= ? AND day <= ? AND raw = 0
		ORDER BY day DESC LIMIT 1`, site, now.AddDate(0, -s.RetentionMonths, -1).UnixMilli()/dayMS,
		now.UnixMilli()/dayMS+1).Scan(&cut); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	from := int64(0)
	if cut.Valid {
		from = (cut.Int64 + 1) * dayMS
	}
	var ts sql.NullInt64
	if err := s.row(ctx, `SELECT MIN(ts) FROM event WHERE site_id = ? AND ts >= ?`, site, from).Scan(&ts); err != nil {
		return 0, false, err
	}
	return ts.Int64, ts.Valid, nil
}

// rawFrom is from, or the oldest raw event when that is later: where a raw-only figure of a span begins.
func (pl *plan) rawFrom(from time.Time) time.Time {
	if pl.hasRaw && pl.oldestTS > ms(from) {
		return time.UnixMilli(pl.oldestTS)
	}
	return from
}

// detailSince is the API's detail_since, what a raw-only read of [from, …) says about its start: where raw begins
// (the oldest event; now when none is left) when its UTC day is after from's (the reads' own day bounds, not the
// viewer's dates) and the figures reach further back (an hour of aggregates before it); null otherwise, nothing
// missing: raw covers the span, or the site's figures begin where its raw does (a site younger than the range).
func (pl *plan) detailSince(ctx context.Context, from time.Time) (any, error) {
	start := pl.oldestTS
	if !pl.hasRaw {
		start = ms(pl.now)
	}
	if start/dayMS <= ms(from)/dayMS {
		return nil, nil
	}
	first, ok, err := pl.s.firstFigure(ctx, pl.site)
	if err != nil || !ok || first >= start/hourMS {
		return nil, err
	}
	return iso(time.UnixMilli(start)), nil
}

// firstFigure is the oldest hour of a site's aggregates on a day they can be read for (ok false: none). A day the
// size cap cut (AggWriting) has none: its hours left are skipped, every cut day's agg_day row walked once, in key
// order.
func (s *Server) firstFigure(ctx context.Context, site int64) (int64, bool, error) {
	for from := int64(0); ; {
		var hour sql.NullInt64
		if err := s.row(ctx, `SELECT MIN(hour) FROM agg_hour WHERE site_id = ? AND hour >= ?`, site, from*24).
			Scan(&hour); err != nil || !hour.Valid {
			return 0, false, err
		}
		day := hour.Int64 / 24
		var dirty int
		if err := s.row(ctx, `SELECT COALESCE(MAX(dirty), 0) FROM agg_day WHERE site_id = ? AND day = ?`, site, day).
			Scan(&dirty); err != nil || dirty != store.AggWriting {
			return hour.Int64, err == nil, err
		}
		var next sql.NullInt64
		if err := s.row(ctx, `SELECT (SELECT day FROM agg_day WHERE site_id = ? AND day > ? AND dirty != ? ORDER BY day
			LIMIT 1)`, site, day, store.AggWriting).Scan(&next); err != nil || !next.Valid {
			return 0, false, err
		}
		from = next.Int64
	}
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

// pick is the source of the piece [lo, hi). unit is the table's grain (an hour, a day; 0: onGrain): a piece not on it is no
// aggregate's and is read raw; with its day's raw cut by retention nothing can answer it, and it counts as a gap
// (never an empty piece taken for a quiet one). short marks a 24h or 7d range and its prior period: raw first.
func (pl *plan) pick(ctx context.Context, lo, hi time.Time, unit time.Duration, short bool) (how, error) {
	if pl.src == SourceRaw {
		return readRaw, nil
	}
	if !lo.Before(pl.now) { // the open bucket's future: nothing is there yet
		return readNone, nil
	}
	day := ms(lo) / dayMS
	if err := pl.ensure(ctx, day, day); err != nil {
		return readNone, err
	}
	st, ok := pl.days[day]
	if !onGrain(lo, hi, unit) {
		if ok && !st.raw {
			pl.gaps++
			return readNone, nil
		}
		return readRaw, nil
	}
	aggOK, rawOK := ok && st.frozen && st.clean && st.builder >= pl.need, pl.rawOK(day)
	if ok && !aggOK && !rawOK && st.builder < pl.need { // its raw is gone and its build never held the figure
		pl.gaps++
		pl.older++
	}
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
	pl.s.traced(false, onGrain(lo, hi, unit))
	return fromRaw()
}

// onGrain: a table of that grain can serve [lo, hi); none has grain 0 (what is read is finer than any table).
func onGrain(lo, hi time.Time, unit time.Duration) bool {
	return unit > 0 && lo.Equal(lo.Truncate(unit)) && hi.Equal(hi.Truncate(unit))
}

// aggFrom is the FROM clause of an aggregate read of one day: the day's agg_day row (?1 site, ?2 day), and the
// table's rows matching on (t, the further placeholders from ?3) left-joined to it. Every row carries ok, the day
// still frozen and clean in the statement's own snapshot: a rebuild's half-written rows are never merged. No row at
// all is a day without a row.
func aggFrom(table, on string) string {
	return ` FROM (SELECT frozen = 1 AND dirty = 0 AS ok FROM agg_day WHERE site_id = ?1 AND day = ?2) AS d
		LEFT JOIN ` + table + ` t ON t.site_id = ?1 AND ` + on
}
