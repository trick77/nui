package dash

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/trakk/backend/internal/apix"
)

// Where a script error comes from, as far as the browser says. The UI words them.
const (
	// originWithheld: the browser hides a muted script's error behind "Script error.", without file, line or stack.
	// Measured on uncaught throws in a timer: Chromium 159 hides another host's script loaded without CORS; WebKit
	// 27.2 (desktop Playwright) one loaded with crossorigin and Access-Control-Allow-Origin too, and the
	// SecurityError past its 100 pushState calls per 10 s, which the page's own script throws readably without us:
	// trakk.js wraps pushState. The browser still hides that one; trakk.js, whose wrapper sees it, reports it itself.
	originWithheld  = "withheld"
	originExtension = "extension" // a browser extension, or another script that is not the page's
	originOwn       = "own"       // the site's own host
	originOther     = "other"     // another host, whose name is known
	originUnknown   = "unknown"   // no location reported (a rejected promise)
)

const (
	errorRows   = 6  // the rows of each breakdown
	errorRecent = 10 // the latest occurrences listed
	maxErrorKey = 256
	maxPlace    = 300
	maxFrames   = 8
)

// errorWhere is where one script error happened.
type errorWhere struct {
	Origin string `json:"origin"`
	Host   string `json:"host"`  // originOther's
	Place  string `json:"place"` // file:line:col without query, "" when not reported
}

// errorPlace is an error's props as SQL: its file, line and column. The sender writes them: another JSON type there
// reads as none and must not fail the page (a whole number past int64 would fail an integer scan).
const errorPlace = `CASE WHEN kind = 'js_error' AND json_type(props, '$.p.src') = 'text'
	THEN json_extract(props, '$.p.src') ELSE '' END,
	CASE WHEN kind = 'js_error' AND json_type(props, '$.p.line') IN ('integer', 'real')
	THEN json_extract(props, '$.p.line') END,
	CASE WHEN kind = 'js_error' AND json_type(props, '$.p.col') IN ('integer', 'real')
	THEN json_extract(props, '$.p.col') END`

// errorStack is an error's type and its stack (a JSON array) as SQL, "" for what is none.
const errorStack = `CASE WHEN json_type(props, '$.p.type') = 'text' THEN json_extract(props, '$.p.type') ELSE '' END,
	CASE WHEN json_type(props, '$.p.stack') = 'array' THEN json_extract(props, '$.p.stack') ELSE '' END`

// stackFrames is the frames of a stored stack: its strings, as many as the script sends.
func stackFrames(raw string) []string {
	var all []any
	if raw == "" || json.Unmarshal([]byte(raw), &all) != nil {
		return nil
	}
	var out []string
	for _, x := range all {
		if f, ok := x.(string); ok && f != "" && len(out) < maxFrames {
			out = append(out, truncate(f, maxPlace))
		}
	}
	return out
}

// maskedError reports whether an error's message is the browser's stand-in for one it hides.
func maskedError(name string) bool {
	return strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(name), "."), "script error")
}

// locate says where an error on domain comes from.
func locate(domain, name, src string, line, col sql.NullFloat64) errorWhere {
	none := errorWhere{Origin: originUnknown}
	if maskedError(name) {
		none.Origin = originWithheld
	}
	// A blob's URL holds its page's after the scheme.
	u, err := url.Parse(strings.TrimPrefix(src, "blob:"))
	if src == "" || err != nil || u.Scheme == "" {
		return none
	}
	pos := ""
	if n, ok := position(line); ok {
		pos = ":" + n
		if n, ok := position(col); ok {
			pos += ":" + n
		}
	}
	path := u.EscapedPath()
	switch {
	case strings.HasSuffix(u.Scheme, "-extension"), u.Scheme == "webkit-masked-url", u.Scheme == "user-script":
		return errorWhere{Origin: originExtension, Place: truncate(u.Scheme+"://"+u.Host+path, maxPlace) + pos}
	case strings.EqualFold(u.Hostname(), domain):
		return errorWhere{Origin: originOwn, Place: truncate(cmp.Or(path, "/"), maxPlace) + pos}
	case u.Host != "":
		return errorWhere{Origin: originOther, Host: truncate(u.Hostname(), maxPlace),
			Place: truncate(u.Host+path, maxPlace) + pos}
	}
	return none
}

// position is a line or column as text; ok false for one no browser reports.
func position(v sql.NullFloat64) (string, bool) {
	if !v.Valid || v.Float64 < 1 || v.Float64 > 1e9 || v.Float64 != math.Trunc(v.Float64) {
		return "", false
	}
	return strconv.FormatInt(int64(v.Float64), 10), true
}

// errorCount is one row of a breakdown.
type errorCount struct {
	Key      string      `json:"key"`
	Where    *errorWhere `json:"where,omitempty"`
	Count    int64       `json:"count"`
	Visitors int64       `json:"visitors"`
	ids      uniq
}

// errorTally counts occurrences and visitors per key.
type errorTally map[string]*errorCount

func (t errorTally) add(key, visitor string) *errorCount {
	c, ok := t[key]
	if !ok {
		c = &errorCount{Key: key}
		t[key] = c
	}
	c.Count++
	c.ids.add(visitor)
	return c
}

// errorBreakdown is a tally's largest rows, and how many more keys there are.
type errorBreakdown struct {
	Rows []*errorCount `json:"rows"`
	More int           `json:"more"`
}

func (t errorTally) top() errorBreakdown {
	out := make([]*errorCount, 0, len(t))
	for _, c := range t {
		c.Visitors = c.ids.count()
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b *errorCount) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Key, b.Key))
	})
	n := min(len(out), errorRows)
	return errorBreakdown{Rows: out[:n], More: len(out) - n}
}

