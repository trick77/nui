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

	"github.com/trick77/toolkit/notify"

	"github.com/trick77/trakk/backend/internal/alert"
	"github.com/trick77/trakk/backend/internal/store"
)

// dispatch sends the due messages: one per channel, every due row that channel still owes in it, within the
// channel's hourly cap. A row older than staleAfter is given up. A send the shutdown cut is no attempt: its rows
// stay due for the next start, and what the channels before it took is kept.
func (e *Engine) dispatch(ctx context.Context, now time.Time) error {
	rows, errs, err := e.sendable(ctx, now)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	nowMS := now.UnixMilli()
	owed := map[string][]*store.Alert{}
	touched := map[int64]*store.Alert{}
	for _, a := range rows {
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
		failure := Failure(res)
		if ctx.Err() != nil {
			// What took its message is kept, this channel's too when it did before the cut, or they would get it
			// again after the restart. A send the cut ended is no attempt.
			if failure == "" {
				delivered(items, name, nowMS, touched)
			}
			pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistBudget)
			errs = append(errs, e.settle(pctx, now, touched), ctx.Err())
			cancel()
			return errors.Join(errs...)
		}
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
		if failure != "" {
			for _, a := range items {
				touched[a.ID] = a
				a.Failed = appendNew(a.Failed, name)
			}
			continue
		}
		delivered(items, name, nowMS, touched)
	}
	return errors.Join(append(errs, e.settle(ctx, now, touched))...)
}

// delivered records that a channel took a message about items: what its recipients now know.
func delivered(items []*store.Alert, name string, nowMS int64, touched map[int64]*store.Alert) {
	for _, a := range items {
		touched[a.ID] = a
		a.Delivered = appendNew(a.Delivered, name)
		a.Failed = slices.DeleteFunc(a.Failed, func(n string) bool { return n == name })
		a.NotifiedAt = &nowMS
		a.Told = "open"
		if a.Due == "clear" {
			a.Told = "clear"
		}
	}
}

// persistBudget bounds the write of what a send cut by the shutdown already delivered: well inside the shutdown's
// margin (cmd/server's bgStop).
const persistBudget = time.Second

// settle writes the rows a step sent: an attempt each, done once every channel took it, else a growing backoff.
func (e *Engine) settle(ctx context.Context, now time.Time, touched map[int64]*store.Alert) error {
	var errs []error
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

// maxDueScan bounds the due rows one step reads to find its batch.
const maxDueScan = 10 * dueBatch

// sendable are the due rows a step sends, at most dueBatch, the oldest first: those not backing off after a failure
// that owe a channel not at its hourly cap. The outbox is read a page at a time past the rows waiting, so a channel
// that is down holds no slot a healthy one needs. Rows older than staleAfter are given up on the way.
func (e *Engine) sendable(ctx context.Context, now time.Time) ([]*store.Alert, []error, error) {
	nowMS := now.UnixMilli()
	capped := map[string]bool{}
	isCapped := func(name string) bool {
		c, ok := capped[name]
		if !ok {
			ch, inConf := e.o.Config.Channel(name)
			c = inConf && e.capped(name, ch.MaxPerHour, now)
			capped[name] = c
		}
		return c
	}
	var out []*store.Alert
	var errs []error
	var at, id int64
	for scanned := 0; scanned < maxDueScan && len(out) < dueBatch; {
		page, err := e.o.Store.DueAlerts(ctx, at, id, dueBatch)
		if err != nil {
			return nil, errs, err
		}
		for i := range page {
			a := &page[i]
			if a.DueAt != nil {
				at, id = *a.DueAt, a.ID
			}
			// A clearing of what the recipients know open never ages out: dropped, they would believe it open for
			// good. It goes once the channel takes it again.
			stale := a.DueAt == nil || nowMS-*a.DueAt > staleAfter.Milliseconds()
			if heard := a.Due == "clear" && a.Told == "open"; stale && !heard {
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
			if slices.ContainsFunc(a.Channels, func(n string) bool {
				return !slices.Contains(a.Delivered, n) && !isCapped(n)
			}) && len(out) < dueBatch {
				out = append(out, a)
			}
		}
		scanned += len(page)
		if len(page) < dueBatch {
			break
		}
	}
	return out, errs, nil
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
	for _, a := range items { // those past maxLines too: the link is to all of them
		sites[a.SiteID] = true
	}
	for i, a := range items {
		if i == maxLines {
			fmt.Fprintf(&b, "\nand %d more\n", len(items)-maxLines)
			break
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(e.headline(a))
		if a.Figure != nil {
			fmt.Fprintf(&b, " (%s%s)", figure(*a.Figure), unitText(a.Unit))
		}
		b.WriteByte('\n')
		if a.Detail != "" {
			b.WriteString(notify.Plain(a.Detail, maxText) + "\n")
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

// maxText bounds an alert's text and detail in a message.
const maxText = 300

// headline is an alert's line: what happened, where. Its text holds the outside world's (a path, an error's
// message): defused on its way out, no channel formats, links or mentions anything in it. The links a message
// carries are the dashboard's own, added as they are.
func (e *Engine) headline(a *store.Alert) string {
	prefix := map[string]string{"clear": "Cleared: ", "remind": "Still open: "}[a.Due]
	return prefix + e.where(a.SiteID) + ": " + notify.Plain(a.Text, maxText)
}

// where names a site as the dashboard does, the instance as such.
func (e *Engine) where(site int64) string {
	if site == 0 {
		return "Trakk"
	}
	if e.o.Sites != nil {
		if s, ok := e.o.Sites.ByID(site); ok {
			return s.Label()
		}
	}
	return "site " + strconv.FormatInt(site, 10)
}

// link is the dashboard's alerts page for the sites of a message (one: that site's), "" without a public host.
func (e *Engine) link(sites map[int64]bool) string {
	if e.o.PublicFQDN == "" {
		return ""
	}
	// One site's alerts, or all of them: the page shows the domain the browser remembers unless told otherwise.
	u := url.URL{Scheme: "https", Host: e.o.PublicFQDN, Path: "/dashboard/alerts", RawQuery: "all=1"}
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
