// Command server runs Trakk: the collector, the background work (classification, reference data, aggregates, the
// watch, retention, the model) and the dashboard.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/trick77/ship/kit/auth"
	"github.com/trick77/ship/kit/httpx"
	"github.com/trick77/ship/kit/logging"
	"github.com/trick77/ship/kit/run"
	"github.com/trick77/ship/kit/version"
	"github.com/trick77/toolkit/llmwire"

	"github.com/trick77/trakk/backend/internal/agg"
	"github.com/trick77/trakk/backend/internal/alert"
	"github.com/trick77/trakk/backend/internal/alert/dispatch"
	"github.com/trick77/trakk/backend/internal/app"
	"github.com/trick77/trakk/backend/internal/classify"
	"github.com/trick77/trakk/backend/internal/dash"
	"github.com/trick77/trakk/backend/internal/ingest"
	"github.com/trick77/trakk/backend/internal/llm"
	"github.com/trick77/trakk/backend/internal/refdata"
	"github.com/trick77/trakk/backend/internal/sim"
	"github.com/trick77/trakk/backend/internal/sim/simcli"
	"github.com/trick77/trakk/backend/internal/sites"
	"github.com/trick77/trakk/backend/internal/store"
	"github.com/trick77/trakk/backend/internal/watch"
	"github.com/trick77/trakk/backend/web"
)

var (
	// The container healthcheck: distroless has no wget.
	healthcheckFlag = flag.Bool("healthcheck", false, "probe the local /readyz and exit 0 (ready) or 1 (not)")
	newKeyFlag      = flag.Bool("new-feed-key", false, "print a new feed key and the hash the sites file takes, then exit")
	demoFlag        = flag.Int("demo", 0, "fill an empty database with this many days of simulated traffic on the configured sites (development)")
	simFlag         = flag.Bool("sim", false, "serve a simulated world from a temporary data directory, sign-in off (development; BACKEND_SIM_* knobs, README)")
	notifyCheck     = flag.Bool("notify-check", false, "check the notify file (BACKEND_NOTIFY_FILE) and exit 0 (no fault) or 1; sends nothing")
	notifyTest      = flag.Bool("notify-test", false, "send a test message to every channel of the notify file, print each one's outcome and exit 0 (all sent) or 1")
)

// newSenders makes each channel's sender, the seam the shoutrrr-backed one (toolkit/notify) plugs into. Until it
// does, no channel has one: nothing is sent, the alerts are recorded and shown all the same.
var newSenders = func(alert.Config) map[string]dispatch.Sender { return nil }

