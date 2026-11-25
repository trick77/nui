package dispatch

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/trakk/backend/internal/alert"
	"github.com/trick77/trakk/backend/internal/dash"
	"github.com/trick77/trakk/backend/internal/insight"
	"github.com/trick77/trakk/backend/internal/sites"
)

// Eval is the site rules' Source: the dashboard's own facts and figures, read by its code on the step's clock. The
// model never triggers anything: no reading or briefing is read here, only what Go computed.
type Eval struct {
	d       *dash.Server
	model   insight.Model
	started time.Time
	now     time.Time
	// loosest are each site's thresholds: the loosest of its fact rules', Noteworthy's where a rule asks for more.
	loosest map[int64]dash.Thresholds
	// facts are this pass's findings by site (factsAt the pass).
	facts   map[int64]siteFacts
	factsAt time.Time
	// errors are the script errors looked at, by site and message, kept while they are new; looked counts this pass's
	// looks per site (lookedAt the pass).
	errors   map[string]dash.ScriptError
	looked   map[int64]int
	lookedAt time.Time
}

// siteFacts are a site's findings and the parts a gap left unjudged.
type siteFacts struct {
	facts   []insight.Fact
	unknown map[string]bool
}

const (
	// alertNewKeys is the most new keys of one list the rules see: far past Noteworthy's ten, since a rule's own
	// filters run after the cut.
	alertNewKeys = 100
	// maxLooks bounds the script errors looked at a site and pass (a day of raw a statement each, two at most per
	// day): one not looked at yet waits for the next pass, its row held.
	maxLooks = 10
)

// factPart is the part of the findings each fact trigger is in: a gap there is no judgement.
var factPart = map[string]string{"error.new": "errors", "failure.new": "failures", "probe.new": "probed",
	"endpoint.new": "endpoints", "referrer.new": "referrers", "spike.addr": "spikes", "spike.net": "spikes",
	"trend.visitors": "tiles", "trend.calls": "tiles", "trend.failed": "tiles", "trend.auto_share": "tiles"}

// NewEval reads through d, a dashboard server of its own: its clock becomes the step's. started is when the process
// started (source.silent judges only after its after since then); model, when configured, is what llm.budget asks.
func NewEval(d *dash.Server, rules []alert.Rule, started time.Time, model insight.Model) *Eval {
	v := &Eval{d: d, model: model, started: started, loosest: map[int64]dash.Thresholds{},
		errors: map[string]dash.ScriptError{}}
	d.Now = func() time.Time { return v.now }
	if d.Sites != nil {
		for _, s := range d.Sites.All() {
			th := dash.DefaultThresholds()
			th.NewKeys = alertNewKeys
			for _, r := range rules {
				if r.Enabled && selects(r, s) {
					loosen(&th, r)
				}
			}
			v.loosest[s.ID] = th
		}
	}
	return v
}

// selects reports whether rule r is about site s: one it names, one of a product it names, every one when it names
// neither.
func selects(r alert.Rule, s *sites.Site) bool {
	return (len(r.Sites) == 0 && len(r.Products) == 0) || slices.Contains(r.Sites, s.Domain) ||
		slices.Contains(r.Products, s.Product)
}

// loosen lowers th to what rule r's facts need: findings then finds every fact r may match.
func loosen(th *dash.Thresholds, r alert.Rule) {
	p := r.P
	lower := func(v *int64) { *v = min(*v, int64(p.Min)) }
	spike := func(s *dash.Spike) {
		s.Min, s.Share, s.Growth = min(s.Min, int64(p.Min)), min(s.Share, p.Share/100), min(s.Growth, p.Factor)
	}
	switch r.Spec.ID {
	case "error.new":
		lower(&th.NewErrors)
	case "failure.new":
		lower(&th.NewFailures)
	case "probe.new":
		lower(&th.NewProbes)
	case "endpoint.new":
		lower(&th.NewEndpoints)
	case "referrer.new":
		lower(&th.NewReferrers)
	case "spike.addr":
		spike(&th.SpikeAddr)
	case "spike.net":
		spike(&th.SpikeNet)
	case "trend.visitors":
		th.TrendVisitors, th.TrendVisitorsPct = min(th.TrendVisitors, p.Min), min(th.TrendVisitorsPct, p.Change)
	case "trend.calls":
		th.TrendCalls, th.TrendCallsPct = min(th.TrendCalls, p.Min), min(th.TrendCallsPct, p.Change)
	case "trend.failed":
		th.FailedCalls, th.FailedPP = min(th.FailedCalls, p.Min), min(th.FailedPP, p.Change)
	case "trend.auto_share":
		th.AutoSharePP = min(th.AutoSharePP, p.Change)
	}
}

