package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/trick77/ship/kit/run"
)

// The states of an alert row.
const (
	AlertBaseline = "baseline"
	AlertPending  = "pending"
	AlertOpen     = "open"
	AlertCleared  = "cleared"
	AlertOnce     = "once"
)

// alertCols are an alert row's columns in scanAlert's order.
const alertCols = `id, site_id, rule, kind, key, state, severity, figure, unit, text, detail, panel, filter, first_at,
	last_at, left_at, told, due, due_at, notified_at, attempts, channels, delivered, failed`

func scanAlert(rows *sql.Rows) (Alert, error) {
	var a Alert
	var filter, channels, delivered, failed string
	if err := rows.Scan(&a.ID, &a.SiteID, &a.Rule, &a.Kind, &a.Key, &a.State, &a.Severity, &a.Figure, &a.Unit, &a.Text,
		&a.Detail, &a.Panel, &filter, &a.FirstAt, &a.LastAt, &a.LeftAt, &a.Told, &a.Due, &a.DueAt, &a.NotifiedAt,
		&a.Attempts, &channels, &delivered, &failed); err != nil {
		return a, err
	}
	err := errors.Join(json.Unmarshal([]byte(filter), &a.Filter), json.Unmarshal([]byte(channels), &a.Channels),
		json.Unmarshal([]byte(delivered), &a.Delivered), json.Unmarshal([]byte(failed), &a.Failed))
	return a, err
}

// The alert table's reads, each on an index (TestAlertPlans).
const (
	selectAlert = `SELECT ` + alertCols + ` FROM alert `
	qInSet      = selectAlert + `WHERE rule = ? AND site_id = ? AND left_at IS NULL`
	qLastAlert  = selectAlert + `WHERE rule = ? AND site_id = ? AND key = ? ORDER BY id DESC LIMIT 1`
	qDue        = selectAlert + `WHERE due != '' AND (due_at, id) > (?, ?) ORDER BY due_at, id LIMIT ?`
	shownAlert  = `state NOT IN ('` + AlertBaseline + `', '` + AlertPending + `')`
	qPageSite   = selectAlert + `WHERE site_id = ? AND id < ? AND ` + shownAlert + ` ORDER BY id DESC LIMIT ?`
	qPageAll    = selectAlert + `WHERE id < ? AND ` + shownAlert + ` ORDER BY id DESC LIMIT ?`
	qPrune      = `DELETE FROM alert WHERE id IN (SELECT id FROM alert WHERE last_at < ? AND left_at IS NOT NULL LIMIT ?)`
)

func (s *Store) alerts(ctx context.Context, q string, args ...any) ([]Alert, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// jsonOf is v as JSON, its nil slice or map as an empty one.
func jsonOf[T any](v T, empty string) string {
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return empty
	}
	return string(b)
}

// alertArgs are a's columns after id, in alertCols' order.
func alertArgs(a *Alert) []any {
	return []any{a.SiteID, a.Rule, a.Kind, a.Key, a.State, a.Severity, a.Figure, a.Unit, a.Text, a.Detail, a.Panel,
		jsonOf(a.Filter, "{}"), a.FirstAt, a.LastAt, a.LeftAt, a.Told, a.Due, a.DueAt, a.NotifiedAt, a.Attempts,
		jsonOf(a.Channels, "[]"), jsonOf(a.Delivered, "[]"), jsonOf(a.Failed, "[]")}
}

// InsertAlert inserts a and sets its id.
func (s *Store) InsertAlert(ctx context.Context, a *Alert) error {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO alert (site_id, rule, kind, key, state, severity, figure, unit, text,
		detail, panel, filter, first_at, last_at, left_at, told, due, due_at, notified_at, attempts, channels, delivered,
		failed) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, alertArgs(a)...)
	if err != nil {
		return err
	}
	a.ID, err = res.LastInsertId()
	return err
}

// UpdateAlert writes a's columns to its row.
func (s *Store) UpdateAlert(ctx context.Context, a *Alert) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE alert SET site_id = ?, rule = ?, kind = ?, key = ?, state = ?, severity = ?,
		figure = ?, unit = ?, text = ?, detail = ?, panel = ?, filter = ?, first_at = ?, last_at = ?, left_at = ?, told = ?,
		due = ?, due_at = ?, notified_at = ?, attempts = ?, channels = ?, delivered = ?, failed = ? WHERE id = ?`,
		append(alertArgs(a), a.ID)...)
	return err
}

// DeleteAlert deletes a row: a pending condition that cleared before it was ever reported.
func (s *Store) DeleteAlert(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM alert WHERE id = ?`, id)
	return err
}

