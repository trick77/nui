// Package watch walks the raw events a day at a time into a small state per site that outlives them, and finds in
// it a new host the pages load scripts from (script): a skimmer injected into a site shows that way. No list of
// hosts, no code per service: the pages' own reports judge them.
//
// A day merges into the state once it is closed (two hours after its end, as the aggregates'): the thresholds and
// the caps are a whole day's, and every first is set once. The days still open are judged whole again on every pass
// against the state; their findings are provisional (watch_open) and may go again. The job never writes an event:
// the aggregates' versions stay as they are.
package watch

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/trick77/ship/kit/logging"
	"github.com/trick77/ship/kit/run"

	"github.com/trick77/trakk/backend/internal/store"
)

const (
	dayMS = 86_400_000
	// freezeLag is how long after its end a day is closed: the aggregates' (agg.freezeLag), past the collector's
	// back-dating and a classifier pass.
	freezeLag = 2 * time.Hour
	// yield is the pause between two site-days: the writer's turn at the lock.
	yield = 20 * time.Millisecond
	// LearnDays is the learning window: the first week of a site's script reports finds nothing, every host is new
	// then.
	LearnDays = 7

	// The state's bounds per site, and the new hosts one day may add: the most networks first, the rest counted
	// (Site.Cut). Found hosts keep their place before the others, at most maxFoundHosts of them.
	maxHosts      = 500
	maxNewHosts   = 50
	maxFoundHosts = maxHosts / 2
	// maxPerDay is the most findings a day makes.
	maxPerDay = 10

	// minNets: a host is new once address keys of this many networks (IPv4 /24, IPv6 /48) reported it on one day. A
	// browser extension is one visitor; a report is the browser's word and its visitor id too, an address is not, and
	// one /48 is thousands of /64 keys.
	minNets = 3

	// A day's bounds in memory: hosts, addresses per host, the hosts one address may bring in (a page loads at most
	// maxScripts; one address reporting made-up names fills no more), the left-out hosts counted apart.
	dayHosts        = 5_000
	hostAddrs       = 1_000
	dayHostsPerAddr = 50
	dayCutKeys      = 100_000
	// maxScripts and maxHostLen bound a page's report (the script's own bounds); a longer name is cut to its last
	// labels, as the script does.
	maxScripts = 20
	maxHostLen = 100
)

// Options shape Run.
type Options struct {
	// Every is the pause between two wakes; five minutes by default. Run also wakes once at start.
	Every time.Duration
	Now   func() time.Time
	Log   *slog.Logger
}

// Run walks every site's days until ctx ends: the backfill of every day with raw after an upgrade, oldest first,
// then each day as it closes, and the open days again on every wake.
func Run(ctx context.Context, st *store.Store, o Options) {
	if o.Every <= 0 {
		o.Every = 5 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	j := &job{db: st, yield: yield, log: o.Log}
	for wait := time.Duration(0); run.Sleep(ctx, wait); wait = o.Every {
		start := time.Now()
		n, err := j.pass(ctx, o.Now())
		switch {
		case err != nil && ctx.Err() == nil:
			o.Log.Warn("Watch failed, retried at the next wake", "merged", n, logging.Err(err))
		case n > 0:
			o.Log.Info("Watch merged", "site_days", n, "took", time.Since(start).Round(time.Millisecond).String())
		}
	}
}

// Pass runs one pass at now, without pauses: the simulator's and the tests' (nothing else writes meanwhile).
func Pass(ctx context.Context, st *store.Store, now time.Time) error {
	j := &job{db: st, log: slog.New(slog.DiscardHandler)}
	_, err := j.pass(ctx, now)
	return err
}

type job struct {
	db    *store.Store
	yield time.Duration
	log   *slog.Logger
}

// lastClosed is the newest day closed at now: two hours past its end.
func lastClosed(now time.Time) int64 { return (now.UnixMilli()-freezeLag.Milliseconds())/dayMS - 1 }

// pass walks every active site, in id order, and returns the site-days it merged. A site that fails leaves the others
// to go on, its error joined to the result: one broken site never stops the rest on every wake.
func (j *job) pass(ctx context.Context, now time.Time) (int, error) {
	sites, err := j.db.Sites(ctx)
	if err != nil {
		return 0, err
	}
	list := slices.SortedFunc(maps.Values(sites), func(a, b store.Site) int { return cmp.Compare(a.ID, b.ID) })
	merged := 0
	var errs []error
	for _, s := range list {
		if !s.Active {
			continue
		}
		n, err := j.site(ctx, &siteRef{id: s.ID, domain: s.Domain}, now)
		merged += n
		if ctx.Err() != nil {
			return merged, ctx.Err()
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("site %d: %w", s.ID, err))
		}
	}
	// Run logs them, each site's.
	return merged, errors.Join(errs...)
}

