package dash

import (
	"fmt"
	"math"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/agg"
	"github.com/trick77/trakk/backend/internal/store"
)

// CLS is read back by the factor the aggregates stored it with.
func TestExperienceCLSUnits(t *testing.T) {
	if _, u, ok := agg.VitalUnits("CLS", int64(agg.VitalsVersion), 1.0); !ok || u != clsUnits {
		t.Errorf("CLS 1.0 is %d units, read back as %d", u, clsUnits)
	}
}

// expEvent is a browser row of one visitor (its own address) on a page.
func expEvent(when, kind, name, tmpl, vid string, cls int, props string) store.Event {
	ip := "192.0.2." + fmt.Sprint(len(vid))
	return store.Event{TS: at(when), Source: "browser", Kind: kind, Name: name, Visitor: vid, SID: vid + "-s", IP: ip,
		IPKey: ip, Path: tmpl, Template: tmpl, Cls: cls, Props: props}
}

func vitalProps(name string, value float64) string {
	return fmt.Sprintf(`{"p":{"name":%q,"value":%v,"v":2}}`, name, value)
}

func within5(v any, want float64) bool {
	f, ok := v.(float64)
	return ok && math.Abs(f-want) <= want*0.05
}

// What people experienced, exactly: their visits, engaged time and vitals, never a bot's nor an old script's
// vital, whatever the audience asked; the same from raw and from the aggregates.
func TestExperienceFigures(t *testing.T) {
	t.Parallel()
	r, insert := exactRig(t)
	const day, prior = "2026-11-10T10:00:00Z", "2026-10-15T10:00:00Z"
	insert(
		expEvent(day, "session_start", "", "/a", "aa", store.ClsHuman, ""),
		expEvent(day, "page_view", "", "/a", "aa", store.ClsHuman, ""),
		expEvent(day, "page_view", "", "/a", "aa", store.ClsHuman, ""),
		expEvent(day, "engagement", "", "/a", "aa", store.ClsHuman, `{"p":{"ms":3000}}`),
		expEvent(day, "vital", "LCP", "/a", "aa", store.ClsHuman, vitalProps("LCP", 1000)),
		expEvent(day, "vital", "LCP", "/a", "aa", store.ClsHuman, vitalProps("LCP", 2000)),
		expEvent(day, "vital", "LCP", "/a", "aa", store.ClsHuman, vitalProps("LCP", 3000)),
		expEvent(day, "vital", "LCP", "/a", "aa", store.ClsHuman, vitalProps("LCP", 4000)),
		expEvent(day, "vital", "LCP", "/a", "aa", store.ClsHuman, `{"p":{"name":"LCP","value":9999}}`), // old script
		expEvent(day, "vital", "INP", "/a", "aa", store.ClsHuman, vitalProps("INP", 200)),
		expEvent(day, "vital", "CLS", "/a", "aa", store.ClsHuman, vitalProps("CLS", 0.1)),
		expEvent(day, "session_start", "", "/b", "bbb", store.ClsHuman, ""),
		expEvent(day, "page_view", "", "/b", "bbb", store.ClsHuman, ""),
		expEvent(day, "engagement", "", "/b", "bbb", store.ClsHuman, `{"p":{"ms":1000}}`),
		expEvent(day, "engagement", "", "/c", "bbb", store.ClsHuman, `{"p":{"ms":500}}`), // no view, no sample
		expEvent(day, "vital", "LCP", "/b", "bbb", store.ClsHuman, vitalProps("LCP", 500)),
		// A bot's visit counts nowhere.
		expEvent(day, "session_start", "", "/a", "botty", store.ClsBot, ""),
		expEvent(day, "page_view", "", "/a", "botty", store.ClsBot, ""),
		expEvent(day, "engagement", "", "/a", "botty", store.ClsBot, `{"p":{"ms":99999}}`),
		expEvent(day, "vital", "LCP", "/a", "botty", store.ClsBot, vitalProps("LCP", 50000)),
		// The prior 30 days: one visit, half the engaged time, a faster LCP.
		expEvent(prior, "session_start", "", "/a", "cccc", store.ClsHuman, ""),
		expEvent(prior, "engagement", "", "/a", "cccc", store.ClsHuman, `{"p":{"ms":1125}}`),
		expEvent(prior, "vital", "LCP", "/a", "cccc", store.ClsHuman, vitalProps("LCP", 1500)),
	)
	r.build(t)
	for _, src := range []Source{SourceRaw, SourceAgg, SourceAuto} {
		for _, aud := range []string{"all", "bots"} {
			where := fmt.Sprint(srcName[src], " ", aud)
			code, body := r.getFrom(t, src, "experience", p(feedSite, "range", "30d", "tz", "UTC", "aud", aud))
			if code != 200 {
				t.Fatalf("%s: %d %v", where, code, body)
			}
			vs := map[string]map[string]any{}
			for _, x := range body["vitals"].([]any) {
				vs[x.(map[string]any)["name"].(string)] = x.(map[string]any)
			}
			// 4500 ms over two visits; the prior period's 1125 over one.
			if num64(body["visits"]) != 2 || num64(body["engaged_ms"]) != 2250 || body["since"] != nil ||
				changeOf(body["engaged_change"]) != `{"pct":100}` {
				t.Errorf("%s: %v", where, body)
			}
			// Five samples, the fourth of them; up on the prior period's, which is bad.
			lcp := vs["LCP"]
			if num64(lcp["n"]) != 5 || !within5(lcp["p75"], 3000) || !within5(lcp["prev"], 1500) ||
				lcp["verdict"] != "bad" || num64(lcp["change"].(map[string]any)["pct"]) < 80 {
				t.Errorf("%s LCP %v", where, lcp)
			}
			if c := vs["CLS"]; num64(c["n"]) != 1 || !within5(c["p75"], 0.1) || c["prev"] != nil || c["change"] != nil ||
				c["verdict"] != "flat" {
				t.Errorf("%s CLS %v", where, c)
			}
			if i := vs["INP"]; num64(i["n"]) != 1 || !within5(i["p75"], 200) {
				t.Errorf("%s INP %v", where, i)
			}
			if f := vs["FCP"]; num64(f["n"]) != 0 || f["p75"] != nil || f["prev"] != nil || len(body["vitals"].([]any)) != 5 {
				t.Errorf("%s FCP %v", where, body["vitals"])
			}

			code, body = r.getFrom(t, src, "list", p(feedSite, "dim", "experience", "range", "30d", "tz", "UTC", "aud", aud))
			rows := listRows(body)
			if code != 200 || len(rows) != 2 || num64(body["total"]) != 2 || body["since"] != nil {
				t.Fatalf("%s list: %d %v", where, code, body)
			}
			a, b := rows[0], rows[1]
			if a["key"] != "/a" || num64(a["views"]) != 2 || num64(a["engaged"]) != 1500 || !within5(a["lcp"], 3000) ||
				num64(a["loads"]) != 4 || !within5(a["inp"], 200) || !within5(a["cls"], 0.1) {
				t.Errorf("%s /a %v", where, a)
			}
			if b["key"] != "/b" || num64(b["views"]) != 1 || num64(b["engaged"]) != 1000 || !within5(b["lcp"], 500) ||
				num64(b["loads"]) != 1 || b["inp"] != nil || b["cls"] != nil {
				t.Errorf("%s /b %v", where, b)
			}
		}
	}
	// The list's parameters: a limit, a search, and what it has not.
	_, body := r.get(t, "list", p(feedSite, "dim", "experience", "range", "30d", "tz", "UTC", "limit", "1"))
	if rows := listRows(body); len(rows) != 1 || num64(body["total"]) != 2 ||
		fmt.Sprint(body["other"]) != "map[count:1 value:1]" {
		t.Errorf("limit 1: %v", body)
	}
	_, body = r.get(t, "list", p(feedSite, "dim", "experience", "range", "30d", "tz", "UTC", "q", "B"))
	if rows := listRows(body); len(rows) != 1 || rows[0]["key"] != "/b" {
		t.Errorf("q=B: %v", body)
	}
	for _, bad := range [][]string{{"sort", "change"}, {"within", "/a"}, {"sub", "auto"}, {"cat", "ai"}, {"limit", "0"}} {
		if code, _ := r.get(t, "list", p(feedSite, append([]string{"dim", "experience"}, bad...)...)); code != 400 {
			t.Errorf("%v: %d", bad, code)
		}
	}
	// An API has no pages.
	api := apiRig(t, nil)
	for _, ask := range [][]string{{"experience"}, {"list", "dim", "experience"}} {
		if code, _ := api.get(t, ask[0], p(apiSite, ask[1:]...)); code != 404 {
			t.Errorf("%v of an API: %d", ask, code)
		}
	}
}

