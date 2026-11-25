package dash

import (
	"context"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/trakk/backend/internal/alert"
	"github.com/trick77/trakk/backend/internal/apix"
	"github.com/trick77/trakk/backend/internal/insight"
	"github.com/trick77/trakk/backend/internal/sites"
	"github.com/trick77/trakk/backend/internal/store"
)

// What the alert rules read (internal/alert/dispatch): the dashboard's own figures, never a copy of their code. The
// model never triggers anything: a rule reads computed facts and figures only, no reading or briefing.

// Findings are Noteworthy's 24h facts of a site as of now at the thresholds th: the closed hours of the 24 before
// now against the 24 before those, all classes. The briefing's set: the watch's provisional findings and those past
// its bound are not among them. Unknown names the parts a gap left unjudged (a list's dimension, "spikes", "tiles"):
// a fact missing there has not left.
func (s *Server) Findings(ctx context.Context, site *sites.Site, now time.Time, th Thresholds) (facts []insight.Fact,
	unknown map[string]bool, err error) {
	q := &query{site: site, rng: "24h", loc: time.UTC, tz: "UTC", aud: "all", cmp: "prev", p: hoursPeriod(now, time.UTC)}
	in, _, err := s.briefingInputWith(ctx, q, th)
	return in.Facts, q.unknown, err
}

// ScriptError is what a new script error's alert says beside its message: where it comes from and whom it hit, over
// the window Findings read it in. Raw only, a day per statement (errorDetail); never its stack.
type ScriptError struct {
	// Origins are its places' origins, the most frequent first; empty when its events are gone.
	Origins []string
	// Place is its most frequent place ("own /assets/index.js:1:48"), Type its error type, "" for none.
	Place, Type string
	// Visitors it hit, Viewers who saw a page meanwhile, Share the one of the other in percent (nil without viewers).
	Visitors, Viewers int64
	Share             *float64
}

// ScriptErrorOf reads a script error of a site by its message, over the closed hours of the 24 before now.
func (s *Server) ScriptErrorOf(ctx context.Context, site *sites.Site, key string, now time.Time) (ScriptError, error) {
	q := &query{site: site, rng: "24h", loc: time.UTC, tz: "UTC", aud: "all", cmp: "prev", p: hoursPeriod(now, time.UTC)}
	w := closedPart(q.p.win)
	d, err := s.errorDetail(ctx, q, key, ms(w.From), ms(w.End))
	if err != nil {
		return ScriptError{}, err
	}
	out := ScriptError{Origins: d.Origins, Type: d.Type, Visitors: d.Visitors, Viewers: d.Viewers, Share: d.Share}
	if rows := d.Places.Rows; len(rows) > 0 && rows[0].Where != nil {
		out.Place = strings.TrimSpace(rows[0].Where.Origin + " " + rows[0].Where.Place)
	}
	return out, nil
}

// LastSeen is when each source of a site last delivered, after a restart too (the hourly counters, not this
// process's own record alone); zero for one that never did.
func (s *Server) LastSeen(ctx context.Context, site int64) (browser, server time.Time, err error) {
	return s.lastSeen(ctx, site)
}

// Collector counts a site's collector refusals since from (whole hours: the counters are hourly).
func (s *Server) Collector(ctx context.Context, site int64, from, now time.Time) (Collected, error) {
	var c Collected
	lo, hi := from.Unix()/3600, now.Unix()/3600
	if err := s.row(ctx, `SELECT COALESCE(SUM(rate_limited + rejected), 0), COALESCE(SUM(dropped), 0),
		COALESCE(SUM(unknown_domain), 0) FROM ingest_stat WHERE site_id = ? AND hour >= ? AND hour <= ?`, site, lo, hi).
		Scan(&c.Refused, &c.Dropped, &c.UnknownDomain); err != nil {
		return c, err
	}
	if s.Counters != nil {
		for _, p := range s.Counters.Pending() {
			if p.SiteID == site && p.Hour >= lo && p.Hour <= hi {
				c.Refused += p.RateLimited + p.Rejected
				c.Dropped += p.Dropped
				c.UnknownDomain += p.UnknownDomain
			}
		}
	}
	return c, nil
}

// The level metrics.
const (
	LevelRequests = "requests" // server requests
	LevelVisitors = "visitors" // distinct browser visitors
	LevelFailed   = "failed"   // percent of an API's calls that failed
	LevelP95      = "p95"      // an API's 95th percentile duration, ms
)

