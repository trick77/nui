package dash

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/store"
)

func TestLocate(t *testing.T) {
	n := func(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: true} }
	none := sql.NullFloat64{}
	const d = "lens.example.com"
	for _, tt := range []struct {
		name, src string
		line, col sql.NullFloat64
		want      errorWhere
	}{
		{"Script error.", "", n(0), n(0), errorWhere{Origin: originWithheld}},
		{"script error", "", none, none, errorWhere{Origin: originWithheld}},
		// A message that only mentions it is no stand-in.
		{"Script error in widget", "", none, none, errorWhere{Origin: originUnknown}},
		{"TypeError: x", "https://lens.example.com/assets/a.js?token=1#f", n(3), n(7),
			errorWhere{Origin: originOwn, Place: "/assets/a.js:3:7"}},
		{"TypeError: x", "https://LENS.example.com", n(3), none, errorWhere{Origin: originOwn, Place: "/:3"}},
		{"TypeError: x", "blob:https://lens.example.com/0b1c", n(1), n(2), errorWhere{Origin: originOwn, Place: "/0b1c:1:2"}},
		{"TypeError: x", "https://cdn.example.net/w.js?k=1", n(1), n(9),
			errorWhere{Origin: originOther, Host: "cdn.example.net", Place: "cdn.example.net/w.js:1:9"}},
		{"TypeError: x", "safari-web-extension://AB-12/content.js", n(1), n(2),
			errorWhere{Origin: originExtension, Place: "safari-web-extension://AB-12/content.js:1:2"}},
		{"TypeError: x", "webkit-masked-url://hidden/", n(4), n(1),
			errorWhere{Origin: originExtension, Place: "webkit-masked-url://hidden/:4:1"}},
		{"Unhandled rejection: x", "", none, none, errorWhere{Origin: originUnknown}},
		// What no browser reports: no URL, a line that is none.
		{"TypeError: x", "%zz", n(3), n(7), errorWhere{Origin: originUnknown}},
		{"TypeError: x", "https://lens.example.com/a.js", n(1e20), n(7), errorWhere{Origin: originOwn, Place: "/a.js"}},
		{"TypeError: x", "https://lens.example.com/a.js", n(-3), n(7), errorWhere{Origin: originOwn, Place: "/a.js"}},
	} {
		if got := locate(d, tt.name, tt.src, tt.line, tt.col); got != tt.want {
			t.Errorf("%s %s: %+v, want %+v", tt.name, tt.src, got, tt.want)
		}
	}
}

