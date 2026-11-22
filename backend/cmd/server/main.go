// Command server runs nui: the collector, the background work (classification, reference data, retention, the
// model) and the dashboard.
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

	"github.com/trick77/nui/backend/internal/app"
	"github.com/trick77/nui/backend/internal/classify"
	"github.com/trick77/nui/backend/internal/dash"
	"github.com/trick77/nui/backend/internal/demo"
	"github.com/trick77/nui/backend/internal/ingest"
	"github.com/trick77/nui/backend/internal/llm"
	"github.com/trick77/nui/backend/internal/refdata"
	"github.com/trick77/nui/backend/internal/sites"
	"github.com/trick77/nui/backend/internal/store"
	"github.com/trick77/nui/backend/web"
)

var (
	// The container healthcheck: distroless has no wget.
	healthcheckFlag = flag.Bool("healthcheck", false, "probe the local /readyz and exit 0 (ready) or 1 (not)")
	newKeyFlag      = flag.Bool("new-feed-key", false, "print a new feed key and the hash the sites file takes, then exit")
	demoFlag        = flag.Int("demo", 0, "fill an empty database with this many days of synthetic traffic per site (development)")
)

func main() {
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
	if err := serve(cfg); err != nil {
		slog.Error("fatal", logging.Err(err))
		os.Exit(1)
	}
}

func newFeedKey() (key, hash string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	key = hex.EncodeToString(b)
	return key, sites.HashKey(key)
}

func serve(cfg app.Config) error {
	if _, err := logging.Setup(logging.Options{Level: cfg.LogLevel, LevelVar: "BACKEND_LOG_LEVEL"}); err != nil {
		return err
	}
	slog.Info("starting nui", "version", version.String(), "public_fqdn", cfg.PublicFQDN, "data_dir", cfg.DataDir)

	ctx, stop := run.Context(context.Background())
	defer stop()

	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "nui.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	reg, err := loadSites(ctx, cfg, st)
	if err != nil {
		return err
	}
	if *demoFlag > 0 {
		if err := seedDemo(ctx, st, reg, *demoFlag); err != nil {
			return err
		}
	}

	ref := refdata.New(refdata.Options{Dir: filepath.Join(cfg.DataDir, "ref")})
	cls := &classify.Classifier{DB: st, Sites: reg, Ref: ref, Verifier: refdata.NewVerifier(nil, 3*time.Second)}
	counters := ingest.NewCounters(nil)
	writer := ingest.NewWriter(st, counters, 0, cls.Enrich, nil)
	collector := &ingest.Collector{Sites: reg, Writer: writer, Counters: counters,
		Browser: ingest.BrowserTiers(app.TrustedHops, nil), Feed: ingest.FeedLimiter(nil), TrustedHops: app.TrustedHops}

	a, err := newAuth(cfg)
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
	ds := &dash.Server{DB: st, Sites: reg, Ref: ref, Counters: counters, Model: model,
		RetentionMonths: cfg.RetentionMonths, Build: version.String()}

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
	start(func(c context.Context) {
		ref.LoadCached()
		if !cfg.RefDataDisabled {
			ref.Run(c)
		}
	})
	start(cls.Run)
	start(model.Run)
	start(func(c context.Context) { prune(c, st, cfg) })
	if cfg.OIDCConfigured() && !cfg.AuthDisabled {
		start(func(c context.Context) {
			a.Discover(c, auth.OIDCConfig{Issuer: cfg.OIDCIssuer, ClientID: cfg.OIDCClientID,
				ClientSecret: cfg.OIDCClientSecret, RedirectURL: cfg.OIDCRedirectURL, Group: cfg.OIDCAdminGroup,
				CookiePrefix: app.CookiePrefix, Secure: cfg.Secure()})
		})
	}

	srv := httpx.NewServer(cfg.Addr, app.Build(app.Routes{Dashboard: dashboard, Tracker: tracker,
		Browser: http.HandlerFunc(collector.HandleBrowser), Feed: http.HandlerFunc(collector.HandleFeed), Dash: ds, Auth: a,
		Ready: []httpx.Check{{Name: "db", Run: st.Ping}}}))
	slog.Info("listening", "addr", cfg.Addr, "sites", len(reg.All()))
	err = httpx.Serve(ctx, srv, 10*time.Second)
	stopBG()
	wg.Wait()
	if ferr := counters.Flush(context.Background(), st); ferr != nil {
		slog.Warn("final counter flush failed", logging.Err(ferr))
	}
	// Past the drain deadline Shutdown cut the remaining connections; the process still exits as asked.
	if errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("shutdown deadline passed, open connections cut")
		return nil
	}
	return err
}

