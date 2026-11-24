package classify

import (
	"fmt"
	"strings"
	"testing"

	"github.com/trick77/trakk/backend/internal/store"
)

const (
	chromeUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/142.0.0.0 Safari/537.36"
	phoneUA  = "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 Chrome/142.0.0.0 Mobile Safari/537.36"
	// A person's desktop Chrome: ctx members, and the headers of its beacon.
	plainCtx = `"webdriver":false,"wdd":"proto","auto":[],"synth":0,"touch":0,"outer":[1400,987],"viewport":[1400,900],` +
		`"plg":[5,true],"perm":["default","prompt"],"gl":["Google Inc. (Apple)","ANGLE (Apple, Apple M2)"],` +
		`"langs":["de-CH","en"],"uach":{"mobile":false,"platform":"macOS","brands":[["Chromium","142"]]},` +
		`"inputs":{"pointer":1,"key":0,"touch":0,"scroll":3,"move":40,"mdown":1}`
	plainHdr = `"accept-language":"de-CH,de;q=0.9","origin":"https://app.example.com","sec-fetch-mode":"cors",` +
		`"sec-ch-ua-platform":"\"macOS\""`
	silentInputs = `"inputs":{"pointer":0,"key":0,"touch":0,"scroll":0,"move":0,"mdown":0}`
)

// with replaces members of a JSON object's inside, each "key":value pair of repl by its key.
func with(t *testing.T, base string, repl ...string) string {
	t.Helper()
	for i := 0; i < len(repl); i += 2 {
		if !strings.Contains(base, repl[i]) {
			t.Fatalf("%q not in the base", repl[i])
		}
		base = strings.Replace(base, repl[i], repl[i+1], 1)
	}
	return base
}

