// Package app holds the configuration and wires the router and middleware stack.
package app

import (
	"log/slog"
	"net/http"

	"github.com/trick77/ship/kit/httpx"
)

// DashboardPath is where the SPA lives. The server mounts it itself, no StripPrefix at the proxy: the collector
// shares the host under /api.
const DashboardPath = "/dashboard"

// TrustedHops is the number of proxies in front of the server: one Traefik. Running without a proxy means 0.
const TrustedHops = 1

// ClientIP is the caller's address as the trusted proxy saw it.
func ClientIP(r *http.Request) string { return httpx.ClientIP(r, TrustedHops) }

// Build returns the root handler; dashboard serves the UI with DashboardPath stripped (web.Handler).
func Build(dashboard http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", http.RedirectHandler(DashboardPath+"/", http.StatusFound))
	// The bare /dashboard redirects to /dashboard/ (the mux does that itself). No catch-all: /api and every other
	// path outside the dashboard is the mux's 404, never the shell.
	mux.Handle("GET "+DashboardPath+"/", http.StripPrefix(DashboardPath, dashboard))
	// /healthz and /readyz answer only a direct local caller (the binary's own -healthcheck); through the proxy
	// they are 404.
	httpx.Health(mux, slog.Default())
	return chain(mux)
}

// chain wraps h in the middleware every request passes. SecurityHeaders sits outside Recover, so a panic's 500
// keeps its headers.
func chain(h http.Handler) http.Handler {
	return httpx.Chain(h,
		httpx.AccessLog(slog.Default(), httpx.AccessLogOptions{ClientIP: ClientIP, RoutineLevel: slog.LevelInfo}),
		httpx.SecurityHeadersWith(httpx.SecurityOptions{HTMLOnly: true}),
		httpx.Recover(slog.Default()),
	)
}
