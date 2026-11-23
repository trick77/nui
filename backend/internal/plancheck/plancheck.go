// Package plancheck reads SQLite query plans for the tests that pin them (store, agg, dash): no statement may read
// all of the event table or an aggregate table, nor all of one site's rows in it.
package plancheck

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
)

// Explain returns q's plan, one detail line per step.
func Explain(ctx context.Context, db *sql.DB, q string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			return nil, err
		}
		plan = append(plan, detail)
	}
	return plan, rows.Err()
}

// tableRef is a table named after FROM, JOIN or a comma (a comma join), its alias after it. A comma in a column list
// matches too: it maps a column name to itself, which is harmless.
var tableRef = regexp.MustCompile(`(?i)(?:\bFROM|\bJOIN|,)\s+(\w+)(?:\s+INDEXED\s+BY\s+\w+)?(?:\s+(?:AS\s+)?(\w+))?`)

// constraint is a SEARCH step's index constraint list.
var constraint = regexp.MustCompile(`\(([^()]*)\)\s*(?:LEFT-JOIN)?\s*$`)

// minMax is a lone MIN or MAX, a COALESCE around it too, with a WHERE of plain terms: its group is the WHERE.
var minMax = regexp.MustCompile(
	`(?is)^\s*SELECT\s+(?:COALESCE\(\s*)?(?:MIN|MAX)\(\s*\w+\s*\)(?:\s*,\s*\w+\s*\))?\s+FROM\s+\w+\s+WHERE\s+([^()]*)$`)

var (
	// andTerms splits a WHERE or a constraint list into its terms.
	andTerms = regexp.MustCompile(`(?i)\s+AND\s+`)
	// grouped is what may follow a WHERE's terms and makes a MIN or MAX more than one probe.
	grouped = regexp.MustCompile(`(?i)\b(GROUP|ORDER|LIMIT|HAVING)\b`)
)

// oneProbe: SQLite answers q with one index probe however wide its constraint (measured: 5 µs over a site's 400k
// events): a lone MIN or MAX whose every WHERE term is in the index constraint c. A term outside it (an unindexed
// filter) or a GROUP BY walks every row the constraint leaves.
func oneProbe(q, c string) bool {
	m := minMax.FindStringSubmatch(q)
	if m == nil || grouped.MatchString(m[1]) {
		return false
	}
	return len(andTerms.Split(strings.TrimSpace(m[1]), -1)) == len(andTerms.Split(c, -1))
}

// limited: q takes a LIMIT, a keyset page or a chunk: an open range read only that far.
var limited = regexp.MustCompile(`(?i)\bLIMIT\b`)

// Problem names what of q's plan reads too much, "" when nothing: a SCAN of event or an agg_ table, or a SEARCH of
// one whose index constraint is the leading site_id alone, or with a list's dim (every row of the site, every day of
// the list), or bounds a range on one side only without a LIMIT (every row of the site after or before a point).
// Plans name aliases; an alias q uses for several tables counts as each of them.
func Problem(q string, plan []string) string {
	alias := map[string][]string{}
	for _, m := range tableRef.FindAllStringSubmatch(q, -1) {
		alias[m[1]] = append(alias[m[1]], m[1])
		if m[2] != "" && !isKeyword(m[2]) {
			alias[m[2]] = append(alias[m[2]], m[1])
		}
	}
	for _, d := range plan {
		f := strings.Fields(d)
		if len(f) < 2 || (f[0] != "SCAN" && f[0] != "SEARCH") {
			continue
		}
		tbls := alias[f[1]]
		if len(tbls) == 0 {
			tbls = []string{f[1]}
		}
		for _, tbl := range tbls {
			if tbl != "event" && !strings.HasPrefix(tbl, "agg_") {
				continue
			}
			if f[0] == "SCAN" {
				return "full scan of " + tbl
			}
			c := constraint.FindStringSubmatch(d)
			switch {
			case c == nil || oneProbe(q, c[1]):
			case wholeSite(c[1]):
				return "every row of a site in " + tbl + " (" + c[1] + ")"
			case oneSided(c[1]) && !limited.MatchString(q):
				return "an open range of " + tbl + " (" + c[1] + ")"
			}
		}
	}
	return ""
}

// oneSided: the constraint list bounds a column from one side only (SQLite writes >= as > and <= as <).
func oneSided(c string) bool {
	lo, hi := map[string]bool{}, map[string]bool{}
	for _, term := range andTerms.Split(c, -1) {
		t := strings.TrimSpace(term)
		if i := strings.IndexAny(t, "<>"); i > 0 {
			if t[i] == '>' {
				lo[t[:i]] = true
			} else {
				hi[t[:i]] = true
			}
		}
	}
	for col := range lo {
		if !hi[col] {
			return true
		}
	}
	for col := range hi {
		if !lo[col] {
			return true
		}
	}
	return false
}

// wholeSite: the constraint list bounds nothing but the site and a list's dimension.
func wholeSite(c string) bool {
	for _, term := range strings.Split(c, " AND ") {
		if t := strings.TrimSpace(term); t != "site_id=?" && t != "dim=?" {
			return false
		}
	}
	return true
}

func isKeyword(w string) bool {
	switch strings.ToUpper(w) {
	case "WHERE", "ON", "LEFT", "JOIN", "INNER", "CROSS", "GROUP", "ORDER", "LIMIT", "USING", "INDEXED", "UNION",
		"FROM", "SET", "VALUES":
		return true
	}
	return false
}
