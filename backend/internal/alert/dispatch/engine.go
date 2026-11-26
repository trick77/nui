// Package dispatch runs the alert rules and delivers what they find: a step evaluates the rules against the
// dashboard's figures and the jobs' conditions, records the transitions in the alert table, and sends what is due,
// one bundled message per channel. The server's ticker and the simulator's virtual clock call the same step.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/trick77/ship/kit/logging"
	"github.com/trick77/ship/kit/run"

	"github.com/trick77/trakk/backend/internal/alert"
	"github.com/trick77/trakk/backend/internal/sites"
	"github.com/trick77/trakk/backend/internal/store"
)

const (
	// Every is the ticker's step: the outbox is sent from once a minute.
	Every = time.Minute
	// condEvery spaces the condition rules' passes; the facts are evaluated once an hour closes.
	condEvery = 5 * time.Minute
	// staleAfter is how old a due message may be and still go out, and how long a rule may not have run before its
	// next pass starts with a baseline: after a downtime nothing older is sent, nor everything found meanwhile at once.
	staleAfter = 6 * time.Hour
	// dueBatch is the most due rows one step sends.
	dueBatch = 200
	// maxBackoff bounds the wait before a failed channel is tried again.
	maxBackoff = 30 * time.Minute
	// maxLines is the most alerts one message lists; the rest is "and n more".
	maxLines = 20
)

// Options shape an Engine.
type Options struct {
	Store  *store.Store
	Sites  *sites.Registry
	Config alert.Config
	// Senders are the channels' senders by name; a channel without one is not sendable.
	Senders map[string]Sender
	// Board holds the jobs' conditions (job.failed, db.size_cap).
	Board *alert.Board
	// Source is where the site rules read their figures; nil: only the board's rules run.
	Source Source
	// PublicFQDN is the host a message's link names: the dashboard's own, never another.
	PublicFQDN string
	// Hello sends every channel a test message when Run starts. Never set over simulated traffic.
	Hello bool
	Log   *slog.Logger
}

// Source is what the site rules read: the dashboard's computed facts and figures.
type Source interface {
	// Occurrences is what rule r matches on site now, ok false when r is not evaluated in this pass. facts says
	// whether the pass evaluates the fact rules (an hour closed since the last).
	Occurrences(ctx context.Context, now time.Time, r alert.Rule, site *sites.Site, facts bool) ([]alert.Occurrence,
		bool, error)
	// Instance is what an instance rule other than the board's matches now (collector.unknown_domain).
	Instance(ctx context.Context, now time.Time, r alert.Rule) ([]alert.Occurrence, bool, error)
}

// Engine evaluates the rules and sends their alerts. Step runs on one goroutine; Status may be asked beside it.
type Engine struct {
	o       Options
	log     *slog.Logger
	sending bool
	// factHour is the unix hour of the last facts pass, condAt the last conditions pass.
	factHour int64
	condAt   time.Time
	// factTries counts this hour's failed facts passes; retired: the rows of rules gone are closed.
	factTries int
	retired   bool
	// sent is each channel's sends in the last hour (its cap), retry each failed row's next try.
	sent  map[string][]time.Time
	retry map[int64]time.Time
}

// New returns an engine. A channel without a sender is logged once and sent nothing.
func New(o Options) *Engine {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Board == nil {
		o.Board = alert.NewBoard(nil)
	}
	e := &Engine{o: o, log: o.Log, sent: map[string][]time.Time{}, retry: map[int64]time.Time{}}
	e.sending = o.Config.Sending()
	if e.sending {
		for _, ch := range o.Config.Channels {
			if o.Senders[ch.Name] == nil {
				e.log.Warn("Notification channel not sendable: this build has no sender for it, nothing is sent there",
					"channel", ch)
			}
		}
	}
	return e
}

// Run steps every minute until ctx ends; the first step at once, the start's test message beside the steps.
func (e *Engine) Run(ctx context.Context) {
	said := make(chan struct{})
	go func() { defer close(said); e.hello(ctx, time.Now) }()
	defer func() { <-said }()
	for wait := time.Duration(0); run.Sleep(ctx, wait); wait = Every {
		if err := e.Step(ctx, time.Now()); err != nil && ctx.Err() == nil {
			e.log.Warn("Alerts step failed, retried at the next", logging.Err(err))
		}
	}
}

