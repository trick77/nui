package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/trick77/ship/kit/auth"
)

func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func echo(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(name + " " + r.URL.Path))
	})
}

func newAuth(t *testing.T, disabled bool) *Auth {
	t.Helper()
	s, err := auth.NewSessions(auth.SessionConfig{Name: SessionCookie, Key: auth.RandomKey()})
	if err != nil {
		t.Fatal(err)
	}
	return &Auth{Sessions: s, Group: "nui-admins", Disabled: disabled}
}

func routes(_ *testing.T, a *Auth) http.Handler {
	return Build(Routes{Dashboard: echo("dashboard"), Tracker: echo("tracker"), Browser: echo("browser"),
		Feed: echo("feed"), Dash: echo("dash"), Auth: a})
}

func TestRoutes(t *testing.T) {
	h := routes(t, newAuth(t, false))
	for _, tt := range []struct {
		path, wantLocation string
		wantCode           int
	}{
		{"/", "/dashboard/", http.StatusFound},
		{"/dashboard", "/dashboard/", http.StatusTemporaryRedirect},
	} {
		w := do(h, httptest.NewRequest("GET", tt.path, nil))
		if w.Code != tt.wantCode || w.Header().Get("Location") != tt.wantLocation {
			t.Errorf("GET %s: %d %q", tt.path, w.Code, w.Header().Get("Location"))
		}
	}
	for path, want := range map[string]string{"/dashboard/": "dashboard /", "/dashboard/log": "dashboard /log",
		"/nui.js": "tracker /nui.js"} {
		if w := do(h, httptest.NewRequest("GET", path, nil)); w.Code != http.StatusOK || w.Body.String() != want {
			t.Errorf("GET %s: %d %q", path, w.Code, w.Body)
		}
	}
	w := do(h, httptest.NewRequest("GET", "/nui.js", nil))
	if w.Header().Get("Cross-Origin-Resource-Policy") != "cross-origin" {
		t.Errorf("tracker CORP: %v", w.Header())
	}
	for _, path := range []string{"/api", "/api/", "/api/x", "/dashboardx", "/other", "/api/dash/overview"} {
		want := http.StatusNotFound
		if path == "/api/dash/overview" {
			want = http.StatusUnauthorized
		}
		if w := do(h, httptest.NewRequest("GET", path, nil)); w.Code != want {
			t.Errorf("GET %s: %d", path, w.Code)
		}
	}
}

func TestCollectorRoutes(t *testing.T) {
	h := routes(t, newAuth(t, false))
	// A cross-site POST from any page reaches the collector, with an open CORS grant and no credentials.
	r := httptest.NewRequest("POST", "/api/in", strings.NewReader("{}"))
	r.Header.Set("Origin", "https://app.example.com")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	w := do(h, r)
	if w.Code != http.StatusOK || w.Body.String() != "browser /api/in" || w.Header().Get("Access-Control-Allow-Origin") != "*" ||
		w.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Errorf("POST /api/in: %d %q %v", w.Code, w.Body, w.Header())
	}
	// A preflight is answered by CORS; any other method is refused.
	r = httptest.NewRequest("OPTIONS", "/api/in", nil)
	r.Header.Set("Origin", "https://app.example.com")
	r.Header.Set("Access-Control-Request-Method", "POST")
	if w := do(h, r); w.Code != http.StatusOK || w.Header().Get("Access-Control-Allow-Methods") != "POST" {
		t.Errorf("preflight: %d %v", w.Code, w.Header())
	}
	if w := do(h, httptest.NewRequest("GET", "/api/in", nil)); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/in: %d", w.Code)
	}
	r = httptest.NewRequest("POST", "/api/feed", strings.NewReader("{}"))
	r.Header.Set("Origin", "https://elsewhere.example.net")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if w := do(h, r); w.Code != http.StatusOK || w.Body.String() != "feed /api/feed" {
		t.Errorf("POST /api/feed: %d %q", w.Code, w.Body)
	}
	if w := do(h, httptest.NewRequest("GET", "/api/feed", nil)); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/feed: %d", w.Code)
	}
}

