package plancheck

import (
	"path/filepath"
	"testing"

	"github.com/trick77/stakeout/backend/internal/store"
)

// The checker on the real schema: what reads a table or a site in full is named, through aliases, comma joins and
// an alias used twice; one-key, one-day and single-probe reads pass.
func TestProblem(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "stakeout.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for q, bad := range map[string]bool{
		`SELECT COUNT(*) FROM event e WHERE e.ua = 'x'`:                                       true,
		`SELECT * FROM event WHERE site_id = 1`:                                               true,
		`SELECT ip_key FROM event WHERE site_id = 1 GROUP BY ip_key`:                          true,
		`SELECT * FROM agg_list t WHERE t.site_id = 1 AND t.dim = 'pages'`:                    true,
		`SELECT d.ok, t.ua FROM (SELECT 1 AS ok) AS d LEFT JOIN agg_addr t ON t.ua = 'x'`:     true,
		`SELECT a.ts FROM agg_day x, event a WHERE x.site_id = 1 AND x.day = 2 AND a.ua = ''`: true,
		// t is agg_day in one subquery and agg_hour in the other: the agg_hour read is a site's every hour.
		`SELECT (SELECT COUNT(*) FROM agg_day t WHERE t.site_id = 1 AND t.day = 2),
			(SELECT COUNT(*) FROM agg_hour t WHERE t.site_id = 1)`: true,
		// A MIN with an unindexed filter or a GROUP BY walks the site; an open range walks it from a point on.
		`SELECT MIN(ts) FROM event WHERE site_id = 1 AND ua = 'x'`:                                 true,
		`SELECT MIN(ts) FROM event WHERE site_id = 1 GROUP BY ip_key`:                              true,
		`SELECT ip_key FROM event WHERE site_id = 1 AND ts >= 5`:                                   true,
		`SELECT key FROM agg_list WHERE site_id = 1 AND dim = 'x' AND day < 9`:                     true,
		`SELECT MIN(hour) FROM agg_hour WHERE site_id = 1 AND hour >= 5`:                           false,
		`SELECT day FROM agg_day WHERE site_id = 1 AND day > 5 AND dirty = 3 ORDER BY day LIMIT 1`: false,
		`SELECT ts FROM event WHERE site_id = 1 AND ip_key = 'x' AND ts >= 5`:                      true,

		`SELECT MIN(ts) FROM event WHERE site_id = 1`:                                       false,
		`SELECT COALESCE(MIN(ts), 0) FROM event WHERE site_id = 1`:                          false,
		`SELECT COALESCE(MIN(ts) + 1, 0) FROM event WHERE site_id = 1`:                      true,
		`SELECT MIN(day) FROM agg_list WHERE site_id = 1 AND dim = 'pages'`:                 false,
		`SELECT * FROM event WHERE site_id = 1 AND ts >= 1 AND ts < 2`:                      false,
		`SELECT asn FROM event WHERE site_id = 1 AND ip_key = 'x' ORDER BY ts DESC LIMIT 1`: false,
		`SELECT dim FROM agg_list WHERE site_id = 1 AND dim > '' ORDER BY dim LIMIT 1`:      false,
		`SELECT * FROM agg_list t WHERE t.site_id = 1 AND t.dim = 'pages' AND t.day = 3`:    false,
		`SELECT d.ok, t.ip_key FROM (SELECT frozen AS ok FROM agg_day WHERE site_id = 1 AND day = 2) AS d
			LEFT JOIN agg_addr t ON t.site_id = 1 AND t.day = 2`: false,
	} {
		plan, err := Explain(t.Context(), st.DB, q)
		if err != nil {
			t.Fatal(err)
		}
		if got := Problem(q, plan); (got != "") != bad {
			t.Errorf("%s\nplan %v: problem %q, want one: %v", q, plan, got, bad)
		}
	}
}
