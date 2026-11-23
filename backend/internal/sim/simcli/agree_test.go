package simcli

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trick77/stakeout/backend/internal/agg"
	"github.com/trick77/stakeout/backend/internal/dash"
	"github.com/trick77/stakeout/backend/internal/sim"
	"github.com/trick77/stakeout/backend/internal/store"
)

// distinctDims are the lists whose rows hold a distinct count (pages, entry, referrers, countries: visitors;
// networks: addresses; events, outbound, probed, errors: their second figure), merged from sketches; refused is
// counts only.
var distinctDims = map[string]bool{"pages": true, "entry": true, "referrers": true, "countries": true,
	"networks": true, "events": true, "outbound": true, "probed": true, "errors": true}

// countValued are the lists whose value is a count: rows and Other add up to the raw total however many entries
// a day keeps.
var countValued = map[string]bool{"events": true, "outbound": true, "probed": true, "errors": true}

// agreeCase is one zone and comparison of TestSourcesAgree, and its ranges.
type agreeCase struct {
	tz, cmp string
	rngs    []string
}

// agreeCases are TestSourcesAgree's: every zone, both comparisons and every range (12mo in UTC only: its requests read
// a year of day pieces each, three quarters of the test's time in every zone).
func agreeCases() (cases []agreeCase) {
	for _, tz := range []string{"UTC", "Europe/Zurich", "Asia/Kolkata", "America/New_York"} {
		for _, c := range []string{"prev", "yoy"} {
			rngs := ranges
			if tz != "UTC" {
				rngs = slices.DeleteFunc(slices.Clone(ranges), func(r string) bool { return r == "12mo" })
			}
			cases = append(cases, agreeCase{tz, c, rngs})
		}
	}
	return cases
}

// agreeScenarios play on a 400-day history: the half year of growth, and the stories of the last days, whose lists
// (new referrers, probes, errors, spikes) the comparisons read.
const agreeScenarios = "growth,referrer-spike,scan-wave,deploy-error,api-walker,flood-48"