// A vital up from a p75 of 0 has no percentage but is worse; 0 against 0 is flat. From either source.
func TestExperienceUpFromZero(t *testing.T) {
	t.Parallel()
	r, insert := exactRig(t)
	const day, prior = "2026-11-10T10:00:00Z", "2026-10-15T10:00:00Z"
	insert(
		expEvent(day, "vital", "CLS", "/a", "aa", store.ClsHuman, vitalProps("CLS", 0.2)),
		expEvent(day, "vital", "INP", "/a", "aa", store.ClsHuman, vitalProps("INP", 0)),
		expEvent(prior, "vital", "CLS", "/a", "bb", store.ClsHuman, vitalProps("CLS", 0)),
		expEvent(prior, "vital", "INP", "/a", "bb", store.ClsHuman, vitalProps("INP", 0)),
	)
	r.build(t)
	for _, src := range []Source{SourceRaw, SourceAgg} {
		_, body := r.getFrom(t, src, "experience", p(feedSite, "range", "30d", "tz", "UTC"))
		vs := map[string]map[string]any{}
		for _, x := range body["vitals"].([]any) {
			vs[x.(map[string]any)["name"].(string)] = x.(map[string]any)
		}
		if c := vs["CLS"]; !within5(c["p75"], 0.2) || c["prev"] != float64(0) || c["change"] != nil || c["verdict"] != "bad" {
			t.Errorf("%s CLS %v", srcName[src], c)
		}
		if i := vs["INP"]; i["p75"] != float64(0) || i["prev"] != float64(0) || i["change"] != nil || i["verdict"] != "flat" {
			t.Errorf("%s INP %v", srcName[src], i)
		}
	}
}

