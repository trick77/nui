package dash

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/trakk/backend/internal/apix"
	"github.com/trick77/trakk/backend/internal/insight"
	"github.com/trick77/trakk/backend/internal/watch"
)

// maxWatchFindings is the most findings of one kind the briefing gets from the watch, the weightiest and then the
// latest first; findings=all lists the rest too.
const maxWatchFindings = 10

// The watch's findings' weights: an answered probe or a new script host outweighs every counted finding (their
// scores grow with their counts: 1800 probes of one path once led the list above a flipped path), a new target sits
// with the new referrers. An answer of its own size, clearly not the refusals', adds sizeScore.
const (
	flipScore     = 3e9
	hitScore      = 2e9
	scriptScore   = 1e9
	endpointScore = 170
	sizeScore     = 50
)

// watchFindings adds the watch job's findings (internal/watch) from the daily span's start on: answered probes (flip,
// hit) and new targets on a site with a feed, new script hosts. The provisional ones of the days still open go to
// open: shown and counted, never in the briefing's set (they come and go between passes, and its cached briefing is
// the set's). Past maxWatchFindings of a kind, closed and provisional each, they go to over instead: listed with
// findings=all, never in the briefing's set either.
func (s *Server) watchFindings(ctx context.Context, q *query, add, open, over func(insight.Fact)) error {
	fs, err := watch.Findings(ctx, s.DB.DB, q.site.ID, ms(q.p.win.From))
	if err != nil {
		return err
	}
	rawFrom, err := s.rawFrom(ctx, q.site.ID)
	if err != nil {
		return err
	}
	type kindFact struct {
		kind        string
		provisional bool
		fact        insight.Fact
	}
	var facts []kindFact
	for _, f := range fs {
		switch {
		case f.Kind == "script":
		case !q.site.HasFeed, f.Kind == "endpoint" && q.api():
			continue
		}
		fact := watchFact(f, rawFrom, q.loc)
		if f.Provisional { // the card marks its row instead
			fact.Detail = append(fact.Detail, "Provisional: judged on the day so far")
		}
		facts = append(facts, kindFact{f.Kind, f.Provisional, fact})
	}
	// Flips share a score: the latest first.
	slices.SortStableFunc(facts, func(a, b kindFact) int {
		return cmp.Or(cmp.Compare(a.kind, b.kind), factOrder(a.fact, b.fact))
	})
	// Closed and provisional apart: a provisional finding never pushes a closed one out of the briefing's set.
	type bound struct {
		kind        string
		provisional bool
	}
	per := map[bound]int{}
	for _, f := range facts {
		k := bound{f.kind, f.provisional}
		switch {
		case per[k] >= maxWatchFindings:
			over(f.fact)
			continue
		case f.provisional:
			open(f.fact)
		default:
			add(f.fact)
		}
		per[k]++
	}
	return nil
}

// rawFrom is where a site's raw events begin (unix ms), past now when it has none: a finding before it can no longer
// open its requests.
func (s *Server) rawFrom(ctx context.Context, site int64) (int64, error) {
	d, ok, err := s.DB.OldestEventDay(ctx, site)
	if err != nil || !ok {
		return ms(s.now()) + 1, err
	}
	return d * dayMS, nil
}

