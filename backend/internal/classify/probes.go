package classify

import (
	"context"
	"encoding/json"
	"math/bits"
	"slices"
	"strings"
)

// The script's automation probes, read as reasons only: a browser showing indicators of two families is marked, its
// class stands. Each indicator alone fits some person (remote desktops render in software, a full-screen window has no
// frame, password managers dispatch input, a VPN leaves from a hosting network), and what real traffic shows of each is
// unknown until the reason has been seen on it.

// family is a set of independent kinds of indicator, one bit each, in the order a reason names them.
type family uint8

const (
	famTools     family = 1 << iota // what an automation tool leaves, or a patch that hides it
	famRendering                    // a software renderer, or none
	famWindow                       // no window frame, no plugins, permissions that contradict themselves
	famHeaders                      // the request's headers against what the script read
	famBehaviour                    // input a script made, none over several pages, or a page activated without any
	famHosting                      // a hosting network
)

var familyName = []string{"tool traces", "software rendering", "window", "headers against the script", "behaviour",
	"hosting network"}

// reasonIndicators starts the reason naming an actor's families.
const reasonIndicators = "automation indicators: "

func (f family) count() int { return bits.OnesCount8(uint8(f)) }

// reason names the families, "" for fewer than two.
func (f family) reason() string {
	if f.count() < 2 {
		return ""
	}
	var names []string
	for i, n := range familyName {
		if f&(1<<i) != 0 {
			names = append(names, n)
		}
	}
	return reasonIndicators + strings.Join(names, ", ")
}

// more is the set with more families, the same whatever the order.
func (f family) more(o family) family {
	if o.count() > f.count() || o.count() == f.count() && o > f {
		return o
	}
	return f
}

const (
	// maxProbeRows bounds the rows read per actor-day, the newest: a context per batch and the engagements.
	maxProbeRows = 1000
	// silentPages is how many pages a browser is engaged on without any input to count as an indicator.
	silentPages = 3
	// throwaway is how many visitors of one user agent, each seen on one page without input, and no others, count as
	// one program starting over. A choice, not measured.
	throwaway = 5
)

// softwareGL are the renderers of a machine without a GPU.
var softwareGL = []string{"swiftshader", "llvmpipe", "softpipe", "microsoft basic render"}

// visit is what one visitor's contexts and engagements say, newest first: the newest known value of each.
type visit struct {
	ctx   map[string]json.RawMessage // the newest value per key, never null
	hdr   map[string]string          // the newest batch's headers
	auto  bool
	synth bool
	left  bool // a page was left: its engagement says what input it saw, a page still open not yet
	input bool
	// scrolled without input known: a screen reader's gestures scroll so, a script too. Neither input nor silence.
	scrolled bool
	act      bool // a page was activated
	active   int64
	pages    map[string]bool
	silent   map[string]bool // the pages engaged on without input
}

func newVisit() *visit {
	return &visit{ctx: map[string]json.RawMessage{}, pages: map[string]bool{}, silent: map[string]bool{}}
}

// inputCounts are the script's counts, per page or per document. Synth is the part of pointer, key and touch a script
// dispatched. Without the wheel (a script before probe version 2) scroll counts as input, though a script scrolls too: a
// person reading by the wheel alone must not read as none.
type inputCounts struct {
	Pointer, Key, Touch, Scroll, Move, Synth float64
	Wheel                                    *float64
}

func (n inputCounts) any() bool {
	if n.Pointer+n.Key+n.Touch-n.Synth > 0 || n.Move > 0 {
		return true
	}
	if n.Wheel != nil {
		return *n.Wheel > 0
	}
	return n.Scroll > 0
}

