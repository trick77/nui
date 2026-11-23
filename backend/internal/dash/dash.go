// Package dash is the dashboard API, GET /api/dash/*: every figure computed in Go, per UTC day from the aggregate
// tables once the day is built and clean (internal/agg), from the raw events otherwise (source.go). A long range is
// read as one short statement per piece, never one long read: a read transaction held for seconds keeps SQLite's
// WAL checkpoint from completing while the collector writes on.
package dash

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/stakeout/backend/internal/apix"
	"github.com/trick77/stakeout/backend/internal/ingest"
	"github.com/trick77/stakeout/backend/internal/insight"
	"github.com/trick77/stakeout/backend/internal/refdata"
	"github.com/trick77/stakeout/backend/internal/sites"
	"github.com/trick77/stakeout/backend/internal/store"
)

// Server answers the dashboard endpoints.
type Server struct {
	DB              *store.Store
	Sites           *sites.Registry
	Ref             *refdata.Data
	Counters        *ingest.Counters
	Model           insight.Model
	RetentionMonths int
	Build           string
	Now             func() time.Time
	// Source is where the figures the aggregates can serve come from: SourceAuto but in tests.
	Source Source

	snaps snapshots // the visitor tables a cursor pages through
	// trace, when set (tests), sees the source of every piece a plan read: from the aggregates or not, and whether
	// the piece was on the table's grain (one off it is raw under every source).
	trace func(fromAgg, aligned bool)
}

