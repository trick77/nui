package dash

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/stakeout/backend/internal/agg"
	"github.com/trick77/stakeout/backend/internal/apix"
	"github.com/trick77/stakeout/backend/internal/classify"
	"github.com/trick77/stakeout/backend/internal/store"
)

// dimSpec is one list: which events, grouped by what, counting what. a and b are SQL sums per group; distinct, when
// set, is counted across buckets in Go (a set per key), since distinct counts do not add up.
type dimSpec struct {
	where    string
	key      string
	sub      string
	a, b     string
	distinct string
	server   bool
	upBad    bool
	// api marks the figures of calls: they need a day's aggregates from apiBuilder on. durations: rows carry
	// percentiles. failedB: b counts the failed calls.
	api, durations, failedB bool
	// name turns the SQL key into the row's, where that is the server's to say: rows of one name are one.
	name func(string) string
	// value and second pick the row's figures from the sums and the distinct count.
	value, second func(a, b, d int64) int64
}

func countA(a, _, _ int64) int64     { return a }
func countB(_, b, _ int64) int64     { return b }
func countD(_, _, d int64) int64     { return d }
func zeroSecond(_, _, _ int64) int64 { return 0 }

var dims = map[string]dimSpec{
	"pages": {where: "kind = 'page_view'", key: "template", a: "COUNT(*)", b: "0", distinct: "visitor",
		value: countD, second: countA},
	"entry": {where: "kind = 'session_start'", key: "template", a: "COUNT(*)", b: "0", distinct: "visitor",
		value: countD, second: countA},
	"events": {where: "source = 'browser' AND " + eventKinds,
		key: "CASE WHEN kind = 'custom' THEN name ELSE kind END",
		sub: "CASE WHEN kind = 'custom' THEN 'custom' ELSE 'auto' END", a: "COUNT(*)", b: "0", distinct: "visitor",
		value: countA, second: countD},
	"outbound": {where: "kind = 'outbound_click'", key: "name", a: "COUNT(*)", b: "0", distinct: "visitor",
		value: countA, second: countD},
	"referrers": {where: "kind IN ('page_view', 'session_start') AND ref_host != ''", key: "ref_host",
		a: "SUM(kind = 'page_view')", b: "0", distinct: "visitor", value: countD, second: countA},
	"networks": {where: "asn != 0", key: "CAST(asn AS TEXT)", a: "COUNT(*)", b: "0", distinct: "ip_key",
		value: countD, second: countA},
	"countries": {where: "country != ''", key: "country", a: "COUNT(*)", b: "0", distinct: "visitor",
		value: countD, second: countA},
	"probed": {where: "source = 'server' AND status IN (401, 403, 404)", key: "template",
		sub: "CAST(status AS TEXT)", a: "COUNT(*)", b: "0", distinct: "ip_key", server: true, upBad: true,
		value: countA, second: countD},
	"refused": {where: "source = 'server'", key: "ip_key", a: "SUM(status >= 400)", b: "COUNT(*)", server: true,
		upBad: true, value: countA, second: countB},
	"errors": {where: "(kind = 'js_error' OR (source = 'server' AND status >= 500))",
		key: "CASE WHEN kind = 'js_error' THEN name ELSE template END",
		sub: "CASE WHEN kind = 'js_error' THEN 'js' ELSE 'server ' || status END", a: "COUNT(*)", b: "0",
		distinct: "actor", upBad: true, value: countA, second: countD},
	"endpoints": {where: callWhere, key: endpointSQL, sub: "op", a: "COUNT(*)", b: "SUM(" + failedSQL + ")",
		distinct: "ip_key", server: true, api: true, durations: true, failedB: true, value: countA, second: countD},
	"accounts": {where: callWhere + " AND account != ''", key: "account", a: "COUNT(*)", b: "SUM(" + failedSQL + ")",
		distinct: "ip_key", server: true, api: true, failedB: true, value: countA, second: countD},
	"agents": {where: callWhere, key: "ua", name: classify.Client, a: "COUNT(*)", b: "SUM(" + failedSQL + ")",
		distinct: "ip_key", server: true, api: true, failedB: true, value: countA, second: countD},
	"failures": {where: callWhere + " AND " + failedSQL, key: failureSQL, a: "COUNT(*)", b: "0",
		distinct: "ip_key", server: true, api: true, upBad: true, value: countA, second: countD},
}

// distinctExpr is the SQL a list's distinct count runs on. "visitor" is who the Visitors tile counts: a browser's
// own id, never the hash a server request carries (one person loading one page would count twice), and for the
// bots audience the address key. "actor" also counts a server row, by its address: a server error has no browser.
func distinctExpr(distinct, aud string) string {
	switch {
	case distinct != "visitor" && distinct != "actor":
		return distinct
	case aud == "bots":
		return "ip_key"
	case distinct == "actor":
		return "CASE WHEN source = 'browser' THEN visitor ELSE ip_key END"
	}
	return "CASE WHEN source = 'browser' THEN visitor ELSE '' END"
}

// keySums is one key's sums: a and b added up, d its distinct ids.
type keySums struct {
	sub  string
	a, b int64
	d    uniq
	// durations is set for a list whose rows carry percentiles.
	durations agg.Hist
}

