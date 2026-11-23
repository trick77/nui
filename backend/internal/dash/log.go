package dash

import (
	"context"
	"database/sql"
	"math"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/ship/kit/ratelimit"
	"github.com/trick77/ship/kit/version"

	"github.com/trick77/stakeout/backend/internal/apix"
	"github.com/trick77/stakeout/backend/internal/classify"
	"github.com/trick77/stakeout/backend/internal/ingest"
	"github.com/trick77/stakeout/backend/internal/store"
)

// logPage is the rows one log page holds.
const logPage = 100

var statusRE = regexp.MustCompile(`^([1-5])(?:xx|[0-9]{2})$`)

type logRow struct {
	T      any    `json:"t"`
	Addr   string `json:"addr"`
	Class  string `json:"class"`
	Kind   string `json:"kind"`
	What   string `json:"what"`
	Client string `json:"client"`
	Source string `json:"source"`
}

// handleLog is the raw event log, newest first, keyset-paginated on (ts, id): the time index walks it without a
// sort. Domain and range apply; aud and cmp do not.
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request, q *query) {
	v := r.URL.Query()
	where := []string{"site_id = ?"}
	args := []any{q.site.ID}
	if a := strings.TrimSpace(v.Get("addr")); a != "" {
		if _, err := netip.ParseAddr(a); err == nil {
			where, args = append(where, "ip_key = ?", "ip = ?"), append(args, ratelimit.Key(a), netip.MustParseAddr(a).Unmap().String())
		} else if p, err := netip.ParsePrefix(a); err == nil && p.Addr().Is6() && p.Bits() == 64 {
			// Keys are addresses or /64s: any other prefix would match nothing, silently.
			where, args = append(where, "ip_key = ?"), append(args, p.Masked().String())
		} else {
			apix.Error(w, http.StatusBadRequest, "addr is an address or a /64")
			return
		}
	}
	// One network by its AS number: a network spike in Noteworthy opens the log on it.
	if a := strings.TrimSpace(v.Get("asn")); a != "" {
		n, err := strconv.ParseInt(strings.TrimPrefix(strings.ToUpper(a), "AS"), 10, 64)
		if err != nil || n < 1 || n > math.MaxUint32 {
			apix.Error(w, http.StatusBadRequest, "asn is an AS number")
			return
		}
		where, args = append(where, "asn = ?"), append(args, n)
	}
	// A substring of the template, the raw path or the event name (an error's message is its name): the UI opens
	// the log from an error row with the error's label.
	if p := v.Get("path"); p != "" {
		where = append(where, "(instr(template, ?) > 0 OR instr(path, ?) > 0 OR instr(name, ?) > 0)")
		args = append(args, p, p, p)
	}
	if c := v.Get("class"); c != "" {
		if !slices.Contains(classOrder, c) {
			apix.Error(w, http.StatusBadRequest, "class must be human, bot, monitor or unknown")
			return
		}
		if c == "unknown" { // pending counted as unknown, as classOf does
			where = append(where, "cls IN (0, 4)")
		} else {
			where, args = append(where, "cls = ?"), append(args, store.ClassCode(c))
		}
	}
	if k := v.Get("kind"); k != "" {
		switch k {
		case "browser", "server":
			where, args = append(where, "source = ?"), append(args, k)
		default:
			where, args = append(where, "kind = ?"), append(args, k)
		}
	}
	// A class (2xx … 5xx) or one code; server events only, browser events carry no status.
	if st := v.Get("status"); st != "" {
		m := statusRE.FindStringSubmatch(st)
		switch {
		case m == nil:
			apix.Error(w, http.StatusBadRequest, "status is a class (4xx) or a code (404)")
			return
		case strings.HasSuffix(st, "xx"):
			c, _ := strconv.Atoi(m[1])
			where, args = append(where, "source = 'server' AND status >= ? AND status < ?"), append(args, c*100, c*100+100)
		default:
			c, _ := strconv.Atoi(st)
			where, args = append(where, "source = 'server' AND status = ?"), append(args, c)
		}
	}
	cur, ok := parseCursor(v.Get("cursor"))
	if !ok {
		apix.Error(w, http.StatusBadRequest, "bad cursor")
		return
	}
	cond := strings.Join(where, " AND ") + " AND ts >= ? AND ts < ?"
	ctx := r.Context()
	pl, err := s.planOf(ctx, q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Raw events only: from the oldest one on, not a window per empty day before it.
	from, to := ms(pl.rawFrom(q.p.win.From)), ms(q.p.win.End)
	// The total and the page are read a window at a time: a filter the indexes cannot answer (a path substring)
	// otherwise reads the whole range in one statement, seconds long on a year of events.
	var total int64
	for _, win := range windows(from, to, scanWindow) {
		var n int64
		if err := s.row(ctx, `SELECT COUNT(*) FROM event WHERE `+cond, append(slices.Clone(args), win[0], win[1])...).
			Scan(&n); err != nil {
			s.fail(w, r, err)
			return
		}
		total += n
	}
	if cur.ts > 0 {
		to = min(to, cur.ts+1)
	}
	out := []logRow{}
	next := ""
	var last cursor
	for _, win := range windows(from, to, scanWindow) {
		pageArgs := append(slices.Clone(args), win[0], win[1])
		pageCond := cond
		if cur.ts > 0 {
			pageCond += " AND (ts < ? OR (ts = ? AND id < ?))"
			pageArgs = append(pageArgs, cur.ts, cur.ts, cur.id)
		}
		more, err := s.logRows(ctx, pageCond, pageArgs, logPage+1-len(out))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		for _, lr := range more {
			if len(out) == logPage {
				next = last.String()
				break
			}
			out = append(out, lr.row)
			last = lr.at
		}
		if next != "" {
			break
		}
	}
	apix.JSON(w, http.StatusOK, map[string]any{"rows": out, "total": total, "next": next,
		"detail_since": pl.detailSince()})
}

// scanWindow is the longest span one log or footer statement reads: a day of a busy site is short, a year is not.
const scanWindow = 24 * time.Hour

// countWindow is the span of one footer COUNT: an index-only count, so a week of a busy site is still short.
const countWindow = 7 * 24 * time.Hour

// windows cuts [from, to) (unix ms) into spans of at most step, newest first.
func windows(from, to int64, step time.Duration) [][2]int64 {
	var out [][2]int64
	for hi := to; hi > from; hi -= step.Milliseconds() {
		out = append(out, [2]int64{max(from, hi-step.Milliseconds()), hi})
	}
	return out
}

type cursorRow struct {
	row logRow
	at  cursor
}

// logRows reads up to limit log rows matching cond, newest first.
func (s *Server) logRows(ctx context.Context, cond string, args []any, limit int) ([]cursorRow, error) {
	rows, err := s.query(ctx, `SELECT id, ts, ip, cls, source, kind, name, template, status, ua, `+vitalValue+`
		FROM event WHERE `+cond+` ORDER BY ts DESC, id DESC LIMIT `+strconv.Itoa(limit), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []cursorRow
	for rows.Next() {
		var id, ts int64
		var ip, source, kind, name, tmpl, ua string
		var cls, status int
		var value sql.NullFloat64
		if err := rows.Scan(&id, &ts, &ip, &cls, &source, &kind, &name, &tmpl, &status, &ua, &value); err != nil {
			return nil, err
		}
		what, k := describe(source, kind, name, tmpl, status)
		what = withValue(what, name, value)
		out = append(out, cursorRow{row: logRow{T: iso(time.UnixMilli(ts)), Addr: ip, Class: classOf(cls), Kind: k,
			What: what, Client: classify.Client(ua), Source: source}, at: cursor{ts, id}})
	}
	return out, rows.Err()
}

// handleDomain is the footer: collector counters over the range, sources, storage, rules and reference data.
func (s *Server) handleDomain(w http.ResponseWriter, r *http.Request, q *query) {
	ctx := r.Context()
	from := q.p.From().Unix() / 3600
	var c struct {
		Accepted      int64 `json:"accepted"`
		RateLimited   int64 `json:"rate_limited"`
		Rejected      int64 `json:"rejected"`
		Dropped       int64 `json:"dropped"`
		UnknownDomain int64 `json:"unknown_domain"`
	}
	// The site's own rows, plus unknown domains from the siteless row: a request naming no configured site cannot be
	// attributed, so every site's footer shows the collector-wide count.
	if err := s.row(ctx, `SELECT
		COALESCE(SUM(CASE WHEN site_id = ? THEN accepted END), 0),
		COALESCE(SUM(CASE WHEN site_id = ? THEN rate_limited END), 0),
		COALESCE(SUM(CASE WHEN site_id = ? THEN rejected END), 0),
		COALESCE(SUM(CASE WHEN site_id = ? THEN dropped END), 0),
		COALESCE(SUM(unknown_domain), 0)
		FROM ingest_stat WHERE site_id IN (?, 0) AND hour >= ?`,
		q.site.ID, q.site.ID, q.site.ID, q.site.ID, q.site.ID, from).
		Scan(&c.Accepted, &c.RateLimited, &c.Rejected, &c.Dropped, &c.UnknownDomain); err != nil {
		s.fail(w, r, err)
		return
	}
	if s.Counters != nil { // the current hour, not flushed yet
		for _, p := range s.Counters.Pending() {
			if p.Hour < from {
				continue
			}
			if p.SiteID == q.site.ID {
				c.Accepted += p.Accepted
				c.RateLimited += p.RateLimited
				c.Rejected += p.Rejected
				c.Dropped += p.Dropped
			}
			if p.SiteID == q.site.ID || p.SiteID == 0 {
				c.UnknownDomain += p.UnknownDomain
			}
		}
	}
	browser, server, err := s.lastSeen(ctx, q.site.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var first, events int64
	if err := s.row(ctx, `SELECT COALESCE(MIN(ts), 0) FROM event WHERE site_id = ?`, q.site.ID).
		Scan(&first); err != nil {
		s.fail(w, r, err)
		return
	}
	// Counted a window at a time from the oldest event on: one COUNT over 25 months is a read seconds long.
	if first > 0 {
		for _, win := range windows(first, max(s.now().UnixMilli(), first)+1, countWindow) {
			var n int64
			if err := s.row(ctx, `SELECT COUNT(*) FROM event WHERE site_id = ? AND ts >= ? AND ts < ?`,
				q.site.ID, win[0], win[1]).Scan(&n); err != nil {
				s.fail(w, r, err)
				return
			}
			events += n
		}
	}
	used, err := s.DB.UsedBytes(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The first figure: the oldest built hour or the oldest raw event, whichever is older.
	var firstHour sql.NullInt64
	if err := s.row(ctx, `SELECT MIN(hour) FROM agg_hour WHERE site_id = ?`, q.site.ID).Scan(&firstHour); err != nil {
		s.fail(w, r, err)
		return
	}
	figures := first
	if firstHour.Valid && (figures == 0 || firstHour.Int64*hourMS < figures) {
		figures = firstHour.Int64 * hourMS
	}
	// feed: the site has a server feed configured, so a null server source is one that has not delivered yet.
	sources := map[string]any{"browser": source(browser, s.now()), "server": nil, "feed": q.site.HasFeed,
		"script": "/stakeout.js", "build": version.String(), "first": nil}
	if q.site.HasFeed {
		sources["server"] = source(server, s.now())
	}
	var oldest, figuresSince any
	if first > 0 {
		oldest = iso(time.UnixMilli(first))
	}
	if figures > 0 {
		figuresSince = iso(time.UnixMilli(figures))
		sources["first"] = figuresSince
	}
	// Raw events are kept as long as everything else until their own retention ships: the months, in days.
	rawDays := int(s.now().Sub(s.now().AddDate(0, -s.RetentionMonths, 0)).Hours() / 24)
	rules := map[string]any{"rate": ingest.RateRule(), "batch": ingest.BatchRule(), "tz": q.tz,
		"botlist": map[string]any{"services": 0, "updated": nil}, "networks": map[string]any{"updated": nil},
		"llm": map[string]any{"calls_today": 0, "budget": 0, "configured": false}}
	if s.Ref != nil {
		if b := s.Ref.Bots(); b != nil {
			rules["botlist"] = map[string]any{"services": len(b.Providers), "updated": iso(b.Updated)}
		}
		if n := s.Ref.Networks(); n != nil {
			rules["networks"] = map[string]any{"updated": iso(n.Updated)}
		}
	}
	if s.Model != nil {
		calls, budget := s.Model.Usage(ctx, q.site.ID)
		rules["llm"] = map[string]any{"calls_today": calls, "budget": budget, "configured": s.Model.Configured()}
	}
	apix.JSON(w, http.StatusOK, map[string]any{
		"counters": c,
		"sources":  sources,
		"storage": map[string]any{"events": events, "bytes": used, "retention_months": s.RetentionMonths,
			"oldest": oldest, "identity": "address + user agent, plus browser id", "raw_days": rawDays,
			"figures_since": figuresSince},
		"rules": rules,
	})
}

// source is a source's state: receiving within the last hour, idle before, nil (null) when it never delivered.
func source(last, now time.Time) map[string]any {
	if last.IsZero() {
		return nil // never delivered: null, as for a source the site does not have
	}
	state := "receiving"
	if now.Sub(last) > time.Hour {
		state = "idle"
	}
	return map[string]any{"state": state, "last": iso(last)}
}