// The figures read the same from the aggregates as from raw, with samples, in every zone and comparison; the
// visits are the people's Visits tile.
func TestExperienceSources(t *testing.T) {
	t.Parallel()
	r := sharedRig(t, nil)
	for _, c := range sourceCases("30d", "90d", "12mo") {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()
			for _, ask := range [][]string{{"experience"}, {"list", "dim", "experience", "limit", "500"}} {
				ps := p(feedSite, append([]string{"range", c.rng, "cmp", c.cmp, "tz", c.tz}, ask[1:]...)...)
				code, raw := r.getFrom(t, SourceRaw, ask[0], ps)
				var got map[string]any
				var gotCode int
				n := countPieces(r, func(r *rig) { gotCode, got = r.getFrom(t, SourceAgg, ask[0], ps) })
				if code != 200 || gotCode != 200 {
					t.Fatalf("%v: %d, %d %v", ask, code, gotCode, got)
				}
				agree(t, fmt.Sprint(ask, " ", c), raw, got, nil)
				if n.agg == 0 || n.raw != 0 {
					t.Errorf("%v %v: pieces %+v", ask, c, n)
				}
				if ask[0] == "experience" {
					for _, v := range raw["vitals"].([]any) {
						if m := v.(map[string]any); num64(m["n"]) == 0 || m["p75"] == nil {
							t.Errorf("%v: no samples of %v", c, m)
						}
					}
					_, ov := r.getFrom(t, SourceRaw, "overview", p(feedSite, "range", c.rng, "cmp", c.cmp, "tz", c.tz,
						"aud", "humans"))
					if v := num64(tiles(t, ov)["visits"]["value"]); v == 0 || num64(raw["visits"]) != v ||
						raw["engaged_ms"] == nil {
						t.Errorf("%v: %v visits, %v engaged, the tile %v", c, raw["visits"], raw["engaged_ms"], v)
					}
					continue
				}
				full := 0
				for _, row := range listRows(raw) {
					if row["engaged"] != nil && row["lcp"] != nil && row["inp"] != nil && row["cls"] != nil {
						full++
					}
				}
				if full == 0 {
					t.Errorf("%v: no page with every figure %v", c, raw)
				}
			}
		})
	}
}

