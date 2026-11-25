package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/trick77/trakk/backend/internal/alert"
)

// Delivery is one service URL's outcome of a send.
type Delivery struct {
	// Service is the URL's scheme (ntfy, smtp, …).
	Service string
	// Err is nil when the service took the message. Its text must never hold the URL: a service's error quotes it,
	// token and all, and the dispatcher scrubs it again before it is logged or stored.
	Err error
}

// Sender delivers a message to one named channel's services. Deliver returns when ctx ends (the shutdown aborts a
// send instead of waiting for it) or every service answered, one Delivery each.
type Sender interface {
	Deliver(ctx context.Context, title, body string, sev alert.Severity) []Delivery
}

// Message is one message a Recorder kept.
type Message struct {
	Channel, Title, Body string
	Severity             alert.Severity
}

// Recorder keeps every message instead of sending it: the simulator's sender and the tests'. Fail, when set, fails
// every delivery with that error instead.
type Recorder struct {
	mu       sync.Mutex
	messages []Message
	Fail     error
}

// Senders is a sender per channel of c, each recording into r.
func (r *Recorder) Senders(c alert.Config) map[string]Sender {
	out := map[string]Sender{}
	for _, ch := range c.Channels {
		out[ch.Name] = recorded{r: r, channel: ch}
	}
	return out
}

// Messages are the messages kept so far, oldest first.
func (r *Recorder) Messages() []Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.messages)
}

type recorded struct {
	r       *Recorder
	channel alert.Channel
}

func (s recorded) Deliver(ctx context.Context, title, body string, sev alert.Severity) []Delivery {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	d := Delivery{Service: s.channel.Service, Err: s.r.Fail}
	if err := ctx.Err(); err != nil {
		d.Err = err
	}
	if d.Err == nil {
		s.r.messages = append(s.r.messages, Message{Channel: s.channel.Name, Title: title, Body: body, Severity: sev})
	}
	return []Delivery{d}
}

// Failure is the first failed delivery's text, scrubbed of any URL; "" when every service took it. No delivery at
// all is a failure too.
func Failure(ds []Delivery) string {
	if len(ds) == 0 {
		return "no service answered"
	}
	for _, d := range ds {
		if d.Err != nil {
			return alert.Clip(alert.Scrub(fmt.Sprintf("%s: %v", d.Service, d.Err)), 300)
		}
	}
	return ""
}

// ErrNotSent is Test's error when a channel did not take the test message.
var ErrNotSent = errors.New("not every channel took the test message")

// Test sends a test message to every channel of c and writes each one's outcome to out (-notify-test). A channel
// without a sender (none wired into this build) is reported as such.
func Test(ctx context.Context, c alert.Config, senders map[string]Sender, out io.Writer) error {
	if len(c.Channels) == 0 {
		_, _ = fmt.Fprintln(out, "no channel configured")
		return ErrNotSent
	}
	failed := false
	for _, ch := range c.Channels {
		s, ok := senders[ch.Name]
		if !ok {
			failed = true
			_, _ = fmt.Fprintf(out, "%s (%s): not sent, this build has no sender for it\n", ch.Name, ch.Service)
			continue
		}
		f := Failure(s.Deliver(ctx, "[low] Trakk test message", "A test message from Trakk's -notify-test: the "+
			ch.Name+" channel works.", alert.Low))
		if f != "" {
			failed = true
			_, _ = fmt.Fprintf(out, "%s (%s): failed: %s\n", ch.Name, ch.Service, f)
			continue
		}
		_, _ = fmt.Fprintf(out, "%s (%s): sent\n", ch.Name, ch.Service)
	}
	if failed {
		return ErrNotSent
	}
	return nil
}