func TestVisitFamilies(t *testing.T) {
	const none = family(0)
	for name, tt := range map[string]struct {
		ua, ctx, hdr string
		want         family
	}{
		"a person":                {chromeUA, plainCtx, plainHdr, none},
		"nothing asked":           {chromeUA, `"webdriver":false`, plainHdr, none},
		"a tool's global":         {chromeUA, with(t, plainCtx, `"auto":[]`, `"auto":["chromedriver"]`), plainHdr, famTools},
		"webdriver patched":       {chromeUA, with(t, plainCtx, `"wdd":"proto"`, `"wdd":"own"`), plainHdr, famTools},
		"webdriver deleted":       {chromeUA, with(t, plainCtx, `"wdd":"proto"`, `"wdd":"none"`), plainHdr, famTools},
		"webdriver no boolean":    {chromeUA, with(t, plainCtx, `"webdriver":false,`, ``), plainHdr, famTools},
		"swiftshader":             {chromeUA, with(t, plainCtx, `ANGLE (Apple, Apple M2)`, `ANGLE (Google, SwiftShader)`), plainHdr, famRendering},
		"no context":              {chromeUA, with(t, plainCtx, `["Google Inc. (Apple)","ANGLE (Apple, Apple M2)"]`, `["",""]`), plainHdr, famRendering},
		"gl not asked":            {chromeUA, with(t, plainCtx, `"gl":["Google Inc. (Apple)","ANGLE (Apple, Apple M2)"],`, ``), plainHdr, none},
		"no frame":                {chromeUA, with(t, plainCtx, `[1400,987]`, `[1400,900]`), plainHdr, famWindow},
		"no frame, touch":         {chromeUA, with(t, plainCtx, `[1400,987]`, `[1400,900]`, `"touch":0,"outer"`, `"touch":10,"outer"`), plainHdr, none},
		"no plugins":              {chromeUA, with(t, plainCtx, `[5,true]`, `[0,false]`), plainHdr, famWindow},
		"plugins hidden":          {chromeUA, with(t, plainCtx, `[5,true]`, `[0,true]`), plainHdr, none},
		"permission contradicted": {chromeUA, with(t, plainCtx, `["default","prompt"]`, `["denied","prompt"]`), plainHdr, famWindow},
		"a phone": {phoneUA, with(t, plainCtx, `[1400,987]`, `[1400,900]`, `[5,true]`, `[0,false]`, `macOS`, `Android`),
			with(t, plainHdr, `macOS`, `Android`), none},
		"another platform":      {chromeUA, with(t, plainCtx, `"platform":"macOS"`, `"platform":"Linux"`), plainHdr, famHeaders},
		"hints contradict":      {chromeUA, plainCtx, with(t, plainHdr, `macOS`, `Linux`), famHeaders},
		"Safari with hints":     {browserUA, plainCtx, plainHdr, famHeaders},
		"another language":      {chromeUA, with(t, plainCtx, `["de-CH","en"]`, `["fr"]`), plainHdr, famHeaders},
		"a region apart":        {chromeUA, with(t, plainCtx, `["de-CH","en"]`, `["de"]`), plainHdr, none},
		"no browser's headers":  {chromeUA, plainCtx, `"accept-language":"de"`, famHeaders},
		"no headers":            {chromeUA, plainCtx, ``, famHeaders},
		"dispatched input":      {chromeUA, with(t, plainCtx, `"synth":0`, `"synth":3`), plainHdr, famBehaviour},
		"values of other types": {chromeUA, `"auto":"x","wdd":7,"gl":{"a":1},"outer":"x","plg":[1],"perm":5,"langs":1,"uach":[],"synth":"9","inputs":3`, plainHdr, none},
		"nulls": {chromeUA, `"touch":0,"outer":[null,null],"viewport":[1400,900],"gl":[null,null],"perm":[null,null],` +
			`"plg":[null,null],"wdd":null,"auto":null,"synth":null,"langs":null`, plainHdr, none},
		// resistFingerprinting: the window as large as the page and no WebGL, from one setting.
		"a hardened Firefox": {"Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0",
			`"touch":0,"outer":[1400,900],"viewport":[1400,900],"gl":["",""],"plg":[5,true],"langs":["de"]`, plainHdr, famRendering},
		"a stealth setup": {chromeUA, with(t, plainCtx, `"wdd":"proto"`, `"wdd":"own"`,
			`ANGLE (Apple, Apple M2)`, `llvmpipe (LLVM 15)`), plainHdr, famTools | famRendering},
	} {
		v := newVisit()
		v.add("page_view", "/", `{"ctx":{`+tt.ctx+`},"hdr":{`+tt.hdr+`}}`)
		if got := v.families(tt.ua); got != tt.want {
			t.Errorf("%s: %b, want %b", name, got, tt.want)
		}
	}
	// The newest known value stands; one the newest batch did not ask for is an older one's.
	v := newVisit()
	v.add("page_view", "/", `{"ctx":{"wdd":"proto","webdriver":false,"gl":null},"hdr":{`+plainHdr+`}}`)
	v.add("page_view", "/", `{"ctx":{"wdd":"own","webdriver":false,"gl":["x","SwiftShader"],"auto":["selenium"]}}`)
	v.add("page_view", "/", `not json`)
	if got := v.families(chromeUA); got != famTools|famRendering {
		t.Errorf("newest known: %b", got)
	}
	// Engaged on three pages without input.
	silent := func(p string) { v.add("engagement", p, `{"p":{"ms":3000,`+silentInputs+`}}`) }
	v = newVisit()
	v.add("page_view", "/", `{"ctx":{"wdd":"proto","webdriver":false},"hdr":{`+plainHdr+`}}`)
	for i := range silentPages {
		if got := v.families(chromeUA); got != 0 {
			t.Errorf("after %d silent pages: %b", i, got)
		}
		silent(fmt.Sprintf("/%d", i))
	}
	if got := v.families(chromeUA); got != famBehaviour {
		t.Errorf("silent pages: %b", got)
	}
	v.add("engagement", "/z", engagedProps)
	if got := v.families(chromeUA); got != 0 {
		t.Errorf("input on one page: %b", got)
	}
}

func TestFamilyReason(t *testing.T) {
	for f, want := range map[family]string{
		0:                                    "",
		famTools:                             "",
		famRendering | famWindow:             "automation indicators: software rendering, window",
		famHeaders | famHosting:              "automation indicators: headers against the script, hosting network",
		famBehaviour | famTools | famHosting: "automation indicators: tool traces, behaviour, hosting network",
	} {
		if got := f.reason(); got != want {
			t.Errorf("%b: %q, want %q", f, got, want)
		}
	}
	if got := famHosting.more(famTools | famWindow); got != famTools|famWindow {
		t.Errorf("more %b", got)
	}
}