// factPrefix is each fact trigger's fact id prefix; the rest of the id is the key. A fact of no trigger's prefix
// (the watch's flip:, hit: and script:) no rule matches.
var factPrefix = map[string]string{"error.new": "err:", "failure.new": "failure:", "probe.new": "probe:",
	"endpoint.new": "endpoint:", "referrer.new": "ref:", "spike.addr": "spike:addr:", "spike.net": "spike:net:",
	"trend.visitors": "trend:visitors:", "trend.calls": "trend:calls:", "trend.failed": "trend:failed:",
	"trend.auto_share": "trend:auto_share:"}

// Occurrences is what rule r matches on site now.
func (v *Eval) Occurrences(ctx context.Context, now time.Time, r alert.Rule, site *sites.Site, facts bool) (
	[]alert.Occurrence, bool, error) {
	v.now = now
	switch r.Spec.Group {
	case "traffic":
		if !facts {
			return nil, false, nil
		}
		// A new endpoint is an API's: a website's endpoint: facts are the watch's new targets answered, no rule's.
		if r.Spec.ID == "endpoint.new" && site.Kind != "api" {
			return nil, true, nil
		}
		sf, err := v.findings(ctx, now, site)
		if err != nil {
			return nil, false, err
		}
		if sf.unknown[factPart[r.Spec.ID]] { // a gap: nothing left, nothing is new
			return nil, false, nil
		}
		occ := factOccurrences(r, sf.facts)
		if r.Spec.ID == "error.new" {
			if occ, err = v.scriptErrors(ctx, now, r, site, sf.facts, occ); err != nil {
				return nil, false, err
			}
		}
		return occ, true, nil
	case "source":
		return v.silent(ctx, now, r, site)
	case "collector":
		c, err := v.d.Collector(ctx, site.ID, now.Add(-r.P.Window), now)
		n := c.Refused
		if r.Spec.ID == "collector.dropped" {
			n = c.Dropped
		}
		return collectorOccurrence(r, n), err == nil, err
	case "level":
		return v.level(ctx, now, r, site)
	}
	if r.Spec.ID == "llm.budget" {
		return v.budget(ctx, site), true, nil
	}
	return nil, false, nil
}

// Instance is what an instance rule other than the board's matches now: requests naming no configured site.
func (v *Eval) Instance(ctx context.Context, now time.Time, r alert.Rule) ([]alert.Occurrence, bool, error) {
	v.now = now
	if r.Spec.ID != "collector.unknown_domain" {
		return nil, false, nil
	}
	ids := []int64{0}
	if v.d.Sites != nil {
		for _, s := range v.d.Sites.All() {
			ids = append(ids, s.ID)
		}
	}
	var n int64
	for _, id := range ids {
		c, err := v.d.Collector(ctx, id, now.Add(-r.P.Window), now)
		if err != nil {
			return nil, false, err
		}
		n += c.UnknownDomain
	}
	return collectorOccurrence(r, n), true, nil
}

// findings are a site's facts at its loosest thresholds, computed once a pass.
func (v *Eval) findings(ctx context.Context, now time.Time, site *sites.Site) (siteFacts, error) {
	if !now.Equal(v.factsAt) {
		v.facts, v.factsAt = map[int64]siteFacts{}, now
	}
	if sf, ok := v.facts[site.ID]; ok {
		return sf, nil
	}
	th, ok := v.loosest[site.ID]
	if !ok {
		th = dash.DefaultThresholds()
		th.NewKeys = alertNewKeys
	}
	fs, unknown, err := v.d.Findings(ctx, site, now, th)
	if err != nil {
		return siteFacts{}, err
	}
	sf := siteFacts{facts: fs, unknown: unknown}
	v.facts[site.ID] = sf
	// The script errors looked at stay while they are new: an error no longer among the facts is forgotten.
	if !unknown["errors"] {
		prefix := strconv.FormatInt(site.ID, 10) + "\x00"
		keep := map[string]bool{}
		for _, f := range fs {
			if k, ok := strings.CutPrefix(f.ID, factPrefix["error.new"]); ok {
				keep[prefix+k] = true
			}
		}
		for k := range v.errors {
			if strings.HasPrefix(k, prefix) && !keep[k] {
				delete(v.errors, k)
			}
		}
	}
	return sf, nil
}

