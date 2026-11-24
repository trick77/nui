package dash

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/trick77/trakk/backend/internal/apix"
)

// browserNoise are the automatic kinds the Events tile leaves out: they describe a view, they are not something a
// visitor did.
const eventKinds = `kind NOT IN ('page_view', 'session_start', 'engagement', 'vital')`

// audCls is the agg_hour classes of an audience (pending is stored as 4, unknown automated).
func audCls(aud string) func(cls int) bool {
	switch aud {
	case "humans":
		return func(cls int) bool { return cls == 1 }
	case "bots":
		return func(cls int) bool { return cls != 1 }
	}
	return func(int) bool { return true }
}

// bucketHours adds one piece's hours from agg_hour to the slots they are in, false when its day is not clean in the
// statement's snapshot (nothing added then).
func (s *Server) bucketHours(ctx context.Context, site int64, aud string, from, to time.Time, sl slotting,
	out []bucketStats) (bool, error) {
	rows, err := s.query(ctx, `SELECT d.ok, t.hour, t.cls, t.views, t.actions, t.requests, t.errors, t.browser_ids, t.addrs`+
		aggFrom("agg_hour", "t.hour >= ?3 AND t.hour < ?4"), site, ms(from)/dayMS, ms(from)/hourMS, ms(to)/hourMS)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	in := audCls(aud)
	adds := make([]bucketStats, len(out))
	ok := false
	for rows.Next() {
		var hour, cls sql.NullInt64
		var views, actions, requests, errs sql.NullInt64
		var ids, addrs []byte
		if err := rows.Scan(&ok, &hour, &cls, &views, &actions, &requests, &errs, &ids, &addrs); err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		if !cls.Valid { // a day without an hour of events
			continue
		}
		add := &adds[sl.at(hour.Int64*hourMS)]
		c := int(cls.Int64)
		add.allReq += float64(requests.Int64)
		if c == 4 {
			add.unknown += float64(requests.Int64)
		}
		if !in(c) {
			continue
		}
		add.views += float64(views.Int64)
		add.events += float64(actions.Int64)
		add.requests += float64(requests.Int64)
		add.errors += float64(errs.Int64)
		sk := ids
		if aud == "bots" {
			sk = addrs
		}
		if err := add.visitors.addSketch(sk); err != nil {
			return false, err
		}
	}
	if err := rows.Err(); err != nil || !ok {
		return false, err
	}
	for i := range adds {
		out[i].add(&adds[i])
	}
	return true, nil
}

// add adds o's figures to b.
func (b *bucketStats) add(o *bucketStats) {
	b.views += o.views
	b.events += o.events
	b.requests += o.requests
	b.unknown += o.unknown
	b.allReq += o.allReq
	b.errors += o.errors
	b.visitors.merge(&o.visitors)
	b.api.add(&o.api)
}

// bucketPiece adds one raw piece to the slots its events are in: two short statements (the sums, the distinct
// visitors).
func (s *Server) bucketPiece(ctx context.Context, site int64, aud string, from, to time.Time, sl slotting,
	out []bucketStats) error {
	cond := audWhere(aud)
	args := append(sl.args(), site, ms(from), ms(to))
	if err := s.each(ctx, `SELECT `+cellSQL+` AS cell,
		SUM(`+cond+` AND kind = 'page_view'),
		SUM(`+cond+` AND source = 'browser' AND `+eventKinds+`),
		SUM(`+cond+` AND source = 'server'),
		SUM(source = 'server' AND cls IN (0, 4)),
		SUM(source = 'server'),
		SUM(`+cond+` AND (kind = 'js_error' OR (source = 'server' AND status >= 500)))
		FROM event WHERE site_id = ? AND ts >= ? AND ts < ? GROUP BY cell`, args, func(rows *sql.Rows) error {
		var cell int64
		var views, events, requests, unknown, allReq, errs float64
		if err := rows.Scan(&cell, &views, &events, &requests, &unknown, &allReq, &errs); err != nil {
			return err
		}
		b := &out[sl.slot(cell)]
		b.views += views
		b.events += events
		b.requests += requests
		b.unknown += unknown
		b.allReq += allReq
		b.errors += errs
		return nil
	}); err != nil {
		return err
	}
	ids := `visitor FROM event WHERE site_id = ? AND ts >= ? AND ts < ? AND source = 'browser' AND `
	if aud == "bots" {
		ids = `ip_key FROM event WHERE site_id = ? AND ts >= ? AND ts < ? AND `
	}
	return s.each(ctx, `SELECT DISTINCT `+cellSQL+`, `+ids+cond, args, func(rows *sql.Rows) error {
		var cell int64
		var v string
		if err := rows.Scan(&cell, &v); err != nil {
			return err
		}
		out[sl.slot(cell)].visitors.add(v)
		return nil
	})
}

