package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SyncSites makes the site table match the configured sites: new domains are inserted, known ones updated and
// reactivated, the rest deactivated (their events stay). It returns every site, active or not, keyed by domain.
func (s *Store) SyncSites(ctx context.Context, sites []Site) (map[string]Site, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE site SET active = 0`); err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	for _, st := range sites {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO site (domain, kind, product, feed_key_hash, path_rules, active, created_at)
			VALUES (?, ?, ?, ?, ?, 1, ?)
			ON CONFLICT (domain) DO UPDATE SET kind = excluded.kind, product = excluded.product,
				feed_key_hash = excluded.feed_key_hash, path_rules = excluded.path_rules, active = 1`,
			st.Domain, st.Kind, st.Product, st.FeedKeyHash, st.PathRules, now); err != nil {
			return nil, fmt.Errorf("sync site %s: %w", st.Domain, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Sites(ctx)
}

// Sites returns every site keyed by domain.
func (s *Store) Sites(ctx context.Context) (map[string]Site, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, domain, kind, product, feed_key_hash, path_rules, active FROM site ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]Site{}
	for rows.Next() {
		var st Site
		if err := rows.Scan(&st.ID, &st.Domain, &st.Kind, &st.Product, &st.FeedKeyHash, &st.PathRules,
			&st.Active); err != nil {
			return nil, err
		}
		out[st.Domain] = st
	}
	return out, rows.Err()
}

const eventColumns = `site_id, ts, source, kind, name, visitor, sid, ip, ip_key, ua, path, template, url, ref,
	ref_host, method, status, dur_ms, asn, country, account, op, err, unrouted, cls, props`

// InsertEvents writes one batch in one transaction: the batch writer's only statement.
func (s *Store) InsertEvents(ctx context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO event (`+eventColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	days := map[[2]int64]bool{}
	for _, e := range events {
		props := e.Props
		if props == "" {
			props = "{}"
		}
		if _, err := stmt.ExecContext(ctx, e.SiteID, e.TS, e.Source, e.Kind, e.Name, e.Visitor, e.SID, e.IP,
			e.IPKey, e.UA, e.Path, e.Template, e.URL, e.Ref, e.RefHost, e.Method, e.Status, e.DurMS, e.ASN,
			e.Country, e.Account, e.Op, e.Err, e.Unrouted, e.Cls, props); err != nil {
			return err
		}
		days[[2]int64{e.SiteID, e.TS / dayMS}] = true
	}
	for d := range days {
		if err := markAggDirty(ctx, tx, d[0], d[1]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// markAggDirty tells the aggregate job that a site-day's raw changed, in the transaction that changed it: a built
// day goes dirty (read raw until rebuilt) and a build under way finds its version moved. A day mid-write keeps that
// mark: its rows may be partial. The collector only writes today and yesterday, which have no row yet: a probe on
// the primary key that finds nothing.
func markAggDirty(ctx context.Context, tx *sql.Tx, siteID, day int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE agg_day SET version = version + 1,
		dirty = CASE dirty WHEN 3 THEN 3 ELSE 1 END WHERE site_id = ? AND day = ? AND raw = 1`, siteID, day)
	return err
}

