package app

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trick77/ship/kit/auth"
	"github.com/trick77/ship/kit/ratelimit"
)

const testToken = "0123456789abcdef0123456789abcdef-admin"

// tokenAuth builds the sign-in from vars; without BACKEND_DATA_DIR in them its session secret is made in a new
// temporary directory.
func tokenAuth(t *testing.T, vars map[string]string) *Auth {
	t.Helper()
	all := map[string]string{"BACKEND_DATA_DIR": t.TempDir()}
	for k, v := range vars {
		all[k] = v
	}
	cfg, err := LoadConfig(lookup(all))
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAuth(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// No spacing unless a test asks for it.
	a.logins = ratelimit.NewThrottle(time.Nanosecond, 0)
	a.loginsNet = ratelimit.NewThrottle(time.Nanosecond, 0)
	a.loginsAll = ratelimit.NewThrottle(time.Nanosecond, 0)
	return a
}

// The token throttles key the caller by address, narrow to wide: a header a caller writes around the proxy never
// buys a fresh bucket or names someone else's, another /64 of the same /48 meets the network's queue, and another
// network the shared one.
func TestTokenThrottleTiers(t *testing.T) {
	post := func(h http.Handler, peer, xff string) int {
		r := tokenPost("token=wrong-guess")
		r.RemoteAddr = peer
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return do(h, r).Code
	}
	hour := func() *ratelimit.Throttle { return ratelimit.NewThrottle(time.Hour, 0) }

	// The client tier, behind one proxy and reached around it (hops 1 and 0).
	for _, hops := range []string{"1", "0"} {
		a := tokenAuth(t, map[string]string{"BACKEND_ADMIN_TOKEN": testToken, "BACKEND_TRUSTED_HOPS": hops})
		a.logins = hour()
		h := routes(t, a)
		if got := post(h, "198.51.100.7:1", "spoof-1"); got != http.StatusUnauthorized {
			t.Fatalf("hops %s: first: %d", hops, got)
		}
		// A new header each time is no new client: the peer is.
		if got := post(h, "198.51.100.7:2", "spoof-2-"+strings.Repeat("x", 300)); got != http.StatusTooManyRequests {
			t.Errorf("hops %s: spoofed header: %d, want 429", hops, got)
		}
		// Reached directly, naming the owner's address does not charge the owner's bucket.
		if hops == "0" {
			_ = post(h, "198.51.100.8:1", "203.0.113.50")
			if got := post(h, "203.0.113.50:1", ""); got != http.StatusUnauthorized {
				t.Errorf("owner locked out by a spoofed header: %d", got)
			}
		}
	}

	// The network tier: two /64s of one /48, and two addresses of one /24.
	a := tokenAuth(t, map[string]string{"BACKEND_ADMIN_TOKEN": testToken})
	a.loginsNet = hour()
	h := routes(t, a)
	for _, pair := range [][2]string{{"2001:db8:1:1::7", "2001:db8:1:2::7"}, {"192.0.2.1", "192.0.2.2"}} {
		if got := post(h, "10.0.0.2:1", pair[0]); got != http.StatusUnauthorized {
			t.Fatalf("network %s: %d", pair[0], got)
		}
		if got := post(h, "10.0.0.2:1", pair[1]); got != http.StatusTooManyRequests {
			t.Errorf("same network %s: %d, want 429", pair[1], got)
		}
	}
	if got := post(h, "10.0.0.2:1", "2001:db8:2::7"); got != http.StatusUnauthorized {
		t.Errorf("another /48: %d", got)
	}

	// The shared tier bounds every network together.
	a = tokenAuth(t, map[string]string{"BACKEND_ADMIN_TOKEN": testToken})
	a.loginsAll = hour()
	h = routes(t, a)
	_ = post(h, "10.0.0.2:1", "192.0.2.1")
	if got := post(h, "10.0.0.2:1", "2001:db8:9::1"); got != http.StatusTooManyRequests {
		t.Errorf("shared tier: %d, want 429", got)
	}

	// The defaults, as sized: one a second per client, two per network, four in all.
	a = tokenAuth(t, map[string]string{"BACKEND_ADMIN_TOKEN": testToken})
	a.logins, a.loginsNet, a.loginsAll = nil, nil, nil
	if err := a.tokenTurn(tokenPost("")); err != nil {
		t.Fatal(err)
	}
	if a.logins == nil || a.loginsNet == nil || a.loginsAll == nil ||
		tokenGap != time.Second || tokenNetGap != 500*time.Millisecond || tokenAllGap != 250*time.Millisecond {
		t.Error("token throttles not as documented")
	}
}

func tokenPost(body string) *http.Request {
	r := httptest.NewRequest("POST", "/api/auth/token", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	return r
}

func TestAuthMethods(t *testing.T) {
	oidc := map[string]string{"BACKEND_OIDC_ISSUER": "https://auth.example.com", "BACKEND_OIDC_CLIENT_ID": "stakeout",
		"BACKEND_OIDC_CLIENT_SECRET": "s", "BACKEND_OIDC_REDIRECT_URL": "https://stakeout.example.com/api/auth/callback"}
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
	a := tokenAuth(t, map[string]string{"BACKEND_ADMIN_TOKEN": testToken, "BACKEND_OIDC_ADMIN_GROUP": "stakeout-admins"})
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
		vars := map[string]string{"BACKEND_ADMIN_TOKEN": testToken, "BACKEND_SESSION_SECRET": secret,
			"BACKEND_DATA_DIR": t.TempDir()}
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
		// The same token beside another session secret (another data directory's, another configured one): the
		// cookie is worth nothing there, the key is never the token's alone.
		other := map[string]string{"BACKEND_ADMIN_TOKEN": testToken, "BACKEND_DATA_DIR": t.TempDir()}
		if secret != "" {
			other["BACKEND_SESSION_SECRET"] = strings.Repeat("t", 32)
		}
		if got := get(tokenAuth(t, other)); got != http.StatusUnauthorized {
			t.Errorf("secret %q: same token, other secret: %d, want 401", secret, got)
		}
		vars["BACKEND_ADMIN_TOKEN"] = strings.Repeat("n", 40)
		if got := get(tokenAuth(t, vars)); got != http.StatusUnauthorized {
			t.Errorf("secret %q: rotated token: %d, want 401", secret, got)
		}
	}
}

// Without BACKEND_SESSION_SECRET the secret is made once in the data directory, owner-only, and kept; a directory it
// cannot be made in, or a damaged file, fails the start.
func TestSessionSecretFile(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DataDir: dir, AdminToken: testToken}
	k1, err := sessionKey(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, SessionSecretFile)
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("secret file: %v %v", st, err)
	}
	if k2, err := sessionKey(cfg); err != nil || !bytes.Equal(k1, k2) {
		t.Errorf("a restart made another key: %v", err)
	}
	// Neither the token alone nor kit's derivation from it.
	if bytes.Equal(k1, auth.DeriveKey(testToken, "session")) {
		t.Error("the key is the token's alone")
	}
	// A file others can read is restricted, not refused.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if k3, err := sessionKey(cfg); err != nil || !bytes.Equal(k1, k3) {
		t.Errorf("after chmod: %v", err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("not restricted: %v", st.Mode())
	}
	// A configured secret wins over the file.
	withSecret := Config{DataDir: dir, AdminToken: testToken, SessionSecret: strings.Repeat("s", 32)}
	if k4, _ := sessionKey(withSecret); bytes.Equal(k1, k4) {
		t.Error("configured secret ignored")
	}

	if err := os.WriteFile(path, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionKey(cfg); err == nil || !strings.Contains(err.Error(), "want at least 32") {
		t.Errorf("damaged file: %v", err)
	}
	if _, err := NewAuth(Config{DataDir: filepath.Join(dir, "missing")}); err == nil ||
		!strings.Contains(err.Error(), "BACKEND_DATA_DIR") {
		t.Errorf("missing directory: %v", err)
	}
	if os.Geteuid() == 0 {
		return // root writes into a read-only directory regardless
	}
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	if _, err := sessionKey(Config{DataDir: ro}); err == nil || !strings.Contains(err.Error(), "BACKEND_DATA_DIR") {
		t.Errorf("read-only directory: %v", err)
	}
}
