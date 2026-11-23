package dash

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trick77/stakeout/backend/internal/agg"
	"github.com/trick77/stakeout/backend/internal/store"
)

// build builds every day of the rig's store up to its end, as BuildThrough does for a history.
func (r *rig) build(t *testing.T) {
	t.Helper()
	if _, err := agg.BuildThrough(t.Context(), r.s.DB, r.end, agg.TopK); err != nil {
		t.Fatal(err)
	}
}

// pieceCount counts the pieces read from each source: from the aggregates, raw on the tables' grain (pieces the
// aggregates could have served), raw off it.
type pieceCount struct{ agg, raw, off int }

// countPieces counts the pieces the requests f makes through its view of r read.
func countPieces(r *rig, f func(r *rig)) pieceCount {
	var mu sync.Mutex
	var c pieceCount
	v := *r
	v.trace = func(fromAgg, aligned bool) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case fromAgg:
			c.agg++
		case aligned:
			c.raw++
		default:
			c.off++
		}
	}
	f(&v)
	return c
}

// server is a server of its own over the rig's store reading from src: parallel subtests share the rig.
func (r *rig) server(src Source) *Server {
	s := r.s
	return &Server{DB: s.DB, Sites: s.Sites, Ref: s.Ref, Counters: s.Counters, Model: s.Model,
		RetentionMonths: s.RetentionMonths, Build: s.Build, Now: s.Now, Source: src, trace: r.trace}
}

// getFrom asks the endpoint of a server reading from src.
func (r *rig) getFrom(t *testing.T, src Source, endpoint string, params map[string]string) (int, map[string]any) {
	t.Helper()
	return serve(t, handler(r.server(src)), endpoint, params)
}

// both asks an endpoint from raw and from the aggregates, each answering 200: two equal failures never agree.
func (r *rig) both(t *testing.T, ep string, params map[string]string) (raw, got map[string]any) {
	t.Helper()
	code, raw := r.getFrom(t, SourceRaw, ep, params)
	gotCode, got := r.getFrom(t, SourceAgg, ep, params)
	if code != 200 || gotCode != 200 {
		t.Fatalf("%s %v: %d %v, %d %v", ep, params, code, raw, gotCode, got)
	}
	return raw, got
}

// sourceCase is a zone, range and comparison both sources are asked in.
type sourceCase struct{ tz, rng, cmp string }

func (c sourceCase) String() string { return strings.ReplaceAll(c.tz, "/", "-") + "_" + c.rng + "_" + c.cmp }

// raceCases replace the full matrix under -race, where SQLite crawls: every zone, range and comparison at least
// once, and the pairs with a path of their own (Kolkata's half hour at 24h and 7d, read raw, and at 30d, floored;
// a zone west of UTC at 30d and 90d; 24h against last year, which compares no list; 12mo in UTC).
var raceCases = []sourceCase{{"UTC", "24h", "prev"}, {"UTC", "7d", "yoy"}, {"UTC", "30d", "prev"},
	{"UTC", "90d", "yoy"}, {"UTC", "12mo", "prev"}, {"Europe/Zurich", "7d", "prev"}, {"Europe/Zurich", "30d", "yoy"},
	{"Asia/Kolkata", "24h", "yoy"}, {"Asia/Kolkata", "7d", "prev"}, {"Asia/Kolkata", "30d", "prev"},
	{"America/New_York", "24h", "prev"}, {"America/New_York", "30d", "prev"}, {"America/New_York", "90d", "yoy"}}

// sourceCases are the cases of the ranges given: every zone × range × comparison (12mo in UTC only: a year of
// pieces reads alike in every zone), under -race raceCases.
func sourceCases(ranges ...string) []sourceCase {
	var out []sourceCase
	if raceOn {
		for _, c := range raceCases {
			if slices.Contains(ranges, c.rng) {
				out = append(out, c)
			}
		}
		return out
	}
	for _, tz := range []string{"UTC", "Europe/Zurich", "Asia/Kolkata", "America/New_York"} {
		for _, rng := range ranges {
			for _, c := range []string{"prev", "yoy"} {
				if rng != "12mo" || tz == "UTC" {
					out = append(out, sourceCase{tz, rng, c})
				}
			}
		}
	}
	return out
}

