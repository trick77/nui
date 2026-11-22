package dash

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/trick77/nui/backend/internal/apix"
	"github.com/trick77/nui/backend/internal/classify"
	"github.com/trick77/nui/backend/internal/insight"
)

// actors sums every address key over the range, a piece (at most a day) at a time. Visits are distinct sessions
// over the whole range: a session crossing a piece boundary is one visit, so they are counted from (key, session)
// pairs, never summed per piece.
func (s *Server) actors(ctx context.Context, q *query, from, to time.Time) (map[string]*actor, error) {
	out := map[string]*actor{}
	// A hash per (key, session) seen: a few bytes each, not the strings.
	sessions := map[uint64]struct{}{}
	for _, pc := range pieces(from, to) {
		if err := s.actorSessions(ctx, q, pc[0], pc[1], sessions, out); err != nil {
			return nil, err
		}
		// One MAX only: SQLite then takes the bare columns (ua, asn, country, cls) from the row holding it, the
		// key's latest event. A second MIN or MAX would make that row arbitrary.
		rows, err := s.query(ctx, `SELECT ip_key,
			SUM(kind = 'page_view'), SUM(source = 'server'), SUM(source = 'server' AND status >= 400),
			SUM(source = 'browser' AND `+eventKinds+`), MAX(ts), ua, asn, country, cls,
			SUM(source = 'browser') > 0
			FROM event WHERE site_id = ? AND ts >= ? AND ts < ? AND `+audWhere(q.aud)+` GROUP BY ip_key`,
			q.site.ID, ms(pc[0]), ms(pc[1]))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var a actor
			if err := rows.Scan(&a.key, &a.views, &a.requests, &a.refused, &a.events, &a.last, &a.ua,
				&a.asn, &a.country, &a.cls, &a.browser); err != nil {
				_ = rows.Close()
				return nil, err
			}
			g := actorOf(out, a.key)
			g.views += a.views
			g.requests += a.requests
			g.refused += a.refused
			g.events += a.events
			g.browser = g.browser || a.browser
			// The latest event's client, network and country, as the visitor detail shows them.
			if a.last >= g.last {
				g.last, g.ua, g.cls = a.last, a.ua, a.cls
				if a.asn != 0 {
					g.asn = a.asn
				}
				if a.country != "" {
					g.country = a.country
				}
			}
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func actorOf(m map[string]*actor, key string) *actor {
	g, ok := m[key]
	if !ok {
		g = &actor{key: key}
		m[key] = g
	}
	return g
}

// actorSessions counts the sessions of one bucket that no earlier bucket had, per key.
func (s *Server) actorSessions(ctx context.Context, q *query, from, to time.Time, seen map[uint64]struct{},
	out map[string]*actor) error {
	rows, err := s.query(ctx, `SELECT ip_key, sid FROM event WHERE site_id = ? AND ts >= ? AND ts < ? AND sid != ''
		AND `+audWhere(q.aud)+` GROUP BY ip_key, sid`, q.site.ID, ms(from), ms(to))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key, sid string
		if err := rows.Scan(&key, &sid); err != nil {
			return err
		}
		h := fnv.New64a()
		_, _ = h.Write([]byte(key))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(sid))
		if _, dup := seen[h.Sum64()]; dup {
			continue
		}
		seen[h.Sum64()] = struct{}{}
		actorOf(out, key).visits++
	}
	return rows.Err()
}