// factUnits are the units an alert names a fact's figure by.
var factUnits = map[string]string{"times": "times", "probes of": "probes", "visitors from": "visitors",
	"calls of": "calls", "calls failed with": "failed calls", "requests from": "requests"}

// factOccurrences are the facts rule r matches by its own thresholds.
func factOccurrences(r alert.Rule, fs []insight.Fact) []alert.Occurrence {
	prefix := factPrefix[r.Spec.ID]
	p := r.P
	var out []alert.Occurrence
	for _, f := range fs {
		key, ok := strings.CutPrefix(f.ID, prefix)
		if !ok {
			continue
		}
		trend := strings.HasPrefix(r.Spec.ID, "trend.")
		dir := p.Direction == "" || p.Direction == "both" || p.Direction == key
		var match bool
		switch r.Spec.ID {
		case "spike.addr", "spike.net":
			match = f.Value >= p.Min && f.Share*100 >= p.Share && f.Growth >= p.Factor
		case "trend.failed":
			match = f.Volume >= p.Min && math.Abs(f.Change) >= p.Change && dir
		case "trend.auto_share":
			match = math.Abs(f.Change) >= p.Change && dir
		case "trend.visitors", "trend.calls":
			match = f.Value >= p.Min && math.Abs(f.Change) >= p.Change && dir
		case "error.new":
			// 1, the default, is every error found: one an event without a visitor id counts none for.
			match = f.Value >= p.Min && (p.Visitors <= 1 || f.Visitors >= p.Visitors)
		default:
			match = f.Value >= p.Min
		}
		if !match {
			continue
		}
		o := alert.Occurrence{Key: key, Text: alert.Line(f.Text, 200),
			Detail: alert.Line(strings.Join(f.Detail, "; "), 300),
			Target: &alert.Target{Panel: f.Target.Panel, Filter: f.Target.Filter}}
		switch {
		case trend:
			change, unit := f.Change, "%"
			if r.Spec.ID == "trend.failed" || r.Spec.ID == "trend.auto_share" {
				unit = "pp"
			}
			o.Figure, o.Unit = &change, unit
		default:
			value := f.Value
			o.Figure, o.Unit = &value, factUnits[f.Unit]
		}
		if strings.HasPrefix(r.Spec.ID, "spike.") {
			o.Text = alert.Line("A spike of requests from "+f.Text, 200)
		}
		if r.Spec.ID == "error.new" {
			o.Target = errorTarget(key, f.Server)
		}
		out = append(out, o)
	}
	return out
}

// errorTarget is a new error's row in the log, its every occurrence: a script error by its message (the log's exact
// filter), a server error by its endpoint among the 5xx answers.
func errorTarget(key string, server bool) *alert.Target {
	if server {
		return &alert.Target{Panel: "log", Filter: map[string]string{"kind": "server", "status": "5xx", "path": key}}
	}
	return &alert.Target{Panel: "log", Filter: map[string]string{"kind": "js_error", "name": key}}
}

// scriptErrors keeps the new script errors of occ that come from one of rule r's origins and says, for each kept, its
// place, type and the share of visitors it hit: one look at its raw events per key while it stays new, at most
// maxLooks a site and pass (the largest counts first); one not looked at yet holds its row. A server error is kept
// as it is.
func (v *Eval) scriptErrors(ctx context.Context, now time.Time, r alert.Rule, site *sites.Site,
	fs []insight.Fact, occ []alert.Occurrence) ([]alert.Occurrence, error) {
	server := map[string]bool{}
	for _, f := range fs {
		if k, ok := strings.CutPrefix(f.ID, factPrefix["error.new"]); ok {
			server[k] = f.Server
		}
	}
	var out []alert.Occurrence
	for _, o := range occ {
		if server[o.Key] {
			out = append(out, o)
			continue
		}
		e, ok, err := v.scriptError(ctx, now, site, o.Key)
		if err != nil {
			return nil, err
		}
		if !ok {
			out = append(out, alert.Occurrence{Key: o.Key, Hold: true})
			continue
		}
		// Its events gone meanwhile: no origin known, nothing to filter by.
		if len(e.Origins) > 0 && !slices.ContainsFunc(e.Origins, func(x string) bool {
			return slices.Contains(r.P.Origins, x)
		}) {
			continue
		}
		o.Detail = alert.Line(scriptErrorText(e), 300)
		out = append(out, o)
	}
	return out, nil
}

