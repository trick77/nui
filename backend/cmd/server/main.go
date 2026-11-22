// Command server serves the embedded dashboard.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"time"

	"github.com/trick77/ship/kit/httpx"
	"github.com/trick77/ship/kit/logging"
	"github.com/trick77/ship/kit/run"
	"github.com/trick77/ship/kit/version"

	"github.com/trick77/nui/backend/internal/app"
	"github.com/trick77/nui/backend/web"
)

// The container healthcheck: distroless has no wget.
var healthcheckFlag = flag.Bool("healthcheck", false, "probe the local /readyz and exit 0 (ready) or 1 (not)")

func main() {
	flag.Parse()
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

func serve(cfg app.Config) error {
	if _, err := logging.Setup(logging.Options{Level: cfg.LogLevel, LevelVar: "BACKEND_LOG_LEVEL"}); err != nil {
		return err
	}
	slog.Info("starting nui", "version", version.String(), "public_fqdn", cfg.PublicFQDN, "data_dir", cfg.DataDir)

	ctx, stop := run.Context(context.Background())
	defer stop()

	dashboard, err := web.Handler(app.DashboardPath)
	if err != nil {
		return err
	}
	srv := httpx.NewServer(cfg.Addr, app.Build(dashboard))

	slog.Info("listening", "addr", cfg.Addr)
	err = httpx.Serve(ctx, srv, 10*time.Second)
	// Past the drain deadline Shutdown cut the remaining connections; the process still exits as asked.
	if errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("shutdown deadline passed, open connections cut")
		return nil
	}
	return err
}
