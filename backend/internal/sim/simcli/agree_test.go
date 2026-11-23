package simcli

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
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

// agreeCase is one zone and comparison of TestSourcesAgree.
type agreeCase struct{ tz, cmp string }

// agreeSetup is TestSourcesAgree's size: without -race seeds 3 and 7, 400 days, every zone, both comparisons and
// every range (12mo in UTC only: its requests read a year of day pieces each, three quarters of the test's time in
// every zone). Under -race (SQLite crawls: the 400 days alone took 100 s) a smoke run: seed 7, 40 days at a fifth
// of the scale, Kolkata (a half-hour zone, floored buckets) against the prior period, 24h, 7d and 30d (90d and 12mo
// read a day piece per statement for months), raceSkips left out, and the three-entry pass at 30d only.
func agreeSetup() (seeds []uint64, days int, scale float64, cases []agreeCase, rngs []string) {
	if raceOn {
		return []uint64{7}, 40, 0.06, []agreeCase{{"Asia/Kolkata", "prev"}}, []string{"24h", "7d", "30d"}
	}
	for _, tz := range []string{"UTC", "Europe/Zurich", "Asia/Kolkata", "America/New_York"} {
		for _, c := range []string{"prev", "yoy"} {
			cases = append(cases, agreeCase{tz, c})
		}
	}
	return []uint64{3, 7}, 400, 0.15, cases, ranges
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
	if testing.Short() {
		t.Skip("400-day histories")
	}
	seeds, days, scale, cases, rngs := agreeSetup()
	for _, seed := range seeds {
		t.Run("seed"+strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			start := time.Now()
			st, w := classifiedHistory(t, seed, scale, days, agreeScenarios)
			t.Logf("history in %v", time.Since(start).Round(time.Millisecond))
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
						for _, rng := range rngs {
							if rng == "12mo" && c.tz != "UTC" {
								continue // a year of day pieces per request: in UTC only, as the dash tests
							}
							t.Run(rng, func(t *testing.T) {
								t.Parallel()
								get := func(ep string, params map[string]string) (map[string]any, error) {
									if raceOn && raceSkips(ep, params) {
										return map[string]any{}, nil
									}
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
			path := filepath.Join(t.TempDir(), "nui.db")
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
						{"list", "dim", "pages"}, {"list", "dim", "probed"}, {"list", "dim", "networks"}} {
						if !s.Feed && (ask[0] == "classes" || ask[0] == "status" || len(ask) > 2 && ask[2] == "probed") {
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
							if m := x.(map[string]any); m["key"] == "requests" {
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

// raceSkips is what -race leaves unasked: the short lists (cuts of the full ones, computed alike from either
// source; with them the expansions of the top pages, raw under every source) and the visitors table at 50 rows
// (the 7 are compared).
func raceSkips(ep string, params map[string]string) bool {
	return ep == "list" && params["limit"] != "500" && params["within"] == "" ||
		ep == "visitors" && params["limit"] == "50"
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
			return distinctDims[params["dim"]] && (strings.HasPrefix(path, ".rows[") || path == ".other")
		case "clients": // at 30d and longer a sketch per client
			return !short
		}
		return false
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
			for k, v := range x {
				if isLoose && (k == "change" || k == "verdict") {
					continue
				}
				walk(path+"."+k, v, y[k], isLoose)
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

// near: a distinct count from sketches is off the exact one by index collisions only, while sparse.
func near(a, b float64) bool {
	return math.Abs(a-b) <= math.Max(1, 0.001*math.Max(math.Abs(a), math.Abs(b)))
}

// topThree checks the lists and Noteworthy over aggregates kept to three entries a day per part.
func topThree(t *testing.T, st *store.Store, w *sim.World, raw, aggs *Dashboard) {
	t.Helper()
	collapsed := map[string]map[uint64]bool{} // per dimension, the hashes of every key some day cut
	cut, err := st.DB.QueryContext(t.Context(), `SELECT dim, keys FROM agg_list WHERE other = 1`)
	if err != nil {
		t.Fatal(err)
	}
	for cut.Next() {
		var dim string
		var keys []byte
		if err := cut.Scan(&dim, &keys); err != nil {
			t.Fatal(err)
		}
		if collapsed[dim] == nil {
			collapsed[dim] = map[uint64]bool{}
		}
		for _, h := range agg.OtherKeys(keys) {
			collapsed[dim][h] = true
		}
	}
	_ = cut.Close()
	if len(collapsed["pages"]) == 0 {
		t.Fatal("no page cut at three entries a day")
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
	if raceOn {
		rngs, cmps = []string{"30d"}, []string{"prev"}
	}
	for _, s := range w.Sites {
		for _, rng := range rngs {
			for _, c := range cmps {
				for _, dim := range listDims {
					if dim == "refused" { // per address, from agg_addr: never cut
						continue
					}
					params := map[string]string{"site": s.Domain, "range": rng, "cmp": c, "tz": "UTC", "dim": dim,
						"limit": "500"}
					where := fmt.Sprint(params)
					rb, gb := answer(t, raw, "list", params), answer(t, aggs, "list", params)
					rawRows := byKey(rb)
					for k, g := range byKey(gb) {
						r, ok := rawRows[k]
						switch {
						case !ok:
							t.Errorf("%s: %s is no raw key", where, k)
							continue
						case !collapsed[dim][agg.Hash(k)] && (!near(num(g["value"]), num(r["value"])) ||
							!near(num(g["second"]), num(r["second"])) ||
							!distinctDims[dim] && (g["value"] != r["value"] || g["second"] != r["second"])):
							t.Errorf("%s %s, never cut: %v, raw %v", where, k, g, r)
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
					if num(gb["total"]) != float64(len(rows(gb)))+num(gb["other"].(map[string]any)["count"]) {
						t.Errorf("%s: total %v", where, gb["total"])
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
}

func isNew(change any) bool {
	m, ok := change.(map[string]any)
	return ok && m["new"] == true
}
