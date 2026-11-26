package dash

import (
	"cmp"
	"context"
	"database/sql"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// What a client says it is: the Signature-Agent header of Web Bot Auth, the URL of the directory holding the keys
// it signs with. Nothing here checks a signature, so it is shown as a claim only, never a signal, never to the model:
// hostile text, of which only a normalised host leaves sigAgent.

// sigAgentSQL is an event's Signature-Agent as stored: the feed's sig_agent, the browser headers' copy.
const sigAgentSQL = `COALESCE(json_extract(props, '$.sig_agent'), json_extract(props, '$.hdr."signature-agent"'))`

// mayClaimSQL keeps the events whose props can hold a claim, before any JSON is parsed: most hold none.
const mayClaimSQL = `(instr(props, 'sig_agent') > 0 OR instr(props, 'signature-agent') > 0)`

// sayDays are the newest raw days a claim is read from, a statement each.
const sayDays = 7

// maxSigAgent is the longest header read, as the feed stores it (ingest's feedSigAgent, the browser's maxHeader).
const maxSigAgent = 256

// sigHostRE is a host as shown.
var sigHostRE = regexp.MustCompile(`^[a-z0-9.-]{1,80}$`)

// sfKeyRE is a structured field dictionary member's key, its "=" included.
var sfKeyRE = regexp.MustCompile(`^[a-z*][a-z0-9_.*-]*=`)

// sigAgent is the host a Signature-Agent header names, "" when it names none usable. The draft's form is a dictionary
// of strings (sig1="https://example.com"; draft-meunier-http-message-signatures-directory-05), the first member's
// read; earlier drafts sent one string, some clients a bare URL. A header holding a control character or anything
// but ASCII names none. Only an https URL counts, without user info; its host lower-cased, without port and root dot.
func sigAgent(raw string) string {
	raw = strings.Trim(raw[:min(len(raw), maxSigAgent)], " ")
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x20 || raw[i] > 0x7e {
			return ""
		}
	}
	if k := sfKeyRE.FindString(raw); k != "" {
		raw = raw[len(k):]
	}
	v, ok := raw, true
	if strings.HasPrefix(raw, `"`) {
		v, ok = sfString(raw)
	} else if i := strings.IndexAny(raw, ",; "); i >= 0 {
		v = raw[:i]
	}
	if !ok {
		return ""
	}
	return httpsHost(v)
}

// sfString is the structured field string s (printable ASCII) starts with, false when it is none or something other
// than parameters or a further member follows it.
func sfString(s string) (string, bool) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			if i+1 >= len(s) || (s[i+1] != '"' && s[i+1] != '\\') {
				return "", false
			}
			i++
			b.WriteByte(s[i])
		case '"':
			rest := strings.TrimLeft(s[i+1:], " ")
			if rest != "" && rest[0] != ';' && rest[0] != ',' {
				return "", false
			}
			return b.String(), true
		default:
			b.WriteByte(c)
		}
	}
	return "", false // unterminated: clipped, or never closed
}

// httpsHost is the host of an https URL as sigHostRE has it, "" for anything else.
func httpsHost(v string) string {
	for i := 0; i < len(v); i++ {
		if v[i] <= 0x20 || v[i] > 0x7e {
			return ""
		}
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil {
		return ""
	}
	h := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if !sigHostRE.MatchString(h) || strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") || strings.Contains(h, "..") {
		return ""
	}
	return h
}

// hostCounts tallies claimed hosts.
type hostCounts map[string]int64

// add counts n events of a raw header.
func (c hostCounts) add(raw sql.NullString, n int64) {
	if h := sigAgent(raw.String); raw.Valid && h != "" {
		c[h] += n
	}
}

// top are the n most frequent hosts, ties by name.
func (c hostCounts) top(n int) []string {
	out := make([]string, 0, len(c))
	for h := range c {
		out = append(out, h)
	}
	slices.SortFunc(out, func(a, b string) int { return cmp.Or(cmp.Compare(c[b], c[a]), strings.Compare(a, b)) })
	return out[:min(n, len(out))]
}

// newestDays are the pieces of [from, to)'s newest sayDays days.
func newestDays(from, to time.Time) [][2]time.Time {
	pcs := pieces(from, to)
	return pcs[max(0, len(pcs)-sayDays):]
}

// servicesSay fills the services rows' says: of each name's events (any source: a bot's browser rows carry the name
// too) over the newest raw days of the range, the host most claim. Raw whatever the source: no aggregate keeps it.
// Only the rows' names, only events whose props mention a claim.
func (s *Server) servicesSay(ctx context.Context, q *query, rows []listRow) error {
	if len(rows) == 0 {
		return nil
	}
	pl, err := s.planOf(ctx, q)
	if err != nil {
		return err
	}
	byName := map[string]hostCounts{}
	names := make([]any, 0, len(rows))
	for _, r := range rows {
		byName[r.Key] = hostCounts{}
		names = append(names, r.Key)
	}
	in := strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ")
	for _, pc := range newestDays(pl.rawFrom(q.p.win.From), q.p.win.End) {
		// The day's bounds last: the plan test reads them there.
		if err := s.each(ctx, `SELECT svc, `+sigAgentSQL+`, COUNT(*) FROM event WHERE site_id = ? AND svc IN (`+in+`)
			AND `+mayClaimSQL+` AND `+audWhere(q.aud)+` AND ts >= ? AND ts < ? GROUP BY 1, 2`,
			append(append([]any{q.site.ID}, names...), ms(pc[0]), ms(pc[1])),
			func(r *sql.Rows) error {
				var svc string
				var raw sql.NullString
				var n int64
				if err := r.Scan(&svc, &raw, &n); err != nil {
					return err
				}
				if c, ok := byName[svc]; ok {
					c.add(raw, n)
				}
				return nil
			}); err != nil {
			return err
		}
	}
	for i := range rows {
		if top := byName[rows[i].Key].top(1); len(top) > 0 {
			rows[i].Says = top[0]
		}
	}
	return nil
}

// actorSays are the hosts an actor's events claim most, at most three, over the newest days to its last event within
// [from, to) (unix ms of the last: last).
func (s *Server) actorSays(ctx context.Context, site int64, key string, from, to time.Time, last int64) ([]string, error) {
	end := minTime(to, time.UnixMilli(last+1))
	if lo := utcDay(time.UnixMilli(last).UTC()).AddDate(0, 0, 1-sayDays); lo.After(from) {
		from = lo
	}
	c := hostCounts{}
	for _, pc := range newestDays(from, end) {
		if err := s.each(ctx, `SELECT `+sigAgentSQL+`, COUNT(*) FROM event `+byActor+` WHERE site_id = ? AND ip_key = ?
			AND ts >= ? AND ts < ? AND `+mayClaimSQL+` GROUP BY 1`, []any{site, key, ms(pc[0]), ms(pc[1])},
			func(r *sql.Rows) error {
				var raw sql.NullString
				var n int64
				if err := r.Scan(&raw, &n); err != nil {
					return err
				}
				c.add(raw, n)
				return nil
			}); err != nil {
			return nil, err
		}
	}
	return c.top(3), nil
}
