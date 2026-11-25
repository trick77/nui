package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/alert"
)

const helloFile = `{"channels": {"mail": {"url": "smtp://mail.example.com/?from=a@example.com&to=b@example.com",
	"min_severity": "high", "max_per_hour": 1}, "ops": "ntfy://ntfy.example.com/t"},
	"rules": [{"name": "jobs", "on": "job.failed", "channels": ["ops"]}]}`

// helloAt sends the start's test message with the clock at now.
func (r *rig) helloAt(ctx context.Context, now time.Time) {
	r.e.hello(ctx, func() time.Time { return now })
}

// The start's test message goes to every channel whatever its least severity, names the instance and is recorded
// as a delivery; without the option (simulated traffic) nothing is sent.
func TestHello(t *testing.T) {
	r := newRig(t, helloFile)
	r.helloAt(t.Context(), r.now)
	if n := len(r.rec.Messages()); n != 0 {
		t.Fatalf("%d messages without the option", n)
	}
	r.e.o.Hello = true
	r.helloAt(t.Context(), r.now)
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
	r.helloAt(t.Context(), r.now)
	r.helloAt(t.Context(), r.now.Add(helloQuiet-time.Second))
	if n := len(r.rec.Messages()); n != 2 {
		t.Fatalf("%d messages after a restart inside the quiet time, want 2", n)
	}
	r.helloAt(t.Context(), r.now.Add(helloQuiet))
	if n := len(r.rec.Messages()); n != 4 {
		t.Fatalf("%d messages after the quiet time, want 4", n)
	}
}

// A failure is recorded for the footer and never quiets the next start's test, a delivery minutes before it
// neither: that start checks the fix.
func TestHelloFailure(t *testing.T) {
	r := newRig(t, helloFile)
	r.e.o.Hello = true
	r.helloAt(t.Context(), r.now) // both took one
	r.rec.Fail = errors.New("refused by ntfy://user:secret@ntfy.example.com/t")
	r.helloAt(t.Context(), r.now.Add(helloQuiet)) // both fail
	s, err := r.e.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range s.Channels {
		if !ch.ErrorAt.Equal(r.now.Add(helloQuiet)) || !strings.Contains(ch.Error, "refused") ||
			strings.Contains(ch.Error, "secret") {
			t.Errorf("channel %s: %+v", ch.Name, ch)
		}
	}
	r.rec.Fail = nil
	// A real delivery, then a failure after it: inside the quiet time of the delivery, tested all the same.
	at := r.now.Add(time.Hour)
	for _, name := range []string{"mail", "ops"} {
		if err := r.st.ChannelDelivered(t.Context(), name, at, ""); err != nil {
			t.Fatal(err)
		}
		if err := r.st.ChannelDelivered(t.Context(), name, at.Add(time.Minute), "smtp: refused"); err != nil {
			t.Fatal(err)
		}
	}
	r.helloAt(t.Context(), at.Add(2*time.Minute))
	if n := len(r.rec.Messages()); n != 4 {
		t.Fatalf("%d messages at the start after a failure, want 4", n)
	}
}

// cutting is a sender the shutdown cuts in its send: took says whether the service answered before the cut.
type cutting struct {
	cancel context.CancelFunc
	took   bool
}

func (c cutting) Deliver(ctx context.Context, _, _ string, _ alert.Severity) []Delivery {
	c.cancel()
	if c.took {
		return []Delivery{{Service: "ntfy"}}
	}
	return []Delivery{{Service: "ntfy", Err: ctx.Err()}}
}

// A send the shutdown cut is no attempt: nothing is recorded, and no channel after it is tried. One the channel
// took before the cut is kept, or the next start would send it again.
func TestHelloCut(t *testing.T) {
	for _, took := range []bool{false, true} {
		r := newRig(t, helloFile)
		r.e.o.Hello = true
		ctx, cancel := context.WithCancel(t.Context())
		r.e.o.Senders["mail"] = cutting{cancel: cancel, took: took}
		r.helloAt(ctx, r.now)
		if n := len(r.rec.Messages()); n != 0 {
			t.Errorf("took %v: %d messages after the cut", took, n)
		}
		s, err := r.e.Status(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range s.Channels {
			wantOK := took && ch.Name == "mail"
			if ch.LastOK.IsZero() == wantOK || !ch.ErrorAt.IsZero() {
				t.Errorf("took %v, channel %s: %+v", took, ch.Name, ch)
			}
		}
	}
}
