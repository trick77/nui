package classify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/alert"
	"github.com/trick77/trakk/backend/internal/refdata"
	"github.com/trick77/trakk/backend/internal/store"
)

// eventNames is each address's names, "" for none, and fails the test on a name beside a class that takes none.
func eventNames(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	var stray int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE svc != '' AND cls NOT IN (?, ?)`, store.ClsBot,
		store.ClsMonitor).Scan(&stray); err != nil || stray != 0 {
		t.Errorf("%d names beside another class, %v", stray, err)
	}
	rows, err := st.DB.Query(`SELECT ip, group_concat(DISTINCT svc) FROM event GROUP BY ip`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var ip, names string
		if err := rows.Scan(&ip, &names); err != nil {
			t.Fatal(err)
		}
		out[ip] = names
	}
	return out
}

// clientNames is each address and client's name.
func clientNames(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	rows, err := st.DB.Query(`SELECT ip || ' ' || ua, group_concat(DISTINCT svc) FROM event GROUP BY ip, ua`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var k, n string
		if err := rows.Scan(&k, &n); err != nil {
			t.Fatal(err)
		}
		out[k] = n
	}
	return out
}

// rowNames is every event's id, class and name.
func rowNames(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.DB.Query(`SELECT id, cls, svc FROM event ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id int64
		var cls int
		var svc string
		if err := rows.Scan(&id, &cls, &svc); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d %d %s", id, cls, svc))
	}
	return out
}

// wantNames are the names namesRig's clients end with.
var wantNames = map[string]string{
	// Two services on one range, two agents of one, a client no agent names.
	"192.0.2.1 RangedBot/2":     "RangedBot",
	"192.0.2.1 Ranged-Images/1": "Ranged-Images",
	"192.0.2.1 Ranged/1":        "Ranged",
	"192.0.2.1 SharedFetcher/1": "Shared (user-triggered)",
	"192.0.2.18 Probe-Check/1":  "Probe-Check",
	"192.0.2.18 Probe/1":        "Probe",
	"192.0.2.65 Labelled/1":     "Labelled",
	"192.0.2.97 Rangeless/1":    "Rangeless",
	"192.0.2.66 Nameless/1":     "", // refuted
	"198.51.100.7 curl/8":       "",
	"198.51.100.1 " + browserUA: "",
	"198.51.100.30 SomeAPI/1.0": "", // signed in
}

// namesRig is a store whose events the pass has classified and named with the made-up list (anyCrawlers), the rDNS
// answers in.
func namesRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, true)
	b, err := refdata.ParseBots([]byte(anyCrawlers), []byte(anyMonitors))
	if err != nil {
		t.Fatal(err)
	}
	r.c.Ref.SetBots(b)
	r.c.Verifier = refdata.NewVerifier(anyResolver{}, time.Second)
	var evs []store.Event
	for _, ua := range []string{"RangedBot/2", "RangedBot/2", "Ranged-Images/1", "Ranged-Images/1", "Ranged/1",
		"SharedFetcher/1"} {
		evs = append(evs, req("192.0.2.1", ua, "document", "/", 200, ""))
	}
	evs = append(evs,
		req("192.0.2.18", "Probe-Check/1", "", "/", 200, ""),
		req("192.0.2.18", "Probe/1", "", "/", 200, ""),
		req("192.0.2.65", "Labelled/1", "document", "/", 200, ""),
		req("192.0.2.66", "Nameless/1", "document", "/", 200, ""),
		req("192.0.2.97", "Rangeless/1", "document", "/", 200, ""),
		req("198.51.100.7", "curl/8", "", "/", 200, ""),
		req("198.51.100.1", browserUA, "document", "/", 200, ""),
		store.Event{Source: "browser", Kind: "page_view", IP: "198.51.100.1", UA: browserUA,
			Props: `{"ctx":{"webdriver":false}}`},
		account(req("198.51.100.30", "SomeAPI/1.0", "", "/api/x", 200, `{}`)),
	)
	r.insert(t, evs...)
	for range 2 {
		if err := r.c.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		r.c.Verifier.Work(t.Context())
	}
	return r
}

