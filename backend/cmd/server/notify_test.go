package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trick77/trakk/backend/internal/alert"
	"github.com/trick77/trakk/backend/internal/alert/dispatch"
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
	// No sender in this build: the test says so and fails.
	if code, out := run(true); code != 1 || !strings.Contains(out, "no sender") {
		t.Errorf("test without a sender: %d %s", code, out)
	}
	rec := &dispatch.Recorder{}
	newSenders = rec.Senders
	t.Cleanup(func() { newSenders = func(alert.Config) map[string]dispatch.Sender { return nil } })
	if code, out := run(true); code != 0 || !strings.Contains(out, "ops (ntfy): sent") || len(rec.Messages()) != 1 {
		t.Errorf("test: %d %s", code, out)
	}
}