func main() {
	// server sim <command>: the simulator's own command line (backfill, live, export, check).
	if len(os.Args) > 1 && os.Args[1] == "sim" {
		ctx, stop := run.Context(context.Background())
		code := simcli.Main(ctx, os.Args[2:], os.Getenv, os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	}
	flag.Parse()
	if *newKeyFlag {
		key, hash := newFeedKey()
		fmt.Printf("key (the product's feed emitter sends it as Bearer): %s\nfeed_key_sha256 (sites file): %s\n", key, hash)
		return
	}
	cfg, err := app.LoadConfig(os.Getenv)
	if err != nil {
		slog.Error("config", logging.Err(err))
		os.Exit(1)
	}
	if *healthcheckFlag {
		os.Exit(httpx.Healthcheck(httpx.LocalURL(cfg.Addr, "/readyz")))
	}
	if *notifyCheck || *notifyTest {
		ctx, stop := run.Context(context.Background())
		code := notifyCmd(ctx, cfg, *notifyTest, os.Stdout)
		stop()
		os.Exit(code)
	}
	mode, err := startMode(cfg, *simFlag, *demoFlag)
	if err != nil {
		slog.Error("flags", logging.Err(err))
		os.Exit(1)
	}
	var simulated *simcli.Server
	if mode == simRun {
		k := sim.ReadKnobs(os.Getenv, func(s string) { slog.Warn(s) })
		if simulated, err = simcli.NewServer(k, time.Now()); err == nil {
			err = simulated.Configure(&cfg)
		}
		if err != nil {
			slog.Error("sim", logging.Err(err))
			if simulated != nil {
				simulated.Close()
			}
			os.Exit(1)
		}
	}
	err = serve(cfg, mode, simulated)
	if simulated != nil {
		simulated.Close()
	}
	if err != nil {
		slog.Error("fatal", logging.Err(err))
		os.Exit(1)
	}
}

// runMode is how the server was started: a normal run, or one of the simulator's development modes.
type runMode int

const (
	normalRun runMode = iota
	simRun            // -sim: a world of its own in a temporary data directory
	demoRun           // -demo: simulated history in the configured data directory
)

// startMode reads the flags against the configuration. The simulator's modes are development only: refused
// together (both would backfill, one world on the other) and where a public host is configured.
func startMode(cfg app.Config, simulate bool, demoDays int) (runMode, error) {
	switch {
	case simulate && demoDays != 0:
		return normalRun, errors.New("-sim and -demo together: -sim serves a world of its own, -demo fills the " +
			"configured data directory; choose one")
	case demoDays < 0:
		return normalRun, fmt.Errorf("-demo %d: days below 0", demoDays)
	case simulate:
		return simRun, simcli.RefusePublic(cfg.PublicFQDN, "server -sim")
	case demoDays > 0:
		return demoRun, simcli.RefusePublic(cfg.PublicFQDN, "server -demo")
	}
	return normalRun, nil
}

// verifyBots reports whether the classifier confirms bots' claims by reverse DNS: always in a normal run, whatever
// its data holds. Not in the simulator's modes: their bots send from documentation addresses, which have no rDNS
// (a lookup would refute what the backfill classed).
func verifyBots(mode runMode) bool { return mode == normalRun }

// checkDataDir is what a normal run makes of what the simulator left in its data directory, decided by the
// simulator's markers, never by what the data looks like: its made-up reference data is discarded (the real data is
// fetched in its place), and a directory it wrote is refused for a deployment and named loudly otherwise. The
// simulator's own modes keep both.
func checkDataDir(cfg app.Config, mode runMode) error {
	if mode != normalRun {
		return nil
	}
	refDir := filepath.Join(cfg.DataDir, "ref")
	if sim.SimulatedReference(refDir) {
		if err := sim.DiscardReference(refDir); err != nil {
			return err
		}
		if cfg.RefDataDisabled {
			slog.Warn("The reference cache held the simulator's made-up data: discarded, and with "+
				"BACKEND_REFDATA_DISABLED=true nothing replaces it (server -demo keeps it)", "dir", refDir)
		} else {
			slog.Warn("The reference cache held the simulator's made-up data: discarded, the real data is fetched",
				"dir", refDir)
		}
	}
	if sim.Owned(cfg.DataDir) {
		if cfg.PublicFQDN != "" {
			return fmt.Errorf("%s was written by the simulator (%s): its events are synthetic, a deployment never "+
				"runs on it", cfg.DataDir, sim.OwnerFile)
		}
		slog.Warn("SIMULATED DATA: this data directory was written by the simulator, its events are synthetic "+
			"(server -demo serves it as such; delete the directory for a real one)", "data_dir", cfg.DataDir,
			"marker", sim.OwnerFile)
	}
	return nil
}

func newFeedKey() (key, hash string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	key = hex.EncodeToString(b)
	return key, sites.HashKey(key)
}

func serve(cfg app.Config, mode runMode, simulated *simcli.Server) error {
	if _, err := logging.Setup(logging.Options{Level: cfg.LogLevel, LevelVar: "BACKEND_LOG_LEVEL"}); err != nil {
		return err
	}
	slog.Info("starting Trakk", "version", version.String(), "public_fqdn", cfg.PublicFQDN, "data_dir", cfg.DataDir,
		"conf_dir", cfg.ConfDir)
	if err := checkDataDir(cfg, mode); err != nil {
		return err
	}

	ctx, stop := run.Context(context.Background())
	defer stop()

	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, store.FileName))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	reg, err := loadSites(ctx, cfg, st)
	if err != nil {
		return err
	}
	if mode == demoRun {
		if err := seedDemo(ctx, st, cfg, *demoFlag); err != nil {
			return err
		}
	}
	if simulated != nil {
		if err := simulated.Backfill(ctx, st); err != nil {
			return err
		}
	}

	// The jobs report their failures and the size cap to the board; the alert engine reads it.
	board := alert.NewBoard(nil)
	ref := refdata.New(refdata.Options{Dir: filepath.Join(cfg.DataDir, "ref"), Report: board})
	verifier := refdata.NewVerifier(nil, 3*time.Second)
	cls := &classify.Classifier{DB: st, Sites: reg, Ref: ref, Verifier: verifier, Report: board}
	if !verifyBots(mode) {
		cls.Verifier = nil
	}
	counters := ingest.NewCounters(nil)
	writer := ingest.NewWriter(st, counters, 0, cls.Enrich, nil)
	writer.Report = board
	collector := &ingest.Collector{Sites: reg, Writer: writer, Counters: counters,
		Browser: ingest.BrowserTiers(cfg.TrustedHops, nil), Feed: ingest.FeedLimiter(nil), TrustedHops: cfg.TrustedHops}

	a, err := app.NewAuth(cfg)
	if err != nil {
		return err
	}
	dashboard, err := web.Handler(app.DashboardPath)
	if err != nil {
		return err
	}
	tracker, err := web.Tracker()
	if err != nil {
		return err
	}
	client, err := llm.FromEnv(cfg.LLMModel, llmwire.Config{})
	if err != nil {
		return fmt.Errorf("model: %w", err)
	}
	if client == nil {
		slog.Info("No model configured (LLMWIRE_* key unset): the dashboard shows computed text only")
	}
	model := llm.New(st, llm.Options{Client: client, Model: cfg.LLMModel, DailyCalls: int64(cfg.LLMDailyCalls),
		DailyTokens: int64(cfg.LLMDailyTokens)})
	notify := loadNotify(cfg, reg)
	senders := newSenders(notify)
	if simulated != nil {
		senders = simulated.Recorder.Senders(notify)
	}
	// The rules read through a dashboard server of their own: its clock is the alert step's.
	reads := &dash.Server{DB: st, Sites: reg, Ref: ref, Counters: counters, RetentionMonths: cfg.RetentionMonths,
		RawDays: cfg.RawRetentionDays}
	alerts := dispatch.New(dispatch.Options{Store: st, Sites: reg, Config: notify, Senders: senders, Board: board,
		Source: dispatch.NewEval(reads, notify.Rules, time.Now(), model), PublicFQDN: cfg.PublicFQDN})
	ds := &dash.Server{DB: st, Sites: reg, Ref: ref, Counters: counters, Model: model,
		RetentionMonths: cfg.RetentionMonths, RawDays: cfg.RawRetentionDays, Build: version.String(), Notify: alerts}

	// Background work runs on its own context: the server drains first, then the writer flushes what the drained
	// requests queued.
	bg, stopBG := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	start := func(f func(context.Context)) {
		wg.Add(1)
		go func() { defer wg.Done(); f(bg) }()
	}
	start(writer.Run)
	start(func(c context.Context) { flushCounters(c, counters, st) })
	// The classifier's first pass re-decides yesterday and today: it waits for the cached reference data (the AS
	// table takes seconds), or it would miss bot ranges and hosting networks.
	refCached := make(chan struct{})
	start(func(c context.Context) {
		ref.LoadCached()
		close(refCached)
		if !cfg.RefDataDisabled {
			ref.Run(c)
		}
	})
	start(func(c context.Context) {
		select {
		case <-refCached:
			cls.Run(c)
		case <-c.Done():
		}
	})
	// The aggregate job backfills every day with raw after an upgrade, then builds each day as it falls due. It waits
	// for the reference data as the classifier does: the start's own reads first.
	aggOpts := agg.Options{Months: cfg.RetentionMonths, Report: board}
	start(func(c context.Context) {
		select {
		case <-refCached:
			agg.Run(c, st, aggOpts)
		case <-c.Done():
		}
	})
	// The watch job walks raw into its state per site (every day with raw after an upgrade, then each as it closes)
	// and judges the open days on every wake. It waits as the aggregate job does.
	start(func(c context.Context) {
		select {
		case <-refCached:
			watch.Run(c, st, watch.Options{})
		case <-c.Done():
		}
	})
	start(model.Run)
	start(verifier.Run)
	// The alerts' sends end with the background context: the shutdown aborts them, their rows stay due.
	start(alerts.Run)
	// Retention purges raw only from days the job built clean; the cap builds a day through Cover before it deletes
	// the day's raw.
	start(func(c context.Context) {
		st.RunRetention(c, store.RetentionOptions{Months: cfg.RetentionMonths, RawDays: cfg.RawRetentionDays,
			Builder: agg.Version, Cover: agg.Cover(st, aggOpts), MaxBytes: int64(cfg.MaxDBMB) << 20, Report: board})
	})
	if cfg.OIDCConfigured() && !cfg.AuthDisabled {
		start(func(c context.Context) {
			a.Discover(c, auth.OIDCConfig{Issuer: cfg.OIDCIssuer, ClientID: cfg.OIDCClientID,
				ClientSecret: cfg.OIDCClientSecret, RedirectURL: cfg.OIDCRedirectURL, Group: cfg.OIDCAdminGroup,
				CookiePrefix: app.CookiePrefix, Secure: cfg.Secure()})
		})
	}

	var dashAPI http.Handler = ds
	if simulated != nil {
		dashAPI = simulated.Wrap(ds, sim.Handlers{Browser: http.HandlerFunc(collector.HandleBrowser),
			Feed: http.HandlerFunc(collector.HandleFeed)})
	}
	srv := httpx.NewServer(cfg.Addr, app.Build(app.Routes{TrustedHops: cfg.TrustedHops, Dashboard: dashboard, Tracker: tracker,
		Browser: http.HandlerFunc(collector.HandleBrowser), Feed: http.HandlerFunc(collector.HandleFeed), Dash: dashAPI, Auth: a,
		Ready: []httpx.Check{{Name: "db", Run: st.Ping}}}))
	slog.Info("listening", "addr", cfg.Addr, "sites", len(reg.All()))
	err = httpx.Serve(ctx, srv, httpDrain)
	if !stopWithin(stopBG, &wg, bgStop) {
		slog.Warn("background work still running at the shutdown budget, the writer's tail may be lost")
	}
	fctx, cancel := context.WithTimeout(context.Background(), counterFlush)
	if ferr := counters.Flush(fctx, st); ferr != nil {
		slog.Warn("final counter flush failed", logging.Err(ferr))
	}
	cancel()
	// Past the drain deadline Shutdown cut the remaining connections; the process still exits as asked.
	if errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("shutdown deadline passed, open connections cut")
		return nil
	}
	return err
}