func loadSites(ctx context.Context, cfg app.Config, st *store.Store) (*sites.Registry, error) {
	cs, err := sites.Load(cfg.SitesFile)
	if err != nil {
		return nil, err
	}
	switch _, statErr := os.Stat(cfg.SitesFile); {
	case errors.Is(statErr, fs.ErrNotExist):
		slog.Warn("Sites file missing: the collector refuses every event and the dashboard lists no domain until it "+
			"exists (docs/sites.example.json in the repo, restart after writing it)", "sites_file", cfg.SitesFile)
	case len(cs) == 0:
		slog.Warn("Sites file lists no site: the collector refuses every event", "sites_file", cfg.SitesFile)
	default:
		domains := make([]string, 0, len(cs))
		for _, c := range cs {
			domains = append(domains, c.Domain)
		}
		slog.Info("Sites loaded", "sites_file", cfg.SitesFile, "domains", strings.Join(domains, ","))
	}
	rows, err := st.SyncSites(ctx, sites.Rows(cs))
	if err != nil {
		return nil, err
	}
	return sites.New(rows)
}

func newAuth(cfg app.Config) (*app.Auth, error) {
	key := []byte(cfg.SessionSecret)
	if len(key) == 0 {
		slog.Warn("BACKEND_SESSION_SECRET is empty: sessions end with this process")
		key = auth.RandomKey()
	}
	sessions, err := auth.NewSessions(auth.SessionConfig{Name: app.SessionCookie, Key: key, Secure: cfg.Secure(),
		KeepGroup: cfg.OIDCAdminGroup})
	if err != nil {
		return nil, err
	}
	if !cfg.OIDCConfigured() && !cfg.AuthDisabled {
		slog.Warn("OIDC is not configured: the dashboard answers 401 until it is")
	}
	if cfg.AuthDisabled {
		slog.Warn("BACKEND_AUTH_DISABLED: the dashboard is open to anyone reaching this port")
	}
	return &app.Auth{Sessions: sessions, Group: cfg.OIDCAdminGroup, Disabled: cfg.AuthDisabled}, nil
}

// flushCounters writes the collector's counters every half minute.
func flushCounters(ctx context.Context, c *ingest.Counters, st *store.Store) {
	for run.Sleep(ctx, 30*time.Second) {
		if err := c.Flush(ctx, st); err != nil {
			slog.Warn("counter flush failed, kept for the next one", logging.Err(err))
		}
	}
}

// prune runs retention a minute after start and then daily.
func prune(ctx context.Context, st *store.Store, cfg app.Config) {
	wait := time.Minute
	for run.Sleep(ctx, wait) {
		wait = 24 * time.Hour
		n, err := st.Prune(ctx, time.Now().AddDate(0, -cfg.RetentionMonths, 0), int64(cfg.MaxDBMB)<<20)
		if err != nil {
			slog.Warn("retention failed", logging.Err(err))
			continue
		}
		slog.Info("retention done", "deleted", n)
	}
}

// seedDemo fills an empty database with synthetic traffic for every site.
func seedDemo(ctx context.Context, st *store.Store, reg *sites.Registry, days int) error {
	var n int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM event LIMIT 1)`).Scan(&n); err != nil || n > 0 {
		return err
	}
	for i, s := range reg.All() {
		written, err := demo.Generate(ctx, st, demo.Options{SiteID: s.ID, Days: days, People: 25, Seed: uint64(i + 1),
			Feed: s.HasFeed, Classes: true})
		if err != nil {
			return err
		}
		slog.Info("demo traffic written", "site", s.Domain, "events", written)
	}
	return nil
}
