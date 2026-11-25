package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"

	"github.com/trick77/trakk/backend/internal/alert"
	"github.com/trick77/trakk/backend/internal/alert/dispatch"
	"github.com/trick77/trakk/backend/internal/app"
	"github.com/trick77/trakk/backend/internal/sites"
)

// notifyCmd is -notify-check and -notify-test: the notify file checked against the sites file (its selectors name
// domains and products), never the database. Check sends nothing; test sends a test message to every channel. 0
// when the file has no fault (and, testing, every channel took the message).
func notifyCmd(ctx context.Context, cfg app.Config, test bool, out io.Writer) int {
	path, _ := cfg.SitesPath()
	cs, err := sites.Load(path)
	if err != nil {
		_, _ = fmt.Fprintf(out, "sites file %s: %v\n", path, err)
		return 1
	}
	var k alert.Known
	for _, c := range cs {
		k.Domains = append(k.Domains, c.Domain)
		if !slices.Contains(k.Products, c.Product) {
			k.Products = append(k.Products, c.Product)
		}
	}
	c := alert.Load(cfg.NotifyFile, k)
	_, _ = fmt.Fprintf(out, "notify file %s\n", cfg.NotifyFile)
	switch {
	case !c.Configured:
		_, _ = fmt.Fprintln(out, "missing: nothing is sent, the default rules record (README, Notifications)")
		return 1
	case len(c.Faults) > 0:
		for _, f := range c.Faults {
			_, _ = fmt.Fprintln(out, "fault:", f)
		}
		_, _ = fmt.Fprintf(out, "%d faults: Trakk starts with this file, records its default rules and sends nothing\n",
			len(c.Faults))
		return 1
	}
	for _, ch := range c.Channels {
		_, _ = fmt.Fprintf(out, "channel %s (%s), from %s\n", ch.Name, ch.Service, ch.MinSeverity)
	}
	for _, r := range c.Rules {
		_, _ = fmt.Fprintf(out, "rule %s: %s, %s\n", r.Name, r.Spec.ID, ruleState(c, r))
	}
	if c.Defaulted {
		_, _ = fmt.Fprintln(out, "no rule: the default rules record, nothing is sent")
	}
	if !test {
		_, _ = fmt.Fprintln(out, "ok")
		return 0
	}
	if err := dispatch.Test(ctx, c, newSenders(c), out); err != nil {
		return 1
	}
	return 0
}

// ruleState says whether a rule runs and where it sends.
func ruleState(c alert.Config, r alert.Rule) string {
	switch {
	case !r.Enabled:
		return "off"
	case !c.Enabled:
		return "active, sending switched off"
	case len(r.Channels) == 0:
		return "active, recorded only"
	}
	return "active, to " + strings.Join(r.Channels, ", ")
}

// loadNotify reads the notify file for the registry's sites and logs what it holds: a line per rule, every fault.
// Faults never stop the start: the collector must not stop over a typo in a rule.
func loadNotify(cfg app.Config, reg *sites.Registry) alert.Config {
	var k alert.Known
	for _, s := range reg.All() {
		k.Domains = append(k.Domains, s.Domain)
		if !slices.Contains(k.Products, s.Product) {
			k.Products = append(k.Products, s.Product)
		}
	}
	c := alert.Load(cfg.NotifyFile, k)
	switch {
	case !c.Configured:
		slog.Info("No notify file: alerts are recorded with the default rules, nothing is sent",
			"notify_file", cfg.NotifyFile)
	case len(c.Faults) > 0:
		for _, f := range c.Faults {
			slog.Warn("Notify file fault", "notify_file", cfg.NotifyFile, "fault", f)
		}
		slog.Warn("Notify file has faults: nothing is sent, alerts are recorded with the default rules "+
			"(server -notify-check lists the faults)", "notify_file", cfg.NotifyFile, "faults", len(c.Faults))
	case !c.Enabled:
		slog.Info("Notifications switched off in the notify file: alerts are recorded, nothing is sent")
	}
	if !c.Defaulted {
		for _, r := range c.Rules {
			slog.Info("Alert rule", "rule", r.Name, "on", r.Spec.ID, "state", ruleState(c, r))
		}
	}
	return c
}