// The shutdown budget, in order: the HTTP drain (requests in flight finish and queue their events), the background
// stop (the writer's write under way and its drain of the queue, ingest.StopTime, plus a margin for the rest: the
// alerts' sends are aborted, not waited for, as a channel may take 45 s), the final counter flush. Their sum stays under compose.yaml's stop_grace_period, after which Docker kills the process.
const (
	httpDrain      = 10 * time.Second
	bgStop         = ingest.StopTime + 2*time.Second
	counterFlush   = 3 * time.Second
	shutdownBudget = httpDrain + bgStop + counterFlush
)

// stopWithin cancels the background work and waits for it at most limit; false when it did not stop in time.
func stopWithin(stop func(), wg *sync.WaitGroup, limit time.Duration) bool {
	stop()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	t := time.NewTimer(limit)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

func loadSites(ctx context.Context, cfg app.Config, st *store.Store) (*sites.Registry, error) {
	path, legacy := cfg.SitesPath()
	if legacy {
		slog.Warn("Sites file read from the data directory, where it no longer belongs: move it to the conf directory "+
			"(compose.yaml mounts ./conf at /conf), the fallback goes in a later release", "sites_file", path,
			"move_to", cfg.SitesFile)
	}
	cs, err := sites.Load(path)
	if err != nil {
		return nil, err
	}
	switch _, statErr := os.Stat(path); {
	case errors.Is(statErr, fs.ErrNotExist):
		slog.Warn("Sites file missing: the collector refuses every event and the dashboard lists no domain until it "+
			"exists (docs/sites.example.json in the repo, restart after writing it)", "sites_file", path)
	case len(cs) == 0:
		slog.Warn("Sites file lists no site: the collector refuses every event", "sites_file", path)
	default:
		domains := make([]string, 0, len(cs))
		for _, c := range cs {
			domains = append(domains, c.Domain)
		}
		slog.Info("Sites loaded", "sites_file", path, "domains", strings.Join(domains, ","))
	}
	rows, err := st.SyncSites(ctx, sites.Rows(cs))
	if err != nil {
		return nil, err
	}
	return sites.New(rows)
}

// flushCounters writes the collector's counters every half minute.
func flushCounters(ctx context.Context, c *ingest.Counters, st *store.Store) {
	for run.Sleep(ctx, 30*time.Second) {
		if err := c.Flush(ctx, st); err != nil {
			slog.Warn("counter flush failed, kept for the next one", logging.Err(err))
		}
	}
}

// seedDemo fills an empty database with simulated traffic on the configured sites, enriched and classified with
// the data directory's cached reference data. Without a cache, the world's own is written there when the refresh
// is off (the server then enriches as the backfill did); with it on, the backfill takes the world's from a directory
// of its own and the refresh fills the cache.
//
// A database with events is served as it is when the simulator wrote the directory (a second start, or sim
// backfill's directory), and refused otherwise: -demo never mixes simulated traffic into real. A directory it fills
// is marked as the simulator's.
func seedDemo(ctx context.Context, st *store.Store, cfg app.Config, days int) error {
	var n int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM event LIMIT 1)`).Scan(&n); err != nil {
		return err
	}
	switch {
	case n > 0 && sim.Owned(cfg.DataDir):
		slog.Info("Demo: the data directory holds the simulator's history already, served as it is")
		return nil
	case n > 0:
		return fmt.Errorf("-demo refuses %s: its database holds events and the simulator did not write it (no %s); "+
			"-demo fills an empty database only", cfg.DataDir, sim.OwnerFile)
	}
	if err := sim.Mark(cfg.DataDir); err != nil {
		return err
	}
	refDir := filepath.Join(cfg.DataDir, "ref")
	if _, err := os.Stat(filepath.Join(refDir, "crawlers.json")); err != nil && !cfg.RefDataDisabled {
		tmp, err := os.MkdirTemp("", "trakk-demo-ref-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		refDir = tmp
		slog.Warn("Demo traffic is classified with the simulator's reference data, which the refresh then replaces: " +
			"the classifier's first pass re-decides the last two days (BACKEND_REFDATA_DISABLED=true keeps it)")
	}
	stats, err := simcli.Demo(ctx, st, days, refDir, slog.Default())
	if err != nil {
		return err
	}
	slog.Info("Demo traffic written", "days", days, "stats", stats.String())
	return nil
}