// scriptError is a site's script error as looked at while it is new, ok false when this pass's looks at the site are
// spent.
func (v *Eval) scriptError(ctx context.Context, now time.Time, site *sites.Site, key string) (dash.ScriptError, bool,
	error) {
	k := strconv.FormatInt(site.ID, 10) + "\x00" + key
	if e, ok := v.errors[k]; ok {
		return e, true, nil
	}
	if !now.Equal(v.lookedAt) {
		v.looked, v.lookedAt = map[int64]int{}, now
	}
	if v.looked[site.ID] >= maxLooks {
		return dash.ScriptError{}, false, nil
	}
	v.looked[site.ID]++
	e, err := v.d.ScriptErrorOf(ctx, site, key, now)
	if err != nil {
		return e, false, err
	}
	v.errors[k] = e
	return e, true, nil
}

// scriptErrorText is a script error's second line: its type and place, and whom it hit. Foreign text all of it: the
// caller defuses it.
func scriptErrorText(e dash.ScriptError) string {
	var parts []string
	if what := strings.TrimSpace(e.Type + " at " + e.Place); e.Place != "" {
		parts = append(parts, strings.TrimPrefix(what, "at "))
	} else if e.Type != "" {
		parts = append(parts, e.Type)
	}
	switch {
	case e.Share != nil:
		parts = append(parts, fmt.Sprintf("%d visitors, %s %% of %d who saw a page", e.Visitors, figure(*e.Share),
			e.Viewers))
	case e.Visitors > 0:
		parts = append(parts, fmt.Sprintf("%d visitors", e.Visitors))
	}
	return strings.Join(parts, "; ")
}

func collectorOccurrence(r alert.Rule, n int64) []alert.Occurrence {
	if n < int64(r.P.Min) || n == 0 {
		return nil
	}
	what := map[string]string{"collector.refused": "The collector refused events (rate limit, malformed batches)",
		"collector.dropped":        "The collector dropped events it had taken (full queue, failed writes)",
		"collector.unknown_domain": "Requests named no configured site"}[r.Spec.ID]
	f, unit := float64(n), "events"
	if r.Spec.ID == "collector.unknown_domain" {
		unit = "requests"
	}
	return []alert.Occurrence{{Text: what, Detail: "In the last " + span(r.P.Window) + ", counted by the whole hour",
		Figure: &f, Unit: unit, Target: &alert.Target{Panel: "domain", Filter: map[string]string{}}}}
}

// sourceNames are the sources as a message names them.
var sourceNames = map[string]string{"browser": "script", "server": "feed"}

// silent is source.silent on a site: a source that delivered before and not for the rule's after. Within after
// of the process's start it does not judge: a source is not silent because Trakk was down.
func (v *Eval) silent(ctx context.Context, now time.Time, r alert.Rule, site *sites.Site) ([]alert.Occurrence, bool,
	error) {
	browser, server, err := v.d.LastSeen(ctx, site.ID)
	if err != nil {
		return nil, false, err
	}
	var out []alert.Occurrence
	for _, src := range []struct {
		name string
		last time.Time
	}{{"browser", browser}, {"server", server}} {
		if (r.P.Source != "" && r.P.Source != src.name) || (src.name == "server" && !site.HasFeed) || src.last.IsZero() {
			continue
		}
		if now.Sub(v.started) < r.P.After {
			out = append(out, alert.Occurrence{Key: src.name, Hold: true})
			continue
		}
		quiet := now.Sub(src.last)
		if quiet < r.P.After {
			continue
		}
		mins := math.Floor(quiet.Minutes())
		out = append(out, alert.Occurrence{Key: src.name,
			Text:   fmt.Sprintf("No events from the %s for %s", sourceNames[src.name], span(quiet.Truncate(time.Minute))),
			Detail: "Last at " + src.last.UTC().Format("2006-01-02 15:04") + " UTC", Figure: &mins, Unit: "min",
			Target: &alert.Target{Panel: "domain", Filter: map[string]string{}}})
	}
	return out, true, nil
}