// otherSums is what the aggregates collapsed into their days' Other rows (top-K per day): exact sums, n the
// collapsed entries' daily distinct counts added up (an upper bound), and the collapsed keys' hashes.
type otherSums struct {
	a, b, n, entries int64
	hashes           map[uint64]struct{}
	partial          bool  // some day's Other did not list its keys (past 50,000)
	unlisted         int64 // the most keys one such day's Other held
}

// hides reports whether key may be in an Other row: its figure there is not known.
func (o *otherSums) hides(key string) bool {
	if o.partial {
		return true
	}
	_, ok := o.hashes[agg.Hash(key)]
	return ok
}

// empty: no day collapsed anything.
func (o *otherSums) empty() bool { return o.entries == 0 && !o.partial }

// listSums is a list's keys over a span and what the aggregates' Other rows hold of it.
type listSums struct {
	m     map[string]*keySums
	other otherSums
}

func newSums() *listSums { return &listSums{m: map[string]*keySums{}} }

func (l *listSums) get(k string) *keySums {
	g, ok := l.m[k]
	if !ok {
		g = &keySums{}
		l.m[k] = g
	}
	return g
}

// merge adds o to l.
func (l *listSums) merge(o *listSums) {
	for k, g := range o.m {
		t := l.get(k)
		t.sub = max(t.sub, g.sub)
		t.a += g.a
		t.b += g.b
		t.d.merge(&g.d)
		t.durations.Merge(&g.durations)
	}
	l.other.a += o.other.a
	l.other.b += o.other.b
	l.other.n += o.other.n
	l.other.entries += o.other.entries
	l.other.partial = l.other.partial || o.other.partial
	l.other.unlisted = max(l.other.unlisted, o.other.unlisted)
	for h := range o.other.hashes {
		if l.other.hashes == nil {
			l.other.hashes = map[uint64]struct{}{}
		}
		l.other.hashes[h] = struct{}{}
	}
}

// closedWin is [from, to) as a window without an open part.
func closedWin(from, to time.Time) window { return window{From: from, Closed: to, End: to} }

// scan is what a list reads: the raw statement's (dimSpec, extra and args), and the agg_list dimension that serves
// it, with what to take of its rows.
type scan struct {
	dimSpec
	dim   string // "" when only raw answers (the paths inside a template)
	extra string
	args  []any
	sub   string // only the rows of this sub (events' sub=)
	fromB bool   // a row's b is the figure (a network's server rows, spike:net)
}

// scanOf is a list dimension's scan. refused is agg_addr's (every address of a day, no Other).
func scanOf(dim string) scan {
	sc := scan{dimSpec: dims[dim]}
	if slices.Contains(agg.Dims, dim) || dim == "refused" {
		sc.dim = dim
	}
	return sc
}

// partCond is the agg_list (and agg_addr) part an audience reads: people are part 1, everything automated part 2.
func partCond(aud string) string {
	switch aud {
	case "humans":
		return " AND t.part = 1"
	case "bots":
		return " AND t.part = 2"
	}
	return ""
}

// scanDim reads a list over w, a day at a time from its source, into into[0] (the whole window) and into[1] (its
// closed part only). short: a 24h or 7d range or its prior period, read raw first.
func (s *Server) scanDim(ctx context.Context, q *query, sc scan, w window, short bool, into ...*listSums) error {
	pl, err := s.planOf(ctx, q)
	if err != nil {
		return err
	}
	if sc.api {
		pl.need = apiBuilder
		defer func() { pl.need = 0 }()
	}
	sub := "''"
	if sc.dimSpec.sub != "" {
		sub = "MAX(" + sc.dimSpec.sub + ")"
	}
	dist, group := "''", sc.key
	if sc.distinct != "" {
		dist = distinctExpr(sc.distinct, q.aud)
		group = sc.key + ", " + dist
	}
	stmt := `SELECT ` + sc.key + `, ` + sub + `, ` + sc.a + `, ` + sc.b + `, ` + dist + ` FROM event
		WHERE site_id = ? AND ts >= ? AND ts < ? AND ` + sc.where + ` AND ` + audWhere(q.aud) + sc.extra +
		` GROUP BY ` + group
	for _, part := range []struct {
		lo, hi time.Time
		open   bool
	}{{w.From, w.Closed, false}, {w.Closed, w.End, true}} {
		for _, pc := range pieces(part.lo, part.hi) {
			add := newSums()
			raw := func() error {
				if err := s.scanPiece(ctx, stmt, append([]any{q.site.ID, ms(pc[0]), ms(pc[1])}, sc.args...), sc.name, add); err != nil ||
					!sc.durations {
					return err
				}
				return s.scanDurations(ctx, q, sc, pc[0], pc[1], add)
			}
			if sc.dim == "" {
				err = raw()
			} else {
				err = pl.read(ctx, pc[0], pc[1], readSpan, short, func() (bool, error) {
					if sc.dim == "refused" {
						return s.refusedDay(ctx, q, ms(pc[0])/dayMS, add)
					}
					return s.scanDay(ctx, q, sc, ms(pc[0])/dayMS, add)
				}, raw)
			}
			if err != nil {
				return err
			}
			for j, m := range into {
				if j == 0 || !part.open {
					m.merge(add)
				}
			}
		}
	}
	return nil
}