// buckets reads every bucket of bounds, one at a time.
func (s *Server) buckets(ctx context.Context, pl *plan, aud string, bounds []time.Time, short bool) ([]bucketStats, error) {
	out := make([]bucketStats, 0, len(bounds))
	for i := 0; i+1 < len(bounds); i++ {
		older := pl.older
		b, err := s.slots(ctx, pl, aud, bounds[i:i+2], short)
		if err != nil {
			return nil, err
		}
		b[0].missing = pl.older > older
		out = append(out, b[0])
	}
	return out, nil
}

type tile struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Unit    string   `json:"unit"`
	Kind    string   `json:"kind"`
	Value   float64  `json:"value"`
	Prev    *float64 `json:"prev"`
	Change  change   `json:"change"`
	Verdict string   `json:"verdict"`
	Sub     string   `json:"sub"`
	// Series is the figure of each slot, Mean its moving mean; PrevMean the comparison's, Delta the one against the
	// other.
	Series   []float64 `json:"series"`
	Mean     []float64 `json:"mean"`
	PrevMean []float64 `json:"prev_mean"`
	Delta    []float64 `json:"delta"`
	Server   bool      `json:"server"`
}

// MarshalJSON writes a figure that does not exist (NaN: a percentile of no calls, a slot without figures) as null.
func (t tile) MarshalJSON() ([]byte, error) {
	orNull := func(v float64) *float64 {
		if math.IsNaN(v) {
			return nil
		}
		return &v
	}
	list := func(vs []float64) []*float64 {
		out := make([]*float64, len(vs))
		for i, v := range vs {
			out[i] = orNull(v)
		}
		return out
	}
	type plain tile
	return json.Marshal(struct {
		plain
		Value    *float64   `json:"value"`
		Series   []*float64 `json:"series"`
		Mean     []*float64 `json:"mean"`
		PrevMean []*float64 `json:"prev_mean"`
		Delta    []*float64 `json:"delta"`
	}{plain(t), orNull(t.Value), list(t.Series), list(t.Mean), list(t.PrevMean), list(t.Delta)})
}

type overview struct {
	Period struct {
		From      any    `json:"from"`
		To        any    `json:"to"`
		PrevLabel string `json:"prev_label"`
		// Slot is what a point of the chart covers, Mean the moving mean's window; Labels name the slots.
		Slot   string   `json:"slot"`
		Mean   string   `json:"mean"`
		Labels []string `json:"labels"`
		axis
		// Open says whether the last slot is still filling.
		Open bool `json:"open"`
		// First is the place (axis) the site's figures begin at, null when that is before the range: the slots before it
		// are null.
		First *float64 `json:"first"`
		// Since is set when the figures begin only within the range (an API's, over days an older builder made and
		// whose raw is gone): the slots before it are null.
		Since any `json:"since"`
	} `json:"period"`
	Live  live   `json:"live"`
	Tiles []tile `json:"tiles"`
}

// tileDef is how one tile reads a slot and judges its change.
type tileDef struct {
	key, label, unit, kind string
	server, upBad          bool
	// One of three reads it: of a count, distinct a count of ids, pool a figure that does not add up (a share, a
	// percentile), of all the slots given together, NaN when they hold nothing it could be of.
	of       func(b bucketStats) float64
	distinct func(b *bucketStats) *uniq
	pool     func(bs []bucketStats) float64
	// base is what a rate is a share of; nil: all requests.
	base func(b bucketStats) float64
}

