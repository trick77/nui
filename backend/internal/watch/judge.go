package watch

import (
	"cmp"
	"maps"
	"slices"
)

// siteRef is a site as the job reads it.
type siteRef struct {
	id     int64
	domain string
}

// state is one site's whole state in memory: its row and its hosts. A pass loads it once and merges every day it
// walks into it, written day by day.
type state struct {
	site  Site
	hosts map[string]*Host
}

// outcome is what one day does to the state: the rows to write and delete, the site's row after it, and the findings
// it made.
type outcome struct {
	site      Site
	hosts     map[string]Host
	dropHosts []string
	// found are the findings the day made, provisional for an open day.
	found []Open
}

func minNZ(a, b int64) int64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	}
	return min(a, b)
}

func mergeHost(h Host, d Host) Host {
	h.FirstSeen, h.LastSeen = minNZ(h.FirstSeen, d.FirstSeen), max(h.LastSeen, d.LastSeen)
	h.Addrs, h.Nets = max(h.Addrs, d.Addrs), max(h.Nets, d.Nets)
	return h
}

// apply judges day d against the state and returns what it changes, now (unix ms) the bookkeeping's time. closed:
// the day is over and merges into the state under the caps; otherwise its findings are provisional and the outcome
// only goes into the pass's overlay. Nothing is found while the walk backfills or learns (applyHosts).
func (st *state) apply(d *day, closed bool, now int64) outcome {
	s := st.site
	o := outcome{hosts: map[string]Host{}}
	if closed {
		// Distinct hosts of the day left out in memory: a host counts once a day.
		s.Cut += d.cut
	}
	st.applyHosts(d, closed, now, &o, &s)
	s.DoneDay = d.n
	o.site = s
	return o
}

// clone is a copy of the state an open day's overlay may change: the rows are replaced, never changed in place.
func (st *state) clone() *state {
	return &state{site: st.site, hosts: maps.Clone(st.hosts)}
}

// applyHosts merges the day's script hosts. A host is new once address keys of at least minNets networks reported it
// on one day and it was first seen after the learning window: a week from the first day any page reported its hosts,
// never before the walk caught up (findHosts makes the findings).
func (st *state) applyHosts(d *day, closed bool, now int64, o *outcome, s *Site) {
	if len(d.hosts) == 0 {
		// A day without reports still finds the hosts an earlier day left past its bound.
		merged := map[string]Host{}
		st.findHosts(merged, nil, now, o)
		maps.Copy(o.hosts, merged)
		return
	}
	from := s.ScriptsFrom
	if from == 0 {
		from = d.n
	}
	if closed {
		s.ScriptsFrom = from
	}
	floor := from + LearnDays
	if s.ReadyAt == 0 {
		floor = d.n + 1
	} else {
		floor = max(floor, s.ReadyAt/dayMS-1)
	}
	// The most networks first, then addresses: they take the day's new places and its findings. Made-up names from one
	// /48's addresses never outrank a host many networks report.
	names := slices.SortedFunc(maps.Keys(d.hosts), func(a, b string) int {
		x, y := d.hosts[a], d.hosts[b]
		return cmp.Or(cmp.Compare(len(y.nets), len(x.nets)), cmp.Compare(len(y.addrs), len(x.addrs)), cmp.Compare(a, b))
	})
	merged := map[string]Host{}
	var fresh []string
	for _, name := range names {
		hd := d.hosts[name]
		var h Host
		if old := st.hosts[name]; old != nil {
			h = *old
		} else {
			fresh = append(fresh, name)
		}
		h.Host = name
		merged[name] = mergeHost(h, Host{FirstSeen: hd.first, LastSeen: hd.last, Addrs: int64(len(hd.addrs)),
			Nets: int64(len(hd.nets))})
	}
	if closed {
		take := min(len(fresh), maxNewHosts)
		for _, name := range fresh[take:] {
			delete(merged, name)
		}
		s.Cut += int64(len(fresh) - take)
		for _, name := range st.overHosts(merged) {
			if st.hosts[name] == nil { // a new one that found no place
				s.Cut++
			} else {
				o.dropHosts = append(o.dropHosts, name)
			}
			delete(merged, name)
		}
	}
	// New: its first sight that day is the finding's time (a host seen before by too few networks is new today), an
	// open day's and the closed one's alike, whichever day the finding is made.
	for name, h := range merged {
		if h.FoundAt == 0 && h.FoundFirst == 0 && int64(len(d.hosts[name].nets)) >= minNets &&
			h.FirstSeen/dayMS >= floor {
			h.FoundFirst = d.hosts[name].first
			merged[name] = h
		}
	}
	st.findHosts(merged, o.dropHosts, now, o)
	maps.Copy(o.hosts, merged)
}