// scanPiece adds one raw piece's rows to into, under their names (nil: the keys as they are).
func (s *Server) scanPiece(ctx context.Context, stmt string, args []any, name func(string) string, into *listSums) error {
	rows, err := s.query(ctx, stmt, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key, sb, dv string
		var x, y int64
		if err := rows.Scan(&key, &sb, &x, &y, &dv); err != nil {
			return err
		}
		if name != nil {
			key = name(key)
		}
		g := into.get(key)
		g.sub = max(g.sub, sb)
		g.a += x
		g.b += y
		if dv != "" {
			g.d.add(dv)
		}
	}
	return rows.Err()
}

// refusedDay adds one day's refusals and requests per address from agg_addr to into (the audience's parts), false
// when the day is not clean in the statement's snapshot.
func (s *Server) refusedDay(ctx context.Context, q *query, day int64, into *listSums) (bool, error) {
	rows, err := s.query(ctx, `SELECT d.ok, t.ip_key, t.refused, t.requests`+
		aggFrom("agg_addr", "t.day = ?2"+partCond(q.aud)), q.site.ID, day)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	add := newSums()
	ok := false
	for rows.Next() {
		var key sql.NullString
		var refused, requests sql.NullInt64
		if err := rows.Scan(&ok, &key, &refused, &requests); err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		if !key.Valid || requests.Int64 == 0 {
			continue
		}
		g := add.get(key.String)
		g.a += refused.Int64
		g.b += requests.Int64
	}
	if err := rows.Err(); err != nil || !ok {
		return false, err
	}
	into.merge(add)
	return true, nil
}

// scanDay adds one day's agg_list rows to into, false (nothing added) when the day is not clean in the statement's
// snapshot. The distinct ids are the rows' identity sketch, for the bots audience the address keys of a visitor or
// actor dimension (an Other row's: their counts' sum, addr_n). Events keep an Other row per sub, so a sub= filter
// keeps its own.
func (s *Server) scanDay(ctx context.Context, q *query, sc scan, day int64, into *listSums) (bool, error) {
	rows, err := s.query(ctx, `SELECT d.ok, t.other, t.key, t.sub, t.a, t.b, t.ids, t.addrs, t.n, t.addr_n,
		t.entries, t.keys, t.durations`+aggFrom("agg_list", "t.dim = ?3 AND t.day = ?2"+partCond(q.aud)), q.site.ID, day, sc.dim)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	byAddr := q.aud == "bots" && (sc.distinct == "visitor" || sc.distinct == "actor")
	add := newSums()
	ok := false
	for rows.Next() {
		var other, a, b, n, addrN, entries sql.NullInt64
		var key, sub sql.NullString
		var ids, addrs, keys, durations []byte
		if err := rows.Scan(&ok, &other, &key, &sub, &a, &b, &ids, &addrs, &n, &addrN, &entries, &keys,
			&durations); err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		if !other.Valid || (sc.sub != "" && sub.String != sc.sub) {
			continue
		}
		if sc.fromB {
			a.Int64 = b.Int64
		}
		if other.Int64 == 1 {
			o := &add.other
			o.a += a.Int64
			o.b += b.Int64
			if byAddr {
				o.n += addrN.Int64
			} else {
				o.n += n.Int64
			}
			o.entries += entries.Int64
			if keys == nil {
				o.partial = true
				o.unlisted = max(o.unlisted, entries.Int64)
			}
			for _, h := range agg.OtherKeys(keys) {
				if o.hashes == nil {
					o.hashes = map[uint64]struct{}{}
				}
				o.hashes[h] = struct{}{}
			}
			continue
		}
		g := add.get(key.String)
		g.sub = max(g.sub, sub.String)
		g.a += a.Int64
		g.b += b.Int64
		if err := g.durations.AddEncoded(durations); err != nil {
			return false, err
		}
		sk := ids
		if byAddr {
			sk = addrs
		}
		if sc.distinct != "" {
			if err := g.d.addSketch(sk); err != nil {
				return false, err
			}
		}
	}
	if err := rows.Err(); err != nil || !ok {
		return false, err
	}
	into.merge(add)
	return true, nil
}

type listRow struct {
	Key        string  `json:"key"`
	Label      string  `json:"label"`
	Sub        string  `json:"sub"`
	Gen        *string `json:"gen"`
	Value      int64   `json:"value"`
	Second     int64   `json:"second"`
	Share      int64   `json:"share"`
	Change     change  `json:"change"`
	Verdict    string  `json:"verdict"`
	Expandable bool    `json:"expandable"`
	// Entry is set for pages and entry only: percent of visits that started there.
	Entry *float64 `json:"entry,omitempty"`
	// Level is set for countries only: the map's shade step, 1-4 over the returned rows.
	Level *int `json:"level,omitempty"`
	// Failed is set for endpoints and accounts: percent of the calls that failed. P50 and P95 are set for endpoints:
	// their durations in ms.
	Failed *float64 `json:"failed,omitempty"`
	P50    *int64   `json:"p50,omitempty"`
	P95    *int64   `json:"p95,omitempty"`
}

type listResp struct {
	Rows   []listRow `json:"rows"`
	Other  other     `json:"other"`
	Total  int       `json:"total"`
	Server bool      `json:"server"`
}

type other struct {
	Count int   `json:"count"`
	Value int64 `json:"value"`
}