// watchFact words one finding. Its requests open in the log while their raw is kept (a hit's address in the visitors
// table); after, the text says they are gone and the row opens the watch's card.
func watchFact(f watch.Finding, rawFrom int64, loc *time.Location) insight.Fact {
	when := func(t int64) string { return time.UnixMilli(t).In(loc).Format("2 Jan 15:04") }
	var fact insight.Fact
	if f.Kind == "script" {
		h := f.Host
		fact = insight.Fact{ID: "script:" + f.Key, Kind: "problem", Figure: num(float64(h.Addrs)),
			Unit: "addresses loaded", Text: "New script host: " + f.Key, Score: scriptScore + float64(h.Addrs),
			At:     f.At,
			Detail: []string{"First seen " + when(h.FirstSeen) + ", pages load scripts from it"},
			Target: insight.Target{Panel: "scripts", Filter: map[string]string{"key": f.Key}}}
	} else {
		p := f.Path
		unit := "addresses"
		if p.OKAddrs == 1 {
			unit = "address"
		}
		fact = insight.Fact{ID: f.Kind + ":" + f.Key, Figure: num(float64(p.OKAddrs)), Unit: unit, At: f.At,
			Target: insight.Target{Panel: "log", Filter: map[string]string{"kind": "request", "path": f.Key}}}
		switch f.Kind {
		case "flip":
			fact.Kind, fact.Score, fact.Text = "problem", flipScore, "Refused path now answered: "+f.Key
			line := "Refused since " + when(p.RefusedFirst) + ", answered " + when(p.OKFirst)
			if p.RefusedAddrs > 1 {
				line += fmt.Sprintf(" (up to %d addresses refused a day)", p.RefusedAddrs)
			}
			fact.Detail = append(fact.Detail, line)
			if p.HitFirst != 0 {
				fact.Detail = append(fact.Detail, "Answered to a mostly refused address: "+p.HitAddr)
			}
		case "hit":
			fact.Kind, fact.Score, fact.Text = "problem", hitScore, "A scanner got an answer: "+f.Key
			fact.Detail = append(fact.Detail, fmt.Sprintf("Answered %s to %s, mostly refused that day", when(p.HitFirst),
				p.HitAddr))
			fact.Target = insight.Target{Panel: "visitors", Filter: map[string]string{"key": p.HitAddr}}
		default:
			fact.Kind, fact.Score, fact.Text = "automation", endpointScore+float64(p.OKAddrs), "New target answered: "+f.Key
			fact.Detail = append(fact.Detail, "First answered "+when(p.OKFirst))
		}
		if sizeDiffers(p) {
			fact.Score += sizeScore
			fact.Detail = append(fact.Detail, fmt.Sprintf("Answered %s, refused %s", byteRange(p.OKLo, p.OKHi),
				byteList(p.RefusedSizes)))
		}
		if f.At < rawFrom {
			fact.Text += " (its requests are no longer kept)"
			fact.Target = insight.Target{Panel: "answered", Filter: map[string]string{"key": f.Key}}
		}
	}
	return fact
}

// sizeDiffers: the answers' sizes are known and clearly none of the refusals' (the job's tolerance, doubled).
func sizeDiffers(p *watch.Path) bool {
	if p.OKHi <= 0 || len(p.RefusedSizes) == 0 {
		return false
	}
	for _, x := range p.RefusedSizes {
		tol := 2 * watch.SizeTolerance(x)
		if p.OKLo <= x+tol && p.OKHi >= x-tol {
			return false
		}
	}
	return true
}

// byteRange is a size or a span of sizes in words.
func byteRange(lo, hi int64) string {
	if lo == hi {
		return bytesText(hi)
	}
	return bytesText(lo) + " to " + bytesText(hi)
}

// byteList is a few distinct sizes in words: "153 B or 4.5 kB".
func byteList(sizes []int64) string {
	words := make([]string, len(sizes))
	for i, n := range sizes {
		words[i] = bytesText(n)
	}
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}

func bytesText(n int64) string {
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10) + " B"
	case n < 1_000_000:
		return num(round1(float64(n)/1000)) + " kB"
	}
	return num(round1(float64(n)/1_000_000)) + " MB"
}

// watchPath is one row of the answered probes card: a path finding with its state.
type watchPath struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Key         string `json:"key"`
	At          any    `json:"at"`
	Provisional bool   `json:"provisional"`
	// Raw: its requests are still kept (the log opens on them).
	Raw          bool  `json:"raw"`
	RefusedFirst any   `json:"refused_first"`
	RefusedLast  any   `json:"refused_last"`
	RefusedAddrs int64 `json:"refused_addrs"`
	// RefusedBytes are the refusals' distinct sizes, ascending; OKBytes the answers' span [lo, hi].
	RefusedBytes []int64  `json:"refused_bytes"`
	OKFirst      any      `json:"ok_first"`
	OKLast       any      `json:"ok_last"`
	OKAddrs      int64    `json:"ok_addrs"`
	OKBytes      []int64  `json:"ok_bytes"`
	HitAt        any      `json:"hit_at"`
	HitAddr      string   `json:"hit_addr"`
	Detail       []string `json:"detail"`
}

// watchHost is one row of the script hosts card.
type watchHost struct {
	Host      string `json:"host"`
	FirstSeen any    `json:"first_seen"`
	LastSeen  any    `json:"last_seen"`
	// Addrs: the most address keys of one day that reported it.
	Addrs int64 `json:"addrs"`
	// New: a finding of the range; Provisional: that finding is of a day still open.
	New         bool `json:"new"`
	Provisional bool `json:"provisional"`
}