func (s *Server) traced(fromAgg, aligned bool) {
	if s.trace != nil {
		s.trace(fromAgg, aligned)
	}
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ServeHTTP dispatches GET /api/dash/{endpoint}.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("endpoint")
	if name == "sites" {
		s.handleSites(w, r)
		return
	}
	h, ok := map[string]func(http.ResponseWriter, *http.Request, *query){
		"overview": s.handleOverview, "noteworthy": s.handleNoteworthy, "classes": s.handleClasses,
		"list": s.handleList, "status": s.handleStatus, "clients": s.handleClients, "visitors": s.handleVisitors,
		"visitor": s.handleVisitor, "log": s.handleLog, "domain": s.handleDomain,
	}[name]
	if !ok {
		apix.Error(w, http.StatusNotFound, "no such endpoint")
		return
	}
	q, err := s.parse(r)
	if err != nil {
		apix.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	h(w, r, q)
}

// query is the common parameters, checked.
type query struct {
	site *sites.Site
	rng  string
	loc  *time.Location
	tz   string
	aud  string
	cmp  string
	p    period
	plan *plan // made on first use (planOf)
}

var ranges = map[string]bool{"24h": true, "7d": true, "30d": true, "90d": true, "12mo": true}

func (s *Server) parse(r *http.Request) (*query, error) {
	v := r.URL.Query()
	site, ok := s.Sites.Lookup(v.Get("site"))
	if !ok {
		return nil, errors.New("unknown site")
	}
	q := &query{site: site, rng: v.Get("range"), aud: v.Get("aud"), cmp: v.Get("cmp"), tz: v.Get("tz")}
	if q.rng == "" {
		q.rng = "7d"
	}
	if !ranges[q.rng] {
		return nil, errors.New("range must be 24h, 7d, 30d, 90d or 12mo")
	}
	switch q.aud {
	case "":
		q.aud = "all"
	case "all", "humans", "bots":
	default:
		return nil, errors.New("aud must be all, humans or bots")
	}
	switch q.cmp {
	case "":
		q.cmp = "prev"
	case "prev", "yoy", "off":
	default:
		return nil, errors.New("cmp must be prev, yoy or off")
	}
	if q.tz == "" {
		q.tz = "UTC"
	}
	loc, err := time.LoadLocation(q.tz)
	if err != nil || strings.HasPrefix(q.tz, "/") || strings.Contains(q.tz, "..") {
		return nil, errors.New("tz must be an IANA zone")
	}
	q.loc = loc
	q.p = newPeriod(q.rng, q.cmp, s.now(), loc)
	return q, nil
}

// period is a range's buckets in the viewer's zone. Bounds holds n+1 instants for n buckets, the last bucket the
// open one (it ends in the future). Prev holds the comparison's, nil when cmp is off.
//
// An aggregate period (30d, 90d, 12mo, and every yoy comparison) is one the daily and hourly tables can serve: its
// bucket bounds are floored to the UTC hour (a zone on a half hour moves 30 min earlier), and its daily figures read
// whole UTC days by the viewer's calendar date (win). 24h and 7d and their prior period keep the exact local bounds.
type period struct {
	Bucket string
	Bounds []time.Time
	Prev   []time.Time
	Now    time.Time
	labels []string
	prevL  string
	// agg and prevAgg say whether the range, and its comparison, is an aggregate period.
	agg, prevAgg bool
	// win is the span of the daily figures (lists, status, clients, the visitors table, a visitor, the log);
	// prevWin the comparison's closed part, nil when lists compare nothing (cmp off, and 24h against last year,
	// which whole days cannot match).
	win     window
	prevWin *window
}

// window is a daily figure's span: [From, Closed) the closed part a change compares, [Closed, End) the open one. On
// an aggregate period every edge is a UTC midnight, End the one after now's day.
type window struct{ From, Closed, End time.Time }

// utcDay is the UTC midnight of t's calendar date in its own zone: a local day read as a whole UTC day.
func utcDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func newPeriod(rng, cmp string, now time.Time, loc *time.Location) period {
	t := now.In(loc)
	var n int
	var step func(time.Time, int) time.Time
	var start time.Time
	p := period{Now: now, agg: rng != "24h" && rng != "7d"}
	p.prevAgg = p.agg || cmp == "yoy"
	switch rng {
	case "24h":
		n, p.Bucket = 24, "hour"
		start = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc)
		step = func(x time.Time, k int) time.Time { return x.Add(time.Duration(k) * time.Hour) }
	case "12mo":
		n, p.Bucket = 12, "month"
		start = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
		step = func(x time.Time, k int) time.Time { return x.AddDate(0, k, 0) }
	default:
		n, _ = strconv.Atoi(strings.TrimSuffix(rng, "d"))
		p.Bucket = "day"
		start = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
		step = func(x time.Time, k int) time.Time { return x.AddDate(0, 0, k) }
	}
	first := step(start, -(n - 1))
	for i := 0; i <= n; i++ {
		b := step(first, i)
		p.Bounds = append(p.Bounds, b)
		if i < n {
			p.labels = append(p.labels, label(b, p.Bucket))
		}
	}
	switch cmp {
	case "prev":
		for i := 0; i <= n; i++ {
			p.Prev = append(p.Prev, step(first, i-n))
		}
		p.prevL = "prior " + strings.Replace(strings.Replace(rng, "h", " h", 1), "d", " d", 1)
		if rng == "12mo" {
			p.prevL = "prior 12 mo"
		}
	case "yoy":
		for _, b := range p.Bounds {
			p.Prev = append(p.Prev, b.AddDate(-1, 0, 0))
		}
		p.prevL = "last year"
	}

	// The daily figures' spans. Whole UTC days: from the start's date to today's, never past the UTC day now is in
	// (east of UTC after local midnight, today's date has not begun in UTC).
	today := utcDay(now.UTC())
	snapped := window{From: utcDay(p.Bounds[0]), Closed: utcDay(p.Bounds[n-1]), End: today.AddDate(0, 0, 1)}
	if snapped.Closed.After(today) {
		snapped.Closed = today
	}
	p.win = window{From: p.Bounds[0], Closed: p.Bounds[n-1], End: p.Bounds[n]}
	closedDays := n - 1 // 7d: local days, whatever UTC's clamp
	if p.agg {
		p.win = snapped
		closedDays = int(snapped.Closed.Sub(snapped.From) / (24 * time.Hour))
	}
	switch {
	// 12mo's prior period is last year: both comparisons the same months back, as its month buckets are.
	case cmp == "prev" && p.agg, cmp == "yoy" && rng == "12mo":
		p.prevWin = &window{From: step(snapped.From, -n), Closed: step(snapped.Closed, -n)}
	case cmp == "prev":
		p.prevWin = &window{From: p.Prev[0], Closed: p.Prev[n-1]}
	case cmp == "yoy" && rng != "24h":
		// As many whole days as the current closed part, a leap day or a clamp to UTC's today included.
		from := snapped.From.AddDate(-1, 0, 0)
		p.prevWin = &window{From: from, Closed: from.AddDate(0, 0, closedDays)}
	}
	if p.prevWin != nil {
		p.prevWin.End = p.prevWin.Closed
	}
	// The buckets of an aggregate period on whole UTC hours.
	floor := func(bs []time.Time) {
		for i := range bs {
			bs[i] = bs[i].Truncate(time.Hour)
		}
	}
	if p.agg {
		floor(p.Bounds)
	}
	if p.prevAgg {
		floor(p.Prev)
	}
	return p
}

