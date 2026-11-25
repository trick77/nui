package watch

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
)

// Finding is one new script host as the dashboard reads it.
type Finding struct {
	Key string
	// At is when it happened: the host's first sight on the day it was found, the same before and after that day
	// closes. Unix ms.
	At int64
	// Provisional: a day not closed yet made it; a later pass may drop it.
	Provisional bool
	Host        *Host
}

// Findings are a site's findings that happened from from on (unix ms), the provisional ones of the open days
// included, newest first. A host has one finding: between a closed day's merge and the pass's new provisional ones
// both are stored a moment, the closed one counts.
func Findings(ctx context.Context, db *sql.DB, site, from int64) ([]Finding, error) {
	return findings(ctx, db, site, from, false)
}

// Made are a site's findings the job made from from on (unix ms), newest first: the closed ones by their found_at,
// the provisional ones all (every pass makes them anew). The alert rules': a finding is made after it happened (a
// third network late that day, a place under a day's bound freed on a later one), so a window by when it happened
// may never hold it.
func Made(ctx context.Context, db *sql.DB, site, from int64) ([]Finding, error) {
	return findings(ctx, db, site, from, true)
}

func findings(ctx context.Context, db *sql.DB, site, from int64, made bool) ([]Finding, error) {
	hosts, err := readHosts(ctx, db, foundHosts, site, 1)
	if err != nil {
		return nil, err
	}
	var out []Finding
	// closed are the hosts with a closed finding, whatever its time.
	closed := map[string]bool{}
	for i := range hosts {
		h := &hosts[i]
		closed[h.Host] = true
		when := h.FoundFirst
		if made {
			when = h.FoundAt
		}
		if when >= from {
			out = append(out, Finding{Key: h.Host, At: h.FoundFirst, Host: h})
		}
	}
	open, err := readOpen(ctx, db, site)
	if err != nil {
		return nil, err
	}
	for _, o := range open {
		if o.Kind != "script" || closed[o.Key] {
			continue
		}
		f := Finding{Key: o.Key, At: o.FirstSeen, Provisional: true, Host: &Host{}}
		if err := json.Unmarshal([]byte(o.Detail), f.Host); err != nil {
			return nil, err
		}
		if made || f.At >= from {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b Finding) int {
		return cmp.Or(cmp.Compare(b.At, a.At), cmp.Compare(a.Key, b.Key))
	})
	return out, nil
}

func readOpen(ctx context.Context, db *sql.DB, site int64) ([]Open, error) {
	rows, err := db.QueryContext(ctx, `SELECT kind, key, first_seen, detail FROM watch_open WHERE site_id = ?`, site)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Open
	for rows.Next() {
		var o Open
		if err := rows.Scan(&o.Kind, &o.Key, &o.FirstSeen, &o.Detail); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Hosts are a site's script hosts seen from from on (unix ms), the latest seen first: the closed days' state, a host
// the open days reported as the state merged with them holds it (its last sight today, its most addresses).
func Hosts(ctx context.Context, db *sql.DB, site, from int64) ([]Host, error) {
	closed, err := readHosts(ctx, db, siteHosts, site)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]Host, len(closed))
	for _, h := range closed {
		byName[h.Host] = h
	}
	open, err := readOpen(ctx, db, site)
	if err != nil {
		return nil, err
	}
	for _, o := range open {
		if o.Kind != "host" {
			continue
		}
		var h Host
		if err := json.Unmarshal([]byte(o.Detail), &h); err != nil {
			return nil, err
		}
		// A pass merged since: the closed state is as new.
		if c, ok := byName[h.Host]; !ok || c.LastSeen < h.LastSeen {
			byName[h.Host] = mergeHost(h, c)
		}
	}
	hosts := []Host{}
	for _, h := range byName {
		if h.LastSeen >= from {
			hosts = append(hosts, h)
		}
	}
	slices.SortFunc(hosts, func(a, b Host) int {
		return cmp.Or(cmp.Compare(b.LastSeen, a.LastSeen), cmp.Compare(a.Host, b.Host))
	})
	return hosts, nil
}

// SiteState is a site's walk state, ok false before the job first walked it.
func SiteState(ctx context.Context, db *sql.DB, site int64) (Site, bool, error) {
	var s Site
	err := db.QueryRowContext(ctx, `SELECT site_id, from_day, done_day, ready_at, scripts_from, cut FROM watch_site
		WHERE site_id = ?`, site).Scan(&s.SiteID, &s.FromDay, &s.DoneDay, &s.ReadyAt, &s.ScriptsFrom, &s.Cut)
	if errors.Is(err, sql.ErrNoRows) {
		return s, false, nil
	}
	return s, err == nil, err
}
