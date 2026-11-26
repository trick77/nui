package agg

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trick77/trakk/backend/internal/classify"
	"github.com/trick77/trakk/backend/internal/store"
)

const (
	// Version is the build code's version, stored per day. Raise it with any change to what a day's rows hold: the
	// job then rebuilds every day whose raw still exists (older days keep what they were built with). 2: events'
	// Other per sub, Other's address counts. 3: the API figures (endpoints, accounts, failures; the hours' failed,
	// durations and accounts). 4: what people experienced (vital.go; the hours' engaged and vitals, engaged and
	// vital_*) and the known bots by service (services), named before the build (Options.Name).
	Version = 4
	// TopK is the entries a list part keeps per day: the dashboard's "Show all" (maxLimit).
	TopK = 500
	// otherKeysMax bounds an Other row's key hashes; past it the row says only how many keys it holds.
	otherKeysMax = 50_000

	dayMS  = 86_400_000
	hourMS = 3_600_000
)

// Dims are the list dimensions agg_list holds. A rebuild clears these from the day, so a dimension dropped here
// stays listed until its rows are gone.
var Dims = []string{"pages", "entry", "events", "outbound", "referrers", "networks", "countries", "probed", "errors",
	"status", "clients", "endpoints", "accounts", "failures", "agents", "services", "engaged", "vital_lcp", "vital_inp",
	"vital_cls"}

// Called reports whether a server request counts as a call of an endpoint: a 404 does not (a probe's and an
// endpoint's own look alike, a route with placeholders catches both), nor does one refused without a route (a scanner's
// path behind a key check; an MCP client's GET of a POST-only path, answered 405 as its protocol wants). "probed" has
// them.
func Called(status int, unrouted bool) bool {
	switch status {
	case 401, 403, 405:
		return !unrouted
	}
	return status != 404
}

// Endpoint is a call's endpoint: its route's template, and the operation inside it when the product names one.
func Endpoint(tmpl, op string) string {
	if op == "" {
		return tmpl
	}
	return tmpl + " " + op
}

// Failure is why a call failed, "" when it did not: the product's own name for it, else its status. The status alone
// is not enough (a JSON-RPC error is answered 200). Only a call has one.
func Failure(status int, err string) string {
	switch {
	case err != "":
		return err
	case status >= 400:
		return "HTTP " + strconv.Itoa(status)
	}
	return ""
}

// fullDims keep every entry: closed key spaces the server derives (ISO codes, status classes, ParseUA's names).
var fullDims = map[string]bool{"countries": true, "status": true, "clients": true}

// idByAddr count addresses under every audience: their ids are the address keys already, they store no addrs.
var idByAddr = map[string]bool{"networks": true, "probed": true, "status": true, "endpoints": true, "accounts": true,
	"failures": true, "agents": true, "services": true}

// otherBySub keep an Other row per sub: the dashboard filters them by it (events' sub=auto|custom).
var otherBySub = map[string]bool{"events": true}

// eventKinds are the browser kinds that are no action (the dashboard's eventKinds).
var eventKinds = map[string]bool{"page_view": true, "session_start": true, "engagement": true, "vital": true}

// TraceSQL sees, when set (tests: this package's and the dashboard's large-table test), every statement the
// builder reads with: their plans are checked.
var TraceSQL func(query string, args []any)

func query(ctx context.Context, db store.Queryer, q string, args ...any) (*sql.Rows, error) {
	if TraceSQL != nil {
		TraceSQL(q, args)
	}
	return db.QueryContext(ctx, q, args...)
}

// Builder builds site-days.
type Builder struct {
	Store *store.Store
	// K is the entries a list part keeps per day; 0 is TopK.
	K int
	// Name is Options.Name.
	Name func(ctx context.Context, site, day int64) (int64, error)
	// Test hooks: beforeRead runs between BeginAggDay and the read, afterRead between the read and the writes,
	// beforeWrite sees the build about to be written.
	beforeRead, afterRead func(site, day int64)
	beforeWrite           func(w *store.AggBuild)
	// version replaces Version (tests: a release with a new builder).
	version int64
}

func (b *Builder) builder() int64 {
	if b.version != 0 {
		return b.version
	}
	return Version
}

// buildLocks holds one mutex per store: the job and the size cap's Cover never build at once, so neither writes a
// day the other is writing.
var buildLocks sync.Map // *store.Store -> *sync.Mutex