// fullAuds: every audience is asked at 30d (against the prior period only, unless anyCmp), under -race in UTC
// against the prior period only.
func fullAuds(c sourceCase, anyCmp bool) []string {
	if c.rng == "30d" && (c.cmp == "prev" || anyCmp && !raceOn) && (!raceOn || c.tz == "UTC") {
		return []string{"all", "humans", "bots"}
	}
	return []string{"all"}
}

// distinctTol is how far a distinct count from sketches may be from the exact one: index collisions, rare in the
// sparse form; none below 50, where an off-by-one is a defect.
func distinctTol(a, b float64) bool {
	if math.Max(a, b) < 50 {
		return a == b
	}
	return math.Abs(a-b) <= math.Max(1, 0.001*math.Max(math.Abs(a), math.Abs(b)))
}

// rowPath is a list answer's row.
var rowPath = regexp.MustCompile(`^\.rows\[\d+\]$`)

// agree compares two answers field by field: equal, but inside a loose subtree (a distinct figure) numbers within
// distinctTol, and the change and verdict derived from them not at all, unless a list row's two values are equal.
func agree(t *testing.T, where string, raw, got any, loose func(path string, v any) bool) {
	t.Helper()
	var walk func(path string, a, b any, isLoose bool)
	walk = func(path string, a, b any, isLoose bool) {
		isLoose = isLoose || (loose != nil && loose(path, a))
		switch x := a.(type) {
		case map[string]any:
			y, ok := b.(map[string]any)
			if !ok || len(x) != len(y) {
				t.Errorf("%s %s: %v vs %v", where, path, a, b)
				return
			}
			same := rowPath.MatchString(path) && x["value"] == y["value"]
			for k, v := range x {
				if isLoose && (k == "change" || k == "verdict") && !same {
					continue
				}
				walk(path+"."+k, v, y[k], isLoose)
			}
		case []any:
			y, ok := b.([]any)
			if !ok || len(x) != len(y) {
				t.Errorf("%s %s: %d items vs %v", where, path, len(x), b)
				return
			}
			for i := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), x[i], y[i], isLoose)
			}
		case float64:
			y, ok := b.(float64)
			if !ok || (x != y && !(isLoose && distinctTol(x, y))) {
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

// looseTiles marks the distinct figures of the overview and the classes: the visitors tile, addresses per class.
func looseTiles(path string, v any) bool {
	if m, ok := v.(map[string]any); ok && m["key"] == "visitors" {
		return true
	}
	return strings.HasSuffix(path, ".addresses")
}

// The tiles, the classes and Noteworthy's trends read the same from the hourly aggregates as from raw, in every zone
// and comparison; the aggregates were read, and no piece on their grain was read raw.
func TestSources_Hourly(t *testing.T) {
	t.Parallel()
	r := sharedRig(t, nil)
	for _, c := range sourceCases("24h", "7d", "30d", "90d", "12mo") {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()
			for _, aud := range fullAuds(c, true) {
				params := p(feedSite, "range", c.rng, "cmp", c.cmp, "aud", aud, "tz", c.tz)
				where := fmt.Sprint(params)
				for _, ep := range []string{"overview", "classes"} {
					code, raw := r.getFrom(t, SourceRaw, ep, params)
					var got map[string]any
					var gotCode int
					n := countPieces(r, func(r *rig) { gotCode, got = r.getFrom(t, SourceAgg, ep, params) })
					if code != 200 || gotCode != 200 {
						t.Fatalf("%s %s: %d, %d %v", ep, where, code, gotCode, got)
					}
					delete(raw, "live")
					delete(got, "live")
					agree(t, ep+" "+where, raw, got, looseTiles)
					// Kolkata's 24h and 7d hours are on the half hour: read raw, under every source.
					offGrain := c.tz == "Asia/Kolkata" && (c.rng == "24h" || c.rng == "7d")
					if (n.agg == 0 && !offGrain) || n.raw != 0 || (n.off != 0 && !offGrain) {
						t.Errorf("%s %s: pieces %+v", ep, where, n)
					}
				}
			}
		})
	}
}

// looseRows marks a list's rows as loose when a figure of theirs is a distinct count; count dimensions agree exactly.
func looseRows(dim string) func(string, any) bool {
	d, ok := dims[dim]
	distinct := ok && d.distinct != ""
	return func(path string, v any) bool {
		return distinct && (strings.HasPrefix(path, ".rows[") || path == ".other")
	}
}

