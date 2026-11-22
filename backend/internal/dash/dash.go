// Package dash is the dashboard API, GET /api/dash/*: every figure computed in Go from the raw events, nothing
// pre-aggregated. A long range is read as one short statement per bucket, never one long read: a read transaction
// held for seconds keeps SQLite's WAL checkpoint from completing while the collector writes on.
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

	"github.com/trick77/nui/backend/internal/apix"
	"github.com/trick77/nui/backend/internal/ingest"
	"github.com/trick77/nui/backend/internal/insight"
	"github.com/trick77/nui/backend/internal/refdata"
	"github.com/trick77/nui/backend/internal/sites"
	"github.com/trick77/nui/backend/internal/store"
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
type period struct {
	Bucket string
	Bounds []time.Time
	Prev   []time.Time
	Now    time.Time
	labels []string
	prevL  string
}

func newPeriod(rng, cmp string, now time.Time, loc *time.Location) period {
	t := now.In(loc)
	var n int
	var step func(time.Time, int) time.Time
	var start time.Time
	p := period{Now: now}
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

// n is the number of buckets.
func (p period) n() int { return len(p.Bounds) - 1 }

// From is the range's start, To now.
func (p period) From() time.Time { return p.Bounds[0] }

// ClosedEnd is where the open bucket starts.
func (p period) ClosedEnd() time.Time { return p.Bounds[p.n()-1] }

// prevClosedEnd is the comparison's matching end: as many closed buckets as the current range has.
func (p period) prevClosedEnd() time.Time { return p.Prev[p.n()-1] }

// audWhere is the SQL condition for the audience (event.cls codes; pending counts as human until classified).
func audWhere(aud string) string {
	switch aud {
	case "humans":
		return "cls IN (0, 1)"
	case "bots":
		return "cls IN (2, 3, 4)"
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
