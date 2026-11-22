package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRoutes(t *testing.T) {
	// Echoes the path the dashboard handler sees: the prefix must arrive stripped.
	dashboard := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(r.URL.Path)) })
	h := Build(dashboard)

	for _, tt := range []struct {
		path, wantLocation string
		wantCode           int
	}{
		{"/", "/dashboard/", http.StatusFound},
		{"/dashboard", "/dashboard/", http.StatusTemporaryRedirect},
	} {
		w := do(h, httptest.NewRequest("GET", tt.path, nil))
		if w.Code != tt.wantCode || w.Header().Get("Location") != tt.wantLocation {
			t.Errorf("GET %s: %d %q, want %d %q", tt.path, w.Code, w.Header().Get("Location"), tt.wantCode,
				tt.wantLocation)
		}
	}
	for path, want := range map[string]string{"/dashboard/": "/", "/dashboard/sites/3": "/sites/3"} {
		if w := do(h, httptest.NewRequest("GET", path, nil)); w.Code != http.StatusOK || w.Body.String() != want {
			t.Errorf("GET %s: %d %q, want 200 %q", path, w.Code, w.Body, want)
		}
	}
	// /api never reaches the dashboard, nor does any other path outside it.
	for _, path := range []string{"/api", "/api/", "/api/x", "/dashboardx", "/other"} {
		if w := do(h, httptest.NewRequest("GET", path, nil)); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d %q, want 404", path, w.Code, w.Body)
		}
	}
}

func TestReadyz_LocalOnly(t *testing.T) {
	h := Build(http.NotFoundHandler())
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

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.2:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := ClientIP(r); got != "203.0.113.9" {
		t.Errorf("ClientIP = %q", got)
	}
}