// Level is a metric of a site over one window.
type Level struct {
	// Value is the metric, NaN where there is none: before the site's first figure, a window read short (raw gone off
	// the hours' grain), no call for a share or a percentile, no such figure on the site (an API's on a website).
	Value float64
	// Calls are the window's calls (an API's share needs enough of them).
	Calls float64
}

// Levels reads a metric of a site over the window before now and over the same window each of back days earlier
// (the comparison: the same time of day), each piece from its source (raw first, the hours where raw is gone).
func (s *Server) Levels(ctx context.Context, site *sites.Site, metric string, span time.Duration, now time.Time,
	back []int) (Level, []Level, error) {
	from := now.Add(-span)
	first := from
	if len(back) > 0 {
		first = from.AddDate(0, 0, -slices.Max(back))
	}
	q := &query{site: site, rng: "24h", loc: time.UTC, tz: "UTC", aud: "all", cmp: "off",
		p: period{Bounds: []time.Time{first, now}, Now: now, win: window{From: first, Closed: now, End: now}}}
	pl, err := s.planOf(ctx, q)
	if err != nil {
		return Level{}, nil, err
	}
	since, err := pl.first(ctx)
	if err != nil {
		return Level{}, nil, err
	}
	read := func(from, to time.Time) (Level, error) {
		gaps := pl.gaps
		b, err := s.slots(ctx, pl, "all", []time.Time{from, to}, true)
		if err != nil {
			return Level{Value: math.NaN()}, err
		}
		v, calls := levelOf(q, metric, b[0])
		if pl.gaps > gaps || since.IsZero() || from.Before(since) {
			v = math.NaN()
		}
		return Level{Value: v, Calls: calls}, nil
	}
	cur, err := read(from, now)
	if err != nil {
		return cur, nil, err
	}
	var prev []Level
	for _, d := range back {
		l, err := read(from.AddDate(0, 0, -d), now.AddDate(0, 0, -d))
		if err != nil {
			return cur, nil, err
		}
		prev = append(prev, l)
	}
	return cur, prev, nil
}

// levelOf is a metric of one bucket, NaN where the site has no such figure.
func levelOf(q *query, metric string, b bucketStats) (float64, float64) {
	calls := b.api.calls()
	switch {
	case metric == LevelRequests && q.site.HasFeed:
		return b.requests, calls
	case metric == LevelVisitors && !q.api():
		return float64(b.visitors.count()), calls
	case metric == LevelFailed && q.api():
		return b.api.failedShare(), calls
	case metric == LevelP95 && q.api():
		return b.api.p95(), calls
	}
	return math.NaN(), calls
}

// NotifyStatus is where the footer reads the notifications' state (the alert engine).
type NotifyStatus interface {
	Status(ctx context.Context) (alert.Status, error)
}

// notifyBlock is the footer's notify object: never a channel's URL.
func (s *Server) notifyBlock(ctx context.Context) (map[string]any, error) {
	st := alert.Status{}
	if s.Notify != nil {
		var err error
		if st, err = s.Notify.Status(ctx); err != nil {
			return nil, err
		}
	}
	chans := []map[string]any{}
	for _, c := range st.Channels {
		var lastErr any
		if !c.ErrorAt.IsZero() {
			lastErr = map[string]any{"at": iso(c.ErrorAt), "text": alert.Scrub(c.Error)}
		}
		chans = append(chans, map[string]any{"name": c.Name, "service": c.Service,
			"min_severity": c.MinSeverity.String(), "last_ok": iso(c.LastOK), "last_error": lastErr})
	}
	return map[string]any{"configured": st.Configured, "enabled": st.Enabled, "faults": nonNil(st.Faults),
		"rules": map[string]any{"active": st.Active, "off": st.Off}, "channels": chans}, nil
}

// Bounds of the alerts' page size.
const (
	alertsLimit    = 50
	maxAlertsLimit = 200
)

// alertRow is one alert as GET /api/dash/alerts answers it.
type alertRow struct {
	ID       int64           `json:"id"`
	Site     string          `json:"site"`
	Rule     string          `json:"rule"`
	Kind     string          `json:"kind"`
	Key      string          `json:"key"`
	Severity string          `json:"severity"`
	State    string          `json:"state"`
	Text     string          `json:"text"`
	Detail   string          `json:"detail"`
	Figure   *float64        `json:"figure"`
	Unit     string          `json:"unit"`
	FirstAt  any             `json:"first_at"`
	LastAt   any             `json:"last_at"`
	Cleared  any             `json:"cleared_at"`
	Delivery delivery        `json:"delivery"`
	Target   *insight.Target `json:"target"`
}