func TestErrorDetail(t *testing.T) {
	r := newRig(t, 0, 0, nil)
	site, _ := r.s.Sites.Lookup(feedSite)
	const msg = "TypeError: broken"
	const iosUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 26_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/145.0 Mobile/15E148 Safari/605.1.15"
	day := 24 * time.Hour
	ev := func(ago time.Duration, kind, name, visitor, path, props string) store.Event {
		return store.Event{SiteID: site.ID, TS: r.end.Add(-ago).UnixMilli(), Source: "browser", Kind: kind, Name: name,
			Visitor: visitor, SID: "s-" + visitor, IP: "192.0.2.70", IPKey: "192.0.2.70", UA: iosUA, Path: path,
			Template: path, Cls: 1, Props: props}
	}
	at := func(src string, line int) string {
		return fmt.Sprintf(`{"p":{"msg":"m","src":%q,"line":%d,"col":7}}`, src, line)
	}
	evs := []store.Event{
		ev(time.Hour, "js_error", msg, "a", "/one", at("https://lens.example.com/a.js?secret=1", 3)),
		// The newest with a stack; what is no frame is left out.
		ev(2*time.Hour, "js_error", msg, "a", "/one", `{"p":{"src":"https://lens.example.com/a.js?secret=2","line":3,"col":7,
			"type":"TypeError","stack":["f@a.js:3:7",{"x":1},"","g@a.js:9:1"]}}`),
		// The same visitor on another day counts once.
		ev(2*day, "js_error", msg, "a", "/two", at("https://lens.example.com/a.js", 9)),
		ev(3*day, "js_error", msg, "b", "/one", at("chrome-extension://abc/c.js", 1)),
		// Another JSON type everywhere, and a number past int64: no location, no failure.
		ev(4*day, "js_error", msg, "c", "/one", `{"p":{"src":{"a":1},"line":"3","col":[1],"type":{},"stack":"x"}}`),
		ev(4*day, "js_error", msg, "c", "/one", `{"p":{"src":"https://lens.example.com/a.js","line":100000000000000000000}}`),
		// Not these: another message, the same one past the range, a page view.
		ev(time.Hour, "js_error", msg+" too", "z", "/one", "{}"),
		ev(20*day, "js_error", msg, "old", "/one", "{}"),
	}
	for i := range 10 {
		evs = append(evs, ev(time.Duration(i)*time.Hour, "page_view", "", fmt.Sprintf("v%d", i), "/one", "{}"),
			ev(5*day, "page_view", "", fmt.Sprintf("v%d", i), "/one", "{}"))
	}
	if err := r.s.DB.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	code, b := r.get(t, "error", p(feedSite, "range", "7d", "key", msg))
	if code != 200 {
		t.Fatalf("%d %v", code, b)
	}
	for k, want := range map[string]any{"count": 6.0, "visitors": 3.0, "sessions": 3.0, "viewers": 10.0, "share": 30.0,
		"last": iso(r.end.Add(-time.Hour)), "first": iso(r.end.Add(-4 * day))} {
		if b[k] != want {
			t.Errorf("%s = %v, want %v", k, b[k], want)
		}
	}
	rows := func(k string) []any { return b[k].(map[string]any)["rows"].([]any) }
	places := map[string][2]float64{}
	for _, x := range rows("places") {
		m := x.(map[string]any)
		w := m["where"].(map[string]any)
		places[w["origin"].(string)+" "+w["place"].(string)] = [2]float64{m["count"].(float64), m["visitors"].(float64)}
	}
	want := map[string][2]float64{"own /a.js:3:7": {2, 1}, "own /a.js:9:7": {1, 1}, "own /a.js": {1, 1},
		"extension chrome-extension://abc/c.js:1:7": {1, 1}, "unknown ": {1, 1}}
	if fmt.Sprint(places) != fmt.Sprint(want) {
		t.Errorf("places %v, want %v", places, want)
	}
	if pg := rows("pages")[0].(map[string]any); pg["key"] != "/one" || pg["count"] != 5.0 || pg["visitors"] != 3.0 {
		t.Errorf("pages %v", pg)
	}
	if c := rows("classes")[0].(map[string]any); c["key"] != "human" || c["count"] != 6.0 {
		t.Errorf("classes %v", c)
	}
	if len(b["recent"].([]any)) != 6 || b["recent"].([]any)[0].(map[string]any)["page"] != "/one" {
		t.Errorf("recent %v", b["recent"])
	}

	if b["type"] != "TypeError" || fmt.Sprint(b["stack"]) != "[f@a.js:3:7 g@a.js:9:1]" {
		t.Errorf("type %v, stack %v", b["type"], b["stack"])
	}

	// The log's row says the same; any other event's has neither.
	_, l := r.get(t, "log", p(feedSite, "range", "24h", "path", msg))
	found := 0
	for _, row := range listRows(l) {
		if row["what"] != "js_error "+msg {
			continue
		}
		found++
		w, _ := row["error"].(map[string]any)
		if w["origin"] != "own" || w["place"] != "/a.js:3:7" || row["page"] != "/one" {
			t.Errorf("log row %v", row)
		}
	}
	if found != 2 {
		t.Errorf("%d log rows of the error, want 2", found)
	}
	_, l = r.get(t, "log", p(feedSite, "range", "24h", "kind", "page_view"))
	if row := listRows(l)[0]; row["error"] != nil || row["page"] != "" {
		t.Errorf("a page view's row %v", row)
	}

	// No such error: an answer with nothing in it. No key: refused.
	if code, b := r.get(t, "error", p(feedSite, "range", "24h", "key", "nothing")); code != 200 || b["count"] != 0.0 ||
		b["first"] != nil || len(b["recent"].([]any)) != 0 {
		t.Errorf("unknown key: %d %v", code, b)
	}
	if code, _ := r.get(t, "error", p(feedSite, "range", "24h")); code != 400 {
		t.Errorf("no key: %d", code)
	}
}

