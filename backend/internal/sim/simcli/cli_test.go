package simcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/apix"
	"github.com/trick77/trakk/backend/internal/app"
	"github.com/trick77/trakk/backend/internal/ingest"
	"github.com/trick77/trakk/backend/internal/sim"
	"github.com/trick77/trakk/backend/internal/sites"
	"github.com/trick77/trakk/backend/internal/store"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func run(t *testing.T, vars map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errs bytes.Buffer
	code := Main(t.Context(), args, env(vars), &out, &errs)
	return code, out.String(), errs.String()
}

// The export answers every request with valid JSON, the same bytes for the same seed, and names them in its index.
func TestExport(t *testing.T) {
	t.Parallel()
	// The ranges exported are the package's tests' (TestMain).
	vars := map[string]string{"BACKEND_SIM_DAYS": "3", "BACKEND_SIM_SCALE": "0.3", "BACKEND_SIM_SCENARIOS": "api-walker"}
	if raceOn {
		vars["BACKEND_SIM_DAYS"], vars["BACKEND_SIM_SCALE"] = "1", "0.1"
	}
	dirs := []string{filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")}
	for _, d := range dirs {
		if code, out, errs := run(t, vars, "export", "-out", d); code != 0 || !strings.Contains(out, "exported") {
			t.Fatalf("export: %d %s %s", code, out, errs)
		}
	}
	b, err := os.ReadFile(filepath.Join(dirs[0], "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx ExportIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Responses) < 100 || idx.World["anchor"] != FixtureAnchor.Format(time.RFC3339) {
		t.Fatalf("index: %d answers, world %v", len(idx.Responses), idx.World)
	}
	for _, k := range []string{"sites", "overview?aud=bots&cmp=yoy&range=24h&site=lens.example.com",
		"visitor?aud=all&cmp=prev&key=203.0.113.5&range=24h&site=lens.example.com",
		"watch?aud=all&cmp=prev&range=24h&site=console.example.com",
		"experience?aud=all&cmp=prev&range=24h&site=console.example.com",
		"list?aud=all&cmp=prev&dim=experience&limit=6&range=24h&site=lens.example.com&sort=volume"} {
		if e, ok := idx.Responses[k]; !ok || e.Status != http.StatusOK {
			t.Errorf("%s: %+v", k, e)
		}
	}
	// A site without a feed answers the server-only panels 404, as the server does.
	if e := idx.Responses["classes?aud=all&cmp=prev&range=24h&site=console.example.com"]; e.Status != http.StatusNotFound {
		t.Errorf("classes without a feed: %+v", e)
	}
	// An API has no pages to experience.
	if e := idx.Responses["experience?aud=all&cmp=prev&range=24h&site=api.example.com"]; e.Status != http.StatusNotFound {
		t.Errorf("experience of an API: %+v", e)
	}
	for k, e := range idx.Responses {
		a, err := os.ReadFile(filepath.Join(dirs[0], e.File))
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid(a) {
			t.Fatalf("%s: not JSON", k)
		}
		if b, _ := os.ReadFile(filepath.Join(dirs[1], e.File)); !bytes.Equal(a, b) {
			t.Fatalf("%s differs between two exports of one seed:\n%s\n%s", k, a, b)
		}
	}
	if Key("log", map[string]string{"tz": "Europe/Zurich", "site": "x", "q": ""}) != "log?site=x" {
		t.Error("key keeps the zone or an empty value")
	}
}

func TestCommands(t *testing.T) {
	t.Parallel()
	if code, _, errs := run(t, nil); code != 2 || !strings.Contains(errs, "usage") {
		t.Errorf("no command: %d", code)
	}
	if code, _, _ := run(t, nil, "nope"); code != 2 {
		t.Errorf("unknown command: %d", code)
	}
	if code, _, _ := run(t, nil, "backfill", "-nope"); code != 2 {
		t.Errorf("unknown flag: %d", code)
	}
	for _, args := range [][]string{{"backfill"}, {"export"}, {"live"}, {"live", "-target", "x", "-key", "a"}} {
		if code, _, _ := run(t, nil, args...); code == 0 {
			t.Errorf("%v accepted", args)
		}
	}
	// The flags pass the knobs' ranges: refused at once with the reason, never a panic or an endless loop.
	for _, args := range [][]string{
		{"live", "-target", "http://127.0.0.1:1", "-speed", "0", "-rate", "50"},
		{"live", "-target", "http://127.0.0.1:1", "-rate", "NaN"},
		{"live", "-target", "http://127.0.0.1:1", "-rate", "0"},
		{"live", "-target", "http://127.0.0.1:1", "-rate", "-5"},
		{"live", "-target", "http://127.0.0.1:1", "-for", "-1s"},
		{"export", "-out", t.TempDir(), "-scale", "NaN"},
		{"export", "-out", t.TempDir(), "-scale", "+Inf"},
		{"export", "-out", t.TempDir(), "-scale", "1e9"},
		{"backfill", "-data", t.TempDir(), "-days", "-1"},
		{"check", "-scenarios", "nope"},
	} {
		code, _, errs := run(t, nil, args...)
		if code != 2 || !strings.Contains(errs, "sim: ") {
			t.Errorf("%v: %d %s", args, code, errs)
		}
	}
	// A rate whose scale is out of range says what to change.
	if code, _, errs := run(t, nil, "live", "-target", "http://127.0.0.1:1", "-speed", "0.01", "-rate", "1000"); code != 1 ||
		!strings.Contains(errs, "-speed") {
		t.Errorf("rate beyond the scale: %d %s", code, errs)
	}
	// The port server -sim listens on, as make sim hands it to the UI: the knob, or its default when it is bad.
	for raw, want := range map[string]string{"9123": "9123\n", "x": "8080\n", "70000": "8080\n"} {
		if code, out, _ := run(t, map[string]string{"BACKEND_SIM_PORT": raw}, "port"); code != 0 || out != want {
			t.Errorf("port with BACKEND_SIM_PORT=%s: %d %q", raw, code, out)
		}
	}
	code, out, _ := run(t, nil, "scenarios")
	if code != 0 || !strings.Contains(out, "api-walker (always)") || !strings.Contains(out, "Noteworthy lists the new referrer") {
		t.Errorf("scenarios: %d %s", code, out)
	}
	code, out, errs := run(t, map[string]string{"BACKEND_SIM_DAYS": "2", "BACKEND_SIM_SCALE": "0.3",
		"BACKEND_SIM_SCENARIOS": "cred-prober"}, "check")
	if code != 0 || !strings.Contains(out, "cred-prober: ") {
		t.Errorf("check: %d %s %s", code, out, errs)
	}
	// Backfill into an absent data directory: its sites file and reference data written, both marked as the
	// simulator's; a second run refused (it would double the history), one with -wipe written anew.
	dir := filepath.Join(t.TempDir(), "data")
	vars := map[string]string{"BACKEND_SIM_DAYS": "1", "BACKEND_SIM_SCALE": "0.2", "BACKEND_SIM_SEED": "x"}
	backfill := func(args ...string) (int, string, string) {
		return run(t, vars, append([]string{"backfill", "-data", dir, "-seed", "3"}, args...)...)
	}
	if code, out, errs := backfill(); code != 0 || !strings.Contains(out, "backfilled 1 days") ||
		!strings.Contains(errs, "BACKEND_SIM_SEED=x") {
		t.Fatalf("backfill: %d %s %s", code, out, errs)
	}
	for _, f := range []string{"app.db", "sites.json", "ref/crawlers.json", "ref/as-ip-blocks.tar.gz", sim.OwnerFile,
		"ref/" + sim.RefMarkerFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Error(err)
		}
	}
	if code, _, errs := backfill(); code != 1 || !strings.Contains(errs, "-wipe") {
		t.Errorf("a second backfill: %d %s", code, errs)
	}
	if code, out, errs := backfill("-wipe"); code != 0 || !strings.Contains(out, "backfilled") {
		t.Errorf("-wipe: %d %s %s", code, out, errs)
	}
	// A directory the simulator did not write is refused and left as it was, -wipe or not; so is any directory
	// where a deployment is configured.
	foreign := t.TempDir()
	if err := os.WriteFile(filepath.Join(foreign, "sites.json"), []byte(`{"sites":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"backfill", "-data", foreign}, {"backfill", "-data", foreign, "-wipe"}} {
		if code, _, errs := run(t, vars, args...); code != 1 || !strings.Contains(errs, "not written by the simulator") {
			t.Errorf("%v: %d %s", args, code, errs)
		}
	}
	if entries, _ := os.ReadDir(foreign); len(entries) != 1 {
		t.Errorf("the refused directory changed: %v", entries)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if code, _, errs := run(t, map[string]string{"PUBLIC_FQDN": "trakk.example.com"}, "backfill", "-data", empty); code != 1 ||
		!strings.Contains(errs, "PUBLIC_FQDN") {
		t.Errorf("a public host: %d %s", code, errs)
	}
	if _, err := os.Stat(empty); err == nil {
		t.Error("a refused backfill made its directory")
	}
}

// check fails on any failure, the baseline's in a scenario's own run too, and says what it checked. The counting is
// under test, not the dashboard (TestScenarioExpectations): a stub fails one baseline expectation, over the smallest
// histories.
func TestCheckCountsTheBaseline(t *testing.T) {
	t.Cleanup(func() { checkWorld = Check })
	checkWorld = func(_ context.Context, _ *store.Store, w *sim.World) ([]Failure, error) {
		for _, e := range w.Expectations() {
			if e.Scenario == "baseline" {
				return []Failure{{Expectation: e, Err: errors.New("broken on purpose")}}, nil
			}
		}
		return nil, nil
	}
	code, out, errs := run(t, map[string]string{"BACKEND_SIM_DAYS": "0", "BACKEND_SIM_SCALE": "0.01",
		"BACKEND_SIM_CLASSIFY": "false"}, "check", "-scenarios", "cred-prober")
	if code != 1 || !strings.Contains(out, "FAIL baseline") || !strings.Contains(out, "the baseline's), 1 failed") ||
		!strings.Contains(errs, "not met") {
		t.Errorf("a baseline failure: %d %s %s", code, out, errs)
	}
}

// Live over HTTP, the rate setting the scale, the feed key from -key.
func TestLiveCommand(t *testing.T) {
	ref, err := sim.OpenReference(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	k := sim.DefaultKnobs()
	w, err := k.World(ref, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	lens, _ := w.Site("lens.example.com")
	lens.FeedKey = "custom-key"
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	reg, err := w.SyncSites(t.Context(), st)
	if err != nil {
		t.Fatal(err)
	}
	counters := ingest.NewCounters(nil)
	c := &ingest.Collector{Sites: reg, Counters: counters, Writer: ingest.NewWriter(st, counters, 0, nil, nil),
		Browser: ingest.BrowserTiers(1, nil), Feed: ingest.FeedLimiter(nil), TrustedHops: 1}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/in", c.HandleBrowser)
	var feeds, unauthorized atomic.Int64
	mux.HandleFunc("POST /api/feed", func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		c.HandleFeed(rec, r)
		if r.Header.Get("Authorization") == "Bearer custom-key" {
			feeds.Add(1)
		}
		if rec.Code == http.StatusUnauthorized {
			unauthorized.Add(1)
		}
		w.WriteHeader(rec.Code)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	code, out, errs := run(t, map[string]string{"BACKEND_SIM_SPEED": "3600"}, "live", "-target", srv.URL, "-for", "1s",
		"-rate", "200", "-key", "lens.example.com=custom-key")
	if code != 0 || !strings.Contains(out, "live:") || feeds.Load() == 0 || unauthorized.Load() != 0 {
		t.Errorf("live: %d %s %s, lens feeds %d, unauthorized %d", code, out, errs, feeds.Load(), unauthorized.Load())
	}
	if code, _, _ := run(t, nil, "live", "-target", srv.URL, "-key", "nope.example.com=k"); code != 1 {
		t.Errorf("a key for no site: %d", code)
	}
	// A wrong key is refused for good: the run stops, non-zero, and says so.
	code, _, errs = run(t, map[string]string{"BACKEND_SIM_SPEED": "3600"}, "live", "-target", srv.URL, "-for", "20s",
		"-key", "lens.example.com=wrong-key")
	if code != 1 || !strings.Contains(errs, "feed key is wrong") {
		t.Errorf("a wrong key: %d %s", code, errs)
	}
}

// The world at the tests' anchor, not the wall clock: the hours a one-day history steps the alert engine through and
// the runs the day's draws place decide what the rules see.
func TestServer(t *testing.T) {
	k := sim.DefaultKnobs()
	k.Start, k.Days, k.Scale, k.Port, k.Speed, k.SlowMS = anchor, 1, 1.5, 9099, 3600, 1
	if raceOn {
		k.Scale = 0.3 // the rules' passes are checked without -race (ship.yaml's step)
	}
	s, err := NewServer(k, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := app.Config{PublicFQDN: "trakk.example.com"}
	if s.Configure(&cfg) == nil {
		t.Error("a public host accepted")
	}
	cfg = app.Config{}
	if err := s.Configure(&cfg); err != nil || !cfg.AuthDisabled || !cfg.RefDataDisabled || cfg.Addr != "127.0.0.1:9099" ||
		cfg.DataDir != s.Dir || cfg.ConfDir != s.Dir || cfg.TrustedHops != 1 || !sim.Owned(s.Dir) ||
		cfg.NotifyFile != filepath.Join(s.Dir, "notify.json") {
		t.Fatalf("config %+v, %v", cfg, err)
	}
	st, err := store.Open(t.Context(), filepath.Join(cfg.DataDir, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	cs, err := sites.Load(cfg.SitesFile)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.SyncSites(t.Context(), sites.Rows(cs))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Backfill(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	// The alert engine stepped through the history: every rule of the world's notify file passed but the requests'
	// drop, whose baseline waits for days to compare with that one day of history lacks: known (checked at 0), not
	// passed yet. The API's failing level waits for its 20 calls an hour as well, which a stepped hour has only with an
	// integration's run in it. Hourly anchors over two weeks, seed 1, without that pass: 152 of 336 at scale 0.3, 7 of
	// 336 at 1, 3 of 1008 at 1.5 (minutes 07, 30, 52). The anchor's is one of those that have it.
	if waiting := unpassed(t, st); !raceOn && !slices.Equal(waiting, []string{"requests-drop"}) {
		t.Errorf("rules without a pass: %v, want [requests-drop]", waiting)
	}
	reg, _ := sites.New(rows)
	counters := ingest.NewCounters(nil)
	c := &ingest.Collector{Sites: reg, Counters: counters, Writer: ingest.NewWriter(st, counters, 0, nil, nil),
		Browser: ingest.BrowserTiers(cfg.TrustedHops, nil), Feed: ingest.FeedLimiter(nil), TrustedHops: cfg.TrustedHops}
	h := s.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }),
		sim.Handlers{Browser: http.HandlerFunc(c.HandleBrowser), Feed: http.HandlerFunc(c.HandleFeed)})
	time.Sleep(500 * time.Millisecond) // half an hour of the world after serving began: the anchor's afternoon
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/dash/sites", nil))
	if rec.Code != http.StatusTeapot || c.Writer.Queued() == 0 || rec.Header().Get(PIDHeader) != strconv.Itoa(os.Getpid()) {
		t.Errorf("wrapped: %d, queued %d", rec.Code, c.Writer.Queued())
	}
}

// unpassed are the notify file's rules without a pass in st, in the file's order.
func unpassed(t *testing.T, st *store.Store) []string {
	t.Helper()
	var nf struct{ Rules []struct{ Name string } }
	if err := json.Unmarshal([]byte(NotifyFile), &nf); err != nil {
		t.Fatal(err)
	}
	rows, err := st.DB.Query(`SELECT DISTINCT rule FROM alert_rule WHERE checked_at > 0`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	passed := map[string]bool{}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		passed[r] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range nf.Rules {
		if !passed[r.Name] {
			out = append(out, r.Name)
		}
	}
	return out
}

// Without BACKEND_SIM_START (server -sim, make sim) the world is anchored at the now it is given.
func TestServerAnchorsAtNow(t *testing.T) {
	t.Parallel()
	now := anchor.Add(-3*time.Hour - 17*time.Minute)
	s, err := NewServer(sim.DefaultKnobs(), now)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.World.Anchor.Equal(now) {
		t.Errorf("anchor %v, want %v", s.World.Anchor, now)
	}
}

// server -demo: the configured sites, the world's reference data written where there was none.
func TestDemo(t *testing.T) {
	t.Parallel()
	ref, err := sim.OpenReference(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := sim.DefaultKnobs().World(ref, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.SyncSites(t.Context(), sites.Rows(w.SitesFile().Sites)); err != nil {
		t.Fatal(err)
	}
	refDir := filepath.Join(t.TempDir(), "ref")
	stats, err := Demo(t.Context(), st, 1, refDir, nil)
	if err != nil || stats.Events == 0 {
		t.Errorf("demo: %v, %v", stats, err)
	}
	if _, err := os.Stat(filepath.Join(refDir, "crawlers.json")); err != nil {
		t.Error(err)
	}
}

// BACKEND_SIM_START is the world's anchor, not the wall time live mode begins: in the past or the future, the live
// world starts at the anchor when serving begins, so the behaviours a minute after the start come a minute later.
func TestServerStartsLiveAtTheAnchor(t *testing.T) {
	for _, start := range []time.Time{time.Now().Add(-7 * 24 * time.Hour), time.Now().Add(7 * 24 * time.Hour)} {
		k := sim.DefaultKnobs()
		k.Start, k.Scale, k.FeedOutage = start, 0.1, 10*time.Minute
		s, err := NewServer(k, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		s.Wrap(http.NotFoundHandler(), sim.Handlers{})
		got := s.live.WorldTime(time.Now())
		s.Close()
		lens, _ := s.World.Site("lens.example.com")
		if d := got.Sub(s.World.Anchor); d < 0 || d > 5*time.Second || !s.World.FeedDelivers(lens, got) {
			t.Errorf("start %v: world time %v after serving began, %v from the anchor", start, got, d)
		}
	}
}

// sim live against server -sim: each browser's address, its one X-Forwarded-For entry, is the caller the server
// reads, also where the environment trusts no proxy (a local run's BACKEND_TRUSTED_HOPS=0).
func TestServerReadsSimulatedAddresses(t *testing.T) {
	k := sim.DefaultKnobs()
	k.Start, k.Scale = anchor, 3 // the afternoon: the half hour below has visitors
	s, err := NewServer(k, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := app.Config{TrustedHops: 0}
	if err := s.Configure(&cfg); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var browsers, loopback int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a, ok := apix.ClientAddr(r, cfg.TrustedHops); ok && r.URL.Path == "/api/in" {
			mu.Lock()
			browsers++
			if a.IsLoopback() {
				loopback++
			}
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	l := sim.NewLive(s.World, time.Now(), sim.SpeedLimit.Max())
	if _, err := sim.Send(t.Context(), l, sim.SendOptions{Target: srv.URL, For: 500 * time.Millisecond,
		Every: 20 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if browsers == 0 || loopback != 0 {
		t.Errorf("browser batches %d, %d of them from the loopback", browsers, loopback)
	}
}
