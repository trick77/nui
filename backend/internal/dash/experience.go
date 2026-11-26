package dash

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/trick77/trakk/backend/internal/agg"
	"github.com/trick77/trakk/backend/internal/apix"
)

// What people experienced: their engaged time and the Web Vitals, always of people (class human, the lists' part 1)
// whatever the audience. From the days builder 4 made, or raw through agg/vital.go's fragments: both take the same
// rows the same way.

// expBuilder is the first agg.Version whose days hold these figures.
const expBuilder = 4

// clsUnits are CLS's histogram units per unit of its score (agg's scale, pinned by TestExperienceCLSUnits).
const clsUnits = 1000

// expSums are a span's figures.
type expSums struct {
	starts, engaged int64
	vitals          [len(agg.Vitals)]agg.Hist
}

func (e *expSums) add(o *expSums) {
	e.starts += o.starts
	e.engaged += o.engaged
	for i := range e.vitals {
		e.vitals[i].Merge(&o.vitals[i])
	}
}

// expHourCols are agg_hour's histograms in agg.Vitals' order.
var expHourCols = func() string {
	cols := make([]string, len(agg.Vitals))
	for i, v := range agg.Vitals {
		cols[i] = "t.v_" + strings.ToLower(v)
	}
	return strings.Join(cols, ", ")
}()

// expSpan reads [from, to) a piece at a time, each from its source. A piece a builder older than expBuilder made,
// its raw gone, starts the figures over: since is its end. gap: a piece was read as nothing (plan.gaps).
func (s *Server) expSpan(ctx context.Context, pl *plan, from, to time.Time, short bool) (sums expSums, since time.Time,
	gap bool, err error) {
	gaps := pl.gaps
	for _, pc := range pieces(from, to) {
		older := pl.older
		var add expSums
		if err := pl.read(ctx, pc[0], pc[1], time.Hour, short,
			func() (bool, error) { return s.expHours(ctx, pl.site, pc[0], pc[1], &add) },
			func() error { return s.expPiece(ctx, pl.site, pc[0], pc[1], &add) }); err != nil {
			return sums, since, false, err
		}
		if pl.older > older {
			sums, since = expSums{}, pc[1]
			continue
		}
		sums.add(&add)
	}
	return sums, since, pl.gaps > gaps, nil
}

