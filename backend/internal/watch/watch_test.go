package watch

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/plancheck"
	"github.com/trick77/trakk/backend/internal/store"
)

// d0 is the test site's first day; the learning window ends at d0 + LearnDays.
var d0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixMilli() / dayMS

// rig is one site's events and the job over them.
type rig struct {
	t    *testing.T
	st   *store.Store
	site int64
	evs  []store.Event
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rows, err := st.SyncSites(t.Context(), []store.Site{{Domain: "a.example.com", Kind: "site", PathRules: "[]"}})
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, st: st, site: rows["a.example.com"].ID}
	// The site's first day: where the walk begins. A pass that day: the job runs from the site's start, never a
	// backfill (which finds nothing).
	r.req(at(d0, 9), "192.0.2.1")
	r.pass(at(d0, 12))
	return r
}

// at is hour h of day d, unix ms.
func at(d int64, h int) int64 { return d*dayMS + int64(h)*3_600_000 }

// req queues a server request: raw the job walks past.
func (r *rig) req(ts int64, addr string) {
	r.evs = append(r.evs, store.Event{SiteID: r.site, TS: ts, Source: "server", Kind: "request", IP: addr, IPKey: addr,
		Method: "GET", Path: "/", Template: "GET /", Status: 200, Props: `{"bytes":900}`})
}

// engage queues an engagement event from an address reporting script hosts: the event's props as given, in the
// collector's envelope. Its visitor id is the address's.
func (r *rig) engage(ts int64, addr, props string) { r.engageAs(ts, "v-"+addr, addr, props) }

// engageAs is engage with a visitor id of the browser's choosing.
func (r *rig) engageAs(ts int64, visitor, addr, props string) {
	r.evs = append(r.evs, store.Event{SiteID: r.site, TS: ts, Source: "browser", Kind: "engagement", Visitor: visitor,
		IP: addr, IPKey: addr, Props: `{"p":` + props + `,"title":"T"}`})
}

func (r *rig) flush() {
	r.t.Helper()
	if len(r.evs) == 0 {
		return
	}
	if err := r.st.InsertEvents(r.t.Context(), r.evs); err != nil {
		r.t.Fatal(err)
	}
	r.evs = nil
}