// maxLimit bounds "Show all".
const maxLimit = 500

func (s *Server) handleList(w http.ResponseWriter, r *http.Request, q *query) {
	v := r.URL.Query()
	dim := v.Get("dim")
	d, ok := dims[dim]
	if !ok {
		apix.Error(w, http.StatusBadRequest, "unknown dim")
		return
	}
	if d.server && serverOnly(w, q) {
		return
	}
	limit, err := intParam(v.Get("limit"), 6, 1, maxLimit)
	if err != nil {
		apix.Error(w, http.StatusBadRequest, "bad limit")
		return
	}
	sortBy := v.Get("sort")
	if sortBy == "" {
		sortBy = "volume"
	}
	if sortBy != "volume" && sortBy != "change" {
		apix.Error(w, http.StatusBadRequest, "sort must be volume or change")
		return
	}
	o := listOpts{scan: scanOf(dim), sortBy: sortBy, limit: limit,
		needle: strings.ToLower(strings.TrimSpace(v.Get("q")))}
	within := v.Get("within")
	if within != "" {
		if dim != "pages" && dim != "entry" {
			apix.Error(w, http.StatusBadRequest, "within is for pages and entry")
			return
		}
		// The raw paths inside one template: no aggregate keeps paths, so raw only.
		o.extra, o.args = " AND template = ?", []any{within}
		o.key, o.dim = "path", ""
	}
	// A name used both as a custom event and an automatic kind is one aggregate row: its larger sub takes it.
	switch sub := v.Get("sub"); {
	case sub == "":
	case dim != "events" || (sub != "auto" && sub != "custom"):
		apix.Error(w, http.StatusBadRequest, "sub is auto or custom, for dim=events")
		return
	case sub == "custom":
		o.extra, o.sub = o.extra+" AND kind = 'custom'", sub
	default:
		o.extra, o.sub = o.extra+" AND kind != 'custom'", sub
	}
	resp, err := s.list(r.Context(), q, dim, o)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	resp.Server = d.server
	if within != "" {
		// The paths are raw events: they reach back to detail_since, the template's row to the range's start.
		pl, err := s.planOf(r.Context(), q)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		since, err := pl.detailSince(r.Context(), q.p.win.From)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		apix.JSON(w, http.StatusOK, struct {
			listResp
			DetailSince any `json:"detail_since"`
		}{resp, since})
		return
	}
	if dim == "pages" || dim == "entry" {
		for i := range resp.Rows {
			resp.Rows[i].Expandable = true
		}
	}
	apix.JSON(w, http.StatusOK, resp)
}

// listOpts are a list request's choices beyond the dimension.
type listOpts struct {
	scan
	sortBy string
	limit  int
	needle string // lower-case substring of key or label, applied before the top-N
}

// figure is a key's value in a list.
func (d dimSpec) figure(g *keySums) int64 { return d.value(g.a, g.b, g.d.count()) }

// list computes a top-N list: the whole range's figures, the change on the closed part against the comparison's.
//
// From the aggregates, a key collapsed into a day's Other row (past the day's top-K) is a lower bound, and Other holds
// the rest: its count is the hidden rows plus the collapsed keys no row shows, its value the hidden rows' plus the
// Other rows' (exact for counted figures, an upper bound for distinct ones). A key that may sit in an Other row of
// the closed part or of the comparison has an unknown change (null), never "new". The paths inside a template are
// raw only: with the comparison's raw not all there, none has a change.
func (s *Server) list(ctx context.Context, q *query, dim string, o listOpts) (listResp, error) {
	d := o.dimSpec
	pl, err := s.planOf(ctx, q)
	if err != nil {
		return listResp{}, err
	}
	gaps := pl.gaps
	cur, closed := newSums(), newSums()
	if err := s.scanDim(ctx, q, o.scan, q.p.win, !q.p.agg, cur, closed); err != nil {
		return listResp{}, err
	}
	prevClosed := newSums()
	cmpOn := q.p.prevWin != nil
	if cmpOn {
		if err := s.scanDim(ctx, q, o.scan, *q.p.prevWin, !q.p.prevAgg, prevClosed); err != nil {
			return listResp{}, err
		}
		if o.dim == "" {
			if cmpOn, err = pl.rawCovers(ctx, q.p.prevWin.From, q.p.prevWin.Closed); err != nil {
				return listResp{}, err
			}
		}
	}
	// A gap on either side (plan.gaps): no change is known.
	cmpOn = cmpOn && pl.gaps == gaps
	rows := make([]listRow, 0, len(cur.m))
	var sum int64
	for k, g := range cur.m {
		row := listRow{Key: k, Label: k, Sub: g.sub, Value: d.figure(g), Second: d.second(g.a, g.b, g.d.count())}
		if row.Value == 0 { // an address with requests but no refusals is no row of "refused"
			continue
		}
		var c, p float64
		if cg, ok := closed.m[k]; ok {
			c = float64(d.figure(cg))
		}
		pg, seen := prevClosed.m[k]
		if seen {
			p = float64(d.figure(pg))
		}
		row.Change = sumChange(c, p, cmpOn && !prevClosed.other.hides(k) && !closed.other.hides(k))
		row.Verdict = verdict(row.Change, d.upBad)
		if d.failedB {
			f := round1(pct(float64(g.b), float64(g.a)))
			row.Failed = &f
		}
		if p50, ok := g.durations.Percentile(50); d.durations && ok {
			p95, _ := g.durations.Percentile(95)
			row.P50, row.P95 = &p50, &p95
		}
		sum += row.Value
		rows = append(rows, row)
	}
	s.cheapLabels(dim, rows)
	if o.needle != "" {
		rows = slices.DeleteFunc(rows, func(r listRow) bool {
			return !strings.Contains(strings.ToLower(r.Key), o.needle) && !strings.Contains(strings.ToLower(r.Label), o.needle)
		})
	}
	slices.SortFunc(rows, func(a, b listRow) int {
		if o.sortBy == "change" {
			if c := cmp.Compare(changeRank(b.Change), changeRank(a.Change)); c != 0 {
				return c
			}
		}
		if c := cmp.Compare(b.Value, a.Value); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	})
	resp := listResp{Rows: []listRow{}, Total: len(rows)}
	// The keys only the aggregates' Other rows hold: part of every share's base, as raw's unmatched rows are, but no
	// search can match them. Their count is the listed hashes no row shows; with a day's Other past the listing
	// bound, a lower bound: the larger of that and the most keys one such day held less every row shown (a key can
	// sit in several days' Others, so the days do not add up).
	if co := &cur.other; !co.empty() {
		sum += d.value(co.a, co.b, co.n)
	}
	if co := &cur.other; !co.empty() && o.needle == "" {
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
		resp.Other.Value += d.value(co.a, co.b, co.n)
	}
	for i, row := range rows {
		if i >= o.limit {
			resp.Other.Count++
			resp.Other.Value += row.Value
			continue
		}
		if sum > 0 {
			row.Share = int64(math.Round(float64(row.Value) / float64(sum) * 100))
		}
		resp.Rows = append(resp.Rows, row)
	}
	if err := s.richLabels(ctx, q, dim, o, resp.Rows); err != nil {
		return resp, err
	}
	return resp, nil
}