// InSet is a rule's rows on a site still in its set (left_at NULL).
func (s *Store) InSet(ctx context.Context, rule string, site int64) ([]Alert, error) {
	return s.alerts(ctx, qInSet, rule, site)
}

// LastAlert is a rule's latest row for a key on a site, ok false without one.
func (s *Store) LastAlert(ctx context.Context, rule string, site int64, key string) (Alert, bool, error) {
	as, err := s.alerts(ctx, qLastAlert, rule, site, key)
	if err != nil || len(as) == 0 {
		return Alert{}, false, err
	}
	return as[0], true, nil
}

// DueAlerts are the rows with a message waiting, oldest first, after the row (dueAt, id) (0, 0: from the oldest), at
// most limit: a page of the outbox.
func (s *Store) DueAlerts(ctx context.Context, dueAt, id int64, limit int) ([]Alert, error) {
	return s.alerts(ctx, qDue, dueAt, id, limit)
}

// AlertPage is a page of the history, newest first: every site's and the instance's (site nil) or one site's, the
// rows before the id before (0: from the newest), at most limit. Baseline and pending rows are no alerts.
func (s *Store) AlertPage(ctx context.Context, site *int64, before int64, limit int) ([]Alert, error) {
	if before <= 0 {
		before = 1 << 62
	}
	if site != nil {
		return s.alerts(ctx, qPageSite, *site, before, limit)
	}
	return s.alerts(ctx, qPageAll, before, limit)
}

// AlertRuleOf is a rule's last pass on a site, ok false before its first.
func (s *Store) AlertRuleOf(ctx context.Context, rule string, site int64) (AlertRule, bool, error) {
	r := AlertRule{Rule: rule, SiteID: site}
	err := s.DB.QueryRowContext(ctx, `SELECT sig, checked_at FROM alert_rule WHERE rule = ? AND site_id = ?`, rule, site).
		Scan(&r.Sig, &r.CheckedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// AlertRules are every rule's last pass on every site: what may still have rows in its set.
func (s *Store) AlertRules(ctx context.Context) ([]AlertRule, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT rule, site_id, sig, checked_at FROM alert_rule`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AlertRule
	for rows.Next() {
		var r AlertRule
		if err := rows.Scan(&r.Rule, &r.SiteID, &r.Sig, &r.CheckedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetAlertRule records a rule's pass.
func (s *Store) SetAlertRule(ctx context.Context, r AlertRule) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO alert_rule (rule, site_id, sig, checked_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (rule, site_id) DO UPDATE SET sig = excluded.sig, checked_at = excluded.checked_at`,
		r.Rule, r.SiteID, r.Sig, r.CheckedAt)
	return err
}

// AlertChannels are the channels' last deliveries and failures, by name.
func (s *Store) AlertChannels(ctx context.Context) (map[string]AlertChannel, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT name, last_ok, error_at, error FROM alert_channel`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]AlertChannel{}
	for rows.Next() {
		var c AlertChannel
		if err := rows.Scan(&c.Name, &c.LastOK, &c.ErrorAt, &c.Error); err != nil {
			return nil, err
		}
		out[c.Name] = c
	}
	return out, rows.Err()
}

// ChannelDelivered records a channel's delivery at, or its failure with text (scrubbed by the caller).
func (s *Store) ChannelDelivered(ctx context.Context, name string, at time.Time, failure string) error {
	if failure == "" {
		_, err := s.DB.ExecContext(ctx, `INSERT INTO alert_channel (name, last_ok) VALUES (?, ?)
			ON CONFLICT (name) DO UPDATE SET last_ok = excluded.last_ok`, name, at.UnixMilli())
		return err
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO alert_channel (name, error_at, error) VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET error_at = excluded.error_at, error = excluded.error`, name, at.UnixMilli(), failure)
	return err
}

// AlertHistoryDays is how long an alert that left its set is kept.
const AlertHistoryDays = 90

// PruneAlerts deletes the rows that left their set before before, a chunk at a time; rows still in their set (an
// open condition, a baseline) stay whatever their age.
func (s *Store) PruneAlerts(ctx context.Context, before time.Time) (int64, error) {
	var total int64
	for {
		n, err := s.execRows(ctx, qPrune, before.UnixMilli(), retentionChunk)
		total += n
		if err != nil || n < retentionChunk {
			return total, err
		}
		if !run.Sleep(ctx, retentionYield) {
			return total, ctx.Err()
		}
	}
}
