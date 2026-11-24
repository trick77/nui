package dash

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/store"
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// Slots cover their bucket whole, in order, at the viewer's wall clock: a day of 23 or 25 hours keeps its eight
// three-hour slots, an hourly one has as many slots as hours; an aggregate period's are on whole UTC hours.
func TestCuts(t *testing.T) {
	zh, kol := zone(t, "Europe/Zurich"), zone(t, "Asia/Kolkata")
	day := func(loc *time.Location, m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, loc) }
	for _, c := range []struct {
		rng      string
		from, to time.Time
		loc      *time.Location
		floor    bool
		n        int
	}{
		{"7d", day(zh, 3, 29), day(zh, 3, 30), zh, false, 23},
		{"7d", day(zh, 10, 25), day(zh, 10, 26), zh, false, 25},
		{"30d", day(zh, 3, 29), day(zh, 3, 30), zh, true, 8},
		{"30d", day(zh, 10, 25), day(zh, 10, 26), zh, true, 8},
		{"90d", day(zh, 10, 25), day(zh, 10, 26), zh, true, 4},
		{"30d", day(kol, 11, 2).Truncate(time.Hour), day(kol, 11, 3).Truncate(time.Hour), kol, true, 8},
		{"12mo", day(kol, 2, 1).Truncate(time.Hour), day(kol, 3, 1).Truncate(time.Hour), kol, true, 28},
		{"24h", day(zh, 3, 29), day(zh, 3, 29).Add(time.Hour), zh, false, 6},
	} {
		got := grids[c.rng].cuts(c.from, c.to, c.loc, c.floor)
		if len(got)-1 != c.n || !got[0].Equal(c.from) || !got[c.n].Equal(c.to) {
			t.Errorf("%s of %v: %v", c.rng, c.from, got)
		}
		for i := range c.n {
			if !got[i].Before(got[i+1]) || c.floor && !got[i].Equal(got[i].Truncate(time.Hour)) {
				t.Errorf("%s of %v: slot %d %v to %v", c.rng, c.from, i, got[i], got[i+1])
			}
		}
	}
	// The lead holds at least what the first mean reaches back to, and ends where the range begins.
	for rng, g := range grids {
		from := day(zh, 3, 30)
		lead := g.lead(from, zh, true)
		if len(lead)-1 < g.window-1 || !lead[len(lead)-1].Equal(from) || !slices.IsSortedFunc(lead, time.Time.Compare) {
			t.Errorf("%s lead %v", rng, lead)
		}
	}
}

func TestSlotting(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 11, 22, 0, m, 0, 0, time.UTC) }
	for _, c := range []struct {
		bounds []time.Time
		unit   time.Duration
		grain  time.Duration
	}{
		{[]time.Time{at(0), at(60)}, time.Hour, time.Hour},
		{[]time.Time{at(7), at(17), at(27)}, 10 * time.Minute, 0},
		{[]time.Time{at(0), at(180), at(300), at(540)}, time.Hour, time.Hour},
		{[]time.Time{at(30), at(90), at(150)}, time.Hour, 0},
	} {
		s := newSlotting(c.bounds)
		if s.unit != c.unit.Milliseconds() || s.grain != c.grain {
			t.Errorf("%v: unit %d, grain %v", c.bounds, s.unit, s.grain)
		}
		for i := range s.n() {
			if s.at(ms(c.bounds[i])) != i || s.at(ms(c.bounds[i+1])-1) != i {
				t.Errorf("%v: slot %d is not its own", c.bounds, i)
			}
		}
	}
}

