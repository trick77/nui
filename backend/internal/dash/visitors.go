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
	"maps"
	"math"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/trick77/trakk/backend/internal/agg"
	"github.com/trick77/trakk/backend/internal/apix"
	"github.com/trick77/trakk/backend/internal/classify"
	"github.com/trick77/trakk/backend/internal/insight"
	"github.com/trick77/trakk/backend/internal/store"
)

// actors sums every address key over the range, a piece (a UTC day at most) at a time from its source. Visits are
// distinct sessions over the whole range: a session crossing a piece boundary is one visit. Raw pieces count (key,
// session) pairs, never per piece; an aggregate day counts its sessions less those carried from the day before
// (when that day is in the range). A raw day after an aggregate one first learns the day before's pairs, uncounted;
// with that raw gone, it counts its sessions less the carried ones its own stored rows hold (carriedOn). An aggregate
// day without rows in the audience's parts counted no session: age retention takes a day's per-address rows with
// its raw, its other rows stay.
func (s *Server) actors(ctx context.Context, q *query, from, to time.Time) (map[string]*actor, error) {
	pl, err := s.planOf(ctx, q)
	if err != nil {
		return nil, err
	}
	out := map[string]*actor{}
	// A hash per (key, session) seen: a few bytes each, not the strings.
	sessions := map[uint64]struct{}{}
	var counted time.Time // where the last piece read ends
	prevAgg := false      // that piece was an aggregate day
	for i, pc := range pieces(from, to) {
		wasAgg, wasRaw, held := false, false, false
		raw := func() error {
			wasRaw = true
			if !prevAgg || !counted.Equal(pc[0]) {
				return s.actorPiece(ctx, q, pc[0], pc[1], sessions, out)
			}
			if pl.rawOK(ms(pc[0])/dayMS - 1) {
				if err := s.actorSessions(ctx, q, pc[0].Add(-readSpan), pc[0], sessions, nil); err != nil {
					return err
				}
				return s.actorPiece(ctx, q, pc[0], pc[1], sessions, out)
			}
			carried, err := s.carriedOn(ctx, q, ms(pc[0])/dayMS, "")
			if err != nil {
				return err
			}
			piece := map[string]*actor{}
			if err := s.actorPiece(ctx, q, pc[0], pc[1], sessions, piece); err != nil {
				return err
			}
			for k, a := range piece {
				a.visits -= min(carried[k], a.visits)
				actorOf(out, k).add(a)
			}
			return nil
		}
		if i == 1 && headCut(from, pl) {
			err = raw()
		} else {
			err = pl.read(ctx, pc[0], pc[1], readSpan, !q.p.agg, func() (bool, error) {
				ok, n, err := s.actorDay(ctx, q, ms(pc[0])/dayMS, stitch(counted, pc[0]), out)
				wasAgg, held = ok, n
				return ok, err
			}, raw)
		}
		if err != nil {
			return nil, err
		}
		if wasAgg && held || wasRaw {
			counted, prevAgg = pc[1], wasAgg
		}
	}
	return out, nil
}

