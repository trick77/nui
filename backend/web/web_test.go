package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

const shell = `<!doctype html><script type="module" crossorigin src="./assets/index-abc.js"></script>` +
	`<link rel="stylesheet" crossorigin href="./assets/index-def.css">`

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestServe(t *testing.T) {
	h := serve(fstest.MapFS{
		"index.html":           {Data: []byte(shell)},
		"assets/index-abc.js":  {Data: []byte("export {}")},
		"assets/index-def.css": {Data: []byte("body{}")},
	}, "/dashboard")
	for path, want := range map[string]int{
		"/":                    http.StatusOK,
		"/sites/3":             http.StatusOK,
		"/assets/index-abc.js": http.StatusOK,
		"/assets/gone.js":      http.StatusNotFound,
		"/api":                 http.StatusNotFound,
		"/api/x":               http.StatusNotFound,
	} {
		if w := get(h, path); w.Code != want {
			t.Errorf("%s: status=%d, want %d", path, w.Code, want)
		}
	}
	// A deep route's shell loads its assets from the mount, not from beside the route.
	body := get(h, "/sites/3").Body.String()
	if strings.Contains(body, `"./`) || !strings.Contains(body, `src="/dashboard/assets/index-abc.js"`) ||
		!strings.Contains(body, `href="/dashboard/assets/index-def.css"`) {
		t.Errorf("shell not rewritten: %s", body)
	}
}

func TestServe_Unbuilt(t *testing.T) {
	h := serve(fstest.MapFS{".gitkeep": {}}, "/dashboard")
	if w := get(h, "/"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("unbuilt /: status=%d, want 503", w.Code)
	}
}

func TestHandler_ServesTheEmbed(t *testing.T) {
	h, err := Handler("/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	// A built dist answers 200, the unbuilt .gitkeep-only one kit's 503 placeholder.
	if w := get(h, "/"); w.Code != http.StatusOK && w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", w.Code)
	}
}