// cheapLabels fills the labels that need no query (network and country names, decoded paths): before the search,
// which matches them.
func (s *Server) cheapLabels(dim string, rows []listRow) {
	switch dim {
	case "networks":
		for i := range rows {
			asn, _ := strconv.ParseInt(rows[i].Key, 10, 64)
			rows[i].Label, rows[i].Sub = s.networkName(asn)
		}
	case "countries":
		for i := range rows {
			rows[i].Label = s.countryName(rows[i].Key)
		}
	case "pages", "entry":
		for i := range rows {
			if l, err := url.PathUnescape(rows[i].Key); err == nil {
				rows[i].Label = strings.ToValidUTF8(l, "�")
			}
		}
	}
}

// changeRank orders rows by change: new first, then the largest growth.
func changeRank(c change) float64 {
	switch {
	case c == nil:
		return math.Inf(-1)
	case c["new"] == true:
		return math.Inf(1)
	}
	f, _ := c["pct"].(float64)
	return f
}

// richLabels fills what the returned rows alone need: the network of a refused address, the model's probe labels,
// the entry share of a page, the map shade of a country.
func (s *Server) richLabels(ctx context.Context, q *query, dim string, o listOpts, rows []listRow) error {
	switch dim {
	case "countries":
		var top int64
		for _, r := range rows {
			top = max(top, r.Value)
		}
		for i := range rows {
			lv := 1
			if top > 0 {
				lv = max(1, int(math.Ceil(float64(rows[i].Value)/float64(top)*4)))
			}
			rows[i].Level = &lv
		}
	case "refused":
		keys := make([]string, len(rows))
		for i := range rows {
			keys[i] = rows[i].Key
		}
		asns := s.asnOf(ctx, q.site.ID, keys)
		for i := range rows {
			rows[i].Sub, _ = s.networkName(asns[rows[i].Key])
		}
	case "pages", "entry":
		// Entry: the share of all visits (session starts) that began on this page or template. The aggregates'
		// entry list counts them per template, Other included.
		starts := newSums()
		sd := scan{dimSpec: dimSpec{where: "kind = 'session_start'", key: o.key, a: "COUNT(*)", b: "0"}, dim: o.dim,
			extra: o.extra, args: o.args}
		if o.dim != "" {
			sd.dim = "entry"
		}
		if err := s.scanDim(ctx, q, sd, q.p.win, !q.p.agg, starts); err != nil {
			return err
		}
		all := starts
		if o.dim == "" { // the paths inside one template: every visit is still the whole, raw as the paths are
			all = newSums()
			if err := s.scanDim(ctx, q, scan{dimSpec: dimSpec{where: "kind = 'session_start'", key: "template",
				a: "COUNT(*)", b: "0"}}, q.p.win, !q.p.agg, all); err != nil {
				return err
			}
		}
		visits := all.other.a
		for _, g := range all.m {
			visits += g.a
		}
		for i := range rows {
			e := 0.0
			if g, ok := starts.m[rows[i].Key]; ok && visits > 0 {
				e = round1(pct(float64(g.a), float64(visits)))
			}
			rows[i].Entry = &e
		}
	case "probed":
		if s.Model == nil {
			return nil
		}
		vals := make([]string, len(rows))
		for i := range rows {
			vals[i] = rows[i].Key
		}
		labels := s.Model.Labels(ctx, q.site.ID, "path", vals)
		for i := range rows {
			if l, ok := labels[rows[i].Key]; ok {
				rows[i].Gen = &l
			}
		}
	}
	return nil
}