func (b *Builder) lock() func() {
	m, _ := buildLocks.LoadOrStore(b.Store, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// BuildDay builds one site-day from its raw events and freezes it. It returns whether the day came out clean: false
// when raw changed after the build's read (a write, or retention cutting the day), which leaves it dirty for the next
// pass, or when a write cut short left rows nothing can rebuild. A day whose raw is gone is not rebuilt. A day with a
// bot's or a probe's row is not written while Name has no bot list: an error wrapping classify.ErrNoBotList.
func (b *Builder) BuildDay(ctx context.Context, site, day int64) (bool, error) {
	defer b.lock()()
	return b.buildDay(ctx, site, day)
}

// buildIfNeeded builds a site-day unless it is built already (store.AggDay.BuiltBy), checked under the lock: a list
// of days made before (the job's pass) goes stale while Cover builds. It returns whether it built.
func (b *Builder) buildIfNeeded(ctx context.Context, site, day int64) (bool, error) {
	defer b.lock()()
	days, err := b.Store.AggDays(ctx, site, day, day)
	if err != nil {
		return false, err
	}
	if d, ok := days[day]; ok && d.BuiltBy(b.builder()) {
		return false, nil
	}
	_, err = b.buildDay(ctx, site, day)
	return err == nil, err
}

func (b *Builder) buildDay(ctx context.Context, site, day int64) (bool, error) {
	st := b.Store
	if err := st.BeginAggDay(ctx, site, day); err != nil {
		return false, err
	}
	if b.beforeRead != nil {
		b.beforeRead(site, day)
	}
	// The day's state before the naming: the dirty mark the naming sets is no change to the sessions the next day
	// carries (RequeueNext).
	days, err := st.AggDays(ctx, site, day, day)
	if err != nil {
		return false, err
	}
	was := days[day]
	if !was.Raw {
		return was.Dirty != store.AggWriting, st.ClearAggDirty(ctx, site, day)
	}
	// Named on every build, before the read: names refreshed on some days of a range only would split a service.
	var named int64
	noList := false
	if b.Name != nil {
		named, err = b.Name(ctx, site, day)
		switch {
		case errors.Is(err, classify.ErrNoBotList):
			noList = true
		case err != nil:
			return false, err
		}
	}
	d := newDayBuild(site, day)
	pre, err := d.load(ctx, st)
	if err != nil {
		return false, err
	}
	if !pre.Raw {
		return pre.Dirty != store.AggWriting, st.ClearAggDirty(ctx, site, day)
	}
	// Frozen without a list, its bots would never be named (the purge waits for a built day, Run reports the wait).
	if noList && d.bots {
		return false, fmt.Errorf("its bots wait for their names: %w", classify.ErrNoBotList)
	}
	if b.afterRead != nil {
		b.afterRead(site, day)
	}
	k := b.K
	if k <= 0 {
		k = TopK
	}
	w := d.rows(k)
	w.Version, w.Builder, w.Events = pre.Version, b.builder(), d.events
	w.Carried, w.CarriedSaved = d.keep, d.keepSaved
	// A first build, or one for a raw write (or after a write cut short), may change the next day's carried
	// sessions; a rebuild for a new builder or for the day before does not change this day's raw, nor does a name.
	// The read's state where the naming set no mark; where it did, the state before it (a raw write racing the
	// naming is then taken for its mark).
	changed := func(a store.AggDay) bool {
		return !a.Frozen || a.Dirty == store.AggRawChanged || a.Dirty == store.AggWriting
	}
	w.RequeueNext = changed(was) || named == 0 && changed(pre)
	if b.beforeWrite != nil {
		b.beforeWrite(&w)
	}
	return st.WriteAggDay(ctx, w)
}

// load reads the day's state, the day before's sessions and the day's rows in one read transaction: one snapshot,
// so the sums are the whole raw at the version they are written back with, whatever retention or the collector
// commit meanwhile. It returns the day's agg_day row as the snapshot has it.
func (d *dayBuild) load(ctx context.Context, st *store.Store) (store.AggDay, error) {
	tx, err := st.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return store.AggDay{}, err
	}
	defer func() { _ = tx.Rollback() }()
	days, err := store.AggDaysIn(ctx, tx, d.site, d.day-1, d.day)
	if err != nil {
		return store.AggDay{}, err
	}
	pre := days[d.day]
	if !pre.Raw {
		return pre, nil
	}
	prev, ok := days[d.day-1]
	if err := d.prevSessions(ctx, tx, ok && !prev.Raw, pre.Dirty == store.AggWriting); err != nil {
		return store.AggDay{}, err
	}
	return pre, d.read(ctx, tx)
}

type hourKey struct {
	hour int64
	cls  int
}

type hourAcc struct {
	events, views, actions, starts, requests, refused, errors, failed int64
	ids, addrs, server, callers, accounts                             set
	durations                                                         Hist
	engaged                                                           int64
	vitals                                                            [len(Vitals)]Hist
}

type listKey struct {
	dim    string
	part   int
	parent string
	key    string
}

type entry struct {
	sub       string
	a, b      int64
	ids, addr set
	durations Hist
}

// addrAcc is one address key's sums in one part. A busy day has hundreds of thousands, most with one session or
// none: the first session id is a field, the set only made for a second.
type addrAcc struct {
	events, views, actions, requests, refused int64
	sessions, carried                         int64
	first, last, asn                          int64
	sid                                       string
	more                                      map[string]struct{}
	ua, country                               string
	cls                                       int
	browser, human                            bool
}

// session counts sid once: whether it is new to the key.
func (a *addrAcc) session(sid string) bool {
	switch a.sid {
	case "":
		a.sid = sid
	case sid:
		return false
	default:
		if _, ok := a.more[sid]; ok {
			return false
		}
		if a.more == nil {
			a.more = map[string]struct{}{}
		}
		a.more[sid] = struct{}{}
	}
	a.sessions++
	return true
}

// set holds hashes of ids: the exact distinct count and the sketch's input.
type set map[uint64]struct{}

func (s *set) add(id string) {
	if *s == nil {
		*s = set{}
	}
	(*s)[Hash(id)] = struct{}{}
}

func (s set) sketch() []byte {
	if len(s) == 0 {
		return nil
	}
	k := NewSketch()
	for h := range s {
		k.Add(h)
	}
	return k.Marshal()
}

// dayBuild is one site-day's figures, summed from one read of its rows.
type dayBuild struct {
	site, day int64
	events    int64
	hours     map[hourKey]*hourAcc
	lists     map[listKey]*entry
	addrs     map[store.AggAddrKey]*addrAcc
	// prev are the day before's (key, part, sid) hashes; keep, when set, the stored carried counts instead, from the
	// copy a cut-short rewrite saved when keepSaved.
	prev      set
	keep      map[store.AggAddrKey]int64
	keepSaved bool
	clients   map[string][3]string
	agents    map[string]string // classify.Client by user agent
	// bots: a bot's or a probe's row, which only a bot list names (Builder.Name).
	bots bool
}

func newDayBuild(site, day int64) *dayBuild {
	return &dayBuild{site: site, day: day, hours: map[hourKey]*hourAcc{}, lists: map[listKey]*entry{},
		addrs: map[store.AggAddrKey]*addrAcc{}, clients: map[string][3]string{}, agents: map[string]string{}}
}

func sessionHash(ipKey string, part int, sid string) string {
	return ipKey + "\x00" + strconv.Itoa(part) + "\x00" + sid
}

func partOf(cls int) int {
	if cls == store.ClsHuman {
		return 1
	}
	return 2
}

// prevSessions loads the day before's sessions per key and part, which carried compares with: from its raw, one
// statement over that day; or, when its raw is gone (noRaw), the day's stored carried counts stand. A day left
// mid-write (writing) holds part of its rows: its counts come from the copy the cut-short rewrite saved first
// (store.AggBuild.Carried). Without one (a write cut short before its day before lost its raw) nothing tells which
// sessions were carried: each counts as new, a session over the midnight on both days, and the build says so.
func (d *dayBuild) prevSessions(ctx context.Context, q store.Queryer, noRaw, writing bool) error {
	if noRaw && writing {
		keep, whole, err := store.AggSavedCarriedIn(ctx, q, d.site, d.day)
		if err != nil {
			return err
		}
		if whole {
			d.keep, d.keepSaved = keep, true
			return nil
		}
		slog.Warn("Aggregates: a day left mid-write whose day before has no raw is rebuilt without its carried "+
			"sessions: visits over its midnight count on both days", "site", d.site,
			"day", time.UnixMilli(d.day*dayMS).UTC().Format(time.DateOnly))
		d.keep = map[store.AggAddrKey]int64{}
		return nil
	}
	if noRaw {
		var err error
		d.keep, err = store.AggCarriedIn(ctx, q, d.site, d.day)
		return err
	}
	rows, err := query(ctx, q, `SELECT ip_key, sid, cls FROM event
		WHERE site_id = ? AND ts >= ? AND ts < ? AND sid != ''`, d.site, (d.day-1)*dayMS, d.day*dayMS)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	d.prev = set{}
	for rows.Next() {
		var key, sid string
		var cls int
		if err := rows.Scan(&key, &sid, &cls); err != nil {
			return err
		}
		d.prev.add(sessionHash(key, partOf(cls), sid))
	}
	return rows.Err()
}

// read streams the day's rows once, in time order (id breaks a tie: the latest row is the one written last).
func (d *dayBuild) read(ctx context.Context, db store.Queryer) error {
	rows, err := query(ctx, db, `SELECT ts, source, kind, name, visitor, sid, ip, ip_key, ua, template, ref_host,
		status, asn, country, cls, dur_ms, account, op, err, unrouted, svc, `+VitalValueSQL+`, `+VitalVerSQL+`,
		`+EngagedSQL+` FROM event WHERE site_id = ? AND ts >= ? AND ts < ? ORDER BY ts, id`,
		d.site, d.day*dayMS, (d.day+1)*dayMS)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var e raw
	for rows.Next() {
		if err := rows.Scan(&e.ts, &e.source, &e.kind, &e.name, &e.visitor, &e.sid, &e.ip, &e.ipKey, &e.ua, &e.tmpl,
			&e.refHost, &e.status, &e.asn, &e.country, &e.cls, &e.dur, &e.account, &e.op, &e.err, &e.unrouted, &e.svc,
			&e.value, &e.ver, &e.engaged); err != nil {
			return err
		}
		d.add(&e)
	}
	return rows.Err()
}

func (d *dayBuild) add(e *raw) {
	d.events++
	part := partOf(e.cls)
	browser, server := e.source == "browser", e.source == "server"
	action := browser && !eventKinds[e.kind]
	isErr := e.kind == "js_error" || (server && e.status >= 500)

	hc := e.cls
	if hc == store.ClsPending {
		hc = store.ClsUnknown
	}
	h := d.hours[hourKey{e.ts / hourMS, hc}]
	if h == nil {
		h = &hourAcc{}
		d.hours[hourKey{e.ts / hourMS, hc}] = h
	}
	h.events++
	h.addrs.add(e.ipKey)
	if browser {
		h.ids.add(e.visitor)
	}
	if e.kind == "page_view" {
		h.views++
	}
	if action {
		h.actions++
	}
	if e.kind == "session_start" {
		h.starts++
	}
	if server {
		h.requests++
		h.server.add(e.ipKey)
		if e.status >= 400 {
			h.refused++
		}
		if Called(e.status, e.unrouted) {
			h.durations.Add(e.dur, 1)
			h.callers.add(e.ipKey)
			if Failure(e.status, e.err) != "" {
				h.failed++
			}
			if e.account != "" {
				h.accounts.add(e.account)
			}
		}
	}
	if isErr {
		h.errors++
	}
	h.engaged += e.engaged
	vital, units := -1, int64(0)
	if e.kind == "vital" {
		if i, u, ok := VitalUnits(e.name, e.ver, e.value); ok {
			vital, units = i, u
			h.vitals[i].Add(u, 1)
		}
	}
	d.bots = d.bots || e.cls == store.ClsBot || e.cls == store.ClsMonitor

	d.addLists(e, part, browser, server, action, isErr)
	d.addExperience(e, part, vital, units)

	ak := store.AggAddrKey{IPKey: e.ipKey, Part: part}
	a := d.addrs[ak]
	if a == nil {
		a = &addrAcc{first: e.ts}
		d.addrs[ak] = a
	}
	a.events++
	if e.kind == "page_view" {
		a.views++
	}
	if action {
		a.actions++
	}
	if server {
		a.requests++
		if e.status >= 400 {
			a.refused++
		}
	}
	if e.sid != "" && a.session(e.sid) {
		if _, ok := d.prev[Hash(sessionHash(e.ipKey, part, e.sid))]; ok {
			a.carried++
		}
	}
	a.browser = a.browser || browser
	a.human = a.human || e.cls == store.ClsHuman
	a.last, a.ua, a.asn, a.country, a.cls = e.ts, e.ua, e.asn, e.country, e.cls
}

// addLists adds a row to every list dimension it belongs to, with the identities the dashboard counts there: a
// browser's own id ("visitor"; the server's h: hash never), the address key, or either ("actor", errors).
func (d *dayBuild) addLists(e *raw, part int, browser, server, action, isErr bool) {
	vid := ""
	if browser {
		vid = e.visitor
	}
	addr := ""
	if part == 2 {
		addr = e.ipKey // the bots audience counts addresses
	}
	switch e.kind {
	case "page_view":
		d.entry("pages", part, "", e.tmpl, "", 1, 0, vid, addr)
	case "session_start":
		d.entry("entry", part, "", e.tmpl, "", 1, 0, vid, addr)
	}
	if action {
		k, sub := e.kind, "auto"
		if e.kind == "custom" {
			k, sub = e.name, "custom"
		}
		d.entry("events", part, "", k, sub, 1, 0, vid, addr)
	}
	if e.kind == "outbound_click" {
		d.entry("outbound", part, "", e.name, "", 1, 0, vid, addr)
	}
	if (e.kind == "page_view" || e.kind == "session_start") && e.refHost != "" {
		var pv int64
		if e.kind == "page_view" {
			pv = 1
		}
		d.entry("referrers", part, "", e.refHost, "", pv, 0, vid, addr)
	}
	if e.asn != 0 {
		var srv int64
		if server {
			srv = 1
		}
		d.entry("networks", part, "", strconv.FormatInt(e.asn, 10), "", 1, srv, e.ipKey, "")
	}
	if e.country != "" {
		d.entry("countries", part, "", e.country, "", 1, 0, vid, addr)
	}
	if server && (e.status == 401 || e.status == 403 || e.status == 404) {
		d.entry("probed", part, "", e.tmpl, strconv.Itoa(e.status), 1, 0, e.ipKey, "")
	}
	if isErr {
		k, sub := e.tmpl, "server "+strconv.Itoa(e.status)
		if e.kind == "js_error" {
			k, sub = e.name, "js"
		}
		actor := e.ipKey
		if browser {
			actor = e.visitor
		}
		d.entry("errors", part, "", k, sub, 1, 0, actor, addr)
	}
	if server && Called(e.status, e.unrouted) {
		var failed int64
		reason := Failure(e.status, e.err)
		if reason != "" {
			failed = 1
			d.entry("failures", part, "", reason, "", 1, 0, e.ipKey, "")
		}
		d.entry("endpoints", part, "", Endpoint(e.tmpl, e.op), e.op, 1, failed, e.ipKey, "").durations.Add(e.dur, 1)
		name, ok := d.agents[e.ua]
		if !ok {
			name = classify.Client(e.ua)
			d.agents[e.ua] = name
		}
		d.entry("agents", part, "", name, "", 1, failed, e.ipKey, "")
		if e.account != "" {
			d.entry("accounts", part, "", e.account, "", 1, failed, e.ipKey, "")
		}
	}
	if server && e.status > 0 {
		d.entry("status", part, "", strconv.Itoa(e.status/100)+"xx", "", 1, 0, "", "")
	}
	if server && e.svc != "" {
		sub := "bot"
		if e.cls == store.ClsMonitor {
			sub = "monitor"
		}
		var refused int64
		if e.status >= 400 {
			refused = 1
		}
		d.entry("services", part, "", e.svc, sub, 1, refused, e.ipKey, "")
	}
	// Clients: every client a visitor used that day (the dashboard's latest-client rule does not add up over days).
	if vid != "" || addr != "" {
		c, ok := d.clients[e.ua]
		if !ok {
			br, os, dev := classify.ParseUA(e.ua)
			c = [3]string{br, os, dev}
			d.clients[e.ua] = c
		}
		v := "IPv4"
		if strings.Contains(e.ip, ":") {
			v = "IPv6"
		}
		d.entry("clients", part, "browsers", c[0], "", 0, 0, vid, addr)
		d.entry("clients", part, "os", c[1], "", 0, 0, vid, addr)
		d.entry("clients", part, "devices", c[2], "", 0, 0, vid, addr)
		d.entry("clients", part, "ip_version", v, "", 0, 0, vid, addr)
	}
}

// addExperience adds a person's row (part 1, class human: no dimension before these was people's only) to what
// people experienced per page template: engaged (a the active ms, b the page views, so an average divides figures of
// one builder) and a vital's dimension (a the samples, the histogram their units). vital is its index in Vitals, -1
// for none.
func (d *dayBuild) addExperience(e *raw, part, vital int, units int64) {
	if part != 1 {
		return
	}
	switch e.kind {
	case "page_view":
		d.entry("engaged", part, "", e.tmpl, "", 0, 1, e.visitor, "")
	case "engagement":
		d.entry("engaged", part, "", e.tmpl, "", e.engaged, 0, e.visitor, "")
	case "vital":
		if vital >= 0 && vitalDims[vital] != "" {
			d.entry(vitalDims[vital], part, "", e.tmpl, "", 1, 0, e.visitor, "").durations.Add(units, 1)
		}
	}
}

func (d *dayBuild) entry(dim string, part int, parent, key, sub string, a, b int64, id, addr string) *entry {
	k := listKey{dim, part, parent, key}
	g := d.lists[k]
	if g == nil {
		g = &entry{}
		d.lists[k] = g
	}
	g.sub = max(g.sub, sub)
	g.a += a
	g.b += b
	if id != "" {
		g.ids.add(id)
	}
	if addr != "" {
		g.addr.add(addr)
	}
	return g
}

// rows turns the day's sums into table rows, each list part cut to the union of its top k by every measure. The
// sums go as their rows are made, so the peak is the read's: hours and lists here, the address rows streamed from
// their sums in key order as WriteAggDay takes them (the build is used once).
func (d *dayBuild) rows(k int) store.AggBuild {
	out := store.AggBuild{Site: d.site, Day: d.day, Dims: Dims}
	d.prev, d.clients = nil, nil
	for hk, h := range d.hours {
		row := store.AggHour{SiteID: d.site, Hour: hk.hour, Cls: hk.cls, Events: h.events,
			Views: h.views, Actions: h.actions, Starts: h.starts, Requests: h.requests, Refused: h.refused,
			Errors: h.errors, BrowserIDs: h.ids.sketch(), Addrs: h.addrs.sketch(), ServerAddrs: h.server.sketch(),
			Failed: h.failed, Durations: h.durations.Marshal(), Callers: h.callers.sketch(),
			Accounts: h.accounts.sketch(), Engaged: h.engaged}
		for i := range h.vitals {
			row.Vitals[i] = h.vitals[i].Marshal()
		}
		out.Hours = append(out.Hours, row)
	}
	d.hours = nil
	slices.SortFunc(out.Hours, func(a, b store.AggHour) int {
		return cmp.Or(cmp.Compare(a.Hour, b.Hour), cmp.Compare(a.Cls, b.Cls))
	})

	groups := map[listKey][]listKey{} // (dim, part) -> its entries' keys
	for lk := range d.lists {
		g := listKey{dim: lk.dim, part: lk.part}
		groups[g] = append(groups[g], lk)
	}
	for _, g := range slices.SortedFunc(maps.Keys(groups), compareListKey) {
		out.Lists = append(out.Lists, d.listPart(g, groups[g], k)...)
		for _, lk := range groups[g] {
			delete(d.lists, lk)
		}
		delete(groups, g)
	}
	d.lists = nil

	keys := slices.SortedFunc(maps.Keys(d.addrs), func(a, b store.AggAddrKey) int {
		return cmp.Or(strings.Compare(a.IPKey, b.IPKey), cmp.Compare(a.Part, b.Part))
	})
	out.Addrs = func(yield func(store.AggAddr) bool) {
		for _, ak := range keys {
			a := d.addrs[ak]
			delete(d.addrs, ak)
			carried := a.carried
			if d.keep != nil {
				carried = d.keep[ak]
			}
			if !yield(store.AggAddr{SiteID: d.site, Day: d.day, IPKey: ak.IPKey, Part: ak.Part, Events: a.events,
				Views: a.views, Actions: a.actions, Requests: a.requests, Refused: a.refused, Sessions: a.sessions,
				Carried: carried, Browser: a.browser, Human: a.human, FirstTS: a.first, LastTS: a.last, UA: a.ua,
				ASN: a.asn, Country: a.country, Cls: a.cls}) {
				return
			}
		}
	}
	return out
}

func compareListKey(a, b listKey) int {
	return cmp.Or(strings.Compare(a.dim, b.dim), cmp.Compare(a.part, b.part), strings.Compare(a.parent, b.parent),
		strings.Compare(a.key, b.key))
}

// listPart is one (dimension, part)'s rows: the kept entries and an Other row per parent for the rest.
func (d *dayBuild) listPart(g listKey, keys []listKey, k int) []store.AggList {
	slices.SortFunc(keys, compareListKey)
	keep := map[listKey]bool{}
	if fullDims[g.dim] || len(keys) <= k {
		for _, lk := range keys {
			keep[lk] = true
		}
	} else {
		// The top k by each measure the dashboard ranks or thresholds on: the distinct count, the sum, the bots
		// audience's address count (automated part), and for networks the server rows (spike:net).
		measures := []func(*entry) int64{
			func(e *entry) int64 { return int64(len(e.ids)) },
			func(e *entry) int64 { return e.a },
		}
		if g.part == 2 && !idByAddr[g.dim] {
			measures = append(measures, func(e *entry) int64 { return int64(len(e.addr)) })
		}
		if g.dim == "networks" {
			measures = append(measures, func(e *entry) int64 { return e.b })
		}
		ranked := slices.Clone(keys)
		for _, m := range measures {
			slices.SortStableFunc(ranked, func(x, y listKey) int {
				return cmp.Compare(m(d.lists[y]), m(d.lists[x]))
			})
			for _, lk := range ranked[:k] {
				keep[lk] = true
			}
			slices.SortFunc(ranked, compareListKey) // ties by key again for the next measure
		}
	}
	var out []store.AggList
	type rest struct {
		a, b, n, addrN, entries int64
		hashes                  []uint64
	}
	others := map[[2]string]*rest{} // (parent, sub)
	for _, lk := range keys {
		e := d.lists[lk]
		if keep[lk] {
			row := store.AggList{SiteID: d.site, Dim: g.dim, Day: d.day, Part: g.part, Parent: lk.parent, Key: lk.key,
				Sub: e.sub, A: e.a, B: e.b, N: int64(len(e.ids)), IDs: e.ids.sketch(), Durations: e.durations.Marshal()}
			if g.part == 2 {
				row.Addrs = e.addr.sketch()
			}
			out = append(out, row)
			continue
		}
		ok := [2]string{lk.parent, ""}
		if otherBySub[g.dim] {
			ok[1] = e.sub
		}
		o := others[ok]
		if o == nil {
			o = &rest{}
			others[ok] = o
		}
		o.a += e.a
		o.b += e.b
		o.n += int64(len(e.ids))
		o.addrN += int64(len(e.addr))
		o.entries++
		o.hashes = append(o.hashes, Hash(lk.key))
	}
	for _, ok := range slices.SortedFunc(maps.Keys(others), func(a, b [2]string) int {
		return cmp.Or(strings.Compare(a[0], b[0]), strings.Compare(a[1], b[1]))
	}) {
		o := others[ok]
		row := store.AggList{SiteID: d.site, Dim: g.dim, Day: d.day, Part: g.part, Other: true, Parent: ok[0],
			Sub: ok[1], A: o.a, B: o.b, N: o.n, AddrN: o.addrN, Entries: o.entries}
		if o.entries <= otherKeysMax {
			slices.Sort(o.hashes)
			row.Keys = make([]byte, 0, 8*len(o.hashes))
			for _, h := range slices.Compact(o.hashes) {
				row.Keys = binary.BigEndian.AppendUint64(row.Keys, h)
			}
		}
		out = append(out, row)
	}
	return out
}

// OtherKeys decodes an Other row's key hashes; nil when the row did not enumerate them.
func OtherKeys(b []byte) []uint64 {
	if b == nil {
		return nil
	}
	out := make([]uint64, 0, len(b)/8)
	for i := 0; i+8 <= len(b); i += 8 {
		out = append(out, binary.BigEndian.Uint64(b[i:]))
	}
	return out
}