// errorSeen is one occurrence.
type errorSeen struct {
	T      any        `json:"t"`
	Addr   string     `json:"addr"`
	Key    string     `json:"key"`
	Class  string     `json:"class"`
	Client string     `json:"client"`
	Page   string     `json:"page"`
	Where  errorWhere `json:"where"`
}

// errorDetail is everything one message's events say.
type errorDetail struct {
	Count    int64          `json:"count"`
	Visitors int64          `json:"visitors"`
	Sessions int64          `json:"sessions"`
	Viewers  int64          `json:"viewers"` // visitors with a page view in the same span
	Share    *float64       `json:"share"`   // visitors of viewers in percent, null without viewers
	First    any            `json:"first"`
	Last     any            `json:"last"`
	Places   errorBreakdown `json:"places"`
	Pages    errorBreakdown `json:"pages"`
	Clients  errorBreakdown `json:"clients"`
	Classes  errorBreakdown `json:"classes"`
	Recent   []errorSeen    `json:"recent"`
	// Type and Stack are the newest occurrence's that has a stack; older scripts sent none, a hidden error has none.
	Type        string   `json:"type"`
	Stack       []string `json:"stack"`
	DetailSince any      `json:"detail_since"`
}

// handleError is one script error, by its message: how many it hit, where, on what. Raw events only (no aggregate
// keeps a place), so it reaches back to detail_since, a day per statement.
func (s *Server) handleError(w http.ResponseWriter, r *http.Request, q *query) {
	key := r.URL.Query().Get("key")
	if key == "" || len(key) > maxErrorKey {
		apix.Error(w, http.StatusBadRequest, "key is a script error's message")
		return
	}
	ctx := r.Context()
	pl, err := s.planOf(ctx, q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	from, to := ms(pl.rawFrom(q.p.win.From)), ms(q.p.win.End)
	d, err := s.errorDetail(ctx, q, key, from, to)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if d.DetailSince, err = pl.detailSince(ctx, q.p.win.From); err != nil {
		s.fail(w, r, err)
		return
	}
	apix.JSON(w, http.StatusOK, d)
}

// errorDetail reads one message's events over [from, to), newest first.
func (s *Server) errorDetail(ctx context.Context, q *query, key string, from, to int64) (*errorDetail, error) {
	d := &errorDetail{Recent: []errorSeen{}, Stack: []string{}}
	var visitors, sessions, viewers uniq
	var first, last int64
	places, pages, clients, classes := errorTally{}, errorTally{}, errorTally{}, errorTally{}
	names := map[string]string{} // a client's name is parsed once per user agent
	day := func(lo, hi int64) error {
		rows, err := s.query(ctx, `SELECT ts, ip, ip_key, cls, visitor, sid, ua, path, template, `+errorPlace+`,
			`+errorStack+` FROM event WHERE site_id = ? AND ts >= ? AND ts < ? AND kind = 'js_error' AND name = ? AND `+
			audWhere(q.aud)+` ORDER BY ts DESC, id DESC`, q.site.ID, lo, hi, key)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var ts int64
			var cls int
			var ip, ipKey, visitor, sid, ua, path, tmpl, src, typ, stack string
			var line, col sql.NullFloat64
			if err := rows.Scan(&ts, &ip, &ipKey, &cls, &visitor, &sid, &ua, &path, &tmpl, &src, &line, &col, &typ,
				&stack); err != nil {
				return err
			}
			if len(d.Stack) == 0 {
				if f := stackFrames(stack); f != nil {
					d.Type, d.Stack = truncate(typ, maxPlace), f
				}
			}
			client, ok := names[ua]
			if !ok {
				client = clientName(ua)
				names[ua] = client
			}
			at := locate(q.site.Domain, key, src, line, col)
			d.Count++
			visitors.add(visitor)
			if sid != "" {
				sessions.add(sid)
			}
			if last == 0 {
				last = ts
			}
			first = ts
			places.add(at.Origin+" "+at.Place, visitor).Where = &at
			pages.add(tmpl, visitor)
			clients.add(client, visitor)
			classes.add(classOf(cls), visitor)
			if len(d.Recent) < errorRecent {
				d.Recent = append(d.Recent, errorSeen{T: iso(time.UnixMilli(ts)), Addr: ip, Key: ipKey,
					Class: classOf(cls), Client: client, Page: path, Where: at})
			}
		}
		return rows.Err()
	}
	// Who could have hit it: everyone who saw a page.
	views := func(lo, hi int64) error {
		rows, err := s.query(ctx, `SELECT DISTINCT visitor FROM event WHERE site_id = ? AND ts >= ? AND ts < ?
			AND kind = 'page_view' AND `+audWhere(q.aud), q.site.ID, lo, hi)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return err
			}
			viewers.add(v)
		}
		return rows.Err()
	}
	for _, win := range windows(from, to, scanWindow) {
		if err := day(win[0], win[1]); err != nil {
			return nil, fmt.Errorf("error events: %w", err)
		}
		if err := views(win[0], win[1]); err != nil {
			return nil, fmt.Errorf("error viewers: %w", err)
		}
	}
	if d.Count > 0 {
		d.First, d.Last = iso(time.UnixMilli(first)), iso(time.UnixMilli(last))
	}
	d.Visitors, d.Sessions, d.Viewers = visitors.count(), sessions.count(), viewers.count()
	if d.Viewers > 0 { // a visitor whose page view lies before the span is in the one count only
		share := round1(min(100, float64(d.Visitors)/float64(d.Viewers)*100))
		d.Share = &share
	}
	d.Places, d.Pages, d.Clients, d.Classes = places.top(), pages.top(), clients.top(), classes.top()
	return d, nil
}