func TestAxis(t *testing.T) {
	zh := zone(t, "Europe/Zurich")
	now := time.Date(2026, 11, 24, 6, 35, 0, 0, time.UTC) // Tuesday 07:35 in Zurich
	ax := func(rng string) axis {
		p := newPeriod(rng, "off", now, zh)
		g := grids[rng]
		return newAxis(rng, g.days(p.Bounds, zh, p.agg), zh)
	}
	labels := func(a axis, lines bool) (out []string) {
		for _, x := range a.Ticks {
			if x.Line || !lines {
				out = append(out, x.Label)
			}
		}
		return out
	}
	if a := ax("24h"); fmt.Sprint(labels(a, false)) !=
		"[08:00 10:00 12:00 14:00 16:00 18:00 20:00 22:00 00:00 02:00 04:00 06:00]" ||
		fmt.Sprint(labels(a, true)) != "[00:00]" || a.Ticks[0].At != 2.5 || len(a.Weekends) != 0 {
		t.Errorf("24h %+v", a)
	}
	// Wednesday the 18th to today: one weekend, whole.
	if a := ax("7d"); labels(a, false)[0] != "Wed 18.11." || len(a.Ticks) != 7 || a.Ticks[6].At != 144 ||
		fmt.Sprint(a.Weekends) != "[[72 120]]" {
		t.Errorf("7d %+v", a)
	}
	if a := ax("30d"); len(a.Ticks) != 30 || fmt.Sprint(labels(a, true)) != "[26.10. 02.11. 09.11. 16.11. 23.11.]" ||
		len(a.Weekends) != 4 || a.Weekends[0] != [2]float64{40, 56} {
		t.Errorf("30d %+v", a)
	}
	// A range that begins on a Sunday begins with half a weekend.
	sun := newPeriod("7d", "off", now.AddDate(0, 0, -3), zh)
	if a := newAxis("7d", grids["7d"].days(sun.Bounds, zh, false), zh); fmt.Sprint(a.Weekends) != "[[0 24] [144 168]]" {
		t.Errorf("7d from a Sunday %+v", a)
	}
	if a := ax("90d"); len(labels(a, true)) != 3 || len(a.Weekends) != 13 {
		t.Errorf("90d %+v", a)
	}
	if a := ax("12mo"); fmt.Sprint(labels(a, true)) != "[Dec Jan Feb Mar Apr May Jun Jul Aug Sep Oct Nov]" ||
		a.Ticks[1].At != 31 || len(a.Weekends) != 0 {
		t.Errorf("12mo %+v", a)
	}
}

func TestBars(t *testing.T) {
	zh := zone(t, "Europe/Zurich")
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	for rng, n := range map[string]int{"24h": 24, "7d": 28, "30d": 30, "90d": 90, "12mo": 53} {
		p := newPeriod(rng, "off", now, zh)
		b := p.bars(rng, zh)
		if len(b)-1 != n || !b[0].Equal(p.Bounds[0]) || !b[n].Equal(p.Bounds[p.n()]) || !slices.IsSortedFunc(b, time.Time.Compare) {
			t.Errorf("%s: %d bars, %v to %v", rng, len(b)-1, b[0], b[len(b)-1])
		}
	}
	// A range that begins on a Saturday begins with two days of a week.
	p := newPeriod("12mo", "off", now, zh)
	if b := p.bars("12mo", zh); barName("12mo", b[0], b[1], zh) != "01.11.–02.11." || monday(b[0], zh) || !monday(b[1], zh) {
		t.Errorf("first week %s", barName("12mo", b[0], b[1], zh))
	}
}

// A half-hour zone's bounds are floored: a range that begins on a Monday has no empty week before it. A day the clocks
// skip the midnight of is named, as itself.
func TestDaysWithoutTheirMidnight(t *testing.T) {
	kol := zone(t, "Asia/Kolkata")
	p := newPeriod("12mo", "off", time.Date(2026, 11, 24, 6, 35, 0, 0, time.UTC), kol) // from Monday, 1 December 2025
	b := p.bars("12mo", kol)
	for i := 1; i < len(b); i++ {
		if !b[i-1].Before(b[i]) {
			t.Fatalf("week %d is empty: %v", i, b[:3])
		}
	}
	if n := barName("12mo", b[0], b[1], kol); n != "01.12.–07.12." || !monday(b[0], kol) {
		t.Errorf("first week %s", n)
	}
	scl := zone(t, "America/Santiago")
	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC) // the clocks went forward at 00:00 on Sunday 6 September
	p = newPeriod("12mo", "off", now, scl)
	g, names := grids["12mo"], []string{}
	days := g.cuts(p.Bounds[11], p.Bounds[12], scl, true)
	for i := range 8 {
		names = append(names, slotName("12mo", days[i], days[i+1], scl)[:6])
	}
	if fmt.Sprint(names) != "[Tue 01 Wed 02 Thu 03 Fri 04 Sat 05 Sun 06 Mon 07 Tue 08]" {
		t.Errorf("days %v", names)
	}
	// A range that begins on it: every day has its own midnight still, and as many slots.
	for rng, perDay := range map[string]int{"30d": 8, "90d": 4} {
		n := map[string]int{"30d": 29, "90d": 89}[rng]
		p := newPeriod(rng, "prev", time.Date(2026, 9, 6+n, 15, 0, 0, 0, time.UTC), scl)
		for i := range p.n() {
			if c := grids[rng].cuts(p.Bounds[i], p.Bounds[i+1], scl, true); len(c)-1 != perDay ||
				i > 0 && p.Bounds[i].In(scl).Hour() != 0 {
				t.Errorf("%s day %d from %v has %d slots", rng, i, p.Bounds[i], len(c)-1)
			}
		}
	}
	// A floored start is its own day's: no sliver of the one before.
	p = newPeriod("30d", "off", time.Date(2026, 11, 24, 6, 35, 0, 0, time.UTC), kol) // from Monday, 26 October
	if a := newAxis("30d", grids["30d"].days(p.Bounds, kol, true), kol); a.Ticks[0] != (tick{0, "26.10.", true}) ||
		a.Weekends[0] != [2]float64{40, 56} {
		t.Errorf("Kolkata 30d %+v %+v", a.Ticks[0], a.Weekends)
	}
	p = newPeriod("7d", "off", now, scl)
	if a := newAxis("7d", g.days(p.Bounds, scl, false), scl); a.Ticks[2].Label != "Sun 06.09." {
		t.Errorf("ticks %+v", a.Ticks)
	}
}