// classOne is one key's latest stored class and reasons within the range, on the primary key: the detail needs no
// other actor's rows.
func (s *Server) classOne(ctx context.Context, site int64, key string, from, to time.Time) (classInfo, error) {
	var ci classInfo
	var reasons string
	err := s.row(ctx, `SELECT class, reasons FROM actor_class WHERE site_id = ? AND ip_key = ? AND day >= ?
		AND day <= ? ORDER BY day DESC LIMIT 1`, site, key, ms(from)/dayMS, ms(to)/dayMS).Scan(&ci.class, &reasons)
	if errors.Is(err, sql.ErrNoRows) {
		return classInfo{}, nil
	}
	if err != nil {
		return ci, err
	}
	_ = json.Unmarshal([]byte(reasons), &ci.signals)
	return ci, nil
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
	after, err := parseVisitorCursor(v.Get("cursor"), q.p.Now)
	if err != nil {
		apix.Error(w, http.StatusBadRequest, "bad cursor")
		return
	}
	// The first page freezes the span [range start, now); every later page reads the same span (its cursor says
	// which) from the same snapshot, so rows neither move between pages nor repeat.
	span := visitorCursor{From: ms(q.p.From()), To: ms(q.p.Now)}
	if after != nil {
		span.From, span.To = after.From, after.To
	}
	ctx := r.Context()
	needle := strings.ToLower(strings.TrimSpace(v.Get("q")))
	snapKey := fmt.Sprintf("%d|%s|%s|%s|%s|%d|%d", q.site.ID, q.aud, sortBy, class, needle, span.From, span.To)
	rows, ok := s.snaps.get(snapKey, s.now())
	if !ok {
		if rows, err = s.visitorRows(ctx, q, span, sortBy, class, needle); err != nil {
			s.fail(w, r, err)
			return
		}
		s.snaps.put(snapKey, rows, s.now())
	}
	start := 0
	if after != nil {
		// The first row past the cursor's (value, key) in the snapshot's order.
		start = len(rows)
		for i := range rows {
			if v := sortValue(rows[i], sortBy); v < after.Value || (v == after.Value && rows[i].Key > after.Key) {
				start = i
				break
			}
		}
	}
	end := min(start+limit, len(rows))
	page := slices.Clone(rows[start:end])
	resp := map[string]any{"total": len(rows), "next": ""}
	if end < len(rows) {
		last := page[len(page)-1]
		resp["next"] = visitorCursor{From: span.From, To: span.To, Value: sortValue(last, sortBy), Key: last.Key}.String()
	}
	s.readRows(ctx, q, span, page, readQueue(rows, page))
	resp["rows"] = page
	apix.JSON(w, http.StatusOK, resp)
}

// visitorRows is the table over a span: every actor, filtered and sorted (by the sort value, then the key).
func (s *Server) visitorRows(ctx context.Context, q *query, span visitorCursor, sortBy, class, needle string) ([]visitorRow, error) {
	from, to := time.UnixMilli(span.From), time.UnixMilli(span.To)
	all, err := s.actors(ctx, q, from, to)
	if err != nil {
		return nil, err
	}
	cls, err := s.classes(ctx, q.site.ID, from, to)
	if err != nil {
		return nil, err
	}
	labelled, err := s.labelMatches(ctx, needle)
	if err != nil {
		return nil, err
	}
	rows := make([]visitorRow, 0, len(all))
	for _, a := range all {
		row := s.visitorRow(q, a, cls[a.key])
		if class != "" && row.Class != class {
			continue
		}
		// The search covers what the row shows: the model's client label too (labelClients shows it on unknown
		// automated rows).
		if needle != "" && !strings.Contains(strings.ToLower(row.Key+" "+row.Network+" "+row.Client+" "+row.Country),
			needle) && (row.Class != "unknown" || !labelled[row.ua]) {
			continue
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b visitorRow) int {
		if c := cmp.Compare(sortValue(b, sortBy), sortValue(a, sortBy)); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	})
	return rows, nil
}

// labelMatches is the user agents whose stored model label holds needle, nil without a needle or a model (the
// table shows no label then).
func (s *Server) labelMatches(ctx context.Context, needle string) (map[string]bool, error) {
	if needle == "" || s.Model == nil || !s.Model.Configured() {
		return nil, nil
	}
	rows, err := s.query(ctx, `SELECT value FROM llm_label WHERE kind = 'ua' AND instr(lower(label), ?) > 0`, needle)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var ua string
		if err := rows.Scan(&ua); err != nil {
			return nil, err
		}
		out[ua] = true
	}
	return out, rows.Err()
}

func sortValue(r visitorRow, sortBy string) int64 {
	switch sortBy {
	case "requests":
		return deref(r.Requests)
	case "refused":
		return deref(r.Refused)
	case "events":
		return r.Events
	}
	return r.Views
}

// visitorCursor is a visitors page's position: the frozen span and the last row's sort value and key, base64url
// JSON on the wire.
type visitorCursor struct {
	From  int64  `json:"f"`
	To    int64  `json:"t"`
	Value int64  `json:"v"`
	Key   string `json:"k"`
}

func (c visitorCursor) String() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// parseVisitorCursor reads a cursor, nil for none. Its span must be one a range can have: at most 13 months, not
// in the future.
func parseVisitorCursor(s string, now time.Time) (*visitorCursor, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var c visitorCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.From <= 0 || c.To <= c.From || c.To > ms(now)+60_000 || c.To-c.From > 400*dayMS || len(c.Key) > 64 {
		return nil, errors.New("cursor out of bounds")
	}
	return &c, nil
}