// The lists, status, the clients split and Noteworthy's findings read the same from the daily aggregates as from
// raw, in every zone and comparison.
func TestSources_Lists(t *testing.T) {
	t.Parallel()
	r := sharedRig(t, nil)
	ask := func(t *testing.T, ep, where string, params map[string]string, loose func(string, any) bool) {
		t.Helper()
		code, raw := r.getFrom(t, SourceRaw, ep, params)
		var got map[string]any
		var gotCode int
		n := countPieces(r, func(r *rig) { gotCode, got = r.getFrom(t, SourceAgg, ep, params) })
		if code != 200 || gotCode != 200 {
			t.Fatalf("%s %s: %d, %d %v", ep, where, code, gotCode, got)
		}
		agree(t, ep+" "+where, raw, got, loose)
		if n.raw != 0 || (n.agg == 0 && params["range"] != "7d" && params["within"] == "") {
			t.Errorf("%s %s: pieces %+v", ep, where, n)
		}
	}
	names := slices.Sorted(maps.Keys(dims))
	for i, c := range sourceCases("7d", "30d", "90d", "12mo") {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()
			// Under -race every dimension in UTC at 30d, and two of them in each other case, in turn.
			ds := names
			if raceOn && c != (sourceCase{"UTC", "30d", "prev"}) {
				ds = []string{names[(2*i)%len(names)], names[(2*i+1)%len(names)]}
			}
			for _, aud := range fullAuds(c, false) {
				base := p(feedSite, "range", c.rng, "cmp", c.cmp, "aud", aud, "tz", c.tz)
				where := fmt.Sprint(base)
				for _, dim := range ds {
					params := p(feedSite, "range", c.rng, "cmp", c.cmp, "aud", aud, "tz", c.tz, "dim", dim, "limit", "500")
					ask(t, "list", where+" "+dim, params, looseRows(dim))
				}
				ask(t, "status", where, base, nil)
				ask(t, "clients", where, base, func(path string, _ any) bool { return c.rng != "7d" })
				if aud == "all" {
					ask(t, "noteworthy", where, p(feedSite, "range", c.rng, "cmp", c.cmp, "tz", c.tz, "findings", "all"), nil)
				}
			}
		})
	}
	// The other parameters: a sort, a search, a sub, the paths inside a template (raw only).
	t.Run("params", func(t *testing.T) {
		t.Parallel()
		for _, kv := range [][]string{{"dim", "pages", "sort", "change"}, {"dim", "pages", "limit", "2"},
			{"dim", "countries", "q", "ch"}, {"dim", "events", "sub", "custom"}, {"dim", "events", "sub", "auto"},
			{"dim", "pages", "within", "/:cidr"}, {"dim", "entry", "limit", "3"}} {
			params := p(feedSite, append([]string{"range", "30d", "tz", "UTC"}, kv...)...)
			ask(t, "list", fmt.Sprint(kv), params, looseRows(kv[1]))
		}
	})
}

