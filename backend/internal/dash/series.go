package dash

import (
	"context"
	"math"
	"slices"
	"sort"
	"time"
)

// The overview's chart. Its points are slots, parts of the period's buckets: the period (what is compared, what
// Noteworthy calls closed) keeps its buckets, only what is drawn is finer.

// grid is how a range's chart is cut into slots.
type grid struct {
	slot string // one slot, as the UI writes it
	mean string // the moving mean's window, as the UI writes it
	// window is the moving mean's slots: a whole day where days would swing the line, a whole week over a year.
	window int
	step   time.Duration
	// wall cuts at the viewer's wall clock (a day of 23 hours has a shorter slot, not one fewer).
	wall bool
}

var grids = map[string]grid{
	"24h":  {slot: "10 min", mean: "1 h", window: 6, step: 10 * time.Minute},
	"7d":   {slot: "hour", mean: "6 h", window: 6, step: time.Hour},
	"30d":  {slot: "3 hours", mean: "24 h", window: 8, step: 3 * time.Hour, wall: true},
	"90d":  {slot: "6 hours", mean: "24 h", window: 4, step: 6 * time.Hour, wall: true},
	"12mo": {slot: "day", mean: "7 d", window: 7, step: 24 * time.Hour, wall: true},
}

// hourGrid is the prior line of 24h against last year, whose raw may be gone: its buckets, the hours.
var hourGrid = grid{window: 1, step: time.Hour}

// inDay is an instant inside the local day whose midnight t is: t itself may be before it, floored to the UTC hour (a
// zone on a half hour) or on a day without a midnight (clocks that go forward at 00:00: Go has 23:00).
func inDay(t time.Time, loc *time.Location) time.Time { return t.Add(2 * time.Hour).In(loc) }

// localDay is the calendar date a bucket's start is the midnight of.
func localDay(t time.Time, loc *time.Location) (int, time.Month, int) { return inDay(t, loc).Date() }