func label(t time.Time, bucket string) string {
	switch bucket {
	case "hour":
		return t.Format("15:00")
	case "month":
		return t.Format("Jan")
	}
	return t.Format("02.01.")
}

// readSpan is the longest span one bucket statement reads. A month bucket (12mo) is read in day pieces: a month of a
// busy site in one GROUP BY held a read for over a second (the visitors' actor sums, 24 M events), twelve times in a
// row, and the WAL cannot restart past an open read while the collector writes. Sums and sets merge exactly.
const readSpan = 24 * time.Hour

// pieces cuts [from, to) at every UTC midnight, oldest first: a piece is at most a day, and never spans two of the
// daily tables' days, so each piece is read from one source.
func pieces(from, to time.Time) [][2]time.Time {
	var out [][2]time.Time
	for lo := from; lo.Before(to); {
		hi := minTime(utcDay(lo.UTC()).Add(readSpan), to)
		out = append(out, [2]time.Time{lo, hi})
		lo = hi
	}
	return out
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// n is the number of buckets.
func (p period) n() int { return len(p.Bounds) - 1 }

// From is the range's start, To now.
func (p period) From() time.Time { return p.Bounds[0] }

// ClosedEnd is where the open bucket starts.
func (p period) ClosedEnd() time.Time { return p.Bounds[p.n()-1] }

// audWhere is the SQL condition for the audience (event.cls codes; pending counts as automated until classified,
// as classOf).
func audWhere(aud string) string {
	switch aud {
	case "humans":
		return "cls = 1"
	case "bots":
		return "cls IN (0, 2, 3, 4)"
	}
	return "1"
}

// change compares a sum (pct) or a rate (pp); nil when there is nothing to compare.
type change map[string]any

func sumChange(cur, prev float64, cmpOn bool) change {
	switch {
	case !cmpOn:
		return nil
	case prev == 0 && cur == 0:
		return nil
	case prev == 0:
		return change{"new": true}
	}
	return change{"pct": round1((cur - prev) / prev * 100)}
}

func rateChange(cur, prev float64, cmpOn, hadPrev bool) change {
	if !cmpOn || !hadPrev {
		return nil
	}
	return change{"pp": round1(cur - prev)}
}

// verdict judges a change: up is good unless upBad (unknown automated, errors, probes, refusals).
func verdict(c change, upBad bool) string {
	var d float64
	switch {
	case c == nil:
		return "flat"
	case c["new"] == true:
		d = 1
	case c["pct"] != nil:
		d = c["pct"].(float64)
		if math.Abs(d) < 2 {
			return "flat"
		}
	case c["pp"] != nil:
		d = c["pp"].(float64)
		if math.Abs(d) < 0.5 {
			return "flat"
		}
	}
	if (d > 0) != upBad {
		return "good"
	}
	return "bad"
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

func pct(part, whole float64) float64 {
	if whole == 0 {
		return 0
	}
	return part / whole * 100
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	m := len(s) / 2
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}

func ms(t time.Time) int64 { return t.UnixMilli() }

// iso formats a time for the API, "" for zero.
func iso(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	apix.ServerError(w, r, err)
}

// serverOnly answers 404 for a server-only endpoint on a site without a feed.
func serverOnly(w http.ResponseWriter, q *query) bool {
	if q.site.HasFeed {
		return false
	}
	apix.Error(w, http.StatusNotFound, fmt.Sprintf("%s has no server feed", q.site.Domain))
	return true
}

// networkName is "AS15169 Google LLC" and the network kind for an ASN.
func (s *Server) networkName(asn int64) (string, string) {
	if asn == 0 {
		return "", ""
	}
	if s.Ref != nil {
		if as, ok := s.Ref.Networks().ByASN(asn); ok {
			name := strings.TrimSpace(fmt.Sprintf("AS%d %s", asn, as.Name))
			return name, refdata.NetworkKind(as.Category)
		}
	}
	return fmt.Sprintf("AS%d", asn), ""
}

// traceSQL, when set (tests), sees every statement the dashboard runs: the large-table test checks their plans.
var traceSQL func(query string, args []any)

func (s *Server) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	if traceSQL != nil {
		traceSQL(q, args)
	}
	return s.DB.DB.QueryContext(ctx, q, args...)
}

func (s *Server) row(ctx context.Context, q string, args ...any) *sql.Row {
	if traceSQL != nil {
		traceSQL(q, args)
	}
	return s.DB.DB.QueryRowContext(ctx, q, args...)
}

func (s *Server) countryName(code string) string {
	if s.Ref == nil {
		return code
	}
	return s.Ref.Countries().Name(code)
}
