package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/ship/kit/run"
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
			INSERT INTO site (domain, name, kind, product, sort_order, feed_key_hash, path_rules, active, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?)
			ON CONFLICT (domain) DO UPDATE SET name = excluded.name, kind = excluded.kind,
				product = excluded.product, sort_order = excluded.sort_order,
				feed_key_hash = excluded.feed_key_hash, path_rules = excluded.path_rules, active = 1`,
			st.Domain, st.Name, st.Kind, st.Product, st.SortOrder, st.FeedKeyHash, st.PathRules, now); err != nil {
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
		`SELECT id, domain, name, kind, product, sort_order, feed_key_hash, path_rules, active FROM site ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]Site{}
	for rows.Next() {
		var st Site
		if err := rows.Scan(&st.ID, &st.Domain, &st.Name, &st.Kind, &st.Product, &st.SortOrder, &st.FeedKeyHash,
			&st.PathRules, &st.Active); err != nil {
			return nil, err
		}
		out[st.Domain] = st
	}
	return out, rows.Err()
}

const eventColumns = `site_id, ts, source, kind, name, visitor, sid, ip, ip_key, ua, path, template, url, ref,
	ref_host, method, status, dur_ms, asn, country, account, op, err, unrouted, cls, svc, props`

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
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
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
			e.Country, e.Account, e.Op, e.Err, e.Unrouted, e.Cls, e.Svc, props); err != nil {
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

// SetActorClass stores an actor-day's class and stamps it on that actor-day's events, their names after it (a.Name,
// nameActor), in one short transaction. byUA are the user agents whose events get a class of their own (a curl request
// behind an address of people).
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
	if a.Name != nil {
		n, err := nameActor(ctx, tx, a)
		if err != nil {
			return err
		}
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

// svcFor is the name a group's events take: name's for a bot's or a probe's (ClsBot, ClsMonitor), none for any other
// class.
func svcFor(name SvcNamer, g SvcGroup) string {
	if g.Cls != ClsBot && g.Cls != ClsMonitor {
		return ""
	}
	return name(g.IP, g.UA, g.Cls)
}

// svcGroups reads a site-day's events that hold a name or may take one (a bot's, a probe's), grouped, the one actor's
// of ipKey when given: one statement over the day, read whole before any write.
func svcGroups(ctx context.Context, q Queryer, siteID, day int64, ipKey ...string) ([]SvcGroup, error) {
	actor, args := "", []any{siteID, day * dayMS, (day + 1) * dayMS}
	if len(ipKey) > 0 {
		actor, args = "AND ip_key = ?", append(args, ipKey[0])
	}
	rows, err := q.QueryContext(ctx, `SELECT ip_key, ip, ua, cls, svc FROM event WHERE site_id = ? AND ts >= ? AND ts < ?
		`+actor+` AND (cls IN (2, 3) OR svc != '') GROUP BY ip_key, ip, ua, cls, svc`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SvcGroup
	for rows.Next() {
		var g SvcGroup
		if err := rows.Scan(&g.IPKey, &g.IP, &g.UA, &g.Cls, &g.Svc); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// svcTuples is the groups one renaming statement names, four values bound each.
const svcTuples = 200

// svcIn is the condition on n groups' events: their address, client, class and name, four values bound each.
func svcIn(n int) string {
	row := "(" + placeholders(4) + ")"
	return "(ip, ua, cls, svc) IN (VALUES " + strings.TrimSuffix(strings.Repeat(row+", ", n), ", ") + ")"
}

// svcRename is groups of one actor-day that take one name, svcTuples at most: one statement's, which reads the
// actor-day once.
type svcRename struct {
	ipKey, to string
	keys      []any // each group's ip, ua, cls, svc
}

// svcRenames are the statements that give groups the names name gives them: an actor's groups taking one name
// together, svcTuples a statement, those named so already left out.
func svcRenames(name SvcNamer, groups []SvcGroup) []svcRename {
	type rename struct {
		SvcGroup
		to string
	}
	var rs []rename
	for _, g := range groups {
		if to := svcFor(name, g); to != g.Svc {
			rs = append(rs, rename{g, to})
		}
	}
	slices.SortStableFunc(rs, func(a, b rename) int {
		return cmp.Or(strings.Compare(a.IPKey, b.IPKey), strings.Compare(a.to, b.to))
	})
	var todo []svcRename
	for _, r := range rs {
		k := len(todo) - 1
		if k < 0 || todo[k].ipKey != r.IPKey || todo[k].to != r.to || len(todo[k].keys) == 4*svcTuples {
			todo, k = append(todo, svcRename{ipKey: r.IPKey, to: r.to}), k+1
		}
		todo[k].keys = append(todo[k].keys, r.IP, r.UA, r.Cls, r.Svc)
	}
	return todo
}

// nameActor names an actor-day's events by a.Name once their classes are stamped (SetActorClass), per address, client
// and class: one read of the actor-day, then a statement per name given (svcTuples groups each), on the rows whose
// name differs only. A pass that names nothing new changes nothing.
func nameActor(ctx context.Context, tx *sql.Tx, a ActorClass) (int64, error) {
	groups, err := svcGroups(ctx, tx, a.SiteID, a.Day, a.IPKey)
	if err != nil {
		return 0, err
	}
	from := a.Day * dayMS
	var changed int64
	for _, r := range svcRenames(a.Name, groups) {
		//nolint:gosec // only placeholders are spliced in; values are bound
		res, err := tx.ExecContext(ctx, `UPDATE event SET svc = ? WHERE site_id = ? AND ip_key = ? AND ts >= ?
			AND ts < ? AND `+svcIn(len(r.keys)/4),
			slices.Concat([]any{r.to, a.SiteID, a.IPKey, from, from + dayMS}, r.keys)...)
		if err != nil {
			return 0, err
		}
		k, _ := res.RowsAffected()
		changed += k
	}
	return changed, nil
}

// svcBatch and svcScans bound one of NameSvcDay's transactions: the events it changes and its statements, each a read
// of an actor-day (one whose groups were restamped meanwhile changes none, but reads all the same).
const (
	svcBatch = 500
	svcScans = 50
)

// NameSvcDay gives a site-day's events the names name gives them by their address, client and class, and takes the
// name off any other class's: one read of the day, then a statement per actor and name (svcRenames) in batches,
// svcBatch events and svcScans statements a transaction at most with one dirty mark each, the writer's turn at the
// lock between two. Only events still as read: a pass that restamped one meanwhile has named it by the same rule. It
// returns the events changed: none on a day named already.
func (s *Store) NameSvcDay(ctx context.Context, siteID, day int64, name SvcNamer) (int64, error) {
	groups, err := svcGroups(ctx, s.DB, siteID, day)
	if err != nil {
		return 0, err
	}
	todo := svcRenames(name, groups)
	var total int64
	for i := 0; len(todo) > 0; i++ {
		if i > 0 && !run.Sleep(ctx, retentionYield) {
			return total, ctx.Err()
		}
		n, done, err := s.renameBatch(ctx, siteID, day, todo)
		if err != nil {
			return total, err
		}
		total += n
		todo = todo[done:]
	}
	return total, nil
}

// renameBatch runs the first of todo in one transaction, svcScans statements and svcBatch events at most: it returns
// the events changed and the statements done, none of either when the commit fails. A statement cut short goes on in
// the next: the events it renamed are no longer as read.
func (s *Store) renameBatch(ctx context.Context, siteID, day int64, todo []svcRename) (int64, int, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var changed int64
	done := 0
	for _, r := range todo[:min(len(todo), svcScans)] {
		budget := svcBatch - changed
		//nolint:gosec // only placeholders are spliced in; values are bound
		res, err := tx.ExecContext(ctx, `UPDATE event SET svc = ? WHERE id IN (SELECT id FROM event WHERE site_id = ?
			AND ip_key = ? AND ts >= ? AND ts < ? AND `+svcIn(len(r.keys)/4)+` LIMIT ?)`,
			slices.Concat([]any{r.to, siteID, r.ipKey, day * dayMS, (day + 1) * dayMS}, r.keys, []any{budget})...)
		if err != nil {
			return 0, 0, err
		}
		n, _ := res.RowsAffected()
		changed += n
		if n == budget {
			break
		}
		done++
	}
	if changed > 0 {
		if err := markAggDirty(ctx, tx, siteID, day); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return changed, done, nil
}

// metaSvcNamed is agg_meta's key for when the events written before event.svc were named, unix ms.
const metaSvcNamed = "svc_named"

// SvcNamed reports whether the events written before event.svc are named (MarkSvcNamed).
func (s *Store) SvcNamed(ctx context.Context) (bool, error) {
	var ok bool
	err := s.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM agg_meta WHERE key = ?)`, metaSvcNamed).Scan(&ok)
	return ok, err
}

// MarkSvcNamed records that the events written before event.svc are named: a restart names them no more.
func (s *Store) MarkSvcNamed(ctx context.Context, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO agg_meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO NOTHING`,
		metaSvcNamed, strconv.FormatInt(now.UnixMilli(), 10))
	return err
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
