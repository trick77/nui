package app

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/trick77/ship/kit/ratelimit"
)

const testToken = "0123456789abcdef0123456789abcdef-admin"

func tokenAuth(t *testing.T, vars map[string]string) *Auth {
	t.Helper()
	cfg, err := LoadConfig(lookup(vars))
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAuth(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.logins = ratelimit.NewThrottle(time.Nanosecond, 0) // no spacing unless a test asks for it
	return a
}

func tokenPost(body string) *http.Request {
	r := httptest.NewRequest("POST", "/api/auth/token", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	return r
}

func TestAuthMethods(t *testing.T) {
	oidc := map[string]string{"BACKEND_OIDC_ISSUER": "https://auth.example.com", "BACKEND_OIDC_CLIENT_ID": "nui",
		"BACKEND_OIDC_CLIENT_SECRET": "s", "BACKEND_OIDC_REDIRECT_URL": "https://nui.example.com/api/auth/callback"}
	both := map[string]string{"BACKEND_ADMIN_TOKEN": testToken}
	for k, v := range oidc {
		both[k] = v
	}
	for _, tt := range []struct {
		name string
		vars map[string]string
		want string
	}{
		{"none", nil, `{"oidc":false,"token":false}`},
		{"token", map[string]string{"BACKEND_ADMIN_TOKEN": testToken}, `{"oidc":false,"token":true}`},
		{"oidc", oidc, `{"oidc":true,"token":false}`},
		{"both", both, `{"oidc":true,"token":true}`},
		{"auth off", map[string]string{"BACKEND_AUTH_DISABLED": "true", "BACKEND_ADMIN_TOKEN": testToken},
			`{"oidc":false,"token":false}`},
	} {
		w := do(routes(t, tokenAuth(t, tt.vars)), httptest.NewRequest("GET", "/api/auth/methods", nil))
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != tt.want ||
			w.Header().Get("Cache-Control") != "max-age=60" {
			t.Errorf("%s: %d %q %v", tt.name, w.Code, w.Body, w.Header())
		}
	}
}

func TestTokenSignIn(t *testing.T) {
	a := tokenAuth(t, map[string]string{"BACKEND_ADMIN_TOKEN": testToken, "BACKEND_OIDC_ADMIN_GROUP": "nui-admins"})
	h := routes(t, a)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	for _, tt := range []struct {
		name, body string
		want       int
	}{
		{"wrong", "token=" + url.QueryEscape(strings.Repeat("x", 40)), http.StatusUnauthorized},
		{"missing", "other=1", http.StatusUnauthorized},
		{"empty", "token=", http.StatusUnauthorized},
		{"unreadable", "token=%zz", http.StatusBadRequest},
		{"too long", "token=" + strings.Repeat("%41", MaxAdminTokenBytes+10), http.StatusRequestEntityTooLarge},
		{"body too large", "token=" + strings.Repeat("A", maxTokenBody), http.StatusRequestEntityTooLarge},
	} {
		w := do(h, tokenPost(tt.body))
		if w.Code != tt.want || len(w.Result().Cookies()) != 0 || strings.Contains(w.Body.String(), "xxxx") {
			t.Errorf("%s: %d %q", tt.name, w.Code, w.Body)
		}
	}
	// A cross-site post is refused by the CSRF guard before the token is looked at.
	r := tokenPost("token=" + testToken)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("Origin", "https://evil.example.net")
	if w := do(h, r); w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 {
		t.Errorf("cross-site: %d", w.Code)
	}

	w := do(h, tokenPost("token="+url.QueryEscape(testToken)))
	if w.Code != http.StatusNoContent || len(w.Result().Cookies()) != 1 {
		t.Fatalf("right token: %d %q", w.Code, w.Body)
	}
	session := w.Result().Cookies()[0]
	if strings.Contains(logs.String(), testToken) || strings.Contains(logs.String(), "xxxx") {
		t.Errorf("a submitted token reached the log: %s", logs.String())
	}
	// The session passes the dashboard's guard (group included), /api/me names it, logout ends it like any other.
	for path, want := range map[string]string{"/api/dash/overview": "dash /api/dash/overview",
		"/api/me": "{\"name\":\"admin\"}\n"} {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(session)
		if w := do(h, r); w.Code != http.StatusOK || w.Body.String() != want {
			t.Errorf("%s with the token's session: %d %q", path, w.Code, w.Body)
		}
	}
	r = httptest.NewRequest("POST", "/api/auth/logout", nil)
	r.AddCookie(session)
	if w := do(h, r); w.Code != http.StatusOK || w.Result().Cookies()[0].MaxAge >= 0 {
		t.Errorf("logout: %d %v", w.Code, w.Result().Cookies())
	}

	// Attempts are spaced per client: past the queue a 429 with Retry-After, another client unaffected.
	a.logins = ratelimit.NewThrottle(time.Hour, 0)
	_ = do(h, tokenPost("token=wrong-guess"))
	w = do(h, tokenPost("token=wrong-guess"))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Errorf("throttled: %d %v", w.Code, w.Header())
	}
	r = tokenPost("token=" + testToken)
	r.RemoteAddr = "198.51.100.7:1234"
	if w := do(h, r); w.Code != http.StatusNoContent {
		t.Errorf("another client: %d", w.Code)
	}

	// Without a token configured the route is not there.
	if w := do(routes(t, tokenAuth(t, nil)), tokenPost("token="+testToken)); w.Code != http.StatusNotFound {
		t.Errorf("no token configured: %d", w.Code)
	}
}

// Rotating BACKEND_ADMIN_TOKEN ends the sessions issued under the old one, with or without a session secret.
func TestTokenRotation(t *testing.T) {
	for _, secret := range []string{"", strings.Repeat("s", 32)} {
		vars := map[string]string{"BACKEND_ADMIN_TOKEN": testToken, "BACKEND_SESSION_SECRET": secret}
		w := do(routes(t, tokenAuth(t, vars)), tokenPost("token="+testToken))
		if w.Code != http.StatusNoContent {
			t.Fatalf("sign-in: %d", w.Code)
		}
		session := w.Result().Cookies()[0]
		get := func(a *Auth) int {
			r := httptest.NewRequest("GET", "/api/me", nil)
			r.AddCookie(session)
			return do(routes(t, a), r).Code
		}
		if got := get(tokenAuth(t, vars)); got != http.StatusOK {
			t.Errorf("secret %q: same token after a restart: %d", secret, got)
		}
		vars["BACKEND_ADMIN_TOKEN"] = strings.Repeat("n", 40)
		if got := get(tokenAuth(t, vars)); got != http.StatusUnauthorized {
			t.Errorf("secret %q: rotated token: %d, want 401", secret, got)
		}
	}
}
