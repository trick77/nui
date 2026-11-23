package dash

import (
	"context"
	"database/sql"
	"net/http"
	"slices"
	"time"

	"github.com/trick77/stakeout/backend/internal/apix"
)

// browserNoise are the automatic kinds the Events tile leaves out: they describe a view, they are not something a
// visitor did.
const eventKinds = `kind NOT IN ('page_view', 'session_start', 'engagement', 'vital')`

// bucket reads one bucket, a piece (at most a day) at a time, each from its source. short: a 24h or 7d range or its
// prior period, read raw first.
func (s *Server) bucket(ctx context.Context, pl *plan, aud string, from, to time.Time, short bool) (bucketStats, error) {
	var b bucketStats
	for _, pc := range pieces(from, to) {
		if err := pl.read(ctx, pc[0], pc[1], time.Hour, short,
			func() (bool, error) { return s.bucketHours(ctx, pl.site, aud, pc[0], pc[1], &b) },
			func() error { return s.bucketPiece(ctx, pl.site, aud, pc[0], pc[1], &b) }); err != nil {
			return b, err
		}
	}
	return b, nil
}

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

// bucketHours adds one piece's hours from agg_hour to b, false when its day is not clean in the statement's
// snapshot (nothing added then).
func (s *Server) bucketHours(ctx context.Context, site int64, aud string, from, to time.Time, b *bucketStats) (bool, error) {
	rows, err := s.query(ctx, `SELECT d.ok, t.cls, t.views, t.actions, t.requests, t.errors, t.browser_ids, t.addrs`+
		aggFrom("agg_hour", "t.hour >= ?3 AND t.hour < ?4"), site, ms(from)/dayMS, ms(from)/hourMS, ms(to)/hourMS)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	in := audCls(aud)
	var add bucketStats
	ok := false
	for rows.Next() {
		var cls sql.NullInt64
		var views, actions, requests, errs sql.NullInt64
		var ids, addrs []byte
		if err := rows.Scan(&ok, &cls, &views, &actions, &requests, &errs, &ids, &addrs); err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		if !cls.Valid { // a day without an hour of events
			continue
		}
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
	b.add(&add)
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
}

// bucketPiece adds one piece to b: two short statements (the sums, the distinct visitors).
func (s *Server) bucketPiece(ctx context.Context, site int64, aud string, from, to time.Time, b *bucketStats) error {
	var views, events, requests, unknown, allReq, errs float64
	cond := audWhere(aud)
	err := s.row(ctx, `SELECT
		COALESCE(SUM(`+cond+` AND kind = 'page_view'), 0),
		COALESCE(SUM(`+cond+` AND source = 'browser' AND `+eventKinds+`), 0),
		COALESCE(SUM(`+cond+` AND source = 'server'), 0),
		COALESCE(SUM(source = 'server' AND cls IN (0, 4)), 0),
		COALESCE(SUM(source = 'server'), 0),
		COALESCE(SUM(`+cond+` AND (kind = 'js_error' OR (source = 'server' AND status >= 500))), 0)
		FROM event WHERE site_id = ? AND ts >= ? AND ts < ?`, site, ms(from), ms(to)).
		Scan(&views, &events, &requests, &unknown, &allReq, &errs)
	if err != nil {
		return err
	}
	b.views += views
	b.events += events
	b.requests += requests
	b.unknown += unknown
	b.allReq += allReq
	b.errors += errs
	vq := `SELECT DISTINCT visitor FROM event WHERE site_id = ? AND ts >= ? AND ts < ? AND source = 'browser' AND ` + cond
	if aud == "bots" {
		vq = `SELECT DISTINCT ip_key FROM event WHERE site_id = ? AND ts >= ? AND ts < ? AND ` + cond
	}
	rows, err := s.query(ctx, vq, site, ms(from), ms(to))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return err
		}
		b.visitors.add(v)
	}
	return rows.Err()
}

