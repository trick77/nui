package dash

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/trick77/trakk/backend/internal/apix"
	"github.com/trick77/trakk/backend/internal/insight"
	"github.com/trick77/trakk/backend/internal/watch"
)

// maxWatchFindings is the most new script hosts the briefing gets from the watch, the weightiest and then the latest
// first; findings=all lists the rest too.
const maxWatchFindings = 10

// scriptScore is a new script host's weight: it outweighs every counted finding (their scores grow with their
// counts), the most addresses first.
const scriptScore = 1e9

// watchFindings adds the watch job's findings (internal/watch) from the daily span's start on: new script hosts. The
// provisional ones of the days still open go to open: shown and counted, never in the briefing's set (they come and
// go between passes, and its cached briefing is the set's). Past maxWatchFindings, closed and provisional each, they
// go to over instead: listed with findings=all, never in the briefing's set either. The alert rules' take those the
// job made in the span instead (watch.Made).
func (s *Server) watchFindings(ctx context.Context, q *query, add, open, over func(insight.Fact)) error {
	read := watch.Findings
	if q.alerts {
		read = watch.Made
	}
	fs, err := read(ctx, s.DB.DB, q.site.ID, ms(q.p.win.From))
	if err != nil {
		return err
	}
	type found struct {
		provisional bool
		fact        insight.Fact
	}
	var facts []found
	for _, f := range fs {
		fact := watchFact(f, q)
		if f.Provisional { // the card marks its row instead
			fact.Detail = append(fact.Detail, "Provisional: judged on the day so far")
		}
		facts = append(facts, found{f.Provisional, fact})
	}
	slices.SortStableFunc(facts, func(a, b found) int { return factOrder(a.fact, b.fact) })
	// Closed and provisional apart: a provisional finding never pushes a closed one out of the briefing's set.
	per := map[bool]int{}
	for _, f := range facts {
		switch {
		case per[f.provisional] >= maxWatchFindings:
			over(f.fact)
			continue
		case f.provisional:
			open(f.fact)
		default:
			add(f.fact)
		}
		per[f.provisional]++
	}
	return nil
}

// watchFact words one new script host; its row opens the script hosts card. An alert's time names its zone, as the
// other alerts' do: it is read away from the dashboard.
func watchFact(f watch.Finding, q *query) insight.Fact {
	h := f.Host
	first := time.UnixMilli(h.FirstSeen).In(q.loc).Format("2 Jan 15:04")
	if q.alerts {
		first = time.UnixMilli(h.FirstSeen).UTC().Format("2006-01-02 15:04") + " UTC"
	}
	return insight.Fact{ID: "script:" + f.Key, Kind: "problem", Figure: num(float64(h.Addrs)),
		Unit: "addresses loaded", Text: "New script host: " + f.Key, Score: scriptScore + float64(h.Addrs), At: f.At,
		Detail: []string{"First seen " + first + ", pages load scripts from it"},
		Target: insight.Target{Panel: "scripts", Filter: map[string]string{"key": f.Key}}, Value: float64(h.Addrs)}
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

// handleWatch answers the script hosts card for the range: the hosts the pages loaded, the range's new ones marked.
// Domain and range apply.
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request, q *query) {
	ctx := r.Context()
	from := ms(q.p.win.From)
	fs, err := watch.Findings(ctx, s.DB.DB, q.site.ID, from)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	hosts, err := watch.Hosts(ctx, s.DB.DB, q.site.ID, from)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	newHosts := map[string]watch.Finding{}
	for _, f := range fs {
		if _, seen := newHosts[f.Key]; !seen {
			newHosts[f.Key] = f
		}
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
	out := map[string]any{"hosts": rows, "ready": ok && st.ReadyAt != 0, "scripts_learning_until": nil, "cut": st.Cut}
	if ok && st.ScriptsFrom != 0 {
		out["scripts_learning_until"] = iso(time.UnixMilli((st.ScriptsFrom + watch.LearnDays) * dayMS))
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

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