// The visitors table and a visitor's sums read the same from agg_addr as from raw. In each case every row of the
// table is compared before its page's per-row reads (what it does, the reading: raw under every source, and most of
// the cost); the whole answer at the UI's limits in UTC, and the details of the keys listed first.
func TestSources_Addresses(t *testing.T) {
	t.Parallel()
	r := sharedRig(t, &fakeModel{})
	site, _ := r.s.Sites.Lookup(feedSite)
	type pair struct{ aud, sort string }
	var pairs []pair
	for _, aud := range []string{"all", "humans", "bots"} {
		for _, sortBy := range []string{"requests", "views"} {
			pairs = append(pairs, pair{aud, sortBy})
		}
	}
	seen := map[[2]string]bool{} // the visitors table has no comparison: one case per zone and range
	for i, c := range sourceCases("7d", "30d", "90d", "12mo") {
		if seen[[2]string{c.tz, c.rng}] {
			continue
		}
		seen[[2]string{c.tz, c.rng}] = true
		t.Run(c.tz+"_"+c.rng, func(t *testing.T) {
			t.Parallel()
			loc, _ := time.LoadLocation(c.tz)
			// Under -race every audience and order in UTC at 30d, one pair in each other case, in turn.
			ps := pairs
			if raceOn && (c.tz != "UTC" || c.rng != "30d") {
				ps = pairs[i%len(pairs) : i%len(pairs)+1]
			}
			for _, pr := range ps {
				table := func(r *rig, src Source) []any {
					q := &query{site: site, rng: c.rng, loc: loc, tz: c.tz, aud: pr.aud, cmp: "off",
						p: newPeriod(c.rng, "off", r.s.now(), loc)}
					rows, err := r.server(src).visitorRows(t.Context(), q, visitorCursor{From: ms(q.p.win.From),
						To: ms(q.p.Now)}, pr.sort, "", "")
					if err != nil {
						t.Fatal(err)
					}
					b, _ := json.Marshal(rows)
					var out []any
					_ = json.Unmarshal(b, &out)
					return out
				}
				raw := table(r, SourceRaw)
				var got []any
				n := countPieces(r, func(r *rig) { got = table(r, SourceAgg) })
				where := fmt.Sprint(c, pr)
				agree(t, "visitors "+where, raw, got, nil)
				if len(raw) == 0 || n.raw != 0 || (n.agg == 0 && c.rng != "7d") {
					t.Errorf("visitors %s: %d rows, pieces %+v", where, len(raw), n)
				}
			}
		})
	}
	both := r.both
	t.Run("answers", func(t *testing.T) {
		t.Parallel()
		limit := "50"
		if raceOn {
			limit = "7"
		}
		for _, params := range []map[string]string{p(feedSite, "range", "30d", "tz", "UTC", "limit", limit),
			p(feedSite, "range", "12mo", "tz", "UTC", "aud", "bots", "sort", "views", "limit", "7")} {
			raw, got := both(t, "visitors", params)
			agree(t, "visitors "+fmt.Sprint(params), raw, got, nil)
			if len(listRows(got)) == 0 {
				t.Errorf("visitors %v: no rows", params)
			}
		}
	})
	t.Run("details", func(t *testing.T) {
		t.Parallel()
		// The keys the tables list first (every audience's), under -race three of the whole table's.
		keys := map[string]bool{}
		auds := []string{"all", "humans", "bots"}
		if raceOn {
			auds = auds[:1]
		}
		for _, rng := range []string{"7d", "30d", "12mo"} {
			for _, aud := range auds {
				_, body := r.getFrom(t, SourceRaw, "visitors", p(feedSite, "range", rng, "tz", "UTC", "aud", aud,
					"limit", "3"))
				for _, row := range listRows(body) {
					keys[row["key"].(string)] = true
				}
			}
		}
		asked := 0
		for _, key := range slices.Sorted(maps.Keys(keys)) {
			if raceOn && asked == 3 {
				break
			}
			asked++
			for _, rng := range []string{"30d", "12mo"} {
				params := p(feedSite, "range", rng, "tz", "UTC", "key", key)
				raw, got := both(t, "visitor", params)
				agree(t, "visitor "+fmt.Sprint(params), raw, got, nil)
			}
		}
	})
}

