package dash

import (
	"context"
	"database/sql"
	"math"
	"time"

	"github.com/trick77/stakeout/backend/internal/agg"
)

// The figures of an API (a site of kind api with a feed): its server requests as calls of endpoints.

// callWhere is a call in SQL (agg.Called): a server request that is no 404 and was not refused without a route.
const callWhere = `source = 'server' AND status != 404 AND NOT (unrouted = 1 AND status IN (401, 403, 405))`

// failedSQL is a failed call in SQL (agg.Failure): the product said so, or the status does.
const failedSQL = `(err != '' OR status >= 400)`

// endpointSQL is a call's endpoint in SQL (agg.Endpoint).
const endpointSQL = `CASE WHEN op = '' THEN template ELSE template || ' ' || op END`

// failureSQL is why a failed call failed in SQL (agg.Failure).
const failureSQL = `CASE WHEN err != '' THEN err ELSE 'HTTP ' || status END`

// apiBuilder is the first agg.Version whose days hold these figures: a day an older one built is read raw until it
// is rebuilt.
const apiBuilder = 3

// api reports whether the site is shown as an API.
func (q *query) api() bool { return q.site.Kind == "api" && q.site.HasFeed }

// apiStats are a bucket's API figures.
//
// Table: -
type apiStats struct {
	failed            float64
	callers, accounts uniq
	durations         agg.Hist // one per call
}

func (a *apiStats) calls() float64 { return float64(a.durations.Count()) }

func (a *apiStats) add(o *apiStats) {
	a.failed += o.failed
	a.callers.merge(&o.callers)
	a.accounts.merge(&o.accounts)
	a.durations.Merge(&o.durations)
}

// failedShare is the percent of calls that failed, NaN without a call.
func (a *apiStats) failedShare() float64 {
	if a.calls() == 0 {
		return math.NaN()
	}
	return pct(a.failed, a.calls())
}

// p95 is the 95th percentile duration in ms, NaN without a call: nothing took no time.
func (a *apiStats) p95() float64 {
	v, ok := a.durations.Percentile(95)
	if !ok {
		return math.NaN()
	}
	return float64(v)
}

// apiSlots reads the slots' API figures into out, a piece at a time, each from its source.
func (s *Server) apiSlots(ctx context.Context, pl *plan, aud string, sl slotting, short bool, out []bucketStats) error {
	pl.need = apiBuilder
	defer func() { pl.need = 0 }()
	for _, pc := range pieces(sl.bounds[0], sl.bounds[sl.n()]) {
		if err := pl.read(ctx, pc[0], pc[1], sl.grain, short,
			func() (bool, error) { return s.apiHours(ctx, pl.site, aud, pc[0], pc[1], sl, out) },
			func() error { return s.apiPiece(ctx, pl.site, aud, pc[0], pc[1], sl, out) }); err != nil {
			return err
		}
	}
	return nil
}

// apiHours adds one piece's hours from agg_hour, false when its day is not clean in the statement's snapshot (nothing
// added then).
func (s *Server) apiHours(ctx context.Context, site int64, aud string, from, to time.Time, sl slotting,
	out []bucketStats) (bool, error) {
	rows, err := s.query(ctx, `SELECT d.ok, t.hour, t.cls, t.failed, t.durations, t.callers, t.accounts`+
		aggFrom("agg_hour", "t.hour >= ?3 AND t.hour < ?4"), site, ms(from)/dayMS, ms(from)/hourMS, ms(to)/hourMS)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	in := audCls(aud)
	adds := make([]apiStats, len(out))
	ok := false
	for rows.Next() {
		var hour, cls, failed sql.NullInt64
		var durations, callers, accounts []byte
		if err := rows.Scan(&ok, &hour, &cls, &failed, &durations, &callers, &accounts); err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		if !cls.Valid || !in(int(cls.Int64)) {
			continue
		}
		add := &adds[sl.at(hour.Int64*hourMS)]
		add.failed += float64(failed.Int64)
		if err := add.durations.AddEncoded(durations); err != nil {
			return false, err
		}
		if err := add.callers.addSketch(callers); err != nil {
			return false, err
		}
		if err := add.accounts.addSketch(accounts); err != nil {
			return false, err
		}
	}
	if err := rows.Err(); err != nil || !ok {
		return false, err
	}
	for i := range adds {
		out[i].api.add(&adds[i])
	}
	return true, nil
}