// Step is one pass at now: the condition rules every five minutes, the fact rules once an hour closed (after a
// downtime the latest closed hour once, never the hours missed), then the outbox.
func (e *Engine) Step(ctx context.Context, now time.Time) error {
	var errs []error
	if !e.retired {
		err := e.retire(ctx, now)
		e.retired = err == nil
		errs = append(errs, err)
	}
	hour := now.Unix() / 3600
	facts := hour != e.factHour
	conds := facts || e.condAt.IsZero() || now.Sub(e.condAt) >= condEvery
	if conds {
		err := e.evaluate(ctx, now, facts)
		errs = append(errs, err)
		e.condAt = now
		// A facts pass that failed is tried again at the next step, a few times an hour: not every minute for good.
		if facts && (err == nil || e.factTries+1 >= factTries) {
			e.factHour, e.factTries = hour, 0
		} else if facts {
			e.factTries++
		}
	}
	errs = append(errs, e.dispatch(ctx, now))
	return errors.Join(errs...)
}

// factTries is how often a failed facts pass is run in its hour before the next hour's.
const factTries = 3

// retire closes, in silence, the rows still in their set of a rule or site no longer evaluated: a rule renamed,
// removed or switched off, a site gone from the rule's selectors or the sites file. Nothing would ever close them,
// nor prune them; nothing is sent about a rule that is gone. The configuration changes only with a restart: once,
// at the first step.
func (e *Engine) retire(ctx context.Context, now time.Time) error {
	live := map[store.AlertRule]bool{}
	for _, r := range e.o.Config.Rules {
		if !r.Enabled {
			continue
		}
		if r.Spec.Instance {
			live[store.AlertRule{Rule: r.Name}] = true
			continue
		}
		for _, s := range e.sitesOf(r) {
			live[store.AlertRule{Rule: r.Name, SiteID: s.ID}] = true
		}
	}
	passes, err := e.o.Store.AlertRules(ctx)
	if err != nil {
		return err
	}
	nowMS := now.UnixMilli()
	for _, p := range passes {
		if live[store.AlertRule{Rule: p.Rule, SiteID: p.SiteID}] {
			continue
		}
		rows, err := e.o.Store.InSet(ctx, p.Rule, p.SiteID)
		if err != nil {
			return err
		}
		for i := range rows {
			a := &rows[i]
			if a.State == store.AlertPending {
				if err := e.o.Store.DeleteAlert(ctx, a.ID); err != nil {
					return err
				}
				continue
			}
			if a.State == store.AlertOpen {
				a.State = store.AlertCleared
			}
			if a.Due != "" { // withdrawn: unsent, it is no failure either
				a.Due, a.DueAt = "", nil
				if len(a.Delivered) == 0 {
					a.Channels, a.Failed = nil, nil
				}
			}
			a.LeftAt = &nowMS
			delete(e.retry, a.ID)
			if err := e.o.Store.UpdateAlert(ctx, a); err != nil {
				return err
			}
		}
	}
	return nil
}

