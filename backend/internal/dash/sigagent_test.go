package dash

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/agg"
	"github.com/trick77/trakk/backend/internal/insight"
	"github.com/trick77/trakk/backend/internal/plancheck"
	"github.com/trick77/trakk/backend/internal/refdata"
	"github.com/trick77/trakk/backend/internal/sites"
	"github.com/trick77/trakk/backend/internal/store"
)

func TestSigAgent(t *testing.T) {
	t.Parallel()
	pad := strings.Repeat("a", 300)
	quotedLong, bareLong := `"https://example.com/`+pad+`"`, `https://example.com/`+pad
	longHost := `"https://` + strings.Repeat("a", 81) + `"`
	draft := `sig1="https://Signer.Example.com/.well-known/http-message-signatures-directory"`
	for raw, want := range map[string]string{
		// The draft's dictionary, the first member's; an older draft's string; a bare URL.
		draft: "signer.example.com",
		`sig1="https://a.example.com";x=1, sig2="https://b.example.com"`: "a.example.com",
		`"https://example.com"`:          "example.com",
		`  "https://example.com"  `:      "example.com",
		`"https://example.com";a=1`:      "example.com",
		`https://example.com/x?y=1`:      "example.com",
		`https://example.com, https://b`: "example.com",
		`HTTPS://EXAMPLE.COM`:            "example.com",
		`"https://ex\"ample.com"`:        "",
		`"https://example.com/\\\\"`:     "example.com",
		`"https://example.com\"`:         "",
		`"https://example.com" x`:        "",
		`"https://example.com`:           "", // never closed
		quotedLong:                       "", // clipped inside the string
		bareLong:                         "example.com",
		// Only an https URL's plain host.
		`"http://example.com"`:                "",
		`javascript:alert(1)`:                 "",
		`"javascript://example.com/%0aalert"`: "",
		`data:application/json,{}`:            "",
		`"https://user:pw@example.com"`:       "",
		`"https://example.com@evil.example"`:  "",
		`"https://example.com:8443/x"`:        "example.com",
		`"https://example.com./"`:             "example.com",
		`"https://example.com.."`:             "",
		`"https://.example.com"`:              "",
		`"https://a..b"`:                      "",
		`"https://[2001:db8::1]/"`:            "",
		`"https://192.0.2.1/"`:                "192.0.2.1",
		`"https://exämple.com"`:               "",
		`"https://xn--exmple-cua.com"`:        "xn--exmple-cua.com",
		"\"https://exa\x00mple.com\"":         "",
		"https://exa\tmple.com":               "",
		"https://example.com, \x01":           "",
		"https://example.com, ü":              "",
		"\"https://example.com\x7f\"":         "",
		`"https://ex_ample.com"`:              "",
		longHost:                              "",
		`"https:///x"`:                        "",
		`"https:x"`:                           "",
		``:                                    "",
		`sig1=`:                               "",
		`sig1=token`:                          "",
	} {
		if got := sigAgent(raw); got != want {
			t.Errorf("sigAgent(%q) = %q, want %q", raw, got, want)
		}
	}
}

// botNames is a bot list of one service whose agents differ in purpose, one agent named like its service, one purpose
// no code knows.
const botNames = `{"services":{
 "Anthropic":{"ipv4":["192.0.2.0/24"],"ip_list_authoritative":true,
   "user_agent_patterns":["*ClaudeBot*","*Claude-User*"],
   "agents":[{"name":"ClaudeBot","pattern":"*ClaudeBot*","purpose":"ai-training"},
     {"name":"Claude-User","pattern":"*Claude-User*","purpose":"ai-assistant"}]},
 "Searchy":{"ipv4":["198.51.100.0/24"],"ip_list_authoritative":true,"user_agent_patterns":["*Searchy*"],
   "agents":[{"name":"Searchy","pattern":"*Searchy*","purpose":"search-engine"}]},
 "Quirky":{"ipv4":["198.51.101.0/24"],"ip_list_authoritative":true,"user_agent_patterns":["*Quirky*"],
   "agents":[{"name":"Quirky","pattern":"*Quirky*","purpose":"quantum-crawl"}]}}}`