// Over aggregates kept to three entries a day, a page in a list's Other has no figure of that list, never a part of
// it; every other figure is raw's.
func TestExperienceListTopThree(t *testing.T) {
	t.Parallel()
	skipRace(t)
	r := copyRig(t, nil)
	for _, tbl := range []string{"agg_day", "agg_hour", "agg_list", "agg_addr"} {
		if _, err := r.s.DB.DB.Exec(`DELETE FROM ` + tbl); err != nil { //nolint:gosec // the test's own names
			t.Fatal(err)
		}
	}
	if _, err := agg.BuildThrough(t.Context(), r.s.DB, r.end, 3); err != nil {
		t.Fatal(err)
	}
	site, _ := r.s.Sites.Lookup(feedSite)
	collapsed := map[string]map[uint64]bool{}
	rows, err := r.s.DB.DB.Query(`SELECT dim, keys FROM agg_list WHERE site_id = ? AND other = 1 AND part = 1`, site.ID)
	if err != nil {
		t.Fatal(err)
	}
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
		}
	}
	_ = rows.Close()
	params := p(feedSite, "dim", "experience", "range", "30d", "tz", "UTC", "limit", "500")
	raw, got := r.both(t, "list", params)
	rawRows := map[string]map[string]any{}
	for _, row := range listRows(raw) {
		rawRows[row["key"].(string)] = row
	}
	cells := map[string]string{"engaged": "engaged", "lcp": "vital_lcp", "loads": "vital_lcp", "inp": "vital_inp",
		"cls": "vital_cls"}
	left := 0
	for _, g := range listRows(got) {
		k := g["key"].(string)
		w, ok := rawRows[k]
		if !ok {
			t.Errorf("%s is no raw key", k)
			continue
		}
		if !collapsed["engaged"][agg.Hash(k)] && g["views"] != w["views"] {
			t.Errorf("%s, never collapsed: views %v, raw %v", k, g["views"], w["views"])
		}
		for field, dim := range cells {
			cut := collapsed[dim][agg.Hash(k)]
			switch {
			case g[field] == nil && cut:
				left++
			case fmt.Sprint(g[field]) != fmt.Sprint(w[field]):
				t.Errorf("%s %s: %v, raw %v (collapsed %v)", k, field, g[field], w[field], cut)
			}
		}
	}
	if left == 0 {
		t.Errorf("no figure of a page in Other left out: %v", got)
	}
	// Total and Other's count are an upper bound of raw's, with every row shown and with most past the limit.
	for _, limit := range []string{"500", "2"} {
		params["limit"] = limit
		raw, got := r.both(t, "list", params)
		ro, ao := raw["other"].(map[string]any), got["other"].(map[string]any)
		if num64(got["total"]) < num64(raw["total"]) || num64(ao["count"]) < num64(ro["count"]) {
			t.Errorf("limit %s: total %v, other %v; raw %v, %v", limit, got["total"], ao, raw["total"], ro)
		}
		if limit == "2" && num64(ro["count"]) == 0 {
			t.Errorf("limit 2: raw's other %v", ro)
		}
	}
	// The hours are never cut.
	raw, got = r.both(t, "experience", p(feedSite, "range", "30d", "tz", "UTC"))
	agree(t, "experience", raw, got, nil)
}