// carriedOn is the sessions a day's stored rows carry from the day before, per address key (key's alone, or every
// key's for ""), the audience's parts: what a raw read of the day cannot learn once the day before's raw is gone.
// Only from complete rows (built, not mid-write or cut by the cap), their state read in the same statement; a day
// without them (unbuilt: the size cap's last days) counts a session crossing its midnight on both days.
func (s *Server) carriedOn(ctx context.Context, q *query, day int64, key string) (map[string]int64, error) {
	cond, args := "", []any{q.site.ID, day}
	if key != "" {
		cond, args = " AND t.ip_key = ?3", append(args, key)
	}
	rows, err := s.query(ctx, `SELECT t.ip_key, SUM(t.carried) FROM agg_day d JOIN agg_addr t ON t.site_id = d.site_id
		AND t.day = d.day WHERE d.site_id = ?1 AND d.day = ?2 AND d.frozen = 1 AND d.dirty != `+
		strconv.Itoa(store.AggWriting)+` AND t.carried > 0`+cond+partCond(q.aud)+` GROUP BY t.ip_key`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// stitch: an aggregate day's visits leave out the sessions carried from the day before only when that day was
// counted, read right before it; a day before not read (no raw, no build) counted none of them.
func stitch(counted, lo time.Time) bool { return !counted.IsZero() && counted.Equal(lo) }

// headCut: a span starting off a UTC midnight (24h, 7d outside UTC) reads its first whole day raw under every
// source. That day's carried sessions may have begun before the span, uncounted, and the aggregates cannot tell
// which: subtracting them all undercounts visits.
func headCut(from time.Time, pl *plan) bool {
	day := ms(from)/dayMS + 1
	if from.Equal(from.Truncate(readSpan)) || !pl.rawOK(day) {
		return false
	}
	pl.s.traced(false, false)
	return true
}

// actorPiece adds one raw piece's sums per key to out.
func (s *Server) actorPiece(ctx context.Context, q *query, from, to time.Time, sessions map[uint64]struct{},
	out map[string]*actor) error {
	if err := s.actorSessions(ctx, q, from, to, sessions, out); err != nil {
		return err
	}
	// One MAX only: SQLite then takes the bare columns (ua, asn, country, cls) from the row holding it, the key's
	// latest event. A second MIN or MAX would make that row arbitrary.
	rows, err := s.query(ctx, `SELECT ip_key,
		SUM(kind = 'page_view'), SUM(source = 'server'), SUM(source = 'server' AND status >= 400),
		SUM(source = 'browser' AND `+eventKinds+`), MAX(ts), ua, asn, country, cls,
		SUM(source = 'browser') > 0, SUM(cls = 1) > 0
		FROM event WHERE site_id = ? AND ts >= ? AND ts < ? AND `+audWhere(q.aud)+` GROUP BY ip_key`,
		q.site.ID, ms(from), ms(to))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var a actor
		if err := rows.Scan(&a.key, &a.views, &a.requests, &a.refused, &a.events, &a.last, &a.ua,
			&a.asn, &a.country, &a.cls, &a.browser, &a.human); err != nil {
			return err
		}
		actorOf(out, a.key).add(&a)
	}
	return rows.Err()
}

// add adds one piece's sums of the key; the latest event's client, network and country win, as the visitor detail
// shows them (a piece without a network or country keeps the earlier one's).
func (g *actor) add(a *actor) {
	g.views += a.views
	g.requests += a.requests
	g.refused += a.refused
	g.events += a.events
	g.visits += a.visits
	g.browser = g.browser || a.browser
	g.human = g.human || a.human
	if g.first == 0 || (a.first != 0 && a.first < g.first) {
		g.first = a.first
	}
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

// addrCols are the agg_addr columns an actor is read from (t), in scanAddr's order.
const addrCols = `t.ip_key, t.views, t.requests, t.refused, t.actions, t.sessions, t.carried, t.browser, t.human,
	t.first_ts, t.last_ts, t.ua, t.asn, t.country, t.cls`

// addrRow is one agg_addr row as an actor's piece; carried apart, subtracted only when the day before is counted.
type addrRow struct {
	actor
	carried int64
}

// scanAddr reads addrCols from a left join: ok false for the join's empty side.
func scanAddr(rows *sql.Rows, lead ...any) (addrRow, bool, error) {
	var key, ua, country sql.NullString
	var views, requests, refused, actions, sessions, carried, first, last, asn, cls sql.NullInt64
	var browser, human sql.NullBool
	if err := rows.Scan(append(lead, &key, &views, &requests, &refused, &actions, &sessions, &carried, &browser,
		&human, &first, &last, &ua, &asn, &country, &cls)...); err != nil {
		return addrRow{}, false, err
	}
	return addrRow{actor: actor{key: key.String, views: views.Int64, requests: requests.Int64, refused: refused.Int64,
		events: actions.Int64, visits: sessions.Int64, browser: browser.Bool, human: human.Bool, first: first.Int64,
		last: last.Int64, ua: ua.String, asn: asn.Int64, country: country.String, cls: int(cls.Int64)},
		carried: carried.Int64}, key.Valid, nil
}

// dayActor folds one day's rows of a key (a part each) into its piece: the sums added, the client and network of the
// part holding the day's latest event, as a raw piece takes the day's latest row. stitch: the day before is counted,
// so the sessions carried from it are not new.
func dayActor(parts []addrRow, stitch bool) *actor {
	var a actor
	var latest *addrRow
	for i := range parts {
		r := &parts[i]
		a.views += r.views
		a.requests += r.requests
		a.refused += r.refused
		a.events += r.events
		a.visits += r.visits
		if stitch {
			a.visits -= r.carried
		}
		a.browser = a.browser || r.browser
		a.human = a.human || r.human
		if a.first == 0 || r.first < a.first {
			a.first = r.first
		}
		if latest == nil || r.last > latest.last {
			latest = r
		}
	}
	a.key, a.last, a.ua, a.asn, a.country, a.cls = latest.key, latest.last, latest.ua, latest.asn, latest.country,
		latest.cls
	return &a
}

// actorDay adds one day's agg_addr rows (the audience's parts) to out, ok false when the day is not clean in the
// statement's snapshot; held is whether it had any.
func (s *Server) actorDay(ctx context.Context, q *query, day int64, stitch bool,
	out map[string]*actor) (ok, held bool, err error) {
	rows, err := s.query(ctx, `SELECT d.ok, `+addrCols+aggFrom("agg_addr", "t.day = ?2"+partCond(q.aud)),
		q.site.ID, day)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = rows.Close() }()
	byKey := map[string][]addrRow{}
	for rows.Next() {
		r, has, err := scanAddr(rows, &ok)
		if err != nil {
			return false, false, err
		}
		if !ok {
			return false, false, nil
		}
		if has {
			byKey[r.key] = append(byKey[r.key], r)
		}
	}
	if err := rows.Err(); err != nil || !ok {
		return false, false, err
	}
	for key, parts := range byKey {
		actorOf(out, key).add(dayActor(parts, stitch))
	}
	return true, len(byKey) > 0, nil
}

func actorOf(m map[string]*actor, key string) *actor {
	g, ok := m[key]
	if !ok {
		g = &actor{key: key}
		m[key] = g
	}
	return g
}

// actorSessions counts the sessions of one bucket that no earlier bucket had, per key; with out nil it only learns
// them.
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
		if out != nil {
			actorOf(out, key).visits++
		}
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
	ci.signals = respace(ci.signals)
	return ci, nil
}

// classDays is the days one read of actor_class spans: a year of a busy site's rows in one statement is a long read.
const classDays = 31