// budget is llm.budget: the site's model calls today at the daily budget.
func (v *Eval) budget(ctx context.Context, site *sites.Site) []alert.Occurrence {
	if v.model == nil || !v.model.Configured() {
		return nil
	}
	calls, budget := v.model.Usage(ctx, site.ID)
	if budget <= 0 || calls < budget {
		return nil
	}
	f := float64(calls)
	return []alert.Occurrence{{Text: "The model's daily budget is spent: no new briefing or reading until midnight UTC",
		Figure: &f, Unit: "calls", Target: &alert.Target{Panel: "domain", Filter: map[string]string{}}}}
}

// The level metrics by trigger: the figure read, its words, its unit, where its row jumps to.
var levels = map[string]struct {
	metric, label, unit string
	target              alert.Target
}{
	"level.requests": {dash.LevelRequests, "Requests", "requests", alert.Target{Panel: "log",
		Filter: map[string]string{"kind": "request"}}},
	"level.visitors": {dash.LevelVisitors, "Visitors", "visitors", alert.Target{Panel: "visitors",
		Filter: map[string]string{}}},
	"level.failed": {dash.LevelFailed, "Failed calls", "%", alert.Target{Panel: "failures",
		Filter: map[string]string{}}},
	"level.p95": {dash.LevelP95, "95th percentile", "ms", alert.Target{Panel: "endpoints",
		Filter: map[string]string{}}},
}

// minComparison is the comparison days with a figure a dynamic level needs: with fewer it does not judge.
const minComparison = 3

// noComparison is the key of a level rule's quiet note that it had too few days to compare with.
const noComparison = "no-comparison"

// level is a level rule on a site: the window's figure against min and max, and against the usual (the median of
// the same window at the same time of day on the 7 days before, or the same weekday of the 4 weeks before) by factor
// and drop, all that the rule sets together. Without a figure (none on the site, too few calls) it holds what it
// had; with too few days to compare it notes so, quietly, and holds too: never an alert from a missing baseline.
func (v *Eval) level(ctx context.Context, now time.Time, r alert.Rule, site *sites.Site) ([]alert.Occurrence, bool,
	error) {
	l := levels[r.Spec.ID]
	p := r.P
	dynamic := p.HasFactor || p.HasDrop
	var back []int
	if dynamic {
		back = []int{1, 2, 3, 4, 5, 6, 7}
		if p.Baseline == "weekday" {
			back = []int{7, 14, 21, 28}
		}
	}
	cur, prev, err := v.d.Levels(ctx, site, l.metric, p.Window, now, back)
	if err != nil {
		return nil, false, err
	}
	hold := []alert.Occurrence{{Hold: true}}
	if math.IsNaN(cur.Value) || (r.Spec.ID == "level.failed" && cur.Calls < p.Calls) {
		return hold, true, nil
	}
	holds := (!p.HasMin || cur.Value >= p.Min) && (!p.HasMax || cur.Value <= p.Max)
	detail := ""
	if dynamic {
		var vals []float64
		for _, x := range prev {
			if !math.IsNaN(x.Value) {
				vals = append(vals, x.Value)
			}
		}
		if len(vals) < minComparison {
			return append(hold, alert.Occurrence{Key: noComparison, Quiet: true,
				Text: alert.Line(fmt.Sprintf("%s: no comparison, fewer than %d days with figures before", l.label,
					minComparison), 200)}), true, nil
		}
		usual := median(vals)
		holds = holds && (p.HasMin || p.HasMax || usual >= p.Floor) &&
			(!p.HasFactor || cur.Value >= p.Factor*usual) && (!p.HasDrop || cur.Value <= usual*(1-p.Drop/100))
		detail = fmt.Sprintf("Usually %s%s (median of %d days)", figure(usual), unitText(l.unit), len(vals))
	}
	if !holds {
		return nil, true, nil
	}
	value := cur.Value
	target := l.target
	return []alert.Occurrence{{Text: fmt.Sprintf("%s over the last %s: %s%s", l.label, span(p.Window),
		figure(value), unitText(l.unit)), Detail: detail, Figure: &value, Unit: l.unit, Target: &target}}, true, nil
}

func median(xs []float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	m := len(s) / 2
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}

// span says a duration in words: 2 h, 45 min, 1 h 30 min.
func span(d time.Duration) string {
	h, m := int64(d/time.Hour), int64(d%time.Hour/time.Minute)
	switch {
	case h > 0 && m > 0:
		return strconv.FormatInt(h, 10) + " h " + strconv.FormatInt(m, 10) + " min"
	case h > 0:
		return strconv.FormatInt(h, 10) + " h"
	}
	return strconv.FormatInt(m, 10) + " min"
}