// pass runs a pass at now (unix ms).
func (r *rig) pass(now int64) {
	r.t.Helper()
	r.flush()
	if err := Pass(r.t.Context(), r.st, time.UnixMilli(now)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) site0() Site {
	r.t.Helper()
	s, _, err := SiteState(r.t.Context(), r.st.DB, r.site)
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

func (r *rig) host(name string) Host {
	r.t.Helper()
	hs, err := readHosts(r.t.Context(), r.st.DB, siteHosts+` AND host = ?`, r.site, name)
	if err != nil {
		r.t.Fatal(err)
	}
	if len(hs) == 0 {
		return Host{}
	}
	return hs[0]
}

// findings are the site's findings from day from on, as "script:host" (provisional ones with a trailing ?).
func (r *rig) findings(from int64) []string {
	r.t.Helper()
	fs, err := Findings(r.t.Context(), r.st.DB, r.site, from*dayMS)
	if err != nil {
		r.t.Fatal(err)
	}
	out := []string{}
	for _, f := range fs {
		id := "script:" + f.Key
		if f.Provisional {
			id += "?"
		}
		out = append(out, id)
	}
	return out
}

func want(t *testing.T, got []string, ids ...string) {
	t.Helper()
	if ids == nil {
		ids = []string{}
	}
	if !reflect.DeepEqual(got, ids) {
		t.Errorf("findings %v, want %v", got, ids)
	}
}

// reported queues reports of hosts (their JSON strings, comma-separated) from addresses of n networks on day d.
func (r *rig) reported(d int64, h, n int, hosts string) {
	for i := range n {
		r.engage(at(d, h)+int64(i), netKey(i), `{"scripts":[`+hosts+`]}`)
	}
}

// Between a closed day's merge and the pass's new provisional rows a finding is stored twice a moment: it is read
// once, as closed.
func TestClosedOutranksOpen(t *testing.T) {
	r := newRig(t)
	day := d0 + LearnDays + 2
	r.engage(at(d0, 9), "192.0.2.1", `{"scripts":["cdn.example.org"]}`)
	r.reported(day, 20, minNets, `"new.example.net"`)
	r.pass(at(day, 23))
	open, err := readOpen(t.Context(), r.st.DB, r.site)
	if err != nil || len(open) != 2 {
		t.Fatalf("open %v %v", open, err)
	}
	want(t, r.findings(day), "script:new.example.net?")
	r.pass(at(day+1, 3))
	if err := writeOpen(t.Context(), r.st.DB, r.site, open); err != nil {
		t.Fatal(err)
	}
	want(t, r.findings(day), "script:new.example.net")
}

// The open days' hosts are listed before their day closes, as the closed state merged with them holds them; they
// are no findings.
func TestOpenHosts(t *testing.T) {
	r := newRig(t)
	day := d0 + 10
	r.engage(at(day-1, 9), "192.0.2.1", `{"scripts":["cdn.example.org"]}`)
	r.engage(at(day-1, 9), "192.0.2.2", `{"scripts":["cdn.example.org"]}`)
	r.pass(at(day, 3))
	r.engage(at(day, 9), "192.0.2.3", `{"scripts":["cdn.example.org","late.example.net"]}`)
	r.engage(at(day, 9)+1, "192.0.2.4", `{"scripts":["late.example.net"]}`)
	r.pass(at(day, 12))
	hs, err := Hosts(t.Context(), r.st.DB, r.site, day*dayMS)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(hs); got != fmt.Sprintf("[{late.example.net %d %d 2 1 0 0} {cdn.example.org %d %d 2 1 0 0}]",
		at(day, 9), at(day, 9)+1, at(day-1, 9), at(day, 9)) {
		t.Errorf("hosts %s", got)
	}
	want(t, r.findings(d0))
}

// Age retention cuts the state idle past the months, the rest stays.
func TestRetention(t *testing.T) {
	r := newRig(t)
	r.history()
	r.pass(at(d0+14, 3))
	// An open row's first_seen is a finding's time, or a host's merged first sight: a long-known host's row stays.
	if err := writeOpen(t.Context(), r.st.DB, r.site, []Open{{Kind: "host", Key: "cdn.example.org", FirstSeen: at(d0, 9),
		Detail: "{}"}, {Kind: "script", Key: "old.example.net", FirstSeen: at(d0, 9), Detail: "{}"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.PruneAge(t.Context(), time.UnixMilli(at(d0+12, 0))); err != nil {
		t.Fatal(err)
	}
	if open, err := readOpen(t.Context(), r.st.DB, r.site); err != nil || fmt.Sprint(open) != "[{host cdn.example.org "+
		fmt.Sprint(at(d0, 9))+" {}}]" {
		t.Errorf("open rows %v %v", open, err)
	}
	if h := r.host("cdn.example.org"); h.Host == "" {
		t.Error("an active host was cut")
	}
	if h := r.host("once.example.net"); h.Host != "" {
		t.Errorf("an idle host was kept: %+v", h)
	}
	want(t, r.findings(d0), "script:pay.example.net")
}

// The learning window finds nothing: a host three networks report in the first week of reports is learned, one a
// week on is found.
func TestLearningWindow(t *testing.T) {
	r := newRig(t)
	r.engage(at(d0+1, 9), "192.0.2.1", `{"scripts":["cdn.example.org"]}`)
	r.reported(d0+3, 9, minNets, `"early.example.net"`)
	r.reported(d0+1+LearnDays, 9, minNets, `"late.example.net"`)
	r.pass(at(d0+LearnDays+2, 3))
	want(t, r.findings(d0), "script:late.example.net")
	if s := r.site0(); s.ScriptsFrom != d0+1 {
		t.Errorf("scripts from %d, want %d", s.ScriptsFrom, d0+1)
	}
}

// snapshot is every watch table's rows, as text: the kept columns by name.
func snapshot(t *testing.T, st *store.Store, skipCut bool) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{`SELECT site_id, host, first_seen, last_seen, addrs, nets, found_at, found_first
		FROM script_host ORDER BY site_id, host`,
		`SELECT site_id, kind, key, first_seen, detail FROM watch_open ORDER BY site_id, kind, key`,
		`SELECT site_id, from_day, done_day, ready_at, scripts_from, cut FROM watch_site ORDER BY site_id`} {
		rows, err := st.DB.QueryContext(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			if skipCut && strings.Contains(q, "watch_site") {
				vals[len(vals)-1] = nil
			}
			fmt.Fprintln(&b, vals...)
		}
		_ = rows.Close()
	}
	return b.String()
}

// history fills a fortnight: people loading the CDN's scripts, a host reported once, a new host.
func (r *rig) history() {
	for d := d0; d < d0+14; d++ {
		for i := range 5 {
			r.req(at(d, 8)+int64(i), fmt.Sprintf("192.0.2.%d", 10+i))
			r.engage(at(d, 9)+int64(i), fmt.Sprintf("192.0.2.%d", 10+i), `{"scripts":["cdn.example.org"]}`)
		}
	}
	r.engage(at(d0+1, 10), "192.0.2.10", `{"scripts":["once.example.net"]}`)
	for i := range 4 {
		r.engage(at(d0+13, 10)+int64(i), netKey(i), `{"scripts":["cdn.example.org","pay.example.net"]}`)
	}
}

// netKey is an address key of the i-th of distinct networks: an IPv6 /64 in its own /48.
func netKey(i int) string { return fmt.Sprintf("2001:db8:%x::/64", 0x100+i) }

// A pass merges each closed day once; a second changes nothing, and walking every closed day again (the cursor set
// back) leaves the same state: every value is a first, a last, a most or set once.
func TestIdempotent(t *testing.T) {
	r := newRig(t)
	r.history()
	now := at(d0+14, 3)
	r.pass(now)
	want(t, r.findings(d0), "script:pay.example.net")
	once := snapshot(t, r.st, false)
	r.pass(now)
	if again := snapshot(t, r.st, false); again != once {
		t.Errorf("second pass:\n%s\nfirst:\n%s", again, once)
	}
	// The cut counts what a walk of a day left out: a day walked twice counts it twice, the rest stays.
	once = snapshot(t, r.st, true)
	if _, err := r.st.DB.ExecContext(t.Context(), `UPDATE watch_site SET done_day = from_day - 1`); err != nil {
		t.Fatal(err)
	}
	r.pass(now)
	if again := snapshot(t, r.st, true); again != once {
		t.Errorf("walked again:\n%s\nfirst:\n%s", again, once)
	}
}

// A pass cut short mid-backfill goes on where it stopped: the state is what one uninterrupted pass makes.
func TestRestartMidBackfill(t *testing.T) {
	whole := newRig(t)
	whole.history()
	now := at(d0+14, 3)
	whole.pass(now)

	r := newRig(t)
	r.history()
	r.flush()
	ctx, cancel := context.WithCancel(t.Context())
	var mu sync.Mutex
	reads := 0
	TraceSQL = func(q string, _ []any) {
		mu.Lock()
		defer mu.Unlock()
		if q == scriptRows {
			if reads++; reads == 5 {
				cancel()
			}
		}
	}
	err := Pass(ctx, r.st, time.UnixMilli(now))
	TraceSQL = nil
	if err == nil {
		t.Fatal("the cut pass ended well")
	}
	if s := r.site0(); s.DoneDay < d0 || s.DoneDay >= d0+12 {
		t.Fatalf("cut at %+v", s)
	}
	r.pass(now)
	if got, want := snapshot(t, r.st, false), snapshot(t, whole.st, false); got != want {
		t.Errorf("restarted:\n%s\nwhole:\n%s", got, want)
	}
}

// The state outlives raw: once the events are gone a finding stays, and the next pass keeps it.
func TestRawDeleted(t *testing.T) {
	r := newRig(t)
	r.history()
	now := at(d0+14, 3)
	r.pass(now)
	if _, err := r.st.DB.ExecContext(t.Context(), `DELETE FROM event`); err != nil {
		t.Fatal(err)
	}
	r.pass(now + dayMS)
	want(t, r.findings(d0), "script:pay.example.net")
}

// Script hosts are the browser's word, counted by the addresses that reported them: lower-cased, a host name (an
// underscore in a label too, an IPv6 address in brackets in its canonical form, a name past 100 bytes cut to its
// last labels), never the site's own. A new one is a finding once addresses of three networks reported it on one day
// after the learning window: three visitor ids of one address are one, three addresses of one /24 or three /64s of
// one /48 are one network.
func TestScriptHosts(t *testing.T) {
	r := newRig(t)
	for d := d0; d < d0+LearnDays+2; d++ {
		r.engage(at(d, 9), "192.0.2.1", `{"scripts":["cdn.example.org"]}`)
	}
	day := d0 + LearnDays + 2
	long := strings.Repeat("a", 60) + "." + strings.Repeat("b", 30) + ".example.com"
	good := []string{`"CDN.Example.NET"`, `"a_b.example.net"`, `"[2001:DB8:0:0::1]"`, `"` + long + `"`}
	bad := []string{`"bad host"`, `123`, `"example.com/path"`, `"a.example.com"`, `"é.example.com"`,
		`"x..example.com"`, `"-x.example.com"`, `"[fe80::1%eth0]"`, `"[203.0.113.1]"`, `"[2001:db8::g]"`,
		`"` + strings.Repeat("a", 101) + `"`}
	for i, a := range []string{"192.0.2.10", "198.51.100.10", "203.0.113.10"} {
		r.engage(at(day, 9)+int64(i), a, `{"scripts":[`+strings.Join(append(good, bad...), ",")+`]}`)
	}
	r.engage(at(day, 9), "192.0.2.10", "{\"scripts\":[\"\xff.example.net\"]}")
	r.engage(at(day, 10), "192.0.2.10", `{"scripts":["two.example.net"]}`)
	r.engage(at(day, 10), "198.51.100.10", `{"scripts":["two.example.net"]}`)
	for i, a := range []string{"192.0.2.20", "192.0.2.21", "192.0.2.22"} {
		r.engage(at(day, 8)+int64(i), a, `{"scripts":["one24.example.net"]}`)
	}
	for i, a := range []string{"2001:db8:5:1::/64", "2001:db8:5:2::/64", "2001:db8:5:3::/64"} {
		r.engage(at(day, 8)+int64(i), a, `{"scripts":["one48.example.net"]}`)
	}
	for i := range 3 {
		r.engageAs(at(day, 11), fmt.Sprint("forged-", i), "203.0.113.66", `{"scripts":["forged.example.net"]}`)
	}
	r.pass(at(day+1, 3))
	hs, err := Hosts(t.Context(), r.st.DB, r.site, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, h := range hs {
		names = append(names, fmt.Sprint(h.Host, "/", h.Addrs))
	}
	clipped := strings.Repeat("b", 30) + ".example.com"
	if got, want := strings.Join(names, " "), "forged.example.net/1 two.example.net/2 [2001:db8::1]/3 a_b.example.net/3 "+
		clipped+"/3 cdn.example.net/3 one24.example.net/3 one48.example.net/3 cdn.example.org/1"; got != want {
		t.Errorf("hosts %s\nwant  %s", got, want)
	}
	want(t, r.findings(d0), "script:[2001:db8::1]", "script:a_b.example.net", "script:"+clipped,
		"script:cdn.example.net")
}

// The script's forms, as the job keeps them: a name past 100 bytes loses its first labels until it fits.
func TestScriptHost(t *testing.T) {
	long, label := strings.Repeat("x", 50)+"."+strings.Repeat("y", 50)+".example", strings.Repeat("x", 100)
	for raw, want := range map[string]string{
		`"a.example.org."`:   "a.example.org",
		`"[::FFFF:1.2.3.4]"`: "[::ffff:1.2.3.4]",
		`"` + long + `"`:     long[51:],
		`"` + label + `"`:    label,
		`"[2001:db8::1"`:     "",
	} {
		got, ok := scriptHost([]byte(raw))
		if got != want || ok != (want != "") {
			t.Errorf("%s: %q %v, want %q", raw, got, ok, want)
		}
	}
}

// One address reporting made-up names brings in at most dayHostsPerAddr a day; a name it could not bring in counts
// once in the cut, however often reported, and not at all once another address brought it in.
func TestScriptHostsPerAddress(t *testing.T) {
	r := newRig(t)
	day := d0 + LearnDays + 2
	var names []string
	for i := range 60 {
		names = append(names, fmt.Sprintf(`"h%02d.example.net"`, i))
	}
	for rep := range 2 {
		for i := 0; i < 60; i += maxScripts {
			r.engage(at(day, 9+rep)+int64(i), "203.0.113.66", `{"scripts":[`+strings.Join(names[i:i+maxScripts], ",")+`]}`)
		}
	}
	r.engage(at(day, 12), "192.0.2.10", `{"scripts":["h59.example.net"]}`)
	r.flush()
	dd, err := scan(t.Context(), r.st.DB, &siteRef{id: r.site, domain: "a.example.com"}, day, day*dayMS, (day+1)*dayMS)
	if err != nil {
		t.Fatal(err)
	}
	if len(dd.hosts) != dayHostsPerAddr+1 || dd.cut != 60-dayHostsPerAddr-1 {
		t.Errorf("%d hosts, cut %d", len(dd.hosts), dd.cut)
	}
	// The day's new hosts past maxNewHosts count too, once.
	r.pass(at(day+1, 3))
	if s := r.site0(); s.Cut != dd.cut+int64(len(dd.hosts)-maxNewHosts) {
		t.Errorf("cut %d", s.Cut)
	}
}

// Hosts a few addresses keep reporting stay evictable, seen that day or not: at the site's bound real hosts push them
// out. Found hosts keep their place up to their own bound, the latest seen first.
func TestHostEviction(t *testing.T) {
	day := d0 + 20
	// found: how many of the state's hosts were found (the rest one address keeps reporting, today too).
	run := func(found int) (outcome, *state) {
		st := &state{site: Site{FromDay: d0, ReadyAt: 1, ScriptsFrom: d0}, hosts: map[string]*Host{}}
		d := newDay(day)
		for i := range maxHosts {
			name := fmt.Sprintf("h%03d.example.net", i)
			h := &Host{Host: name, FirstSeen: at(d0+1, 0), LastSeen: at(day-1, 0), Addrs: 1}
			if i < found {
				h.FoundAt, h.Addrs, h.Nets, h.LastSeen = at(d0+2, 0)+int64(i), minNets, minNets, at(d0+2, 0)+int64(i)
			} else {
				d.hosts[name] = &hostDay{first: at(day, 1), last: at(day, 1), addrs: map[string]bool{"203.0.113.66": true},
					nets: map[string]bool{"203.0.113.0/24": true}}
			}
			st.hosts[name] = h
		}
		addrs := map[string]bool{"192.0.2.1": true, "198.51.100.1": true, "203.0.113.1": true}
		nets := map[string]bool{"192.0.2.0/24": true, "198.51.100.0/24": true, "203.0.113.0/24": true}
		for i := range 3 {
			d.hosts[fmt.Sprintf("real%d.example.org", i)] = &hostDay{first: at(day, 2), last: at(day, 2), addrs: addrs,
				nets: nets}
		}
		o := st.apply(d, true, at(day+1, 3))
		st.commit(o)
		if len(st.hosts) != maxHosts || len(o.found) != 3 {
			t.Errorf("found %d: %d hosts, findings %v", found, len(st.hosts), o.found)
		}
		for i := range 3 {
			if st.hosts[fmt.Sprintf("real%d.example.org", i)] == nil {
				t.Errorf("found %d: real%d not kept", found, i)
			}
		}
		return o, st
	}
	// The weak ones go, reported today or not, the found ones stay.
	if o, _ := run(10); fmt.Sprint(o.dropHosts) != "[h497.example.net h498.example.net h499.example.net]" {
		t.Errorf("dropped %v", o.dropHosts)
	}
	// All found: past maxFoundHosts the longest idle go.
	if o, _ := run(maxHosts); fmt.Sprint(o.dropHosts) != "[h002.example.net h001.example.net h000.example.net]" {
		t.Errorf("dropped %v", o.dropHosts)
	}
}

// A day's script host findings go to the hosts the most networks report: names a few networks report in bulk never
// push a host many report out. Those that found no place are found the next day, reported again or not, at their
// first sight the day they passed as new.
func TestScriptHostSlots(t *testing.T) {
	r := newRig(t)
	day := d0 + LearnDays + 2
	r.engage(at(d0, 9), "192.0.2.1", `{"scripts":["cdn.example.org"]}`)
	// Each made-up name from more addresses than the real host has, of fewer networks.
	for i := range maxPerDay + 1 {
		for n := range minNets {
			for k := range 2 {
				r.engage(at(day, 9)+int64(i), fmt.Sprintf("2001:db8:%x:%x::/64", 0x100+n, k),
					fmt.Sprintf(`{"scripts":["h%02d.example.net"]}`, i))
			}
		}
	}
	for n := range minNets + 1 {
		r.engage(at(day, 10), netKey(10+n), `{"scripts":["real.example.org"]}`)
	}
	r.pass(at(day+1, 3))
	// The two past the day's bound wait: provisional on the day still open.
	got := r.findings(day)
	if len(got) != maxPerDay+2 || !slices.Contains(got, "script:real.example.org") ||
		!slices.Contains(got, "script:h09.example.net?") || !slices.Contains(got, "script:h10.example.net?") {
		t.Errorf("findings %v", got)
	}
	r.engage(at(day+1, 9), netKey(0), `{"scripts":["h09.example.net"]}`)
	r.pass(at(day+2, 3))
	fs, err := Findings(t.Context(), r.st.DB, r.site, 0)
	if err != nil {
		t.Fatal(err)
	}
	late := map[string]int64{}
	for _, f := range fs {
		if !f.Provisional && (f.Key == "h09.example.net" || f.Key == "h10.example.net") {
			late[f.Key] = f.At
		}
	}
	if fmt.Sprint(late) != fmt.Sprintf("map[h09.example.net:%d h10.example.net:%d]", at(day, 9)+9, at(day, 9)+10) {
		t.Errorf("late %v", late)
	}
}

// A script host finding happens at its first sight on the day it was found, provisional or closed: the merging
// pass's time is bookkeeping. A sight the day before, by too few networks, is no part of it.
func TestScriptHostFoundTime(t *testing.T) {
	r := newRig(t)
	day := d0 + LearnDays + 2
	r.engage(at(d0, 9), "192.0.2.1", `{"scripts":["cdn.example.org"]}`)
	r.engage(at(day-1, 9), netKey(0), `{"scripts":["new.example.net"]}`)
	for n := range minNets {
		r.engage(at(day, 9)+int64(n), netKey(n), `{"scripts":["new.example.net"]}`)
	}
	times := func() []int64 {
		fs, err := Findings(t.Context(), r.st.DB, r.site, 0)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, f := range fs {
			out = append(out, f.At)
		}
		return out
	}
	r.pass(at(day, 12))
	if got := times(); !slices.Equal(got, []int64{at(day, 9)}) {
		t.Errorf("provisional %v", got)
	}
	r.pass(at(day+1, 5))
	if got := times(); !slices.Equal(got, []int64{at(day, 9)}) {
		t.Errorf("closed %v", got)
	}
	if h := r.host("new.example.net"); h.FoundAt != at(day+1, 5) || h.FirstSeen != at(day-1, 9) {
		t.Errorf("state %+v", h)
	}
}

// A site that fails leaves the others to go on, its error returned: the next site after it is walked all the same.
func TestPassGoesOn(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rows, err := st.SyncSites(t.Context(), []store.Site{{Domain: "a.example.com", Kind: "site", PathRules: "[]"},
		{Domain: "b.example.com", Kind: "site", PathRules: "[]"}})
	if err != nil {
		t.Fatal(err)
	}
	a, b := rows["a.example.com"].ID, rows["b.example.com"].ID
	if a > b {
		a, b = b, a
	}
	var evs []store.Event
	for _, id := range []int64{a, b} {
		evs = append(evs, store.Event{SiteID: id, TS: at(d0, 9), Source: "server", Kind: "request", IP: "192.0.2.1",
			IPKey: "192.0.2.1", Method: "GET", Path: "/", Template: "GET /", Status: 200, Props: "{}"})
	}
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	// The first site's state cannot be read.
	if _, err := st.DB.ExecContext(t.Context(), `INSERT INTO script_host (site_id, host, first_seen, last_seen)
		VALUES (?, 'x.example.net', 'not a time', 0)`, a); err != nil {
		t.Fatal(err)
	}
	err = Pass(t.Context(), st, time.UnixMilli(at(d0+2, 3)))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint("site ", a, ":")) {
		t.Errorf("error %v", err)
	}
	if s, ok, err := SiteState(t.Context(), st.DB, b); err != nil || !ok || s.DoneDay != d0+1 || s.ReadyAt == 0 {
		t.Errorf("site after it %+v %v %v", s, ok, err)
	}
}

// A site without raw is caught up and has no open rows; its first raw day, weeks on or a history older than the row,
// starts the walk.
func TestNoRaw(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rows, err := st.SyncSites(t.Context(), []store.Site{{Domain: "a.example.com", Kind: "site", PathRules: "[]"},
		{Domain: "b.example.com", Kind: "site", PathRules: "[]"}})
	if err != nil {
		t.Fatal(err)
	}
	site, hist := rows["a.example.com"].ID, rows["b.example.com"].ID
	if err := writeOpen(t.Context(), st.DB, site, []Open{{Kind: "script", Key: "x.example.net", FirstSeen: 1,
		Detail: "{}"}}); err != nil {
		t.Fatal(err)
	}
	if err := Pass(t.Context(), st, time.UnixMilli(at(d0, 12))); err != nil {
		t.Fatal(err)
	}
	if s, ok, err := SiteState(t.Context(), st.DB, site); err != nil || !ok || s.ReadyAt != at(d0, 12) ||
		s.FromDay != d0 {
		t.Errorf("state %+v %v %v", s, ok, err)
	}
	if open, err := readOpen(t.Context(), st.DB, site); err != nil || len(open) != 0 {
		t.Errorf("open %v %v", open, err)
	}
	first := d0 + 30
	ev := func(site, ts int64) store.Event {
		return store.Event{SiteID: site, TS: ts, Source: "server", Kind: "request", IP: "192.0.2.1", IPKey: "192.0.2.1",
			Method: "GET", Path: "/", Template: "GET /", Status: 200, Props: "{}"}
	}
	if err := st.InsertEvents(t.Context(), []store.Event{ev(site, at(first, 9)), ev(hist, at(d0-10, 9))}); err != nil {
		t.Fatal(err)
	}
	now := at(first+1, 3)
	if err := Pass(t.Context(), st, time.UnixMilli(now)); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := SiteState(t.Context(), st.DB, site); s.FromDay != first || s.DoneDay != first {
		t.Errorf("after the first raw %+v", s)
	}
	if s, _, _ := SiteState(t.Context(), st.DB, hist); s.FromDay != d0-10 || s.DoneDay != first || s.ReadyAt != now {
		t.Errorf("a history older than the row %+v", s)
	}
}

// Every statement the job reads events with stays in one site-day of the time index, without a sort.
func TestPlans(t *testing.T) {
	r := newRig(t)
	r.history()
	r.flush()
	var mu sync.Mutex
	seen := map[string][]any{}
	TraceSQL = func(q string, args []any) {
		mu.Lock()
		defer mu.Unlock()
		seen[q] = append([]any(nil), args...)
	}
	t.Cleanup(func() { TraceSQL = nil })
	r.pass(at(d0+14, 3))
	if len(seen) != 1 {
		t.Fatalf("%d statements traced", len(seen))
	}
	for q, args := range seen {
		plan, err := plancheck.Explain(t.Context(), r.st.DB, q, args...)
		if err != nil {
			t.Fatal(err)
		}
		if p := plancheck.Problem(q, plan); p != "" || strings.Contains(strings.Join(plan, "\n"), "TEMP B-TREE") {
			t.Errorf("plan of %s: %s\n%s", q, p, strings.Join(plan, "\n"))
		}
	}
}