// date is a calendar date to name: no zone, so every one has its midnight.
func date(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// cuts is the slots' bounds in [from, to), both ends in. floor: on whole UTC hours, as an aggregate period's buckets.
func (g grid) cuts(from, to time.Time, loc *time.Location, floor bool) []time.Time {
	out := []time.Time{from}
	if !g.wall {
		for t := from.Add(g.step); t.Before(to); t = t.Add(g.step) {
			out = append(out, t)
		}
		return append(out, to)
	}
	y, m, d := localDay(from, loc)
	for h := 0; ; h += int(g.step / time.Hour) {
		t := time.Date(y, m, d, h, 0, 0, 0, loc)
		if floor {
			t = t.Truncate(time.Hour)
		}
		if !t.Before(to) {
			return append(out, to)
		}
		if t.After(out[len(out)-1]) {
			out = append(out, t)
		}
	}
}

// lead is the slots' bounds before from that the first means reach back to, nil without any.
func (g grid) lead(from time.Time, loc *time.Location, floor bool) []time.Time {
	if g.window < 2 {
		return nil
	}
	start := from.Add(-time.Duration(g.window-1) * g.step)
	if g.wall {
		days := 1
		if g.step == 24*time.Hour {
			days = g.window - 1
		}
		y, m, d := localDay(from, loc)
		start = time.Date(y, m, d-days, 0, 0, 0, 0, loc)
		if floor {
			start = start.Truncate(time.Hour)
		}
	}
	return g.cuts(start, from, loc, floor)
}

// slotting says which slot between bounds an instant falls in.
type slotting struct {
	bounds []time.Time
	// unit is the largest cell (ms, counted from the first bound) every bound is a multiple of: what a raw statement
	// groups by.
	unit int64
	// grain is what an aggregate read must be aligned to: an hour, 0 when a slot's edge is inside one.
	grain time.Duration
}

func newSlotting(bounds []time.Time) slotting {
	s := slotting{bounds: bounds, grain: time.Hour}
	for _, b := range bounds[1:] {
		s.unit = gcd(ms(b)-ms(bounds[0]), s.unit)
	}
	s.unit = max(s.unit, 1)
	for _, b := range bounds[1 : len(bounds)-1] { // the ends are the pieces' own: plan.pick looks at them
		if !b.Equal(b.Truncate(time.Hour)) {
			s.grain = 0
		}
	}
	return s
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// n is the number of slots.
func (s slotting) n() int { return len(s.bounds) - 1 }

// at is the slot of an instant (unix ms).
func (s slotting) at(t int64) int {
	i := sort.Search(len(s.bounds), func(i int) bool { return ms(s.bounds[i]) > t }) - 1
	return min(max(i, 0), s.n()-1)
}

// cell is a raw statement's grouping expression (two placeholders: args), slot the slot of one of its values.
const cellSQL = `(ts - ?) / ?`

func (s slotting) args() []any { return []any{ms(s.bounds[0]), s.unit} }

func (s slotting) slot(cell int64) int { return s.at(ms(s.bounds[0]) + cell*s.unit) }

// slots reads the slots between bounds (a bucket's, at most), a piece at a time, each from its source.
func (s *Server) slots(ctx context.Context, pl *plan, aud string, bounds []time.Time, short bool) ([]bucketStats, error) {
	sl := newSlotting(bounds)
	out := make([]bucketStats, sl.n())
	for _, pc := range pieces(bounds[0], bounds[len(bounds)-1]) {
		if err := pl.read(ctx, pc[0], pc[1], sl.grain, short,
			func() (bool, error) { return s.bucketHours(ctx, pl.site, aud, pc[0], pc[1], sl, out) },
			func() error { return s.bucketPiece(ctx, pl.site, aud, pc[0], pc[1], sl, out) }); err != nil {
			return nil, err
		}
	}
	if pl.api {
		return out, s.apiSlots(ctx, pl, aud, sl, short, out)
	}
	return out, nil
}

// point is one slot of a track.
type point struct {
	from, to time.Time
	// bucket and k are its place, what it is compared by: its bucket in the period, the slot in that (7d: the hour by
	// the wall clock, which a day of 23 or 25 hours does not have in order). bucket is -1 in the lead.
	bucket, k int
	st        bucketStats
	// known: it has figures; not before the site's first, nor where they are gone. whole: all of them, not only since
	// the site's first, which would read as a low.
	known, whole bool
}

// track is a period's slots, or its comparison's.
type track struct {
	grid   grid
	points []point // the lead first
	lead   int
	start  int            // the first whole point, 0 without one
	at     map[[2]int]int // a place's index in points
	// missing says which buckets are not whole (bucketStats.missing).
	missing []bool
	// total is all a tile's figure is of, up to the cut, as one. The points keep a count of their ids only: a year's
	// days and its comparison's, each with its ids, would be hundreds of sketches held until the answer is written.
	total bucketStats
}

// readTrack reads the slots of bounds' buckets and the lead before them. A bucket that is not whole has no known slot,
// nor has the time before the site's first figure. cut, inside the last bucket when set, is where total ends.
func (s *Server) readTrack(ctx context.Context, pl *plan, q *query, g grid, bounds []time.Time, floor bool,
	first, cut time.Time) (*track, error) {
	t := &track{grid: g, at: map[[2]int]int{}}
	found := false
	add := func(from, to time.Time, bucket, k int, st bucketStats) {
		for _, u := range []*uniq{&st.visitors, &st.api.callers, &st.api.accounts} {
			u.fix()
		}
		p := point{from: from, to: to, bucket: bucket, k: k, st: st,
			known: !st.missing && !st.gap && !first.IsZero() && to.After(first)}
		p.whole = p.known && !from.Before(first)
		if p.whole && !found {
			t.start, found = len(t.points), true
		}
		t.points = append(t.points, p)
	}
	if lead := g.lead(bounds[0], q.loc, floor); lead != nil {
		gaps, older := pl.gaps, pl.older
		st, err := s.slots(ctx, pl, q.aud, lead, !floor)
		if err != nil {
			return nil, err
		}
		if pl.gaps == gaps { // else the first means have fewer slots: before the range, so no figure of it is short
			for k := range st {
				add(lead[k], lead[k+1], -1, k, st[k])
			}
			t.lead = len(t.points)
		}
		pl.gaps, pl.older = gaps, older
	}
	for i := 0; i+1 < len(bounds); i++ {
		edges := g.cuts(bounds[i], bounds[i+1], q.loc, floor)
		split := -1 // the slot the cut is inside of
		if j, on := slices.BinarySearchFunc(edges, cut, time.Time.Compare); !cut.IsZero() && !on && j > 0 && j < len(edges) {
			edges, split = slices.Insert(edges, j, cut), j-1
		}
		gaps, older := pl.gaps, pl.older
		st, err := s.slots(ctx, pl, q.aud, edges, !floor)
		if err != nil {
			return nil, err
		}
		t.missing = append(t.missing, pl.older > older)
		for k := range st {
			st[k].missing, st[k].gap = pl.older > older, pl.gaps > gaps
			// A bucket that is not whole is in no figure, as in no series.
			if !st[k].missing && (cut.IsZero() || !edges[k+1].After(cut)) {
				t.total.add(&st[k])
			}
		}
		if split >= 0 {
			st[split].add(&st[split+1])
			st = slices.Delete(st, split+1, split+2)
			edges = slices.Delete(edges, split+1, split+2)
		}
		for j := range st {
			k := j
			if q.rng == "7d" && j > 0 { // the first is hour 0, whatever the clock says (inDay)
				k = edges[j].Add(30 * time.Minute).In(q.loc).Hour() // floored or not
				if _, again := t.at[[2]int{i, k}]; again {          // the hour the clocks went back over: compared with none
					k += 24
				}
			}
			t.at[[2]int{i, k}] = len(t.points)
			add(edges[j], edges[j+1], i, k, st[j])
		}
	}
	return t, nil
}

// first is the hour a site's figures begin in (the aggregates know no more), zero without any.
func (pl *plan) first(ctx context.Context) (time.Time, error) {
	var out time.Time
	if pl.hasRaw {
		out = time.UnixMilli(pl.oldestTS).Truncate(time.Hour)
	}
	hour, ok, err := pl.s.firstFigure(ctx, pl.site)
	if err != nil || !ok {
		return out, err
	}
	if at := time.UnixMilli(hour * hourMS); out.IsZero() || at.Before(out) {
		out = at
	}
	return out, nil
}

// shown is the points drawn: all but the lead and what has not begun by now.
func (t *track) shown(now time.Time) []point {
	out := t.points[t.lead:]
	for len(out) > 0 && out[len(out)-1].from.After(now) {
		out = out[:len(out)-1]
	}
	return out
}

// value is a tile's figure of one slot, NaN without one.
func (d tileDef) value(p point) float64 {
	switch {
	case !p.known:
		return math.NaN()
	case d.pool != nil:
		return d.pool([]bucketStats{p.st})
	}
	return d.one(&p.st)
}

// one is a count's figure of one slot.
func (d tileDef) one(b *bucketStats) float64 {
	if d.distinct != nil {
		return float64(d.distinct(b).count())
	}
	return d.of(*b)
}

// window is the points [from, to) the moving mean at point i is of. Around it, not up to it: a mean that looks back
// peaks half a window after what it is of. Where it would pass end (the last slot that is over) or the first whole
// point it is moved in, never cut: half a day is either a night or not.
func (t *track) window(i, end int) (from, to int) {
	w := t.grid.window
	from = min(i-w/2, end-w+1)
	if i >= t.start {
		from = max(from, t.start)
	}
	return from, min(from+w, end+1)
}

// mean is a tile's mean at point i over the points [from, to) that are whole: a rate's and a percentile's over all they
// hold together (a mean of shares weighs a slot of one request like one of a thousand), a count's per slot. NaN
// without a figure.
func (t *track) mean(d tileDef, i, from, to int) float64 {
	if !t.points[i].whole || from < t.start || to > len(t.points) {
		return math.NaN()
	}
	var win []bucketStats
	for _, p := range t.points[from:to] {
		if p.whole {
			win = append(win, p.st)
		}
	}
	switch {
	case len(win) == 0:
		return math.NaN()
	case d.pool != nil:
		return d.pool(win)
	}
	var sum float64
	for j := range win {
		sum += d.one(&win[j])
	}
	return sum / float64(len(win))
}

// chart fills a tile's series from the range's track and its comparison's (nil: none).
func (d tileDef) chart(t *tile, cur, prev *track, now time.Time) {
	pts := cur.shown(now)
	t.Series, t.Mean = make([]float64, len(pts)), make([]float64, len(pts))
	t.PrevMean, t.Delta = []float64{}, []float64{}
	end := cur.lead + len(pts) - 1
	if pts[len(pts)-1].to.After(now) { // a slot still filling would pull it down
		end--
	}
	means := make([]float64, len(pts)) // not rounded: a change of two thirds of a visitor is none of the figures shown
	for i, p := range pts {
		means[i] = math.NaN()
		if j := cur.lead + i; j <= end {
			from, to := cur.window(j, end)
			means[i] = cur.mean(d, j, from, to)
		}
		t.Series[i], t.Mean[i] = round1(d.value(p)), round2(means[i])
	}
	// Hours beside ten minutes (24h against last year): a count is spread over the hour's slots, but the distinct of an
	// hour are not the sum of its slots'.
	hours := prev != nil && prev.grid.step != cur.grid.step
	if prev == nil || hours && d.distinct != nil {
		return
	}
	any := false
	for i, p := range pts {
		at, v := [2]int{p.bucket, p.k}, math.NaN()
		if hours {
			at[1] = 0
		}
		// Of the same slots as the mean it is compared with, or of none: near now that one's window is moved in, and the
		// morning must not be compared with a noon.
		if j, ok := prev.at[at]; ok && cur.lead+i <= end {
			from, to := cur.window(cur.lead+i, end)
			from, to = from+j-(cur.lead+i), to+j-(cur.lead+i)
			if hours {
				from, to = j, j+1
			}
			if v = prev.mean(d, j, from, to); hours && d.pool == nil {
				v /= float64(prev.grid.step / cur.grid.step)
			}
		}
		any = any || !math.IsNaN(v)
		t.PrevMean = append(t.PrevMean, round2(v))
		t.Delta = append(t.Delta, delta(d, means[i], v))
	}
	if !any {
		t.PrevMean, t.Delta = []float64{}, []float64{}
	}
}

// delta is a mean against the prior one: points of a rate, else percent; NaN when there is nothing to compare.
func delta(d tileDef, cur, prev float64) float64 {
	switch {
	case d.kind == "rate":
		return round1(cur - prev)
	case prev == 0:
		return math.NaN()
	}
	return round1((cur - prev) / prev * 100)
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// tick is a mark on a chart's time axis; At is its place, in slots from the chart's left edge.
type tick struct {
	At    float64 `json:"at"`
	Label string  `json:"label"`
	// Line: something begins there (a day, a week, a month), worth a rule across the chart.
	Line bool `json:"line"`
}

// axis is a chart's time axis: its ticks, and the weekends as pairs of places.
type axis struct {
	Ticks    []tick       `json:"ticks"`
	Weekends [][2]float64 `json:"weekends"`
}

// place is an instant's place on slots bounds, clamped to them.
func place(bounds []time.Time, t time.Time) float64 {
	n := len(bounds) - 1
	i := sort.Search(len(bounds), func(i int) bool { return bounds[i].After(t) }) - 1
	switch {
	case i < 0:
		return 0
	case i >= n:
		return float64(n)
	}
	return math.Round((float64(i)+float64(t.Sub(bounds[i]))/float64(bounds[i+1].Sub(bounds[i])))*1000) / 1000
}

// newAxis is the time axis of a range's slots (or bars) in the viewer's zone.
func newAxis(rng string, bounds []time.Time, loc *time.Location) axis {
	a := axis{Ticks: []tick{}, Weekends: [][2]float64{}}
	if len(bounds) < 2 {
		return a
	}
	from, to := bounds[0], bounds[len(bounds)-1]
	mark := func(t time.Time, label string, line bool) {
		if !t.Before(from) && t.Before(to) {
			a.Ticks = append(a.Ticks, tick{At: place(bounds, t), Label: label, Line: line})
		}
	}
	if rng == "24h" {
		y, m, d := from.In(loc).Date()
		for h := 0; h <= 48; h += 2 {
			t := time.Date(y, m, d, h, 0, 0, 0, loc)
			mark(t, t.Format("15:04"), t.Hour() == 0)
		}
		return a
	}
	y, m, d := localDay(from, loc)
	midnight := func(d int) time.Time {
		t := time.Date(y, m, d, 0, 0, 0, 0, loc)
		if from.Equal(from.Truncate(time.Hour)) { // where the bounds have it, floored (a zone on a half hour) or not
			t = t.Truncate(time.Hour)
		}
		return t
	}
	if rng != "12mo" { // two days of a year are a hairline
		// From the Saturday before: the range may begin on a Sunday.
		for sat := d - (int(date(y, m, d).Weekday())+1)%7; ; sat += 7 {
			start := midnight(sat)
			if !start.Before(to) {
				break
			}
			if end := midnight(sat + 2); end.After(from) {
				a.Weekends = append(a.Weekends, [2]float64{place(bounds, start), place(bounds, end)})
			}
		}
	}
	for ; ; d++ {
		t := midnight(d)
		if !t.Before(to) {
			return a
		}
		day := date(y, m, d)
		name, isMonday, month := day.Format("02.01."), day.Weekday() == time.Monday, day.Day() == 1
		switch {
		case rng == "7d":
			mark(t, day.Format("Mon ")+name, true)
		case rng == "30d":
			mark(t, name, isMonday)
		case rng == "90d" && isMonday:
			mark(t, name, month)
		case rng == "90d" && month:
			mark(t, "", true)
		case rng == "12mo" && month:
			mark(t, day.Format("Jan"), true)
		}
	}
}

// slotName is a slot in a tooltip.
func slotName(rng string, from, to time.Time, loc *time.Location) string {
	a, b := from.In(loc), to.In(loc)
	switch rng {
	case "24h":
		return a.Format("15:04") + "–" + b.Format("15:04")
	case "12mo":
		y, m, d := localDay(from, loc)
		return date(y, m, d).Format("Mon 02.01.2006")
	}
	return a.Format("Mon 02.01. 15:04") + "–" + b.Format("15:04")
}

// bars are the class chart's bounds: fewer than the main chart's slots (a stack needs width), whole days but at 7d,
// weeks over a year. They cover the period; the last one drawn is the one now is in.
func (p period) bars(rng string, loc *time.Location) []time.Time {
	from, to := p.Bounds[0], p.Bounds[p.n()]
	switch rng {
	case "7d":
		return grid{step: 6 * time.Hour, wall: true}.days(p.Bounds, loc, false)
	case "12mo":
		out := []time.Time{from}
		y, m, d := localDay(from, loc)
		for ; ; d++ {
			t := time.Date(y, m, d, 0, 0, 0, 0, loc).Truncate(time.Hour) // floored as the period's bounds are
			if !t.Before(to) {
				return append(out, to)
			}
			if date(y, m, d).Weekday() == time.Monday && t.After(from) {
				out = append(out, t)
			}
		}
	}
	return p.Bounds
}

// monday: a bucket's start is a Monday's midnight.
func monday(t time.Time, loc *time.Location) bool {
	return inDay(t, loc).Weekday() == time.Monday
}

// days is every bucket's cuts in one list.
func (g grid) days(bounds []time.Time, loc *time.Location, floor bool) []time.Time {
	out := []time.Time{bounds[0]}
	for i := 0; i+1 < len(bounds); i++ {
		out = append(out, g.cuts(bounds[i], bounds[i+1], loc, floor)[1:]...)
	}
	return out
}

// barName is a bar in a tooltip.
func barName(rng string, from, to time.Time, loc *time.Location) string {
	y, m, d := localDay(from, loc)
	day := date(y, m, d)
	switch rng {
	case "24h":
		return "hour to " + to.In(loc).Format("15:04")
	case "7d":
		return slotName(rng, from, to, loc)
	case "12mo":
		ty, tm, td := localDay(to, loc)
		return day.Format("02.01.") + "–" + date(ty, tm, td-1).Format("02.01.")
	}
	return day.Format("Mon 02.01.")
}
