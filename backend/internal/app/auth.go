package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trick77/ship/kit/auth"
	"github.com/trick77/ship/kit/logging"
	"github.com/trick77/ship/kit/ratelimit"
	"github.com/trick77/ship/kit/run"
	"github.com/trick77/ship/kit/secret"

	"github.com/trick77/nui/backend/internal/apix"
)

// SessionCookie is the session cookie's name, prefixed so another app on the same parent domain cannot overwrite it.
const SessionCookie = "nui_session"

// CookiePrefix names the OIDC flow's transient cookies.
const CookiePrefix = "nui_"

// AdminName is the identity of a session signed in with the admin token: whoever held it, not a person.
const AdminName = "admin"

// Sign-in attempts with the admin token are spaced tokenGap apart per client (an IPv4 address, an IPv6 /64), queued
// up to tokenMaxWait and refused past that with the wait.
const (
	tokenGap     = time.Second
	tokenMaxWait = 5 * time.Second
)

// maxTokenBody caps the token form: the longest token with every byte url-encoded as %XX, plus the field name.
const maxTokenBody = 4 * MaxAdminTokenBytes

// Auth is the sign-in: OIDC (kit/auth), the admin token, and the session cookie both issue. Discovery may fail at
// start (the provider down): the collector must not wait for it, so the flow is built in the background and login
// answers 503 until then.
type Auth struct {
	Sessions *auth.Sessions
	Group    string
	// Disabled serves the dashboard to everyone as "dev" (local development only, config refuses it in production).
	Disabled bool
	// OIDCConfigured: the OIDC flow is configured (it may still be discovering). AdminToken: token sign-in, "" off.
	OIDCConfigured bool
	AdminToken     string
	oidc           atomic.Pointer[auth.OIDC]
	loginsOnce     sync.Once
	logins         *ratelimit.Throttle
}

// HandleMethods is GET /api/auth/methods: which sign-in the card offers, nothing else.
func (a *Auth) HandleMethods(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "max-age=60")
	_ = json.NewEncoder(w).Encode(map[string]bool{
		"oidc":  !a.Disabled && (a.OIDCConfigured || a.oidc.Load() != nil),
		"token": !a.Disabled && a.AdminToken != "",
	})
}

// HandleToken is POST /api/auth/token: a form with the admin token, a session of AdminName on a match (204). The
// value is never echoed or logged: a wrong token is often a right one for something else. Attempts are spaced per
// client by kit's Throttle, 429 with Retry-After past its queue.
func (a *Auth) HandleToken(w http.ResponseWriter, r *http.Request) {
	if a.Disabled || a.AdminToken == "" {
		apix.Error(w, http.StatusNotFound, "token sign-in is not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTokenBody)
	if err := r.ParseForm(); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			apix.Error(w, http.StatusRequestEntityTooLarge, "token too long")
			return
		}
		apix.Error(w, http.StatusBadRequest, "unreadable form")
		return
	}
	tok := r.PostFormValue("token")
	switch {
	case tok == "":
		apix.Error(w, http.StatusUnauthorized, "wrong token") // nothing to compare, nothing to space out
		return
	case len(tok) > MaxAdminTokenBytes:
		apix.Error(w, http.StatusRequestEntityTooLarge, "token too long")
		return
	}
	a.loginsOnce.Do(func() {
		if a.logins == nil {
			a.logins = ratelimit.NewThrottle(tokenGap, tokenMaxWait)
		}
	})
	if err := a.logins.Wait(r.Context(), ratelimit.Key(ClientIP(r))); err != nil {
		if t, ok := errors.AsType[*ratelimit.ThrottledError](err); ok {
			apix.TooMany(w, t.RetryAfter)
			return
		}
		apix.Error(w, http.StatusServiceUnavailable, "sign-in not checked") // the client left while queued
		return
	}
	if !secret.Match(tok, a.AdminToken) {
		slog.WarnContext(r.Context(), "Admin token sign-in refused", "ip", ClientIP(r))
		apix.Error(w, http.StatusUnauthorized, "wrong token")
		return
	}
	c := auth.Claims{Subject: AdminName, Name: AdminName}
	if a.Group != "" {
		c.Groups = []string{a.Group}
	}
	if err := a.Sessions.Issue(w, c); err != nil {
		apix.ServerError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "Admin token sign-in", "ip", ClientIP(r))
	w.WriteHeader(http.StatusNoContent)
}

// NewAuth builds the sign-in from cfg. The session key derives from the admin token whenever one is set, so
// rotating the token ends every session issued under the old one (with a session secret too, the key derives from
// both: the OIDC sessions end with it). No secret and no token: a random key, sessions end with the process.
func NewAuth(cfg Config) (*Auth, error) {
	var key []byte
	switch {
	case cfg.SessionSecret != "" && cfg.AdminToken != "":
		key = auth.DeriveKey(cfg.SessionSecret+"\x00"+cfg.AdminToken, "nui-session")
	case cfg.SessionSecret != "":
		key = []byte(cfg.SessionSecret)
	case cfg.AdminToken != "":
		key = auth.DeriveKey(cfg.AdminToken, "nui-session")
	default:
		slog.Warn("BACKEND_SESSION_SECRET and BACKEND_ADMIN_TOKEN are empty: sessions end with this process")
		key = auth.RandomKey()
	}
	sessions, err := auth.NewSessions(auth.SessionConfig{Name: SessionCookie, Key: key, Secure: cfg.Secure(),
		KeepGroup: cfg.OIDCAdminGroup})
	if err != nil {
		return nil, err
	}
	if !cfg.OIDCConfigured() && !cfg.TokenConfigured() && !cfg.AuthDisabled {
		slog.Warn("Neither OIDC nor BACKEND_ADMIN_TOKEN is configured: the dashboard answers 401 until one is")
	}
	if cfg.AuthDisabled {
		slog.Warn("BACKEND_AUTH_DISABLED: the dashboard is open to anyone reaching this port")
	}
	return &Auth{Sessions: sessions, Group: cfg.OIDCAdminGroup, Disabled: cfg.AuthDisabled,
		OIDCConfigured: cfg.OIDCConfigured() && !cfg.AuthDisabled, AdminToken: cfg.AdminToken}, nil
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