// evaluate runs every enabled rule on its sites (the instance: site 0): the condition rules, and with facts the
// fact rules.
func (e *Engine) evaluate(ctx context.Context, now time.Time, facts bool) error {
	var errs []error
	for _, r := range e.o.Config.Rules {
		if !r.Enabled || (!r.Spec.Condition && !facts) {
			continue
		}
		if r.Spec.Instance {
			occ, ok, err := e.instance(ctx, now, r)
			if err == nil && ok {
				err = e.apply(ctx, now, r, 0, occ)
			}
			errs = append(errs, err)
			continue
		}
		if e.o.Source == nil {
			continue
		}
		for _, s := range e.sitesOf(r) {
			occ, ok, err := e.o.Source.Occurrences(ctx, now, r, s, facts)
			if err == nil && ok {
				err = e.apply(ctx, now, r, s.ID, occ)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("rule %s, %s: %w", r.Name, s.Domain, err))
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	return errors.Join(errs...)
}

// sitesOf are the sites a rule selects: those it names and those of the products it names, every one when it
// names neither.
func (e *Engine) sitesOf(r alert.Rule) []*sites.Site {
	if e.o.Sites == nil {
		return nil
	}
	var out []*sites.Site
	for _, s := range e.o.Sites.All() {
		if selects(r, s) {
			out = append(out, s)
		}
	}
	return out
}

// instance is what an instance rule matches now: the board's conditions for the jobs and the size cap. A job not
// heard from since start holds its row as it is: a restart is no success.
func (e *Engine) instance(ctx context.Context, now time.Time, r alert.Rule) ([]alert.Occurrence, bool, error) {
	switch r.Spec.ID {
	case alert.KindJobFailed, alert.KindSizeCap:
	default:
		if e.o.Source == nil {
			return nil, false, nil
		}
		return e.o.Source.Instance(ctx, now, r)
	}
	keys := []string{""}
	if r.Spec.ID == alert.KindJobFailed {
		keys = r.P.Jobs
	}
	known := map[string]alert.Condition{}
	for _, c := range e.o.Board.Conditions() {
		if c.Kind == r.Spec.ID {
			known[c.Key] = c
		}
	}
	var out []alert.Occurrence
	for _, k := range keys {
		c, ok := known[k]
		switch {
		case !ok:
			out = append(out, alert.Occurrence{Key: k, Hold: true})
		case c.Open:
			out = append(out, conditionOccurrence(c))
		}
	}
	return out, true, nil
}

// jobNames are the jobs as a message names them.
var jobNames = map[string]string{alert.JobAgg: "aggregate build", alert.JobRetention: "retention",
	alert.JobPurge: "raw purge", alert.JobRefdata: "reference data refresh", alert.JobClassify: "classifier",
	alert.JobNames: "naming of the stored events", alert.JobWriter: "event writer"}

func conditionOccurrence(c alert.Condition) alert.Occurrence {
	o := alert.Occurrence{Key: c.Key, Detail: alert.Line(alert.Scrub(c.Text), 300),
		Target: &alert.Target{Panel: "domain", Filter: map[string]string{}}}
	if c.Kind == alert.KindSizeCap {
		o.Text = "The database reached its size cap: the oldest data is deleted"
	} else {
		o.Text = "The " + jobNames[c.Key] + " failed"
	}
	return o
}

// apply records one pass of rule r on a site: a key entering the set opens a row (sent, unless the pass is a
// baseline or the key left less than the cooldown ago), one leaving closes it (a condition's clearing sent once its
// opening was). The rows are the state across passes and restarts.
func (e *Engine) apply(ctx context.Context, now time.Time, r alert.Rule, site int64, set []alert.Occurrence) error {
	st := e.o.Store
	last, ok, err := st.AlertRuleOf(ctx, r.Name, site)
	if err != nil {
		return err
	}
	nowMS := now.UnixMilli()
	baseline := !ok || last.Sig != r.Sig || nowMS-last.CheckedAt > staleAfter.Milliseconds()
	rows, err := st.InSet(ctx, r.Name, site)
	if err != nil {
		return err
	}
	open := map[string]*store.Alert{}
	for i := range rows {
		open[rows[i].Key] = &rows[i]
	}
	seen := map[string]bool{}
	waiting := false
	for _, occ := range set {
		occ.Key = alert.ClipKey(occ.Key, maxKey)
		if seen[occ.Key] {
			continue
		}
		seen[occ.Key] = true
		waiting = waiting || (occ.Hold && occ.Wait)
		if a, in := open[occ.Key]; in {
			if occ.Hold {
				// A pending one is not known to have held meanwhile: its for starts over.
				if a.State == store.AlertPending {
					if err := e.leave(ctx, now, r, a); err != nil {
						return err
					}
				}
				continue
			}
			if err := e.stay(ctx, now, r, a, occ); err != nil {
				return err
			}
			continue
		}
		if occ.Hold {
			continue
		}
		if err := e.enter(ctx, now, r, site, occ, baseline); err != nil {
			return err
		}
	}
	for _, k := range slices.Sorted(maps.Keys(open)) {
		if !seen[k] {
			if err := e.leave(ctx, now, r, open[k]); err != nil {
				return err
			}
		}
	}
	if baseline && waiting {
		// Not recorded as a pass: the next is the baseline still, for the keys judged only then (a source silent since
		// before the start would otherwise be sent as news once its after passed). A first one is known all the same,
		// so retire closes its rows (a quiet note, shown) should the rule go: checked at 0 means known, never judged,
		// stale for the next pass.
		if !ok {
			return st.SetAlertRule(ctx, store.AlertRule{Rule: r.Name, SiteID: site, Sig: r.Sig})
		}
		return nil
	}
	return st.SetAlertRule(ctx, store.AlertRule{Rule: r.Name, SiteID: site, Sig: r.Sig, CheckedAt: nowMS})
}

// maxKey bounds a stored key, in bytes.
const maxKey = 512

// stay updates a row still in the set: its figures, a pending condition that held long enough opened, an open one
// reminded of every repeat.
func (e *Engine) stay(ctx context.Context, now time.Time, r alert.Rule, a *store.Alert, occ alert.Occurrence) error {
	nowMS := now.UnixMilli()
	fill(a, occ)
	a.LastAt = nowMS
	switch {
	case a.State == store.AlertPending && nowMS-a.FirstAt >= r.For.Milliseconds():
		a.State = store.AlertOpen
		e.due(a, r, "open", now)
	case a.State == store.AlertOpen && r.Repeat > 0 && a.Told == "open" && a.Due == "" && a.NotifiedAt != nil &&
		nowMS-*a.NotifiedAt >= r.Repeat.Milliseconds():
		e.due(a, r, "remind", now)
	case e.unheard(a, r, now):
		e.due(a, r, "open", now)
	}
	return e.o.Store.UpdateAlert(ctx, a)
}

// unheard reports whether an open condition is one its recipients do not know is open, with nothing on its way:
// its opening never got through (given up after a downtime, or withdrawn when it cleared before it was sent and
// came back within the cooldown), or it came back after a clearing they heard and has held for the cooldown since. Its
// opening is sent again, or it would stay unreported for as long as it lasts.
func (e *Engine) unheard(a *store.Alert, r alert.Rule, now time.Time) bool {
	if a.State != store.AlertOpen || a.Told == "open" || a.Due != "" || !e.sending {
		return false
	}
	if a.Told == "clear" && a.NotifiedAt != nil && now.UnixMilli()-*a.NotifiedAt < r.Cooldown.Milliseconds() {
		return false // a flap within the cooldown stays one message
	}
	return len(e.wanted(r)) > 0
}

// enter records a key new in the set.
func (e *Engine) enter(ctx context.Context, now time.Time, r alert.Rule, site int64, occ alert.Occurrence,
	baseline bool) error {
	st, nowMS := e.o.Store, now.UnixMilli()
	if !baseline && !occ.Quiet && r.Cooldown > 0 {
		// Back within the cooldown: the row it left reopens, quietly. Its clearing then stays quiet too (told is
		// still clear): a flapping key is one message, not one per flap.
		prev, ok, err := st.LastAlert(ctx, r.Name, site, occ.Key)
		if err != nil {
			return err
		}
		if ok && prev.LeftAt != nil && prev.State != store.AlertBaseline && nowMS-*prev.LeftAt < r.Cooldown.Milliseconds() {
			fill(&prev, occ)
			prev.LeftAt, prev.LastAt = nil, nowMS
			if prev.State == store.AlertCleared {
				prev.State = store.AlertOpen
				if prev.Due == "clear" { // its clearing not sent yet: withdrawn, the recipients still know it open
					prev.Due, prev.DueAt, prev.Channels = "", nil, nil
					delete(e.retry, prev.ID)
				}
				if e.unheard(&prev, r, now) {
					e.due(&prev, r, "open", now)
				}
			}
			return st.UpdateAlert(ctx, &prev)
		}
	}
	a := &store.Alert{SiteID: site, Rule: r.Name, Kind: r.Spec.ID, Key: occ.Key, Severity: r.Severity.String(),
		FirstAt: nowMS, LastAt: nowMS}
	fill(a, occ)
	switch {
	case occ.Quiet:
		// Shown on a baseline too: the note is of a rule that cannot judge yet, whose every pass is a baseline.
		a.State = store.AlertOnce
	case baseline:
		a.State = store.AlertBaseline
	case !r.Spec.Condition:
		a.State = store.AlertOnce
		e.due(a, r, "open", now)
	case r.For > 0:
		a.State = store.AlertPending
	default:
		a.State = store.AlertOpen
		e.due(a, r, "open", now)
	}
	return st.InsertAlert(ctx, a)
}

// leave closes a row whose key left the set: a pending condition never was one, an open one is cleared (sent when
// its opening was), anything else just leaves.
func (e *Engine) leave(ctx context.Context, now time.Time, r alert.Rule, a *store.Alert) error {
	nowMS := now.UnixMilli()
	switch a.State {
	case store.AlertPending:
		return e.o.Store.DeleteAlert(ctx, a.ID)
	case store.AlertOpen:
		a.State = store.AlertCleared
		switch {
		case a.Told == "open":
			e.due(a, r, "clear", now)
		case a.Due != "":
			// Its opening was never heard: its clearing is not news either.
			a.Due, a.DueAt, a.Channels = "", nil, nil
		}
	}
	a.LeftAt = &nowMS
	return e.o.Store.UpdateAlert(ctx, a)
}

// fill takes an occurrence's figures and text into a row.
func fill(a *store.Alert, occ alert.Occurrence) {
	a.Figure, a.Unit, a.Text, a.Detail = occ.Figure, occ.Unit, occ.Text, occ.Detail
	a.Panel, a.Filter = "", nil
	if occ.Target != nil {
		a.Panel, a.Filter = occ.Target.Panel, occ.Target.Filter
	}
}

// due makes a message about a row wait for its channels: those of the rule that take its severity and can be sent
// to. None (nothing sent at all, or none fits) leaves it unsent, its delivery none.
func (e *Engine) due(a *store.Alert, r alert.Rule, what string, now time.Time) {
	chans := e.wanted(r)
	a.Channels, a.Delivered, a.Failed, a.Attempts = chans, nil, nil, 0
	a.Due, a.DueAt = "", nil
	delete(e.retry, a.ID) // a new message waits for no earlier one's backoff
	if len(chans) > 0 {
		at := now.UnixMilli()
		a.Due, a.DueAt = what, &at
	}
}

// wanted are the channels a rule's messages go to: those it names that take its severity and can be sent to.
func (e *Engine) wanted(r alert.Rule) []string {
	var chans []string
	if !e.sending {
		return nil
	}
	for _, n := range r.Channels {
		ch, ok := e.o.Config.Channel(n)
		if ok && e.o.Senders[n] != nil && r.Severity >= ch.MinSeverity && !slices.Contains(chans, n) {
			chans = append(chans, n)
		}
	}
	return chans
}

// Status is the notifications' state for the dashboard's footer.
func (e *Engine) Status(ctx context.Context) (alert.Status, error) {
	c := e.o.Config
	s := alert.Status{Configured: c.Configured, Enabled: c.Enabled, Faults: slices.Clone(c.Faults)}
	for _, r := range c.Rules {
		if r.Enabled {
			s.Active++
		} else {
			s.Off++
		}
	}
	known, err := e.o.Store.AlertChannels(ctx)
	if err != nil {
		return s, err
	}
	for _, ch := range c.Channels {
		cs := alert.ChannelStatus{Name: ch.Name, Service: ch.Service, MinSeverity: ch.MinSeverity}
		if k, ok := known[ch.Name]; ok {
			if k.LastOK != nil {
				cs.LastOK = time.UnixMilli(*k.LastOK).UTC()
			}
			if k.ErrorAt != nil {
				cs.ErrorAt, cs.Error = time.UnixMilli(*k.ErrorAt).UTC(), k.Error
			}
		}
		s.Channels = append(s.Channels, cs)
	}
	return s, nil
}