// servicesRig is a website with a feed whose known bots made six days of requests, the days built, the bot list
// botNames.
func servicesRig(t *testing.T, model insight.Model) *rig {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rows, err := st.SyncSites(t.Context(), []store.Site{
		{Domain: feedSite, Kind: "site", FeedKeyHash: sites.HashKey("k"), PathRules: "[]"},
		{Domain: plainSite, Kind: "site", PathRules: "[]"},
	})
	if err != nil {
		t.Fatal(err)
	}
	site := rows[feedSite].ID
	var evs []store.Event
	req := func(ago time.Duration, ip, svc string, cls, status int, props string) {
		evs = append(evs, store.Event{SiteID: site, TS: rigEnd.Add(-ago).UnixMilli(), Source: "server", Kind: "request",
			IP: ip, IPKey: ip, UA: svc + "/1.0", Path: "/x", Template: "GET /x", Method: "GET", Status: status, Cls: cls,
			Svc: svc, Props: props})
	}
	for d := range 6 {
		ago := time.Duration(d)*day1 + time.Hour
		for i := range 4 {
			req(ago+time.Duration(i)*time.Minute, "192.0.2.1", "ClaudeBot", store.ClsBot, 200,
				`{"sig_agent":"sig1=\"https://Anthropic.example.com/.well-known/http-message-signatures-directory\""}`)
		}
		req(ago, "192.0.2.2", "ClaudeBot", store.ClsBot, 404, `{"sig_agent":"\"https://other.example.com\""}`)
		req(ago, "192.0.2.3", "Claude-User", store.ClsBot, 200, `{}`)
		req(ago, "198.51.100.1", "Searchy", store.ClsBot, 200, `{"sig_agent":"javascript:alert(1)"}`)
		req(ago, "198.51.100.1", "Searchy", store.ClsBot, 403, `{}`)
		req(ago, "198.51.101.1", "Quirky", store.ClsMonitor, 200, `{}`)
		req(ago, "203.0.113.9", "Gone", store.ClsBot, 200, `{}`)
		req(ago, "203.0.113.10", "", store.ClsUnknown, 404, `{}`)
		// A bot's browser row carries its name and its headers.
		evs = append(evs, store.Event{SiteID: site, TS: rigEnd.Add(-ago).UnixMilli(), Source: "browser",
			Kind: "page_view", Visitor: "v1", IP: "192.0.2.1", IPKey: "192.0.2.1", UA: "ClaudeBot/1.0", Path: "/",
			Template: "/", Cls: store.ClsBot, Svc: "ClaudeBot",
			Props: `{"hdr":{"signature-agent":"sig1=\"https://other.example.com\""}}`})
	}
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	if _, err := agg.BuildThrough(t.Context(), st, rigEnd, agg.TopK); err != nil {
		t.Fatal(err)
	}
	r := rigOver(t, st, nil, model)
	bots, err := refdata.ParseBots([]byte(botNames), []byte(`{"services":{"UptimeRobot":{
		"ipv4":["69.162.124.224/28"],"ip_list_authoritative":true,"user_agent_patterns":["*UptimeRobot*"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	r.s.Ref = refdata.New(refdata.Options{Dir: t.TempDir()})
	r.s.Ref.SetBots(bots)
	return r
}

// The known bots by name, labelled from today's list, from either source alike.
func TestServicesList(t *testing.T) {
	t.Parallel()
	r := servicesRig(t, nil)
	for _, src := range []Source{SourceRaw, SourceAgg, SourceAuto} {
		r.s.Source = src
		code, body := r.get(t, "list", p(feedSite, "dim", "services", "range", "30d", "tz", "UTC", "limit", "50"))
		if code != 200 {
			t.Fatalf("%v: %d %v", src, code, body)
		}
		rows := rowsByKey(body)
		if len(rows) != 5 || body["since"] != nil {
			t.Fatalf("%v: %v", src, body)
		}
		cb := rows["ClaudeBot"]
		// An agent of a service named otherwise, its purpose only what it declares; its clients' claims, the most
		// frequent one's host.
		if num64(cb["value"]) != 30 || num64(cb["second"]) != 2 || cb["sub"] != "bot" || cb["category"] != "ai-training" ||
			cb["service"] != "Anthropic" || cb["declared"] != true || cb["says"] != "anthropic.example.com" {
			t.Errorf("%v ClaudeBot %v", src, cb)
		}
		// An agent named like its service; a claim that is no https URL is none.
		if s := rows["Searchy"]; s["category"] != "search-engine" || s["service"] != nil || s["declared"] != nil ||
			s["says"] != nil || num64(s["value"]) != 12 {
			t.Errorf("%v Searchy %v", src, s)
		}
		// A purpose no code knows, verbatim; a probe.
		if q := rows["Quirky"]; q["category"] != "quantum-crawl" || q["sub"] != "monitor" {
			t.Errorf("%v Quirky %v", src, q)
		}
		// A name the list no longer knows: no purpose.
		if g := rows["Gone"]; g == nil || g["category"] != nil || g["service"] != nil || g["declared"] != nil {
			t.Errorf("%v Gone %v", src, g)
		}
		if g := fmt.Sprint(body["groups"]); g != "[ai quantum search]" {
			t.Errorf("%v groups %s", src, g)
		}
	}
	// A group's rows, before the top-N; the shares stay of all rows, the groups all of them.
	r.s.Source = SourceAuto
	_, all := r.get(t, "list", p(feedSite, "dim", "services", "range", "30d", "tz", "UTC", "limit", "50"))
	_, ai := r.get(t, "list", p(feedSite, "dim", "services", "range", "30d", "tz", "UTC", "cat", "ai", "limit", "1"))
	rows := listRows(ai)
	if len(rows) != 1 || rows[0]["key"] != "ClaudeBot" || num64(ai["total"]) != 2 ||
		num64(ai["other"].(map[string]any)["count"]) != 1 || rows[0]["share"] != rowsByKey(all)["ClaudeBot"]["share"] ||
		fmt.Sprint(ai["groups"]) != "[ai quantum search]" {
		t.Errorf("cat=ai %v", ai)
	}
	if _, q := r.get(t, "list", p(feedSite, "dim", "services", "range", "30d", "cat", "quantum")); len(listRows(q)) != 1 {
		t.Errorf("cat=quantum %v", q)
	}
	if _, n := r.get(t, "list", p(feedSite, "dim", "services", "range", "30d", "cat", "nope")); listRows(n) != nil ||
		num64(n["total"]) != 0 {
		t.Errorf("cat=nope %v", n)
	}
	for _, bad := range [][]string{{"dim", "pages", "cat", "ai"}, {"dim", "services", "cat", "AI"},
		{"dim", "services", "cat", "ai-training"}, {"dim", "services", "cat", "a b"}} {
		if code, _ := r.get(t, "list", p(feedSite, bad...)); code != 400 {
			t.Errorf("%v: %d", bad, code)
		}
	}
	if code, _ := r.get(t, "list", p(plainSite, "dim", "services")); code != 404 {
		t.Errorf("services without a feed: %d", code)
	}
	// Other lists have no groups.
	if _, b := r.get(t, "list", p(feedSite, "dim", "probed", "range", "30d")); b["groups"] != nil {
		t.Errorf("probed groups %v", b["groups"])
	}
	// The log opens the events a name's row counts: its server rows, never its browser rows.
	for name, want := range map[string]float64{"ClaudeBot": 30, "Gone": 6, "nope": 0} {
		if _, b := r.get(t, "log", p(feedSite, "range", "30d", "service", name)); num64(b["total"]) != want {
			t.Errorf("log service=%s: %v, want %v", name, b["total"], want)
		}
	}
}

// Days an older builder made, their raw gone, hold no services: the list says from when on it has figures, and no
// other figure of the site moves.
func TestServicesSince(t *testing.T) {
	t.Parallel()
	r := servicesRig(t, nil)
	r.s.Source = SourceAuto
	others := []string{"probed", "refused", "networks"}
	before := map[string]map[string]any{}
	for _, dim := range others {
		_, before[dim] = r.get(t, "list", p(feedSite, "dim", dim, "range", "30d", "tz", "UTC", "limit", "50"))
	}
	_, ov := r.get(t, "overview", p(feedSite, "range", "30d", "tz", "UTC"))
	cut := rigEnd.Truncate(day1).Add(-3 * day1)
	for _, q := range []string{`UPDATE agg_day SET builder = 3, raw = 0 WHERE day < ?1`,
		`DELETE FROM event WHERE ts < ?1 * 86400000`, `DELETE FROM agg_list WHERE dim = 'services' AND day < ?1`} {
		if _, err := r.s.DB.DB.Exec(q, cut.UnixMilli()/dayMS); err != nil {
			t.Fatal(err)
		}
	}
	_, body := r.get(t, "list", p(feedSite, "dim", "services", "range", "30d", "tz", "UTC", "limit", "50"))
	if body["since"] != cut.Format(time.RFC3339) {
		t.Errorf("since %v, want %s", body["since"], cut.Format(time.RFC3339))
	}
	// The four days' requests from the cut on, no change against a gap.
	if cb := rowsByKey(body)["ClaudeBot"]; num64(cb["value"]) != 20 || cb["change"] != nil {
		t.Errorf("ClaudeBot %v", cb)
	}
	for _, dim := range others {
		_, after := r.get(t, "list", p(feedSite, "dim", dim, "range", "30d", "tz", "UTC", "limit", "50"))
		a, b := mustJSON(t, after), mustJSON(t, before[dim])
		if a != b || after["since"] != nil {
			t.Errorf("%s moved:\n%s\nwas\n%s", dim, a, b)
		}
	}
	_, ov2 := r.get(t, "overview", p(feedSite, "range", "30d", "tz", "UTC"))
	for k, tl := range tiles(t, ov) {
		if v := tiles(t, ov2)[k]["value"]; fmt.Sprint(v) != fmt.Sprint(tl["value"]) {
			t.Errorf("tile %s %v, was %v", k, v, tl["value"])
		}
	}
	if s := ov2["period"].(map[string]any)["since"]; s != nil {
		t.Errorf("overview since %v", s)
	}
}

// A day an older builder made inside the range starts the figures over: what lies before it is in no row, as since
// says.
func TestServicesSinceDrops(t *testing.T) {
	t.Parallel()
	r := servicesRig(t, nil)
	r.s.Source = SourceAuto
	old := rigEnd.Truncate(day1).Add(-3 * day1)
	for _, q := range []string{`UPDATE agg_day SET builder = 3, raw = 0 WHERE day = ?1`,
		`DELETE FROM event WHERE ts >= ?1 * 86400000 AND ts < (?1 + 1) * 86400000`,
		`DELETE FROM agg_list WHERE dim = 'services' AND day = ?1`} {
		if _, err := r.s.DB.DB.Exec(q, old.UnixMilli()/dayMS); err != nil {
			t.Fatal(err)
		}
	}
	_, body := r.get(t, "list", p(feedSite, "dim", "services", "range", "30d", "tz", "UTC", "limit", "50"))
	if want := old.Add(day1).Format(time.RFC3339); body["since"] != want {
		t.Errorf("since %v, want %s", body["since"], want)
	}
	// The three days after it, never the two built before it.
	if cb := rowsByKey(body)["ClaudeBot"]; num64(cb["value"]) != 15 || cb["change"] != nil {
		t.Errorf("ClaudeBot %v", cb)
	}
}

// A comparison over days an older builder made has no change, and the list says from when on the comparison has
// figures.
func TestServicesPrevSince(t *testing.T) {
	t.Parallel()
	r := servicesRig(t, nil)
	r.s.Source = SourceAuto
	ask := p(feedSite, "dim", "services", "range", "7d", "tz", "UTC", "limit", "50")
	_, before := r.get(t, "list", ask)
	if cb := rowsByKey(before)["ClaudeBot"]; cb["change"] == nil || before["prev_since"] != nil {
		t.Fatalf("before: %v", before)
	}
	// A known bot's change is no verdict either way.
	for k, row := range rowsByKey(before) {
		if row["change"] != nil && row["verdict"] != "flat" {
			t.Errorf("%s verdict %v", k, row["verdict"])
		}
	}
	// 7d's closed part is the 16th to the 21st, compared with the 9th to the 14th.
	site, _ := r.s.Sites.Lookup(feedSite)
	from := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	for d := range 6 {
		if _, err := r.s.DB.DB.Exec(`INSERT INTO agg_day (site_id, day, frozen, builder, raw) VALUES (?, ?, 1, 3, 0)`,
			site.ID, from.Add(time.Duration(d)*day1).UnixMilli()/dayMS); err != nil {
			t.Fatal(err)
		}
	}
	_, after := r.get(t, "list", ask)
	if after["since"] != nil || after["prev_since"] != from.Add(6*day1).Format(time.RFC3339) {
		t.Errorf("since %v, prev_since %v", after["since"], after["prev_since"])
	}
	for k, row := range rowsByKey(after) {
		if row["change"] != nil || num64(row["value"]) != num64(rowsByKey(before)[k]["value"]) {
			t.Errorf("%s %v, was %v", k, row, rowsByKey(before)[k])
		}
	}
}

// A known bot's rise and fall are no verdict either way.
func TestServicesChangeFlat(t *testing.T) {
	t.Parallel()
	r := servicesRig(t, nil)
	r.s.Source = SourceRaw
	// 7d's closed part (the 17th to the 21st here) holds 25 of ClaudeBot's requests and 10 of Searchy's; the 9th to
	// the 14th 10 and 20.
	site, _ := r.s.Sites.Lookup(feedSite)
	ts := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC).UnixMilli()
	var evs []store.Event
	for svc, n := range map[string]int{"ClaudeBot": 10, "Searchy": 20} {
		for range n {
			evs = append(evs, store.Event{SiteID: site.ID, TS: ts, Source: "server", Kind: "request", IP: "192.0.2.9",
				IPKey: "192.0.2.9", UA: svc + "/1.0", Path: "/x", Template: "GET /x", Method: "GET", Status: 200,
				Cls: store.ClsBot, Svc: svc, Props: `{}`})
		}
	}
	if err := r.s.DB.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	_, body := r.get(t, "list", p(feedSite, "dim", "services", "range", "7d", "tz", "UTC", "limit", "50"))
	for k, want := range map[string]float64{"ClaudeBot": 150, "Searchy": -50} {
		row := rowsByKey(body)[k]
		if c, _ := row["change"].(map[string]any); c["pct"] != want || row["verdict"] != "flat" {
			t.Errorf("%s %v", k, row)
		}
	}
}

// A service's own row, its agents' purposes differing within one group, is that group's; a search also finds a
// row by its service and its category.
func TestServicesGroupSearch(t *testing.T) {
	t.Parallel()
	r := servicesRig(t, nil)
	r.s.Source = SourceRaw
	site, _ := r.s.Sites.Lookup(feedSite)
	if err := r.s.DB.InsertEvents(t.Context(), []store.Event{{SiteID: site.ID, TS: rigEnd.Add(-time.Hour).UnixMilli(),
		Source: "server", Kind: "request", IP: "192.0.2.9", IPKey: "192.0.2.9", UA: "Anthropic/1.0", Path: "/x",
		Template: "GET /x", Method: "GET", Status: 200, Cls: store.ClsBot, Svc: "Anthropic", Props: `{}`}}); err != nil {
		t.Fatal(err)
	}
	list := func(kv ...string) map[string]any {
		_, b := r.get(t, "list", p(feedSite, append([]string{"dim", "services", "range", "30d", "tz", "UTC", "limit",
			"50"}, kv...)...))
		return b
	}
	keys := func(b map[string]any) string {
		var ks []string
		for _, row := range listRows(b) {
			ks = append(ks, row["key"].(string))
		}
		slices.Sort(ks)
		return fmt.Sprint(ks)
	}
	all := list()
	if a := rowsByKey(all)["Anthropic"]; a == nil || a["category"] != nil || a["declared"] != nil || a["group"] != nil {
		t.Errorf("Anthropic %v", a)
	}
	if g := fmt.Sprint(all["groups"]); g != "[ai quantum search]" {
		t.Errorf("groups %s", g)
	}
	for _, c := range []struct {
		kv   []string
		want string
	}{
		{[]string{"cat", "ai"}, "[Anthropic Claude-User ClaudeBot]"},
		// By the service an agent belongs to, by the category's words.
		{[]string{"q", "anthropic"}, "[Anthropic Claude-User ClaudeBot]"},
		{[]string{"q", "training"}, "[ClaudeBot]"},
		{[]string{"q", "QUANTUM"}, "[Quirky]"},
		{[]string{"q", "search-engine"}, "[Searchy]"},
		{[]string{"q", "nothing"}, "[]"},
	} {
		if got := keys(list(c.kv...)); got != c.want {
			t.Errorf("%v: %s, want %s", c.kv, got, c.want)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// What a client says it is, shown on its detail, never reaches the model or its signals.
func TestVisitorSaysNotRead(t *testing.T) {
	t.Parallel()
	model := &fakeModel{}
	r := servicesRig(t, model)
	code, body := r.get(t, "visitor", p(feedSite, "key", "192.0.2.1", "range", "7d", "tz", "UTC"))
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	if got := fmt.Sprint(body["says"]); got != "[anthropic.example.com other.example.com]" {
		t.Errorf("says %s", got)
	}
	if strings.Contains(fmt.Sprint(body["signals"]), "example") {
		t.Errorf("signals %v", body["signals"])
	}
	model.mu.Lock()
	ins := slices.Clone(model.readIns)
	model.mu.Unlock()
	if len(ins) == 0 {
		t.Fatal("no reading asked")
	}
	for _, in := range ins {
		if b := mustJSON(t, in); strings.Contains(b, "example") || slices.ContainsFunc(in.Signals, func(s string) bool {
			return strings.Contains(strings.ToLower(s), "signature")
		}) {
			t.Errorf("reading input %s", b)
		}
	}
	// No claim: an empty list.
	if _, body := r.get(t, "visitor", p(feedSite, "key", "198.51.100.1", "range", "7d")); fmt.Sprint(body["says"]) != "[]" {
		t.Errorf("says %v", body["says"])
	}
}

// The claims' statements: a day each, on an index, the newest days only.
func TestSaysPlans(t *testing.T) {
	r := servicesRig(t, nil)
	seen := map[string][][]any{}
	traceSQL = func(q string, args []any) {
		if strings.Contains(q, "signature-agent") {
			seen[q] = append(seen[q], append([]any(nil), args...))
		}
	}
	t.Cleanup(func() { traceSQL = nil })
	_, list := r.get(t, "list", p(feedSite, "dim", "services", "range", "12mo", "limit", "2"))
	r.get(t, "visitor", p(feedSite, "key", "192.0.2.1", "range", "12mo"))
	traceSQL = nil
	if len(seen) != 2 {
		t.Fatalf("%d statements", len(seen))
	}
	var shown []any
	for _, row := range listRows(list) {
		shown = append(shown, row["key"])
	}
	named := 0
	for q, calls := range seen {
		// Only the rows shown, only props that mention a claim parsed.
		if !strings.Contains(q, "instr(props, 'sig_agent')") {
			t.Errorf("no claim pre-filter: %s", strings.Join(strings.Fields(q), " "))
		}
		if strings.Contains(q, "svc IN") {
			named++
			if len(shown) != 2 || fmt.Sprint(calls[0][1:len(calls[0])-2]) != fmt.Sprint(shown) {
				t.Errorf("names read %v, rows %v", calls[0], shown)
			}
		}
		if len(calls) > sayDays {
			t.Errorf("%d reads: %s", len(calls), strings.Join(strings.Fields(q), " "))
		}
		for _, args := range calls {
			plan, err := plancheck.Explain(t.Context(), r.s.DB.DB, q, args...)
			if err != nil {
				t.Fatal(err)
			}
			if p := plancheck.Problem(q, plan); p != "" {
				t.Errorf("%s: %s", p, strings.Join(strings.Fields(q), " "))
			}
			if lo, hi := args[len(args)-2].(int64), args[len(args)-1].(int64); hi-lo > dayMS {
				t.Errorf("a statement over %d ms: %s", hi-lo, strings.Join(strings.Fields(q), " "))
			}
		}
	}
	if named != 1 {
		t.Errorf("%d statements read by the rows' names, want the services list's", named)
	}
}