func TestDashboardGuard(t *testing.T) {
	a := newAuth(t, false)
	h := routes(t, a)
	w := do(h, httptest.NewRequest("GET", "/api/dash/overview", nil))
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"detail"`) {
		t.Errorf("signed out: %d %q", w.Code, w.Body)
	}
	cookie := func(c auth.Claims) *http.Cookie {
		rec := httptest.NewRecorder()
		if err := a.Sessions.Issue(rec, c); err != nil {
			t.Fatal(err)
		}
		return rec.Result().Cookies()[0]
	}
	r := httptest.NewRequest("GET", "/api/dash/overview", nil)
	r.AddCookie(cookie(auth.Claims{Subject: "x", Groups: []string{"others"}}))
	if w := do(h, r); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"detail"`) {
		t.Errorf("outside the group: %d %q", w.Code, w.Body)
	}
	member := cookie(auth.Claims{Subject: "x", PreferredUsername: "jan", Groups: []string{"nui-admins"}})
	r = httptest.NewRequest("GET", "/api/dash/overview", nil)
	r.AddCookie(member)
	if w := do(h, r); w.Code != http.StatusOK || w.Body.String() != "dash /api/dash/overview" {
		t.Errorf("member: %d %q", w.Code, w.Body)
	}
	r = httptest.NewRequest("GET", "/api/me", nil)
	r.AddCookie(member)
	if w := do(h, r); w.Code != http.StatusOK || w.Body.String() != "{\"name\":\"jan\"}\n" {
		t.Errorf("me: %d %q", w.Code, w.Body)
	}
	// Logout is a POST under the CSRF guard: a cross-site one is refused.
	r = httptest.NewRequest("POST", "/api/auth/logout", nil)
	r.Header.Set("Origin", "https://evil.example.net")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if w := do(h, r); w.Code != http.StatusForbidden {
		t.Errorf("cross-site logout: %d", w.Code)
	}
	w = do(h, httptest.NewRequest("POST", "/api/auth/logout", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "/dashboard/?signed_out=1") {
		t.Errorf("logout: %d %q", w.Code, w.Body)
	}
	// Without a discovered provider, sign-in is unavailable, not a loop.
	for _, p := range []string{"/api/auth/login", "/api/auth/callback"} {
		if w := do(h, httptest.NewRequest("GET", p, nil)); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
}

func TestAuthDisabled(t *testing.T) {
	h := routes(t, newAuth(t, true))
	if w := do(h, httptest.NewRequest("GET", "/api/me", nil)); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "dev") {
		t.Errorf("me: %d %q", w.Code, w.Body)
	}
	if w := do(h, httptest.NewRequest("GET", "/api/auth/login", nil)); w.Code != http.StatusFound {
		t.Errorf("login: %d", w.Code)
	}
}

// fakeIDP is kit's OIDC Backend without a network.
type fakeIDP struct {
	claims auth.Claims
	nonce  string
}

func (f *fakeIDP) AuthCodeURL(state string, _ ...oauth2.AuthCodeOption) string {
	return "https://idp.example.com/auth?state=" + state
}

func (f *fakeIDP) Exchange(context.Context, string, ...oauth2.AuthCodeOption) (*oauth2.Token, error) {
	return &oauth2.Token{AccessToken: "t"}, nil
}

func (f *fakeIDP) Verify(context.Context, *oauth2.Token) (auth.Claims, string, error) {
	return f.claims, f.nonce, nil
}

func TestOIDCFlow(t *testing.T) {
	for _, tt := range []struct {
		name   string
		groups []string
		want   string
	}{{"member", []string{"nui-admins"}, "/dashboard/"}, {"outsider", []string{"x"}, "/dashboard/?no_access=1"}} {
		a := newAuth(t, false)
		idp := &fakeIDP{claims: auth.Claims{Subject: "s", Groups: tt.groups}}
		a.SetOIDC(auth.NewOIDCWithBackend(idp, auth.OIDCConfig{Group: "nui-admins", CookiePrefix: CookiePrefix}))
		h := routes(t, a)
		w := do(h, httptest.NewRequest("GET", "/api/auth/login", nil))
		loc, _ := url.Parse(w.Header().Get("Location"))
		if w.Code != http.StatusFound || loc.Host != "idp.example.com" {
			t.Fatalf("%s login: %d %v", tt.name, w.Code, loc)
		}
		r := httptest.NewRequest("GET", "/api/auth/callback?code=c&state="+loc.Query().Get("state"), nil)
		for _, c := range w.Result().Cookies() {
			if strings.HasSuffix(c.Name, "nonce") {
				idp.nonce = c.Value
			}
			r.AddCookie(c)
		}
		if w := do(h, r); w.Header().Get("Location") != tt.want {
			t.Errorf("%s callback: %d %q", tt.name, w.Code, w.Header().Get("Location"))
		}
	}
	// A callback that fails a check lands on the generic error marker.
	a := newAuth(t, false)
	a.SetOIDC(auth.NewOIDCWithBackend(&fakeIDP{}, auth.OIDCConfig{CookiePrefix: CookiePrefix}))
	if w := do(routes(t, a), httptest.NewRequest("GET", "/api/auth/callback?state=x", nil)); w.Header().Get("Location") != "/dashboard/?auth_error=1" {
		t.Errorf("bad callback: %q", w.Header().Get("Location"))
	}
	// Discovery against nothing gives up when its context ends.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	a.Discover(ctx, auth.OIDCConfig{Issuer: "http://127.0.0.1:1"})
}

func TestReadyz_LocalOnly(t *testing.T) {
	h := Build(Routes{})
	r := httptest.NewRequest("GET", "/readyz", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	if w := do(h, r); w.Code != http.StatusOK {
		t.Errorf("local /readyz: %d", w.Code)
	}
	r = httptest.NewRequest("GET", "/readyz", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if w := do(h, r); w.Code != http.StatusNotFound {
		t.Errorf("proxied /readyz: %d, want 404", w.Code)
	}
}

func TestClientIPAndQuiet(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.2:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := ClientIP(r); got != "203.0.113.9" {
		t.Errorf("ClientIP = %q", got)
	}
	for _, tt := range []struct {
		path   string
		status int
		want   bool
	}{{"/api/in", 204, true}, {"/api/in", 429, true}, {"/api/in", 500, false}, {"/api/feed", 401, false},
		{"/api/dash/overview", 200, false}} {
		if got := quietIngest(httptest.NewRequest("POST", tt.path, nil), tt.status); got != tt.want {
			t.Errorf("quietIngest(%s, %d) = %v", tt.path, tt.status, got)
		}
	}
}