// A session crossing midnight from an aggregate day into a raw one is one visit, in the table and the detail. One
// session and one address-day with rows of both parts (people and automated) count once per part under aud=all from
// the aggregates, once from raw: the aggregates keep the parts apart.
func TestSources_Sessions(t *testing.T) {
	r, insert := exactRig(t)
	view := func(ts int64, sid string, src string, cls int) store.Event {
		return store.Event{TS: ts, Source: src, Kind: "page_view", Visitor: "v1", SID: sid, IP: "192.0.2.40",
			IPKey: "192.0.2.40", Template: "/", Cls: cls}
	}
	insert(view(at("2026-11-12T23:50:00Z"), "s1", "browser", store.ClsHuman),
		view(at("2026-11-13T00:10:00Z"), "s1", "browser", store.ClsHuman),
		view(at("2026-11-13T10:00:00Z"), "s2", "browser", store.ClsHuman),
		view(at("2026-11-14T09:00:00Z"), "s3", "browser", store.ClsHuman),
		view(at("2026-11-14T09:01:00Z"), "s3", "server", store.ClsUnknown))
	r.build(t)
	site, _ := r.s.Sites.Lookup(feedSite)
	// The 13th is raw again (unbuilt), the 12th stays built.
	if _, err := r.s.DB.DB.Exec(`DELETE FROM agg_day WHERE site_id = ? AND day = ?`, site.ID,
		at("2026-11-13T00:00:00Z")/dayMS); err != nil {
		t.Fatal(err)
	}
	visitsVia := func(r *rig, src Source, aud string) (float64, float64) {
		t.Helper()
		_, v := r.getFrom(t, src, "visitors", p(feedSite, "range", "30d", "tz", "UTC", "aud", aud))
		var table float64
		for _, row := range listRows(v) {
			if row["key"] == "192.0.2.40" {
				table = num64(row["visits"])
			}
		}
		_, d := r.getFrom(t, src, "visitor", p(feedSite, "range", "30d", "tz", "UTC", "key", "192.0.2.40"))
		return table, num64(d["visits"])
	}
	visits := func(src Source, aud string) (float64, float64) { return visitsVia(r, src, aud) }
	var table float64
	n := countPieces(r, func(r *rig) { table, _ = visitsVia(r, SourceAuto, "humans") })
	if table != 3 || n.agg == 0 || n.raw == 0 {
		t.Errorf("humans across the raw boundary: %v visits, pieces %+v", table, n)
	}
	if table, _ := visits(SourceRaw, "all"); table != 3 {
		t.Errorf("raw, all: %v visits", table)
	}
	if table, _ := visits(SourceAuto, "all"); table != 4 {
		t.Errorf("aggregates, all: %v visits, want s3 once per part", table)
	}
	a, err := r.s.actorIn(t.Context(), &query{site: site, aud: "all", p: newPeriod("30d", "off", r.end, time.UTC)},
		"192.0.2.40", time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC), false)
	if err != nil || a.visits != 2 || a.views != 3 {
		t.Errorf("detail across the boundary: %+v, %v", a, err)
	}
}

// Where raw is gone (retention cut it), the sums still come from the aggregates, while the trail, the log and a
// reading reach back to detail_since only; the footer says since when figures go back.
func TestSources_RawSpan(t *testing.T) {
	t.Parallel()
	r := copyRig(t, &fakeModel{})
	_, before := r.get(t, "visitors", p(feedSite, "range", "30d", "tz", "UTC", "limit", "500"))
	cut := r.end.AddDate(0, 0, -5)
	if _, err := r.s.DB.PruneAge(t.Context(), cut); err != nil {
		t.Fatal(err)
	}
	since := cut.Truncate(24 * time.Hour)
	_, after := r.get(t, "visitors", p(feedSite, "range", "30d", "tz", "UTC", "limit", "500"))
	detailSince := after["detail_since"]
	if d, _ := time.Parse(time.RFC3339, detailSince.(string)); d.Before(since) {
		t.Errorf("detail_since %v before the cut %v", detailSince, since)
	}
	delete(before, "detail_since")
	delete(after, "detail_since")
	rows := func(b map[string]any) []map[string]any {
		out := listRows(b)
		for _, row := range out {
			delete(row, "does") // what it does reads the raw span
		}
		return out
	}
	agree(t, "visitors after the cut", rows(before), rows(after), nil)
	key := listRows(after)[0]["key"].(string)
	_, d := r.get(t, "visitor", p(feedSite, "range", "30d", "tz", "UTC", "key", key))
	for _, x := range d["trail"].([]any) {
		if ts, _ := time.Parse(time.RFC3339, x.(map[string]any)["from"].(string)); ts.Before(since) {
			t.Errorf("trail before detail_since: %v", x)
		}
	}
	if d["detail_since"] != detailSince || d["requests"] != listRows(after)[0]["requests"] {
		t.Errorf("detail %v %v", d["detail_since"], d["requests"])
	}
	_, l := r.get(t, "log", p(feedSite, "range", "30d", "tz", "UTC"))
	if l["detail_since"] != detailSince {
		t.Errorf("log detail_since %v", l["detail_since"])
	}
	_, dom := r.get(t, "domain", p(feedSite, "range", "30d", "tz", "UTC"))
	st, src := dom["storage"].(map[string]any), dom["sources"].(map[string]any)
	if st["oldest"] != detailSince || st["figures_since"] == nil ||
		st["figures_since"].(string) >= st["oldest"].(string) || src["first"] != st["figures_since"] ||
		num64(st["raw_days"]) < 750 {
		t.Errorf("storage %v, first %v", st, src["first"])
	}
}