// expHours adds one piece's people's hours from agg_hour, false when its day is not clean in the statement's snapshot
// (nothing added then).
func (s *Server) expHours(ctx context.Context, site int64, from, to time.Time, into *expSums) (bool, error) {
	rows, err := s.query(ctx, `SELECT d.ok, t.starts, t.engaged, `+expHourCols+
		aggFrom("agg_hour", "t.hour >= ?3 AND t.hour < ?4 AND t.cls = 1"), site, ms(from)/dayMS, ms(from)/hourMS,
		ms(to)/hourMS)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	var add expSums
	ok := false
	for rows.Next() {
		var starts, engaged sql.NullInt64
		var v [len(agg.Vitals)][]byte
		dest := []any{&ok, &starts, &engaged}
		for i := range v {
			dest = append(dest, &v[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		add.starts += starts.Int64
		add.engaged += engaged.Int64
		for i := range v {
			if err := add.vitals[i].AddEncoded(v[i]); err != nil {
				return false, err
			}
		}
	}
	if err := rows.Err(); err != nil || !ok {
		return false, err
	}
	into.add(&add)
	return true, nil
}

// expPiece adds one raw piece: its people's vitals by value, as agg.VitalUnits takes them, and their visits and
// engaged time.
func (s *Server) expPiece(ctx context.Context, site int64, from, to time.Time, into *expSums) error {
	args := []any{site, ms(from), ms(to)}
	if err := s.each(ctx, `SELECT name, `+agg.VitalVerSQL+`, `+agg.VitalValueSQL+`, COUNT(*) FROM event
		WHERE site_id = ? AND ts >= ? AND ts < ? AND kind = 'vital' AND cls = 1 GROUP BY 1, 2, 3`, args,
		func(rows *sql.Rows) error {
			var name string
			var ver, value any
			var n int64
			if err := rows.Scan(&name, &ver, &value, &n); err != nil {
				return err
			}
			if i, u, ok := agg.VitalUnits(name, ver, value); ok {
				into.vitals[i].Add(u, n)
			}
			return nil
		}); err != nil {
		return err
	}
	var starts, engaged int64
	if err := s.row(ctx, `SELECT COALESCE(SUM(kind = 'session_start'), 0), COALESCE(SUM(`+agg.EngagedSQL+`), 0)
		FROM event WHERE site_id = ? AND ts >= ? AND ts < ? AND kind IN ('session_start', 'engagement') AND cls = 1`,
		args...).Scan(&starts, &engaged); err != nil {
		return err
	}
	into.starts += starts
	into.engaged += engaged
	return nil
}

// p75 is a histogram's 75th percentile in its vital's unit (ms; CLS its score), NaN without a sample.
func p75(name string, h *agg.Hist) float64 {
	u, ok := h.Percentile(75)
	switch {
	case !ok:
		return math.NaN()
	case name == "CLS":
		return float64(u) / clsUnits
	}
	return float64(u)
}

// perVisit is the engaged time per visit in whole ms, NaN without a visit.
func (e *expSums) perVisit() float64 {
	if e.starts == 0 {
		return math.NaN()
	}
	return math.Round(float64(e.engaged) / float64(e.starts))
}

func orNull(v float64) *float64 {
	if math.IsNaN(v) {
		return nil
	}
	return &v
}

type expVital struct {
	Name string `json:"name"`
	// P75 is the range's 75th percentile (ms; CLS its score), N its samples, Prev the comparison's: null without one.
	P75     *float64 `json:"p75"`
	N       int64    `json:"n"`
	Prev    *float64 `json:"prev"`
	Change  change   `json:"change"`
	Verdict string   `json:"verdict"`
}

type expResp struct {
	// Since is set when the figures begin only within the range (days an older builder made, their raw gone).
	Since  any   `json:"since"`
	Visits int64 `json:"visits"`
	// EngagedMS is the engaged time per visit.
	EngagedMS     *float64   `json:"engaged_ms"`
	EngagedChange change     `json:"engaged_change"`
	Vitals        []expVital `json:"vitals"`
}

// peopleOnly answers 404 for what people experienced on an API: no pages there.
func peopleOnly(w http.ResponseWriter, q *query) bool {
	if !q.api() {
		return false
	}
	apix.Error(w, http.StatusNotFound, fmt.Sprintf("%s is an API: it has no pages", q.site.Domain))
	return true
}

func (s *Server) handleExperience(w http.ResponseWriter, r *http.Request, q *query) {
	if peopleOnly(w, q) {
		return
	}
	resp, err := s.experience(r.Context(), q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	apix.JSON(w, http.StatusOK, resp)
}

// experience reads the range up to now and its comparison up to the same point, as the tiles do. The plan's need,
// gaps and older are its own: no other figure of the request moves by them. A gap in the range leaves every change
// unknown, one in the comparison its figures too; a change is of two figures that exist, never against a p75 of 0
// (a vital up from 0 is bad without one).
func (s *Server) experience(ctx context.Context, q *query) (expResp, error) {
	var resp expResp
	pl, err := s.planOf(ctx, q)
	if err != nil {
		return resp, err
	}
	need, gaps, older := pl.need, pl.gaps, pl.older
	pl.need = expBuilder
	defer func() { pl.need, pl.gaps, pl.older = need, gaps, older }()
	cur, since, curGap, err := s.expSpan(ctx, pl, q.p.Bounds[0], q.p.Bounds[q.p.n()], !q.p.agg)
	if err != nil {
		return resp, err
	}
	var prev *expSums
	if q.p.Prev != nil {
		p, _, prevGap, err := s.expSpan(ctx, pl, q.p.Prev[0], q.p.prevCut, !q.p.prevAgg)
		if err != nil {
			return resp, err
		}
		if !prevGap {
			prev = &p
		}
	}
	cmpOn := prev != nil && !curGap
	compare := func(v, pv float64) change {
		if math.IsNaN(v) || math.IsNaN(pv) || pv == 0 {
			return nil
		}
		return sumChange(v, pv, cmpOn)
	}
	// A vital up from a p75 of 0 (CLS often is) has no percentage, but it got worse.
	judge := func(c change, v, pv float64) string {
		if c == nil && cmpOn && pv == 0 && v > 0 {
			return "bad"
		}
		return verdict(c, true)
	}
	resp.Since, resp.Visits = iso(since), cur.starts
	engaged := cur.perVisit()
	resp.EngagedMS = orNull(engaged)
	if prev != nil {
		resp.EngagedChange = compare(engaged, prev.perVisit())
	}
	resp.Vitals = make([]expVital, len(agg.Vitals))
	for i, name := range agg.Vitals {
		v := expVital{Name: name, N: cur.vitals[i].Count()}
		x, px := p75(name, &cur.vitals[i]), math.NaN()
		v.P75 = orNull(x)
		if prev != nil {
			px = p75(name, &prev.vitals[i])
			v.Prev = orNull(px)
			v.Change = compare(x, px)
		}
		v.Verdict = judge(v.Change, x, px)
		resp.Vitals[i] = v
	}
	return resp, nil
}

// expScans are the experience list's dimensions: the engaged time and page views, then LCP, INP and CLS. Builder 4's
// rows, or raw through agg/vital.go's fragments.
func expScans() []scan {
	out := []scan{{dimSpec: dimSpec{where: "kind IN ('page_view', 'engagement')", key: "template",
		a: "SUM(" + agg.EngagedSQL + ")", b: "SUM(kind = 'page_view')", need: expBuilder}, dim: "engaged"}}
	for _, name := range agg.Vitals[:3] {
		out = append(out, scan{dimSpec: dimSpec{where: "kind = 'vital' AND name = '" + name + "'", key: "template",
			a: "0", b: "0", need: expBuilder, durations: true, sample: []string{agg.VitalVerSQL, agg.VitalValueSQL},
			units: func(vals []any) (int64, bool) {
				_, u, ok := agg.VitalUnits(name, vals[0], vals[1])
				return u, ok
			}}, dim: "vital_" + strings.ToLower(name)})
	}
	return out
}

type expRow struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Views int64  `json:"views"`
	// Engaged is the engaged time per page view (ms), LCP, INP and CLS the p75s, Loads LCP's samples: null without
	// samples, or where the aggregates hold only part of them (the page in a day's Other).
	Engaged *float64 `json:"engaged"`
	LCP     *float64 `json:"lcp"`
	INP     *float64 `json:"inp"`
	CLS     *float64 `json:"cls"`
	Loads   *int64   `json:"loads"`
}

type expListResp struct {
	Rows []expRow `json:"rows"`
	// Other is the rows past the limit and the pages only Other rows hold: their views. From the aggregates its count,
	// and so Total, is an upper bound: a day's Other of the engaged time also lists a page with engagement rows only,
	// which raw shows as no row.
	Other other `json:"other"`
	Total int   `json:"total"`
	Since any   `json:"since"`
}

// handleExperienceList is dim=experience: limit and q as other lists; volume (page views) the only order, no change.
func (s *Server) handleExperienceList(w http.ResponseWriter, r *http.Request, q *query) {
	if peopleOnly(w, q) {
		return
	}
	v := r.URL.Query()
	limit, err := intParam(v.Get("limit"), 6, 1, maxLimit)
	if err != nil {
		apix.Error(w, http.StatusBadRequest, "bad limit")
		return
	}
	if sort := v.Get("sort"); sort != "" && sort != "volume" {
		apix.Error(w, http.StatusBadRequest, "sort must be volume for dim=experience")
		return
	}
	for _, k := range []string{"within", "sub", "cat"} {
		if v.Get(k) != "" {
			apix.Error(w, http.StatusBadRequest, k+" is not for dim=experience")
			return
		}
	}
	resp, err := s.experienceList(r.Context(), q, limit, foldSpaces(strings.ToLower(v.Get("q"))))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	apix.JSON(w, http.StatusOK, resp)
}

// experienceList is the pages by what people experienced on them over the range, by page views. A figure is null
// where a page sits in that list's Other on some day: never a partial p75. Its views are then a lower bound, as any
// list's value; total and other.count an upper bound (expListResp). needle is lower-case, spaces folded.
func (s *Server) experienceList(ctx context.Context, q *query, limit int, needle string) (expListResp, error) {
	resp := expListResp{Rows: []expRow{}}
	if _, err := s.planOf(ctx, q); err != nil {
		return resp, err
	}
	people := *q // the plan shared
	people.aud = "humans"
	scans := expScans()
	sums := make([]*listSums, len(scans))
	var since time.Time
	for i, sc := range scans {
		sums[i] = newSums()
		if err := s.scanDim(ctx, &people, sc, q.p.win, !q.p.agg, sums[i]); err != nil {
			return resp, err
		}
		if sums[i].since.After(since) {
			since = sums[i].since
		}
	}
	resp.Since = iso(since)
	eng := sums[0]
	keys := map[string]bool{}
	for _, l := range sums {
		for k := range l.m {
			keys[k] = true
		}
	}
	var rows []expRow
	for k := range keys {
		row := expRow{Key: k, Label: pageLabel(k)}
		samples := false
		if g, ok := eng.m[k]; ok {
			row.Views = g.b
			if g.b > 0 && !eng.other.hides(k) {
				e := math.Round(float64(g.a) / float64(g.b))
				row.Engaged = &e
			}
		}
		for j, cell := range []**float64{&row.LCP, &row.INP, &row.CLS} {
			l := sums[1+j]
			var h agg.Hist
			if g, ok := l.m[k]; ok {
				h = g.durations
			}
			samples = samples || h.Count() > 0
			if l.other.hides(k) {
				continue
			}
			*cell = orNull(p75(agg.Vitals[j], &h))
			if j == 0 {
				n := h.Count()
				row.Loads = &n
			}
		}
		if row.Views == 0 && !samples {
			continue
		}
		if needle != "" && !strings.Contains(foldSpaces(strings.ToLower(row.Key)), needle) &&
			!strings.Contains(foldSpaces(strings.ToLower(row.Label)), needle) {
			continue
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b expRow) int {
		return cmp.Or(cmp.Compare(b.Views, a.Views), cmp.Compare(a.Key, b.Key))
	})
	resp.Total = len(rows)
	// The pages only the engaged list's Other rows hold, as list() counts them.
	if co := &eng.other; !co.empty() && needle == "" {
		hidden := int64(len(co.hashes))
		for _, r := range rows {
			if _, ok := co.hashes[agg.Hash(r.Key)]; ok {
				hidden--
			}
		}
		if co.partial {
			hidden = max(hidden, co.unlisted-int64(len(rows)))
		}
		resp.Other.Count += int(hidden)
		resp.Total += int(hidden)
		resp.Other.Value += co.b
	}
	for i, row := range rows {
		if i >= limit {
			resp.Other.Count++
			resp.Other.Value += row.Views
			continue
		}
		resp.Rows = append(resp.Rows, row)
	}
	return resp, nil
}