// 7d compares an hour with the same hour by the wall clock, across a day of 23 hours and one of 25; a share near now
// with the same hours before, not with ones that are later in the day.
func TestHoursAreComparedByTheClock(t *testing.T) {
	zh := zone(t, "Europe/Zurich")
	for _, c := range []struct {
		now       time.Time
		hours     int // of the changed day
		unmatched int
	}{{time.Date(2026, 3, 30, 10, 0, 0, 0, time.UTC), 23, 0}, {time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC), 25, 1}} {
		r, _ := exactRig(t)
		r.s.Now = func() time.Time { return c.now }
		site, _ := r.s.Sites.Lookup(feedSite)
		q := &query{site: site, rng: "7d", loc: zh, aud: "all", cmp: "prev", p: newPeriod("7d", "prev", c.now, zh)}
		pl, err := r.s.planOf(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		cur, prev, _, err := r.s.tracks(t.Context(), pl, q, time.UnixMilli(0))
		if err != nil {
			t.Fatal(err)
		}
		n, unmatched := 0, 0
		for _, pt := range cur.points[cur.lead:] {
			if pt.bucket != 5 { // Sunday, the day before now
				continue
			}
			n++
			j, ok := prev.at[[2]int{pt.bucket, pt.k}]
			if !ok {
				unmatched++
				continue
			}
			if a, b := pt.from.In(zh), prev.points[j].from.In(zh); a.Hour() != b.Hour() || a.Weekday() != b.Weekday() {
				t.Errorf("%v is compared with %v", a, b)
			}
		}
		if n != c.hours || unmatched != c.unmatched {
			t.Errorf("%v: %d hours, %d of them compared with none", c.now, n, unmatched)
		}
	}
}

// The same traffic in both weeks is no change, up to the newest mean: near now both windows are moved in.
func TestDeltaNearNow(t *testing.T) {
	r, insert := exactRig(t)
	for d := range 20 {
		for h := range 24 {
			ts := rigEnd.Truncate(24*time.Hour).AddDate(0, 0, -d).Add(time.Duration(h) * time.Hour)
			if ts.After(rigEnd) {
				continue
			}
			for i := range h { // it climbs through the day
				insert(store.Event{TS: ts.UnixMilli() + int64(i), Source: "server", Kind: "request", IP: "192.0.2.9",
					IPKey: "192.0.2.9", Path: "/", Template: "GET /", Method: "GET", Status: 200, Cls: store.ClsHuman})
			}
		}
	}
	for _, rng := range []string{"24h", "7d"} {
		_, ov := r.get(t, "overview", p(feedSite, "range", rng))
		tl := tiles(t, ov)["requests"]
		ds, m := tl["delta"].([]any), tl["mean"].([]any)
		seen := 0
		for i, d := range ds {
			if d != nil && num64(d) != 0 {
				t.Errorf("%s: slot %d of %d changed %v", rng, i, len(ds), d)
			}
			if d != nil {
				seen++
			}
		}
		if seen < 100 || rng == "7d" && (m[len(m)-1] != nil || tl["prev_mean"].([]any)[len(m)-1] != nil) {
			t.Errorf("%s: %d slots compared", rng, seen)
		}
	}
}