// buckets reads every bucket of bounds, one at a time.
func (s *Server) buckets(ctx context.Context, pl *plan, aud string, bounds []time.Time, short bool) ([]bucketStats, error) {
	out := make([]bucketStats, 0, len(bounds))
	for i := 0; i+1 < len(bounds); i++ {
		b, err := s.bucket(ctx, pl, aud, bounds[i], bounds[i+1], short)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

type tile struct {
	Key        string    `json:"key"`
	Label      string    `json:"label"`
	Unit       string    `json:"unit"`
	Kind       string    `json:"kind"`
	Value      float64   `json:"value"`
	Prev       *float64  `json:"prev"`
	Change     change    `json:"change"`
	Verdict    string    `json:"verdict"`
	Sub        string    `json:"sub"`
	Series     []float64 `json:"series"`
	PrevSeries []float64 `json:"prev_series"`
	Median     float64   `json:"median"`
	Server     bool      `json:"server"`
}

type overview struct {
	Period struct {
		From           any      `json:"from"`
		To             any      `json:"to"`
		Bucket         string   `json:"bucket"`
		Labels         []string `json:"labels"`
		PrevLabel      string   `json:"prev_label"`
		LastBucketOpen bool     `json:"last_bucket_open"`
	} `json:"period"`
	Live  live   `json:"live"`
	Tiles []tile `json:"tiles"`
}

// tileDef is how one tile reads a bucket and judges its change.
type tileDef struct {
	key, label, unit, kind string
	server, upBad          bool
	of                     func(b bucketStats) float64
}

var tileDefs = []tileDef{
	{key: "visitors", label: "Unique visitors", kind: "sum", of: func(b bucketStats) float64 { return float64(b.visitors.count()) }},
	{key: "views", label: "Page views", kind: "sum", of: func(b bucketStats) float64 { return b.views }},
	{key: "events", label: "Events", kind: "sum", of: func(b bucketStats) float64 { return b.events }},
	{key: "requests", label: "Requests", kind: "sum", server: true, of: func(b bucketStats) float64 { return b.requests }},
	{key: "auto_share", label: "Unknown automated", unit: "%", kind: "rate", server: true, upBad: true,
		of: func(b bucketStats) float64 { return pct(b.unknown, b.allReq) }},
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
	cur, prev, gaps, err := s.curPrev(ctx, pl, q, q.aud)
	if err != nil {
		return ov, err
	}
	ov.Period.From, ov.Period.To = iso(q.p.From()), iso(q.p.Now)
	ov.Period.Bucket, ov.Period.Labels, ov.Period.PrevLabel = q.p.Bucket, q.p.labels, q.p.prevL
	ov.Period.LastBucketOpen = true
	if ov.Live, err = s.live(ctx, q.site.ID); err != nil {
		return ov, err
	}
	ov.Tiles = s.tiles(q, cur, prev, gaps)
	return ov, nil
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

// tiles builds the tiles from the buckets; value, prev and median leave the open bucket out. A gap in the range
// (gaps[0]) leaves every change unknown (null); one in the comparison (gaps[1]) its prior figures too, never a short
// prior period's dip or a percentage against it.
func (s *Server) tiles(q *query, cur, prev []bucketStats, gaps [2]bool) []tile {
	closed := q.p.n() - 1
	out := []tile{}
	for _, d := range tileDefs {
		if d.server && !q.site.HasFeed {
			continue
		}
		t := tile{Key: d.key, Label: d.label, Unit: d.unit, Kind: d.kind, Server: d.server,
			Series: make([]float64, len(cur)), PrevSeries: []float64{}}
		if d.key == "visitors" && q.aud == "bots" {
			t.Label = "Addresses"
		}
		for i, b := range cur {
			t.Series[i] = round1(d.of(b))
		}
		t.Median = round1(median(t.Series[:closed]))
		t.Value = s.total(d, cur[:closed])
		if prev != nil && !gaps[1] {
			for _, b := range prev {
				t.PrevSeries = append(t.PrevSeries, round1(d.of(b)))
			}
			pv := s.total(d, prev[:closed])
			t.Prev = &pv
			if d.kind == "rate" {
				t.Change = rateChange(t.Value, pv, !gaps[0], totalOf(prev[:closed], func(b bucketStats) float64 { return b.allReq }) > 0)
			} else {
				t.Change = sumChange(t.Value, pv, !gaps[0])
			}
		}
		t.Verdict = verdict(t.Change, d.upBad)
		out = append(out, t)
	}
	return out
}

// total is a tile's figure over buckets: distinct visitors across them, a rate over the sums, else the sum.
func (s *Server) total(d tileDef, bs []bucketStats) float64 {
	switch d.key {
	case "visitors":
		var all uniq
		for i := range bs {
			all.merge(&bs[i].visitors)
		}
		return float64(all.count())
	case "auto_share":
		return round1(pct(totalOf(bs, func(b bucketStats) float64 { return b.unknown }),
			totalOf(bs, func(b bucketStats) float64 { return b.allReq })))
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
		SELECT MAX(cls = 1) AS h, MAX(cls IN (2, 3, 4)) AS a, MAX(cls = 0) AS p
		FROM event WHERE site_id = ? AND ts >= ? AND ts < ? GROUP BY ip_key)`,
		// Up to an hour on: past any time the collector stamps (a minute after receipt at most), a bounded read.
		site, ms(s.now().Add(-5*time.Minute)), ms(s.now().Add(time.Hour))).Scan(&l.Humans, &l.Automated)
	return l, err
}

type siteRow struct {
	Site    string         `json:"site"`
	Product string         `json:"product"`
	Kind    string         `json:"kind"`
	Browser map[string]any `json:"browser"`
	Server  map[string]any `json:"server"`
}

func (s *Server) handleSites(w http.ResponseWriter, r *http.Request) {
	rows := []siteRow{}
	for _, st := range s.Sites.All() {
		row := siteRow{Site: st.Domain, Product: st.Product, Kind: st.Kind}
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