// asnOf is the latest ASN seen for each address key (a short indexed read per key): its latest raw event's, else,
// its raw gone, the latest network its daily sums name.
func (s *Server) asnOf(ctx context.Context, site int64, keys []string) map[string]int64 {
	out := map[string]int64{}
	for _, k := range keys {
		var asn int64
		err := s.row(ctx, `SELECT asn FROM event WHERE site_id = ? AND ip_key = ? ORDER BY ts DESC LIMIT 1`, site, k).
			Scan(&asn)
		if errors.Is(err, sql.ErrNoRows) {
			err = s.row(ctx, `SELECT asn FROM agg_addr INDEXED BY agg_addr_key WHERE site_id = ? AND ip_key = ?
				AND asn != 0 ORDER BY day DESC LIMIT 1`, site, k).Scan(&asn)
		}
		if err == nil {
			out[k] = asn
		}
	}
	return out
}

func intParam(v string, def, lo, hi int) (int, error) {
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo {
		return 0, fmt.Errorf("bad number %q", v)
	}
	return min(n, hi), nil
}

type statusRow struct {
	Class string  `json:"class"`
	Value int64   `json:"value"`
	Share float64 `json:"share"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, q *query) {
	if serverOnly(w, q) {
		return
	}
	sc := scan{dimSpec: dimSpec{where: "source = 'server' AND status > 0", key: "CAST(status / 100 AS TEXT) || 'xx'",
		a: "COUNT(*)", b: "0", value: countA, second: zeroSecond}, dim: "status"}
	all := newSums()
	if err := s.scanDim(r.Context(), q, sc, q.p.win, !q.p.agg, all); err != nil {
		s.fail(w, r, err)
		return
	}
	var total int64
	for _, g := range all.m {
		total += g.a
	}
	rows := []statusRow{}
	for k, g := range all.m {
		rows = append(rows, statusRow{Class: k, Value: g.a, Share: round1(pct(float64(g.a), float64(total)))})
	}
	slices.SortFunc(rows, func(a, b statusRow) int { return cmp.Compare(a.Class, b.Class) })
	apix.JSON(w, http.StatusOK, map[string]any{"rows": rows})
}

type shareRow struct {
	Key   string `json:"key"`
	Value int64  `json:"value"`
	Share int64  `json:"share"`
}

// clientFacets are the clients split's lists, each a client's property.
var clientFacets = []string{"browsers", "os", "devices", "ip_version"}

func (s *Server) handleClients(w http.ResponseWriter, r *http.Request, q *query) {
	var counts map[string]map[string]int64
	var err error
	if q.p.agg {
		counts, err = s.everyClient(r.Context(), q)
	} else {
		counts, err = s.latestClient(r.Context(), q)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := map[string][]shareRow{}
	for _, name := range clientFacets {
		var total int64
		for _, v := range counts[name] {
			total += v
		}
		out[name] = topShares(counts[name], 4, total)
	}
	apix.JSON(w, http.StatusOK, out)
}

// clientID is who the clients split counts: a browser's own id, for the bots audience the address key.
func clientID(aud string) (id, where string) {
	if aud == "bots" {
		return "ip_key", "1"
	}
	return "visitor", "source = 'browser'"
}

// clientOf is a user agent and address as the split's four values.
func clientOf(ua string, v6 bool) map[string]string {
	b, o, d := classify.ParseUA(ua)
	v := "IPv4"
	if v6 {
		v = "IPv6"
	}
	return map[string]string{"browsers": b, "os": o, "devices": d, "ip_version": v}
}

// latestClient counts each visitor once per list, by its latest event's user agent and address (24h, 7d).
func (s *Server) latestClient(ctx context.Context, q *query) (map[string]map[string]int64, error) {
	id, where := clientID(q.aud)
	type client struct {
		ua string
		v6 bool
	}
	seen := map[string]client{}
	// Oldest piece first, so a later piece's client replaces an earlier one's. Within a piece the one MAX makes
	// SQLite take ua and ip from the visitor's latest row.
	for _, pc := range pieces(q.p.win.From, q.p.win.End) {
		rows, err := s.query(ctx, `SELECT `+id+`, MAX(ts), ua, instr(ip, ':') > 0 FROM event
			WHERE site_id = ? AND ts >= ? AND ts < ? AND `+where+` AND `+audWhere(q.aud)+` GROUP BY `+id,
			q.site.ID, ms(pc[0]), ms(pc[1]))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k string
			var ts int64
			var c client
			if err := rows.Scan(&k, &ts, &c.ua, &c.v6); err != nil {
				_ = rows.Close()
				return nil, err
			}
			seen[k] = c
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	counts := map[string]map[string]int64{}
	for _, c := range seen {
		for facet, v := range clientOf(c.ua, c.v6) {
			if counts[facet] == nil {
				counts[facet] = map[string]int64{}
			}
			counts[facet][v]++
		}
	}
	return counts, nil
}

// everyClient counts, per value, the visitors that used it (30d and longer): the latest client over a month does
// not add up from days, so a visitor on two browsers counts under both and the shares are of the sum.
func (s *Server) everyClient(ctx context.Context, q *query) (map[string]map[string]int64, error) {
	pl, err := s.planOf(ctx, q)
	if err != nil {
		return nil, err
	}
	ids := clientIDs{}
	for _, pc := range pieces(q.p.win.From, q.p.win.End) {
		if err := pl.read(ctx, pc[0], pc[1], readSpan, false,
			func() (bool, error) { return s.clientDay(ctx, q, ms(pc[0])/dayMS, ids) },
			func() error { return s.clientPiece(ctx, q, pc[0], pc[1], ids) }); err != nil {
			return nil, err
		}
	}
	counts := map[string]map[string]int64{}
	for facet, m := range ids {
		counts[facet] = map[string]int64{}
		for v, u := range m {
			counts[facet][v] = u.count()
		}
	}
	return counts, nil
}

// clientIDs are the ids that used each value, by facet.
type clientIDs map[string]map[string]*uniq

func (c clientIDs) get(facet, v string) *uniq {
	if c[facet] == nil {
		c[facet] = map[string]*uniq{}
	}
	if c[facet][v] == nil {
		c[facet][v] = &uniq{}
	}
	return c[facet][v]
}

// clientPiece adds one raw piece's visitors per client value.
func (s *Server) clientPiece(ctx context.Context, q *query, from, to time.Time, ids clientIDs) error {
	id, where := clientID(q.aud)
	rows, err := s.query(ctx, `SELECT DISTINCT `+id+`, ua, instr(ip, ':') > 0 FROM event
		WHERE site_id = ? AND ts >= ? AND ts < ? AND `+where+` AND `+audWhere(q.aud), q.site.ID, ms(from), ms(to))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k, ua string
		var v6 bool
		if err := rows.Scan(&k, &ua, &v6); err != nil {
			return err
		}
		if k == "" { // a browser row without an id is nobody's
			continue
		}
		for facet, v := range clientOf(ua, v6) {
			ids.get(facet, v).add(k)
		}
	}
	return rows.Err()
}

// clientDay adds one day's clients list (parent: the facet), false when the day is not clean in the statement's
// snapshot.
func (s *Server) clientDay(ctx context.Context, q *query, day int64, ids clientIDs) (bool, error) {
	rows, err := s.query(ctx, `SELECT d.ok, t.parent, t.key, t.ids, t.addrs`+
		aggFrom("agg_list", "t.dim = 'clients' AND t.day = ?2"+partCond(q.aud)), q.site.ID, day)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	add := clientIDs{}
	ok := false
	for rows.Next() {
		var facet, v sql.NullString
		var vids, addrs []byte
		if err := rows.Scan(&ok, &facet, &v, &vids, &addrs); err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		if !facet.Valid {
			continue
		}
		sk := vids
		if q.aud == "bots" {
			sk = addrs
		}
		if len(sk) == 0 { // an automated server row's entry: no visitor id
			continue
		}
		if err := add.get(facet.String, v.String).addSketch(sk); err != nil {
			return false, err
		}
	}
	if err := rows.Err(); err != nil || !ok {
		return false, err
	}
	for facet, m := range add {
		for v, u := range m {
			ids.get(facet, v).merge(u)
		}
	}
	return true, nil
}

// topShares is the n largest rows plus Other, shares in percent of total.
func topShares(m map[string]int64, n int, total int64) []shareRow {
	rows := make([]shareRow, 0, len(m))
	for k, v := range m {
		if k == "Other" {
			continue
		}
		rows = append(rows, shareRow{Key: k, Value: v})
	}
	slices.SortFunc(rows, func(a, b shareRow) int {
		if c := cmp.Compare(b.Value, a.Value); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	})
	rest := m["Other"]
	if len(rows) > n {
		for _, r := range rows[n:] {
			rest += r.Value
		}
		rows = rows[:n]
	}
	if rest > 0 {
		rows = append(rows, shareRow{Key: "Other", Value: rest})
	}
	for i := range rows {
		if total > 0 {
			rows[i].Share = int64(math.Round(float64(rows[i].Value) / float64(total) * 100))
		}
	}
	return rows
}

type classRow struct {
	Class     string  `json:"class"`
	Requests  int64   `json:"requests"`
	Share     float64 `json:"share"`
	Addresses int64   `json:"addresses"`
	Change    change  `json:"change"`
	Verdict   string  `json:"verdict"`
}

// classOf maps an event.cls code to the API's class. Pending counts as unknown automated: a person is shown by a
// browser's evidence (a beacon is stamped human at once, the pass decides the rest within minutes), never assumed.
func classOf(cls int) string {
	if n, ok := store.ClassNames[cls]; ok {
		return n
	}
	return "unknown"
}

var classOrder = []string{"human", "bot", "monitor", "unknown", "api"}

// classError refuses a class filter that is none of classOrder.
const classError = "class must be human, bot, monitor, unknown or api"

func (s *Server) handleClasses(w http.ResponseWriter, r *http.Request, q *query) {
	if serverOnly(w, q) {
		return
	}
	// Ignores aud.
	ctx := r.Context()
	pl, err := s.planOf(ctx, q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	bars := q.p.bars(q.rng, q.loc)
	n := len(bars) - 1
	gaps := pl.gaps
	series := map[string][]float64{}
	cur, prev := classSums{}, classSums{}
	for _, c := range classOrder {
		series[c] = make([]float64, n)
		cur[c], prev[c] = &classAcc{}, &classAcc{}
	}
	for i := 0; i < n; i++ {
		b, err := s.classSpan(ctx, pl, bars[i], bars[i+1], !q.p.agg)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		for name, g := range b {
			series[name][i] += float64(g.requests)
			cur[name].requests += g.requests
			cur[name].addrs.merge(&g.addrs)
		}
	}
	if q.p.Prev != nil {
		if prev, err = s.classSpan(ctx, pl, q.p.Prev[0], q.p.prevCut, !q.p.prevAgg); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	var total int64
	for _, g := range cur {
		total += g.requests
	}
	rows := []classRow{}
	for _, c := range classOrder {
		g := cur[c]
		row := classRow{Class: c, Requests: g.requests, Addresses: g.addrs.count(),
			Share: round1(pct(float64(g.requests), float64(total)))}
		var was int64
		if p := prev[c]; p != nil {
			was = p.requests
		}
		row.Change = sumChange(float64(g.requests), float64(was), q.p.Prev != nil && pl.gaps == gaps)
		row.Verdict = classVerdict(c, row.Change)
		rows = append(rows, row)
	}
	// Nothing has come in yet for the bars after the one now is in.
	for n > 1 && bars[n-1].After(q.p.Now) {
		n--
	}
	bars = bars[:n+1]
	names, partial := []string{}, []bool{}
	for i := range n {
		names = append(names, barName(q.rng, bars[i], bars[i+1], q.loc))
		// Still filling, or a week the range cuts.
		partial = append(partial, bars[i+1].After(q.p.Now) || i == 0 && q.rng == "12mo" && !monday(bars[0], q.loc))
	}
	for c := range series {
		series[c] = series[c][:n]
	}
	ax := newAxis(q.rng, bars, q.loc)
	if q.rng == "30d" || q.rng == "90d" { // a bar is the day its label names: under it, not where it begins
		for i := range ax.Ticks {
			ax.Ticks[i].At += 0.5
		}
	}
	apix.JSON(w, http.StatusOK, struct {
		Labels []string `json:"labels"`
		axis
		Open    bool                 `json:"open"`
		Partial []bool               `json:"partial"`
		Series  map[string][]float64 `json:"series"`
		Rows    []classRow           `json:"rows"`
	}{names, ax, bars[n].After(q.p.Now), partial, series, rows})
}

// classAcc is one class's server requests and distinct addresses.
type classAcc struct {
	requests int64
	addrs    uniq
}

// classSums are classAccs by the API's class name.
type classSums map[string]*classAcc

func (c classSums) get(name string) *classAcc {
	g := c[name]
	if g == nil {
		g = &classAcc{}
		c[name] = g
	}
	return g
}

// classSpan sums [from, to)'s server requests and addresses per class, a piece at a time from its source.
func (s *Server) classSpan(ctx context.Context, pl *plan, from, to time.Time, short bool) (classSums, error) {
	out := classSums{}
	for _, pc := range pieces(from, to) {
		err := pl.read(ctx, pc[0], pc[1], time.Hour, short, func() (bool, error) {
			rows, err := s.query(ctx, `SELECT d.ok, t.cls, t.requests, t.server_addrs`+
				aggFrom("agg_hour", "t.hour >= ?3 AND t.hour < ?4"), pl.site, ms(pc[0])/dayMS, ms(pc[0])/hourMS,
				ms(pc[1])/hourMS)
			if err != nil {
				return false, err
			}
			defer func() { _ = rows.Close() }()
			add := classSums{}
			ok := false
			for rows.Next() {
				var cls, requests sql.NullInt64
				var addrs []byte
				if err := rows.Scan(&ok, &cls, &requests, &addrs); err != nil {
					return false, err
				}
				if !ok {
					return false, nil
				}
				if !cls.Valid || requests.Int64 == 0 {
					continue
				}
				g := add.get(classOf(int(cls.Int64)))
				g.requests += requests.Int64
				if err := g.addrs.addSketch(addrs); err != nil {
					return false, err
				}
			}
			if err := rows.Err(); err != nil || !ok {
				return false, err
			}
			out.merge(add)
			return true, nil
		}, func() error {
			rows, err := s.query(ctx, `SELECT cls, ip_key, COUNT(*) FROM event WHERE site_id = ? AND ts >= ? AND ts < ?
				AND source = 'server' GROUP BY cls, ip_key`, pl.site, ms(pc[0]), ms(pc[1]))
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var cls int
				var key string
				var n int64
				if err := rows.Scan(&cls, &key, &n); err != nil {
					return err
				}
				g := out.get(classOf(cls))
				g.requests += n
				g.addrs.add(key)
			}
			return rows.Err()
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (c classSums) merge(o classSums) {
	for name, g := range o {
		t := c.get(name)
		t.requests += g.requests
		t.addrs.merge(&g.addrs)
	}
}

// classVerdict judges a class's change: more people is good, more unknown automation bad; API clients, known bots and
// uptime probes are neutral, their volume is their operators' business.
func classVerdict(class string, c change) string {
	switch class {
	case "human":
		return verdict(c, false)
	case "unknown":
		return verdict(c, true)
	}
	return "flat"
}