// A tile is what its slots hold, and the same as the period's buckets would give, in every range and zone.
func TestSlotsHoldTheirBuckets(t *testing.T) {
	r := sharedRig(t, nil)
	site, _ := r.s.Sites.Lookup(feedSite)
	s := r.server(SourceAuto)
	for _, rng := range []string{"24h", "7d", "30d", "90d", "12mo"} {
		for _, tz := range []string{"Europe/Zurich", "Asia/Kolkata"} {
			loc := zone(t, tz)
			for _, cmp := range []string{"prev", "yoy"} {
				q := &query{site: site, rng: rng, loc: loc, tz: tz, aud: "all", cmp: cmp, p: newPeriod(rng, cmp, r.s.now(), loc)}
				pl, err := s.planOf(t.Context(), q)
				if err != nil {
					t.Fatal(err)
				}
				cur, prev, _, err := s.tracks(t.Context(), pl, q, time.UnixMilli(0))
				if err != nil {
					t.Fatal(err)
				}
				coarse, coarsePrev, _, err := s.curPrev(t.Context(), pl, q, "all")
				if err != nil {
					t.Fatal(err)
				}
				// The last bucket of the comparison, up to the cut.
				n := q.p.n()
				head, err := s.slots(t.Context(), pl, "all", []time.Time{q.p.Prev[n-1], maxTime(q.p.prevCut, q.p.Prev[n-1].Add(time.Millisecond))}, !q.p.prevAgg)
				if err != nil {
					t.Fatal(err)
				}
				coarsePrev[n-1] = head[0]
				for _, d := range tileDefs {
					where := fmt.Sprint(rng, " ", tz, " ", cmp, " ", d.key)
					if a, b := s.total(d, []bucketStats{cur.total}), s.total(d, coarse); a != b {
						t.Errorf("%s: %v of the slots, %v of the buckets", where, a, b)
					}
					if a, b := s.total(d, []bucketStats{prev.total}), s.total(d, coarsePrev); !distinctTol(a, b) {
						t.Errorf("%s before: %v of the slots, %v of the buckets", where, a, b)
					}
				}
				// Drawn whole: every slot in its place, the split one too.
				for i, pt := range prev.points[max(prev.lead, 1):] {
					if j := max(prev.lead, 1) + i; !pt.from.Equal(prev.points[j-1].to) {
						t.Fatalf("%s %s %s: slot %d begins %v, the one before ends %v", rng, tz, cmp, j, pt.from, prev.points[j-1].to)
					}
				}
			}
		}
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// No requests is no share of them, and a share's mean is of the requests together, not of the slots' shares.
func TestRateSeries(t *testing.T) {
	r, insert := exactRig(t)
	req := func(ts string, cls, n int) {
		for i := range n {
			insert(store.Event{TS: at(ts) + int64(i), Source: "server", Kind: "request", IP: "192.0.2.9", IPKey: "192.0.2.9",
				Path: "/", Template: "GET /", Method: "GET", Status: 200, Cls: cls})
		}
	}
	req("2026-11-20T10:00:00Z", store.ClsHuman, 9) // 11:00 in Zurich, hour 4*24+11
	req("2026-11-20T11:00:00Z", 4, 1)              // an hour on: all of one request automated
	_, ov := r.get(t, "overview", p(feedSite))
	tl := tiles(t, ov)["auto_share"]
	s, m := tl["series"].([]any), tl["mean"].([]any)
	if at := 4*24 + 11; s[at-1] != nil || num64(s[at]) != 0 || s[at] == nil || num64(s[at+1]) != 100 || s[at+2] != nil ||
		m[at-1] != nil || num64(m[at]) != 10 || num64(m[at+3]) != 10 || num64(m[at+4]) != 100 || m[at+5] != nil {
		t.Errorf("series %v\nmean %v", s, m)
	}
	if num64(tl["value"]) != 10 {
		t.Errorf("value %v", tl["value"])
	}
	// No data yet: the site's first hour.
	if first := ov["period"].(map[string]any)["first"]; num64(first) != 4*24+11 {
		t.Errorf("figures begin at %v", first)
	}
}

// 24h against last year has the year before by the hour: a count is spread over the hour's slots, a distinct count has
// no line.
func TestHoursOfLastYear(t *testing.T) {
	r, insert := exactRig(t)
	for i := range 12 {
		insert(pageLoad(at("2025-11-22T12:05:00Z")+int64(i), fmt.Sprint("v", i), "192.0.2.1", "CH", "Mozilla/5.0", 0)...)
	}
	_, ov := r.get(t, "overview", p(feedSite, "range", "24h", "cmp", "yoy"))
	tl := tiles(t, ov)
	views := tl["views"]["prev_mean"].([]any)
	if len(views) != 144 || num64(views[143]) != 2 || num64(views[138]) != 2 || num64(views[137]) != 0 {
		t.Errorf("views %v", views)
	}
	if v := tl["visitors"]; len(v["prev_mean"].([]any)) != 0 || num64(v["prev"]) != 12 {
		t.Errorf("visitors %v", v)
	}
}

// The chart reads no more statements than its buckets did, but for the lead and the cut: a slot is not a statement.
func TestChartStatements(t *testing.T) {
	r := sharedRig(t, nil)
	var n atomic.Int64
	traceSQL = func(q string, _ []any) {
		if strings.Contains(q, "FROM event") && strings.Contains(q, "AS cell") || strings.Contains(q, "agg_hour t") && strings.Contains(q, "t.hour") {
			n.Add(1)
		}
	}
	t.Cleanup(func() { traceSQL = nil })
	// Twice the range's UTC days (a local day touches two) or its 24 hours (a UTC midnight cuts one), the lead's and
	// the cut's beside.
	for rng, limit := range map[string]int64{"24h": 2*25 + 4, "7d": 4*7 + 6, "30d": 4*30 + 8, "90d": 4*90 + 8, "12mo": 2*366 + 24 + 20} {
		n.Store(0)
		if code, _ := r.get(t, "overview", p(feedSite, "range", rng)); code != 200 {
			t.Fatal(code)
		}
		if got := n.Load(); got == 0 || got > limit {
			t.Errorf("%s: %d statements, want at most %d", rng, got, limit)
		}
	}
}

// The day a site goes live in is drawn, but is in no mean: it would read as a low. A change is of the figures, not of
// the means as they are rounded.
func TestMeansAreOfWholeSlots(t *testing.T) {
	r, insert := exactRig(t)
	view := func(ts string, n int) {
		for i := range n {
			insert(pageLoad(at(ts)+int64(i), fmt.Sprint(ts, i), "192.0.2.1", "CH", "Mozilla/5.0", 0)...)
		}
	}
	view("2026-11-11T21:00:00Z", 1) // 22:00 in Zurich
	for d := 12; d < 22; d++ {
		view(fmt.Sprintf("2026-11-%dT12:00:00Z", d), 7)
	}
	_, ov := r.get(t, "overview", p(feedSite, "range", "12mo"))
	tl := tiles(t, ov)["views"]
	s, m := tl["series"].([]any), tl["mean"].([]any)
	if n := len(s); num64(s[n-12]) != 1 || m[n-12] != nil || num64(m[n-11]) != 7 || num64(m[n-2]) != 7 || m[n-1] != nil {
		t.Errorf("series %v\nmean %v", s[n-12:], m[n-12:])
	}
	r, insert = exactRig(t)
	view("2026-11-10T10:00:00Z", 1)
	view("2026-11-19T10:00:00Z", 2)
	view("2026-11-12T10:00:00Z", 1)
	_, ov = r.get(t, "overview", p(feedSite))
	if tl = tiles(t, ov)["views"]; num64(tl["delta"].([]any)[3*24+11]) != 100 {
		t.Errorf("delta of 2/6 against 1/6: %v", tl["delta"].([]any)[3*24+11])
	}
}

func TestDelta(t *testing.T) {
	sum, rate := tileDef{kind: "sum"}, tileDef{kind: "rate"}
	if delta(sum, 12, 8) != 50 || !math.IsNaN(delta(sum, 12, 0)) || delta(rate, 12, 8) != 4 || delta(rate, 4, 0) != 4 ||
		!math.IsNaN(delta(sum, math.NaN(), 8)) || !math.IsNaN(delta(rate, 1, math.NaN())) {
		t.Error("delta")
	}
}