// findHosts makes the day's script host findings, at most maxPerDay: of the new hosts not found yet (FoundFirst
// without FoundAt), the day's and the state's the day did not report, those new on the earliest day first, then the
// most networks. One past the bound is found on a later day, reported again or not: a day's made-up names never push
// a real one out for good.
func (st *state) findHosts(merged map[string]Host, dropped []string, now int64, o *outcome) {
	var due []Host
	for _, h := range merged {
		if h.FoundFirst != 0 && h.FoundAt == 0 {
			due = append(due, h)
		}
	}
	for name, h := range st.hosts {
		if _, ok := merged[name]; !ok && h.FoundFirst != 0 && h.FoundAt == 0 && !slices.Contains(dropped, name) {
			due = append(due, *h)
		}
	}
	slices.SortFunc(due, func(a, b Host) int {
		return cmp.Or(cmp.Compare(a.FoundFirst/dayMS, b.FoundFirst/dayMS), cmp.Compare(b.Nets, a.Nets),
			cmp.Compare(a.Host, b.Host))
	})
	for _, h := range due[:min(len(due), maxPerDay)] {
		h.FoundAt = now
		o.found = append(o.found, Open{Kind: "script", Key: h.Host, FirstSeen: h.FoundFirst, Detail: jsonOf(h)})
		merged[h.Host] = h
	}
}

// overHosts are the hosts past maxHosts once the day's (merged) join the state's, the weakest: the found ones keep
// their place first (at most maxFoundHosts, the latest seen), then those minNets networks reported on a day, then
// the rest, each the latest seen first. A host a few networks keep reporting stays evictable, seen today or not: made
// up names never starve real ones, nor do findings past their bound.
func (st *state) overHosts(day map[string]Host) []string {
	all := make([]Host, 0, len(st.hosts)+len(day))
	for name, h := range st.hosts {
		if _, ok := day[name]; !ok {
			all = append(all, *h)
		}
	}
	for _, h := range day {
		all = append(all, h)
	}
	if len(all) <= maxHosts {
		return nil
	}
	latest := func(a, b Host) int {
		return cmp.Or(cmp.Compare(b.LastSeen, a.LastSeen), cmp.Compare(b.Addrs, a.Addrs), cmp.Compare(a.Host, b.Host))
	}
	slices.SortFunc(all, latest)
	tier := map[string]int{}
	foundKept := 0
	for _, h := range all {
		switch {
		case h.FoundAt != 0 && foundKept < maxFoundHosts:
			foundKept++
		case h.Nets >= minNets:
			tier[h.Host] = 1
		default:
			tier[h.Host] = 2
		}
	}
	slices.SortStableFunc(all, func(a, b Host) int { return cmp.Compare(tier[a.Host], tier[b.Host]) })
	out := make([]string, 0, len(all)-maxHosts)
	for _, h := range all[maxHosts:] {
		out = append(out, h.Host)
	}
	return out
}

// commit makes an outcome the state's: after its write, or for an open day in the pass's overlay.
func (st *state) commit(o outcome) {
	st.site = o.site
	for _, k := range o.dropHosts {
		delete(st.hosts, k)
	}
	for k, h := range o.hosts {
		st.hosts[k] = &h
	}
}
