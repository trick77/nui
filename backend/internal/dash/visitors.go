package dash

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/nui/backend/internal/apix"
	"github.com/trick77/nui/backend/internal/classify"
	"github.com/trick77/nui/backend/internal/insight"
)

// actor is one address key summed over a range.
type actor struct {
	key                     string
	visits, views, requests int64
	refused, events         int64
	last                    int64
	ua                      string
	asn                     int64
	country                 string
	cls                     int
	browser                 bool
}

// actors sums every address key over the range's buckets.
func (s *Server) actors(ctx context.Context, q *query) (map[string]*actor, error) {
	out := map[string]*actor{}
	for i := 0; i < q.p.n(); i++ {
		rows, err := s.query(ctx, `SELECT ip_key, COUNT(DISTINCT CASE WHEN sid != '' THEN sid END),
			SUM(kind = 'page_view'), SUM(source = 'server'), SUM(source = 'server' AND status >= 400),
			SUM(source = 'browser' AND `+eventKinds+`), MAX(ts), MAX(ua), MAX(asn), MAX(country), MAX(cls),
			MAX(source = 'browser')
			FROM event WHERE site_id = ? AND ts >= ? AND ts < ? AND `+audWhere(q.aud)+` GROUP BY ip_key`,
			q.site.ID, ms(q.p.Bounds[i]), ms(q.p.Bounds[i+1]))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var a actor
			if err := rows.Scan(&a.key, &a.visits, &a.views, &a.requests, &a.refused, &a.events, &a.last, &a.ua,
				&a.asn, &a.country, &a.cls, &a.browser); err != nil {
				_ = rows.Close()
				return nil, err
			}
			g, ok := out[a.key]
			if !ok {
				g = &actor{key: a.key}
				out[a.key] = g
			}
			g.visits += a.visits
			g.views += a.views
			g.requests += a.requests
			g.refused += a.refused
			g.events += a.events
			g.browser = g.browser || a.browser
			if a.last >= g.last {
				g.last, g.ua, g.cls = a.last, a.ua, a.cls
			}
			if a.asn != 0 {
				g.asn = a.asn
			}
			if a.country != "" {
				g.country = a.country
			}
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// classes is each key's latest stored class and reasons within the range.
func (s *Server) classes(ctx context.Context, site int64, from, to time.Time) (map[string]classInfo, error) {
	rows, err := s.query(ctx, `SELECT ip_key, class, reasons FROM actor_class
		WHERE site_id = ? AND day >= ? AND day <= ? ORDER BY day`, site, ms(from)/dayMS, ms(to)/dayMS)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]classInfo{}
	for rows.Next() {
		var k, c, reasons string
		if err := rows.Scan(&k, &c, &reasons); err != nil {
			return nil, err
		}
		ci := classInfo{class: c}
		_ = json.Unmarshal([]byte(reasons), &ci.signals)
		out[k] = ci
	}
	return out, rows.Err()
}

const dayMS = int64(24 * time.Hour / time.Millisecond)

type classInfo struct {
	class   string
	signals []string
}

type does struct {
	Gen  *string `json:"gen"`
	Text string  `json:"text"`
}

type visitorRow struct {
	Key          string   `json:"key"`
	Display      string   `json:"display"`
	Network      string   `json:"network"`
	NetworkKind  string   `json:"network_kind"`
	Country      string   `json:"country"`
	Class        string   `json:"class"`
	Client       string   `json:"client"`
	Visits       *int64   `json:"visits"`
	Views        int64    `json:"views"`
	Requests     *int64   `json:"requests"`
	Refused      *int64   `json:"refused"`
	RefusedShare *float64 `json:"refused_share"`
	Events       int64    `json:"events"`
	Last         any      `json:"last"`
	Signals      []string `json:"signals"`
	Does         does     `json:"does"`
	// ClientGen is the model's label for an unknown automated client's user agent ("Python scraping script"),
	// null when there is none.
	ClientGen *string `json:"client_gen"`

	ua string
}

func (s *Server) handleVisitors(w http.ResponseWriter, r *http.Request, q *query) {
	v := r.URL.Query()
	sortBy := v.Get("sort")
	if sortBy == "" {
		sortBy = "views"
		if q.site.HasFeed {
			sortBy = "requests"
		}
	}
	if !slices.Contains([]string{"requests", "refused", "events", "views"}, sortBy) {
		apix.Error(w, http.StatusBadRequest, "sort must be requests, refused, events or views")
		return
	}
	if !q.site.HasFeed && (sortBy == "requests" || sortBy == "refused") {
		sortBy = "views"
	}
	class := v.Get("class")
	if class != "" && !slices.Contains(classOrder, class) {
		apix.Error(w, http.StatusBadRequest, "class must be human, bot, monitor or unknown")
		return
	}
	limit, err := intParam(v.Get("limit"), 7, 1, maxLimit)
	if err != nil {
		apix.Error(w, http.StatusBadRequest, "bad limit")
		return
	}
	offset, err := intParam(v.Get("cursor"), 0, 0, math.MaxInt32)
	if err != nil {
		apix.Error(w, http.StatusBadRequest, "bad cursor")
		return
	}
	ctx := r.Context()
	all, err := s.actors(ctx, q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	cls, err := s.classes(ctx, q.site.ID, q.p.From(), q.p.Now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	needle := strings.ToLower(strings.TrimSpace(v.Get("q")))
	rows := make([]visitorRow, 0, len(all))
	for _, a := range all {
		row := s.visitorRow(q, a, cls[a.key])
		if class != "" && row.Class != class {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(row.Key+" "+row.Network+" "+row.Client+" "+row.Country),
			needle) {
			continue
		}
		rows = append(rows, row)
	}
	key := func(r visitorRow) int64 {
		switch sortBy {
		case "requests":
			return *r.Requests
		case "refused":
			return *r.Refused
		case "events":
			return r.Events
		}
		return r.Views
	}
	slices.SortFunc(rows, func(a, b visitorRow) int {
		if c := cmp.Compare(key(b), key(a)); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	})
	resp := map[string]any{"total": len(rows), "next": ""}
	page := []visitorRow{}
	if offset < len(rows) {
		page = rows[offset:min(offset+limit, len(rows))]
		if offset+limit < len(rows) {
			resp["next"] = strconv.Itoa(offset + limit)
		}
	}
	s.readRows(ctx, q, page)
	resp["rows"] = page
	apix.JSON(w, http.StatusOK, resp)
}

func (s *Server) visitorRow(q *query, a *actor, ci classInfo) visitorRow {
	row := visitorRow{Key: a.key, Display: a.key, Country: a.country, Client: classify.Client(a.ua),
		Views: a.views, Events: a.events, Last: iso(time.UnixMilli(a.last)), Signals: ci.signals,
		Class: ci.class, ua: a.ua}
	if row.Class == "" {
		row.Class = classOf(a.cls)
	}
	if row.Signals == nil {
		row.Signals = []string{}
	}
	row.Network, row.NetworkKind = s.networkName(a.asn)
	if a.browser {
		v := a.visits
		row.Visits = &v
	}
	if q.site.HasFeed {
		req, ref := a.requests, a.refused
		share := round1(pct(float64(a.refused), float64(a.requests)))
		row.Requests, row.Refused, row.RefusedShare = &req, &ref, &share
	}
	return row
}

// readRows fills the table's "What it does": the computed sentence always, the model's when a reading of the
// row's behaviour is cached (missing ones are queued: the table is the only place that asks for them).
func (s *Server) readRows(ctx context.Context, q *query, rows []visitorRow) {
	ins := make([]insight.ReadingInput, 0, len(rows))
	for i := range rows {
		tr, err := s.trail(ctx, q.site.ID, rows[i].Key, q.p.From(), q.p.Bounds[q.p.n()], cursor{}, 1)
		if err != nil {
			continue
		}
		rows[i].Does.Text = doesText(rows[i], tr)
		ins = append(ins, s.readingInput(q.site.ID, rows[i], tr))
	}
	if s.Model == nil || !s.Model.Configured() {
		return
	}
	got := s.Model.Readings(ctx, ins, false)
	for i := range rows {
		if rd, ok := got[rows[i].Key]; ok && rd.Summary != "" {
			sum := rd.Summary
			rows[i].Does.Gen = &sum
		}
	}
	// Unknown automated clients get a label for their user agent (one per distinct value, kept for good).
	var uas []string
	for _, r := range rows {
		if r.Class == "unknown" && r.ua != "" && !slices.Contains(uas, r.ua) {
			uas = append(uas, r.ua)
		}
	}
	if len(uas) == 0 {
		return
	}
	labels := s.Model.Labels(ctx, q.site.ID, "ua", uas)
	for i := range rows {
		if l, ok := labels[rows[i].ua]; ok && rows[i].Class == "unknown" {
			rows[i].ClientGen = &l
		}
	}
}

// doesText is the computed "What it does": the class and the actor's most frequent action.
func doesText(row visitorRow, tr trailResult) string {
	what := ""
	var top int64
	for w, n := range tr.counts {
		if n > top || (n == top && w < what) {
			what, top = w, n
		}
	}
	phrase := map[string]string{"human": "Person", "bot": "Known bot", "monitor": "Uptime probe",
		"unknown": "Unknown automated"}[row.Class]
	if what == "" {
		return phrase
	}
	return fmt.Sprintf("%s, mostly %s (%d×)", phrase, what, top)
}

// trailLine is one collapsed run of equal actions.
type trailLine struct {
	T         any     `json:"t"`
	Kind      string  `json:"kind"`
	What      string  `json:"what"`
	Count     int64   `json:"count"`
	From      any     `json:"from"`
	To        any     `json:"to"`
	IntervalS float64 `json:"interval_s"`

	fromMS, toMS int64
}

type trailResult struct {
	lines  []trailLine
	next   string
	counts map[string]int64 // what → events, over the lines read
	gaps   []float64        // seconds between consecutive events
}

// maxTrailLines bounds a trail page (and what the model is shown); maxTrailEvents the events read for one page.
const (
	maxTrailLines  = 50
	maxTrailEvents = 2000
)

// trail reads an actor's events newest first from before (a keyset cursor, zero for the newest) and collapses
// runs. Ordered by (ts, id): the actor index holds both, so the read is an index walk, never a sort.
func (s *Server) trail(ctx context.Context, site int64, key string, from, to time.Time, before cursor, pages int) (trailResult, error) {
	tr := trailResult{counts: map[string]int64{}, lines: []trailLine{}}
	args := []any{site, key, ms(from), ms(to)}
	cond := ""
	if before.ts > 0 {
		cond, args = " AND (ts < ? OR (ts = ? AND id < ?))", append(args, before.ts, before.ts, before.id)
	}
	rows, err := s.query(ctx, `SELECT id, ts, source, kind, name, template, status FROM event
		WHERE site_id = ? AND ip_key = ? AND ts >= ? AND ts < ?`+cond+` ORDER BY ts DESC, id DESC LIMIT `+
		strconv.Itoa(maxTrailEvents*pages), args...)
	if err != nil {
		return tr, err
	}
	defer func() { _ = rows.Close() }()
	var prevTS int64
	var last cursor
	read := 0
	for rows.Next() {
		read++
		var id, ts int64
		var source, kind, name, tmpl string
		var status int
		if err := rows.Scan(&id, &ts, &source, &kind, &name, &tmpl, &status); err != nil {
			return tr, err
		}
		what, k := describe(source, kind, name, tmpl, status)
		tr.counts[what]++
		if prevTS != 0 {
			tr.gaps = append(tr.gaps, float64(prevTS-ts)/1000)
		}
		prevTS = ts
		if n := len(tr.lines); n > 0 && tr.lines[n-1].What == what {
			l := &tr.lines[n-1]
			l.Count++
			l.From, l.fromMS = iso(time.UnixMilli(ts)), ts
			l.IntervalS = round1(float64(l.toMS-ts) / 1000 / float64(l.Count-1))
			last = cursor{ts, id}
			continue
		}
		if len(tr.lines) == maxTrailLines {
			tr.next = last.String()
			break
		}
		t := iso(time.UnixMilli(ts))
		tr.lines = append(tr.lines, trailLine{T: t, Kind: k, What: what, Count: 1, From: t, To: t, fromMS: ts, toMS: ts})
		last = cursor{ts, id}
	}
	// The read stopped at its event bound with lines to spare: more may follow.
	if tr.next == "" && read == maxTrailEvents*pages {
		tr.next = last.String()
	}
	return tr, rows.Err()
}

// cursor is a keyset position: the last row's time and id, "ts.id" on the wire.
type cursor struct{ ts, id int64 }

func (c cursor) String() string {
	return strconv.FormatInt(c.ts, 10) + "." + strconv.FormatInt(c.id, 10)
}

func parseCursor(s string) (cursor, bool) {
	if s == "" {
		return cursor{}, true
	}
	a, b, ok := strings.Cut(s, ".")
	ts, err1 := strconv.ParseInt(a, 10, 64)
	id, err2 := strconv.ParseInt(b, 10, 64)
	if !ok || err1 != nil || err2 != nil || ts <= 0 || id <= 0 {
		return cursor{}, false
	}
	return cursor{ts, id}, true
}

// describe is an event as one trail or log line: "GET /api/v1/ip/{addr} → 401", "view /AS:n".
func describe(source, kind, name, tmpl string, status int) (what, k string) {
	if source == "server" {
		return fmt.Sprintf("%s → %d", tmpl, status), "request"
	}
	switch kind {
	case "page_view":
		return "view " + tmpl, kind
	case "custom":
		return "event " + name, kind
	}
	if name != "" {
		return kind + " " + name, kind
	}
	return kind, kind
}

// readingInput is what the model reads about one actor: computed evidence it may cite by id, the trail, a
// signature of the behaviour (not the address) to cache the reading by.
func (s *Server) readingInput(site int64, row visitorRow, tr trailResult) insight.ReadingInput {
	in := insight.ReadingInput{SiteID: site, Key: row.Key, Class: row.Class, Client: row.Client,
		Network: strings.TrimSpace(row.Network + " " + row.NetworkKind), Signals: row.Signals}
	for _, l := range tr.lines {
		line := l.What
		if l.Count > 1 {
			line = fmt.Sprintf("%s ×%d, %.1f s apart", l.What, l.Count, l.IntervalS)
		}
		in.Trail = append(in.Trail, line)
	}
	if len(tr.lines) > 0 {
		in.To = time.UnixMilli(tr.lines[0].toMS).UTC()
		in.From = time.UnixMilli(tr.lines[len(tr.lines)-1].fromMS).UTC()
	}
	ev := func(id, text string) { in.Evidence = append(in.Evidence, insight.Evidence{ID: id, Text: text}) }
	if row.Requests != nil && *row.Requests > 0 {
		ev("requests", fmt.Sprintf("%d requests, %.0f %% refused", *row.Requests, *row.RefusedShare))
	}
	if row.Views > 0 {
		ev("views", fmt.Sprintf("%d page views in %d visits", row.Views, deref(row.Visits)))
	}
	whats := make([]string, 0, len(tr.counts))
	for w := range tr.counts {
		whats = append(whats, w)
	}
	slices.SortFunc(whats, func(a, b string) int {
		if c := cmp.Compare(tr.counts[b], tr.counts[a]); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	if len(whats) > 0 {
		ev("top", fmt.Sprintf("Most frequent: %s (%d×)", whats[0], tr.counts[whats[0]]))
		ev("variety", fmt.Sprintf("%d different actions", len(whats)))
	}
	if len(tr.gaps) >= 3 {
		ev("pace", fmt.Sprintf("Median %.1f s between requests", median(tr.gaps)))
	}
	ev("client", "Client "+row.Client)
	if row.Network != "" {
		ev("network", strings.TrimSpace("Network "+row.Network+" "+row.NetworkKind))
	}
	for i, sg := range row.Signals {
		ev("signal"+strconv.Itoa(i+1), "Signal: "+sg)
	}
	// The signature: what it does (top actions, counts on a log scale), how fast, with what kind of client from what
	// kind of network. Two addresses doing the same share it, and with it the reading.
	sig := []string{row.Class, clientFamily(row.Client), row.NetworkKind}
	for _, w := range whats[:min(8, len(whats))] {
		sig = append(sig, fmt.Sprintf("%s:%d", w, int(math.Log2(float64(tr.counts[w])+1))))
	}
	if len(tr.gaps) >= 3 {
		sig = append(sig, fmt.Sprintf("pace:%d", int(math.Log2(median(tr.gaps)+1))))
	}
	sum := sha256.Sum256([]byte(strings.Join(sig, "\x00")))
	in.Signature = hex.EncodeToString(sum[:16])
	return in
}

// clientFamily drops the version: curl/8.4 and curl/8.5 behave alike.
func clientFamily(c string) string {
	if i := strings.IndexByte(c, '/'); i > 0 {
		return c[:i]
	}
	return c
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (s *Server) handleVisitor(w http.ResponseWriter, r *http.Request, q *query) {
	v := r.URL.Query()
	key := strings.TrimSpace(v.Get("key"))
	if key == "" || len(key) > 64 {
		apix.Error(w, http.StatusBadRequest, "key is an address or a /64")
		return
	}
	before, ok := parseCursor(v.Get("cursor"))
	if !ok {
		apix.Error(w, http.StatusBadRequest, "bad cursor")
		return
	}
	ctx := r.Context()
	a, err := s.actorOne(ctx, q, key)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if a == nil {
		apix.Error(w, http.StatusNotFound, "no events from this address in the range")
		return
	}
	cls, err := s.classes(ctx, q.site.ID, q.p.From(), q.p.Now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	row := s.visitorRow(q, a, cls[key])
	tr, err := s.trail(ctx, q.site.ID, key, q.p.From(), q.p.Bounds[q.p.n()], before, 1)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	hours, addrs, err := s.hours(ctx, q, key)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	resp := map[string]any{"key": key, "class": row.Class, "network": row.Network, "network_kind": row.NetworkKind,
		"country": row.Country, "client": row.Client, "signals": row.Signals, "requests": row.Requests,
		"addresses": addrs, "hours": hours, "trail": tr.lines, "trail_next": tr.next, "reading": nil}
	if s.Model != nil && s.Model.Configured() {
		in := s.readingInput(q.site.ID, row, tr)
		if rd, ok := s.Model.Readings(ctx, []insight.ReadingInput{in}, true)[key]; ok {
			evidence := []string{}
			for _, id := range rd.Evidence {
				for _, e := range in.Evidence {
					if e.ID == id {
						evidence = append(evidence, e.Text)
					}
				}
			}
			related, _ := s.related(ctx, q, key, row.Class)
			resp["reading"] = map[string]any{"verdict": rd.Verdict, "suspicious": insight.Suspicious(rd.Verdict),
				"evidence": evidence, "related": related, "basis": map[string]any{"from": iso(rd.From), "to": iso(rd.To)}}
		}
	}
	apix.JSON(w, http.StatusOK, resp)
}

// actorOne sums one key over the range (nil when it has no events there).
func (s *Server) actorOne(ctx context.Context, q *query, key string) (*actor, error) {
	var a actor
	var last *int64
	var asn *int64
	var ua, country *string
	var cls *int
	var browser *bool
	err := s.row(ctx, `SELECT COUNT(DISTINCT CASE WHEN sid != '' THEN sid END),
		COALESCE(SUM(kind = 'page_view'), 0), COALESCE(SUM(source = 'server'), 0),
		COALESCE(SUM(source = 'server' AND status >= 400), 0), COALESCE(SUM(source = 'browser' AND `+eventKinds+`), 0),
		MAX(ts), MAX(asn), MAX(country), MAX(cls), MAX(source = 'browser')
		FROM event WHERE site_id = ? AND ip_key = ? AND ts >= ? AND ts < ?`,
		q.site.ID, key, ms(q.p.From()), ms(q.p.Bounds[q.p.n()])).Scan(&a.visits, &a.views, &a.requests, &a.refused,
		&a.events, &last, &asn, &country, &cls, &browser)
	if err != nil || last == nil {
		return nil, err
	}
	a.key, a.last, a.asn, a.country, a.cls, a.browser = key, *last, *asn, *country, *cls, *browser
	if err := s.row(ctx, `SELECT ua FROM event WHERE site_id = ? AND ip_key = ? AND ts <= ?
		ORDER BY ts DESC LIMIT 1`, q.site.ID, key, a.last).Scan(&ua); err == nil {
		a.ua = *ua
	}
	return &a, nil
}

// hours is the actor's activity per hour of the viewer's day, as levels 0-4, and its distinct addresses.
func (s *Server) hours(ctx context.Context, q *query, key string) ([]int, int64, error) {
	rows, err := s.query(ctx, `SELECT ts / 3600000, COUNT(*) FROM event
		WHERE site_id = ? AND ip_key = ? AND ts >= ? GROUP BY 1`, q.site.ID, key, ms(q.p.From()))
	if err != nil {
		return nil, 0, err
	}
	counts := make([]int64, 24)
	for rows.Next() {
		var h, n int64
		if err := rows.Scan(&h, &n); err != nil {
			_ = rows.Close()
			return nil, 0, err
		}
		counts[time.Unix(h*3600, 0).In(q.loc).Hour()] += n
	}
	_ = rows.Close()
	top := slices.Max(counts)
	levels := make([]int, 24)
	for i, n := range counts {
		if top > 0 && n > 0 {
			levels[i] = int(math.Ceil(float64(n) / float64(top) * 4))
		}
	}
	var addrs int64
	err = s.row(ctx, `SELECT COUNT(DISTINCT ip) FROM event WHERE site_id = ? AND ip_key = ?
		AND ts >= ?`, q.site.ID, key, ms(q.p.From())).Scan(&addrs)
	return levels, addrs, err
}

// related are other keys of the same class in the same network block (/24, /48) within the range: the operator's
// other addresses.
func (s *Server) related(ctx context.Context, q *query, key, class string) ([]string, error) {
	block, ok := blockOf(key)
	if !ok {
		return []string{}, nil
	}
	rows, err := s.query(ctx, `SELECT DISTINCT ip_key FROM actor_class WHERE site_id = ? AND day >= ?
		AND class = ? AND ip_key != ? LIMIT 5000`, q.site.ID, ms(q.p.From())/dayMS, class, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() && len(out) < 3 {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		if b, ok := blockOf(k); ok && b == block {
			out = append(out, k)
		}
	}
	return out, rows.Err()
}

// blockOf is a key's /24 (IPv4) or /48 (IPv6, from its /64).
func blockOf(key string) (netip.Prefix, bool) {
	addr := strings.TrimSuffix(key, "/64")
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return netip.Prefix{}, false
	}
	bits := 48
	if a.Is4() {
		bits = 24
	}
	p, err := a.Prefix(bits)
	return p, err == nil
}