// A hidden error is a finding from three visitors on, however often two get it; a readable one from the first.
func TestMaskedErrorFinding(t *testing.T) {
	r := newRig(t, 0, 0, nil)
	site, _ := r.s.Sites.Lookup(feedSite)
	insert := func(name, visitor string, n int) {
		t.Helper()
		evs := make([]store.Event, n)
		for i := range evs {
			evs[i] = store.Event{SiteID: site.ID, TS: r.end.Add(-2*time.Hour).UnixMilli() - int64(i)*1000,
				Source: "browser", Kind: "js_error", Name: name, Visitor: visitor, IP: "192.0.2.70",
				IPKey: "192.0.2.70", UA: "Mozilla/5.0", Template: "/", Path: "/", Cls: 1}
		}
		if err := r.s.DB.InsertEvents(t.Context(), evs); err != nil {
			t.Fatal(err)
		}
	}
	found := func() map[string]bool {
		t.Helper()
		code, b := r.get(t, "noteworthy", p(feedSite, "range", "24h", "findings", "all"))
		if code != 200 {
			t.Fatalf("%d %v", code, b)
		}
		out := map[string]bool{}
		for _, x := range b["items"].([]any) {
			out[x.(map[string]any)["id"].(string)] = true
		}
		return out
	}
	insert("Script error.", "a", 20)
	insert("Script error.", "b", 20)
	insert("TypeError: x", "a", 1)
	if f := found(); f["err:Script error."] || !f["err:TypeError: x"] {
		t.Errorf("two visitors: %v", f)
	}
	insert("Script error.", "c", 1)
	if f := found(); !f["err:Script error."] {
		t.Errorf("three visitors: %v", f)
	}
}

// jsError is one occurrence of a script error.
func jsError(ts int64, name, visitor string) store.Event {
	return store.Event{TS: ts, Source: "browser", Kind: "js_error", Name: name, Visitor: visitor, IP: "192.0.2.70",
		IPKey: "192.0.2.70", UA: "Mozilla/5.0", Template: "/", Path: "/", Cls: store.ClsHuman}
}

// errorFindings are the script errors Noteworthy names over 30 days.
func errorFindings(t *testing.T, r *rig, src Source) map[string]bool {
	t.Helper()
	code, b := r.getFrom(t, src, "noteworthy", p(feedSite, "range", "30d", "tz", "UTC", "findings", "all"))
	if code != 200 {
		t.Fatalf("%v: %d %v", src, code, b)
	}
	out := map[string]bool{}
	for _, x := range b["items"].([]any) {
		if k, ok := strings.CutPrefix(x.(map[string]any)["id"].(string), "err:"); ok {
			out[k] = true
		}
	}
	return out
}

// A hidden error too few visitors got to be a finding then is new once enough get it, from raw and from the sketches.
func TestMaskedErrorFinding_Before(t *testing.T) {
	r, insert := exactRig(t)
	const now = "2026-11-15T10:00:00Z"
	prev := []string{"2026-10-08T10:00:00Z", "2026-10-09T10:00:00Z", "2026-10-10T10:00:00Z"}
	// Four names the browser's stand-in has, and a readable one, each three visitors' now.
	for _, name := range []string{"Script error.", "Script error", "SCRIPT ERROR", "script error.", "TypeError: x"} {
		for i := range 3 {
			insert(jsError(at(now), name, fmt.Sprint("v", i)))
		}
	}
	for i := range prev { // one a day
		insert(jsError(at(prev[i]), "Script error.", "v0"), // one visitor every day
			jsError(at(prev[i]), "SCRIPT ERROR", fmt.Sprint("v", i%2)), // two
			jsError(at(prev[i]), "Script error", fmt.Sprint("v", i)))   // three: it was a finding then
	}
	insert(jsError(at(prev[0]), "TypeError: x", "v0"))
	r.build(t)
	want := map[string]bool{"Script error.": true, "SCRIPT ERROR": true, "script error.": true}
	for _, src := range []Source{SourceRaw, SourceAgg} {
		if got := errorFindings(t, r, src); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: %v, want %v", srcName[src], got, want)
		}
	}
}

