package dispatch

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/trakk/backend/internal/alert"
	"github.com/trick77/trakk/backend/internal/store"
)

// dispatch sends the due messages: one per channel, every due row that channel still owes in it, within the
// channel's hourly cap. A row older than staleAfter is given up. A send the shutdown cut is no attempt: its rows
// stay due for the next start.
func (e *Engine) dispatch(ctx context.Context, now time.Time) error {
	rows, err := e.o.Store.DueAlerts(ctx, dueBatch)
	if err != nil {
		return err
	}
	nowMS := now.UnixMilli()
	owed := map[string][]*store.Alert{}
	touched := map[int64]*store.Alert{}
	var errs []error
	for i := range rows {
		a := &rows[i]
		if a.DueAt == nil || nowMS-*a.DueAt > staleAfter.Milliseconds() {
			for _, n := range a.Channels {
				if !slices.Contains(a.Delivered, n) && !slices.Contains(a.Failed, n) {
					a.Failed = append(a.Failed, n)
				}
			}
			a.Due, a.DueAt = "", nil
			delete(e.retry, a.ID)
			errs = append(errs, e.o.Store.UpdateAlert(ctx, a))
			continue
		}
		if next, ok := e.retry[a.ID]; ok && now.Before(next) {
			continue
		}
		for _, n := range a.Channels {
			if !slices.Contains(a.Delivered, n) {
				owed[n] = append(owed[n], a)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(owed)) {
		items := owed[name]
		ch, inConf := e.o.Config.Channel(name)
		sender := e.o.Senders[name]
		if !inConf || sender == nil || !e.sending {
			// A channel gone from the file since (or sending switched off): it never takes these.
			for _, a := range items {
				a.Channels = slices.DeleteFunc(slices.Clone(a.Channels), func(n string) bool { return n == name })
				a.Failed = appendNew(a.Failed, name)
				touched[a.ID] = a
			}
			continue
		}
		if e.capped(name, ch.MaxPerHour, now) {
			continue
		}
		title, body, sev := e.render(items)
		res := sender.Deliver(ctx, title, body, sev)
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		failure := Failure(res)
		if failure == "" { // the cap spares the recipient; a refused send reached nobody (the backoff spaces those)
			e.sent[name] = append(e.sent[name], now)
		}
		if err := e.o.Store.ChannelDelivered(ctx, name, now, failure); err != nil {
			errs = append(errs, err)
		}
		if failure != "" {
			e.log.Warn("Notification not delivered, tried again later", "channel", ch, "alerts", len(items),
				"error", failure)
		}
		for _, a := range items {
			touched[a.ID] = a
			if failure != "" {
				a.Failed = appendNew(a.Failed, name)
				continue
			}
			a.Delivered = appendNew(a.Delivered, name)
			a.Failed = slices.DeleteFunc(a.Failed, func(n string) bool { return n == name })
			a.NotifiedAt = &nowMS
			a.Told = "open"
			if a.Due == "clear" {
				a.Told = "clear"
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(touched)) {
		a := touched[id]
		a.Attempts++
		if allIn(a.Channels, a.Delivered) {
			a.Due, a.DueAt = "", nil
			delete(e.retry, a.ID)
		} else {
			e.retry[a.ID] = now.Add(min(Every<<min(a.Attempts, 10), maxBackoff))
		}
		errs = append(errs, e.o.Store.UpdateAlert(ctx, a))
	}
	return errors.Join(errs...)
}

// capped reports whether a channel had its hour's messages, forgetting those older than an hour.
func (e *Engine) capped(name string, perHour int, now time.Time) bool {
	ts := slices.DeleteFunc(e.sent[name], func(t time.Time) bool { return now.Sub(t) >= time.Hour })
	e.sent[name] = ts
	return len(ts) >= perHour
}

func appendNew(xs []string, x string) []string {
	if slices.Contains(xs, x) {
		return xs
	}
	return append(xs, x)
}

func allIn(want, have []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// render is one channel's message about items: the title names the highest severity and, for one alert, the alert
// itself; the body has a line or two per alert and the link to the dashboard's own alerts page.
func (e *Engine) render(items []*store.Alert) (title, body string, sev alert.Severity) {
	sev = alert.Low
	for _, a := range items {
		if s, _ := alert.ParseSeverity(a.Severity); s > sev {
			sev = s
		}
	}
	if len(items) == 1 {
		title = alert.Clip(fmt.Sprintf("[%s] %s", sev, e.headline(items[0])), 160)
	} else {
		title = fmt.Sprintf("[%s] Trakk: %d alerts", sev, len(items))
	}
	var b strings.Builder
	sites := map[int64]bool{}
	for i, a := range items {
		if i == maxLines {
			fmt.Fprintf(&b, "\nand %d more\n", len(items)-maxLines)
			break
		}
		sites[a.SiteID] = true
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(e.headline(a))
		if a.Figure != nil {
			fmt.Fprintf(&b, " (%s%s)", figure(*a.Figure), unitText(a.Unit))
		}
		b.WriteByte('\n')
		if a.Detail != "" {
			b.WriteString(a.Detail + "\n")
		}
		if link := e.logLink(a); link != "" {
			b.WriteString(link + "\n")
		}
	}
	if link := e.link(sites); link != "" {
		b.WriteString("\n" + link + "\n")
	}
	return title, strings.TrimRight(b.String(), "\n"), sev
}

// headline is an alert's line: what happened, where.
func (e *Engine) headline(a *store.Alert) string {
	prefix := map[string]string{"clear": "Cleared: ", "remind": "Still open: "}[a.Due]
	return prefix + e.where(a.SiteID) + ": " + a.Text
}

// where names a site by its domain, the instance as such.
func (e *Engine) where(site int64) string {
	if site == 0 {
		return "Trakk"
	}
	if e.o.Sites != nil {
		if s, ok := e.o.Sites.ByID(site); ok {
			return s.Domain
		}
	}
	return "site " + strconv.FormatInt(site, 10)
}

// link is the dashboard's alerts page for the sites of a message (one: that site's), "" without a public host.
func (e *Engine) link(sites map[int64]bool) string {
	if e.o.PublicFQDN == "" {
		return ""
	}
	u := url.URL{Scheme: "https", Host: e.o.PublicFQDN, Path: "/dashboard/alerts"}
	if len(sites) == 1 {
		for id := range sites {
			if id != 0 && e.o.Sites != nil {
				if s, ok := e.o.Sites.ByID(id); ok {
					u.RawQuery = url.Values{"site": {s.Domain}}.Encode()
				}
			}
		}
	}
	return u.String()
}

// logLink is an alert's own rows in the dashboard's log over the last 24 hours, the log's filter its target's ("" for
// an alert whose target is no log, or without a public host): a new error's every occurrence.
func (e *Engine) logLink(a *store.Alert) string {
	if e.o.PublicFQDN == "" || a.Panel != "log" || a.SiteID == 0 || e.o.Sites == nil {
		return ""
	}
	s, ok := e.o.Sites.ByID(a.SiteID)
	if !ok {
		return ""
	}
	q := url.Values{"site": {s.Domain}, "range": {"24h"}}
	for k, v := range a.Filter {
		q.Set(k, v)
	}
	u := url.URL{Scheme: "https", Host: e.o.PublicFQDN, Path: "/dashboard/log", RawQuery: q.Encode()}
	return u.String()
}

func figure(f float64) string { return strconv.FormatFloat(math.Round(f*10)/10, 'f', -1, 64) }

func unitText(u string) string {
	if u == "" {
		return ""
	}
	return " " + u
}