// Cut to three entries a day, the aggregates still add up: rows and Other hold the raw total of a counted list, a
// key never collapsed has its raw figures and change, and a key that may hide in a day's Other has no change, never
// new. Four days of the history come again 30, 90 and 365 days earlier: the prior periods of 30d and 90d and the
// year before hold data, some of it collapsed.
func TestSources_ListsTopThree(t *testing.T) {
	t.Parallel()
	r := copyRig(t, nil)
	for _, tbl := range []string{"agg_day", "agg_hour", "agg_list", "agg_addr"} {
		if _, err := r.s.DB.DB.Exec(`DELETE FROM ` + tbl); err != nil { //nolint:gosec // the test's own names
			t.Fatal(err)
		}
	}
	from, to := r.end.AddDate(0, 0, -8).Truncate(24*time.Hour), r.end.AddDate(0, 0, -4).Truncate(24*time.Hour)
	for _, back := range []int{30, 90, 365} {
		if _, err := r.s.DB.DB.Exec(`INSERT INTO event (site_id, ts, source, kind, name, visitor, sid, ip, ip_key, ua,
			path, template, url, ref, ref_host, method, status, dur_ms, asn, country, account, cls, props)
			SELECT site_id, ts - ?, source, kind, name, visitor, sid, ip, ip_key, ua, path, template, url, ref, ref_host,
			method, status, dur_ms, asn, country, account, cls, props FROM event WHERE ts >= ? AND ts < ?`,
			int64(back)*dayMS, from.UnixMilli(), to.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := agg.BuildThrough(t.Context(), r.s.DB, r.end, 3); err != nil {
		t.Fatal(err)
	}
	site, _ := r.s.Sites.Lookup(feedSite)
	// The hashes of every key some day collapsed, by dimension.
	collapsed := map[string]map[uint64]bool{}
	rows, err := r.s.DB.DB.Query(`SELECT dim, keys FROM agg_list WHERE site_id = ? AND other = 1`, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	truncated := 0
	for rows.Next() {
		var dim string
		var keys []byte
		if err := rows.Scan(&dim, &keys); err != nil {
			t.Fatal(err)
		}
		if collapsed[dim] == nil {
			collapsed[dim] = map[uint64]bool{}
		}
		for _, h := range agg.OtherKeys(keys) {
			collapsed[dim][h] = true
			truncated++
		}
	}
	_ = rows.Close()
	if truncated == 0 {
		t.Fatal("nothing collapsed at three entries a day")
	}
	byKey := func(body map[string]any) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, row := range listRows(body) {
			out[row["key"].(string)] = row
		}
		return out
	}
	var priorOthers int
	if err := r.s.DB.DB.QueryRow(`SELECT COUNT(*) FROM agg_list WHERE site_id = ? AND other = 1 AND day < ?`,
		site.ID, r.end.AddDate(0, 0, -30).UnixMilli()/dayMS).Scan(&priorOthers); err != nil || priorOthers == 0 {
		t.Fatalf("no Other row in a prior period: %d, %v", priorOthers, err)
	}
	compared := 0 // changes from the aggregates against prior data
	combos := [][2]string{{"30d", "prev"}, {"90d", "prev"}, {"30d", "yoy"}, {"90d", "yoy"}}
	if raceOn { // a prior period and a year before, at both ranges
		combos = [][2]string{{"30d", "prev"}, {"90d", "yoy"}}
	}
	for _, rc := range combos {
		rng := rc[0] + " " + rc[1]
		for dim, d := range dims {
			if dim == "refused" {
				continue
			}
			params := p(feedSite, "range", rc[0], "cmp", rc[1], "tz", "UTC", "dim", dim, "limit", "500")
			raw, got := r.both(t, "list", params)
			rawRows, gotRows := byKey(raw), byKey(got)
			for k, g := range gotRows {
				w, ok := rawRows[k]
				cut := collapsed[dim][agg.Hash(k)]
				switch {
				case !ok:
					t.Errorf("%s %s: %s is no raw key", rng, dim, k)
				case !cut && (num64(g["value"]) != num64(w["value"]) || num64(g["second"]) != num64(w["second"])):
					t.Errorf("%s %s %s, never collapsed: %v, raw %v", rng, dim, k, g, w)
				case !cut && (changeOf(g["change"]) != changeOf(w["change"]) || g["verdict"] != w["verdict"]):
					t.Errorf("%s %s %s, never collapsed: change %v %v, raw %v %v", rng, dim, k, g["change"],
						g["verdict"], w["change"], w["verdict"])
				case cut && g["change"] != nil && changeOf(g["change"]) != changeOf(w["change"]):
					t.Errorf("%s %s %s, collapsed: change %v, raw %v", rng, dim, k, g["change"], w["change"])
				}
				if c, ok := g["change"].(map[string]any); ok && c["pct"] != nil {
					compared++
				}
				if changeOf(g["change"]) == `{"new":true}` && changeOf(w["change"]) != `{"new":true}` {
					t.Errorf("%s %s %s: new from the aggregates, raw %v", rng, dim, k, w["change"])
				}
			}
			total := func(body map[string]any) float64 {
				sum := num64(body["other"].(map[string]any)["value"])
				for _, row := range listRows(body) {
					sum += num64(row["value"])
				}
				return sum
			}
			if d.distinct == "" || d.value(1, 0, 0) == 1 { // value is the counted a
				if total(raw) != total(got) {
					t.Errorf("%s %s: rows and Other %v, raw %v", rng, dim, total(got), total(raw))
				}
			}
			// Every key, shown or only in an Other's listed hashes: raw's count (no day past the listing bound).
			if num64(got["total"]) != num64(raw["total"]) {
				t.Errorf("%s %s: total %v, raw %v", rng, dim, got["total"], raw["total"])
			}
		}
	}
	if compared == 0 {
		t.Error("no change from the aggregates compared prior data")
	}
	// No finding of a new key the aggregates cannot vouch for.
	for _, rc := range combos {
		rng := rc[0] + " " + rc[1]
		params := p(feedSite, "range", rc[0], "cmp", rc[1], "tz", "UTC", "findings", "all")
		raw, got := r.both(t, "noteworthy", params)
		rawIDs := map[string]bool{}
		for _, x := range raw["items"].([]any) {
			rawIDs[x.(map[string]any)["id"].(string)] = true
		}
		for _, x := range got["items"].([]any) {
			if id := x.(map[string]any)["id"].(string); !rawIDs[id] && !strings.HasPrefix(id, "trend:") {
				t.Errorf("%s: finding %s only from the aggregates", rng, id)
			}
		}
	}
}

// The source of each day: auto reads 24h and 7d raw, every clean day of a longer range from the aggregates, a dirty
// or unbuilt day raw, and nothing before the first event; SourceAgg fails on a day with raw but no clean build.
func TestSources_Plan(t *testing.T) {
	r, insert := exactRig(t)
	insert(pageLoad(at("2026-11-02T10:00:00Z"), "a", "192.0.2.1", "CH", "Mozilla/5.0", 0)...)
	insert(pageLoad(at("2026-11-12T10:00:00Z"), "b", "192.0.2.2", "CH", "Mozilla/5.0", 0)...)
	r.build(t)
	visitors := func(src Source, rng string) (float64, pieceCount) {
		t.Helper()
		var body map[string]any
		var code int
		n := countPieces(r, func(r *rig) {
			code, body = r.getFrom(t, src, "overview", p(feedSite, "range", rng, "tz", "UTC"))
		})
		if code != http.StatusOK {
			t.Fatalf("%v %s: %d %v", src, rng, code, body)
		}
		return num64(tiles(t, body)["visitors"]["value"]), n
	}
	if v, n := visitors(SourceAuto, "7d"); v != 0 || n.agg != 0 || n.raw == 0 {
		t.Errorf("auto 7d: %v visitors, pieces %+v", v, n)
	}
	v, n := visitors(SourceAuto, "30d")
	// The 21 built days from 2 to 22 November; 24 October to 1 November, before the first event, are not read.
	if v != 2 || n.agg != 21 || n.raw != 0 {
		t.Errorf("auto 30d: %v visitors, pieces %+v", v, n)
	}
	// A write to a built day shows at once, read raw, and again once rebuilt.
	insert(pageLoad(at("2026-11-12T11:00:00Z"), "c", "192.0.2.3", "CH", "Mozilla/5.0", 0)...)
	if v, n := visitors(SourceAuto, "30d"); v != 3 || n.raw != 1 {
		t.Errorf("after the write: %v visitors, pieces %+v", v, n)
	}
	if code, body := r.getFrom(t, SourceAgg, "overview", p(feedSite, "range", "30d", "tz", "UTC")); code != 500 {
		t.Errorf("SourceAgg on a dirty day: %d %v", code, body)
	}
	r.build(t)
	if v, n := visitors(SourceAuto, "30d"); v != 3 || n.raw != 0 {
		t.Errorf("after the rebuild: %v visitors, pieces %+v", v, n)
	}
	// A day whose raw retention cut (raw = 0) is read from its build, or as nothing when it has none.
	site, _ := r.s.Sites.Lookup(feedSite)
	if _, err := r.s.DB.DB.Exec(`UPDATE agg_day SET raw = 0 WHERE site_id = ? AND day = ?`, site.ID,
		at("2026-11-12T00:00:00Z")/dayMS); err != nil {
		t.Fatal(err)
	}
	if v, _ := visitors(SourceRaw, "30d"); v != 3 {
		t.Errorf("raw source: %v", v)
	}
	if _, err := r.s.DB.DB.Exec(`UPDATE agg_day SET frozen = 0 WHERE site_id = ? AND day = ?`, site.ID,
		at("2026-11-12T00:00:00Z")/dayMS); err != nil {
		t.Fatal(err)
	}
	if v, _ := visitors(SourceAuto, "30d"); v != 1 {
		t.Errorf("a cut day without a build: %v visitors, want only a's", v)
	}
	if v, _ := visitors(SourceAgg, "30d"); v != 1 {
		t.Errorf("SourceAgg on a cut day without a build: %v", v)
	}
}

// A rebuild under way (AggWriting between its first and last transaction) is not read: the statement's own snapshot
// sees the day out of reading and the piece goes raw.
func TestSources_RebuildWhileRead(t *testing.T) {
	r, insert := exactRig(t)
	insert(pageLoad(at("2026-11-12T10:00:00Z"), "a", "192.0.2.1", "CH", "Mozilla/5.0", 0)...)
	r.build(t)
	site, _ := r.s.Sites.Lookup(feedSite)
	day := at("2026-11-12T00:00:00Z") / dayMS
	q := &query{site: site, aud: "all", p: newPeriod("30d", "off", r.end, time.UTC)}
	pl, err := r.s.planOf(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	// The plan says built and clean; the job takes the day out and deletes its rows before the read.
	if _, err := r.s.DB.DB.Exec(`UPDATE agg_day SET dirty = ? WHERE site_id = ? AND day = ?`, store.AggWriting,
		site.ID, day); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.DB.DB.Exec(`DELETE FROM agg_hour WHERE site_id = ?`, site.ID); err != nil {
		t.Fatal(err)
	}
	b, err := r.s.bucket(t.Context(), pl, "all", time.UnixMilli(day*dayMS), time.UnixMilli((day+1)*dayMS), false)
	if err != nil || b.views != 1 || b.visitors.count() != 1 {
		t.Errorf("a day rebuilt while read: %+v, %v", b, err)
	}
	pl.src = SourceAgg
	if _, err := r.s.bucket(t.Context(), pl, "all", time.UnixMilli(day*dayMS), time.UnixMilli((day+1)*dayMS),
		false); !errors.Is(err, ErrNoAggregates) {
		t.Errorf("SourceAgg on a day rebuilt while read: %v", err)
	}
}

// uniq is exact while it holds ids only, and merges a stored sketch.
func TestUniq(t *testing.T) {
	var u uniq
	for i := range 100 {
		u.add(fmt.Sprint("v", i%60))
	}
	if u.count() != 60 || u.sk != nil {
		t.Errorf("exact: %d", u.count())
	}
	s := agg.NewSketch()
	for i := 40; i < 140; i++ {
		s.AddString(fmt.Sprint("v", i))
	}
	if err := u.addSketch(s.Marshal()); err != nil || u.count() != 140 {
		t.Errorf("with a sketch: %d, %v", u.count(), err)
	}
	var w uniq
	w.add("v1")
	w.merge(&u)
	if w.count() != 140 || w.sk == nil {
		t.Errorf("merged: %d", w.count())
	}
	if err := w.addSketch([]byte{9}); err == nil {
		t.Error("a bad sketch merged")
	}
}