// A hidden error that may sit in a prior day's Other is no finding: its visitors then are not known.
func TestMaskedErrorFinding_PriorOther(t *testing.T) {
	r, insert := exactRig(t)
	for i := range 3 {
		insert(jsError(at("2026-11-15T10:00:00Z"), "Script error.", fmt.Sprint("v", i)),
			jsError(at("2026-10-10T10:00:00Z"), "A louder one", fmt.Sprint("v", i)))
	}
	insert(jsError(at("2026-10-10T10:00:00Z"), "Script error.", "v0"))
	r.buildK(t, 1)
	for src, want := range map[Source]bool{SourceRaw: true, SourceAgg: false} {
		if got := errorFindings(t, r, src); got["Script error."] != want {
			t.Errorf("%s: %v, want it listed: %v", srcName[src], got, want)
		}
	}
}

// A hidden error that is no finding takes no readable one's place among the ten.
func TestMaskedErrorFinding_Place(t *testing.T) {
	r, insert := exactRig(t)
	ts := at("2026-11-15T10:00:00Z")
	for i := range 20 {
		insert(jsError(ts, "Script error.", fmt.Sprint("v", i%2)))
	}
	for i := range 10 {
		insert(jsError(ts, fmt.Sprint("TypeError: ", i), "v0"))
	}
	if got := errorFindings(t, r, SourceRaw); len(got) != 10 || !got["TypeError: 9"] || got["Script error."] {
		t.Errorf("%d: %v", len(got), got)
	}
}

// Every origin the occurrences have, the most frequent first, whatever the places' rows leave out.
func TestErrorOrigins(t *testing.T) {
	r, insert := exactRig(t)
	ts := at("2026-11-20T10:00:00Z")
	ev := func(src string, n int) {
		for range n {
			e := jsError(ts, "TypeError: x", "v0")
			e.Props = fmt.Sprintf(`{"p":{"src":%q,"line":1,"col":1}}`, src)
			insert(e)
		}
	}
	for i := range errorRows + 1 {
		ev(fmt.Sprintf("https://lens.example.com/%d.js", i), 2)
	}
	ev("chrome-extension://abc/c.js", 1)
	// The most frequent place is not the most frequent origin's.
	ev("https://cdn.example.net/w.js", 5)
	ev("", 1)
	_, b := r.get(t, "error", p(feedSite, "key", "TypeError: x"))
	if pl := b["places"].(map[string]any); len(pl["rows"].([]any)) != errorRows || pl["more"] != 4.0 {
		t.Errorf("places %v", pl)
	}
	if got := fmt.Sprint(b["origins"]); got != "[own other extension unknown]" {
		t.Errorf("origins %s", got)
	}
	if _, b := r.get(t, "error", p(feedSite, "key", "nothing")); fmt.Sprint(b["origins"]) != "[]" {
		t.Errorf("no occurrence: origins %v", b["origins"])
	}
}

// A breakdown shows its largest rows and says how many more there are.
func TestErrorTallyTop(t *testing.T) {
	tally := errorTally{}
	for i := range 9 {
		for range i + 1 {
			tally.add(fmt.Sprintf("k%d", i), "v")
		}
	}
	got := tally.top()
	if len(got.Rows) != errorRows || got.More != 3 || got.Rows[0].Key != "k8" || got.Rows[0].Visitors != 1 {
		t.Errorf("%d rows, %d more, first %+v", len(got.Rows), got.More, got.Rows[0])
	}
}
