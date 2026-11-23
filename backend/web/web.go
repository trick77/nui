// Package web embeds the built UI: ui/ builds straight into dist/, which git holds only as .gitkeep (ship check and
// the image build fill it).
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/trick77/ship/kit/spa"
)

//go:embed all:dist
var embedded embed.FS

// Handler serves the embedded UI for an app mounted at base ("/dashboard", the prefix stripped before this handler):
// files as they are, /assets/ cached forever, the shell on every other path outside /api, kit's 503 placeholder
// when the UI was not built.
func Handler(base string) (http.Handler, error) {
	dist, err := fs.Sub(embedded, "dist")
	if err != nil {
		return nil, err
	}
	return serve(dist, base), nil
}

// Tracker serves the browser script the UI build writes to the embed's root as stakeout.js, cached a day (kit's
// spa.Files); 404 while the build has not produced it.
func Tracker() (http.Handler, error) {
	dist, err := fs.Sub(embedded, "dist")
	if err != nil {
		return nil, err
	}
	return spa.Files(dist, "stakeout.js"), nil
}

// serve rewrites the shell for base. Vite builds with base "./", but the shell is served on deep routes
// (/dashboard/sites/3), where "./assets/…" would resolve to /dashboard/sites/assets/… and come back as the shell.
// So every "./" ref becomes base + "/". A <base href> would do it in one tag, but kit's CSP sets base-uri 'none'.
func serve(dist fs.FS, base string) http.Handler {
	var o spa.Options
	if shell, err := fs.ReadFile(dist, "index.html"); err == nil {
		rewritten := []byte(strings.ReplaceAll(string(shell), `="./`, `="`+base+`/`))
		o.Index = func([]byte, *http.Request) []byte { return rewritten }
	}
	return spa.Handler(dist, o)
}