type delivery struct {
	State    string           `json:"state"`
	At       any              `json:"at"`
	Channels []channelOutcome `json:"channels"`
}

type channelOutcome struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
}

// handleAlerts is GET /api/dash/alerts: the alert history, newest first, of one site or (site left out) of every
// site and the instance, a page at a time (before: the cursor next gave).
func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	var site *int64
	if d := v.Get("site"); d != "" {
		st, ok := s.Sites.Lookup(d)
		if !ok {
			apix.Error(w, http.StatusBadRequest, "unknown site")
			return
		}
		site = &st.ID
	}
	var before int64
	if c := v.Get("before"); c != "" {
		n, err := strconv.ParseInt(c, 10, 64)
		if err != nil || n <= 0 {
			apix.Error(w, http.StatusBadRequest, "before is the cursor an answer's next gave")
			return
		}
		before = n
	}
	limit := alertsLimit
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > maxAlertsLimit {
			apix.Error(w, http.StatusBadRequest, "limit is 1 to 200")
			return
		}
		limit = n
	}
	ctx := r.Context()
	rows, err := s.DB.AlertPage(ctx, site, before, limit+1)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	domains, err := s.domains(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var next any
	if len(rows) > limit {
		rows = rows[:limit]
		next = strconv.FormatInt(rows[limit-1].ID, 10)
	}
	out := make([]alertRow, 0, len(rows))
	for _, a := range rows {
		out = append(out, alertRowOf(a, domains[a.SiteID]))
	}
	apix.JSON(w, http.StatusOK, map[string]any{"alerts": out, "next": next})
}

// domains names every site by id, those dropped from the sites file too: their alerts stay in the history.
func (s *Server) domains(ctx context.Context) (map[int64]string, error) {
	rows, err := s.DB.Sites(ctx)
	if err != nil {
		return nil, err
	}
	out := map[int64]string{}
	for _, r := range rows {
		out[r.ID] = r.Domain
	}
	return out, nil
}

func alertRowOf(a store.Alert, domain string) alertRow {
	row := alertRow{ID: a.ID, Site: domain, Rule: a.Rule, Kind: a.Kind, Key: a.Key, Severity: a.Severity,
		State: a.State, Text: a.Text, Detail: a.Detail, Figure: a.Figure, Unit: a.Unit,
		FirstAt: iso(time.UnixMilli(a.FirstAt)), LastAt: iso(time.UnixMilli(a.LastAt)), Delivery: deliveryOf(a)}
	if a.State == store.AlertCleared && a.LeftAt != nil {
		row.Cleared = iso(time.UnixMilli(*a.LeftAt))
	}
	if a.Panel != "" {
		f := a.Filter
		if f == nil {
			f = map[string]string{}
		}
		row.Target = &insight.Target{Panel: a.Panel, Filter: f}
	}
	return row
}

// deliveryOf is an alert's last message's delivery: none when no channel wanted it (or nothing is sent), pending
// while it waits or is tried again, else sent, partial or failed by the channels that took it.
func deliveryOf(a store.Alert) delivery {
	d := delivery{Channels: []channelOutcome{}}
	if a.NotifiedAt != nil {
		d.At = iso(time.UnixMilli(*a.NotifiedAt))
	}
	for _, n := range a.Channels {
		switch {
		case slices.Contains(a.Delivered, n):
			d.Channels = append(d.Channels, channelOutcome{n, true})
		case slices.Contains(a.Failed, n):
			d.Channels = append(d.Channels, channelOutcome{n, false})
		}
	}
	for _, n := range a.Failed { // a channel gone from the file since
		if !slices.Contains(a.Channels, n) {
			d.Channels = append(d.Channels, channelOutcome{n, false})
		}
	}
	took := 0
	for _, n := range a.Channels {
		if slices.Contains(a.Delivered, n) {
			took++
		}
	}
	switch {
	case a.Due != "":
		d.State = "pending"
	case len(a.Channels) == 0 && len(a.Failed) == 0:
		d.State = "none"
	case took == len(a.Channels) && took > 0:
		d.State = "sent"
	case took > 0:
		d.State = "partial"
	default:
		d.State = "failed"
	}
	return d
}