// The probes are reasons: no class changes by them, and two browsers of one user agent are not added up.
func TestPass_ProbesAreReasons(t *testing.T) {
	r := newRig(t, true)
	if err := r.st.AddStats(t.Context(), []store.IngestStat{{SiteID: r.site.ID, Hour: r.now.Unix() / 3600,
		Accepted: 5}}); err != nil {
		t.Fatal(err)
	}
	beacon := func(ip, visitor, ctx, hdr string) store.Event {
		return store.Event{Source: "browser", Kind: "page_view", IP: ip, UA: chromeUA, Visitor: visitor, Path: "/",
			Props: `{"ctx":{` + ctx + `},"hdr":{` + hdr + `}}`}
	}
	soft := with(t, plainCtx, `ANGLE (Apple, Apple M2)`, `SwiftShader`)
	tool := with(t, plainCtx, `"wdd":"proto"`, `"wdd":"own"`)
	evs := []store.Event{
		beacon("192.0.2.10", "a", plainCtx, plainHdr),
		beacon("192.0.2.11", "a", with(t, soft, `"wdd":"proto"`, `"wdd":"own"`), plainHdr),
		beacon("192.0.2.12", "a", soft, plainHdr),
		beacon("192.0.2.12", "b", tool, plainHdr),
		// A page's own property named webdriver, in the pass too.
		{Source: "browser", Kind: "custom", IP: "192.0.2.13", UA: chromeUA, Visitor: "a", Path: "/",
			Props: `{"p":{"webdriver":true},"ctx":{` + plainCtx + `},"hdr":{` + plainHdr + `}}`},
		beacon("192.0.2.14", "a", with(t, plainCtx, `"webdriver":false`, `"webdriver":true`), plainHdr),
		// Engaged without input: no good sign.
		beacon("192.0.2.15", "a", plainCtx, plainHdr),
		{Source: "browser", Kind: "engagement", IP: "192.0.2.15", UA: chromeUA, Visitor: "a", Path: "/",
			Props: `{"p":{"ms":3000,` + silentInputs + `}}`},
		// Counts of other types: no input, and the pass goes on.
		beacon("192.0.2.18", "a", plainCtx, plainHdr),
		{Source: "browser", Kind: "engagement", IP: "192.0.2.18", UA: chromeUA, Visitor: "a", Path: "/",
			Props: `{"p":{"ms":3000,"inputs":{"pointer":"9e999","key":"-9e999"}}}`},
		// A person on a remote desktop beside a program that makes most of the events: not the program's indicators.
		beacon("192.0.2.19", "a", with(t, soft, `[1400,987]`, `[1400,900]`), plainHdr),
	}
	for range 5 {
		evs = append(evs, req("192.0.2.19", "curl/8.4.0", "", "/", 200, ""))
	}
	// A program starting over: each browser new, one page, no input, a software renderer; and browsers whose first page
	// is still open, of which nothing is known yet.
	silent := with(t, soft, `"inputs":{"pointer":1,"key":0,"touch":0,"scroll":3,"move":40,"mdown":1}`, silentInputs)
	for i := range throwaway {
		v := fmt.Sprintf("v%d", i)
		evs = append(evs, beacon("192.0.2.16", v, silent, plainHdr), beacon("192.0.2.17", v, silent, plainHdr),
			store.Event{Source: "browser", Kind: "engagement", IP: "192.0.2.16", UA: chromeUA, Visitor: v, Path: "/",
				Props: `{"p":{"ms":3000,` + silentInputs + `}}`})
	}
	r.insert(t, evs...)
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	classes, err := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"192.0.2.10": "human|script beacon",
		"192.0.2.11": "human|script beacon,automation indicators: tool traces, software rendering",
		"192.0.2.12": "human|script beacon",
		"192.0.2.13": "human|script beacon",
		"192.0.2.14": "unknown|navigator.webdriver set",
		"192.0.2.15": "human|script beacon",
		"192.0.2.16": "human|script beacon,automation indicators: software rendering, behaviour",
		"192.0.2.17": "human|script beacon",
		"192.0.2.18": "human|script beacon",
		"192.0.2.19": "unknown|client curl/8.4.0",
	} {
		a := classes[key]
		if got := a.Class + "|" + strings.Join(a.Reasons, ","); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}
