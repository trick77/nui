package alert

import (
	"slices"
	"time"
)

// Spec is one trigger of the fixed catalogue: what a rule's on names. No expression language: each takes its own
// named parameters, every other one is a fault.
type Spec struct {
	ID    string
	Group string // traffic, source, collector, instance, level
	// Condition: a state that opens and clears (notified both ways, for and repeat apply). Else a fact: notified
	// when its key enters the set, closed in silence when it leaves.
	Condition bool
	// Instance: about the instance, not a site (sites and products are faults).
	Instance bool
	// Params are the parameters it takes beside the common ones, Required those without a default.
	Params, Required []string
	// Defaults are its parameters' values when a rule leaves them out: the dashboard's own thresholds.
	Defaults Params
}

// Params are a rule's trigger parameters with the defaults filled in.
type Params struct {
	Min, Max, Share, Factor, Change, Drop, Floor, Calls float64
	// HasMin, HasMax, HasFactor and HasDrop: the rule set them (a level rule judges by those it sets).
	HasMin, HasMax, HasFactor, HasDrop bool
	// Direction is up, down or both; Source browser, server or "" (both); Baseline days or weekday.
	Direction, Source, Baseline string
	After, Window               time.Duration
	Jobs                        []string
	// Visitors is the least distinct visitors (a server error: addresses) a new error needs; Origins where a script
	// error may come from for it to count (one of its places is enough; a server error has none and always counts).
	Visitors float64
	Origins  []string
}

// The origins of a script error, as the dashboard's error detail names them.
const (
	OriginOwn       = "own"       // the site's own host
	OriginOther     = "other"     // another host
	OriginNamed     = "named"     // code with no web address (a sourceURL's or a bundler's name)
	OriginUnknown   = "unknown"   // no file reported: mostly the site's own rejected promises
	OriginWithheld  = "withheld"  // the browser hides it ("Script error.")
	OriginExtension = "extension" // a browser extension
)

// Origins are every origin; DefaultOrigins all but an extension's, no news of the site's.
var (
	Origins        = []string{OriginOwn, OriginOther, OriginNamed, OriginUnknown, OriginWithheld, OriginExtension}
	DefaultOrigins = []string{OriginOwn, OriginOther, OriginNamed, OriginUnknown, OriginWithheld}
)

// Parameter names.
const (
	pMin       = "min"
	pMax       = "max"
	pShare     = "share"
	pFactor    = "factor"
	pChange    = "change"
	pDirection = "direction"
	pSource    = "source"
	pAfter     = "after"
	pWindow    = "window"
	pJobs      = "jobs"
	pDrop      = "drop"
	pBaseline  = "baseline"
	pFloor     = "floor"
	pCalls     = "calls"
	pVisitors  = "visitors"
	pOrigins   = "origins"
)

var levelParams = []string{pWindow, pMin, pMax, pFactor, pDrop, pBaseline, pFloor}

func fact(id string, min float64) Spec {
	return Spec{ID: id, Group: "traffic", Params: []string{pMin}, Defaults: Params{Min: min}}
}

func trend(id string, min, change float64) Spec {
	ps := []string{pMin, pChange, pDirection}
	if min == 0 {
		ps = []string{pChange, pDirection}
	}
	return Spec{ID: id, Group: "traffic", Params: ps, Defaults: Params{Min: min, Change: change, Direction: "both"}}
}

func collector(id string, instance bool) Spec {
	return Spec{ID: id, Group: "collector", Condition: true, Instance: instance, Params: []string{pMin, pWindow},
		Defaults: Params{Min: 1, Window: time.Hour}}
}

func level(id string, extra ...string) Spec {
	return Spec{ID: id, Group: "level", Condition: true, Params: append(slices.Clone(levelParams), extra...),
		Required: []string{pWindow}, Defaults: Params{Floor: 20, Calls: 100, Baseline: "days"}}
}

// Catalogue is every trigger. The traffic facts' defaults are Noteworthy's thresholds (dash's constants).
var Catalogue = []Spec{
	{ID: "error.new", Group: "traffic", Params: []string{pMin, pVisitors, pOrigins},
		Defaults: Params{Min: 1, Visitors: 1, Origins: DefaultOrigins}},
	fact("failure.new", 3),
	fact("probe.new", 3),
	fact("endpoint.new", 5),
	fact("referrer.new", 2),
	{ID: "spike.addr", Group: "traffic", Params: []string{pMin, pShare, pFactor},
		Defaults: Params{Min: 100, Share: 25, Factor: 3}},
	{ID: "spike.net", Group: "traffic", Params: []string{pMin, pShare, pFactor},
		Defaults: Params{Min: 200, Share: 40, Factor: 3}},
	trend("trend.visitors", 20, 20),
	trend("trend.calls", 100, 20),
	trend("trend.failed", 100, 5),
	trend("trend.auto_share", 0, 5),
	{ID: "source.silent", Group: "source", Condition: true, Params: []string{pSource, pAfter},
		Required: []string{pAfter}},
	collector("collector.refused", false),
	collector("collector.dropped", false),
	collector("collector.unknown_domain", true),
	{ID: KindJobFailed, Group: "instance", Condition: true, Instance: true, Params: []string{pJobs},
		Defaults: Params{Jobs: Jobs}},
	{ID: KindSizeCap, Group: "instance", Condition: true, Instance: true},
	{ID: "llm.budget", Group: "instance", Condition: true},
	level("level.failed", pCalls),
	level("level.p95"),
	level("level.requests"),
	level("level.visitors"),
}

// Lookup is the catalogue's trigger id.
func Lookup(id string) (Spec, bool) {
	i := slices.IndexFunc(Catalogue, func(s Spec) bool { return s.ID == id })
	if i < 0 {
		return Spec{}, false
	}
	return Catalogue[i], true
}

func (s Spec) takes(param string) bool { return slices.Contains(s.Params, param) }