// site merges a site's closed days not merged yet, oldest first, then judges its open days.
func (j *job) site(ctx context.Context, site *siteRef, now time.Time) (int, error) {
	db := j.db.DB
	last, today := lastClosed(now), now.UnixMilli()/dayMS
	oldest, ok, err := j.db.OldestEventDay(ctx, site.id)
	if err != nil {
		return 0, err
	}
	if !ok {
		// No raw: nothing to walk, caught up; no open day holds a finding or a host. The walk starts at the first day
		// not closed, moved to the oldest raw day once there is some (load).
		if _, err := db.ExecContext(ctx, `INSERT INTO watch_site (site_id, from_day, done_day) VALUES (?, ?, ?)
			ON CONFLICT (site_id) DO NOTHING`, site.id, last+1, last); err != nil {
			return 0, err
		}
		if _, err := db.ExecContext(ctx, `UPDATE watch_site SET ready_at = ? WHERE site_id = ? AND ready_at = 0`,
			now.UnixMilli(), site.id); err != nil {
			return 0, err
		}
		return 0, writeOpen(ctx, db, site.id, nil)
	}
	st, err := load(ctx, db, site.id, oldest)
	if err != nil {
		return 0, err
	}
	merged := 0
	logged := time.Now()
	// Days whose raw retention took before the walk reached them have nothing to merge.
	for d := max(st.site.DoneDay+1, oldest); d <= last; d++ {
		dd, err := scan(ctx, db, site, d, d*dayMS, (d+1)*dayMS)
		if err != nil {
			return merged, err
		}
		o := st.apply(dd, true, now.UnixMilli())
		if d == last && o.site.ReadyAt == 0 {
			o.site.ReadyAt = now.UnixMilli()
		}
		if err := write(ctx, db, site.id, o); err != nil {
			return merged, err
		}
		st.commit(o)
		merged++
		if time.Since(logged) >= 30*time.Second {
			logged = time.Now()
			j.log.Info("Watch walking", "site", site.id, "day", time.UnixMilli(d*dayMS).UTC().Format(time.DateOnly),
				"to", time.UnixMilli(last*dayMS).UTC().Format(time.DateOnly))
		}
		if j.yield > 0 && !run.Sleep(ctx, j.yield) {
			return merged, ctx.Err()
		}
	}
	if st.site.ReadyAt == 0 {
		// Nothing closed to walk (a site's first days): caught up.
		st.site.ReadyAt = now.UnixMilli()
		if _, err := db.ExecContext(ctx, `UPDATE watch_site SET ready_at = ? WHERE site_id = ?`, st.site.ReadyAt,
			site.id); err != nil {
			return merged, err
		}
	}
	// The open days, judged whole against the state and the open days before them: findings for now, and the hosts
	// they reported as the state merged with them holds them.
	ov := st.clone()
	var found []Open
	seen := map[string]bool{}
	for d := last + 1; d <= today; d++ {
		dd, err := scan(ctx, db, site, d, d*dayMS, min((d+1)*dayMS, now.UnixMilli()+1))
		if err != nil {
			return merged, err
		}
		o := ov.apply(dd, false, now.UnixMilli())
		found = append(found, o.found...)
		ov.commit(o)
		for h := range o.hosts {
			seen[h] = true
		}
	}
	return merged, writeOpen(ctx, db, site.id, append(found, openHosts(ov, seen)...))
}

// openHosts are the open days' hosts as the overlay holds them, the most addresses first, at most maxHosts: the
// script hosts card's rows for today before the day closes.
func openHosts(ov *state, seen map[string]bool) []Open {
	hs := make([]*Host, 0, len(seen))
	for name := range seen {
		hs = append(hs, ov.hosts[name])
	}
	slices.SortFunc(hs, func(a, b *Host) int {
		return cmp.Or(cmp.Compare(b.Addrs, a.Addrs), cmp.Compare(b.LastSeen, a.LastSeen), cmp.Compare(a.Host, b.Host))
	})
	out := make([]Open, 0, min(len(hs), maxHosts))
	for _, h := range hs[:min(len(hs), maxHosts)] {
		out = append(out, Open{Kind: "host", Key: h.Host, FirstSeen: h.FirstSeen, Detail: jsonOf(h)})
	}
	return out
}