// Every pass names a bot's and a probe's clients by the one rule the write uses: an authoritative range's events
// are named right when written, and a pass over the same events, nothing new among them, changes no row and leaves
// the built day's version alone.
func TestPass_ServiceNames(t *testing.T) {
	r := newRig(t, true)
	b, err := refdata.ParseBots([]byte(anyCrawlers), []byte(anyMonitors))
	if err != nil {
		t.Fatal(err)
	}
	r.c.Ref.SetBots(b)
	written := map[string]string{}
	for _, ua := range []string{"RangedBot/2", "Ranged-Images/1", "Ranged/1", "SharedFetcher/1"} {
		e := store.Event{Source: "server", IP: "192.0.2.1", IPKey: "192.0.2.1", UA: ua}
		r.c.Enrich(&e)
		written["192.0.2.1 "+ua] = e.Svc
	}
	for _, ua := range []string{"Probe-Check/1", "Probe/1"} {
		e := store.Event{Source: "server", IP: "192.0.2.18", IPKey: "192.0.2.18", UA: ua}
		r.c.Enrich(&e)
		written["192.0.2.18 "+ua] = e.Svc
	}
	e := store.Event{Source: "server", IP: "192.0.2.97", IPKey: "192.0.2.97", UA: "Rangeless/1"}
	r.c.Enrich(&e)
	written["192.0.2.97 Rangeless/1"] = e.Svc
	for k, n := range written {
		if n != wantNames[k] {
			t.Errorf("%s written as %q, want %q", k, n, wantNames[k])
		}
	}

	r = namesRig(t)
	if got := clientNames(t, r.st); !maps.Equal(got, wantNames) {
		t.Errorf("names = %v\nwant %v", got, wantNames)
	}
	for k, n := range clientNames(t, r.st) {
		if _, ok := r.c.bots().ByName(n); n != "" && !ok {
			t.Errorf("%s named %q, which the list does not know", k, n)
		}
	}
	eventNames(t, r.st)
	// A pass that decides every actor again (a new classifier, as after a restart) over the same events.
	today := r.now.UnixMilli() / dayMS
	if err := r.st.BeginAggDay(t.Context(), r.site.ID, today); err != nil {
		t.Fatal(err)
	}
	before, version := rowNames(t, r.st), aggVersion(t, r, today)
	again := &Classifier{DB: r.st, Sites: r.c.Sites, Ref: r.c.Ref, Verifier: r.c.Verifier, Now: r.c.Now}
	if err := again.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if after := rowNames(t, r.st); !slices.Equal(after, before) {
		t.Errorf("a second pass changed rows:\n%v\n%v", before, after)
	}
	if v := aggVersion(t, r, today); v != version {
		t.Errorf("a second pass moved the version %d to %d", version, v)
	}
}

// The pass names the clients it does not weigh (past maxUAs) and each address under a key by the rule the write and
// NameDay use: per event's address, client and class. Neither changes a row the pass named.
func TestPass_NamesEveryClient(t *testing.T) {
	r := newRig(t, true)
	b, err := refdata.ParseBots([]byte(anyCrawlers), []byte(anyMonitors))
	if err != nil {
		t.Fatal(err)
	}
	r.c.Ref.SetBots(b)
	var evs []store.Event
	for i := range maxUAs + 6 {
		// A rangeless service and an authoritative range's agent, each rotating its user agent.
		evs = append(evs, req("203.0.113.9", fmt.Sprintf("Rangeless/%d", i), "", "/", 200, ""),
			req("192.0.2.3", fmt.Sprintf("RangedBot/%d", i), "", "/", 200, ""))
	}
	// One key, two addresses: one in Shared's range inside Ranged's, one in Ranged's alone; the second is the key's
	// first address, as the pass reads it.
	for _, ip := range []string{"2001:db8:1:5::1", "2001:db8:1:5:1::1"} {
		e := req(ip, "curl/8", "", "/", 200, "")
		e.IPKey = "2001:db8:1:5::/64"
		evs = append(evs, e, e)
	}
	r.insert(t, evs...)
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"203.0.113.9": "Rangeless", "192.0.2.3": "RangedBot",
		"2001:db8:1:5::1": "Shared (user-triggered)", "2001:db8:1:5:1::1": "Ranged"}
	if got := eventNames(t, r.st); !maps.Equal(got, want) {
		t.Errorf("names = %v\nwant %v", got, want)
	}
	today := r.now.UnixMilli() / dayMS
	if n, err := r.c.NameDay(t.Context(), r.site.ID, today); err != nil || n != 0 {
		t.Errorf("NameDay renamed %d after the pass, %v", n, err)
	}
	for _, e := range evs {
		fresh := store.Event{SiteID: r.site.ID, TS: e.TS, Source: e.Source, IP: e.IP, IPKey: e.IPKey, UA: e.UA}
		r.c.Enrich(&fresh)
		if fresh.Svc != want[e.IP] {
			t.Errorf("%s %s written as %q, the pass named %q", e.IP, e.UA, fresh.Svc, want[e.IP])
		}
	}
}

