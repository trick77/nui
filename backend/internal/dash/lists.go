package dash

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

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

// keySums is one key's sums.
type keySums struct {
	sub  string
	a, b int64
	d    map[string]struct{}
}

type aggMap map[string]*keySums

func (m aggMap) get(k string) *keySums {
	g, ok := m[k]
	if !ok {
		g = &keySums{d: map[string]struct{}{}}
		m[k] = g
	}
	return g
}

// closedWin is [from, to) as a window without an open part.
func closedWin(from, to time.Time) window { return window{From: from, Closed: to, End: to} }

// scanDim reads a dimension over w into into[0] (the whole window) and into[1] (its closed part only).
func (s *Server) scanDim(ctx context.Context, q *query, d dimSpec, extra string, args []any, w window,
	into ...aggMap) error {
	sub := "''"
	if d.sub != "" {
		sub = "MAX(" + d.sub + ")"
	}
	dist, group := "''", d.key
	if d.distinct != "" {
		dist = distinctExpr(d.distinct, q.aud)
		group = d.key + ", " + dist
	}
	stmt := `SELECT ` + d.key + `, ` + sub + `, ` + d.a + `, ` + d.b + `, ` + dist + ` FROM event
		WHERE site_id = ? AND ts >= ? AND ts < ? AND ` + d.where + ` AND ` + audWhere(q.aud) + extra +
		` GROUP BY ` + group
	for _, part := range []struct {
		lo, hi time.Time
		open   bool
	}{{w.From, w.Closed, false}, {w.Closed, w.End, true}} {
		for _, pc := range pieces(part.lo, part.hi) {
			a := append([]any{q.site.ID, ms(pc[0]), ms(pc[1])}, args...)
			if err := s.scanPiece(ctx, stmt, a, part.open, into); err != nil {
				return err
			}
		}
	}
	return nil
}

// scanPiece adds one piece's rows to the maps; an open bucket's skip the closed map (into[1]).
func (s *Server) scanPiece(ctx context.Context, stmt string, args []any, open bool, into []aggMap) error {
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
		for j, m := range into {
			if j == 1 && open {
				continue
			}
			g := m.get(key)
			g.sub = max(g.sub, sb)
			g.a += x
			g.b += y
			if dv != "" {
				g.d[dv] = struct{}{}
			}
		}
	}
	return rows.Err()
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
	o := listOpts{args: []any{}, sortBy: sortBy, limit: limit, needle: strings.ToLower(strings.TrimSpace(v.Get("q")))}
	within := v.Get("within")
	if within != "" {
		if dim != "pages" && dim != "entry" {
			apix.Error(w, http.StatusBadRequest, "within is for pages and entry")
			return
		}
		// The raw paths inside one template.
		o.extra, o.args = " AND template = ?", []any{within}
		d.key = "path"
	}
	switch sub := v.Get("sub"); {
	case sub == "":
	case dim != "events" || (sub != "auto" && sub != "custom"):
		apix.Error(w, http.StatusBadRequest, "sub is auto or custom, for dim=events")
		return
	case sub == "custom":
		o.extra += " AND kind = 'custom'"
	default:
		o.extra += " AND kind != 'custom'"
	}
	resp, err := s.list(r.Context(), q, dim, d, o)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	resp.Server = d.server
	if within == "" && (dim == "pages" || dim == "entry") {
		for i := range resp.Rows {
			resp.Rows[i].Expandable = true
		}
	}
	apix.JSON(w, http.StatusOK, resp)
}

// listOpts are a list request's choices beyond the dimension.
type listOpts struct {
	extra  string
	args   []any
	sortBy string
	limit  int
	needle string // lower-case substring of key or label, applied before the top-N
}

