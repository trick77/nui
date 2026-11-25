package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trick77/trakk/backend/internal/app"
)

// -notify-check and -notify-test: the file against the sites file, every fault printed, exit 1 on any; no URL.
func TestNotifyCmd(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("sites.json", `{"sites": [{"domain": "example.com", "product": "Lens"}]}`)
	cfg, err := app.LoadConfig(func(k string) string {
		return map[string]string{"BACKEND_CONF_DIR": dir, "BACKEND_DATA_DIR": dir}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	run := func(test bool) (int, string) {
		var out strings.Builder
		code := notifyCmd(t.Context(), cfg, test, &out)
		return code, out.String()
	}
	if code, out := run(false); code != 1 || !strings.Contains(out, "missing") {
		t.Errorf("missing: %d %s", code, out)
	}
	write("notify.json", `{"channels": {"ops": "ntfy://user:hunter2@ntfy.example.com/t"},
		"rules": [{"name": "a", "on": "error.new", "products": ["Nope"]}, {"name": "b", "on": "error.old"}]}`)
	if code, out := run(false); code != 1 || strings.Count(out, "fault:") != 2 || strings.Contains(out, "hunter2") {
		t.Errorf("faults: %d %s", code, out)
	}
	write("notify.json", `{"channels": {"ops": "ntfy://user:hunter2@ntfy.example.com/t"}, "defaults": {"channels": ["ops"]},
		"rules": [{"name": "a", "on": "error.new", "products": ["Lens"]}, {"name": "b", "on": "job.failed", "enabled": false}]}`)
	code, out := run(false)
	if code != 0 || !strings.Contains(out, "rule a: error.new, active, to ops") || !strings.Contains(out, "rule b: job.failed, off") ||
		strings.Contains(out, "hunter2") {
		t.Errorf("check: %d %s", code, out)
	}
	// A channel the sender refuses is a fault like any other, naming the channel and the services, never the URL.
	write("notify.json", `{"channels": {"chat": "slack://hook:hunter2@T0/B0/X0"}, "rules": [{"name": "a", "on": "error.new"}]}`)
	if code, out := run(false); code != 1 || !strings.Contains(out, `fault: channel "chat" (slack): unsupported service; supported: generic, gotify, ntfy, pushover, smtp, telegram`) ||
		strings.Contains(out, "hunter2") {
		t.Errorf("unsupported: %d %s", code, out)
	}

	// -notify-test sends: a webhook that takes it, an ntfy server that refuses it. One line each, exit 1, no URL.
	var got []string
	hook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, string(b))
	}))
	defer hook.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "token hunter2 refused", http.StatusInternalServerError)
	}))
	defer down.Close()
	host := func(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }
	write("notify.json", `{"channels": {"hook": "generic://`+host(hook)+`/hook?disabletls=yes&token=hunter2",
		"push": "ntfy://user:hunter2@`+host(down)+`/alerts?scheme=http"}, "rules": [{"name": "a", "on": "error.new"}]}`)
	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	code, out = run(true)
	if code != 1 || !strings.Contains(out, "hook (generic): sent") || !strings.Contains(out, "push (ntfy): failed: ") ||
		len(got) != 1 || !strings.Contains(got[0], "-notify-test") {
		t.Errorf("test: %d %s, webhook got %v", code, out, got)
	}
	for _, s := range []string{out, logs.String()} {
		if strings.Contains(s, "hunter2") || strings.Contains(s, "127.0.0.1") {
			t.Errorf("a URL or token in the output: %s", s)
		}
	}
	down.Close()
	write("notify.json", `{"channels": {"hook": "generic://`+host(hook)+`/hook?disabletls=yes"},
		"rules": [{"name": "a", "on": "error.new"}]}`)
	if code, out := run(true); code != 0 || !strings.Contains(out, "hook (generic): sent") {
		t.Errorf("all sent: %d %s", code, out)
	}
}
