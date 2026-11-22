package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/trick77/ship/kit/auth"
	"github.com/trick77/ship/kit/logging"
	"github.com/trick77/ship/kit/run"

	"github.com/trick77/nui/backend/internal/apix"
)

// SessionCookie is the session cookie's name, prefixed so another app on the same parent domain cannot overwrite it.
const SessionCookie = "nui_session"

// CookiePrefix names the OIDC flow's transient cookies.
const CookiePrefix = "nui_"

// Auth is the sign-in: OIDC (kit/auth) and the session cookie. Discovery may fail at start (the provider down): the
// collector must not wait for it, so the flow is built in the background and login answers 503 until then.
type Auth struct {
	Sessions *auth.Sessions
	Group    string
	// Disabled serves the dashboard to everyone as "dev" (local development only, config refuses it in production).
	Disabled bool
	oidc     atomic.Pointer[auth.OIDC]
}

// SetOIDC installs the flow (tests, and Discover).
func (a *Auth) SetOIDC(o *auth.OIDC) { a.oidc.Store(o) }

// Discover builds the OIDC flow, retrying every minute until the provider answers or ctx ends.
func (a *Auth) Discover(ctx context.Context, cfg auth.OIDCConfig) {
	for {
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		o, err := auth.NewOIDC(dctx, cfg)
		cancel()
		if err == nil {
			a.SetOIDC(o)
			slog.Info("OIDC provider discovered", "issuer", cfg.Issuer)
			return
		}
		slog.Warn("OIDC discovery failed, sign-in unavailable until it succeeds", logging.Err(err))
		if !run.Sleep(ctx, time.Minute) {
			return
		}
	}
}

// Guard admits a signed-in member of the group: 401 {"detail"} without a session (the UI signs in), 403 with one
// outside the group (signing in again would not help).
func (a *Auth) Guard(next http.Handler) http.Handler {
	if a.Disabled {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.NewContext(r.Context(), auth.Claims{Subject: "dev", Name: "dev"})))
		})
	}
	return a.Sessions.Guard(a.Group,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			apix.Error(w, http.StatusUnauthorized, "not signed in")
		}),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			apix.Error(w, http.StatusForbidden, "not in the dashboard group")
		}))(next)
}

// HandleMe is GET /api/me.
func (a *Auth) HandleMe(w http.ResponseWriter, r *http.Request) {
	c, _ := auth.FromContext(r.Context())
	name := c.Name
	if name == "" {
		name = c.Username()
	}
	apix.JSON(w, http.StatusOK, map[string]string{"name": name})
}

// HandleLogin starts the provider's flow.
func (a *Auth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if a.Disabled {
		http.Redirect(w, r, DashboardPath+"/", http.StatusFound)
		return
	}
	o := a.oidc.Load()
	if o == nil {
		apix.Error(w, http.StatusServiceUnavailable, "sign-in is not available")
		return
	}
	o.Start(w, r)
}

// HandleCallback finishes it: a session on success, the no-access page for a proven identity outside the group,
// one generic marker for everything else (the log keeps the reason).
func (a *Auth) HandleCallback(w http.ResponseWriter, r *http.Request) {
	o := a.oidc.Load()
	if o == nil {
		apix.Error(w, http.StatusServiceUnavailable, "sign-in is not available")
		return
	}
	c, err := o.Callback(w, r)
	if errors.Is(err, auth.ErrNotInGroup) {
		slog.WarnContext(r.Context(), "OIDC sign-in refused: not in the dashboard group", "sub", c.Subject,
			"user", c.Username())
		http.Redirect(w, r, DashboardPath+"/?no_access=1", http.StatusFound)
		return
	}
	if err == nil {
		err = a.Sessions.Issue(w, c)
	}
	if err != nil {
		slog.WarnContext(r.Context(), "OIDC callback failed", logging.Err(err))
		http.Redirect(w, r, DashboardPath+"/?auth_error=1", http.StatusFound)
		return
	}
	http.Redirect(w, r, DashboardPath+"/", http.StatusFound)
}

// HandleLogout clears the session and says where to go; the app calls it with fetch, which would follow a redirect.
func (a *Auth) HandleLogout(w http.ResponseWriter, _ *http.Request) {
	a.Sessions.Clear(w)
	apix.JSON(w, http.StatusOK, map[string]string{"redirect_url": DashboardPath + "/?signed_out=1"})
}