// list computes a top-N list: the whole range's figures, the change on closed buckets against the comparison's.
func (s *Server) list(ctx context.Context, q *query, dim string, d dimSpec, o listOpts) (listResp, error) {
	cur, closed := aggMap{}, aggMap{}
	if err := s.scanDim(ctx, q, d, o.extra, o.args, q.p.win, cur, closed); err != nil {
		return listResp{}, err
	}
	prevClosed := aggMap{}
	if q.p.prevWin != nil {
		if err := s.scanDim(ctx, q, d, o.extra, o.args, *q.p.prevWin, prevClosed); err != nil {
			return listResp{}, err
		}
	}
	cmpOn := q.p.prevWin != nil
	rows := make([]listRow, 0, len(cur))
	var sum int64
	for k, g := range cur {
		row := listRow{Key: k, Label: k, Sub: g.sub, Value: d.value(g.a, g.b, int64(len(g.d))),
			Second: d.second(g.a, g.b, int64(len(g.d)))}
		if row.Value == 0 { // an address with requests but no refusals is no row of "refused"
			continue
		}
		var c, p float64
		if cg, ok := closed[k]; ok {
			c = float64(d.value(cg.a, cg.b, int64(len(cg.d))))
		}
		if pg, ok := prevClosed[k]; ok {
			p = float64(d.value(pg.a, pg.b, int64(len(pg.d))))
		}
		row.Change = sumChange(c, p, cmpOn)
		row.Verdict = verdict(row.Change, d.upBad)
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
	if err := s.richLabels(ctx, q, dim, d, o, resp.Rows); err != nil {
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
func (s *Server) richLabels(ctx context.Context, q *query, dim string, d dimSpec, o listOpts, rows []listRow) error {
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
		// Entry: the share of all visits (session starts) that began on this page or template.
		starts := aggMap{}
		sd := dimSpec{where: "kind = 'session_start'", key: d.key, a: "COUNT(*)", b: "0"}
		if err := s.scanDim(ctx, q, sd, o.extra, o.args, q.p.win, starts); err != nil {
			return err
		}
		var visits int64
		all := aggMap{}
		if err := s.scanDim(ctx, q, dimSpec{where: "kind = 'session_start'", key: "''", a: "COUNT(*)", b: "0"}, "", nil,
			q.p.win, all); err != nil {
			return err
		}
		for _, g := range all {
			visits += g.a
		}
		for i := range rows {
			e := 0.0
			if g, ok := starts[rows[i].Key]; ok && visits > 0 {
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

// asnOf is the latest ASN seen for each address key (a short indexed read per key).
func (s *Server) asnOf(ctx context.Context, site int64, keys []string) map[string]int64 {
	out := map[string]int64{}
	for _, k := range keys {
		var asn int64
		if err := s.row(ctx, `SELECT asn FROM event WHERE site_id = ? AND ip_key = ?
			ORDER BY ts DESC LIMIT 1`, site, k).Scan(&asn); err == nil {
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
	d := dimSpec{where: "source = 'server' AND status > 0", key: "CAST(status / 100 AS TEXT) || 'xx'", a: "COUNT(*)",
		b: "0", value: countA, second: zeroSecond}
	all := aggMap{}
	if err := s.scanDim(r.Context(), q, d, "", nil, q.p.win, all); err != nil {
		s.fail(w, r, err)
		return
	}
	var total int64
	for _, g := range all {
		total += g.a
	}
	rows := []statusRow{}
	for k, g := range all {
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
	id, where := clientID(q.aud)
	ids := map[string]map[string]map[string]struct{}{}
	for _, pc := range pieces(q.p.win.From, q.p.win.End) {
		rows, err := s.query(ctx, `SELECT DISTINCT `+id+`, ua, instr(ip, ':') > 0 FROM event
			WHERE site_id = ? AND ts >= ? AND ts < ? AND `+where+` AND `+audWhere(q.aud),
			q.site.ID, ms(pc[0]), ms(pc[1]))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k, ua string
			var v6 bool
			if err := rows.Scan(&k, &ua, &v6); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if k == "" { // a browser row without an id is nobody's
				continue
			}
			for facet, v := range clientOf(ua, v6) {
				if ids[facet] == nil {
					ids[facet] = map[string]map[string]struct{}{}
				}
				if ids[facet][v] == nil {
					ids[facet][v] = map[string]struct{}{}
				}
				ids[facet][v][k] = struct{}{}
			}
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	counts := map[string]map[string]int64{}
	for facet, m := range ids {
		counts[facet] = map[string]int64{}
		for v, u := range m {
			counts[facet][v] = int64(len(u))
		}
	}
	return counts, nil
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

var classOrder = []string{"human", "bot", "monitor", "unknown"}

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
	n := q.p.n()
	series := map[string][]float64{}
	cur, closed, prev := classSums{}, classSums{}, classSums{}
	for _, c := range classOrder {
		series[c] = make([]float64, n)
		cur[c], closed[c], prev[c] = &classAcc{}, &classAcc{}, &classAcc{}
	}
	for i := 0; i < n; i++ {
		b, err := s.classSpan(ctx, pl, q.p.Bounds[i], q.p.Bounds[i+1], !q.p.agg)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		for name, g := range b {
			series[name][i] += float64(g.requests)
			cur[name].requests += g.requests
			cur[name].addrs.merge(&g.addrs)
			if i < n-1 {
				closed[name].requests += g.requests
			}
		}
	}
	if q.p.Prev != nil {
		if prev, err = s.classSpan(ctx, pl, q.p.Prev[0], q.p.Prev[n-1], !q.p.prevAgg); err != nil {
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
		row.Change = sumChange(float64(closed[c].requests), float64(was), q.p.Prev != nil)
		row.Verdict = classVerdict(c, row.Change)
		rows = append(rows, row)
	}
	apix.JSON(w, http.StatusOK, map[string]any{"labels": q.p.labels, "series": series, "rows": rows})
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

// classVerdict judges a class's change: more people is good, more unknown automation bad; known bots and uptime
// probes are neutral, their volume is the services' business.
func classVerdict(class string, c change) string {
	switch class {
	case "human":
		return verdict(c, false)
	case "unknown":
		return verdict(c, true)
	}
	return "flat"
}
