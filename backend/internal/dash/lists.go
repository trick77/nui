package dash

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/nui/backend/internal/apix"
	"github.com/trick77/nui/backend/internal/classify"
	"github.com/trick77/nui/backend/internal/store"
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

// agg is one key's sums.
type agg struct {
	sub  string
	a, b int64
	d    map[string]struct{}
}

type aggMap map[string]*agg

func (m aggMap) get(k string) *agg {
	g, ok := m[k]
	if !ok {
		g = &agg{d: map[string]struct{}{}}
		m[k] = g
	}
	return g
}

// scanDim reads a dimension over bounds' buckets into cur (open bucket included) and closed (without it).
func (s *Server) scanDim(ctx context.Context, q *query, d dimSpec, extra string, args []any, bounds []time.Time,
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
	n := len(bounds) - 1
	for i := 0; i < n; i++ {
		for _, pc := range pieces(bounds[i], bounds[i+1]) {
			a := append([]any{q.site.ID, ms(pc[0]), ms(pc[1])}, args...)
			// into[0] takes every bucket, into[1] only the closed ones.
			if err := s.scanPiece(ctx, stmt, a, i == n-1, into); err != nil {
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
	if err := s.scanDim(ctx, q, d, o.extra, o.args, q.p.Bounds, cur, closed); err != nil {
		return listResp{}, err
	}
	prevClosed := aggMap{}
	if q.p.Prev != nil {
		if err := s.scanDim(ctx, q, d, o.extra, o.args, q.p.Prev[:q.p.n()], prevClosed); err != nil {
			return listResp{}, err
		}
	}
	cmpOn := q.p.Prev != nil
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
		if err := s.scanDim(ctx, q, sd, o.extra, o.args, q.p.Bounds, starts); err != nil {
			return err
		}
		var visits int64
		all := aggMap{}
		if err := s.scanDim(ctx, q, dimSpec{where: "kind = 'session_start'", key: "''", a: "COUNT(*)", b: "0"}, "", nil,
			q.p.Bounds, all); err != nil {
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
	if err := s.scanDim(r.Context(), q, d, "", nil, q.p.Bounds, all); err != nil {
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

func (s *Server) handleClients(w http.ResponseWriter, r *http.Request, q *query) {
	// One user agent and address per visitor, its latest event's, each visitor counted once per list.
	id, where := "visitor", "source = 'browser'"
	if q.aud == "bots" {
		id, where = "ip_key", "1"
	}
	type client struct{ ua, ip string }
	seen := map[string]client{}
	// Oldest piece first, so a later piece's client replaces an earlier one's. Within a piece the one MAX makes
	// SQLite take ua and ip from the visitor's latest row.
	for _, pc := range pieces(q.p.From(), q.p.Bounds[q.p.n()]) {
		rows, err := s.query(r.Context(), `SELECT `+id+`, MAX(ts), ua, ip FROM event
			WHERE site_id = ? AND ts >= ? AND ts < ? AND `+where+` AND `+audWhere(q.aud)+` GROUP BY `+id,
			q.site.ID, ms(pc[0]), ms(pc[1]))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		for rows.Next() {
			var k string
			var ts int64
			var c client
			if err := rows.Scan(&k, &ts, &c.ua, &c.ip); err != nil {
				_ = rows.Close()
				s.fail(w, r, err)
				return
			}
			seen[k] = c
		}
		_ = rows.Close()
	}
	counts := map[string]map[string]int64{"ip_version": {}, "browsers": {}, "os": {}, "devices": {}}
	for _, c := range seen {
		v := "IPv4"
		if strings.Contains(c.ip, ":") {
			v = "IPv6"
		}
		b, o, d := classify.ParseUA(c.ua)
		counts["ip_version"][v]++
		counts["browsers"][b]++
		counts["os"][o]++
		counts["devices"][d]++
	}
	out := map[string][]shareRow{}
	for name, m := range counts {
		out[name] = topShares(m, 4, int64(len(seen)))
	}
	apix.JSON(w, http.StatusOK, out)
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

// classOf maps an event.cls code to the API's class, pending counted as human.
func classOf(cls int) string {
	if n, ok := store.ClassNames[cls]; ok {
		return n
	}
	return "human"
}

var classOrder = []string{"human", "bot", "monitor", "unknown"}

func (s *Server) handleClasses(w http.ResponseWriter, r *http.Request, q *query) {
	if serverOnly(w, q) {
		return
	}
	all := q
	if q.aud != "all" { // ignores aud
		c := *q
		c.aud = "all"
		all = &c
	}
	d := dimSpec{where: "source = 'server'", key: "CAST(cls AS TEXT)", a: "COUNT(*)", b: "0", distinct: "ip_key",
		value: countA, second: countD}
	series := map[string][]float64{}
	for _, c := range classOrder {
		series[c] = make([]float64, q.p.n())
	}
	cur, closed := aggMap{}, aggMap{}
	for i := 0; i < q.p.n(); i++ {
		b := aggMap{}
		if err := s.scanDim(r.Context(), all, d, "", nil, q.p.Bounds[i:i+2], b); err != nil {
			s.fail(w, r, err)
			return
		}
		for k, g := range b {
			cls, _ := strconv.Atoi(k)
			name := classOf(cls)
			series[name][i] += float64(g.a)
			merge(cur.get(name), g)
			if i < q.p.n()-1 {
				merge(closed.get(name), g)
			}
		}
	}
	prev := aggMap{}
	if q.p.Prev != nil {
		raw := aggMap{}
		if err := s.scanDim(r.Context(), all, d, "", nil, q.p.Prev[:q.p.n()], raw); err != nil {
			s.fail(w, r, err)
			return
		}
		for k, g := range raw {
			cls, _ := strconv.Atoi(k)
			merge(prev.get(classOf(cls)), g)
		}
	}
	var total int64
	for _, g := range cur {
		total += g.a
	}
	rows := []classRow{}
	for _, c := range classOrder {
		g := cur.get(c)
		row := classRow{Class: c, Requests: g.a, Addresses: int64(len(g.d)),
			Share: round1(pct(float64(g.a), float64(total)))}
		row.Change = sumChange(float64(closed.get(c).a), float64(prev.get(c).a), q.p.Prev != nil)
		row.Verdict = classVerdict(c, row.Change)
		rows = append(rows, row)
	}
	apix.JSON(w, http.StatusOK, map[string]any{"labels": q.p.labels, "series": series, "rows": rows})
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

func merge(into, g *agg) {
	into.a += g.a
	into.b += g.b
	for k := range g.d {
		into.d[k] = struct{}{}
	}
}