// AddStats adds counters to their hour rows.
func (s *Store) AddStats(ctx context.Context, stats []IngestStat) error {
	if len(stats) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// The counters are deltas (a failed write takes accepted events back): applied to the hour's row, never below
	// zero, and the server's share never above the whole.
	for _, st := range stats {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ingest_stat (site_id, hour) VALUES (?, ?)
			ON CONFLICT (site_id, hour) DO NOTHING`, st.SiteID, st.Hour); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ingest_stat SET accepted = MAX(0, accepted + ?1),
				accepted_server = MAX(0, MIN(accepted + ?1, accepted_server + ?2)),
				rate_limited = MAX(0, rate_limited + ?3), rejected = MAX(0, rejected + ?4),
				dropped = MAX(0, dropped + ?5), unknown_domain = MAX(0, unknown_domain + ?6)
			WHERE site_id = ?7 AND hour = ?8`,
			st.Accepted, st.AcceptedServer, st.RateLimited, st.Rejected, st.Dropped, st.UnknownDomain,
			st.SiteID, st.Hour); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ActorClasses returns the stored classes of a site's actors on day, keyed by address key.
func (s *Store) ActorClasses(ctx context.Context, siteID, day int64) (map[string]ActorClass, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT site_id, ip_key, day, class, reasons FROM actor_class WHERE site_id = ? AND day = ?`, siteID, day)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]ActorClass{}
	for rows.Next() {
		var a ActorClass
		var reasons string
		if err := rows.Scan(&a.SiteID, &a.IPKey, &a.Day, &a.Class, &reasons); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(reasons), &a.Reasons)
		out[a.IPKey] = a
	}
	return out, rows.Err()
}

// SetActorClass stores an actor-day's class and stamps it on that actor-day's events, in one short transaction.
// byUA are the user agents whose events get a class of their own (a curl request behind an address of people).
func (s *Store) SetActorClass(ctx context.Context, a ActorClass, byUA map[string]string) error {
	reasons, err := json.Marshal(nonNil(a.Reasons))
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO actor_class (site_id, ip_key, day, class, reasons, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (site_id, ip_key, day) DO UPDATE SET class = excluded.class, reasons = excluded.reasons,
			updated_at = excluded.updated_at`,
		a.SiteID, a.IPKey, a.Day, a.Class, string(reasons), time.Now().UnixMilli()); err != nil {
		return err
	}
	from := a.Day * dayMS
	// The actor's class skips the clients with one of their own: stamped and stamped back, they would count as a
	// change on every pass.
	rest, keep := ClassCode(cmp.Or(a.Rest, a.Class)), -1
	if a.Rest != "" {
		keep = ClsAPI
	}
	args := []any{rest, a.SiteID, a.IPKey, from, from + dayMS, rest, keep, a.Rest == "human", ClsUnknown}
	for ua := range byUA {
		args = append(args, ua)
	}
	//nolint:gosec // only placeholders are spliced in; values are bound
	res, err := tx.ExecContext(ctx, `UPDATE event SET cls = ? WHERE site_id = ? AND ip_key = ? AND ts >= ? AND ts < ?
		AND cls NOT IN (?, ?) AND NOT (? AND cls = ? AND ua NOT LIKE 'Mozilla/%') AND ua NOT IN (`+placeholders(len(byUA))+`)`, args...)
	if err != nil {
		return err
	}
	changed, _ := res.RowsAffected()
	for ua, class := range byUA {
		res, err := tx.ExecContext(ctx, `UPDATE event SET cls = ? WHERE site_id = ? AND ip_key = ? AND ts >= ?
			AND ts < ? AND ua = ? AND cls != ?`, ClassCode(class), a.SiteID, a.IPKey, from, from+dayMS, ua,
			ClassCode(class))
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		changed += n
	}
	// Most passes restamp nothing: the day's aggregates stand then.
	if changed > 0 {
		if err := markAggDirty(ctx, tx, a.SiteID, a.Day); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const dayMS = int64(24 * time.Hour / time.Millisecond)

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Briefing returns the cached briefing for a site and range, ok false when there is none.
func (s *Store) Briefing(ctx context.Context, siteID int64, rng string) (Briefing, bool, error) {
	var b Briefing
	err := s.DB.QueryRowContext(ctx, `SELECT site_id, range, facts_hash, as_of, body, created_at FROM llm_briefing
		WHERE site_id = ? AND range = ?`, siteID, rng).
		Scan(&b.SiteID, &b.Range, &b.FactsHash, &b.AsOf, &b.Body, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Briefing{}, false, nil
	}
	return b, err == nil, err
}

// PutBriefing replaces a site and range's briefing.
func (s *Store) PutBriefing(ctx context.Context, b Briefing) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO llm_briefing (site_id, range, facts_hash, as_of, body, created_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (site_id, range) DO UPDATE SET facts_hash = excluded.facts_hash,
		as_of = excluded.as_of, body = excluded.body, created_at = excluded.created_at`,
		b.SiteID, b.Range, b.FactsHash, b.AsOf, b.Body, b.CreatedAt)
	return err
}

// Readings returns the cached readings of the given signatures, keyed by signature.
func (s *Store) Readings(ctx context.Context, sigs []string) (map[string]Reading, error) {
	out := map[string]Reading{}
	if len(sigs) == 0 {
		return out, nil
	}
	args := make([]any, len(sigs))
	for i, sg := range sigs {
		args[i] = sg
	}
	//nolint:gosec // only placeholders are spliced in; values are bound
	rows, err := s.DB.QueryContext(ctx, `SELECT signature, body, created_at FROM llm_reading
		WHERE signature IN (`+placeholders(len(sigs))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r Reading
		if err := rows.Scan(&r.Signature, &r.Body, &r.CreatedAt); err != nil {
			return nil, err
		}
		out[r.Signature] = r
	}
	return out, rows.Err()
}

// PutReading stores a signature's reading.
func (s *Store) PutReading(ctx context.Context, r Reading) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO llm_reading (signature, body, created_at)
		VALUES (?, ?, ?) ON CONFLICT (signature) DO UPDATE SET body = excluded.body, created_at = excluded.created_at`,
		r.Signature, r.Body, r.CreatedAt)
	return err
}

// Labels returns the stored labels of one kind for values, keyed by value.
func (s *Store) Labels(ctx context.Context, kind string, values []string) (map[string]string, error) {
	out := map[string]string{}
	if len(values) == 0 {
		return out, nil
	}
	args := []any{kind}
	for _, v := range values {
		args = append(args, v)
	}
	//nolint:gosec // only placeholders are spliced in; values are bound
	rows, err := s.DB.QueryContext(ctx, `SELECT value, label FROM llm_label WHERE kind = ? AND value IN (`+
		placeholders(len(values))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var v, l string
		if err := rows.Scan(&v, &l); err != nil {
			return nil, err
		}
		out[v] = l
	}
	return out, rows.Err()
}

// PutLabel stores a value's label for good.
func (s *Store) PutLabel(ctx context.Context, kind, value, label string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO llm_label (kind, value, label, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (kind, value) DO UPDATE SET label = excluded.label`, kind, value, label, time.Now().UnixMilli())
	return err
}

// UsageOn returns a site's model usage on a unix day.
func (s *Store) UsageOn(ctx context.Context, siteID, day int64) (Usage, error) {
	u := Usage{SiteID: siteID, Day: day}
	err := s.DB.QueryRowContext(ctx, `SELECT calls, input_tokens, output_tokens FROM llm_usage
		WHERE site_id = ? AND day = ?`, siteID, day).Scan(&u.Calls, &u.InputTokens, &u.OutputTokens)
	if errors.Is(err, sql.ErrNoRows) {
		return u, nil
	}
	return u, err
}

// AddUsage books one call's tokens.
func (s *Store) AddUsage(ctx context.Context, u Usage) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO llm_usage (site_id, day, calls, input_tokens, output_tokens)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (site_id, day) DO UPDATE SET calls = calls + excluded.calls,
		input_tokens = input_tokens + excluded.input_tokens, output_tokens = output_tokens + excluded.output_tokens`,
		u.SiteID, u.Day, u.Calls, u.InputTokens, u.OutputTokens)
	return err
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
