package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const helloFile = `{"channels": {"mail": {"url": "smtp://mail.example.com/?from=a@example.com&to=b@example.com",
	"min_severity": "high", "max_per_hour": 1}, "ops": "ntfy://ntfy.example.com/t"},
	"rules": [{"name": "jobs", "on": "job.failed", "channels": ["ops"]}]}`

// The start's test message goes to every channel whatever its least severity, names the instance and is recorded
// as a delivery; without the option (simulated traffic) nothing is sent.
func TestHello(t *testing.T) {
	r := newRig(t, helloFile)
	r.e.hello(t.Context(), r.now)
	if n := len(r.rec.Messages()); n != 0 {
		t.Fatalf("%d messages without the option", n)
	}
	r.e.o.Hello = true
	r.e.hello(t.Context(), r.now)
	msgs := r.rec.Messages()
	if len(msgs) != 2 || msgs[0].Channel != "mail" || msgs[1].Channel != "ops" {
		t.Fatalf("messages: %+v", msgs)
	}
	if m := msgs[0]; m.Title != "[low] Trakk test message" || !strings.Contains(m.Body, "trakk.example.com") ||
		!strings.Contains(m.Body, "the mail channel works") {
		t.Errorf("message: %+v", m)
	}
	s, err := r.e.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range s.Channels {
		if !ch.LastOK.Equal(r.now) || ch.Error != "" {
			t.Errorf("channel %s: %+v", ch.Name, ch)
		}
	}
	// Outside the hourly cap: the hour's first alert still goes to a channel capped at one.
	if r.e.capped("mail", 1, r.now) {
		t.Error("the test message counted against the cap")
	}
}

// A restart minutes after a channel took a message sends it no second test; later it does.
func TestHelloQuiet(t *testing.T) {
	r := newRig(t, helloFile)
	r.e.o.Hello = true
	r.e.hello(t.Context(), r.now)
	r.e.hello(t.Context(), r.now.Add(helloQuiet-time.Second))
	if n := len(r.rec.Messages()); n != 2 {
		t.Fatalf("%d messages after a restart inside the quiet time, want 2", n)
	}
	r.e.hello(t.Context(), r.now.Add(helloQuiet))
	if n := len(r.rec.Messages()); n != 4 {
		t.Fatalf("%d messages after the quiet time, want 4", n)
	}
}

// A failure is recorded for the footer and never quiets the next start's test: that start checks the fix.
func TestHelloFailure(t *testing.T) {
	r := newRig(t, helloFile)
	r.e.o.Hello = true
	r.rec.Fail = errors.New("refused by ntfy://user:secret@ntfy.example.com/t")
	r.e.hello(t.Context(), r.now)
	s, err := r.e.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range s.Channels {
		if !ch.ErrorAt.Equal(r.now) || !strings.Contains(ch.Error, "refused") || strings.Contains(ch.Error, "secret") {
			t.Errorf("channel %s: %+v", ch.Name, ch)
		}
	}
	r.rec.Fail = nil
	r.e.hello(t.Context(), r.now.Add(time.Minute))
	if n := len(r.rec.Messages()); n != 2 {
		t.Fatalf("%d messages at the start after a failure, want 2", n)
	}
}

// A send the shutdown cut is no attempt: nothing is recorded.
func TestHelloCut(t *testing.T) {
	r := newRig(t, helloFile)
	r.e.o.Hello = true
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r.e.hello(ctx, r.now)
	s, err := r.e.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range s.Channels {
		if !ch.LastOK.IsZero() || !ch.ErrorAt.IsZero() {
			t.Errorf("channel %s recorded a cut send: %+v", ch.Name, ch)
		}
	}
}
