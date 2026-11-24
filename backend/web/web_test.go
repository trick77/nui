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

func TestTracker(t *testing.T) {
	h, err := Tracker()
	if err != nil {
		t.Fatal(err)
	}
	// Built: the script; unbuilt: 404. Never the shell.
	if w := get(h, "/trakk.js"); w.Code != http.StatusOK && w.Code != http.StatusNotFound {
		t.Fatalf("status=%d", w.Code)
	}
	if w := get(h, "/index.html"); w.Code != http.StatusNotFound {
		t.Errorf("tracker serves other files: %d", w.Code)
	}
}

// The build as it ships: the shell links the icon under the mount, the sign-in card's files are in the embed, the
// tracker is the minified one. Only a built embed can say so: ship check builds the UI before the Go tests.
func TestHandler_ServesTheBuild(t *testing.T) {
	if _, err := embedded.Open("dist/index.html"); err != nil {
		t.Skip("UI not built (npm run build in ui/); ship check builds it before this test")
	}
	h, err := Handler("/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	const icon = `<link rel="icon" type="image/svg+xml" href="/dashboard/icon.svg"`
	if body := get(h, "/").Body.String(); !strings.Contains(body, icon) {
		t.Errorf("shell does not link the icon under the mount: %s", body)
	}
	for path, typ := range map[string]string{"/icon.svg": "image/svg+xml", "/login-cover.webp": "image/webp"} {
		w := get(h, path)
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), typ) || w.Body.Len() == 0 {
			t.Errorf("%s: status=%d type=%q bytes=%d", path, w.Code, w.Header().Get("Content-Type"), w.Body.Len())
		}
	}
	tr, err := Tracker()
	if err != nil {
		t.Fatal(err)
	}
	js := get(tr, "/trakk.js").Body.String()
	if js == "" || strings.Contains(js, "sourceMappingURL") || strings.Contains(js, "/*") ||
		strings.Count(js, "\n") > 2 {
		t.Errorf("trakk.js is not the minified build (%d bytes, %d lines)", len(js), strings.Count(js, "\n"))
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