// NameDay names the events written before the column existed as the pass does, and a stray name off another class;
// once named, a day changes no more. NameRaw does every day once and marks it; without a bot list it waits, for a
// while.
func TestNameDay(t *testing.T) {
	r := namesRig(t)
	want := rowNames(t, r.st)
	today := r.now.UnixMilli() / dayMS
	if err := r.st.BeginAggDay(t.Context(), r.site.ID, today); err != nil {
		t.Fatal(err)
	}
	forget := func() {
		t.Helper()
		if _, err := r.st.DB.Exec(`UPDATE event SET svc = CASE ip WHEN '198.51.100.7' THEN 'Stale' ELSE '' END`); err != nil {
			t.Fatal(err)
		}
	}
	forget()
	v := aggVersion(t, r, today)
	n, err := r.c.NameDay(t.Context(), r.site.ID, today)
	if err != nil || n == 0 {
		t.Fatalf("named %d, %v", n, err)
	}
	if got := rowNames(t, r.st); !slices.Equal(got, want) {
		t.Errorf("NameDay named\n%v\nthe pass\n%v", got, want)
	}
	if aggVersion(t, r, today) == v {
		t.Error("NameDay left the built day's version")
	}
	v = aggVersion(t, r, today)
	if n, err := r.c.NameDay(t.Context(), r.site.ID, today); err != nil || n != 0 {
		t.Errorf("named again: %d, %v", n, err)
	}
	if aggVersion(t, r, today) != v {
		t.Error("a day named already moved its version")
	}

	forget()
	if err := r.c.NameRaw(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := rowNames(t, r.st); !slices.Equal(got, want) {
		t.Errorf("NameRaw named\n%v\nthe pass\n%v", got, want)
	}
	forget()
	if err := r.c.NameRaw(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := clientNames(t, r.st)["192.0.2.1 Ranged/1"]; got != "" {
		t.Errorf("NameRaw ran twice: %q", got)
	}

	// No bot list yet: it waits, says so once, names nothing, marks nothing, and fails past the wait.
	defer func(poll, wait time.Duration) { nameListPoll, nameListWait = poll, wait }(nameListPoll, nameListWait)
	nameListPoll, nameListWait = 5*time.Millisecond, 50*time.Millisecond
	bare := namesRig(t)
	want = rowNames(t, bare.st)
	if _, err := bare.st.DB.Exec(`UPDATE event SET svc = ''`); err != nil {
		t.Fatal(err)
	}
	unnamed := rowNames(t, bare.st)
	list := bare.c.Ref.Bots()
	empty := refdata.New(refdata.Options{Dir: t.TempDir()})
	bare.c.Ref = empty
	var logs bytes.Buffer
	bare.c.Log = slog.New(slog.NewTextHandler(&logs, nil))
	err = bare.c.NameRaw(t.Context())
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("past the wait without a bot list: %v", err)
	}
	if got := rowNames(t, bare.st); !slices.Equal(got, unnamed) {
		t.Errorf("named without a bot list:\n%v", got)
	}
	if done, err := bare.st.SvcNamed(t.Context()); err != nil || done {
		t.Errorf("marked without a bot list: %v, %v", done, err)
	}
	if n := strings.Count(logs.String(), "waits for the bot list"); n != 1 {
		t.Errorf("the wait logged %d times:\n%s", n, logs.String())
	}
	// The list arrives during the wait: the events are named then, and marked.
	nameListWait = time.Minute
	done := make(chan error, 1)
	go func() { done <- bare.c.NameRaw(t.Context()) }()
	time.Sleep(20 * time.Millisecond)
	empty.SetBots(list)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("not named once the list arrived")
	}
	if got := rowNames(t, bare.st); !slices.Equal(got, want) {
		t.Errorf("named once the list arrived\n%v\nwant\n%v", got, want)
	}
	if done, err := bare.st.SvcNamed(t.Context()); err != nil || !done {
		t.Errorf("not marked once the list arrived: %v, %v", done, err)
	}
}