// TestSourcesAgree asks every request of the UI's parameter set (Export's) in each zone against the prior period and
// last year, from raw events and from the aggregates, over a 400-day history: additive figures equal, distinct ones
// within ±max(1, 0.1 %). Then the aggregates are rebuilt at three entries a day per list part: a key never cut has
// its raw figures, rows and Other add up to the raw total of a counted list, and nothing is new that raw does not
// call new.
func TestSourcesAgree(t *testing.T) {
	t.Parallel()
	skipRace(t)
	if testing.Short() {
		t.Skip("400-day histories")
	}
	cases := agreeCases()
	for _, seed := range []uint64{3, 7} {
		t.Run("seed"+strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			start := time.Now()
			st, w := classifiedHistory(t, seed, 0.15, 400, agreeScenarios)
			t.Logf("history in %v", time.Since(start).Round(time.Millisecond))
			// The premise of equal visits: no session of a classified day has rows in both parts.
			var mixed int64
			if err := st.DB.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM (SELECT 1 FROM event WHERE sid != ''
				GROUP BY site_id, ts / 86400000, ip_key, sid HAVING MIN(cls = 1) != MAX(cls = 1))`).Scan(&mixed); err != nil ||
				mixed != 0 {
				t.Errorf("sessions with rows in both parts: %d, %v", mixed, err)
			}
			now := w.Anchor.Add(time.Minute)
			raw, err := newDashboard(t.Context(), st, w, now, dash.SourceRaw)
			if err != nil {
				t.Fatal(err)
			}
			aggs, err := newDashboard(t.Context(), st, w, now, dash.SourceAgg)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			asked, total := map[string]map[string]any{}, 0 // the raw answers by requestKey
			t.Run("top500", func(t *testing.T) {
				for _, c := range cases {
					t.Run(strings.ReplaceAll(c.tz, "/", "-")+"_"+c.cmp, func(t *testing.T) {
						t.Parallel()
						for _, rng := range c.rngs {
							t.Run(rng, func(t *testing.T) {
								t.Parallel()
								get := func(ep string, params map[string]string) (map[string]any, error) {
									key := requestKey(ep, params)
									mu.Lock()
									rb, again := asked[key]
									total++
									mu.Unlock()
									if again {
										return rb, nil
									}
									rb = answer(t, raw, ep, params)
									agreeBodies(t, ep+" "+key, ep, params, rb, answer(t, aggs, ep, params))
									mu.Lock()
									asked[key] = rb
									mu.Unlock()
									return rb, nil
								}
								for _, s := range w.Sites {
									if err := uiRequests(s.Domain, rng, c.tz, c.cmp, get); err != nil {
										t.Fatal(err)
									}
								}
							})
						}
					})
				}
			})
			t.Logf("%d requests, %d asked from both sources", total, len(asked))
			// Three entries a day per list part: the same store, rebuilt.
			for _, tbl := range []string{"agg_day", "agg_hour", "agg_list", "agg_addr"} {
				if _, err := st.DB.ExecContext(t.Context(), `DELETE FROM `+tbl); err != nil { //nolint:gosec // fixed names
					t.Fatal(err)
				}
			}
			if _, err := agg.BuildThrough(t.Context(), st, w.Anchor, 3); err != nil {
				t.Fatal(err)
			}
			topThree(t, st, w, raw, aggs)
		})
	}
}

// The late feed and the feed outage three days on, their days frozen: the aggregates answer as raw does, the hours
// without a record frozen as zeros, the day the feed starts alike. On a copy of the scenario test's history.
func TestSourcesAgreeFeedGaps(t *testing.T) {
	t.Parallel()
	for _, sc := range sim.Catalogue() {
		if sc.Name != "late-feed" && sc.Name != "feed-outage" {
			continue
		}
		t.Run(sc.Name, func(t *testing.T) {
			t.Parallel()
			days, scale := max(sc.MinDays, 9), 1.0 // TestScenarioExpectations' history
			if raceOn {
				days, scale = max(sc.MinDays, 3), 0.3
			}
			src, w := history(t, scale, days, sc.Name)
			path := filepath.Join(t.TempDir(), "stakeout.db")
			if _, err := src.DB.ExecContext(t.Context(), `VACUUM INTO ?`, path); err != nil {
				t.Fatal(err)
			}
			st, err := store.Open(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			later := w.Anchor.Add(3 * 24 * time.Hour)
			if _, err := agg.BuildThrough(t.Context(), st, later, agg.TopK); err != nil {
				t.Fatal(err)
			}
			now := later.Add(time.Minute)
			raw, err := newDashboard(t.Context(), st, w, now, dash.SourceRaw)
			if err != nil {
				t.Fatal(err)
			}
			aggs, err := newDashboard(t.Context(), st, w, now, dash.SourceAgg)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range w.Sites {
				for _, rng := range []string{"7d", "30d"} {
					for _, ask := range [][]string{{"overview"}, {"classes"}, {"status"}, {"noteworthy", "findings", "all"},
						{"list", "dim", "pages"}, {"list", "dim", "probed"}, {"list", "dim", "networks"},
						{"list", "dim", "endpoints"}, {"list", "dim", "accounts"}, {"list", "dim", "failures"},
						{"list", "dim", "agents"}} {
						if !s.Feed && (ask[0] == "classes" || ask[0] == "status" || len(ask) > 2 &&
							slices.Contains([]string{"probed", "endpoints", "accounts", "failures", "agents"}, ask[2])) {
							continue // the server feed's panels
						}
						params := map[string]string{"site": s.Domain, "range": rng, "tz": "UTC"}
						for i := 1; i+1 < len(ask); i += 2 {
							params[ask[i]] = ask[i+1]
						}
						rb, gb := answer(t, raw, ask[0], params), answer(t, aggs, ask[0], params)
						agreeBodies(t, fmt.Sprint(ask[0], params), ask[0], params, rb, gb)
						if ask[0] != "overview" || rng != "7d" || !s.Feed {
							continue
						}
						// Requests stop with the history: the last three days froze empty.
						var series []any
						for _, x := range gb["tiles"].([]any) {
							if m := x.(map[string]any); m["key"] == "requests" || m["key"] == "calls" {
								series = m["series"].([]any)
							}
						}
						if len(series) != 7 || series[6] != 0.0 || series[5] != 0.0 || series[2] == 0.0 {
							t.Errorf("%s: requests %v", s.Domain, series)
						}
					}
				}
			}
		})
	}
}

// requestKey names a request whose answer does not depend on the comparison without it: asked once per zone.
func requestKey(ep string, params map[string]string) string {
	v := url.Values{}
	for k, x := range params {
		if k == "cmp" && slices.Contains([]string{"visitors", "visitor", "log", "domain"}, ep) {
			continue
		}
		v.Set(k, x)
	}
	return ep + "?" + v.Encode()
}

func answer(t *testing.T, d *Dashboard, ep string, params map[string]string) map[string]any {
	t.Helper()
	code, b := d.Get(t.Context(), ep, params)
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil || code != 200 {
		t.Fatalf("%s %v: %d %s", ep, params, code, b)
	}
	return body
}

// agreeBodies compares a raw and an aggregate answer field by field: equal, but inside a distinct figure's subtree
// numbers within ±max(1, 0.1 %), and the change and verdict derived from them not at all.
func agreeBodies(t *testing.T, where, ep string, params map[string]string, raw, got map[string]any) {
	t.Helper()
	short := params["range"] == "24h" || params["range"] == "7d"
	loose := func(path string, v any) bool {
		switch ep {
		case "overview", "noteworthy":
			m, ok := v.(map[string]any)
			return ok && (m["key"] == "visitors" || strings.HasPrefix(fmt.Sprint(m["id"]), "trend:visitors"))
		case "classes":
			return strings.HasSuffix(path, ".addresses")
		case "list":
			return distinctDims[params["dim"]] && (rowPath.MatchString(path) || path == ".other")
		case "clients": // at 30d and longer a sketch per client
			return !short
		}
		return false
	}
	// In a list's loose rows only the distinct figure is loose (and the change derived from it when it is the
	// value); the counted one stays exact.
	field := ""
	if ep == "list" {
		field = distinctField(params["dim"])
	}
	var walk func(path string, a, b any, isLoose bool)
	walk = func(path string, a, b any, isLoose bool) {
		isLoose = isLoose || loose(path, a)
		switch x := a.(type) {
		case map[string]any:
			y, ok := b.(map[string]any)
			if !ok || len(x) != len(y) {
				t.Errorf("%s %s: raw %v, aggregates %v", where, path, a, b)
				return
			}
			// A list row's change follows its distinct value: compared exactly whenever the two values are equal.
			sameValue := ep == "list" && field == "value" && x["value"] == y["value"]
			for k, v := range x {
				if isLoose && (k == "change" || k == "verdict") && (field == "" || field == "value") && !sameValue {
					continue
				}
				walk(path+"."+k, v, y[k], isLoose && (field == "" || k == field))
			}
		case []any:
			y, ok := b.([]any)
			if !ok || len(x) != len(y) {
				t.Errorf("%s %s: raw %d items, aggregates %v", where, path, len(x), b)
				return
			}
			for i := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), x[i], y[i], isLoose)
			}
		case float64:
			y, ok := b.(float64)
			if !ok || (x != y && !(isLoose && near(x, y))) {
				t.Errorf("%s %s: raw %v, aggregates %v", where, path, a, b)
			}
		default:
			if fmt.Sprint(a) != fmt.Sprint(b) {
				t.Errorf("%s %s: raw %v, aggregates %v", where, path, a, b)
			}
		}
	}
	walk("", raw, got, false)
}

// distinctField is the JSON field of a list's rows holding its distinct count: the second figure of a counted list,
// else the value.
func distinctField(dim string) string {
	if countValued[dim] {
		return "second"
	}
	return "value"
}

// rowPath is a list row's own path in an answer.
var rowPath = regexp.MustCompile(`^\.rows\[\d+\]$`)

// near: a distinct count from sketches is off the exact one by index collisions only, while sparse: none below 50
// (an off-by-one there is a defect, not a collision), at most max(1, 0.1 %) above.
func near(a, b float64) bool {
	if math.Max(a, b) < 50 {
		return a == b
	}
	return math.Abs(a-b) <= math.Max(1, 0.001*math.Max(math.Abs(a), math.Abs(b)))
}

// cutKeys is, per dimension, the hashes of the keys a site's Other rows hold on the days of spans (unix days,
// inclusive).
func cutKeys(t *testing.T, st *store.Store, site int64, spans [][2]int64) map[string]map[uint64]bool {
	t.Helper()
	out := map[string]map[uint64]bool{}
	for _, sp := range spans {
		rows, err := st.DB.QueryContext(t.Context(), `SELECT dim, keys FROM agg_list WHERE site_id = ? AND other = 1
			AND day >= ? AND day <= ?`, site, sp[0], sp[1])
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var dim string
			var keys []byte
			if err := rows.Scan(&dim, &keys); err != nil {
				t.Fatal(err)
			}
			if out[dim] == nil {
				out[dim] = map[uint64]bool{}
			}
			for _, h := range agg.OtherKeys(keys) {
				out[dim][h] = true
			}
		}
		_ = rows.Close()
	}
	return out
}

// topThree checks the lists and Noteworthy over aggregates kept to three entries a day per part, both ways: every
// row of either answer is the other's or, cut on a day of the range or its comparison, a lower bound of it (a counted
// list), with no change or raw's; a key never cut there has raw's figures and change.
func topThree(t *testing.T, st *store.Store, w *sim.World, raw, aggs *Dashboard) {
	t.Helper()
	ids, err := st.Sites(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	byKey := func(body map[string]any) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, row := range rows(body) {
			out[fmt.Sprint(row["key"])] = row
		}
		return out
	}
	num := func(v any) float64 { f, _ := v.(float64); return f }
	rngs, cmps := []string{"30d", "90d", "12mo"}, []string{"prev", "yoy"}
	today := w.Anchor.UnixMilli() / 86_400_000
	anyCut := false
	for _, s := range w.Sites {
		for _, rng := range rngs {
			for _, c := range cmps {
				// The range and its comparison, a day's margin either side.
				n := map[string]int64{"30d": 30, "90d": 90, "12mo": 366}[rng]
				spans := [][2]int64{{today - 2*n - 2, today + 1}}
				if c == "yoy" {
					spans = [][2]int64{{today - n - 2, today + 1}, {today - 365 - n - 2, today - 365 + 2}}
				}
				collapsed := cutKeys(t, st, ids[s.Domain].ID, spans)
				anyCut = anyCut || len(collapsed["pages"]) > 0
				for _, dim := range listDims {
					if dim == "refused" { // per address, from agg_addr: never cut
						continue
					}
					params := map[string]string{"site": s.Domain, "range": rng, "cmp": c, "tz": "UTC", "dim": dim,
						"limit": "500"}
					where := fmt.Sprint(params)
					rb, gb := answer(t, raw, "list", params), answer(t, aggs, "list", params)
					rawRows, gotRows := byKey(rb), byKey(gb)
					dfield, cfield := distinctField(dim), "second" // the distinct figure, the counted one
					if dfield == "second" {
						cfield = "value"
					}
					for k := range rawRows {
						if _, ok := gotRows[k]; !ok && !collapsed[dim][agg.Hash(k)] {
							t.Errorf("%s: %s, never cut, missing from the aggregates", where, k)
						}
					}
					for k, g := range gotRows {
						r, ok := rawRows[k]
						cut := collapsed[dim][agg.Hash(k)]
						sameChange := changeOf(g["change"]) == changeOf(r["change"]) && g["verdict"] == r["verdict"]
						switch {
						case !ok:
							t.Errorf("%s: %s is no raw key", where, k)
							continue
						case !cut && (!near(num(g[dfield]), num(r[dfield])) || g[cfield] != r[cfield]):
							t.Errorf("%s %s, never cut: %v, raw %v", where, k, g, r)
						case !cut && num(g[dfield]) == num(r[dfield]) && !sameChange:
							t.Errorf("%s %s, never cut: change %v %v, raw %v %v", where, k, g["change"], g["verdict"],
								r["change"], r["verdict"])
						case cut && countValued[dim] && num(g["value"]) > num(r["value"]):
							t.Errorf("%s %s, cut: %v above raw's %v", where, k, g["value"], r["value"])
						case cut && g["change"] != nil && changeOf(g["change"]) != changeOf(r["change"]):
							t.Errorf("%s %s, cut: change %v, raw %v", where, k, g["change"], r["change"])
						}
						if isNew(g["change"]) && !isNew(r["change"]) {
							t.Errorf("%s %s: new from the aggregates, raw %v", where, k, r["change"])
						}
					}
					sum := func(body map[string]any) float64 {
						v := num(body["other"].(map[string]any)["value"])
						for _, row := range rows(body) {
							v += num(row["value"])
						}
						return v
					}
					if countValued[dim] && sum(rb) != sum(gb) {
						t.Errorf("%s: rows and Other %v, raw %v", where, sum(gb), sum(rb))
					}
					// Every key, shown or only in an Other's listed hashes: raw's count (no day past the listing bound).
					if num(gb["total"]) != num(rb["total"]) {
						t.Errorf("%s: total %v, raw %v", where, gb["total"], rb["total"])
					}
				}
				params := map[string]string{"site": s.Domain, "range": rng, "cmp": c, "tz": "UTC", "findings": "all"}
				rawIDs := map[string]bool{}
				for _, x := range answer(t, raw, "noteworthy", params)["items"].([]any) {
					rawIDs[fmt.Sprint(x.(map[string]any)["id"])] = true
				}
				for _, x := range answer(t, aggs, "noteworthy", params)["items"].([]any) {
					if id := fmt.Sprint(x.(map[string]any)["id"]); !rawIDs[id] && !strings.HasPrefix(id, "trend:") {
						t.Errorf("%v: finding %s only from the aggregates", params, id)
					}
				}
			}
		}
	}
	if !anyCut {
		t.Fatal("no page cut at three entries a day")
	}
}

func isNew(change any) bool {
	m, ok := change.(map[string]any)
	return ok && m["new"] == true
}

func changeOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