// load reads a site's state, its row made on first use: the walk starts at the site's oldest raw day.
func load(ctx context.Context, db *sql.DB, site, oldest int64) (*state, error) {
	st := &state{hosts: map[string]*Host{}}
	s, ok, err := SiteState(ctx, db, site)
	switch {
	case err == nil && !ok:
		s = Site{SiteID: site, FromDay: oldest, DoneDay: oldest - 1}
		_, err = db.ExecContext(ctx, `INSERT INTO watch_site (site_id, from_day, done_day) VALUES (?, ?, ?)
			ON CONFLICT (site_id) DO NOTHING`, site, s.FromDay, s.DoneDay)
	case err == nil && s.DoneDay < s.FromDay && oldest != s.FromDay:
		// Made while the site had no raw, nothing merged since: the row starts as if made now, from its oldest raw day.
		// Raw older than the row (a history imported after the first wake) would never be walked.
		s = Site{SiteID: site, FromDay: oldest, DoneDay: oldest - 1}
		_, err = db.ExecContext(ctx, `UPDATE watch_site SET from_day = ?, done_day = ?, ready_at = 0 WHERE site_id = ?`,
			s.FromDay, s.DoneDay, site)
	}
	if err != nil {
		return nil, err
	}
	st.site = s
	hosts, err := readHosts(ctx, db, siteHosts, site)
	if err != nil {
		return nil, err
	}
	for i := range hosts {
		st.hosts[hosts[i].Host] = &hosts[i]
	}
	return st, nil
}

// The state's reads: a site's every host (at most maxHosts), its findings.
const (
	hostCols   = `SELECT host, first_seen, last_seen, addrs, nets, found_at, found_first FROM script_host`
	siteHosts  = hostCols + ` WHERE site_id = ?`
	foundHosts = hostCols + ` WHERE site_id = ? AND found_at >= ?`
)

func readHosts(ctx context.Context, db *sql.DB, q string, args ...any) ([]Host, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Host
	for rows.Next() {
		var h Host
		if err := rows.Scan(&h.Host, &h.FirstSeen, &h.LastSeen, &h.Addrs, &h.Nets, &h.FoundAt,
			&h.FoundFirst); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// write stores a closed day's outcome and the site's cursor in one transaction: a day is merged whole or not at all,
// and walked again only if not.
func write(ctx context.Context, db *sql.DB, site int64, o outcome) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	for _, k := range o.dropHosts {
		if _, err = tx.ExecContext(ctx, `DELETE FROM script_host WHERE site_id = ? AND host = ?`, site, k); err != nil {
			return err
		}
	}
	for _, h := range o.hosts {
		if _, err = tx.ExecContext(ctx, `INSERT INTO script_host (site_id, host, first_seen, last_seen, addrs, nets,
			found_at, found_first) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (site_id, host) DO UPDATE SET
			first_seen = excluded.first_seen, last_seen = excluded.last_seen, addrs = excluded.addrs,
			nets = excluded.nets, found_at = excluded.found_at, found_first = excluded.found_first`, site, h.Host,
			h.FirstSeen, h.LastSeen, h.Addrs, h.Nets, h.FoundAt, h.FoundFirst); err != nil {
			return err
		}
	}
	s := o.site
	if _, err = tx.ExecContext(ctx, `UPDATE watch_site SET done_day = ?, ready_at = ?, scripts_from = ?, cut = ?
		WHERE site_id = ?`, s.DoneDay, s.ReadyAt, s.ScriptsFrom, s.Cut, site); err != nil {
		return err
	}
	return tx.Commit()
}

// writeOpen replaces a site's provisional findings.
func writeOpen(ctx context.Context, db *sql.DB, site int64, found []Open) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, `DELETE FROM watch_open WHERE site_id = ?`, site); err != nil {
		return err
	}
	for _, f := range found {
		if _, err = tx.ExecContext(ctx, `INSERT INTO watch_open (site_id, kind, key, first_seen, detail)
			VALUES (?, ?, ?, ?, ?) ON CONFLICT (site_id, kind, key) DO UPDATE SET first_seen = excluded.first_seen,
			detail = excluded.detail`, site, f.Kind, f.Key, f.FirstSeen, f.Detail); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
