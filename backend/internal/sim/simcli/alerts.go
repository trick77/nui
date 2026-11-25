package simcli

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"time"

	"github.com/trick77/trakk/backend/internal/alert"
	"github.com/trick77/trakk/backend/internal/alert/dispatch"
	"github.com/trick77/trakk/backend/internal/dash"
	"github.com/trick77/trakk/backend/internal/sim"
	"github.com/trick77/trakk/backend/internal/sites"
	"github.com/trick77/trakk/backend/internal/store"
)

// NotifyFile is the notify file a simulated world plays: a rule of every group on every site, to one channel the
// recording sender takes (its URL is never dialled).
const NotifyFile = `{
  "channels": {"sim": "generic://sim.example.com/hook"},
  "defaults": {"channels": ["sim"]},
  "rules": [
    {"name": "new-errors", "on": "error.new"},
    {"name": "new-failures", "on": "failure.new"},
    {"name": "new-probes", "on": "probe.new"},
    {"name": "new-endpoints", "on": "endpoint.new"},
    {"name": "new-referrers", "on": "referrer.new"},
    {"name": "addr-spike", "on": "spike.addr", "severity": "high"},
    {"name": "net-spike", "on": "spike.net", "severity": "high"},
    {"name": "visitors-trend", "on": "trend.visitors"},
    {"name": "calls-trend", "on": "trend.calls"},
    {"name": "failed-trend", "on": "trend.failed"},
    {"name": "automation-trend", "on": "trend.auto_share", "severity": "low"},
    {"name": "feed-silent", "on": "source.silent", "source": "server", "after": "2h", "severity": "high"},
    {"name": "refused", "on": "collector.refused", "min": 100, "window": "1h"},
    {"name": "dropped", "on": "collector.dropped", "window": "1h"},
    {"name": "unknown-domains", "on": "collector.unknown_domain", "window": "1h", "severity": "low"},
    {"name": "jobs", "on": "job.failed"},
    {"name": "size-cap", "on": "db.size_cap"},
    {"name": "model-budget", "on": "llm.budget", "severity": "low"},
    {"name": "requests-surge", "on": "level.requests", "window": "1h", "min": 1000},
    {"name": "requests-drop", "on": "level.requests", "window": "1h", "drop": 90},
    {"name": "api-slow", "on": "level.p95", "window": "1h", "min": 2000},
    {"name": "api-failing", "on": "level.failed", "window": "1h", "min": 50, "calls": 20},
    {"name": "errors-up", "on": "level.errors", "window": "12h", "min": 30}
  ]
}`

// stepDays are the days before the anchor server -sim's backfill steps the alert engine through: the alerts page has
// the scenarios' alerts (deploy-error's start, the earliest, 5 days before), a baseline pass before. Every hour of a
// whole history would take minutes.
const stepDays = 6

// Alerting is the alert engine a simulated history is played through: the world's notify file (NotifyFile), every
// message kept by Recorder, the dashboard's own reads on the virtual clock.
type Alerting struct {
	Engine   *dispatch.Engine
	Recorder *dispatch.Recorder
	Config   alert.Config
}

// NewAlerting is st's alert engine for the world w, its sites synced into st.
func NewAlerting(ctx context.Context, st *store.Store, w *sim.World, rec *dispatch.Recorder) (*Alerting, error) {
	rows, err := st.Sites(ctx)
	if err != nil {
		return nil, err
	}
	reg, err := sites.New(rows)
	if err != nil {
		return nil, err
	}
	c := alert.Parse([]byte(NotifyFile), Known(reg))
	d := &dash.Server{DB: st, Sites: reg, Ref: w.Ref.Data, RetentionMonths: 25}
	// No process start: the history's collector ran all along, a source is silent by its events alone.
	e := dispatch.New(dispatch.Options{Store: st, Sites: reg, Config: c, Senders: rec.Senders(c),
		Source: dispatch.NewEval(d, c.Rules, time.Time{}, nil), PublicFQDN: "trakk.example.com",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return &Alerting{Engine: e, Recorder: rec, Config: c}, nil
}

// Known are a registry's domains and products, what a notify file's selectors are checked against.
func Known(reg *sites.Registry) alert.Known {
	var k alert.Known
	for _, s := range reg.All() {
		k.Domains = append(k.Domains, s.Domain)
		if !slices.Contains(k.Products, s.Product) {
			k.Products = append(k.Products, s.Product)
		}
	}
	return k
}

// backfillAlerting is a backfill's options with the alert engine stepping through the hours at takes (by their
// end); at nil: no engine.
func backfillAlerting(ctx context.Context, st *store.Store, w *sim.World, rec *dispatch.Recorder,
	o sim.BackfillOptions, at func(time.Time) bool) (sim.BackfillOptions, error) {
	if at == nil {
		return o, nil
	}
	a, err := NewAlerting(ctx, st, w, rec)
	if err != nil {
		return o, err
	}
	o.Step, o.StepAt = a.Engine.Step, at
	return o, nil
}

// lastDays takes the hours of the last stepDays before w's anchor the alerts page needs: every hour around the
// scenarios whose expectations read alerts (sim check's), every sim.AlertEvery hours else, every rule passing.
func lastDays(w *sim.World) func(time.Time) bool {
	from, near := w.Anchor.AddDate(0, 0, -stepDays), w.AlertHours()
	return func(end time.Time) bool {
		return !end.Before(from) && (end.Unix()/3600%sim.AlertEvery == 0 || (near != nil && near(end)))
	}
}
