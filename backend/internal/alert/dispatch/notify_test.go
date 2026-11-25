package dispatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/alert"
)

// Wire: a sender per channel notify takes, a fault for one it refuses (naming the channel and the services, never
// the URL), the file then sending nothing.
func TestWire(t *testing.T) {
	c, senders := Wire(alert.Parse([]byte(`{"channels": {"ops": "ntfy://ntfy.example.com/t",
		"hook": "generic+https://hooks.example.com/x"}, "defaults": {"channels": ["ops"]},
		"rules": [{"name": "a", "on": "error.new"}]}`), alert.Known{}))
	if len(c.Faults) > 0 || !c.Sending() || len(senders) != 2 {
		t.Fatalf("good: %v %d senders", c.Faults, len(senders))
	}
	if ch, _ := c.Channel("hook"); ch.Service != "generic+https" {
		t.Errorf("service %q", ch.Service)
	}
	// The documented example's channels are all notify's.
	c, _ = Wire(alert.Load(filepath.Join("..", "..", "..", "..", "docs", "notify.example.json"),
		alert.Known{Domains: []string{"lens.example.com"}, Products: []string{"Lens"}}))
	if len(c.Faults) > 0 {
		t.Errorf("example: %v", c.Faults)
	}
	c, senders = Wire(alert.Parse([]byte(`{"channels": {"ops": "ntfy://ntfy.example.com/t",
		"chat": "slack://hook:hunter2@T0/B0/X0", "mail": "smtp://user:hunter2@mail.example.com:587/?to=%zz"},
		"rules": [{"name": "a", "on": "error.new"}]}`), alert.Known{}))
	faults := strings.Join(c.Faults, "\n")
	if len(c.Faults) != 2 || c.Sending() || !c.Defaulted || len(senders) != 1 || strings.Contains(faults, "hunter2") ||
		!strings.Contains(faults, `channel "chat" (slack): unsupported service; supported: generic, gotify, ntfy, pushover, smtp, telegram`) ||
		!strings.Contains(faults, `channel "mail" (smtp): invalid URL`) {
		t.Errorf("faults %q, %d senders", c.Faults, len(senders))
	}
}

// A channel's send: delivered, refused with a scrubbed reason, and cut short when the shutdown ends its context.
func TestChannelSender(t *testing.T) {
	var titles []string
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		titles = append(titles, r.Header.Get("X-Title")+r.Header.Get("Title"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","event":"message"}`))
	}))
	defer ok.Close()
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer hang.Close()
	defer close(release) // before Close: the request may outlive the cancelled wait
	host := func(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }
	c, senders := Wire(alert.Parse([]byte(`{"channels": {"ok": "ntfy://`+host(ok)+`/t?scheme=http",
		"hang": "ntfy://user:hunter2@`+host(hang)+`/t?scheme=http"}}`), alert.Known{}))
	if len(c.Faults) > 0 {
		t.Fatal(c.Faults)
	}
	if ds := senders["ok"].Deliver(t.Context(), "[high] x", "body", alert.High); Failure(ds) != "" || ds[0].Service != "ntfy" {
		t.Errorf("delivered %+v", ds)
	}
	if len(titles) != 1 {
		t.Errorf("the server got %d requests", len(titles))
	}
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	ds := senders["hang"].Deliver(ctx, "t", "b", alert.Low)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("a cancelled send took %v", took)
	}
	if f := Failure(ds); !strings.Contains(f, "cancel") || strings.Contains(f, "hunter2") {
		t.Errorf("cancelled: %q", f)
	}
}