// tileDefsOf are a site's tiles: an API's own, or a website's.
func (q *query) tileDefsOf() []tileDef {
	if q.api() {
		return apiTileDefs
	}
	return tileDefs
}

func allRequests(b bucketStats) float64 { return b.allReq }

var tileDefs = []tileDef{
	{key: "visitors", label: "Unique visitors", kind: "sum", distinct: func(b *bucketStats) *uniq { return &b.visitors }},
	{key: "views", label: "Page views", kind: "sum", of: func(b bucketStats) float64 { return b.views }},
	{key: "events", label: "Events", kind: "sum", of: func(b bucketStats) float64 { return b.events }},
	{key: "requests", label: "Requests", kind: "sum", server: true, of: func(b bucketStats) float64 { return b.requests }},
	{key: "auto_share", label: "Unknown automated", unit: "%", kind: "rate", server: true, upBad: true,
		pool: func(bs []bucketStats) float64 {
			all := totalOf(bs, allRequests)
			if all == 0 {
				return math.NaN()
			}
			return round1(pct(totalOf(bs, func(b bucketStats) float64 { return b.unknown }), all))
		}},
	{key: "errors", label: "Errors", kind: "sum", upBad: true, of: func(b bucketStats) float64 { return b.errors }},
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request, q *query) {
	ov, err := s.overview(r.Context(), q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	apix.JSON(w, http.StatusOK, ov)
}

func (s *Server) overview(ctx context.Context, q *query) (overview, error) {
	var ov overview
	pl, err := s.planOf(ctx, q)
	if err != nil {
		return ov, err
	}
	first, err := pl.first(ctx)
	if err != nil {
		return ov, err
	}
	cur, prev, gaps, err := s.tracks(ctx, pl, q, first)
	if err != nil {
		return ov, err
	}
	p := &ov.Period
	p.From, p.To, p.PrevLabel = iso(q.p.From()), iso(q.p.Now), q.p.prevL
	p.Slot, p.Mean, p.Labels = cur.grid.slot, cur.grid.mean, []string{}
	bounds := []time.Time{q.p.From()}
	for _, pt := range cur.shown(q.p.Now) {
		p.Labels = append(p.Labels, slotName(q.rng, pt.from, pt.to, q.loc))
		bounds = append(bounds, pt.to)
	}
	end := bounds[len(bounds)-1]
	p.Open, p.axis = end.After(q.p.Now), newAxis(q.rng, bounds, q.loc)
	if first.IsZero() {
		first = end
	}
	if first.After(bounds[0]) {
		at := place(bounds, first)
		p.First = &at
	}
	if ov.Live, err = s.live(ctx, q.site.ID); err != nil {
		return ov, err
	}
	for i, missing := range cur.missing {
		if missing {
			p.Since = iso(q.p.Bounds[i+1])
		}
	}
	var prevTotal []bucketStats
	if prev != nil {
		prevTotal = []bucketStats{prev.total}
	}
	if gaps[1] { // no line of a short prior period
		prev = nil
	}
	ov.Tiles = s.tiles(q, []bucketStats{cur.total}, prevTotal, gaps)
	for _, d := range q.tileDefsOf() {
		if i := slices.IndexFunc(ov.Tiles, func(t tile) bool { return t.Key == d.key }); i >= 0 {
			d.chart(&ov.Tiles[i], cur, prev, q.p.Now)
		}
	}
	return ov, nil
}

// tracks reads the range's slots and its comparison's (nil without one), and which of the two had a gap (plan.gaps):
// [0] the range, [1] the comparison.
func (s *Server) tracks(ctx context.Context, pl *plan, q *query, first time.Time) (cur, prev *track, gaps [2]bool,
	err error) {
	gr, g := grids[q.rng], pl.gaps
	if cur, err = s.readTrack(ctx, pl, q, gr, q.p.Bounds, q.p.agg, first, time.Time{}); err != nil {
		return nil, nil, gaps, err
	}
	gaps[0], g = pl.gaps > g, pl.gaps
	if q.p.Prev != nil {
		if q.rng == "24h" && q.p.prevAgg {
			gr = hourGrid
		}
		if prev, err = s.readTrack(ctx, pl, q, gr, q.p.Prev, q.p.prevAgg, first, q.p.prevCut); err != nil {
			return nil, nil, gaps, err
		}
	}
	gaps[1] = pl.gaps > g
	return cur, prev, gaps, nil
}

// curPrev reads the range's buckets and its comparison's (nil without one), and which of the two had a gap
// (plan.gaps): [0] the range, [1] the comparison.
func (s *Server) curPrev(ctx context.Context, pl *plan, q *query, aud string) (cur, prev []bucketStats, gaps [2]bool,
	err error) {
	g := pl.gaps
	if cur, err = s.buckets(ctx, pl, aud, q.p.Bounds, !q.p.agg); err != nil {
		return nil, nil, gaps, err
	}
	gaps[0], g = pl.gaps > g, pl.gaps
	if q.p.Prev != nil {
		if prev, err = s.buckets(ctx, pl, aud, q.p.Prev, !q.p.prevAgg); err != nil {
			return nil, nil, gaps, err
		}
	}
	gaps[1] = pl.gaps > g
	return cur, prev, gaps, nil
}

// tiles builds the tiles without their series: value, prev and change from total and prevTotal (nil: no comparison). A
// gap in the range (gaps[0]) leaves every change unknown (null); one in the comparison (gaps[1]) its prior figures too,
// never a percentage against a short prior period.
func (s *Server) tiles(q *query, total, prevTotal []bucketStats, gaps [2]bool) []tile {
	// A bucket that is not whole is in no figure, as in no series.
	total = slices.DeleteFunc(slices.Clone(total), func(b bucketStats) bool { return b.missing })
	out := []tile{}
	for _, d := range q.tileDefsOf() {
		if d.server && !q.site.HasFeed {
			continue
		}
		t := tile{Key: d.key, Label: d.label, Unit: d.unit, Kind: d.kind, Server: d.server}
		if d.key == "visitors" && q.aud == "bots" {
			t.Label = "Addresses"
		}
		t.Value = s.total(d, total)
		if prevTotal != nil && !gaps[1] {
			pv := s.total(d, prevTotal)
			if !math.IsNaN(pv) {
				t.Prev = &pv
			}
			if d.kind == "rate" {
				base := d.base
				if base == nil {
					base = allRequests
				}
				t.Change = rateChange(t.Value, pv, !gaps[0] && !math.IsNaN(t.Value), totalOf(prevTotal, base) > 0)
			} else {
				t.Change = sumChange(t.Value, pv, !gaps[0] && !math.IsNaN(t.Value) && !math.IsNaN(pv))
			}
		}
		t.Verdict = verdict(t.Change, d.upBad)
		out = append(out, t)
	}
	return out
}

// total is a tile's figure over slots: the distinct across them, a share or a percentile of all they hold, else the
// sum.
func (s *Server) total(d tileDef, bs []bucketStats) float64 {
	switch {
	case d.distinct != nil:
		var all uniq
		for i := range bs {
			all.merge(d.distinct(&bs[i]))
		}
		return float64(all.count())
	case d.pool != nil:
		v := d.pool(bs)
		if d.key == "auto_share" && math.IsNaN(v) { // a tile says 0 of no requests; a line has no point there
			return 0
		}
		return v
	}
	return totalOf(bs, d.of)
}

func totalOf(bs []bucketStats, f func(bucketStats) float64) float64 {
	var t float64
	for _, b := range bs {
		t += f(b)
	}
	return t
}

// live counts the address keys seen in the last five minutes, people apart from the automated. A pending event
// counts as automated only for an address without a person's: a visitor's page request waits for the pass, its
// beacon is human at once, and the visitor is one person, not also an automated address.
func (s *Server) live(ctx context.Context, site int64) (live, error) {
	var l live
	err := s.row(ctx, `SELECT COALESCE(SUM(h), 0), COALESCE(SUM(a OR (p AND NOT h)), 0) FROM (
		SELECT MAX(cls = 1) AS h, MAX(cls IN (2, 3, 4, 5)) AS a, MAX(cls = 0) AS p
		FROM event WHERE site_id = ? AND ts >= ? AND ts < ? GROUP BY ip_key)`,
		// Up to an hour on: past any time the collector stamps (a minute after receipt at most), a bounded read.
		site, ms(s.now().Add(-5*time.Minute)), ms(s.now().Add(time.Hour))).Scan(&l.Humans, &l.Automated)
	return l, err
}

type siteRow struct {
	Site    string `json:"site"`
	Product string `json:"product"`
	Kind    string `json:"kind"`
	// Feed: a server feed is configured, delivered or not.
	Feed    bool           `json:"feed"`
	Browser map[string]any `json:"browser"`
	Server  map[string]any `json:"server"`
}

func (s *Server) handleSites(w http.ResponseWriter, r *http.Request) {
	rows := []siteRow{}
	for _, st := range s.Sites.All() {
		row := siteRow{Site: st.Domain, Product: st.Product, Kind: st.Kind, Feed: st.HasFeed}
		b, sv, err := s.lastSeen(r.Context(), st.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		// A source that never delivered is null, like one the site does not have.
		if !b.IsZero() {
			row.Browser = map[string]any{"last": iso(b)}
		}
		if st.HasFeed && !sv.IsZero() {
			row.Server = map[string]any{"last": iso(sv)}
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b siteRow) int {
		if a.Product != b.Product {
			if a.Product < b.Product {
				return -1
			}
			return 1
		}
		if a.Site < b.Site {
			return -1
		}
		return 1
	})
	apix.JSON(w, http.StatusOK, map[string]any{"sites": rows})
}

// lastSeen is the time of each source's newest stored event, before and after a restart alike. Where to look comes
// from when the source last delivered: this process's own record (exact), else the last hour the counters show. A
// delivery dates its events at most lastSeenBack before it (the feed's hour, the browser's half hour) and a minute
// after, so the newest event is searched in that window only: a short index range, even when the other source
// floods the site and this one delivered long ago. Found nothing (written by now, or pruned): the delivery time.
func (s *Server) lastSeen(ctx context.Context, site int64) (browser, server time.Time, err error) {
	var bh, sh int64
	if err = s.row(ctx, `SELECT COALESCE(MAX(CASE WHEN accepted > accepted_server THEN hour END), 0),
		COALESCE(MAX(CASE WHEN accepted_server > 0 THEN hour END), 0) FROM ingest_stat WHERE site_id = ?`, site).
		Scan(&bh, &sh); err != nil {
		return browser, server, err
	}
	for _, src := range []struct {
		name string
		hour int64
		out  *time.Time
	}{{"browser", bh, &browser}, {"server", sh, &server}} {
		var lo, hi time.Time
		if s.Counters != nil {
			if at := s.Counters.Last(site, src.name); !at.IsZero() {
				lo, hi = at, at
			}
		}
		if lo.IsZero() && src.hour > 0 {
			lo, hi = time.Unix(src.hour*3600, 0), time.Unix((src.hour+1)*3600, 0)
		}
		if lo.IsZero() {
			continue
		}
		var newest *int64
		if err = s.row(ctx, `SELECT MAX(ts) FROM event WHERE site_id = ? AND ts >= ? AND ts <= ? AND source = ?`,
			site, ms(lo.Add(-lastSeenBack)), ms(hi.Add(time.Minute)), src.name).Scan(&newest); err != nil {
			return browser, server, err
		}
		*src.out = lo
		if newest != nil {
			*src.out = time.UnixMilli(*newest)
		}
	}
	return browser, server, nil
}

// lastSeenBack is how far before its delivery an event may be dated: the feed's hour, plus the browser's half hour
// of client offset as a margin.
const lastSeenBack = 90 * time.Minute