// Days an older builder made whose raw is gone hold none of these figures: they count from the cut on, the
// comparison over such days is none; every other figure of the dashboard stays as it was.
func TestExperienceSince(t *testing.T) {
	t.Parallel()
	r := copyRig(t, nil)
	r.s.Source = SourceAuto
	site, _ := r.s.Sites.Lookup(feedSite)
	_, ovBefore := r.get(t, "overview", p(feedSite, "range", "30d", "tz", "UTC"))
	_, pagesBefore := r.get(t, "list", p(feedSite, "dim", "pages", "range", "30d", "tz", "UTC", "limit", "50"))
	_, before := r.get(t, "experience", p(feedSite, "range", "7d", "tz", "UTC"))
	if lcp := before["vitals"].([]any)[0].(map[string]any); lcp["prev"] == nil || before["since"] != nil {
		t.Fatalf("before the cut: %v", before)
	}
	cut := rigEnd.Truncate(day1).Add(-3 * day1)
	for _, q := range []string{`UPDATE agg_day SET builder = 3, raw = 0 WHERE day < ?1`,
		`DELETE FROM event WHERE ts < ?1 * 86400000`,
		`UPDATE agg_hour SET engaged = 0, v_lcp = NULL, v_inp = NULL, v_cls = NULL, v_fcp = NULL, v_ttfb = NULL
			WHERE hour < ?1 * 24`,
		`DELETE FROM agg_list WHERE dim IN ('engaged', 'vital_lcp', 'vital_inp', 'vital_cls') AND day < ?1`} {
		if _, err := r.s.DB.DB.Exec(q, cut.UnixMilli()/dayMS); err != nil {
			t.Fatal(err)
		}
	}
	var visits int64
	if err := r.s.DB.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE site_id = ? AND kind = 'session_start' AND cls = 1`,
		site.ID).Scan(&visits); err != nil || visits == 0 {
		t.Fatalf("%d visits since the cut, %v", visits, err)
	}
	for _, rng := range []string{"7d", "30d"} {
		_, body := r.get(t, "experience", p(feedSite, "range", rng, "tz", "UTC"))
		if body["since"] != cut.Format(time.RFC3339) || num64(body["visits"]) != float64(visits) ||
			body["engaged_change"] != nil {
			t.Errorf("%s: %v, want since %s and %d visits", rng, body, cut.Format(time.RFC3339), visits)
		}
		for _, x := range body["vitals"].([]any) {
			if v := x.(map[string]any); v["prev"] != nil || v["change"] != nil {
				t.Errorf("%s: %v against a gap", rng, v)
			}
		}
		_, list := r.get(t, "list", p(feedSite, "dim", "experience", "range", rng, "tz", "UTC"))
		if list["since"] != cut.Format(time.RFC3339) || len(listRows(list)) == 0 {
			t.Errorf("%s list: %v", rng, list)
		}
	}
	_, ovAfter := r.get(t, "overview", p(feedSite, "range", "30d", "tz", "UTC"))
	for k, tl := range tiles(t, ovBefore) {
		if v := tiles(t, ovAfter)[k]["value"]; fmt.Sprint(v) != fmt.Sprint(tl["value"]) {
			t.Errorf("tile %s %v, was %v", k, v, tl["value"])
		}
	}
	if s := ovAfter["period"].(map[string]any)["since"]; s != nil {
		t.Errorf("overview since %v", s)
	}
	_, pagesAfter := r.get(t, "list", p(feedSite, "dim", "pages", "range", "30d", "tz", "UTC", "limit", "50"))
	if a, b := mustJSON(t, pagesAfter), mustJSON(t, pagesBefore); a != b {
		t.Errorf("pages moved:\n%s\nwas\n%s", a, b)
	}

	// One request's plan: the reads leave its need and gaps as they found them, for what the request reads next.
	q, err := r.s.parse(httptest.NewRequest("GET", "/api/dash/experience?site="+feedSite+"&range=30d&tz=UTC", nil))
	if err != nil {
		t.Fatal(err)
	}
	pl, err := r.s.planOf(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := r.s.experience(t.Context(), q)
	if err != nil || exp.Since == nil || pl.need != 0 || pl.gaps != 0 || pl.older != 0 {
		t.Errorf("since %v, plan need %d, gaps %d, older %d, %v", exp.Since, pl.need, pl.gaps, pl.older, err)
	}
	ov, err := r.s.overview(t.Context(), q)
	if err != nil || ov.Period.Since != nil {
		t.Errorf("the overview after it: since %v, %v", ov.Period.Since, err)
	}
}

// An older builder's day inside the range starts both reads over: the endpoint and the list count the same span,
// from since on, never what lies before it.
func TestExperienceSinceInside(t *testing.T) {
	t.Parallel()
	r := copyRig(t, nil)
	r.s.Source = SourceAuto
	site, _ := r.s.Sites.Lookup(feedSite)
	old := rigEnd.Truncate(day1).Add(-10 * day1)
	for _, q := range []string{`UPDATE agg_day SET builder = 3, raw = 0 WHERE day = ?1`,
		`DELETE FROM event WHERE ts >= ?1 * 86400000 AND ts < (?1 + 1) * 86400000`,
		`UPDATE agg_hour SET engaged = 0, v_lcp = NULL, v_inp = NULL, v_cls = NULL, v_fcp = NULL, v_ttfb = NULL
			WHERE hour >= ?1 * 24 AND hour < (?1 + 1) * 24`,
		`DELETE FROM agg_list WHERE dim IN ('engaged', 'vital_lcp', 'vital_inp', 'vital_cls') AND day = ?1`} {
		if _, err := r.s.DB.DB.Exec(q, old.UnixMilli()/dayMS); err != nil {
			t.Fatal(err)
		}
	}
	since := old.Add(day1)
	// People's visits and views from since on, and their views of the range's days before the older one.
	var visits, views, before int64
	if err := r.s.DB.DB.QueryRow(`SELECT COALESCE(SUM(kind = 'session_start' AND ts >= ?2), 0),
		COALESCE(SUM(kind = 'page_view' AND ts >= ?2), 0), COALESCE(SUM(kind = 'page_view' AND ts < ?2), 0)
		FROM event WHERE site_id = ?1 AND ts >= ?3 AND cls = 1`, site.ID, since.UnixMilli(),
		old.Add(-10*day1).UnixMilli()).Scan(&visits, &views, &before); err != nil || before == 0 || views == 0 {
		t.Fatalf("views %d, %d before the day, %v", views, before, err)
	}
	_, exp := r.get(t, "experience", p(feedSite, "range", "30d", "tz", "UTC"))
	_, list := r.get(t, "list", p(feedSite, "dim", "experience", "range", "30d", "tz", "UTC", "limit", "500"))
	want := since.Format(time.RFC3339)
	if exp["since"] != want || list["since"] != want || num64(exp["visits"]) != float64(visits) {
		t.Errorf("since %v and %v, want %s; visits %v, want %d", exp["since"], list["since"], want, exp["visits"], visits)
	}
	got := int64(num64(list["other"].(map[string]any)["value"]))
	for _, row := range listRows(list) {
		got += int64(num64(row["views"]))
	}
	if got != views {
		t.Errorf("list views %d, want %d from since on (%d before it)", got, views, before)
	}
}