// A failed naming is reported and tried again until it gets through, which clears the report.
func TestRunNames_Retries(t *testing.T) {
	r := namesRig(t)
	want := rowNames(t, r.st)
	if _, err := r.st.DB.Exec(`UPDATE event SET svc = ''`); err != nil {
		t.Fatal(err)
	}
	// A database that refuses the renames, as a busy one would.
	if _, err := r.st.DB.Exec(`CREATE TRIGGER held BEFORE UPDATE OF svc ON event BEGIN
		SELECT RAISE(ABORT, 'held'); END`); err != nil {
		t.Fatal(err)
	}
	defer func(d time.Duration) { nameRetry = d }(nameRetry)
	nameRetry = 10 * time.Millisecond
	board := alert.NewBoard(nil)
	r.c.Report = board
	job := func() (alert.Condition, bool) {
		for _, c := range board.Conditions() {
			if c.Kind == alert.KindJobFailed && c.Key == alert.JobNames {
				return c, true
			}
		}
		return alert.Condition{}, false
	}
	done := make(chan struct{})
	go func() { defer close(done); r.c.RunNames(t.Context()) }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if c, ok := job(); ok && c.Open {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no failure reported")
		}
	}
	if _, err := r.st.DB.Exec(`DROP TRIGGER held`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("not through after the database took writes again")
	}
	if c, ok := job(); !ok || c.Open {
		t.Errorf("report after the retry: %+v", c)
	}
	if got := rowNames(t, r.st); !slices.Equal(got, want) {
		t.Errorf("named\n%v\nwant\n%v", got, want)
	}
	if marked, err := r.st.SvcNamed(t.Context()); err != nil || !marked {
		t.Errorf("not marked: %v, %v", marked, err)
	}
}

// A bot list that does not load is reported past the wait, and the events named once it does.
func TestRunNames_NoBotList(t *testing.T) {
	r := namesRig(t)
	want := rowNames(t, r.st)
	if _, err := r.st.DB.Exec(`UPDATE event SET svc = ''`); err != nil {
		t.Fatal(err)
	}
	defer func(poll, wait, retry time.Duration) {
		nameListPoll, nameListWait, nameRetry = poll, wait, retry
	}(nameListPoll, nameListWait, nameRetry)
	nameListPoll, nameListWait, nameRetry = 5*time.Millisecond, 20*time.Millisecond, 10*time.Millisecond
	list := r.c.Ref.Bots()
	empty := refdata.New(refdata.Options{Dir: t.TempDir()})
	r.c.Ref = empty
	board := alert.NewBoard(nil)
	r.c.Report = board
	job := func() (alert.Condition, bool) {
		for _, c := range board.Conditions() {
			if c.Kind == alert.KindJobFailed && c.Key == alert.JobNames {
				return c, true
			}
		}
		return alert.Condition{}, false
	}
	done := make(chan struct{})
	go func() { defer close(done); r.c.RunNames(t.Context()) }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if c, ok := job(); ok && c.Open {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no failure reported")
		}
	}
	empty.SetBots(list)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("not through once the list loaded")
	}
	if c, ok := job(); !ok || c.Open {
		t.Errorf("report once the list loaded: %+v", c)
	}
	if got := rowNames(t, r.st); !slices.Equal(got, want) {
		t.Errorf("named\n%v\nwant\n%v", got, want)
	}
}

func aggVersion(t *testing.T, r *rig, day int64) int64 {
	t.Helper()
	days, err := r.st.AggDays(t.Context(), r.site.ID, day, day)
	if err != nil {
		t.Fatal(err)
	}
	return days[day].Version
}
