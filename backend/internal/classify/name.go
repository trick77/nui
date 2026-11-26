package classify

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/trick77/ship/kit/logging"
	"github.com/trick77/ship/kit/run"

	"github.com/trick77/trakk/backend/internal/alert"
	"github.com/trick77/trakk/backend/internal/refdata"
	"github.com/trick77/trakk/backend/internal/store"
)

// service is the bot list's name of a client of class cls at a (event.svc): of the service the evidence behind the
// class points to, the agent ua names, else the service; "" for any class but a bot's or a probe's, and where the list
// names nobody. One rule for the write, the pass and NameDay: a name that differed between them would restamp events
// and rebuild their days.
//
// The class carries the rDNS answer (a refuted claim is unknown), so the service follows from the list alone, in the
// order the verdicts weigh it: an authoritative range of the class, the one ua claims first (Rank); else the service ua
// claims, of the class (its client string alone, a claim rDNS confirmed), never one whose authoritative ranges miss a:
// that claim is refuted (clientVerdict), the class rests on other evidence; else the first range of the class,
// confirmed by rDNS without a claim. A client naming none of a shared range's services takes the first ranked's name,
// also where the verdict followed another client of the address naming another. Where rDNS confirmed a range that is
// not authoritative and ua claims another service of the class, or two services of one class share such a range, the
// claim's or the first's is named: which one rDNS confirmed is not kept.
func (c *Classifier) service(a netip.Addr, ua string, cls int) string {
	if cls != store.ClsBot && cls != store.ClsMonitor {
		return ""
	}
	bots := c.bots()
	class := func(p *refdata.Provider) bool { return p != nil && store.ClassCode(p.Class) == cls }
	// Rank puts the authoritative owners first: one of the class needs no claim looked up (the write path's cost).
	ranked := refdata.Rank(bots.ByIP(a), ua)
	p := first(ranked, func(p *refdata.Provider) bool { return class(p) && p.Authoritative })
	if p == nil {
		// Inside its own authoritative range a claim is named above; outside, it is refuted.
		if claimed, _ := bots.Claimed(a, ua); class(claimed) && (!claimed.Authoritative || len(claimed.Ranges) == 0) {
			p = claimed
		} else {
			p = first(ranked, class)
		}
	}
	if p == nil {
		return ""
	}
	if ag, ok := p.Agent(ua); ok {
		return ag.Name
	}
	return p.Name
}

func first(ps []*refdata.Provider, ok func(*refdata.Provider) bool) *refdata.Provider {
	if i := slices.IndexFunc(ps, ok); i >= 0 {
		return ps[i]
	}
	return nil
}

func (c *Classifier) bots() *refdata.Bots {
	if c.Ref == nil {
		return nil
	}
	return c.Ref.Bots()
}

// name is service by an event's stored fields (store.SvcNamer): the pass's and NameDay's rule. An address that does
// not parse takes none, as at the write (Enrich).
func (c *Classifier) name(ip, ua string, cls int) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	return c.service(a, ua, cls)
}

// NameDay gives a site-day's events the names service gives them by their address, client and class, and takes the
// name off any other class's. It returns the events changed: none on a day named already. Without a bot list it names
// nothing, ErrNoBotList: it would take every name off, and the aggregate build it runs before (agg.Options.Name)
// freeze that.
func (c *Classifier) NameDay(ctx context.Context, site, day int64) (int64, error) {
	if b := c.bots(); b == nil || len(b.Providers) == 0 {
		return 0, ErrNoBotList
	}
	return c.DB.NameSvcDay(ctx, site, day, c.name)
}

// nameYield is the pause between two days NameRaw names: the writer's turn at the lock.
const nameYield = 20 * time.Millisecond

// nameRetry is the pause before a failed NameRaw starts over (RunNames).
var nameRetry = Every

// nameListPoll is how often NameRaw looks for a bot list, nameListWait how long before it fails without one (RunNames
// reports it, tries again): a list that never loads (refresh off and nothing cached, a download that keeps failing)
// must not leave the events unnamed unseen. Longer than a cold start's first download.
var (
	nameListPoll = time.Minute
	nameListWait = time.Hour
)

// ErrNoBotList is NameDay's answer while no bot list is loaded, and NameRaw's failure past nameListWait: one text,
// one condition however often it is tried.
var ErrNoBotList = errors.New("no bot list loaded")

// RunNames runs NameRaw until the stored events are named, each failure reported (job.failed, names) and tried again
// after nameRetry: one busy database must not leave them unnamed until the next start, nor the days purged meanwhile
// for good. Through, or named before, it reports the job through.
func (c *Classifier) RunNames(ctx context.Context) {
	for {
		err := c.NameRaw(ctx)
		if ctx.Err() != nil {
			return
		}
		alert.Fail(c.Report, alert.JobNames, err)
		if err == nil {
			return
		}
		c.log().Warn("Naming the stored events failed, tried again", "in", nameRetry.String(), logging.Err(err))
		if !run.Sleep(ctx, nameRetry) {
			return
		}
	}
}

// NameRaw names the events written before event.svc existed, once (svc_named): every site's days, newest first, the
// inactive sites' too. It waits for a bot list, nameListWait at most: named without one, those events would keep no
// name for good. Stopped or failed before the end, it starts over (RunNames, the next start); a day named already
// changes nothing.
func (c *Classifier) NameRaw(ctx context.Context) error {
	done, err := c.DB.SvcNamed(ctx)
	if err != nil || done {
		return err
	}
	if b := c.bots(); b == nil || len(b.Providers) == 0 {
		c.log().Info("Naming the stored events waits for the bot list", "up to", nameListWait.String())
		for deadline := time.Now().Add(nameListWait); b == nil || len(b.Providers) == 0; b = c.bots() {
			wait := time.Until(deadline)
			if wait <= 0 {
				return ErrNoBotList
			}
			if !run.Sleep(ctx, min(nameListPoll, wait)) {
				return ctx.Err()
			}
		}
	}
	all, err := c.DB.Sites(ctx)
	if err != nil {
		return err
	}
	oldest := map[int64]int64{}
	first := int64(-1)
	for _, s := range all {
		day, ok, err := c.DB.OldestEventDay(ctx, s.ID)
		if err != nil {
			return err
		}
		if ok {
			oldest[s.ID] = day
			if first < 0 || day < first {
				first = day
			}
		}
	}
	start := time.Now()
	var total int64
	for day := c.now().UnixMilli() / dayMS; first >= 0 && day >= first; day-- {
		for site, from := range oldest {
			if day < from {
				continue
			}
			n, err := c.NameDay(ctx, site, day)
			total += n
			if err != nil {
				return fmt.Errorf("site %d day %d: %w", site, day, err)
			}
		}
		if !run.Sleep(ctx, nameYield) {
			return ctx.Err()
		}
	}
	if err := c.DB.MarkSvcNamed(ctx, c.now()); err != nil {
		return err
	}
	c.log().Info("Bot list names stamped on the stored events", "events", total,
		"took", time.Since(start).Round(time.Millisecond).String())
	return nil
}