// snapshots keeps the sorted visitor tables a cursor pages through: a few, for minutes, bounded in rows.
type snapshots struct {
	mu sync.Mutex
	m  map[string]snapshot
}

type snapshot struct {
	rows []visitorRow
	at   time.Time
}

const (
	snapshotTTL     = 10 * time.Minute
	snapshotMaxRows = 200_000 // all snapshots together
)

func (c *snapshots) get(key string, now time.Time) ([]visitorRow, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sn, ok := c.m[key]
	if !ok || now.Sub(sn.at) > snapshotTTL {
		return nil, false
	}
	return sn.rows, true
}

func (c *snapshots) put(key string, rows []visitorRow, now time.Time) {
	if len(rows) > snapshotMaxRows {
		return // too large to keep: later pages recompute the same frozen span
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]snapshot{}
	}
	total := len(rows)
	for k, sn := range c.m {
		if now.Sub(sn.at) > snapshotTTL {
			delete(c.m, k)
			continue
		}
		total += len(sn.rows)
	}
	// Over the bound: the oldest go first.
	for total > snapshotMaxRows && len(c.m) > 0 {
		var oldest string
		for k, sn := range c.m {
			if oldest == "" || sn.at.Before(c.m[oldest].at) {
				oldest = k
			}
		}
		total -= len(c.m[oldest].rows)
		delete(c.m, oldest)
	}
	c.m[key] = snapshot{rows: rows, at: now}
}

// Readings queued per table view, and from where: the actors leading by requests and by refusals, and the unknown
// automated, of the page shown.
const (
	readQueueTop = 5
	readQueueMax = 5
)