// readProbes sets each client's indicators (uaDay.probes): its visitor's with most families.
func (c *Classifier) readProbes(ctx context.Context, site int64, m actorKey, uas []uaDay) error {
	if !slices.ContainsFunc(uas, func(g uaDay) bool { return g.browser > 0 }) {
		return nil
	}
	rows, err := c.DB.DB.QueryContext(ctx, `SELECT ua, visitor, kind, path, props FROM event
		WHERE site_id = ? AND ip_key = ? AND ts >= ? AND ts < ? AND source = 'browser'
		AND (kind = 'engagement' OR props LIKE '%"ctx":{%') ORDER BY ts DESC, id DESC LIMIT ?`,
		site, m.key, m.day*dayMS, (m.day+1)*dayMS, maxProbeRows)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	visits := map[string]map[string]*visit{} // user agent → visitor
	for rows.Next() {
		var ua, visitor, kind, path, props string
		if err := rows.Scan(&ua, &visitor, &kind, &path, &props); err != nil {
			return err
		}
		if visits[ua] == nil {
			visits[ua] = map[string]*visit{}
		}
		v := visits[ua][visitor]
		if v == nil {
			v = newVisit()
			visits[ua][visitor] = v
		}
		v.add(kind, path, props)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i, g := range uas {
		vs := visits[g.ua]
		once := 0
		for _, v := range vs {
			uas[i].probes = uas[i].probes.more(v.families(g.ua))
			uas[i].active += v.active
			if v.left && !v.input && !v.scrolled && len(v.pages) == 1 {
				once++
			}
		}
		if once >= throwaway && once == len(vs) {
			uas[i].probes |= famBehaviour
		}
	}
	return nil
}

// add takes a row, newest first. The props are a caller's: every value is read on its own, one of another type is as if
// it were not sent.
func (v *visit) add(kind, path, props string) {
	var p struct {
		P   json.RawMessage            `json:"p"`
		Ctx map[string]json.RawMessage `json:"ctx"`
		Hdr map[string]string          `json:"hdr"`
	}
	if json.Unmarshal([]byte(props), &p) != nil {
		return
	}
	v.pages[path] = true
	if kind == "engagement" {
		var e struct {
			MS     float64      `json:"ms"`
			Inputs *inputCounts `json:"inputs"`
		}
		if json.Unmarshal(p.P, &e) == nil && e.Inputs != nil {
			v.left = true
			switch {
			case e.Inputs.any():
				v.input = true
				v.active++
			case e.Inputs.Scroll > 0:
				v.scrolled = true
			case e.MS > 0:
				v.silent[path] = true
			}
		}
	}
	if p.Ctx == nil {
		return
	}
	if v.hdr == nil {
		v.hdr = p.Hdr
		if v.hdr == nil {
			v.hdr = map[string]string{}
		}
	}
	for k, raw := range p.Ctx {
		if _, ok := v.ctx[k]; !ok && string(raw) != "null" {
			v.ctx[k] = raw
		}
	}
	var in inputCounts
	if raw, ok := p.Ctx["inputs"]; ok && json.Unmarshal(raw, &in) == nil {
		v.input = v.input || in.any()
		v.scrolled = v.scrolled || in.Scroll > 0
	}
	// Marks of an earlier document stand: a tool once seen is seen.
	var auto []string
	v.auto = v.auto || json.Unmarshal(p.Ctx["auto"], &auto) == nil && len(auto) > 0
	var synth float64
	v.synth = v.synth || json.Unmarshal(p.Ctx["synth"], &synth) == nil && synth > 0
	var act bool
	v.act = v.act || json.Unmarshal(p.Ctx["act"], &act) == nil && act
}

// get decodes a context value into out; false where it was not sent or is of another type.
func (v *visit) get(key string, out any) bool {
	raw, ok := v.ctx[key]
	return ok && json.Unmarshal(raw, out) == nil
}

// pair is a context value of two, both known: a null among them decodes to a zero, which is a value of its own here.
func pair[T string | float64](v *visit, key string) (out [2]T, ok bool) {
	var l []*T
	if !v.get(key, &l) || len(l) != 2 || l[0] == nil || l[1] == nil {
		return out, false
	}
	return [2]T{*l[0], *l[1]}, true
}

// families are the indicators of one visitor's browser.
func (v *visit) families(ua string) family {
	var f family
	if len(v.ctx) == 0 {
		return 0
	}
	if v.auto || v.patched() {
		f |= famTools
	}
	if gl, ok := pair[string](v, "gl"); ok {
		ren := strings.ToLower(gl[1])
		if gl[0] == "" && gl[1] == "" || slices.ContainsFunc(softwareGL, func(s string) bool { return strings.Contains(ren, s) }) {
			f |= famRendering
		}
	}
	if v.window(ua) {
		f |= famWindow
	}
	if v.contradicts(ua) {
		f |= famHeaders
	}
	if v.synth || !v.input && len(v.silent) >= silentPages || v.driven() {
		f |= famBehaviour
	}
	return f
}

// driven: pages were activated and left without any input (Playwright's evaluate activates, measured; so does a press in
// an iframe or before the script loaded, on a page still open not told apart). Clicks a script made (sclick) say
// nothing alone: a page's own code clicks too.
func (v *visit) driven() bool { return v.act && v.left && !v.input && !v.scrolled }

// patched: navigator.webdriver is not where every current browser has it, or is there without a boolean.
func (v *visit) patched() bool {
	var wdd string
	if !v.get("wdd", &wdd) {
		return false
	}
	var b bool
	return wdd == "own" || wdd == "none" || wdd == "proto" && !v.get("webdriver", &b)
}

// window: a desktop browser without a window frame or plugins, or a notification permission the Permissions API
// contradicts, as headless Chrome answers. A touch screen says nothing: there the page is the window.
func (v *visit) window(ua string) bool {
	if perm, ok := pair[string](v, "perm"); ok && perm == [2]string{"denied", "prompt"} {
		return true
	}
	browser, _, device := ParseUA(ua)
	var touch float64
	if device != "Desktop" || !v.get("touch", &touch) || touch > 0 {
		return false
	}
	// Firefox's resistFingerprinting reports the page's size as the window's, with WebGL off beside it: one setting.
	outer, okO := pair[float64](v, "outer")
	if view, ok := pair[float64](v, "viewport"); ok && okO && browser != "Firefox" &&
		(outer == [2]float64{} || outer == view) {
		return true
	}
	var plg []any
	return v.get("plg", &plg) && len(plg) == 2 && plg[0] == float64(0) && plg[1] == false
}

// platformOS is the operating system ParseUA reads for a client hint's platform.
var platformOS = map[string]string{"Windows": "Windows", "macOS": "macOS", "Linux": "Linux", "Android": "Android",
	"Chrome OS": "ChromeOS", "Chromium OS": "ChromeOS"}

// contradicts: the request and the script tell of different browsers, or the request carried none of the headers every
// browser sends with a script's POST.
func (v *visit) contradicts(ua string) bool {
	if len(v.hdr) == 0 || v.hdr["origin"] == "" && v.hdr["sec-fetch-mode"] == "" && v.hdr["sec-fetch-site"] == "" {
		return true
	}
	browser, os, _ := ParseUA(ua)
	var uach struct {
		Platform string     `json:"platform"`
		Brands   [][]string `json:"brands"`
	}
	if v.get("uach", &uach) {
		// Client hints are Chromium's alone.
		if len(uach.Brands) > 0 && (browser == "Safari" || browser == "Firefox") {
			return true
		}
		if want, ok := platformOS[uach.Platform]; ok && os != "Other" && os != want {
			return true
		}
		if h := strings.Trim(v.hdr["sec-ch-ua-platform"], `"`); h != "" && uach.Platform != "" && h != uach.Platform {
			return true
		}
	}
	var langs []string
	al, _, _ := strings.Cut(v.hdr["accept-language"], ",")
	al, _, _ = strings.Cut(al, ";")
	if want := primary(al); want != "" && want != "*" && v.get("langs", &langs) && len(langs) > 0 {
		return !slices.ContainsFunc(langs, func(l string) bool { return primary(l) == want })
	}
	return false
}

// primary is a language tag's language: "de" of "de-CH".
func primary(tag string) string {
	p, _, _ := strings.Cut(strings.TrimSpace(tag), "-")
	return strings.ToLower(p)
}