// handleWatch answers the watch's two cards for the range: the answered probes (flips, hits, new targets; a site with
// a feed) and the script hosts the pages loaded, the range's new ones marked. Domain and range apply.
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request, q *query) {
	ctx := r.Context()
	from := ms(q.p.win.From)
	fs, err := watch.Findings(ctx, s.DB.DB, q.site.ID, from)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rawFrom, err := s.rawFrom(ctx, q.site.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	hosts, err := watch.Hosts(ctx, s.DB.DB, q.site.ID, from)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	paths := []watchPath{}
	newHosts := map[string]watch.Finding{}
	for _, f := range fs {
		if f.Kind == "script" {
			if _, seen := newHosts[f.Key]; !seen {
				newHosts[f.Key] = f
			}
			continue
		}
		if !q.site.HasFeed || (f.Kind == "endpoint" && q.api()) {
			continue
		}
		p := f.Path
		paths = append(paths, watchPath{ID: f.Kind + ":" + f.Key, Kind: f.Kind, Key: f.Key, At: isoMS(f.At),
			Provisional: f.Provisional, Raw: f.At >= rawFrom, RefusedFirst: isoMS(p.RefusedFirst),
			RefusedLast: isoMS(p.RefusedLast), RefusedAddrs: p.RefusedAddrs, RefusedBytes: sizes(p.RefusedSizes),
			OKFirst: isoMS(p.OKFirst), OKLast: isoMS(p.OKLast), OKAddrs: p.OKAddrs, OKBytes: span(p.OKLo, p.OKHi),
			HitAt: isoMS(p.HitFirst), HitAddr: p.HitAddr, Detail: nonNil(watchFact(f, rawFrom, q.loc).Detail)})
	}
	rows := []watchHost{}
	row := func(h *watch.Host, f watch.Finding, isNew bool) watchHost {
		return watchHost{Host: h.Host, FirstSeen: isoMS(h.FirstSeen), LastSeen: isoMS(h.LastSeen), Addrs: h.Addrs,
			New: isNew, Provisional: isNew && f.Provisional}
	}
	for i := range hosts {
		h := &hosts[i]
		f, isNew := newHosts[h.Host]
		if isNew && f.Provisional { // its finding's sight of the open days counts, the list's if later
			c := *h
			c.LastSeen, c.Addrs = max(c.LastSeen, f.Host.LastSeen), max(c.Addrs, f.Host.Addrs)
			h = &c
		}
		rows = append(rows, row(h, f, isNew))
		delete(newHosts, h.Host)
	}
	for _, f := range newHosts { // a host the list does not hold (yet)
		rows = append(rows, row(f.Host, f, true))
	}
	slices.SortStableFunc(rows, func(a, b watchHost) int {
		return cmp.Or(cmp.Compare(boolInt(b.New), boolInt(a.New)), cmp.Compare(b.Addrs, a.Addrs),
			cmp.Compare(a.Host, b.Host))
	})
	st, ok, err := watch.SiteState(ctx, s.DB.DB, q.site.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := map[string]any{"paths": paths, "hosts": rows, "feed": q.site.HasFeed, "ready": ok && st.ReadyAt != 0,
		"learning_until": nil, "scripts_learning_until": nil, "soft_day": nil, "cut": st.Cut}
	if ok {
		out["learning_until"] = iso(time.UnixMilli((st.FromDay + watch.LearnDays) * dayMS))
		if st.ScriptsFrom != 0 {
			out["scripts_learning_until"] = iso(time.UnixMilli((st.ScriptsFrom + watch.LearnDays) * dayMS))
		}
		if st.SoftDay != 0 {
			out["soft_day"] = time.UnixMilli(st.SoftDay * dayMS).UTC().Format(time.DateOnly)
		}
	}
	apix.JSON(w, http.StatusOK, out)
}

// isoMS is iso of a unix ms time, null for 0.
func isoMS(t int64) any {
	if t == 0 {
		return nil
	}
	return iso(time.UnixMilli(t))
}

// span is a size span [lo, hi], nil when no size was reported.
func span(lo, hi int64) []int64 {
	if hi <= 0 {
		return nil
	}
	return []int64{lo, hi}
}

// sizes is a size list, nil when no size was reported.
func sizes(s []int64) []int64 {
	if len(s) == 0 {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