// readQueue is the keys of page rows a missing reading is queued for: rows among the whole table's top by requests
// or by refusals, or classed unknown; at most readQueueMax. Every other row only shows a cached reading.
func readQueue(all, page []visitorRow) map[string]bool {
	top := map[string]bool{}
	for _, by := range []string{"requests", "refused"} {
		ranked := slices.Clone(all)
		slices.SortStableFunc(ranked, func(a, b visitorRow) int {
			return cmp.Compare(sortValue(b, by), sortValue(a, by))
		})
		for _, r := range ranked[:min(readQueueTop, len(ranked))] {
			if sortValue(r, by) > 0 {
				top[r.Key] = true
			}
		}
	}
	out := map[string]bool{}
	for _, r := range page {
		if len(out) < readQueueMax && (top[r.Key] || r.Class == "unknown") {
			out[r.Key] = true
		}
	}
	return out
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

// readRows fills the table's "What it does": the computed sentence always, over the whole span, and the model's
// when a reading of the row's behaviour is cached and every fact it cites exists for the row. Missing readings are
// queued only for the rows in queue.
func (s *Server) readRows(ctx context.Context, q *query, span visitorCursor, rows []visitorRow, queue map[string]bool) {
	from, to := time.UnixMilli(span.From), time.UnixMilli(span.To)
	ins := make([]insight.ReadingInput, 0, len(rows))
	shown := map[string]map[string]string{}
	for i := range rows {
		counts, err := s.actions(ctx, q.site.ID, rows[i].Key, from, to)
		if err != nil {
			continue
		}
		rows[i].Does.Text = doesText(rows[i], counts)
		tr, err := s.trail(ctx, q.site.ID, rows[i].Key, from, to, cursor{}, 1)
		if err != nil {
			continue
		}
		in, facts := s.readingInput(q.site.ID, rows[i], counts, tr)
		in.Queue = queue[rows[i].Key]
		ins = append(ins, in)
		shown[rows[i].Key] = facts
	}
	if s.Model == nil || !s.Model.Configured() {
		return
	}
	got := s.Model.Readings(ctx, ins, false)
	for i := range rows {
		if rd, ok := got[rows[i].Key]; ok && rd.Summary != "" {
			if _, ok := resolve(rd, shown[rows[i].Key]); ok {
				sum := rd.Summary
				rows[i].Does.Gen = &sum
			}
		}
	}
	s.labelClients(ctx, q.site.ID, rows)
}

// labelClients sets the model's label for an unknown automated client's user agent (one per distinct value, kept
// for good), the same in the table and the visitor detail. The model must be configured.
func (s *Server) labelClients(ctx context.Context, site int64, rows []visitorRow) {
	var uas []string
	for _, r := range rows {
		if r.Class == "unknown" && r.ua != "" && !slices.Contains(uas, r.ua) {
			uas = append(uas, r.ua)
		}
	}
	if len(uas) == 0 {
		return
	}
	labels := s.Model.Labels(ctx, site, "ua", uas)
	for i := range rows {
		if l, ok := labels[rows[i].ua]; ok && rows[i].Class == "unknown" {
			rows[i].ClientGen = &l
		}
	}
}

// resolve is a reading's evidence as the actor's own statements, ok false when one cited fact does not exist for
// the actor: then the reading is not shown at all.
func resolve(rd insight.Reading, facts map[string]string) ([]string, bool) {
	out := []string{}
	for _, id := range rd.Evidence {
		t, ok := facts[id]
		if !ok {
			return nil, false
		}
		out = append(out, t)
	}
	return out, true
}

// actorSpan is the longest span one statement over a single actor's events reads: the actor index keeps it short,
// a month of a busy scraper is still a long read.
const actorSpan = 31 * 24 * time.Hour

// actions counts an actor's events per action ("GET /x → 401", "view /a") over the whole span.
func (s *Server) actions(ctx context.Context, site int64, key string, from, to time.Time) (map[string]int64, error) {
	out := map[string]int64{}
	for _, win := range windows(ms(from), ms(to), actorSpan) {
		rows, err := s.query(ctx, `SELECT source, kind, name, template, status, COUNT(*) FROM event
			WHERE site_id = ? AND ip_key = ? AND ts >= ? AND ts < ? GROUP BY source, kind, name, template, status`,
			site, key, win[0], win[1])
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var source, kind, name, tmpl string
			var status int
			var n int64
			if err := rows.Scan(&source, &kind, &name, &tmpl, &status, &n); err != nil {
				_ = rows.Close()
				return nil, err
			}
			what, _ := describe(source, kind, name, tmpl, status)
			out[what] += n
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// topActions orders actions by count, then name.
func topActions(counts map[string]int64) []string {
	whats := make([]string, 0, len(counts))
	for w := range counts {
		whats = append(whats, w)
	}
	slices.SortFunc(whats, func(a, b string) int {
		if c := cmp.Compare(counts[b], counts[a]); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	return whats
}

// doesText is the computed "What it does": the class and the actor's most frequent action over the whole span.
func doesText(row visitorRow, counts map[string]int64) string {
	what := ""
	var top int64
	if w := topActions(counts); len(w) > 0 {
		what, top = w[0], counts[w[0]]
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
	T     any    `json:"t"`
	Kind  string `json:"kind"`
	What  string `json:"what"`
	Count int64  `json:"count"`
	From  any    `json:"from"`
	To    any    `json:"to"`
	// IntervalS is the run's median time between its events: what the run itself did, never an average over the
	// days a run may span.
	IntervalS float64 `json:"interval_s"`
	// A page ends at an event bound, maybe inside a run: Continues marks a run whose older events are on the next
	// page, Continued one whose newer events were on the previous page. Count, span and interval are this page's part.
	Continues bool `json:"continues"`
	Continued bool `json:"continued"`

	fromMS, toMS int64
	gaps         []float64
}

type trailResult struct {
	lines []trailLine
	next  string
	gaps  []float64 // seconds between consecutive events read, newest first
}

// maxTrailLines bounds a trail page (and what the model is shown); maxTrailEvents the events read for one page.
const (
	maxTrailLines  = 50
	maxTrailEvents = 2000
)

// trail reads an actor's events newest first from before (a keyset cursor, zero for the newest) and collapses
// runs. Ordered by (ts, id): the actor index holds both, so the read is an index walk, never a sort.
func (s *Server) trail(ctx context.Context, site int64, key string, from, to time.Time, before cursor, pages int) (trailResult, error) {
	tr := trailResult{lines: []trailLine{}}
	args := []any{site, key, ms(from), ms(to)}
	cond := ""
	if before.ts > 0 {
		cond, args = " AND (ts < ? OR (ts = ? AND id < ?))", append(args, before.ts, before.ts, before.id)
	}
	// One event past the bound: whether more follow, and whether the last run goes on into them.
	limit := maxTrailEvents * pages
	rows, err := s.query(ctx, `SELECT id, ts, source, kind, name, template, status FROM event
		WHERE site_id = ? AND ip_key = ? AND ts >= ? AND ts < ?`+cond+` ORDER BY ts DESC, id DESC LIMIT `+
		strconv.Itoa(limit+1), args...)
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
		if read > limit {
			if n := len(tr.lines); n > 0 && tr.lines[n-1].What == what {
				tr.lines[n-1].Continues = true
			}
			tr.next = last.String()
			break
		}
		if prevTS != 0 {
			tr.gaps = append(tr.gaps, float64(prevTS-ts)/1000)
		}
		prevTS = ts
		if n := len(tr.lines); n > 0 && tr.lines[n-1].What == what {
			l := &tr.lines[n-1]
			l.Count++
			l.gaps = append(l.gaps, float64(l.fromMS-ts)/1000)
			l.From, l.fromMS = iso(time.UnixMilli(ts)), ts
			l.IntervalS = round1(median(l.gaps))
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
	if err := rows.Err(); err != nil {
		return tr, err
	}
	_ = rows.Close()
	// A later page: its first run may go on from the previous page's last event, the cursor's.
	if before.id > 0 && len(tr.lines) > 0 {
		var source, kind, name, tmpl string
		var status int
		err := s.row(ctx, `SELECT source, kind, name, template, status FROM event WHERE id = ? AND site_id = ?
			AND ip_key = ?`, before.id, site, key).Scan(&source, &kind, &name, &tmpl, &status)
		switch {
		case err == nil:
			what, _ := describe(source, kind, name, tmpl, status)
			tr.lines[0].Continued = what == tr.lines[0].What
		case !errors.Is(err, sql.ErrNoRows):
			return tr, err
		}
	}
	return tr, nil
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

// readingInput is what the model reads about one actor, and the actor's own statements by evidence id (what the
// dashboard shows for a cited id). The ids name what a statement says, not its figures (requests:refused-most,
// pace:fast), so a reading cached under the behaviour's signature resolves against each actor's own statements.
// The model sees no address, network name or date: the reading is shared by every actor with the signature.
func (s *Server) readingInput(site int64, row visitorRow, counts map[string]int64, tr trailResult) (insight.ReadingInput, map[string]string) {
	in := insight.ReadingInput{SiteID: site, Key: row.Key, Class: row.Class, Client: row.Client,
		NetworkKind: row.NetworkKind, Signals: row.Signals}
	for _, l := range tr.lines {
		line := l.What
		if l.Count > 1 {
			line = fmt.Sprintf("%s ×%d, %.1f s apart", l.What, l.Count, l.IntervalS)
		}
		in.Trail = append(in.Trail, line)
	}
	shown := map[string]string{}
	ev := func(id, model, own string) {
		in.Evidence = append(in.Evidence, insight.Evidence{ID: id, Text: model})
		shown[id] = own
	}
	if row.Requests != nil && *row.Requests > 0 {
		t := fmt.Sprintf("%d requests, %.0f %% refused", *row.Requests, *row.RefusedShare)
		ev("requests:refused-"+bucket(*row.RefusedShare, 10, 90, "none", "some", "most"), t, t)
	}
	if row.Views > 0 {
		t := fmt.Sprintf("%d page views in %d visits", row.Views, deref(row.Visits))
		ev("views", t, t)
	}
	whats := topActions(counts)
	if len(whats) > 0 {
		t := fmt.Sprintf("Most frequent: %s (%d×)", whats[0], counts[whats[0]])
		ev("top", t, t)
		t = fmt.Sprintf("%d different actions", len(whats))
		ev("variety:"+bucket(float64(len(whats)), 2, 6, "one", "few", "many"), t, t)
	}
	pace := ""
	if len(tr.gaps) >= 3 {
		med := median(tr.gaps)
		pace = bucket(med, 2, 60, "fast", "steady", "slow")
		t := fmt.Sprintf("Median %.1f s between its latest %d events", med, len(tr.gaps)+1)
		ev("pace:"+pace, t, t)
	}
	ev("client:"+clientFamily(row.Client), "Client "+row.Client, "Client "+row.Client)
	if row.Network != "" {
		kind := row.NetworkKind
		if kind == "" {
			kind = "other"
		}
		ev("network:"+kind, "Network kind "+kind, strings.TrimSpace("Network "+row.Network+" ("+kind+")"))
	}
	for _, sg := range row.Signals {
		ev("signal:"+slug(sg), "Signal: "+sg, "Signal: "+sg)
	}
	// The signature: what it does (top actions, counts on a log scale), how fast, with what kind of client from what
	// kind of network, and the statements the reading may cite. Two addresses doing the same share it, and with it
	// the reading.
	sig := []string{row.Class, clientFamily(row.Client), row.NetworkKind, "pace:" + pace}
	for _, w := range whats[:min(8, len(whats))] {
		sig = append(sig, fmt.Sprintf("%s:%d", w, int(math.Log2(float64(counts[w])+1))))
	}
	ids := make([]string, 0, len(in.Evidence))
	for _, e := range in.Evidence {
		ids = append(ids, e.ID)
	}
	slices.Sort(ids)
	sig = append(sig, ids...)
	sum := sha256.Sum256([]byte(strings.Join(sig, "\x00")))
	in.Signature = hex.EncodeToString(sum[:16])
	return in, shown
}

// bucket names where v falls: below lo, below hi, or above.
func bucket(v, lo, hi float64, low, mid, high string) string {
	switch {
	case v < lo:
		return low
	case v < hi:
		return mid
	}
	return high
}

// slug is a signal as an id: lower case, digits dropped (a client's version, a count), the rest joined by dashes.
func slug(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r)
	}), "-")
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
	ci, err := s.classOne(ctx, q.site.ID, key, q.p.From(), q.p.Now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	row := s.visitorRow(q, a, ci)
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
	counts, err := s.actions(ctx, q.site.ID, key, q.p.From(), q.p.Bounds[q.p.n()])
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// "What it does" as the table's row has it: the computed sentence, the model's when its reading resolves.
	row.Does.Text = doesText(row, counts)
	resp := map[string]any{"key": key, "class": row.Class, "network": row.Network, "network_kind": row.NetworkKind,
		"country": row.Country, "client": row.Client, "signals": row.Signals, "requests": row.Requests,
		"addresses": addrs, "hours": hours, "trail": tr.lines, "trail_next": tr.next, "reading": nil,
		"client_gen": nil, "does": &row.Does}
	if s.Model != nil && s.Model.Configured() {
		one := []visitorRow{row}
		s.labelClients(ctx, q.site.ID, one)
		resp["client_gen"] = one[0].ClientGen
		// The reading reads the newest trail page, whatever page the user is on.
		latest := tr
		if before.ts > 0 {
			if latest, err = s.trail(ctx, q.site.ID, key, q.p.From(), q.p.Bounds[q.p.n()], cursor{}, 1); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		in, shown := s.readingInput(q.site.ID, row, counts, latest)
		in.Queue = true // the user opened this actor
		if rd, ok := s.Model.Readings(ctx, []insight.ReadingInput{in}, true)[key]; ok {
			// Shown only when every fact it cites exists for this actor, with this actor's figures and span.
			if evidence, ok := resolve(rd, shown); ok {
				if rd.Summary != "" {
					sum := rd.Summary
					row.Does.Gen = &sum
				}
				related, err := s.related(ctx, q, key, row.Class)
				if err != nil {
					s.fail(w, r, err)
					return
				}
				resp["reading"] = map[string]any{"verdict": rd.Verdict, "suspicious": insight.Suspicious(rd.Verdict),
					"evidence": evidence, "related": related,
					"basis": map[string]any{"from": iso(time.UnixMilli(a.first)), "to": iso(time.UnixMilli(a.last))}}
			}
		}
	}
	apix.JSON(w, http.StatusOK, resp)
}

// actorOne sums one key over the range (nil when it has no events there).
func (s *Server) actorOne(ctx context.Context, q *query, key string) (*actor, error) {
	var a actor
	var first, last *int64
	err := s.row(ctx, `SELECT COUNT(DISTINCT CASE WHEN sid != '' THEN sid END),
		COALESCE(SUM(kind = 'page_view'), 0), COALESCE(SUM(source = 'server'), 0),
		COALESCE(SUM(source = 'server' AND status >= 400), 0), COALESCE(SUM(source = 'browser' AND `+eventKinds+`), 0),
		MIN(ts), MAX(ts), COALESCE(SUM(source = 'browser'), 0) > 0
		FROM event WHERE site_id = ? AND ip_key = ? AND ts >= ? AND ts < ?`,
		q.site.ID, key, ms(q.p.From()), ms(q.p.Bounds[q.p.n()])).Scan(&a.visits, &a.views, &a.requests, &a.refused,
		&a.events, &first, &last, &a.browser)
	if err != nil || last == nil {
		return nil, err
	}
	a.key, a.first, a.last = key, *first, *last
	// The latest event's client, network, country and class, as the table's row shows them.
	if err := s.row(ctx, `SELECT ua, asn, country, cls FROM event WHERE site_id = ? AND ip_key = ? AND ts <= ?
		ORDER BY ts DESC LIMIT 1`, q.site.ID, key, a.last).Scan(&a.ua, &a.asn, &a.country, &a.cls); err != nil {
		return nil, err
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