// apiPiece adds one raw piece.
func (s *Server) apiPiece(ctx context.Context, site int64, aud string, from, to time.Time, sl slotting,
	out []bucketStats) error {
	where := ` FROM event WHERE site_id = ? AND ts >= ? AND ts < ? AND ` + callWhere + ` AND ` + audWhere(aud)
	args := append(sl.args(), site, ms(from), ms(to))
	if err := s.each(ctx, `SELECT `+cellSQL+` AS cell, dur_ms, SUM(`+failedSQL+`), COUNT(*)`+where+` GROUP BY cell, dur_ms`,
		args, func(rows *sql.Rows) error {
			var cell, dur, failed, n int64
			if err := rows.Scan(&cell, &dur, &failed, &n); err != nil {
				return err
			}
			a := &out[sl.slot(cell)].api
			a.failed += float64(failed)
			a.durations.Add(dur, n)
			return nil
		}); err != nil {
		return err
	}
	for _, d := range []struct {
		col string
		to  func(*apiStats) *uniq
	}{{"ip_key", func(a *apiStats) *uniq { return &a.callers }}, {"account", func(a *apiStats) *uniq { return &a.accounts }}} {
		if err := s.each(ctx, `SELECT DISTINCT `+cellSQL+`, `+d.col+where+` AND `+d.col+` != ''`, args, func(rows *sql.Rows) error {
			var cell int64
			var v string
			if err := rows.Scan(&cell, &v); err != nil {
				return err
			}
			d.to(&out[sl.slot(cell)].api).add(v)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// each runs a query and f on every row.
func (s *Server) each(ctx context.Context, stmt string, args []any, f func(*sql.Rows) error) error {
	rows, err := s.query(ctx, stmt, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := f(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// apiTileDefs are the overview's tiles for an API.
var apiTileDefs = []tileDef{
	{key: "calls", label: "Calls", kind: "sum", server: true, of: func(b bucketStats) float64 { return b.api.calls() }},
	{key: "accounts", label: "Accounts", kind: "sum", server: true,
		distinct: func(b *bucketStats) *uniq { return &b.api.accounts }},
	{key: "callers", label: "Addresses", kind: "sum", server: true,
		distinct: func(b *bucketStats) *uniq { return &b.api.callers }},
	{key: "failed", label: "Failed", unit: "%", kind: "rate", server: true, upBad: true,
		pool: func(bs []bucketStats) float64 {
			var a apiStats
			for i := range bs {
				a.failed += bs[i].api.failed
				a.durations.Merge(&bs[i].api.durations)
			}
			return round1(a.failedShare())
		},
		base: func(b bucketStats) float64 { return b.api.calls() }},
	{key: "p95", label: "95th percentile", unit: "ms", kind: "sum", server: true, upBad: true,
		pool: func(bs []bucketStats) float64 {
			var a apiStats
			for i := range bs {
				a.durations.Merge(&bs[i].api.durations)
			}
			return a.p95()
		}},
}

// scanDurations adds a raw piece's durations per key to into: a list whose rows carry percentiles.
func (s *Server) scanDurations(ctx context.Context, q *query, sc scan, from, to time.Time, into *listSums) error {
	args := append([]any{q.site.ID, ms(from), ms(to)}, sc.args...)
	return s.each(ctx, `SELECT `+sc.key+`, dur_ms, COUNT(*) FROM event WHERE site_id = ? AND ts >= ? AND ts < ? AND `+
		sc.where+` AND `+audWhere(q.aud)+sc.extra+` GROUP BY `+sc.key+`, dur_ms`, args, func(rows *sql.Rows) error {
		var key string
		var dur, n int64
		if err := rows.Scan(&key, &dur, &n); err != nil {
			return err
		}
		into.get(key).durations.Add(dur, n)
		return nil
	})
}