// classes is each key's latest stored class and reasons within the range, read a month at a time, oldest first.
func (s *Server) classes(ctx context.Context, site int64, from, to time.Time) (map[string]classInfo, error) {
	out := map[string]classInfo{}
	last := ms(to) / dayMS
	for lo := ms(from) / dayMS; lo <= last; lo += classDays {
		if err := s.classPiece(ctx, site, lo, min(lo+classDays-1, last), out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Server) classPiece(ctx context.Context, site, first, last int64, out map[string]classInfo) error {
	rows, err := s.query(ctx, `SELECT ip_key, class, reasons FROM actor_class
		WHERE site_id = ? AND day >= ? AND day <= ? ORDER BY day`, site, first, last)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k, c, reasons string
		if err := rows.Scan(&k, &c, &reasons); err != nil {
			return err
		}
		ci := classInfo{class: c}
		_ = json.Unmarshal([]byte(reasons), &ci.signals)
		ci.signals = respace(ci.signals)
		out[k] = ci
	}
	return rows.Err()
}

const dayMS = int64(24 * time.Hour / time.Millisecond)

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

	ua     string
	values valueFact // the raw values behind its top action, for the model's reading
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
		apix.Error(w, http.StatusBadRequest, classError)
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
	span := visitorCursor{From: ms(q.p.win.From), To: ms(q.p.Now)}
	if after != nil {
		span.From, span.To = after.From, after.To
	}
	ctx := r.Context()
	needle := foldSpaces(strings.ToLower(v.Get("q")))
	snapKey := fmt.Sprintf("%d|%d|%s|%s|%s|%s|%d|%d", s.Source, q.site.ID, q.aud, sortBy, class, needle, span.From,
		span.To)
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
	resp := map[string]any{"total": len(rows), "next": "", "from": span.From}
	if end < len(rows) {
		last := page[len(page)-1]
		resp["next"] = visitorCursor{From: span.From, To: span.To, Value: sortValue(last, sortBy), Key: last.Key}.String()
	}
	s.readRows(ctx, q, span, page, readQueue(rows, page))
	resp["rows"] = page
	pl, err := s.planOf(ctx, q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// What it does and the reading go back this far; the sums cover the whole span.
	if resp["detail_since"], err = pl.detailSince(ctx, q.p.win.From); err != nil {
		s.fail(w, r, err)
		return
	}
	apix.JSON(w, http.StatusOK, resp)
}

// visitorRows is the table over a span: every actor, filtered and sorted (by the sort value, then the key).
func (s *Server) visitorRows(ctx context.Context, q *query, span visitorCursor, sortBy, class, needle string) ([]visitorRow, error) {
	from, to := time.UnixMilli(span.From), time.UnixMilli(span.To)
	// The classes beside the sums: they need nothing of each other.
	var cls map[string]classInfo
	var clsErr error
	var wg sync.WaitGroup
	wg.Go(func() { cls, clsErr = s.classes(ctx, q.site.ID, from, to) })
	all, err := s.actors(ctx, q, from, to)
	wg.Wait()
	if err = cmp.Or(err, clsErr); err != nil {
		return nil, err
	}
	labelled, err := s.labelMatches(ctx, needle)
	if err != nil {
		return nil, err
	}
	rows := make([]visitorRow, 0, len(all))
	names := map[string]string{}
	for _, a := range all {
		row := s.visitorRow(q, a, cls[a.key], names)
		if class != "" && row.Class != class {
			continue
		}
		// The search covers what the row shows: the model's client label too (labelClients shows it on unknown
		// automated rows).
		hay := foldSpaces(strings.ToLower(row.Key + " " + row.Network + " " + row.Client + " " + row.Country))
		if needle != "" && !strings.Contains(hay, needle) && (row.Class != "unknown" || !labelled[row.ua]) {
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
// table shows no label then). A label's en spaces (apix.Sep) match a plain one, as in foldSpaces; the model's text
// holds no other space but the plain one (llm's clip).
func (s *Server) labelMatches(ctx context.Context, needle string) (map[string]bool, error) {
	if needle == "" || s.Model == nil || !s.Model.Configured() {
		return nil, nil
	}
	rows, err := s.query(ctx, `SELECT value FROM llm_label WHERE kind = 'ua'
		AND instr(replace(lower(label), char(8194), ' '), ?) > 0`, needle)
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

// actorClass is the class of a key without a pass's verdict: its latest event's, a pending one counted with the
// key's people when it has any (a visitor's page request waits for the pass, its beacon is human at once).
func actorClass(a *actor) string {
	if a.cls == store.ClsPending && a.human {
		return "human"
	}
	return classOf(a.cls)
}

// clientName is classify.Client, a variable for the tests that count its calls.
var clientName = classify.Client

// visitorRow is an actor's table row. names holds the client name per user agent already derived (nil: none kept):
// classify.Client's patterns took about 40 µs a call, 2.3 s for a year's 59k rows sharing 18 user agents.
func (s *Server) visitorRow(q *query, a *actor, ci classInfo, names map[string]string) visitorRow {
	client, ok := names[a.ua]
	if !ok {
		client = clientName(a.ua)
		if names != nil {
			names[a.ua] = client
		}
	}
	row := visitorRow{Key: a.key, Display: a.key, Country: a.country, Client: client,
		Views: a.views, Events: a.events, Last: iso(time.UnixMilli(a.last)), Signals: ci.signals,
		Class: ci.class, ua: a.ua}
	if row.Class == "" {
		row.Class = actorClass(a)
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
	pl, err := s.planOf(ctx, q)
	if err != nil {
		return
	}
	// What it does and the reading are raw events: the span from detail_since on.
	since := pl.rawFrom(from)
	// Every row's month windows share one limiter: the rows' actions read side by side, the rest in turn.
	type acts struct {
		counts map[string]int64
		tmpls  map[string]string
		err    error
	}
	got := make([]acts, len(rows))
	lim := make(limiter, rawReaders)
	var wg sync.WaitGroup
	for i := range rows {
		wg.Go(func() {
			a := &got[i]
			a.counts, a.tmpls, a.err = s.actions(ctx, lim, q.site.ID, rows[i].Key, since, to, nil)
		})
	}
	wg.Wait()
	ins := make([]insight.ReadingInput, 0, len(rows))
	shown := map[string]map[string]string{}
	for i := range rows {
		counts, tmpls, err := got[i].counts, got[i].tmpls, got[i].err
		if err != nil {
			continue
		}
		rows[i].Does.Text = doesText(rows[i], counts)
		if rows[i].values, err = s.variety(ctx, q.site.ID, rows[i].Key, since, to, counts, tmpls); err != nil {
			continue
		}
		tr, err := s.trail(ctx, q.site.ID, rows[i].Key, since, to, cursor{})
		if err != nil {
			continue
		}
		rd, _, err := s.readingRow(ctx, q, rows[i], nil, classInfo{}, from, to, since)
		if err != nil {
			continue
		}
		rd.values = rows[i].values
		in, facts := s.readingInput(q.site.ID, rd, counts, tr)
		in.Queue = queue[rows[i].Key]
		ins = append(ins, in)
		shown[rows[i].Key] = facts
	}
	if s.Model == nil || !s.Model.Configured() {
		return
	}
	readings := s.Model.Readings(ctx, ins, false)
	for i := range rows {
		if rd, ok := readings[rows[i].Key]; ok && rd.Summary != "" {
			if _, ok := resolve(rd, shown[rows[i].Key]); ok {
				sum := rd.Summary
				rows[i].Does.Gen = &sum
			}
		}
	}
	s.labelClients(ctx, q.site.ID, rows)
}

// labelClients sets the model's label for an unknown automated client's user agent (one per distinct value, kept
// for good), the same in the table and the visitor detail. The model must be configured. A browser's user agent keeps
// its parsed name ("Chrome · Windows"): the model only said "Chrome web browser" or "Chrome browser" for it, no
// two alike.
func (s *Server) labelClients(ctx context.Context, site int64, rows []visitorRow) {
	var uas []string
	for _, r := range rows {
		if r.Class == "unknown" && r.ua != "" && !browserClient(r.Client) && !slices.Contains(uas, r.ua) {
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

// byActor pins a key's events read newest first to the actor index. Unpinned, the planner walks the site's time
// index (it holds the same order) and tests every event of the span for the key: a year for a key with few events
// in it took 1.3 s warm, a month of the site per variety read.
const byActor = "INDEXED BY event_site_actor"

// rawReaders is how many statements over single actors' month windows one request runs side by side. A year of a
// busy scraper is 350k events, each a lookup into the event table: the table page's actions one window after another
// took 1.6 s warm and 22 of a cold call's 26 s. Cold, the reads wait on the disk: 6 side by side took 5.1-6.3 s, 8
// 4.9-5.3 s, 12 3.7 s (warm alike); each holds a connection of the pool.
const rawReaders = 8

// limiter bounds a request's month-window statements run side by side (each holds a connection of the pool). Not
// the request's every statement: the detail reads its hours beside them, so it holds up to rawReaders+1 connections;
// the table's classes run beside its sums, before the windows.
type limiter chan struct{}

// each runs f(0) … f(n-1) side by side, as many at a time as lim allows, and returns the first error by index.
func (lim limiter) each(n int, f func(i int) error) error {
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			lim <- struct{}{}
			defer func() { <-lim }()
			errs[i] = f(i)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// actions counts an actor's events per action ("GET /x → 401", "view /a") over the whole span, and names each
// server request action's template. The month windows are independent statements, read side by side under lim.
// addrs, when not nil, is set to the key's distinct addresses: the same lookups into the event table (the actor index
// holds no address), so one pass, not a second one (a busy scraper's year: 0.37 s warm).
func (s *Server) actions(ctx context.Context, lim limiter, site int64, key string, from, to time.Time,
	addrs *int64) (map[string]int64, map[string]string, error) {
	type part struct {
		whats []string
		ns    []int64
		tmpls map[string]string
		ips   []string
	}
	cols := "source, kind, name, template, status, op"
	if addrs != nil {
		cols += ", ip"
	}
	wins := windows(ms(from), ms(to), actorSpan)
	parts := make([]part, len(wins))
	err := lim.each(len(wins), func(i int) error {
		p := &parts[i]
		p.tmpls = map[string]string{}
		rows, err := s.query(ctx, `SELECT `+cols+`, COUNT(*) FROM event WHERE site_id = ? AND ip_key = ? AND ts >= ?
			AND ts < ? GROUP BY `+cols, site, key, wins[i][0], wins[i][1])
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var source, kind, name, tmpl, op, ip string
			var status int
			var n int64
			dest := []any{&source, &kind, &name, &tmpl, &status, &op, &n}
			if addrs != nil {
				dest = []any{&source, &kind, &name, &tmpl, &status, &op, &ip, &n}
			}
			if err := rows.Scan(dest...); err != nil {
				return err
			}
			what, _ := describe(source, kind, name, agg.Endpoint(tmpl, op), status)
			p.whats, p.ns = append(p.whats, what), append(p.ns, n)
			if source == "server" { // a request's action is its endpoint and status: one template each
				p.tmpls[what] = tmpl
			}
			if addrs != nil {
				p.ips = append(p.ips, ip)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, nil, err
	}
	out, tmpls := map[string]int64{}, map[string]string{}
	var ips []string
	for _, p := range parts {
		for j, what := range p.whats {
			out[what] += p.ns[j]
		}
		maps.Copy(tmpls, p.tmpls)
		ips = append(ips, p.ips...)
	}
	if addrs != nil {
		slices.Sort(ips)
		*addrs = int64(len(slices.Compact(ips)))
	}
	return out, tmpls, nil
}

// valuesSample is how many of the top action's newest raw paths the variety statement weighs.
const valuesSample = 500

// valueFact is a computed statement about the raw values behind an actor's top action, no figures: whether one
// script repeats a request or walks a range is not in the template the model sees. Zero when there is none.
type valueFact struct{ id, text string }

// variety weighs the raw paths behind the actor's most frequent action, when that is a server request, within the
// span (its newest valuesSample):
// all one value, mostly distinct and in sequence (a range walked), mostly distinct in no order, a few repeated, or a
// mix. A template without a variable part, or too few requests, says nothing.
func (s *Server) variety(ctx context.Context, site int64, key string, from, to time.Time, counts map[string]int64,
	tmpls map[string]string) (valueFact, error) {
	whats := topActions(counts)
	if len(whats) == 0 || counts[whats[0]] < 3 {
		return valueFact{}, nil
	}
	tmpl, ok := tmpls[whats[0]]
	if !ok {
		return valueFact{}, nil // the top action is no server request: no requested values to weigh
	}
	if span := to.Add(-actorSpan); span.After(from) {
		from = span // the newest month at most: one statement on the actor index
	}
	rows, err := s.query(ctx, `SELECT path FROM event `+byActor+` WHERE site_id = ? AND ip_key = ? AND ts >= ?
		AND ts < ? AND source = 'server' AND template = ? ORDER BY ts DESC LIMIT `+strconv.Itoa(valuesSample), site, key,
		ms(from), ms(to), tmpl)
	if err != nil {
		return valueFact{}, err
	}
	defer func() { _ = rows.Close() }()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return valueFact{}, err
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return valueFact{}, err
	}
	return varietyOf(paths, tmpl), nil
}

// varietyOf is the statement over paths, newest first, raw requests of the template tmpl.
func varietyOf(paths []string, tmpl string) valueFact {
	if len(paths) < 3 {
		return valueFact{}
	}
	distinct := map[string]bool{}
	for _, p := range paths {
		distinct[p] = true
	}
	bare := tmpl // a page's template is a path; a request's has its method first ("GET /x/{id}")
	if m, rest, ok := strings.Cut(tmpl, " "); ok && !strings.HasPrefix(m, "/") {
		bare = rest
	}
	switch share := float64(len(distinct)) / float64(len(paths)); {
	case len(distinct) == 1 && paths[0] == bare:
		return valueFact{} // a fixed path: no value to vary
	case len(distinct) == 1:
		return valueFact{"values:same", "Every request asked for the same value"}
	case share >= 0.9 && sequential(paths):
		return valueFact{"values:walk", "Almost every request asked for a new value, in sequence: a range walked"}
	case share >= 0.9:
		return valueFact{"values:distinct", "Almost every request asked for a new value, in no order"}
	case share <= 0.2:
		return valueFact{"values:few", "A few values asked for again and again"}
	}
	return valueFact{"values:mixed", "Some values asked for again, others new"}
}

// sequential reports whether the varying part of paths (newest first) ran in sequence: as numbers or addresses, at
// least nine in ten steps going the same way.
func sequential(paths []string) bool {
	pre, suf := shared(paths)
	vals := make([]float64, 0, len(paths))
	for _, p := range paths {
		if pre+suf > len(p) {
			return false
		}
		v, ok := valueOf(p[pre : len(p)-suf])
		if !ok {
			return false
		}
		vals = append(vals, v)
	}
	up, down := 0, 0
	for i := 1; i < len(vals); i++ {
		switch {
		case vals[i] < vals[i-1]: // newest first: an older value below a newer one is a walk upwards
			up++
		case vals[i] > vals[i-1]:
			down++
		}
	}
	steps := len(vals) - 1
	return float64(max(up, down)) >= 0.9*float64(steps)
}

// shared is the length of the prefix and the suffix every path has, each backed off to the edge of a number or
// address it cut into ("/ip/1.2.3.4" and "/ip/1.2.3.5" share "/ip/", not "/ip/1.2.3.").
func shared(paths []string) (pre, suf int) {
	first := paths[0]
	pre, suf = len(first), len(first)
	for _, o := range paths[1:] {
		pre = min(pre, commonPrefix(first, o))
		suf = min(suf, commonSuffix(first, o))
	}
	const numeric = "0123456789abcdefABCDEF.:"
	for pre > 0 && strings.IndexByte(numeric, first[pre-1]) >= 0 {
		pre--
	}
	for suf > 0 && strings.IndexByte(numeric, first[len(first)-suf]) >= 0 {
		suf--
	}
	return pre, suf
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func commonSuffix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	return n
}

// valueOf reads a varying part as a number: an integer, or an IP address by its bits.
func valueOf(v string) (float64, bool) {
	if n, err := strconv.ParseInt(strings.TrimLeft(v, "ASas"), 10, 64); err == nil {
		return float64(n), true
	}
	a, err := netip.ParseAddr(v)
	if err != nil {
		return 0, false
	}
	var f float64
	for _, b := range a.AsSlice() {
		f = f*256 + float64(b)
	}
	return f, true
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
	phrase := map[string]string{"human": "Person", "api": "API client", "bot": "Known bot", "monitor": "Uptime probe",
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
	// base is the line without a vital's value: what runs join on and what the model reads (a reading is shared by
	// every actor with the signature, so one actor's figures must not shape it). value is the line with it, shown
	// only for a single event: a run of one vital holds several.
	base, value string
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
func (s *Server) trail(ctx context.Context, site int64, key string, from, to time.Time, before cursor) (trailResult, error) {
	tr := trailResult{lines: []trailLine{}}
	args := []any{site, key, ms(from), ms(to)}
	cond := ""
	if before.ts > 0 {
		cond, args = " AND (ts < ? OR (ts = ? AND id < ?))", append(args, before.ts, before.ts, before.id)
	}
	// One event past the bound: whether more follow, and whether the last run goes on into them.
	limit := maxTrailEvents
	rows, err := s.query(ctx, `SELECT id, ts, source, kind, name, `+endpointSQL+`, status, `+vitalValue+` FROM event
		`+byActor+` WHERE site_id = ? AND ip_key = ? AND ts >= ? AND ts < ?`+cond+` ORDER BY ts DESC, id DESC LIMIT `+
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
		var value sql.NullFloat64
		if err := rows.Scan(&id, &ts, &source, &kind, &name, &tmpl, &status, &value); err != nil {
			return tr, err
		}
		what, k := describe(source, kind, name, tmpl, status)
		if read > limit {
			if n := len(tr.lines); n > 0 && tr.lines[n-1].base == what {
				tr.lines[n-1].Continues = true
			}
			tr.next = last.String()
			break
		}
		if prevTS != 0 {
			tr.gaps = append(tr.gaps, float64(prevTS-ts)/1000)
		}
		prevTS = ts
		if n := len(tr.lines); n > 0 && tr.lines[n-1].base == what {
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
		tr.lines = append(tr.lines, trailLine{T: t, Kind: k, What: what, Count: 1, From: t, To: t, fromMS: ts, toMS: ts,
			base: what, value: withValue(what, name, value)})
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
		err := s.row(ctx, `SELECT source, kind, name, `+endpointSQL+`, status FROM event WHERE id = ? AND site_id = ?
			AND ip_key = ?`, before.id, site, key).Scan(&source, &kind, &name, &tmpl, &status)
		switch {
		case err == nil:
			what, _ := describe(source, kind, name, tmpl, status)
			tr.lines[0].Continued = what == tr.lines[0].base
		case !errors.Is(err, sql.ErrNoRows):
			return tr, err
		}
	}
	// A single event shows its value; a run, even one a page bound split, shows none (the UI joins split parts by
	// their line).
	for i := range tr.lines {
		if l := &tr.lines[i]; l.Count == 1 && !l.Continues && !l.Continued {
			l.What = l.value
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

// vitalValue is a web vital's measured value as SQL, NULL for any other event and for a value that is no number:
// the sender writes it, and a string there must not fail the page.
const vitalValue = `CASE WHEN kind = 'vital' AND json_type(props, '$.p.value') IN ('integer', 'real')
	THEN json_extract(props, '$.p.value') END`

// withValue appends a vital's value to its line: "vital LCP 2.1 s", "vital CLS 0.042". Only the log and a single
// trail event show it, never the model or actions: those group by the line, where a value would make every vital
// an action of its own. A name the tracker never sends or a value no browser measures shows none. CLS keeps the
// tracker's three decimals: with two, a recorded 0.004 read as no shift.
func withValue(what, name string, v sql.NullFloat64) string {
	if !v.Valid || v.Float64 < 0 {
		return what
	}
	switch name {
	case "CLS":
		if v.Float64 <= 100 {
			return what + " " + strconv.FormatFloat(v.Float64, 'f', 3, 64)
		}
	case "LCP", "FCP", "INP", "TTFB": // milliseconds
		switch {
		case v.Float64 < 999.5:
			return fmt.Sprintf("%s %d ms", what, int(math.Round(v.Float64)))
		case v.Float64 <= 3_600_000:
			return fmt.Sprintf("%s %.1f s", what, v.Float64/1000)
		}
	}
	return what
}

// readingInput is what the model reads about one actor, and the actor's own statements by evidence id (what the
// dashboard shows for a cited id). The ids name what a statement says, not its figures (requests:refused-most,
// pace:fast), so a reading cached under the behaviour's signature resolves against each actor's own statements.
// The model sees no address, network name or date: the reading is shared by every actor with the signature.
func (s *Server) readingInput(site int64, row visitorRow, counts map[string]int64, tr trailResult) (insight.ReadingInput, map[string]string) {
	in := insight.ReadingInput{SiteID: site, Key: row.Key, Class: row.Class, Client: row.Client,
		NetworkKind: row.NetworkKind, Signals: row.Signals}
	for _, l := range tr.lines {
		line := l.base
		if l.Count > 1 {
			line = fmt.Sprintf("%s ×%d, %.1f s apart", l.base, l.Count, l.IntervalS)
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
	if row.values.id != "" {
		ev(row.values.id, row.values.text, row.values.text)
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

// respace spells a stored reason's separators as apix.Sep: a day classified before it keeps its row.
func respace(reasons []string) []string {
	for i, r := range reasons {
		reasons[i] = apix.Respace(r)
	}
	return reasons
}

// foldSpaces writes every run of Unicode space as one plain space: a person types "Safari · macOS" with ordinary
// spaces, the dashboard shows it with apix.Sep's en spaces.
func foldSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// browserJoin joins a browser's two parts in its client family. classify.Client writes any other name as a product
// token ([A-Za-z][A-Za-z0-9._-]*, no "+") or, when none starts it, as raw text that does not start with a letter,
// so "Chrome+macOS" is no tool's family and no shown separator reaches an id.
const browserJoin = "+"

// noClient is the family of an empty name: stable, and no other client's (it starts with a letter and holds a "+").
const noClient = "no+client"

// clientFamily is a client as an id, for an evidence id and the reading signature: a browser's parts joined by
// browserJoin, so a change of the shown separator changes no id (and orphans no cached reading); any other name as
// written, its version dropped (curl/8.4 and curl/8.5 behave alike), case, digits and punctuation kept.
func clientFamily(c string) string {
	if b, o, ok := strings.Cut(c, apix.Sep); ok && browserClient(c) {
		return b + browserJoin + o
	}
	if strings.TrimSpace(c) == "" {
		return noClient
	}
	if i := strings.IndexByte(c, '/'); i > 0 {
		return c[:i]
	}
	return c
}

// browserClient reports whether c is a browser's client name, its browser and OS around the separator.
func browserClient(c string) bool {
	b, o, ok := strings.Cut(c, apix.Sep)
	return ok && browserPart(b) && browserPart(o)
}

// browserPart reports whether s is one of classify.ParseUA's browser or OS names: ASCII letters and spaces, a letter
// first.
func browserPart(s string) bool {
	for i, r := range s {
		letter := r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z'
		if !letter && (i == 0 || r != ' ') {
			return false
		}
	}
	return s != ""
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
	if err := q.holdFrom(v.Get("from")); err != nil {
		apix.Error(w, http.StatusBadRequest, err.Error())
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
	ci, err := s.classOne(ctx, q.site.ID, key, q.p.win.From, q.p.Now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	row := s.visitorRow(q, a, ci, nil)
	// The sums cover the range; the trail, the hours, the addresses, the actions and the reading are raw events,
	// which reach back to detail_since.
	pl, err := s.planOf(ctx, q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	since := pl.rawFrom(q.p.win.From)
	tr, err := s.trail(ctx, q.site.ID, key, since, q.p.win.End, before)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The hours beside the actions and addresses.
	var hours []int
	var addrs int64
	var hoursErr error
	var wg sync.WaitGroup
	wg.Go(func() { hours, hoursErr = s.hours(ctx, q, key, since, q.p.win.End) })
	counts, tmpls, err := s.actions(ctx, make(limiter, rawReaders), q.site.ID, key, since, q.p.win.End, &addrs)
	wg.Wait()
	if err = cmp.Or(hoursErr, err); err != nil {
		s.fail(w, r, err)
		return
	}
	// "What it does" as the table's row has it: the computed sentence, the model's when its reading resolves.
	row.Does.Text = doesText(row, counts)
	detailSince, err := pl.detailSince(ctx, q.p.win.From)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	resp := map[string]any{"key": key, "class": row.Class, "network": row.Network, "network_kind": row.NetworkKind,
		"country": row.Country, "client": row.Client, "signals": row.Signals, "requests": row.Requests,
		"addresses": addrs, "hours": hours, "trail": tr.lines, "trail_next": tr.next, "reading": nil,
		"client_gen": nil, "does": &row.Does, "from": ms(q.p.win.From), "detail_since": detailSince}
	if s.Model != nil && s.Model.Configured() {
		one := []visitorRow{row}
		s.labelClients(ctx, q.site.ID, one)
		resp["client_gen"] = one[0].ClientGen
		// The reading reads the newest trail page, whatever page the user is on.
		latest := tr
		if before.ts > 0 {
			if latest, err = s.trail(ctx, q.site.ID, key, since, q.p.win.End, cursor{}); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		// Its figures are the raw span's too: evidence never mixes the range's sums with the raw span's counts.
		rd, basis, err := s.readingRow(ctx, q, row, a, ci, q.p.win.From, q.p.win.End, since)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		a = basis
		if rd.values, err = s.variety(ctx, q.site.ID, key, since, q.p.win.End, counts, tmpls); err != nil {
			s.fail(w, r, err)
			return
		}
		row.values = rd.values
		in, shown := s.readingInput(q.site.ID, rd, counts, latest)
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

// readingRow is the row the model reads about an actor, and the actor behind it: the table's, unless the span
// [from, to) reaches before the raw events (since), then the raw span's own sums, as its evidence counts.
func (s *Server) readingRow(ctx context.Context, q *query, row visitorRow, a *actor, ci classInfo, from, to,
	since time.Time) (visitorRow, *actor, error) {
	if !since.After(from) {
		return row, a, nil
	}
	ra, err := s.actorIn(ctx, q, row.Key, since, to, true)
	if err != nil || ra == nil {
		return row, a, err
	}
	rr := s.visitorRow(q, ra, ci, nil)
	rr.Class, rr.Signals = row.Class, row.Signals
	return rr, ra, nil
}

// actorOne sums one key over the range (nil when it has no events there).
func (s *Server) actorOne(ctx context.Context, q *query, key string) (*actor, error) {
	return s.actorIn(ctx, q, key, q.p.win.From, q.p.win.End, false)
}

// actorPart is a run of days of one key's span read from one source.
type actorPart struct {
	lo, hi time.Time
	day    int64 // an aggregate day's; the first day of a raw run
	agg    bool
}

// actorIn sums one key over [from, to) (nil when it has no events there): the days the aggregates serve from
// agg_addr on its key index, one statement for the whole span; every run of other days raw on the actor index, a
// statement each. Visits are stitched as in actors. rawOnly reads every day raw (the reading's figures, which must
// match its raw evidence).
func (s *Server) actorIn(ctx context.Context, q *query, key string, from, to time.Time, rawOnly bool) (*actor, error) {
	pl, err := s.planOf(ctx, q)
	if err != nil {
		return nil, err
	}
	var parts []actorPart
	for i, pc := range pieces(from, to) {
		h := readRaw
		if !rawOnly && (i != 1 || !headCut(from, pl)) {
			if h, err = pl.pick(ctx, pc[0], pc[1], readSpan, !q.p.agg); err != nil {
				return nil, err
			}
			if h == readRaw && pl.src != SourceRaw {
				s.traced(false, pc[0].Equal(pc[0].Truncate(readSpan)) && pc[1].Equal(pc[1].Truncate(readSpan)))
			}
		}
		switch {
		case h == readNone:
		case h == readRaw && len(parts) > 0 && !parts[len(parts)-1].agg && parts[len(parts)-1].hi.Equal(pc[0]):
			parts[len(parts)-1].hi = pc[1]
		default:
			parts = append(parts, actorPart{lo: pc[0], hi: pc[1], day: ms(pc[0]) / dayMS, agg: h == readAgg})
		}
	}
	days, err := s.actorDays(ctx, q, key, parts)
	if err != nil {
		return nil, err
	}
	a := &actor{key: key}
	seen := map[string]bool{}
	var rawLast int64 // the latest raw event's time, when the latest event is raw
	for i, pt := range parts {
		// Where the part before ends: parts are the pieces read. An aggregate day without the key's rows counted none
		// of its sessions (age retention takes per-address rows with the raw).
		var counted time.Time
		if i > 0 && (!parts[i-1].agg || len(days[parts[i-1].day]) > 0) {
			counted = parts[i-1].hi
		}
		if pt.agg {
			if day, ok := days[pt.day]; ok {
				a.add(dayActor(day, stitch(counted, pt.lo)))
			}
			continue
		}
		var carried int64
		switch {
		case i == 0 || !parts[i-1].agg || !stitch(counted, pt.lo):
		case pl.rawOK(pt.day - 1):
			// The day before was counted from the aggregates: its sessions are not new here.
			if err := s.actorSids(ctx, q.site.ID, key, pt.lo.Add(-readSpan), pt.lo, seen, nil); err != nil {
				return nil, err
			}
		default: // its raw gone: the sessions this day's stored rows carry from it
			c, err := s.carriedOn(ctx, q, pt.day, key)
			if err != nil {
				return nil, err
			}
			carried = c[key]
		}
		var r actor
		var first, last *int64
		if err := s.row(ctx, `SELECT COALESCE(SUM(kind = 'page_view'), 0), COALESCE(SUM(source = 'server'), 0),
			COALESCE(SUM(source = 'server' AND status >= 400), 0), COALESCE(SUM(source = 'browser' AND `+eventKinds+`), 0),
			MIN(ts), MAX(ts), COALESCE(SUM(source = 'browser'), 0) > 0, COALESCE(SUM(cls = 1), 0) > 0
			FROM event WHERE site_id = ? AND ip_key = ? AND ts >= ? AND ts < ?`, q.site.ID, key, ms(pt.lo), ms(pt.hi)).
			Scan(&r.views, &r.requests, &r.refused, &r.events, &first, &last, &r.browser, &r.human); err != nil {
			return nil, err
		}
		if last == nil {
			continue
		}
		if err := s.actorSids(ctx, q.site.ID, key, pt.lo, pt.hi, seen, &r.visits); err != nil {
			return nil, err
		}
		r.visits -= min(carried, r.visits)
		r.first, r.last = *first, *last
		if r.last >= a.last {
			rawLast = r.last
		}
		a.add(&r)
	}
	if a.last == 0 {
		return nil, nil
	}
	if rawLast == a.last {
		// The latest event's client, network, country and class, as the table's row shows them.
		if err := s.row(ctx, `SELECT ua, asn, country, cls FROM event WHERE site_id = ? AND ip_key = ? AND ts <= ?
			ORDER BY ts DESC LIMIT 1`, q.site.ID, key, a.last).Scan(&a.ua, &a.asn, &a.country, &a.cls); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// actorSids adds the key's sessions in [from, to) to seen, counting the new ones into n (nil: only learn them).
func (s *Server) actorSids(ctx context.Context, site int64, key string, from, to time.Time, seen map[string]bool,
	n *int64) error {
	rows, err := s.query(ctx, `SELECT DISTINCT sid FROM event WHERE site_id = ? AND ip_key = ? AND ts >= ? AND ts < ?
		AND sid != ''`, site, key, ms(from), ms(to))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return err
		}
		if !seen[sid] && n != nil {
			*n++
		}
		seen[sid] = true
	}
	return rows.Err()
}

// actorDays reads one key's agg_addr rows for the aggregate days among parts, by day: a statement on its key index
// per month of days (spanDays), every day's state read with them, so a day rebuilt meanwhile is read raw (SourceAgg:
// an error).
func (s *Server) actorDays(ctx context.Context, q *query, key string, parts []actorPart) (map[int64][]addrRow, error) {
	out := map[int64][]addrRow{}
	clean := map[int64]bool{}
	read := func(first, last int64) error {
		rows, err := s.query(ctx, `SELECT d.day, d.frozen = 1 AND d.dirty = 0, `+addrCols+` FROM agg_day d
			LEFT JOIN agg_addr t INDEXED BY agg_addr_key ON t.site_id = d.site_id AND t.ip_key = ? AND t.day = d.day
			WHERE d.site_id = ? AND d.day >= ? AND d.day <= ?`, key, q.site.ID, first, last)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var day int64
			var ok bool
			r, has, err := scanAddr(rows, &day, &ok)
			if err != nil {
				return err
			}
			clean[day] = ok
			if has && ok {
				out[day] = append(out[day], r)
			}
		}
		return rows.Err()
	}
	first, last := int64(-1), int64(-1)
	for _, pt := range parts {
		if !pt.agg {
			continue
		}
		if first >= 0 && pt.day >= first+spanDays {
			if err := read(first, last); err != nil {
				return nil, err
			}
			first = -1
		}
		if first < 0 {
			first = pt.day
		}
		last = pt.day
	}
	if first >= 0 {
		if err := read(first, last); err != nil {
			return nil, err
		}
	}
	pl, _ := s.planOf(ctx, q)
	for i := range parts {
		pt := &parts[i]
		if !pt.agg {
			continue
		}
		switch {
		case clean[pt.day]:
			s.traced(true, true)
		case pl.src == SourceAgg:
			return nil, fmt.Errorf("%w: site %d, %s rebuilt while read", ErrNoAggregates, q.site.ID,
				time.UnixMilli(pt.day*dayMS).UTC().Format(time.DateOnly))
		default: // rebuilt since the plan: raw, which a rebuild never changes
			pt.agg = false
		}
	}
	return out, nil
}

// hours is the actor's activity per hour of the viewer's day over [from, to), as levels 0-4: index only, one
// statement.
func (s *Server) hours(ctx context.Context, q *query, key string, from, to time.Time) ([]int, error) {
	rows, err := s.query(ctx, `SELECT ts / 3600000, COUNT(*) FROM event
		WHERE site_id = ? AND ip_key = ? AND ts >= ? AND ts < ? GROUP BY 1`, q.site.ID, key, ms(from), ms(to))
	if err != nil {
		return nil, err
	}
	counts := make([]int64, 24)
	for rows.Next() {
		var h, n int64
		if err := rows.Scan(&h, &n); err != nil {
			_ = rows.Close()
			return nil, err
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
	return levels, nil
}

// related are other keys of the same class in the same network block (/24, /48) within the range: the operator's
// other addresses.
func (s *Server) related(ctx context.Context, q *query, key, class string) ([]string, error) {
	block, ok := blockOf(key)
	if !ok {
		return []string{}, nil
	}
	rows, err := s.query(ctx, `SELECT DISTINCT ip_key FROM actor_class WHERE site_id = ? AND day >= ?
		AND class = ? AND ip_key != ? LIMIT 5000`, q.site.ID, ms(q.p.win.From)/dayMS, class, key)
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
