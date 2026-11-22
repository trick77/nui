// Package app holds the configuration and wires the router and middleware stack.
package app

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/trick77/ship/kit/httpx"

	"github.com/trick77/nui/backend/internal/apix"
)

// DashboardPath is where the SPA lives. The server mounts it itself, no StripPrefix at the proxy: the collector
// shares the host under /api.
const DashboardPath = "/dashboard"

// Routes are the handlers Build mounts; nil ones are left out (tests).
type Routes struct {
	// TrustedHops is Config.TrustedHops: the access log names the caller as apix.ClientAddr reads it.
	TrustedHops int
	// Dashboard serves the UI with DashboardPath stripped (web.Handler).
	Dashboard http.Handler
	// Tracker serves /nui.js (web.Tracker).
	Tracker http.Handler
	// Browser and Feed are the collector's POST /api/in and POST /api/feed.
	Browser, Feed http.Handler
	// Dash serves GET /api/dash/{endpoint}.
	Dash http.Handler
	Auth *Auth
	// Ready are the readiness checks.
	Ready []httpx.Check
}

// Build returns the root handler.
func Build(rt Routes) http.Handler {
	mux := http.NewServeMux()
	// The bare /dashboard redirects to /dashboard/ (the mux does that itself). No catch-all: /api and every other
	// path outside the dashboard, the root included, is the mux's 404, never the shell or a pointer to it: the bare
	// host does not lead to the dashboard.
	if rt.Dashboard != nil {
		mux.Handle("GET "+DashboardPath+"/", http.StripPrefix(DashboardPath, rt.Dashboard))
	}
	// /healthz and /readyz answer only a direct local caller (the binary's own -healthcheck); through the proxy
	// they are 404.
	httpx.Health(mux, slog.Default(), rt.Ready...)

	// The collector and the tracker are called from other sites' pages: outside the CSRF guard. The browser
	// endpoint is a CORS-simple POST (text/plain), open to any origin and never with credentials.
	if rt.Tracker != nil {
		mux.Handle("GET /nui.js", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
			rt.Tracker.ServeHTTP(w, r)
		}))
	}
	if rt.Browser != nil {
		cors := httpx.CORS(httpx.CORSOptions{Methods: []string{http.MethodPost}})
		mux.Handle("/api/in", cors(postOnly(rt.Browser)))
	}
	if rt.Feed != nil {
		mux.Handle("POST /api/feed", rt.Feed)
	}

	// The dashboard's API and sign-in keep the CSRF guard (kit's CrossOrigin: a cross-site POST riding the session
	// cookie is refused, the dashboard's own same-origin one passes). Methods and the token form sit outside the
	// session guard: they are how a caller without a session gets one.
	if rt.Auth != nil {
		guarded := http.NewServeMux()
		guarded.HandleFunc("GET /api/auth/methods", rt.Auth.HandleMethods)
		guarded.HandleFunc("POST /api/auth/token", rt.Auth.HandleToken)
		guarded.HandleFunc("GET /api/auth/login", rt.Auth.HandleLogin)
		guarded.HandleFunc("GET /api/auth/callback", rt.Auth.HandleCallback)
		guarded.HandleFunc("POST /api/auth/logout", rt.Auth.HandleLogout)
		guarded.Handle("GET /api/me", rt.Auth.Guard(http.HandlerFunc(rt.Auth.HandleMe)))
		if rt.Dash != nil {
			guarded.Handle("GET /api/dash/{endpoint}", rt.Auth.Guard(rt.Dash))
		}
		h := httpx.CrossOrigin(guarded)
		for _, p := range []string{"/api/auth/", "/api/me", "/api/dash/"} {
			mux.Handle(p, h)
		}
	}
	return chain(mux, rt.TrustedHops)
}

// postOnly answers anything but POST (the preflight is CORS's) with 405.
func postOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST, OPTIONS")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// chain wraps h in the middleware every request passes. SecurityHeaders sits outside Recover, so a panic's 500
// keeps its headers.
func chain(h http.Handler, hops int) http.Handler {
	return httpx.Chain(h,
		httpx.AccessLog(slog.Default(), httpx.AccessLogOptions{ClientIP: apix.ClientIP(hops),
			RoutineLevel: slog.LevelInfo, Skip: quietIngest}),
		httpx.SecurityHeadersWith(httpx.SecurityOptions{HTMLOnly: true,
			NoIndex: func(p string) bool { return strings.HasPrefix(p, "/api/") }}),
		httpx.Recover(slog.Default()),
	)
}

// quietIngest keeps the collector out of the access log unless it failed: every page view of every site would be a
// line, and a flood's refusals are what the counters are for.
func quietIngest(r *http.Request, status int) bool {
	if r.URL.Path != "/api/in" && r.URL.Path != "/api/feed" && r.URL.Path != "/nui.js" {
		return false
	}
	return status < 500 && status != http.StatusUnauthorized
}
