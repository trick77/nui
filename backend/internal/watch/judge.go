package watch

import (
	"cmp"
	"maps"
	"slices"

	"github.com/trick77/trakk/backend/internal/classify"
)

// siteRef is a site as the job reads it.
type siteRef struct {
	id     int64
	domain string
	kind   string
}

// state is one site's whole state in memory: its row, its templates and hosts. A pass loads it once and merges every
// day it walks into it, written day by day.
type state struct {
	site  Site
	paths map[string]*Path
	hosts map[string]*Host
}

// outcome is what one day does to the state: the rows to write and delete, the site's row after it, and the findings
// it made.
type outcome struct {
	site      Site
	paths     map[string]Path
	hosts     map[string]Host
	dropPaths []string
	dropHosts []string
	// found are the findings the day made, provisional for an open day.
	found []Open
}

// dayPath is one template's day, judged: its own values in Path's fields, and what the first answer was.
type dayPath struct {
	Path
	firstUnrouted bool  // the first answer had no route
	firstSafe     bool  // the first answer was to a GET, HEAD or OPTIONS
	refBefore     int64 // addresses refused before the first answer
	unroutedOK    bool  // some answer had no route
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

// merge adds a day's values to a template's: firsts by MIN, lasts and counts by MAX, sizes to the bounded set, the
// first hit's address with it. Merging a day twice changes nothing.
func merge(p Path, d Path) Path {
	p.RefusedFirst, p.RefusedLast = minNZ(p.RefusedFirst, d.RefusedFirst), max(p.RefusedLast, d.RefusedLast)
	p.RefusedAddrs = max(p.RefusedAddrs, d.RefusedAddrs)
	p.RefusedSizes = mergeSizes(p.RefusedSizes, d.RefusedSizes)
	p.OKFirst, p.OKLast = minNZ(p.OKFirst, d.OKFirst), max(p.OKLast, d.OKLast)
	p.OKAddrs = max(p.OKAddrs, d.OKAddrs)
	p.OKLo, p.OKHi = minNZ(p.OKLo, d.OKLo), max(p.OKHi, d.OKHi)
	p.PlainFirst = minNZ(p.PlainFirst, d.PlainFirst)
	if d.HitFirst != 0 && (p.HitFirst == 0 || d.HitFirst < p.HitFirst) {
		p.HitFirst, p.HitAddr = d.HitFirst, d.HitAddr
	}
	return p
}

func mergeHost(h Host, d Host) Host {
	h.FirstSeen, h.LastSeen = minNZ(h.FirstSeen, d.FirstSeen), max(h.LastSeen, d.LastSeen)
	h.Addrs, h.Nets = max(h.Addrs, d.Addrs), max(h.Nets, d.Nets)
	return h
}

// mergeSizes adds a day's refusal sizes to a template's, in order (addSize): a new list, the old one untouched.
func mergeSizes(sizes, day []int64) []int64 {
	out := slices.Clone(sizes)
	for _, s := range day {
		out = addSize(out, s)
	}
	return out
}

// SizeTolerance is how far an answer's size may be from a refusal's and still be that error page answered 2xx: 64
// bytes, or a tenth of the refusal.
func SizeTolerance(size int64) int64 { return max(64, size/10) }

// inSpan: size is within the tolerance of one of the refusals' sizes; never without a size. Each size on its own: two
// error pages of a path are two sizes, an answer between them is neither.
func inSpan(size int64, refused []int64) bool {
	if size <= 0 {
		return false
	}
	for _, x := range refused {
		if abs(size-x) <= SizeTolerance(x) {
			return true
		}
	}
	return false
}

// judge reads a day against the state: every template's day, the shell sizes and whether the day is soft: many new
// unrouted templates answered, to several addresses or many of them refused before (a catch-all answers whoever
// passes, at paths it refused; one client fetching many new files, a backup or a mirror, is none). learning: the day
// judges nothing (soft days included: on a site's first days every path is new).
func (st *state) judge(d *day, learning bool) (map[string]*dayPath, int64, bool) {
	// A GET size many unrouted paths shared is a shell (a single-page app's index answered for any path): such an
	// answer is no answer of its own.
	shells := map[int64]bool{}
	if st.site.ShellBytes > 0 {
		shells[st.site.ShellBytes] = true
	}
	var shell int64
	best := 0
	for size, ts := range d.sizes {
		if len(ts) >= shellTemplates {
			shells[size] = true
			if len(ts) > best || (len(ts) == best && size < shell) {
				shell, best = size, len(ts)
			}
		}
	}
	scanner := map[string]bool{}
	for k, a := range d.addrs {
		if classify.MostlyRefused(a.server, a.refused) {
			scanner[k] = true
		}
	}
	out := map[string]*dayPath{}
	fresh, freshRefused := 0, 0
	freshAddrs := map[string]bool{}
	for tmpl, t := range d.tmpls {
		old := st.paths[tmpl]
		var refused []int64
		if old != nil {
			refused = old.RefusedSizes
		}
		refused = mergeSizes(refused, t.refSizes)
		dp := &dayPath{Path: Path{Template: tmpl, RefusedFirst: t.refFirst, RefusedLast: t.refLast,
			RefusedAddrs: int64(len(t.refAddrs)), RefusedSizes: t.refSizes}}
		okAddrs := map[string]bool{}
		for _, o := range t.oks {
			// The error page answered 2xx (one of its refusals' sizes), or the shell: no answer.
			if o.bytes > 0 && (shells[o.bytes] || inSpan(o.bytes, refused)) {
				continue
			}
			if dp.OKFirst == 0 {
				dp.OKFirst, dp.firstUnrouted, dp.firstSafe = o.ts, o.unrouted, o.safe
			}
			dp.OKLast = o.ts
			dp.unroutedOK = dp.unroutedOK || o.unrouted
			if o.bytes > 0 {
				dp.OKLo, dp.OKHi = minNZ(dp.OKLo, o.bytes), max(dp.OKHi, o.bytes)
			}
			okAddrs[o.addr] = true
			if scanner[o.addr] {
				if dp.HitFirst == 0 {
					dp.HitFirst, dp.HitAddr = o.ts, o.addr
				}
			} else if dp.PlainFirst == 0 {
				dp.PlainFirst = o.ts
			}
		}
		dp.OKAddrs = int64(len(okAddrs))
		if dp.RefusedFirst == 0 && dp.OKFirst == 0 {
			continue
		}
		for _, at := range t.refAddrs {
			if dp.OKFirst == 0 || at < dp.OKFirst {
				dp.refBefore++
			}
		}
		if dp.unroutedOK && (old == nil || old.OKFirst == 0) {
			fresh++
			for a := range okAddrs {
				if len(freshAddrs) < softAddrs {
					freshAddrs[a] = true
				}
			}
			if deferred(old, dp) {
				freshRefused++
			}
		}
		out[tmpl] = dp
	}
	soft := fresh >= softTemplates && (len(freshAddrs) >= softAddrs || freshRefused >= softRefused)
	return out, shell, !learning && soft
}

// deferred: on a soft day a template refused before its first answer keeps no answer, so the next day not soft
// judges it. Merged with its answer it would never be judged again: only a first answer makes a finding.
func deferred(old *Path, d *dayPath) bool {
	if d.OKFirst == 0 || (old != nil && old.OKFirst != 0) {
		return false
	}
	var rf int64
	if old != nil {
		rf = old.RefusedFirst
	}
	rf = minNZ(rf, d.RefusedFirst)
	return rf != 0 && rf < d.OKFirst
}

// decide is the finding a template's day makes, "" none. Only its first answer makes one: a flip (refused on an
// earlier day or by several addresses, then answered without a route), a hit (a mostly refused address answered
// before any other), else on a site a new target (a method that changes something, or an unrouted GET where the
// product names its routes). On an API, new endpoints are the lists' (newKeys).
func (st *state) decide(old *Path, d *dayPath, kind string, routedFresh bool) string {
	if d.OKFirst == 0 || (old != nil && old.OKFirst != 0) {
		return ""
	}
	var prev Path
	if old != nil {
		prev = *old
	}
	rf := minNZ(prev.RefusedFirst, d.RefusedFirst)
	switch {
	case d.firstUnrouted && rf != 0 && rf < d.OKFirst && (prev.RefusedFirst != 0 || d.refBefore >= minRefusedAddrs):
		return "flip"
	case d.HitFirst != 0 && (d.PlainFirst == 0 || d.PlainFirst > d.HitFirst):
		return "hit"
	case kind == "site" && (!d.firstSafe || routedFresh):
		return "endpoint"
	}
	return ""
}

// apply judges day d against the state and returns what it changes, now (unix ms) the finding's time. closed: the
// day is over and merges into the state under the caps; otherwise its findings are provisional and the outcome only
// goes into the pass's overlay. Nothing is found while the walk backfills or learns.
func (st *state) apply(site *siteRef, d *day, closed bool, now int64) outcome {
	s := st.site
	learning := s.ReadyAt == 0 || d.n < s.FromDay+LearnDays
	dps, shell, soft := st.judge(d, learning)
	o := outcome{paths: map[string]Path{}, hosts: map[string]Host{}}

	// The new templates the caps take: answered first (a finding is among them), then by addresses.
	var fresh []string
	for tmpl := range dps {
		if st.paths[tmpl] == nil {
			fresh = append(fresh, tmpl)
		}
	}
	slices.SortFunc(fresh, func(a, b string) int {
		x, y := dps[a], dps[b]
		return cmp.Or(cmp.Compare(answered(y), answered(x)),
			cmp.Compare(y.RefusedAddrs+y.OKAddrs, x.RefusedAddrs+x.OKAddrs), cmp.Compare(a, b))
	})
	if closed {
		take := min(len(fresh), maxNewTemplates)
		evict := st.evictPaths(len(st.paths)+take-maxTemplates, dps)
		o.dropPaths = evict
		take = min(take, maxTemplates-(len(st.paths)-len(evict)))
		// Distinct keys of the day, the hosts left out in memory among them: a key counts once a day.
		s.Cut += d.cut + int64(len(fresh)-take)
		for _, tmpl := range fresh[take:] {
			delete(dps, tmpl)
		}
	}

	routedDay := s.RoutedDay
	if d.routed {
		routedDay = d.n
	}
	routedFresh := routedDay != 0 && routedDay >= d.n-routedFreshDays
	type cand struct {
		tmpl, kind  string
		day, weight int64
	}
	var cands []cand
	if !learning && !soft {
		for tmpl, dp := range dps {
			if k := st.decide(st.paths[tmpl], dp, site.kind, routedFresh); k != "" {
				cands = append(cands, cand{tmpl, k, d.n, dp.OKAddrs + dp.RefusedAddrs})
			}
		}
	}
	if !learning {
		// Findings of earlier days that found no place then (Found without FoundAt): before the day's own, the oldest
		// first, so a day's ten made-up answers never push a real one out for good.
		for tmpl, p := range st.paths {
			if p.Found != "" && p.FoundAt == 0 {
				cands = append(cands, cand{tmpl, p.Found, foundTime(p, p.Found) / dayMS, p.OKAddrs + p.RefusedAddrs})
			}
		}
	}
	slices.SortFunc(cands, func(a, b cand) int {
		return cmp.Or(cmp.Compare(a.day, b.day), cmp.Compare(b.weight, a.weight), cmp.Compare(a.tmpl, b.tmpl))
	})
	// kinds are the findings the day makes, late those past maxPerKind: found on a later day.
	kinds, late, per := map[string]string{}, map[string]string{}, map[string]int{}
	for _, c := range cands {
		if per[c.kind] >= maxPerKind {
			late[c.tmpl] = c.kind
			continue
		}
		per[c.kind]++
		kinds[c.tmpl] = c.kind
	}
	for tmpl, dp := range dps {
		var p Path
		old := st.paths[tmpl]
		if old != nil {
			p = *old
		}
		p.Template = tmpl
		dv := dp.Path
		if soft && deferred(old, dp) {
			dv = Path{Template: tmpl, RefusedFirst: dv.RefusedFirst, RefusedLast: dv.RefusedLast,
				RefusedAddrs: dv.RefusedAddrs, RefusedSizes: dv.RefusedSizes}
		}
		p = merge(p, dv)
		if k := kinds[tmpl]; k != "" && p.FoundAt == 0 {
			p.Found, p.FoundAt = k, now
			o.found = append(o.found, Open{Kind: k, Key: tmpl, FirstSeen: foundTime(&p, k), Detail: jsonOf(p)})
		} else if k := late[tmpl]; k != "" && p.Found == "" {
			p.Found = k
		}
		o.paths[tmpl] = p
	}
	// An earlier day's late finding the day did not touch.
	for tmpl, k := range kinds {
		if dps[tmpl] != nil {
			continue
		}
		p := *st.paths[tmpl]
		p.FoundAt = now
		o.found = append(o.found, Open{Kind: k, Key: tmpl, FirstSeen: foundTime(&p, k), Detail: jsonOf(p)})
		o.paths[tmpl] = p
	}

	st.applyHosts(d, closed, now, &o, &s)

	s.DoneDay = d.n
	s.RoutedDay = routedDay
	if soft {
		s.SoftDay = d.n
	}
	if shell > 0 {
		s.ShellBytes = shell
	}
	o.site = s
	return o
}

// answered is 1 for a template's day with an answer, 0 for refusals alone.
func answered(d *dayPath) int {
	if d.OKFirst != 0 {
		return 1
	}
	return 0
}

// foundTime is when a template's finding of kind happened: its first answer, a hit's the scanner's. It is the same
// whether the finding is made on its day or on a later one.
func foundTime(p *Path, kind string) int64 {
	if kind == "hit" {
		return p.HitFirst
	}
	return p.OKFirst
}

// clone is a copy of the state an open day's overlay may change: the rows are replaced, never changed in place.
func (st *state) clone() *state {
	return &state{site: st.site, paths: maps.Clone(st.paths), hosts: maps.Clone(st.hosts)}
}

// evictPaths picks n templates to drop for new ones, none the day touches: refusals alone first (dropping one can
// only miss a flip, never invent one), the longest idle first; then answered ones that made no finding. A finding's
// row stays, a late one's (made on a later day) too.
func (st *state) evictPaths(n int, touched map[string]*dayPath) []string {
	if n <= 0 {
		return nil
	}
	var refused, answered []*Path
	for k, p := range st.paths {
		switch {
		case touched[k] != nil || p.Found != "":
		case p.OKFirst == 0:
			refused = append(refused, p)
		default:
			answered = append(answered, p)
		}
	}
	idle := func(a, b *Path) int {
		return cmp.Or(cmp.Compare(max(a.RefusedLast, a.OKLast), max(b.RefusedLast, b.OKLast)),
			cmp.Compare(a.Template, b.Template))
	}
	slices.SortFunc(refused, idle)
	slices.SortFunc(answered, idle)
	var out []string
	for _, p := range append(refused, answered...) {
		if len(out) == n {
			break
		}
		out = append(out, p.Template)
	}
	return out
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

// findHosts makes the day's script host findings, at most maxPerKind: of the new hosts not found yet (FoundFirst
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
	for _, h := range due[:min(len(due), maxPerKind)] {
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
	for _, k := range o.dropPaths {
		delete(st.paths, k)
	}
	for _, k := range o.dropHosts {
		delete(st.hosts, k)
	}
	for k, p := range o.paths {
		st.paths[k] = &p
	}
	for k, h := range o.hosts {
		st.hosts[k] = &h
	}
}
