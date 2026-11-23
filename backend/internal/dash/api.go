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

// apiBucket reads one bucket's API figures, a piece at a time, each from its source.
func (s *Server) apiBucket(ctx context.Context, pl *plan, aud string, from, to time.Time, short bool, a *apiStats) error {
	pl.need = apiBuilder
	defer func() { pl.need = 0 }()
	for _, pc := range pieces(from, to) {
		if err := pl.read(ctx, pc[0], pc[1], time.Hour, short,
			func() (bool, error) { return s.apiHours(ctx, pl.site, aud, pc[0], pc[1], a) },
			func() error { return s.apiPiece(ctx, pl.site, aud, pc[0], pc[1], a) }); err != nil {
			return err
		}
	}
	return nil
}

// apiHours adds one piece's hours from agg_hour, false when its day is not clean in the statement's snapshot.
func (s *Server) apiHours(ctx context.Context, site int64, aud string, from, to time.Time, a *apiStats) (bool, error) {
	rows, err := s.query(ctx, `SELECT d.ok, t.cls, t.failed, t.durations, t.callers, t.accounts`+
		aggFrom("agg_hour", "t.hour >= ?3 AND t.hour < ?4"), site, ms(from)/dayMS, ms(from)/hourMS, ms(to)/hourMS)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	in := audCls(aud)
	var add apiStats
	ok := false
	for rows.Next() {
		var cls, failed sql.NullInt64
		var durations, callers, accounts []byte
		if err := rows.Scan(&ok, &cls, &failed, &durations, &callers, &accounts); err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		if !cls.Valid || !in(int(cls.Int64)) {
			continue
		}
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
	a.add(&add)
	return true, nil
}

// apiPiece adds one raw piece.
func (s *Server) apiPiece(ctx context.Context, site int64, aud string, from, to time.Time, a *apiStats) error {
	where := ` FROM event WHERE site_id = ? AND ts >= ? AND ts < ? AND ` + callWhere + ` AND ` + audWhere(aud)
	args := []any{site, ms(from), ms(to)}
	if err := s.each(ctx, `SELECT dur_ms, SUM(`+failedSQL+`), COUNT(*)`+where+` GROUP BY dur_ms`, args,
		func(rows *sql.Rows) error {
			var dur, failed, n int64
			if err := rows.Scan(&dur, &failed, &n); err != nil {
				return err
			}
			a.failed += float64(failed)
			a.durations.Add(dur, n)
			return nil
		}); err != nil {
		return err
	}
	for _, d := range []struct {
		col string
		to  *uniq
	}{{"ip_key", &a.callers}, {"account", &a.accounts}} {
		if err := s.each(ctx, `SELECT DISTINCT `+d.col+where+` AND `+d.col+` != ''`, args, func(rows *sql.Rows) error {
			var v string
			if err := rows.Scan(&v); err != nil {
				return err
			}
			d.to.add(v)
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
		of: func(b bucketStats) float64 { return float64(b.api.accounts.count()) }},
	{key: "callers", label: "Addresses", kind: "sum", server: true,
		of: func(b bucketStats) float64 { return float64(b.api.callers.count()) }},
	{key: "failed", label: "Failed", unit: "%", kind: "rate", server: true, upBad: true,
		of:   func(b bucketStats) float64 { return b.api.failedShare() },
		base: func(b bucketStats) float64 { return b.api.calls() }},
	{key: "p95", label: "95th percentile", unit: "ms", kind: "sum", server: true, upBad: true,
		of: func(b bucketStats) float64 { return b.api.p95() }},
}

// apiTotal is an API tile's figure over buckets.
func apiTotal(key string, bs []bucketStats) float64 {
	var all apiStats
	for i := range bs {
		all.add(&bs[i].api)
	}
	switch key {
	case "accounts":
		return float64(all.accounts.count())
	case "callers":
		return float64(all.callers.count())
	case "failed":
		return round1(all.failedShare())
	case "p95":
		return all.p95()
	}
	return all.calls()
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
